package trat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
)

// SubjectTokenFunc returns a subject_token (a JWT-SVID) minted for the given
// audience — typically wraps identity.Source.JWTSource().FetchJWTSVID.
type SubjectTokenFunc func(ctx context.Context, audience string) (string, error)

// Minter mints TraTs for a given scope+audience.
type Minter interface {
	Token(ctx context.Context, p ExchangeParams) (string, error)
}

// ExchangeMinter mints TraTs via the TTS token-exchange endpoint (RFC 8693)
// and caches them by (scope, audience) until shortly before exp. The minter
// always acts as its own pod identity, so the cache key needs no subject.
type ExchangeMinter struct {
	TokenURL         string           // TTS token-exchange endpoint
	SubjectToken     SubjectTokenFunc // source of the JWT-SVID subject_token
	SubjectTokenType string           // default TokenTypeJWT
	SubjectAudience  string           // audience the JWT-SVID is minted for; default = ExchangeParams.Audience
	HTTP             *http.Client     // default http.DefaultClient (pass an mTLS client for SPIFFE)
	Skew             time.Duration    // refresh margin before exp; default 30s
	Now              func() time.Time

	mu    sync.Mutex
	cache map[string]cachedTraT
}

type cachedTraT struct {
	compact string
	exp     time.Time
}

func (m *ExchangeMinter) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Token returns a valid TraT, minting (and caching) one if needed.
func (m *ExchangeMinter) Token(ctx context.Context, p ExchangeParams) (string, error) {
	if m.SubjectToken == nil {
		return "", fmt.Errorf("%w: SubjectToken func is required", ErrExchange)
	}
	skew := m.Skew
	if skew == 0 {
		skew = 30 * time.Second
	}
	key := p.Scope + "\x00" + p.Audience

	m.mu.Lock()
	if c, ok := m.cache[key]; ok && m.now().Add(skew).Before(c.exp) {
		m.mu.Unlock()
		return c.compact, nil
	}
	m.mu.Unlock()

	subType := m.SubjectTokenType
	if subType == "" {
		subType = TokenTypeJWT
	}
	subAud := m.SubjectAudience
	if subAud == "" {
		subAud = p.Audience
	}
	subTok, err := m.SubjectToken(ctx, subAud)
	if err != nil {
		return "", fmt.Errorf("%w: subject token: %v", ErrExchange, err)
	}

	form := url.Values{
		"grant_type":           {GrantTokenExchange},
		"requested_token_type": {TokenTypeTxn},
		"subject_token":        {subTok},
		"subject_token_type":   {subType},
		"audience":             {p.Audience},
		"scope":                {p.Scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrExchange, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	hc := m.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrExchange, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: TTS status %d", ErrExchange, resp.StatusCode)
	}
	var out struct {
		AccessToken     string `json:"access_token"`
		IssuedTokenType string `json:"issued_token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("%w: decode: %v", ErrExchange, err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("%w: empty access_token", ErrExchange)
	}

	m.mu.Lock()
	if m.cache == nil {
		m.cache = map[string]cachedTraT{}
	}
	m.cache[key] = cachedTraT{compact: out.AccessToken, exp: m.tokenExpiry(out.AccessToken)}
	m.mu.Unlock()
	return out.AccessToken, nil
}

// tokenExpiry reads exp WITHOUT verifying — used only for the local cache TTL.
func (m *ExchangeMinter) tokenExpiry(compact string) time.Time {
	tok, err := josejwt.ParseSigned(compact, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
	if err != nil {
		return m.now().Add(time.Minute)
	}
	var cl josejwt.Claims
	if err := tok.UnsafeClaimsWithoutVerification(&cl); err != nil || cl.Expiry == nil {
		return m.now().Add(time.Minute)
	}
	return cl.Expiry.Time()
}
