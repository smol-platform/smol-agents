//go:build e2e_l2

package l2

import (
	"context"
	"os"
	"testing"
	"time"
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

	// Smoke: SSM was ready, instance is running. Full scenarios run
	// once cloud-init wires k0s + Kata (T-4.*).
}

// TestL2_RegionGate confirms the driver refuses non-us-east-2.
func TestL2_RegionGate(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	if err := ensureRegion(); err == nil {
		t.Error("expected region rejection")
	}
}
