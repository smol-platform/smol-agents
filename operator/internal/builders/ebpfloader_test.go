package builders

import (
	"testing"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
)

func samplePlatform() *v1.SmolAgentPlatform {
	p := &v1.SmolAgentPlatform{}
	p.Name = "default"
	p.Spec.DefaultTrustDomain = "smol-agents.ai"
	p.Spec.EBPFLoader.Enabled = true
	p.Spec.EBPFLoader.Preset = "generic"
	return p
}

func TestLoaderPresets_AllSeven(t *testing.T) {
	want := []string{"generic", "gke-cos", "eks-bottlerocket", "aks-mariner", "k3s", "openshift", "talos"}
	for _, p := range want {
		if _, ok := LoaderPresets[p]; !ok {
			t.Errorf("missing preset %s", p)
		}
	}
	if len(LoaderPresets) != len(want) {
		t.Errorf("preset count mismatch: %d != %d", len(LoaderPresets), len(want))
	}
}

func TestBuildEBPFLoaderDaemonSet_Generic(t *testing.T) {
	ds := BuildEBPFLoaderDaemonSet(samplePlatform(), "smol-agents", "generic")
	if ds.Name != "ebpf-loader" {
		t.Errorf("name = %s", ds.Name)
	}
	if !ds.Spec.Template.Spec.HostPID {
		t.Error("hostPID should be true")
	}
	if ds.Spec.Template.Spec.Containers[0].SecurityContext.Privileged == nil ||
		!*ds.Spec.Template.Spec.Containers[0].SecurityContext.Privileged {
		t.Error("generic preset should be privileged")
	}
}

// The loader binary mounts bpffs itself when MountBPFFS=true (see
// pkg/ebpfloader/loader_linux.go), so the DaemonSet must NOT include an init
// container that tries to do it from a shell — the ebpf-loader image is
// distroless and has no /bin/sh.
func TestBuildEBPFLoaderDaemonSet_NoInitContainer(t *testing.T) {
	for name := range LoaderPresets {
		ds := BuildEBPFLoaderDaemonSet(samplePlatform(), "smol-agents", name)
		if got := len(ds.Spec.Template.Spec.InitContainers); got != 0 {
			t.Errorf("preset %q: want 0 init containers, got %d", name, got)
		}
	}
}

func TestBuildEBPFLoaderDaemonSet_MinimalCaps(t *testing.T) {
	ds := BuildEBPFLoaderDaemonSet(samplePlatform(), "smol-agents", "eks-bottlerocket")
	sc := ds.Spec.Template.Spec.Containers[0].SecurityContext
	if sc.Privileged == nil || *sc.Privileged {
		t.Error("eks-bottlerocket should NOT be privileged")
	}
	if sc.Capabilities == nil {
		t.Fatal("missing capabilities")
	}
	want := map[string]bool{"BPF": false, "PERFMON": false, "NET_ADMIN": false}
	for _, c := range sc.Capabilities.Add {
		want[string(c)] = true
	}
	for c, present := range want {
		if !present {
			t.Errorf("missing capability: %s", c)
		}
	}
}

func TestBuildEBPFLoaderDaemonSet_TalosOmitsModulesAndDebug(t *testing.T) {
	ds := BuildEBPFLoaderDaemonSet(samplePlatform(), "smol-agents", "talos")
	for _, v := range ds.Spec.Template.Spec.Volumes {
		if v.Name == "modules" || v.Name == "kernel-debug" {
			t.Errorf("talos should not mount %s", v.Name)
		}
	}
}

func TestBuildEBPFLoaderDaemonSet_UnknownFallsBackToGeneric(t *testing.T) {
	ds := BuildEBPFLoaderDaemonSet(samplePlatform(), "smol-agents", "no-such-preset")
	sc := ds.Spec.Template.Spec.Containers[0].SecurityContext
	if sc.Privileged == nil || !*sc.Privileged {
		t.Error("unknown preset should fall back to generic (privileged)")
	}
}

func TestBuildEBPFLoaderConfigMap(t *testing.T) {
	cm := BuildEBPFLoaderConfigMap(samplePlatform(), "smol-agents")
	if cm.Name != "ebpf-loader-config" {
		t.Errorf("name = %s", cm.Name)
	}
	yaml := cm.Data["config.yaml"]
	for _, want := range []string{"pinRoot:", "programs:", "syscalls", "network", "mountBPFFS: true"} {
		if !contains(yaml, want) {
			t.Errorf("yaml missing %q\n%s", want, yaml)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
