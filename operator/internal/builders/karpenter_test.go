package builders

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1 "github.com/stigen/smol-agents/operator/api/v1"
)

func sampleANP() *v1.AgentNodePool {
	anp := &v1.AgentNodePool{}
	anp.Name = "kata-arm64"
	anp.Spec.Isolation = "kata-fc"
	anp.Spec.Arch = "arm64"
	anp.Spec.InstanceFamilies = []string{"c7gd", "m7gd"}
	anp.Spec.CapacityType = []string{"on-demand"}
	anp.Spec.Bootstrap.Mode = "UserData"
	anp.Spec.Bootstrap.Distro = "al2023"
	anp.Spec.ThinPool = v1.ThinPoolConfig{Backing: "instance-store", DataSize: "50Gi", MetaSize: "5Gi"}
	return anp
}

func sampleDefaults() KarpenterDefaults {
	return KarpenterDefaults{
		AMIFamily:                 "Custom",
		Role:                      "KarpenterNodeRole-k0s",
		SubnetSelectorTags:        map[string]string{"karpenter.sh/discovery": "k0s"},
		SecurityGroupSelectorTags: map[string]string{"karpenter.sh/discovery": "k0s"},
		BaseAMISelector:           []v1.AMISelectorTerm{{Tags: map[string]string{"k0s-join": "true"}}},
		JoinUserData:              "#!/bin/bash\nk0s install worker --token-file /run/k0s/token\n",
	}
}

// requirementKeys flattens the NodePool requirement keys → values.
func requirementKeys(t *testing.T, np *unstructured.Unstructured) map[string][]string {
	t.Helper()
	reqs, found, err := unstructured.NestedSlice(np.Object, "spec", "template", "spec", "requirements")
	if err != nil || !found {
		t.Fatalf("requirements missing: found=%v err=%v", found, err)
	}
	out := map[string][]string{}
	for _, r := range reqs {
		m := r.(map[string]interface{})
		key := m["key"].(string)
		var vals []string
		for _, v := range m["values"].([]interface{}) {
			vals = append(vals, v.(string))
		}
		out[key] = vals
	}
	return out
}

func TestBuildKarpenterNodePool_KataRequiresMetal(t *testing.T) {
	anp := sampleANP()
	np := BuildKarpenterNodePool(anp, "anp-kata-arm64")

	if got := np.GetAPIVersion(); got != "karpenter.sh/v1" {
		t.Errorf("apiVersion = %q, want karpenter.sh/v1", got)
	}
	if got := np.GetKind(); got != "NodePool" {
		t.Errorf("kind = %q, want NodePool", got)
	}
	if got := np.GetName(); got != "anp-kata-arm64" {
		t.Errorf("name = %q, want anp-kata-arm64", got)
	}
	if got := np.GetLabels()[PoolLabelKey]; got != "kata-arm64" {
		t.Errorf("pool label = %q, want kata-arm64", got)
	}

	keys := requirementKeys(t, np)
	for _, want := range []string{
		"kubernetes.io/arch",
		"karpenter.sh/capacity-type",
		"karpenter.k8s.aws/instance-family",
		"karpenter.k8s.aws/instance-size",
	} {
		if _, ok := keys[want]; !ok {
			t.Errorf("requirement %q missing", want)
		}
	}
	if size := keys["karpenter.k8s.aws/instance-size"]; len(size) != 1 || size[0] != "metal" {
		t.Errorf("instance-size = %v, want [metal]", size)
	}

	// Dedicated taint so general workloads stay off.
	taints, _, _ := unstructured.NestedSlice(np.Object, "spec", "template", "spec", "taints")
	if len(taints) != 1 {
		t.Fatalf("want 1 taint, got %d", len(taints))
	}
	tt := taints[0].(map[string]interface{})
	if tt["key"] != IsolationTaintKey || tt["value"] != "kata-fc" || tt["effect"] != "NoSchedule" {
		t.Errorf("taint = %v", tt)
	}

	ref, _, _ := unstructured.NestedString(np.Object, "spec", "template", "spec", "nodeClassRef", "name")
	if ref != "anp-kata-arm64" {
		t.Errorf("nodeClassRef.name = %q, want anp-kata-arm64", ref)
	}
	cp, _, _ := unstructured.NestedString(np.Object, "spec", "disruption", "consolidationPolicy")
	if cp != "WhenEmpty" {
		t.Errorf("consolidationPolicy = %q, want WhenEmpty (conservative default)", cp)
	}
}

