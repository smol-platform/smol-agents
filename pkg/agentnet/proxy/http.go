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

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/identity"
)

// HTTPProxy is a per-resource reverse proxy that injects a JWT-SVID
// into every upstream request. Implements R-AN-PROXY-2.
type HTTPProxy struct {
	Resource v1.ResourceTarget
	Identity identity.Source
	Metrics  ProxyMetrics

	// JWTFetcher is optional; default uses the identity source's
	// JWTSource. Tests inject fakes.
	JWTFetcher func(ctx context.Context, audience string) (string, error)

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
		token, err := p.fetchJWT(r.Context())
		if err != nil {
			// Stamp a header so the ErrorHandler-equivalent (we use
			// Transport below) can short-circuit.
			r.Header.Set("X-Agentnet-JWT-Error", err.Error())
			return
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Agentnet-Resource", p.Resource.Name)
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
	if errMsg := req.Header.Get("X-Agentnet-JWT-Error"); errMsg != "" {
		// Director couldn't mint a token. Surface 503 so the agent
		// retries with backoff.
		metricsOf(t.metrics).DialError(t.resource, "jwt:"+errMsg)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       http.NoBody,
			Header:     http.Header{"X-Agentnet-Reason": {"jwt-svid-unavailable"}},
			Request:    req,
		}, nil
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
