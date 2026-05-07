//go:build e2e_l2

package l2

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/stigen/knative-agents/test/e2e/fullstack/shared"
)

// TestL2 provisions a Spot c6gd.metal in us-east-2, waits for SSM,
// tears it down. Skipped unless STIGEN_AWS_PROFILE=stigen + the
// Terraform module is applied (the IAM role it relies on must exist).
//
// Cost per run: ~$0.22 at $1.10/hr Spot × ~12 min.
func TestL2(t *testing.T) {
	if os.Getenv("AWS_PROFILE") != "stigen" {
		t.Skip("AWS_PROFILE != stigen; skipping L2 (set AWS_PROFILE=stigen to run)")
	}
	if err := ensureRegion(); err != nil {
		t.Skipf("region check: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cluster, err := Provision(ctx)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() {
		shutdown, c := context.WithTimeout(context.Background(), 5*time.Minute)
		defer c()
		if err := cluster.Teardown(shutdown); err != nil {
			t.Logf("teardown warning: %v", err)
		}
	})

	t.Logf("L2 cluster up: instance=%s public_dns=%s run_id=%s",
		cluster.InstanceID, cluster.PublicDNS, cluster.RunID)

	// Wait for the cloud-init sentinel (set by the bootstrap script
	// after k0s + Kata + manifests are applied).
	env := &l2Env{cluster: cluster}
	err = env.WaitFor(ctx, "l2-bootstrap.READY", 8*time.Minute,
		func(ctx context.Context) bool {
			out, err := env.runSSM(ctx, "test -f /var/log/l2-bootstrap.READY && echo READY",
				15*time.Second)
			return err == nil && bytes.Contains(out, []byte("READY"))
		})
	if err != nil {
		t.Fatalf("bootstrap sentinel never appeared: %v", err)
	}
	t.Log("L2 bootstrap sentinel observed; running scenarios")

	shared.RunAll(t, env, shared.All())
}

// TestL2_RegionGate confirms the driver refuses non-us-east-2.
func TestL2_RegionGate(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	if err := ensureRegion(); err == nil {
		t.Error("expected region rejection")
	}
}
