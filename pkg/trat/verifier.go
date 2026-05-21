package trat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
)

// KeySource provides the JWKS used to verify TraTs (the TTS signing keys).
type KeySource interface {
	Keys(ctx context.Context) (*jose.JSONWebKeySet, error)
}

// Verifier verifies a compact TraT and returns its claims.
type Verifier interface {
	Verify(ctx context.Context, compact string) (*Claims, error)
}

// JWKSVerifier verifies a TraT's signature against the TTS JWKS, then checks
// aud + exp, returning the parsed claims. Signature verification happens first
// so unsigned/forged tokens never reach the claim logic.
type JWKSVerifier struct {
	Keys     KeySource
	Audience string                    // expected trust-domain audience (aud); "" = skip aud check
	Algs     []jose.SignatureAlgorithm // default RS256, ES256
	Now      func() time.Time
}

func (v *JWKSVerifier) Verify(ctx context.Context, compact string) (*Claims, error) {
	if v.Keys == nil {
		return nil, fmt.Errorf("%w: no key source", ErrVerify)
	}
	algs := v.Algs
	if len(algs) == 0 {
		algs = []jose.SignatureAlgorithm{jose.RS256, jose.ES256}
	}
	tok, err := josejwt.ParseSigned(compact, algs)
	if err != nil {
		return nil, fmt.Errorf("%w: parse: %v", ErrVerify, err)
	}
	jwks, err := v.Keys.Keys(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: jwks: %v", ErrVerify, err)
	}

	var reg josejwt.Claims
	var all map[string]any
	if err := tok.Claims(jwks, &reg, &all); err != nil { // verifies signature
		return nil, fmt.Errorf("%w: signature: %v", ErrVerify, err)
	}

	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	if reg.Expiry == nil || !now.Before(reg.Expiry.Time()) {
		return nil, fmt.Errorf("%w: expired", ErrVerify)
	}
	if v.Audience != "" {
		ok := false
		for _, a := range reg.Audience {
			if a == v.Audience {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("%w: audience %v != %q", ErrVerify, reg.Audience, v.Audience)
		}
	}

	c := &Claims{Subject: reg.Subject, Compact: compact}
	if len(reg.Audience) > 0 {
		c.Audience = reg.Audience[0]
	}
	if reg.Expiry != nil {
		c.Expiry = reg.Expiry.Time()
	}
	if reg.IssuedAt != nil {
		c.IssuedAt = reg.IssuedAt.Time()
	}
	if s, ok := all["scope"].(string); ok {
		c.Scope = s
	}
	if s, ok := all["txn"].(string); ok {
		c.TxnID = s
	}
	if s, ok := all["req_wl"].(string); ok {
		c.ReqWL = s
	}
	if m, ok := all["rctx"].(map[string]any); ok {
		c.ReqCtx = m
	}
	if m, ok := all["tctx"].(map[string]any); ok {
		c.TxnCtx = m
	}
	return c, nil
}

// StaticKeySource serves a fixed JWKS (tests, or a pre-loaded bundle).
type StaticKeySource struct{ Set *jose.JSONWebKeySet }

func (s StaticKeySource) Keys(context.Context) (*jose.JSONWebKeySet, error) { return s.Set, nil }

// HTTPKeySource fetches + caches a JWKS from the TTS jwks_uri.
type HTTPKeySource struct {
	URL  string
	HTTP *http.Client
	TTL  time.Duration // default 5m
	Now  func() time.Time

	mu     sync.Mutex
	cached *jose.JSONWebKeySet
	exp    time.Time
}

func (h *HTTPKeySource) Keys(ctx context.Context) (*jose.JSONWebKeySet, error) {
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	h.mu.Lock()
	if h.cached != nil && now().Before(h.exp) {
		ks := h.cached
		h.mu.Unlock()
		return ks, nil
	}
	h.mu.Unlock()

	hc := h.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trat: jwks status %d", resp.StatusCode)
	}
	var ks jose.JSONWebKeySet
	if err := json.NewDecoder(resp.Body).Decode(&ks); err != nil {
		return nil, err
	}
	ttl := h.TTL
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	h.mu.Lock()
	h.cached = &ks
	h.exp = now().Add(ttl)
	h.mu.Unlock()
	return &ks, nil
}
