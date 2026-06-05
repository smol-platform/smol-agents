package builders

import (
	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// BuildAgentRunPod renders the Pod that executes a single AgentRun.
// The Pod's lifecycle == the Run's lifecycle; on completion the
// AgentRun reconciler reads stdout / final-output ConfigMap and
// updates Status.
//
// Supports both Mode=loop (in-process plan-act-observe) and Mode=harness
// (delegates to the harness binary inside the same Pod).
func BuildAgentRunPod(run *amv1.AgentRun, agent *amv1.Agent) *corev1.Pod {
	mode := agent.Spec.Mode
	if mode == "" {
		mode = pure.ModeLoop
	}

	// The single execution container; the AgentFS volume mount (if any) is
	// added by AttachStorageFS after the pod is assembled.
	var main corev1.Container
	switch mode {
	case pure.ModeHarness:
		main = harnessContainer(agent, nil)
	default:
		main = loopContainer(agent, nil)
	}
	// Every run pod executes `agent run` against the mounted spec, regardless of
	// mode — this overrides the container builders' default entrypoint/args. The
	// container image (harness.image / agent default) must contain /agent.
	main.Command = []string{"/agent", "run", "--dir=" + RunSpecMountPath}
	main.Args = nil
	main.VolumeMounts = append(main.VolumeMounts, runSpecMount())

	labels := map[string]string{
		"app.kubernetes.io/name":      "smol-agents",
		"app.kubernetes.io/component": "agent-run",
		"agents.smol-agents.ai/agent": agent.Name,
		"agents.smol-agents.ai/run":   run.Name,
	}

	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.Name,
			Namespace: run.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: AgentSAName(agent.Name),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr.To(true),
				RunAsUser:    ptr.To(RunPodUID),
				RunAsGroup:   ptr.To(RunPodUID),
				FSGroup:      ptr.To(RunPodUID),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{main},
			Volumes:    []corev1.Volume{runSpecVolume(run.Name)},
		},
	}

	// Durable AgentFS storage: bounded volume + restore init container +
	// serving sidecar (S3 backup/WAL). Replaces the former EmptyDir-only stub
	// so the agent's files actually persist across Runs (R-AFS).
	if input, ok := storageMountFor(&agent.Spec); ok {
		AttachStorageFS(pod, input)
		// Artifact egress (M2.26): when the Agent declares spec.artifacts, tell the
		// serve sidecar what to collect + where to key it. The sidecar (not the
		// harness) holds the S3 creds; it reports the manifest via its termination
		// message, which the controller folds into status.
		ApplyArtifactCollection(pod, &agent.Spec, run.Namespace, run.Name)
	}
	// The secret broker (AttachSecretBroker) is wired by the controller, which
	// resolves the secrets to serve (harness env secretRef + loop ModelProvider).
	return pod
}

func harnessContainer(agent *amv1.Agent, mounts []corev1.VolumeMount) corev1.Container {
	// The harness driver lives in the image (a per-kind bundle for CLI agents,
	// else the base agent image); harness.image overrides it. All must carry
	// /agent — see HarnessImage.
	image := HarnessImage(agent)
	env := []corev1.EnvVar{}
	if agent.Spec.Harness != nil {
		for _, e := range agent.Spec.Harness.Env {
			if e.Value != "" {
				env = append(env, corev1.EnvVar{Name: e.Name, Value: e.Value})
			}
		}
	}
	return corev1.Container{
		Name:            "harness",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env:             env,
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(false),
			RunAsNonRoot:             ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: mounts,
	}
}

func loopContainer(agent *amv1.Agent, mounts []corev1.VolumeMount) corev1.Container {
	return corev1.Container{
		Name:            "agent",
		Image:           Image("agent"),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"--config=/etc/smol-agents/agent.yaml"},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			RunAsNonRoot:             ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: mounts,
	}
}
