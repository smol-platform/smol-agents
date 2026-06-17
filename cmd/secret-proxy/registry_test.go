package main

import (
	"testing"

	"github.com/smol-platform/smol-agents/pkg/secrets"
)

// c5r.7: static-lease backends are selected via a registration map, so a backend
// (Vault, cloud-SM, …) registers out-of-tree instead of editing a switch; an
// unregistered kind errors rather than hitting a hardcoded "not yet wired" stub.
func TestRegisterBackend(t *testing.T) {
	const kind = "test-fake"
	RegisterBackend(kind, func(cfg brokerConfig) (secrets.Backend, error) {
		return secrets.NewStaticBackend(), nil
	})
	t.Cleanup(func() { delete(backendBuilders, kind) })

	cfg := brokerConfig{}
	cfg.Backend.Kind = kind
	if b, err := buildBackend(cfg); err != nil || b == nil {
		t.Fatalf("registered backend: b=%v err=%v", b, err)
	}
	cfg.Backend.Kind = "vault" // not registered (until wired)
	if _, err := buildBackend(cfg); err == nil {
		t.Error("unregistered backend kind must error")
	}
	cfg.Backend.Kind = "static" // seeded
	if _, err := buildBackend(cfg); err != nil {
		t.Errorf("seeded static backend: %v", err)
	}
}

// c5r.7: dynamic-mint providers are selected via a registration map too — a
// GitLab / Vault-dynamic minter registers out-of-tree; an unregistered provider
// errors. The common peerAuth guard + verifier + credential policy still apply.
func TestRegisterDynamicBackend(t *testing.T) {
	keyPath := writeRSAKey(t, t.TempDir())
	const provider = "test-minter"
	called := false
	RegisterDynamicBackend(provider, func(cfg brokerConfig) (secrets.DynamicBackend, error) {
		called = true
		return &secrets.GitHubAppBackend{}, nil
	})
	t.Cleanup(func() { delete(dynamicBackendBuilders, provider) })

	cfg := dynamicConfig(t, keyPath, "spire", true)
	cfg.Backend.Dynamic.Provider = provider
	dyn, ver, cp, err := buildDynamic(cfg)
	if err != nil || dyn == nil || ver == nil || cp == nil {
		t.Fatalf("registered minter: dyn=%v ver=%v cp=%v err=%v", dyn, ver, cp, err)
	}
	if !called {
		t.Error("registered minter constructor was not called")
	}

	cfg.Backend.Dynamic.Provider = "no-such-minter"
	if _, _, _, err := buildDynamic(cfg); err == nil {
		t.Error("unregistered dynamic provider must error")
	}
}
