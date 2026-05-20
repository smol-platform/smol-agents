package builders

import (
	"strings"
	"testing"
)

func TestBuildClusterAutoscalerConfigMap(t *testing.T) {
	anp := sampleANP() // kata-fc, arm64, UserData/al2023, families c7gd,m7gd
	cm := BuildClusterAutoscalerConfigMap(anp, "knative-agents-system", sampleDefaults())

	if cm.Name != "anp-kata-arm64-clusterautoscaler" {
		t.Errorf("name = %q", cm.Name)
	}
	if cm.Namespace != "knative-agents-system" {
		t.Errorf("namespace = %q", cm.Namespace)
	}
	if cm.Kind != "ConfigMap" {
		t.Errorf("TypeMeta.Kind = %q (needed for SSA)", cm.Kind)
	}

	d := cm.Data
	if d["provider"] != "cluster-autoscaler" {
		t.Errorf("provider = %q", d["provider"])
	}
	if d["instanceFamilies"] != "c7gd,m7gd" {
		t.Errorf("instanceFamilies = %q", d["instanceFamilies"])
	}
	// Coupling keys the kata pod carries — the ASG must surface the same.
	if d["poolLabel"] != "agents.stigen.ai/pool=kata-arm64" {
		t.Errorf("poolLabel = %q", d["poolLabel"])
	}
	if d["isolationTaint"] != "agents.stigen.ai/isolation=kata-fc:NoSchedule" {
		t.Errorf("isolationTaint = %q", d["isolationTaint"])
	}
	// CAS auto-discovery + node-template tags mirror the coupling keys.
	for _, want := range []string{
		`k8s.io/cluster-autoscaler/enabled: "true"`,
		"node-template/label/agents.stigen.ai/pool",
		"node-template/taint/agents.stigen.ai/isolation",
	} {
		if !strings.Contains(d["requiredASGTags"], want) {
			t.Errorf("requiredASGTags missing %q\n%s", want, d["requiredASGTags"])
		}
	}
	// userData is the same kata layer (existing join + recipe) as Karpenter's.
	for _, want := range []string{"k0s install worker", "dmsetup create kata-thinpool", "kata-static-3.10.0"} {
		if !strings.Contains(d["userData"], want) {
			t.Errorf("userData missing %q", want)
		}
	}
}

func TestBuildClusterAutoscalerConfigMap_PrebakedSkipsKataDownload(t *testing.T) {
	anp := sampleANP()
	anp.Spec.Bootstrap.Mode = "PrebakedAMI"
	cm := BuildClusterAutoscalerConfigMap(anp, "ns", sampleDefaults())
	if strings.Contains(cm.Data["userData"], "kata-static-3.10.0") {
		t.Error("prebaked AMI must not re-download kata in the CAS launch-template userData")
	}
	if !strings.Contains(cm.Data["userData"], "dmsetup create kata-thinpool") {
		t.Error("prebaked still needs the per-launch thin-pool")
	}
}
