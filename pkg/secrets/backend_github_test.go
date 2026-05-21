package secrets

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
)

func TestGitHubAppBackend_Mint(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	exp := now.Add(time.Hour)

	var sawIss string
	var gotBody struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/stigen/app/installation", func(w http.ResponseWriter, r *http.Request) {
		sawIss = bearerIssuer(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42})
	})
	mux.HandleFunc("/app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("access_tokens method = %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_installation_token",
			"expires_at": exp.Format(time.RFC3339),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := &GitHubAppBackend{
		AppID:            "123",
		PrivateKey:       key,
		BaseURL:          srv.URL,
		Now:              func() time.Time { return now },
		ScopePermissions: map[string]map[string]string{"github:repo:read": {"contents": "read"}},
	}
	lease, err := b.Mint(context.Background(), CredentialRequest{
		Name:   "github",
		Scope:  "github:repo:read",
		ReqCtx: map[string]any{"repo": "stigen/app"},
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if string(lease.Value) != "ghs_installation_token" {
		t.Errorf("token = %q", lease.Value)
	}
	if !lease.ExpiresAt.Equal(exp) {
		t.Errorf("expires = %v, want %v", lease.ExpiresAt, exp)
	}
	if sawIss != "123" {
		t.Errorf("app JWT iss = %q, want 123", sawIss)
	}
	if len(gotBody.Repositories) != 1 || gotBody.Repositories[0] != "app" {
		t.Errorf("scoped repositories = %v, want [app]", gotBody.Repositories)
	}
	if gotBody.Permissions["contents"] != "read" {
		t.Errorf("scoped permissions = %v, want contents:read", gotBody.Permissions)
	}
}

func TestGitHubAppBackend_Mint_RequiresRepo(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	b := &GitHubAppBackend{AppID: "1", PrivateKey: key}
	if _, err := b.Mint(context.Background(), CredentialRequest{Name: "github", Scope: "s"}); err == nil {
		t.Fatal("expected error when rctx.repo is absent")
	}
}

// bearerIssuer decodes (without verifying) the iss of the App JWT in the
// Authorization header.
func bearerIssuer(t *testing.T, r *http.Request) string {
	t.Helper()
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok, err := josejwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatalf("parse app jwt: %v", err)
	}
	var cl josejwt.Claims
	if err := tok.UnsafeClaimsWithoutVerification(&cl); err != nil {
		t.Fatalf("claims: %v", err)
	}
	return cl.Issuer
}
