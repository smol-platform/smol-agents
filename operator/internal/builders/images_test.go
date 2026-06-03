package builders

import "testing"

// Default resolution and the two GLOBAL overrides (kept backward compatible).
func TestImage_Defaults_And_GlobalOverrides(t *testing.T) {
	if got := Image("agent"); got != "ghcr.io/smol-platform/smol-agents/agent:0.2.1" {
		t.Errorf("default Image(agent) = %q", got)
	}

	t.Setenv(EnvImageRegistry, "ghcr.io/acme/sa")
	t.Setenv(EnvImageTag, "v2")
	if got := Image("secret-proxy"); got != "ghcr.io/acme/sa/secret-proxy:v2" {
		t.Errorf("global override Image(secret-proxy) = %q", got)
	}
}

// Per-component TAG override wins over the global tag, keeps the global
// registry. This is the primary use case — bumping a single binary without
// republishing the whole image set at the same version.
func TestImage_PerComponentTag_WinsOverGlobalTag(t *testing.T) {
	t.Setenv(EnvImageTag, "0.1.0")
	t.Setenv("SMOL_AGENTS_IMAGE_AGENT_TAG", "0.1.3")

	if got := Image("agent"); got != "ghcr.io/smol-platform/smol-agents/agent:0.1.3" {
		t.Errorf("agent should pick up its per-component tag: got %q", got)
	}
	// Other components are unaffected and use the global tag.
	if got := Image("secret-proxy"); got != "ghcr.io/smol-platform/smol-agents/secret-proxy:0.1.0" {
		t.Errorf("secret-proxy should fall back to the global tag: got %q", got)
	}
}

// Per-component FULL REF wins outright over everything — registry, global tag,
// and per-component tag. Useful for "this one component lives in a different
// registry / pin to a specific digest" scenarios.
func TestImage_PerComponentFullRef_Wins(t *testing.T) {
	t.Setenv(EnvImageRegistry, "ghcr.io/acme/sa")
	t.Setenv(EnvImageTag, "v2")
	t.Setenv("SMOL_AGENTS_IMAGE_AGENT_TAG", "v3")
	t.Setenv("SMOL_AGENTS_IMAGE_AGENT", "private.example/team/agent@sha256:deadbeef")

	if got := Image("agent"); got != "private.example/team/agent@sha256:deadbeef" {
		t.Errorf("full-ref override should win outright: got %q", got)
	}
	// Untouched components still follow the rest of the resolution chain.
	if got := Image("ebpf-loader"); got != "ghcr.io/acme/sa/ebpf-loader:v2" {
		t.Errorf("ebpf-loader unaffected by per-agent overrides: got %q", got)
	}
}

// The kebab-case → UPPER_SNAKE conversion must work for multi-hyphen names so
// every spawned component has a usable override knob.
func TestImage_KebabToUpperSnakeEnvSuffix(t *testing.T) {
	cases := map[string]string{
		"agent":           "AGENT",
		"secret-proxy":    "SECRET_PROXY",
		"ebpf-loader":     "EBPF_LOADER",
		"agentfs-sidecar": "AGENTFS_SIDECAR",
	}
	for name, want := range cases {
		if got := componentEnvSuffix(name); got != want {
			t.Errorf("componentEnvSuffix(%q) = %q, want %q", name, got, want)
		}
	}

	// And the corresponding env vars resolve end-to-end:
	t.Setenv("SMOL_AGENTS_IMAGE_AGENTFS_SIDECAR_TAG", "0.2")
	if got := Image("agentfs-sidecar"); got != "ghcr.io/smol-platform/smol-agents/agentfs-sidecar:0.2" {
		t.Errorf("agentfs-sidecar per-component tag = %q", got)
	}
}
