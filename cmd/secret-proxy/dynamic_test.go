package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/smol-platform/smol-agents/pkg/secrets"
	"github.com/smol-platform/smol-agents/pkg/trat"
)

func writeRSAKey(t *testing.T, dir string) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	p := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(p, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func dynamicConfig(t *testing.T, keyPath, peerAuth string, withTTS bool) brokerConfig {
	t.Helper()
	tts := ""
	if withTTS {
		tts = "tts:\n  jwksURL: https://tts.local/jwks\n  audience: smol-agents.ai\n"
	}
	y := fmt.Sprintf(`peerAuth: %s
backend:
  dynamic:
    provider: githubApp
    appID: "123456"
    privateKeyPath: %s
    baseURL: https://api.github.com
    scopePermissions:
      "github:repo:read": { contents: read }
%scredentialPolicy:
  - spiffeID: spiffe://smol-agents.ai/ns/t/sa/a
    scope: github:repo:read
    credential: github
    repos: [ smol-platform/app ]
`, peerAuth, keyPath, tts)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadBrokerConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadBrokerConfig: %v", err)
	}
	return cfg
}

// M1.22: a dynamic block under peerAuth=spire builds the GitHubApp backend, the
// JWKS verifier, and the credential policy.
func TestBuildDynamic_Builds(t *testing.T) {
	keyPath := writeRSAKey(t, t.TempDir())
	cfg := dynamicConfig(t, keyPath, "spire", true)

	dyn, ver, cp, err := buildDynamic(cfg)
	if err != nil {
		t.Fatalf("buildDynamic: %v", err)
	}
	gh, ok := dyn.(*secrets.GitHubAppBackend)
	if !ok || gh.AppID != "123456" || gh.PrivateKey == nil || gh.ScopePermissions["github:repo:read"]["contents"] != "read" {
		t.Errorf("github backend not wired: %+v", dyn)
	}
	jv, ok := ver.(*trat.JWKSVerifier)
	if !ok || jv.Audience != "smol-agents.ai" {
		t.Errorf("verifier not wired: %+v", ver)
	}
	if cp == nil {
		t.Error("credential policy nil")
	}
}

// M1.22 (negative): dynamic minting requires pure SPIRE attestation. A local or
// spire+local peerAuth must fail closed at startup — the sender-constraint can't
// hold if a local-uid peer can present a TraT minted for another SVID.
func TestBuildDynamic_RequiresSpire(t *testing.T) {
	keyPath := writeRSAKey(t, t.TempDir())
	for _, pa := range []string{"local", "spire+local"} {
		cfg := dynamicConfig(t, keyPath, pa, true)
		if _, _, _, err := buildDynamic(cfg); err == nil {
			t.Errorf("peerAuth=%q + dynamic must fail closed", pa)
		}
	}
	// "" (default) and "spire" are both pure SPIRE → permitted.
	for _, pa := range []string{"", "spire"} {
		cfg := dynamicConfig(t, keyPath, pa, true)
		if _, _, _, err := buildDynamic(cfg); err != nil {
			t.Errorf("peerAuth=%q (pure SPIRE) must be permitted, got %v", pa, err)
		}
	}
}

// M1.22 (negative): dynamic without a tts{} verifier block is rejected.
func TestBuildDynamic_RequiresTTS(t *testing.T) {
	keyPath := writeRSAKey(t, t.TempDir())
	cfg := dynamicConfig(t, keyPath, "spire", false)
	if _, _, _, err := buildDynamic(cfg); err == nil {
		t.Error("dynamic without tts must error")
	}
}

// No dynamic block → no mint wiring (proxy stays static-only).
func TestBuildDynamic_Absent(t *testing.T) {
	cfg := brokerConfig{PeerAuth: "spire"}
	dyn, ver, cp, err := buildDynamic(cfg)
	if err != nil || dyn != nil || ver != nil || cp != nil {
		t.Errorf("absent dynamic must yield all-nil, got dyn=%v ver=%v cp=%v err=%v", dyn, ver, cp, err)
	}
}

func TestLoadRSAKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadRSAKey(writeRSAKey(t, dir)); err != nil {
		t.Errorf("valid PKCS1 key: %v", err)
	}
	if _, err := loadRSAKey(filepath.Join(dir, "nope.pem")); err == nil {
		t.Error("missing file must error")
	}
	bad := filepath.Join(dir, "bad.pem")
	_ = os.WriteFile(bad, []byte("not pem"), 0o600)
	if _, err := loadRSAKey(bad); err == nil {
		t.Error("non-PEM must error")
	}
}
