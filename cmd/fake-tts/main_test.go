package main

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/smol-platform/smol-agents/pkg/trat"
)

func newDiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeSubjectToken builds an unsigned compact JWT carrying sub (fake-tts only
// decodes it; it doesn't verify the subject_token's signature).
func fakeSubjectToken(sub string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + sub + `"}`))
	return "h." + payload + ".sig"
}

// The real ExchangeMinter and JWKSVerifier must interoperate with fake-tts:
// mint a TraT, then verify it against the served JWKS.
func TestFakeTTS_MintVerifyRoundTrip(t *testing.T) {
	srv, err := newServer("smol-platform/app", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	srv.logger = newDiscardLogger()
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	const agent = "spiffe://smol-agents.ai/ns/tenant-a/sa/agent"
	const td = "spiffe://smol-agents.ai"

	minter := &trat.ExchangeMinter{
		TokenURL:        ts.URL + "/token",
		SubjectAudience: "spiffe://smol-agents.ai/ns/security/sa/tts",
		SubjectToken: func(_ context.Context, _ string) (string, error) {
			return fakeSubjectToken(agent), nil
		},
	}
	compact, err := minter.Token(context.Background(), trat.ExchangeParams{
		Scope: "github:repo:read", Audience: td,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	verifier := &trat.JWKSVerifier{
		Keys:     &trat.HTTPKeySource{URL: ts.URL + "/jwks"},
		Audience: td,
	}
	claims, err := verifier.Verify(context.Background(), compact)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	if claims.Subject != agent {
		t.Errorf("sub = %q, want %q", claims.Subject, agent)
	}
	if claims.ReqWL != agent {
		t.Errorf("req_wl = %q, want %q (sender-constraint binding)", claims.ReqWL, agent)
	}
	if claims.Scope != "github:repo:read" {
		t.Errorf("scope = %q", claims.Scope)
	}
	if claims.Audience != td {
		t.Errorf("aud = %q, want %q", claims.Audience, td)
	}
	if repo, _ := claims.ReqCtx["repo"].(string); repo != "smol-platform/app" {
		t.Errorf("rctx.repo = %q, want smol-platform/app", repo)
	}
}

// A wrong expected audience must be rejected by the verifier.
func TestFakeTTS_AudienceMismatchRejected(t *testing.T) {
	srv, _ := newServer("smol-platform/app", 5*time.Minute)
	srv.logger = newDiscardLogger()
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	minter := &trat.ExchangeMinter{
		TokenURL: ts.URL + "/token",
		SubjectToken: func(_ context.Context, _ string) (string, error) {
			return fakeSubjectToken("spiffe://smol-agents.ai/x"), nil
		},
	}
	compact, err := minter.Token(context.Background(), trat.ExchangeParams{Scope: "s", Audience: "spiffe://smol-agents.ai"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	v := &trat.JWKSVerifier{Keys: &trat.HTTPKeySource{URL: ts.URL + "/jwks"}, Audience: "spiffe://evil.example"}
	if _, err := v.Verify(context.Background(), compact); err == nil {
		t.Fatal("expected audience-mismatch rejection")
	}
}
