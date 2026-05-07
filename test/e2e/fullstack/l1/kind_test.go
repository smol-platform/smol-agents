//go:build e2e_l1

package l1

import (
	"context"
	"testing"
	"time"

	"github.com/stigen/knative-agents/test/e2e/fullstack/shared"
)

// TestL1 brings up a kind cluster (reuses scripts/kind-verify.sh)
// and runs every scenario whose capabilities are satisfied by L1's
// CapKubernetes | CapEBPF | CapNetworkEgress (plus CapSPIRE when
// SPIRE is detected in-cluster).
//
// Skipped if kind/kubectl/docker aren't all available.
func TestL1(t *testing.T) {
	if !kindAvailable() {
		t.Skip("kind / kubectl / docker not available; skipping L1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	env, err := kindUp(ctx)
	if err != nil {
		t.Fatalf("kind up: %v", err)
	}
	t.Cleanup(func() {
		shutdown, c := context.WithTimeout(context.Background(), 2*time.Minute)
		defer c()
		if err := env.Cleanup(shutdown); err != nil {
			t.Logf("cleanup warning: %v", err)
		}
	})

	shared.RunAll(t, env, shared.All())
}

// TestL1_EnvImplementsInterface is a build-time check; doesn't need
// a cluster.
func TestL1_EnvImplementsInterface(t *testing.T) {
	var _ shared.Env = (*kindEnv)(nil)
}
