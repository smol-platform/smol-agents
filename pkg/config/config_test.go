package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadAgent_Defaults(t *testing.T) {
	t.Setenv("SMOL_AGENTS_ALLOW_INSECURE", "")
	cfg, err := LoadAgent("")
	if err != nil {
		t.Fatalf("LoadAgent empty path: %v", err)
	}
	if cfg.Mode != ModeStrict {
		t.Errorf("default mode = %q, want strict", cfg.Mode)
	}
	if cfg.TrustDomain != "stigen.ai" {
		t.Errorf("default trustDomain = %q, want stigen.ai", cfg.TrustDomain)
	}
	if cfg.Identity.RotationThreshold != 0.5 {
		t.Errorf("rotationThreshold = %v, want 0.5", cfg.Identity.RotationThreshold)
	}
	if cfg.Sandbox.RuntimeClass != "kata-fc" {
		t.Errorf("RuntimeClass = %q, want kata-fc", cfg.Sandbox.RuntimeClass)
	}
	if cfg.Secrets.MaxLeaseTTL != 15*time.Minute {
		t.Errorf("MaxLeaseTTL = %v, want 15m", cfg.Secrets.MaxLeaseTTL)
	}
}

func TestLoadAgent_FromYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.yaml")
	must(t, os.WriteFile(p, []byte(`
mode: permissive
trustDomain: example.test
transport:
  private:
    addr: 127.0.0.1:9000
secrets:
  maxLeaseTTL: 1m
`), 0o600))
	cfg, err := LoadAgent(p)
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if cfg.Mode != ModePermissive {
		t.Errorf("mode = %q", cfg.Mode)
	}
	if cfg.Transport.Private.Addr != "127.0.0.1:9000" {
		t.Errorf("private addr = %q", cfg.Transport.Private.Addr)
	}
}

func TestValidate_InsecureRequiresEnv(t *testing.T) {
	t.Setenv("SMOL_AGENTS_ALLOW_INSECURE", "")
	cfg := Agent{Mode: ModeInsecure, TrustDomain: "x", Identity: Identity{RotationThreshold: 0.5}, Secrets: Secrets{MaxLeaseTTL: time.Minute}, Runtime: Runtime{DrainTimeout: time.Second}}
	cfg.applyDefaults()
	cfg.Mode = ModeInsecure // applyDefaults won't overwrite
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error when insecure without env")
	}
}

func TestValidate_InsecureWithEnvAllowed(t *testing.T) {
	t.Setenv("SMOL_AGENTS_ALLOW_INSECURE", "1")
	cfg := Agent{}
	cfg.applyDefaults()
	cfg.Mode = ModeInsecure
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected ok with env set: %v", err)
	}
}

func TestValidate_PublicRequiresCertKey(t *testing.T) {
	cfg := Agent{}
	cfg.applyDefaults()
	cfg.Transport.Public.Addr = "0.0.0.0:8444" // no cert/key
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error: public addr without cert/key")
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("SMOL_AGENTS_MODE", "permissive")
	t.Setenv("SMOL_AGENTS_TRUST_DOMAIN", "override.test")
	cfg, err := LoadAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModePermissive {
		t.Errorf("env override mode failed: got %q", cfg.Mode)
	}
	if cfg.TrustDomain != "override.test" {
		t.Errorf("env override trustDomain failed: got %q", cfg.TrustDomain)
	}
}

func TestModeValid(t *testing.T) {
	for _, m := range []Mode{ModeInsecure, ModePermissive, ModeStrict} {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
	}
	if Mode("nope").Valid() {
		t.Error("nope should be invalid")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
}
