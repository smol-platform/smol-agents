package secrets

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
)

// GitHubAppBackend mints short-lived GitHub App *installation access tokens*,
// scoped to a single repository + permissions derived from the TraT scope.
// The App private key is held only here (sourced from a mounted secret / the
// broker) and is never exposed to the agent. Implements R-SEGR-MINT-2.
type GitHubAppBackend struct {
	AppID      string          // GitHub App ID (numeric string)
	PrivateKey *rsa.PrivateKey // App private key (PEM-loaded)
	BaseURL    string          // default https://api.github.com
	HTTP       *http.Client    // default http.DefaultClient
	Now        func() time.Time

	// ScopePermissions maps a TraT scope to the installation-token
	// permissions to request (e.g. "github:repo:read" → {"contents":"read"}).
	// A scope absent here yields a token with the installation's defaults.
	ScopePermissions map[string]map[string]string
}

func (b *GitHubAppBackend) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *GitHubAppBackend) baseURL() string {
	if b.BaseURL != "" {
		return strings.TrimRight(b.BaseURL, "/")
	}
	return "https://api.github.com"
}

func (b *GitHubAppBackend) http() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return http.DefaultClient
}

// Mint resolves the installation for rctx.repo and returns a repo-scoped
// installation access token as the lease value.
func (b *GitHubAppBackend) Mint(ctx context.Context, req CredentialRequest) (Lease, error) {
	repo, _ := req.ReqCtx["repo"].(string)
	if repo == "" {
		return Lease{}, fmt.Errorf("%w: rctx.repo is required for github", ErrInvalidRequest)
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return Lease{}, fmt.Errorf("%w: rctx.repo %q must be owner/name", ErrInvalidRequest, repo)
	}

	appJWT, err := b.appJWT()
	if err != nil {
		return Lease{}, fmt.Errorf("%w: app jwt: %v", ErrBackendDown, err)
	}
	instID, err := b.installationID(ctx, appJWT, owner, name)
	if err != nil {
		return Lease{}, fmt.Errorf("%w: installation: %v", ErrBackendDown, err)
	}
	token, exp, err := b.accessToken(ctx, appJWT, instID, name, b.ScopePermissions[req.Scope])
	if err != nil {
		return Lease{}, fmt.Errorf("%w: access token: %v", ErrBackendDown, err)
	}
	return Lease{Value: []byte(token), ExpiresAt: exp}, nil
}

func (b *GitHubAppBackend) Close() error { return nil }

// appJWT mints the short-lived App-authentication JWT (RS256, iss=AppID).
func (b *GitHubAppBackend) appJWT() (string, error) {
	if b.PrivateKey == nil || b.AppID == "" {
		return "", fmt.Errorf("AppID + PrivateKey are required")
	}
	now := b.now()
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: b.PrivateKey},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	cl := josejwt.Claims{
		Issuer:   b.AppID,
		IssuedAt: josejwt.NewNumericDate(now.Add(-time.Minute)), // clock-skew slack
		Expiry:   josejwt.NewNumericDate(now.Add(10 * time.Minute)),
	}
	return josejwt.Signed(sig).Claims(cl).Serialize()
}

func (b *GitHubAppBackend) installationID(ctx context.Context, appJWT, owner, name string) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/installation", b.baseURL(), owner, name)
	var out struct {
		ID int64 `json:"id"`
	}
	if err := b.do(ctx, http.MethodGet, url, appJWT, nil, &out); err != nil {
		return 0, err
	}
	if out.ID == 0 {
		return 0, fmt.Errorf("no installation for %s/%s", owner, name)
	}
	return out.ID, nil
}

func (b *GitHubAppBackend) accessToken(ctx context.Context, appJWT string, instID int64, repoName string, perms map[string]string) (string, time.Time, error) {
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", b.baseURL(), instID)
	body := map[string]any{"repositories": []string{repoName}}
	if len(perms) > 0 {
		body["permissions"] = perms
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := b.do(ctx, http.MethodPost, url, appJWT, body, &out); err != nil {
		return "", time.Time{}, err
	}
	if out.Token == "" {
		return "", time.Time{}, fmt.Errorf("empty token in response")
	}
	return out.Token, out.ExpiresAt, nil
}

func (b *GitHubAppBackend) do(ctx context.Context, method, url, bearer string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("github %s %s: status %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
