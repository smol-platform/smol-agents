package trat

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
)

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func jwksFor(priv *rsa.PrivateKey) *jose.JSONWebKeySet {
	return &jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &priv.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig"},
	}}
}

func signTraT(t *testing.T, priv *rsa.PrivateKey, reg josejwt.Claims, custom map[string]any) string {
	t.Helper()
	sk := jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: priv, KeyID: "test"}}
	sig, err := jose.NewSigner(sk, (&jose.SignerOptions{}).WithType("txntoken+jwt"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := josejwt.Signed(sig).Claims(reg).Claims(custom).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJWKSVerifier_Verify_OK(t *testing.T) {
	priv := newKey(t)
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	compact := signTraT(t, priv,
		josejwt.Claims{
			Subject:  "spiffe://smol-agents.ai/ns/tenant-a/sa/alice-agent",
			Audience: josejwt.Audience{"smol-agents.ai"},
			Expiry:   josejwt.NewNumericDate(now.Add(30 * time.Second)),
			IssuedAt: josejwt.NewNumericDate(now),
		},
		map[string]any{"scope": "github:repo:read", "txn": "txn-123",
			"req_wl": "alice-agent", "rctx": map[string]any{"repo": "smol-platform/app"}},
	)
	v := &JWKSVerifier{Keys: StaticKeySource{jwksFor(priv)}, Audience: "smol-agents.ai", Now: func() time.Time { return now }}
	c, err := v.Verify(context.Background(), compact)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c.Subject != "spiffe://smol-agents.ai/ns/tenant-a/sa/alice-agent" || c.Scope != "github:repo:read" ||
		c.TxnID != "txn-123" || c.Audience != "smol-agents.ai" {
		t.Errorf("claims = %+v", c)
	}
	if c.ReqCtx["repo"] != "smol-platform/app" {
		t.Errorf("rctx = %v", c.ReqCtx)
	}
}

func TestJWKSVerifier_Verify_RejectsTampered(t *testing.T) {
	priv := newKey(t)
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	compact := signTraT(t, priv, josejwt.Claims{
		Subject: "x", Audience: josejwt.Audience{"smol-agents.ai"},
		Expiry: josejwt.NewNumericDate(now.Add(time.Minute)),
	}, map[string]any{"scope": "s"})
	// Flip a middle char (changes signed bytes; the last char of an
	// RSA-2048 signature only encodes 2 meaningful base64 bits).
	mid := len(compact) / 2
	tampered := compact[:mid] + flip(compact[mid]) + compact[mid+1:]
	v := &JWKSVerifier{Keys: StaticKeySource{jwksFor(priv)}, Audience: "smol-agents.ai", Now: func() time.Time { return now }}
	if _, err := v.Verify(context.Background(), tampered); err == nil {
		t.Fatal("expected tampered token to fail")
	}
}

func TestJWKSVerifier_Verify_RejectsExpired(t *testing.T) {
	priv := newKey(t)
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	compact := signTraT(t, priv, josejwt.Claims{
		Subject: "x", Audience: josejwt.Audience{"smol-agents.ai"},
		Expiry: josejwt.NewNumericDate(now.Add(-time.Second)),
	}, map[string]any{"scope": "s"})
	v := &JWKSVerifier{Keys: StaticKeySource{jwksFor(priv)}, Audience: "smol-agents.ai", Now: func() time.Time { return now }}
	if _, err := v.Verify(context.Background(), compact); err == nil {
		t.Fatal("expected expired token to fail")
	}
}

func TestJWKSVerifier_Verify_RejectsWrongAudience(t *testing.T) {
	priv := newKey(t)
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	compact := signTraT(t, priv, josejwt.Claims{
		Subject: "x", Audience: josejwt.Audience{"other.example"},
		Expiry: josejwt.NewNumericDate(now.Add(time.Minute)),
	}, map[string]any{"scope": "s"})
	v := &JWKSVerifier{Keys: StaticKeySource{jwksFor(priv)}, Audience: "smol-agents.ai", Now: func() time.Time { return now }}
	if _, err := v.Verify(context.Background(), compact); err == nil {
		t.Fatal("expected wrong-audience token to fail")
	}
}

func TestJWKSVerifier_Verify_RejectsWrongKey(t *testing.T) {
	signer, attacker := newKey(t), newKey(t)
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	compact := signTraT(t, signer, josejwt.Claims{
		Subject: "x", Audience: josejwt.Audience{"smol-agents.ai"},
		Expiry: josejwt.NewNumericDate(now.Add(time.Minute)),
	}, map[string]any{"scope": "s"})
	// Verify against a JWKS that does NOT contain the signer's key.
	v := &JWKSVerifier{Keys: StaticKeySource{jwksFor(attacker)}, Audience: "smol-agents.ai", Now: func() time.Time { return now }}
	if _, err := v.Verify(context.Background(), compact); err == nil {
		t.Fatal("expected token signed by an unknown key to fail")
	}
}

func flip(b byte) string {
	if b == 'A' {
		return "B"
	}
	return "A"
}
