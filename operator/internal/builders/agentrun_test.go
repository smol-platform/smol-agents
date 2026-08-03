package builders

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

// knative-agents-qzy: a run/session pod must never carry a live kube-apiserver
// token by default — the M1.18 egress floor deliberately allows the apiserver,
// so a mounted token would be a reachable credential for every run. Only the
// A2A path (AllowA2AToken) re-enables it.
func TestBuildAgentRunPod_AutomountServiceAccountTokenDefaultsFalse(t *testing.T) {
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "tenant-b"}}
	run := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "run-002", Namespace: "tenant-b"}}
	run.Spec.AgentRef = "bob"

	pod := BuildAgentRunPod(run, agent)
	if pod.Spec.AutomountServiceAccountToken == nil {
		t.Fatal("AutomountServiceAccountToken is nil, want explicit false")
	}
	if *pod.Spec.AutomountServiceAccountToken != false {
		t.Errorf("AutomountServiceAccountToken = %v, want false", *pod.Spec.AutomountServiceAccountToken)
	}
}

func TestAllowA2AToken_FlipsToTrue(t *testing.T) {
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "tenant-b"}}
	run := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "run-003", Namespace: "tenant-b"}}
	run.Spec.AgentRef = "bob"

	pod := BuildAgentRunPod(run, agent)
	AllowA2AToken(pod)
	if pod.Spec.AutomountServiceAccountToken == nil || !*pod.Spec.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken after AllowA2AToken = %v, want true", pod.Spec.AutomountServiceAccountToken)
	}
}
