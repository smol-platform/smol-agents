package trat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	josejwt "github.com/go-jose/go-jose/v4/jwt"
)

func TestExchangeMinter_Token(t *testing.T) {
	priv := newKey(t)
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	traT := signTraT(t, priv, josejwt.Claims{
		Subject:  "spiffe://stigen.ai/ns/tenant-a/sa/alice-agent",
		Audience: josejwt.Audience{"stigen.ai"},
		Expiry:   josejwt.NewNumericDate(now.Add(5 * time.Minute)),
	}, map[string]any{"scope": "github:repo:read"})

	var hits int
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token_type":        "N_A",
			"issued_token_type": TokenTypeTxn,
			"access_token":      traT,
		})
	}))
	defer srv.Close()

	m := &ExchangeMinter{
		TokenURL:        srv.URL,
		SubjectAudience: "tts.stigen.ai",
		SubjectToken:    func(_ context.Context, aud string) (string, error) { return "jwt-svid-for:" + aud, nil },
		Now:             func() time.Time { return now },
	}
	p := ExchangeParams{Scope: "github:repo:read", Audience: "stigen.ai"}

	got, err := m.Token(context.Background(), p)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != traT {
		t.Errorf("token mismatch")
	}
	// Form correctness (RFC 8693).
	if gotForm.Get("grant_type") != GrantTokenExchange ||
		gotForm.Get("requested_token_type") != TokenTypeTxn ||
		gotForm.Get("subject_token") != "jwt-svid-for:tts.stigen.ai" ||
		gotForm.Get("subject_token_type") != TokenTypeJWT ||
		gotForm.Get("audience") != "stigen.ai" ||
		gotForm.Get("scope") != "github:repo:read" {
		t.Errorf("form = %v", gotForm)
	}

	// Second call within exp-skew is served from cache (no extra TTS hit).
	if _, err := m.Token(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("expected 1 TTS hit (cached), got %d", hits)
	}
}

func TestExchangeMinter_Token_TTSError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	m := &ExchangeMinter{
		TokenURL:     srv.URL,
		SubjectToken: func(context.Context, string) (string, error) { return "svid", nil },
	}
	if _, err := m.Token(context.Background(), ExchangeParams{Scope: "s", Audience: "stigen.ai"}); err == nil {
		t.Fatal("expected error on TTS 403")
	}
}
