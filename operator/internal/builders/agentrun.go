package builders

import (
	"os"
	"strconv"
	"strings"

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

	// A2A (M3 A1): give a loop run the identity its in-pod AgentRunInvoker needs
	// to create CHILD AgentRuns — its own namespace (downward API), its run name
	// (the delegation-tree label), and its depth in that tree (from the
	// invoker's child label; absent = top-level = 0). Loop mode only; the harness
	// path has no in-process invoker. The invoker is still gated by the pod's
	// RBAC (the <agent>-a2a Role) + a healthy in-cluster client, so this env is
	// inert for a non-A2A run.
	if mode != pure.ModeHarness {
		main.Env = append(main.Env,
			corev1.EnvVar{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
			}},
			corev1.EnvVar{Name: "RUN_NAME", Value: run.Name},
			// The AgentRun's OWN uid (a literal — the pod's downward-API
			// metadata.uid is the POD's uid, not the run's) lets the in-pod A2A
			// invoker set a valid OwnerReference on child AgentRuns, so deleting
			// this parent run GCs the subtree. A wrong uid makes GC delete the
			// child immediately (owner appears non-existent).
			corev1.EnvVar{Name: "AGENT_RUN_UID", Value: string(run.UID)},
		)
		if d := run.Labels[a2aDepthLabel]; d != "" {
			main.Env = append(main.Env, corev1.EnvVar{Name: "A2A_DEPTH", Value: d})
		}
		// A2A recursion ceiling (M3.5), read by WireAgentInvoker → invoker.MaxDepth.
		main.Env = append(main.Env, corev1.EnvVar{Name: "A2A_MAX_DEPTH", Value: a2aMaxDepth()})
	}

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
		// Durable claude session store (M3.19): a persistent claude-code agent with
		// AgentFS keeps its session transcripts under HOME on the workspace, so
		// --resume can reload a prior conversation across runs.
		if agent.Spec.Harness.Kind == pure.HarnessClaudeCode &&
			agent.Spec.Harness.SessionPolicy == pure.SessionPersistent &&
			agent.Spec.Storage != nil && agent.Spec.Storage.AgentFS != nil {
			env = append(env, corev1.EnvVar{Name: "HOME", Value: agent.Spec.EffectiveWorkingDir() + "/.claude-home"})
		}
		// Codex config home (M3.21): when routing codex through a platform gateway,
		// CODEX_HOME is a writable dir the harness copies config.toml into (and codex
		// writes thread state there). On AgentFS for a persistent agent so codex
		// threads survive across runs; else an ephemeral /tmp/.codex.
		if agent.Spec.Harness.Kind == pure.HarnessCodex &&
			agent.Spec.Harness.CLI != nil && agent.Spec.Harness.CLI.CodexBaseURL != "" {
			home := "/tmp/.codex"
			if agent.Spec.Harness.SessionPolicy == pure.SessionPersistent &&
				agent.Spec.Storage != nil && agent.Spec.Storage.AgentFS != nil {
				home = agent.Spec.EffectiveWorkingDir() + "/.codex"
			}
			env = append(env, corev1.EnvVar{Name: "CODEX_HOME", Value: home})
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

// a2aMaxDepth is the A2A recursion ceiling injected into loop-mode run pods
// (A2A_MAX_DEPTH, read by WireAgentInvoker → invoker.MaxDepth). The operator's
// --a2a-max-depth flag sets SMOL_AGENTS_A2A_MAX_DEPTH at startup; default 4.
func a2aMaxDepth() string {
	if v := strings.TrimSpace(os.Getenv("SMOL_AGENTS_A2A_MAX_DEPTH")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return strconv.Itoa(n)
		}
	}
	return "4"
}
