package proxy

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/identity"
)

// fakeIdentity implements identity.Source enough for the HTTP proxy
// (which only needs JWTSource indirectly through fetchJWT) AND for
// the TCP proxy (which dials with X509Source). For TCP we use the
// JWTFetcher hook so we don't need a real workload API.
type fakeIdentity struct {
	td spiffeid.TrustDomain
}

func (f *fakeIdentity) X509Source() *workloadapi.X509Source { return nil }
func (f *fakeIdentity) JWTSource() *workloadapi.JWTSource   { return nil }
func (f *fakeIdentity) TrustDomain() spiffeid.TrustDomain   { return f.td }
func (f *fakeIdentity) Mode() identity.Mode                 { return identity.ModeStrict }
func (f *fakeIdentity) Close() error                        { return nil }

// Compile-time check.
var _ identity.Source = &fakeIdentity{}

func TestHTTPProxy_AddsBearerJWT(t *testing.T) {
	var capturedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	port := freePort(t)
	p := &HTTPProxy{
		Resource: v1.ResourceTarget{
			Name: "billing", Kind: "http", LocalPort: int32(port),
			Gateway: upstream.URL, JWTAudience: "spiffe://stigen.ai/ns/infra/sa/billing",
		},
		Identity: &fakeIdentity{td: spiffeid.RequireTrustDomainFromString("stigen.ai")},
		JWTFetcher: func(_ context.Context, aud string) (string, error) {
			return "test-jwt-for-" + aud, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()
	addr := fmt.Sprintf("127.0.0.1:%d", p.Resource.LocalPort)
	waitListening(t, addr)

	resp, err := http.Get("http://" + addr + "/orders")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(capturedAuth, "Bearer test-jwt-for-spiffe://stigen.ai/ns/infra/sa/billing") {
		t.Errorf("upstream Auth = %q", capturedAuth)
	}
}

func TestHTTPProxy_UpstreamErrorBubbles(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("nope"))
	}))
	defer upstream.Close()

	m := &countingMetrics{}
	p := &HTTPProxy{
		Resource: v1.ResourceTarget{
			Name: "x", Kind: "http", LocalPort: int32(freePort(t)),
			Gateway: upstream.URL, JWTAudience: "spiffe://stigen.ai/ns/x/sa/x",
		},
		Identity:   &fakeIdentity{td: spiffeid.RequireTrustDomainFromString("stigen.ai")},
		Metrics:    m,
		JWTFetcher: func(_ context.Context, _ string) (string, error) { return "tok", nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()
	addr := fmt.Sprintf("127.0.0.1:%d", p.Resource.LocalPort)
	waitListening(t, addr)

	resp, _ := http.Get("http://" + addr + "/")
	resp.Body.Close()
	if m.errors.Load() == 0 {
		t.Error("expected error counter to bump on 5xx")
	}
}

func TestHTTPProxy_JWTFailureReturns503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("never"))
	}))
	defer upstream.Close()

	p := &HTTPProxy{
		Resource: v1.ResourceTarget{
			Name: "x", Kind: "http", LocalPort: int32(freePort(t)),
			Gateway: upstream.URL, JWTAudience: "spiffe://stigen.ai/ns/x/sa/x",
		},
		Identity:   &fakeIdentity{td: spiffeid.RequireTrustDomainFromString("stigen.ai")},
		JWTFetcher: func(_ context.Context, _ string) (string, error) { return "", errors.New("svid down") },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()
	addr := fmt.Sprintf("127.0.0.1:%d", p.Resource.LocalPort)
	waitListening(t, addr)

	resp, _ := http.Get("http://" + addr + "/")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSidecar_Run_RequiresResources(t *testing.T) {
	s := &Sidecar{Identity: &fakeIdentity{td: spiffeid.RequireTrustDomainFromString("stigen.ai")}}
	if err := s.Run(context.Background()); err == nil {
		t.Error("expected error for empty resources")
	}
}

func TestSidecar_Run_FailingResourceCancelsAll(t *testing.T) {
	s := &Sidecar{
		Identity: &fakeIdentity{td: spiffeid.RequireTrustDomainFromString("stigen.ai")},
		Spec: v1.IdentityProxySpec{Resources: []v1.ResourceTarget{
			{Name: "bad", Kind: "tcp"}, // no LocalAddr → fails immediately
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := s.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("expected bad-resource error, got %v", err)
	}
}

func TestTCPProxy_RejectsWrongKind(t *testing.T) {
	p := &TCPProxy{
		Resource: v1.ResourceTarget{Kind: "http", LocalAddr: "127.0.0.1:0"},
		Identity: &fakeIdentity{td: spiffeid.RequireTrustDomainFromString("stigen.ai")},
	}
	if err := p.Run(context.Background()); err == nil {
		t.Error("expected wrong-kind error")
	}
}

// --- helpers ---

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never listening: %s", addr)
}

type countingMetrics struct {
	ok, errors atomic.Int64
	mu         sync.Mutex
}

func (c *countingMetrics) DialOK(_ string)       { c.ok.Add(1) }
func (c *countingMetrics) DialError(_, _ string) { c.errors.Add(1) }

// keep imports used even if some paths above change later
var _ = x509svid.SVID{}
var _ = x509bundle.Bundle{}
var _ = x509.Certificate{}
