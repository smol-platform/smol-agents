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
