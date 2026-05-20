package builders

import (
	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	amv1 "github.com/stigen/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/stigen/smol-agents/pkg/agentmodel/v1"
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

	containers := []corev1.Container{}
	volumes := []corev1.Volume{}
	volumeMounts := []corev1.VolumeMount{}

	// AgentFS sidecar + shared volume when Storage is configured.
	if agent.Spec.Storage != nil && agent.Spec.Storage.Kind == pure.StorageAgentFS && agent.Spec.Storage.AgentFS != nil {
		mp := agent.Spec.Storage.AgentFS.MountPath
		if mp == "" {
			mp = "/var/agentfs"
		}
		volumes = append(volumes, corev1.Volume{
			Name:         "agentfs",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "agentfs", MountPath: mp})
	}

	switch mode {
	case pure.ModeHarness:
		containers = append(containers, harnessContainer(agent, volumeMounts))
	default:
		containers = append(containers, loopContainer(agent, volumeMounts))
	}

	labels := map[string]string{
		"app.kubernetes.io/name":      "smol-agents",
		"app.kubernetes.io/component": "agent-run",
		"agents.stigen.ai/agent":      agent.Name,
		"agents.stigen.ai/run":        run.Name,
	}

	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.Name,
			Namespace: run.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: agent.Name + "-agent",
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr.To(true),
				RunAsUser:    ptr.To[int64](65532),
				RunAsGroup:   ptr.To[int64](65532),
				FSGroup:      ptr.To[int64](65532),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: containers,
			Volumes:    volumes,
		},
	}
}

func harnessContainer(agent *amv1.Agent, mounts []corev1.VolumeMount) corev1.Container {
	image := "smol-agents/agent-harness:0.1.0"
	if agent.Spec.Harness != nil && agent.Spec.Harness.Image != "" {
		image = agent.Spec.Harness.Image
	}
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
		Image:           "smol-agents/agent:0.1.0",
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
