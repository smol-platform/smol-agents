package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/identity"
	"github.com/smol-platform/smol-agents/pkg/trat"
)

// CredentialMinter mints a provider credential authorized by a TraT (the
// broker verifies the TraT). The agentnet sidecar wires this to the secret
// broker (secrets.Client.Mint). The agent never sees the returned value.
type CredentialMinter interface {
	Mint(ctx context.Context, name, compactTraT string) (value []byte, expiry time.Time, err error)
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// HTTPProxy is a per-resource reverse proxy that injects a JWT-SVID
// into every upstream request. Implements R-AN-PROXY-2.
type HTTPProxy struct {
	Resource v1.ResourceTarget
	Identity identity.Source
	Metrics  ProxyMetrics

	// JWTFetcher is optional; default uses the identity source's
	// JWTSource. Tests inject fakes.
	JWTFetcher func(ctx context.Context, audience string) (string, error)

	// TraTMinter mints Txn-Tokens for resources with a TraT/Credential
	// injection (optional; required only for those resources). R-SEGR-MINT-1.
	TraTMinter trat.Minter
	// Broker mints provider credentials for Credential resources, authorized
	// by an internal TraT (optional; secretless egress). R-SEGR-INJECT-1.
	Broker CredentialMinter
	// TraTAudience is the trust-domain audience used for minted TraTs when a
	// resource does not pin its own. R-SEGR-AUTH-1.
	TraTAudience string

	mu  sync.Mutex
	srv *http.Server
}

// Run blocks until ctx is cancelled.
func (p *HTTPProxy) Run(ctx context.Context) error {
	if p.Identity == nil {
		return errors.New("agentnet/proxy: HTTPProxy.Identity required")
	}
	if p.Resource.Kind != "http" {
		return fmt.Errorf("agentnet/proxy: HTTPProxy expects kind=http, got %q", p.Resource.Kind)
	}
	if p.Resource.LocalPort <= 0 {
		return errors.New("agentnet/proxy: resource.localPort required")
	}
	target, err := url.Parse(p.Resource.Gateway)
	if err != nil {
		return fmt.Errorf("agentnet/proxy: gateway URL: %w", err)
	}
	if p.Resource.JWTAudience == "" {
		return errors.New("agentnet/proxy: resource.jwtAudience required (R-AN-PROXY-2)")
	}
	aud, err := spiffeid.FromString(p.Resource.JWTAudience)
	if err != nil {
		return fmt.Errorf("agentnet/proxy: audience: %w", err)
	}
	_ = aud // we keep the parsed form for validation only

	rp := httputil.NewSingleHostReverseProxy(target)
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		metricsOf(p.Metrics).DialError(p.Resource.Name, "upstream:"+classify(err))
		http.Error(w, "upstream unavailable: "+err.Error(), http.StatusBadGateway)
	}
	origDirector := rp.Director
	rp.Director = func(r *http.Request) {
		origDirector(r)
		r.Header.Set("X-Agentnet-Resource", p.Resource.Name)

		// A credential injection that targets Authorization owns that header
		// (the agent's JWT-SVID identity must not clobber the minted secret).
		cred := p.Resource.Credential
		credOwnsAuth := cred != nil && orStr(cred.Header, "Authorization") == "Authorization"
		if !credOwnsAuth {
			token, err := p.fetchJWT(r.Context())
			if err != nil {
				// Stamp a header so the Transport below can short-circuit.
				r.Header.Set("X-Agentnet-JWT-Error", err.Error())
				return
			}
			r.Header.Set("Authorization", "Bearer "+token)
		}

		// Txn-Token injection: forward an internal TraT to an internal
		// upstream that consumes it. R-SEGR-INJECT-1.
		if t := p.Resource.TraT; t != nil {
			compact, err := p.mintTraT(r.Context(), t.Scope, orStr(t.Audience, p.TraTAudience))
			if err != nil {
				r.Header.Set("X-Agentnet-TraT-Error", err.Error())
				return
			}
			r.Header.Set(orStr(t.Header, trat.Header), compact)
		}

		// Secretless credential injection: mint an authorizing TraT (internal
		// only, never sent upstream), ask the broker to mint the provider
		// credential, then inject it. The agent never sees the value.
		// R-SEGR-INJECT-1 / R-SEGR-SEC-1.
		if cred != nil {
			if p.Broker == nil {
				r.Header.Set("X-Agentnet-Cred-Error", "no broker configured")
				return
			}
			authz, err := p.mintTraT(r.Context(), cred.Scope, p.TraTAudience)
			if err != nil {
				r.Header.Set("X-Agentnet-Cred-Error", "trat: "+err.Error())
				return
			}
			value, _, err := p.Broker.Mint(r.Context(), cred.Name, authz)
			if err != nil {
				r.Header.Set("X-Agentnet-Cred-Error", "mint: "+err.Error())
				return
			}
			r.Header.Set(orStr(cred.Header, "Authorization"), orStr(cred.Scheme, "Bearer")+" "+string(value))
		}
	}
	rp.Transport = &jwtTransport{base: http.DefaultTransport, metrics: p.Metrics, resource: p.Resource.Name}

	mux := http.NewServeMux()
	mux.Handle("/", rp)

	srv := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", p.Resource.LocalPort),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	p.mu.Lock()
	p.srv = srv
	p.mu.Unlock()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// mintTraT mints a Txn-Token for scope/audience via the configured minter.
func (p *HTTPProxy) mintTraT(ctx context.Context, scope, audience string) (string, error) {
	if p.TraTMinter == nil {
		return "", errors.New("agentnet/proxy: no TraT minter configured")
	}
	return p.TraTMinter.Token(ctx, trat.ExchangeParams{Scope: scope, Audience: audience})
}

// fetchJWT mints a JWT-SVID for the resource's audience.
func (p *HTTPProxy) fetchJWT(ctx context.Context) (string, error) {
	if p.JWTFetcher != nil {
		return p.JWTFetcher(ctx, p.Resource.JWTAudience)
	}
	src := p.Identity.JWTSource()
	if src == nil {
		return "", errors.New("agentnet/proxy: no JWTSource on identity")
	}
	svid, err := src.FetchJWTSVID(ctx, jwtsvid.Params{Audience: p.Resource.JWTAudience})
	if err != nil {
		return "", err
	}
	return svid.Marshal(), nil
}

// LocalAddr is a test seam.
func (p *HTTPProxy) LocalAddr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.srv != nil {
		return p.srv.Addr
	}
	return ""
}

