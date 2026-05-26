package builders

import "testing"

func TestImage_DefaultAndOverride(t *testing.T) {
	// Default → the published ghcr location.
	if got := Image("agent"); got != "ghcr.io/smol-platform/smol-agents/agent:0.1.0" {
		t.Errorf("default Image(agent) = %q", got)
	}

	// Env repoints registry + tag (forks / private registries).
	t.Setenv(EnvImageRegistry, "ghcr.io/acme/sa")
	t.Setenv(EnvImageTag, "v2")
	if got := Image("secret-proxy"); got != "ghcr.io/acme/sa/secret-proxy:v2" {
		t.Errorf("overridden Image(secret-proxy) = %q", got)
	}
}
