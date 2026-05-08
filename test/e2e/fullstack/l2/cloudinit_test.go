//go:build e2e_l2

package l2

import (
	"strings"
	"testing"
)

func TestRenderCloudInit_FullTemplate(t *testing.T) {
	out, err := renderCloudInit(userDataInputs{
		ArtifactBucket: "test-bucket",
		ECRRegistry:    "123.dkr.ecr.us-east-2.amazonaws.com",
		ImageTag:       "abc123",
		RunID:          "deadbeef",
		Distro:         DistroAL2023,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#cloud-config",
		"k0s install controller --single",
		"test-bucket",
		"abc123",
		"deadbeef",
		"123.dkr.ecr.us-east-2.amazonaws.com",
		"l2-bootstrap.READY",
		"cert-manager.yaml",
		"l2-bootstrap.FAILED",
		"--for=condition=Ready",
		"app=spire-server",
		"app=spire-agent",
		"crd/knativeagents.agents.stigen.ai",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered template missing %q", want)
		}
	}
}

func TestRenderCloudInit_Minimal(t *testing.T) {
	t.Setenv("L2_BOOTSTRAP_MINIMAL", "1")
	out, err := renderCloudInit(userDataInputs{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "l2-bootstrap.READY") {
		t.Errorf("minimal stub doesn't drop sentinel: %s", out)
	}
	if strings.Contains(out, "k0s install") {
		t.Error("minimal stub includes full bootstrap")
	}
}

// TestRenderUserData_PerDistro asserts every distro's template
// renders cleanly with the standard inputs and produces the
// distro-specific markers that distinguish it.
func TestRenderUserData_PerDistro(t *testing.T) {
	in := userDataInputs{
		ArtifactBucket: "test-bucket",
		ECRRegistry:    "123.dkr.ecr.us-east-2.amazonaws.com",
		ImageTag:       "abc123",
		RunID:          "deadbeef",
	}
	cases := map[Distro][]string{
		DistroAL2023:       {"#cloud-config", "test-bucket", "containerd", "k0s install controller"},
		DistroUbuntu:       {"#cloud-config", "test-bucket", "package_update", "k0s install controller"},
		DistroBottlerocket: {"settings.host-containers.admin", "enabled = false"},
		DistroFlatcar:      {"#cloud-config", "runcmd"},
	}
	for d, mustContain := range cases {
		t.Run(string(d), func(t *testing.T) {
			out, err := d.UserData(in)
			if err != nil {
				t.Fatalf("render %s: %v", d, err)
			}
			for _, want := range mustContain {
				if !strings.Contains(out, want) {
					t.Errorf("%s template missing %q", d, want)
				}
			}
		})
	}
}

func TestResolveDistro(t *testing.T) {
	cases := []struct {
		env  string
		want Distro
	}{
		{"", DistroAL2023},
		{"al2023", DistroAL2023},
		{"AL2023", DistroAL2023},
		{"ubuntu", DistroUbuntu},
		{"bottlerocket", DistroBottlerocket},
		{"flatcar", DistroFlatcar},
		{"unknown", DistroAL2023}, // default fallback
	}
	for _, c := range cases {
		t.Setenv("L2_DISTRO", c.env)
		if got := ResolveDistro(); got != c.want {
			t.Errorf("L2_DISTRO=%q: got %s, want %s", c.env, got, c.want)
		}
	}
}
