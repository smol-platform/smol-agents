//go:build e2e_l0

package l0

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/stigen/smol-agents/test/e2e/fullstack/shared"
)

// TestL0 brings up the docker-compose stack and runs every scenario
// whose capabilities are satisfied by L0's CapSPIRE | CapWireGuard.
//
// Skipped if `docker compose` is unavailable (CI without docker, or
// developer mode without OrbStack/Docker Desktop running).
func TestL0(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not on PATH; L0 requires docker compose")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	env, err := composeUp(ctx)
	if err != nil {
		t.Fatalf("compose up: %v", err)
	}
	t.Cleanup(func() {
		shutdown, c := context.WithTimeout(context.Background(), 60*time.Second)
		defer c()
		if err := env.Cleanup(shutdown); err != nil {
			t.Logf("cleanup warning: %v", err)
		}
	})

	shared.RunAll(t, env, shared.All())
}

// TestL0_EnvImplementsInterface is a build-time check that
// composeEnv satisfies the shared.Env contract. Doesn't require
// docker.
func TestL0_EnvImplementsInterface(t *testing.T) {
	var _ shared.Env = (*composeEnv)(nil)
}
