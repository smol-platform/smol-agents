package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// JWKSVerifier is a minimal, dependency-free OIDC ID-token verifier (RS256) for
// the bundled Dex/Keycloak issuer (M4.5). It verifies the signature against the
// issuer's JWKS (cached), checks iss/aud/exp, and returns the human subject
// (email when present, else sub). It is the production OIDCVerifier; the gateway
// tests inject a fake. Only RS256 is accepted (the bundled IdP's default).
type JWKSVerifier struct {
	Issuer   string // expected iss
	Audience string // expected aud (the agentterminal OIDC client id)
	JWKSURL  string // issuer JWKS endpoint
	Client   *http.Client
	Now      func() time.Time

	mu   sync.Mutex
	keys map[string]*rsa.PublicKey // kid → key (cached)
}

func (v *JWKSVerifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *JWKSVerifier) client() *http.Client {
	if v.Client != nil {
		return v.Client
	}
	return http.DefaultClient
}

type oidcClaims struct {
	Iss   string          `json:"iss"`
	Aud   json.RawMessage `json:"aud"` // string OR []string
	Exp   int64           `json:"exp"`
	Sub   string          `json:"sub"`
	Email string          `json:"email"`
}

// Verify checks the RS256 signature + iss/aud/exp and returns the subject.
func (v *JWKSVerifier) Verify(ctx context.Context, raw string) (string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", errors.New("oidc: malformed id token")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := jsonB64(parts[0], &hdr); err != nil {
		return "", fmt.Errorf("oidc: header: %w", err)
	}
	if hdr.Alg != "RS256" {
		return "", fmt.Errorf("oidc: unsupported alg %q (only RS256)", hdr.Alg)
	}
	key, err := v.keyFor(ctx, hdr.Kid)
	if err != nil {
		return "", err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("oidc: bad signature encoding")
	}
	signed := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, signed[:], sig); err != nil {
		return "", errors.New("oidc: signature verification failed")
	}
	var c oidcClaims
	if err := jsonB64(parts[1], &c); err != nil {
		return "", fmt.Errorf("oidc: claims: %w", err)
	}
	if v.Issuer != "" && c.Iss != v.Issuer {
		return "", fmt.Errorf("oidc: issuer %q != expected %q", c.Iss, v.Issuer)
	}
	if !audienceContains(c.Aud, v.Audience) {
		return "", errors.New("oidc: audience mismatch")
	}
	if c.Exp != 0 && v.now().Unix() >= c.Exp {
		return "", errors.New("oidc: token expired")
	}
	if c.Email != "" {
		return c.Email, nil
	}
	if c.Sub == "" {
		return "", errors.New("oidc: no subject")
	}
	return c.Sub, nil
}

// keyFor returns the cached key for kid, fetching+caching the JWKS on a miss.
func (v *JWKSVerifier) keyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	k := v.keys[kid]
	v.mu.Unlock()
	if k != nil {
		return k, nil
	}
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	v.mu.Lock()
	k = v.keys[kid]
	v.mu.Unlock()
	if k == nil {
		return nil, fmt.Errorf("oidc: no JWKS key for kid %q", kid)
	}
	return k, nil
}

func (v *JWKSVerifier) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.JWKSURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client().Do(req)
	if err != nil {
		return fmt.Errorf("oidc: fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: JWKS http %d", resp.StatusCode)
	}
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("oidc: decode JWKS: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, jk := range jwks.Keys {
		if jk.Kty != "RSA" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(jk.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(jk.E)
		if err != nil {
			continue
		}
		keys[jk.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	v.mu.Lock()
	v.keys = keys
	v.mu.Unlock()
	return nil
}

func jsonB64(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// audienceContains handles aud as either a JSON string or array of strings.
func audienceContains(raw json.RawMessage, want string) bool {
	if want == "" {
		return true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == want
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		for _, a := range arr {
			if a == want {
				return true
			}
		}
	}
	return false
}