// jwtTransport is a thin RoundTripper that converts director-side JWT
// fetch failures into 502s and tags metrics on every response.
type jwtTransport struct {
	base     http.RoundTripper
	metrics  ProxyMetrics
	resource string
}

func (t *jwtTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// The Director stamps an error header when it cannot mint a token or
	// credential. Fail closed: surface 503 and never forward the request
	// upstream (a missing credential must not become an anonymous call).
	// R-SEGR-SEC-1.
	for _, h := range []struct{ header, reason string }{
		{"X-Agentnet-JWT-Error", "jwt-svid-unavailable"},
		{"X-Agentnet-TraT-Error", "trat-unavailable"},
		{"X-Agentnet-Cred-Error", "credential-unavailable"},
	} {
		if msg := req.Header.Get(h.header); msg != "" {
			metricsOf(t.metrics).DialError(t.resource, h.reason+":"+msg)
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       http.NoBody,
				Header:     http.Header{"X-Agentnet-Reason": {h.reason}},
				Request:    req,
			}, nil
		}
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		metricsOf(t.metrics).DialError(t.resource, "transport:"+classify(err))
		return nil, err
	}
	if resp.StatusCode >= 400 {
		metricsOf(t.metrics).DialError(t.resource, fmt.Sprintf("upstream:%d", resp.StatusCode))
	} else {
		metricsOf(t.metrics).DialOK(t.resource)
	}
	return resp, nil
}

// ensure JWTSource type is referenced even if unused by some builds.
var _ = workloadapi.JWTSource{}

// stripPort is a small helper used in tests.
func stripPort(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return strings.TrimSuffix(addr, ":")
}
