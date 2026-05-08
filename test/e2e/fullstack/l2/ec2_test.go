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

// TestL2 is the full L2 integration: provision Spot c6gd.metal,
// wait for the cloud-init health gate, run every cross-ring
// scenario via SSM, terminate. ~12 min, ~$0.22/run.
//
// Skipped unless AWS_PROFILE=stigen and us-east-2 is selected.
func TestL2(t *testing.T) {
	env, ok := provisionAndWaitReady(t)
	if !ok {
		return
	}
	shared.RunAll(t, env, shared.All())
}

// TestL2_Smoke is the cheap CI variant: provision, wait for the
// READY sentinel, terminate. No scenarios. ~6 min, ~$0.10/run —
// catches cloud-init drift (kata bundle move, image pull failure,
// CRD-name typo) without paying the full scenario-suite tax.
//
// Run with: go test -tags=e2e_l2 -run TestL2_Smoke ./test/e2e/fullstack/l2/...
func TestL2_Smoke(t *testing.T) {
	if _, ok := provisionAndWaitReady(t); ok {
		t.Log("L2 smoke passed: bootstrap reached READY")
	}
}

// TestL2_RegionGate confirms the driver refuses non-us-east-2.
func TestL2_RegionGate(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	if err := ensureRegion(); err == nil {
		t.Error("expected region rejection")
	}
}

// provisionAndWaitReady spins up an instance, registers teardown,
// and waits for /var/log/l2-bootstrap.{READY,FAILED}. Returns the
// l2Env and ok=true on READY; calls t.Skip when AWS_PROFILE is
// missing and t.Fatal when the bootstrap reports FAILED.
func provisionAndWaitReady(t *testing.T) (env *l2Env, ok bool) {
	t.Helper()
	if os.Getenv("AWS_PROFILE") == "" {
		t.Skip("AWS_PROFILE unset; skipping L2 (set AWS_PROFILE to a sandbox account to run)")
		return nil, false
	}
	if err := ensureRegion(); err != nil {
		t.Skipf("region check: %v", err)
		return nil, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)

	cluster, err := Provision(ctx)
	if err != nil {
		t.Fatalf("Provision: %v", err)
		return nil, false
	}
	t.Cleanup(func() {
		if os.Getenv("L2_KEEP_INSTANCE") != "" {
			t.Logf("L2_KEEP_INSTANCE set; leaving %s alive for debugging — "+
				"sweeper Lambda will reclaim it within 1h or run "+
				"`make e2e-clean-aws`", cluster.InstanceID)
			return
		}
		shutdown, c := context.WithTimeout(context.Background(), 5*time.Minute)
		defer c()
		if err := cluster.Teardown(shutdown); err != nil {
			t.Logf("teardown warning: %v", err)
		}
	})

	t.Logf("L2 cluster up: instance=%s public_dns=%s run_id=%s",
		cluster.InstanceID, cluster.PublicDNS, cluster.RunID)

	// Wait for either sentinel. Capturing which one fired in the
	// closure avoids a second SSM round-trip after WaitFor returns.
	env = &l2Env{ssm: cluster.ssmc, instanceID: cluster.InstanceID}
	var observed string
	err = env.WaitFor(ctx, "l2-bootstrap.{READY,FAILED}", 8*time.Minute,
		func(ctx context.Context) bool {
			out, err := env.runSSM(ctx,
				"test -f "+sentinelREADY+"  && echo READY ; "+
					"test -f "+sentinelFAILED+" && echo FAILED ; true",
				15*time.Second)
			if err != nil {
				return false
			}
			switch {
			case bytes.Contains(out, []byte("FAILED")):
				observed = "FAILED"
			case bytes.Contains(out, []byte("READY")):
				observed = "READY"
			}
			return observed != ""
		})
	if err != nil {
		t.Fatalf("bootstrap sentinel never appeared: %v", err)
		return nil, false
	}
	if observed == "FAILED" {
		log, _ := env.runSSM(ctx, "cat "+bootstrapLog+" 2>/dev/null || true",
			30*time.Second)
		t.Fatalf("bootstrap reported FAILED; log:\n%s", log)
		return nil, false
	}
	t.Log("L2 bootstrap sentinel observed (READY)")
	return env, true
}
