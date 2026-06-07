package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// signIDToken builds a real RS256 JWT for the JWKS verifier test.
func signIDToken(t *testing.T, k *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	b64 := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	hdr := b64(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	body := b64(claims)
	signing := hdr + "." + body
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// jwksServer serves a JWKS containing k's public half under kid.
func jwksServer(t *testing.T, k *rsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()
	n := base64.RawURLEncoding.EncodeToString(k.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.PublicKey.E)).Bytes())
	jwks := map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": kid, "n": n, "e": e}}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
}

func TestJWKSVerifier(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := jwksServer(t, k, "kid-1")
	defer srv.Close()

	v := &JWKSVerifier{
		Issuer: "https://dex.example.com", Audience: "agentterminal",
		JWKSURL: srv.URL, Now: func() time.Time { return time.Unix(1000, 0) },
	}

	// Happy path: valid RS256 token → subject (email preferred).
	tok := signIDToken(t, k, "kid-1", map[string]any{
		"iss": "https://dex.example.com", "aud": "agentterminal", "exp": 2000, "sub": "u1", "email": "alice@example.com",
	})
	sub, err := v.Verify(context.Background(), tok)
	if err != nil || sub != "alice@example.com" {
		t.Fatalf("Verify = (%q, %v), want alice@example.com", sub, err)
	}

	// Wrong audience → rejected.
	bad := signIDToken(t, k, "kid-1", map[string]any{"iss": "https://dex.example.com", "aud": "someone-else", "exp": 2000, "sub": "u1"})
	if _, err := v.Verify(context.Background(), bad); err == nil {
		t.Error("wrong audience must be rejected")
	}

	// Expired → rejected.
	exp := signIDToken(t, k, "kid-1", map[string]any{"iss": "https://dex.example.com", "aud": "agentterminal", "exp": 999, "sub": "u1"})
	if _, err := v.Verify(context.Background(), exp); err == nil {
		t.Error("expired token must be rejected")
	}

	// Wrong issuer → rejected.
	iss := signIDToken(t, k, "kid-1", map[string]any{"iss": "https://evil.example.com", "aud": "agentterminal", "exp": 2000, "sub": "u1"})
	if _, err := v.Verify(context.Background(), iss); err == nil {
		t.Error("wrong issuer must be rejected")
	}

	// Signature by a different key → rejected (forged token).
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := signIDToken(t, other, "kid-1", map[string]any{"iss": "https://dex.example.com", "aud": "agentterminal", "exp": 2000, "sub": "u1"})
	if _, err := v.Verify(context.Background(), forged); err == nil {
		t.Error("token signed by an unknown key must be rejected")
	}
}