func TestBuildKarpenterNodePool_GvisorNoMetal(t *testing.T) {
	anp := sampleANP()
	anp.Spec.Isolation = "gvisor"
	np := BuildKarpenterNodePool(anp, "anp-x")
	if _, ok := requirementKeys(t, np)["karpenter.k8s.aws/instance-size"]; ok {
		t.Error("gvisor pool must not force metal (no KVM needed)")
	}
}

func TestBuildKarpenterEC2NodeClass_UserDataComposesJoinPlusKata(t *testing.T) {
	anp := sampleANP()
	nc := BuildKarpenterEC2NodeClass(anp, sampleDefaults())

	if got := nc.GetAPIVersion(); got != "karpenter.k8s.aws/v1" {
		t.Errorf("apiVersion = %q, want karpenter.k8s.aws/v1", got)
	}
	if got := nc.GetKind(); got != "EC2NodeClass" {
		t.Errorf("kind = %q, want EC2NodeClass", got)
	}
	fam, _, _ := unstructured.NestedString(nc.Object, "spec", "amiFamily")
	if fam != "Custom" {
		t.Errorf("amiFamily = %q, want Custom", fam)
	}

	ud, _, _ := unstructured.NestedString(nc.Object, "spec", "userData")
	for _, want := range []string{
		"k0s install worker",           // existing join snippet preserved
		"dmsetup create kata-thinpool", // thin-pool layer
		"kata-static-3.10.0",           // UserData mode downloads + installs kata
	} {
		if !strings.Contains(ud, want) {
			t.Errorf("userData missing %q\n---\n%s", want, ud)
		}
	}

	// UserData mode selects the base join-capable AMI.
	terms, _, _ := unstructured.NestedSlice(nc.Object, "spec", "amiSelectorTerms")
	if len(terms) != 1 {
		t.Fatalf("want 1 amiSelectorTerm, got %d", len(terms))
	}
	tags, _, _ := unstructured.NestedStringMap(map[string]interface{}{"t": terms[0]}, "t", "tags")
	if tags["k0s-join"] != "true" {
		t.Errorf("amiSelectorTerms = %v, want base k0s-join image", terms[0])
	}
}

func TestBuildKarpenterEC2NodeClass_PrebakedSkipsKataInstall(t *testing.T) {
	anp := sampleANP()
	anp.Spec.Bootstrap.Mode = "PrebakedAMI"
	anp.Spec.Bootstrap.AMISelector = []v1.AMISelectorTerm{{Tags: map[string]string{"kata-ready": "true"}}}
	nc := BuildKarpenterEC2NodeClass(anp, sampleDefaults())

	ud, _, _ := unstructured.NestedString(nc.Object, "spec", "userData")
	if !strings.Contains(ud, "dmsetup create kata-thinpool") {
		t.Error("prebaked still needs the per-launch thin-pool")
	}
	if strings.Contains(ud, "kata-static-3.10.0") {
		t.Error("prebaked AMI must not re-download/install kata at boot")
	}
	terms, _, _ := unstructured.NestedSlice(nc.Object, "spec", "amiSelectorTerms")
	tags, _, _ := unstructured.NestedStringMap(map[string]interface{}{"t": terms[0]}, "t", "tags")
	if tags["kata-ready"] != "true" {
		t.Errorf("amiSelectorTerms = %v, want kata-ready image", terms[0])
	}
}
