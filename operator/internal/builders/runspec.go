// Package builders — runspec.go
//
// The AgentRun pod executes `agent run --dir=<RunSpecMountPath>`, reading the
// Agent + AgentRunSpec from a projected ConfigMap the controller renders. This
// is what gives the run pod something to execute (the executor/harness layer
// otherwise had no caller).
package builders

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

const (
	// RunSpecMountPath is where the run-spec ConfigMap is mounted; the
	// `agent run` entrypoint reads agent.json + run.json from here.
	RunSpecMountPath = "/etc/smol-agents/run"

	runSpecVolumeName = "runspec"

	// These filenames MUST match agentruntime.AgentSpecFile / RunSpecFile and
	// the provider file cmd/agent reads.
	runSpecAgentFile    = "agent.json"
	runSpecRunFile      = "run.json"
	runSpecProviderFile = "provider.json"
	// runSpecToolsFile carries the resolved loop-mode tool catalog (must match
	// agentruntime.ToolsSpecFile, which the run entrypoint LoadTools reads).
	runSpecToolsFile = "tools.json"

	// runSpecToolsMaxBytes guards tools.json against the ~1 MiB ConfigMap ceiling
	// (a ConfigMap maxes at 1 MiB across all keys; keep tools well under it).
	runSpecToolsMaxBytes = 768 << 10
)

// RunProvider is the resolved ModelProvider a Mode=loop run pod needs to build
// its LLM client. The API key is NOT embedded — it's leased from the broker by
// SecretName at runtime. Rendered as provider.json when the agent is loop mode
// with a providerRef.
type RunProvider struct {
	Kind       string `json:"kind"`
	Endpoint   string `json:"endpoint"`
	SecretName string `json:"secretName,omitempty"`
}

// RunSpecConfigMapName is the per-run spec ConfigMap name.
func RunSpecConfigMapName(runName string) string { return runName + "-runspec" }

// BuildRunSpecConfigMap renders the Agent + AgentRunSpec the run pod executes.
// The Agent is marshalled as the pure (CRD-free) v1.Agent the executor loads.
func BuildRunSpecConfigMap(run *amv1.AgentRun, agent *amv1.Agent, provider *RunProvider) (*corev1.ConfigMap, error) {
	return BuildRunSpecConfigMapWithTools(run, agent, provider, nil)
}

// BuildRunSpecConfigMapWithTools is BuildRunSpecConfigMap plus the resolved
// loop-mode tool catalog written as tools.json (omitted when empty, so the
// nil-tools path is byte-identical to the original). The marshaled catalog is
// guarded against the ConfigMap size ceiling.
func BuildRunSpecConfigMapWithTools(run *amv1.AgentRun, agent *amv1.Agent, provider *RunProvider, tools []pure.Tool) (*corev1.ConfigMap, error) {
	agentJSON, err := json.Marshal(pure.Agent{Spec: agent.Spec})
	if err != nil {
		return nil, fmt.Errorf("marshal agent spec: %w", err)
	}
	runJSON, err := json.Marshal(run.Spec)
	if err != nil {
		return nil, fmt.Errorf("marshal run spec: %w", err)
	}
	data := map[string]string{
		runSpecAgentFile: string(agentJSON),
		runSpecRunFile:   string(runJSON),
	}
	if provider != nil {
		pj, err := json.Marshal(provider)
		if err != nil {
			return nil, fmt.Errorf("marshal provider: %w", err)
		}
		data[runSpecProviderFile] = string(pj)
	}
	if len(tools) > 0 {
		tj, err := json.Marshal(tools)
		if err != nil {
			return nil, fmt.Errorf("marshal tools: %w", err)
		}
		if len(tj) > runSpecToolsMaxBytes {
			return nil, fmt.Errorf("tools.json is %d bytes, exceeds the %d ceiling (ToolSpecTooLarge)", len(tj), runSpecToolsMaxBytes)
		}
		data[runSpecToolsFile] = string(tj)
	}
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      RunSpecConfigMapName(run.Name),
			Namespace: run.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "smol-agents",
				"app.kubernetes.io/component": "agent-run",
				"agents.smol-agents.ai/agent": agent.Name,
				"agents.smol-agents.ai/run":   run.Name,
			},
		},
		Data: data,
	}, nil
}

// runSpecVolume + mount wire the spec ConfigMap into the run container.
func runSpecVolume(runName string) corev1.Volume {
	return corev1.Volume{
		Name: runSpecVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: RunSpecConfigMapName(runName)},
			},
		},
	}
}

func runSpecMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: runSpecVolumeName, MountPath: RunSpecMountPath, ReadOnly: true}
}
