package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/trat"
)

type fakeTraTMinter struct {
	compact          string
	err              error
	gotScope, gotAud string
	calls            atomic.Int64
}

func (f *fakeTraTMinter) Token(_ context.Context, p trat.ExchangeParams) (string, error) {
	f.calls.Add(1)
	f.gotScope, f.gotAud = p.Scope, p.Audience
	return f.compact, f.err
}

type fakeBroker struct {
	value            []byte
	expiry           time.Time
	err              error
	gotName, gotTraT string
}

func (f *fakeBroker) Mint(_ context.Context, name, compactTraT string) ([]byte, time.Time, error) {
	f.gotName, f.gotTraT = name, compactTraT
	return f.value, f.expiry, f.err
}

func startProxy(t *testing.T, p *HTTPProxy) string {
	t.Helper()
	p.Identity = &fakeIdentity{td: spiffeid.RequireTrustDomainFromString("stigen.ai")}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = p.Run(ctx) }()
	addr := fmt.Sprintf("127.0.0.1:%d", p.Resource.LocalPort)
	waitListening(t, addr)
	return addr
}

// TraT resources forward an internal Txn-Token alongside the agent's
// JWT-SVID identity. R-SEGR-INJECT-1.
func TestHTTPProxy_InjectsTraT(t *testing.T) {
	var auth, txn string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, txn = r.Header.Get("Authorization"), r.Header.Get(trat.Header)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	m := &fakeTraTMinter{compact: "compact-trat"}
	p := &HTTPProxy{
		Resource: v1.ResourceTarget{
			Name: "internal", Kind: "http", LocalPort: int32(freePort(t)),
			Gateway: upstream.URL, JWTAudience: "spiffe://stigen.ai/ns/x/sa/x",
			TraT: &v1.TraTInjection{Scope: "github:repo:read"},
		},
		JWTFetcher:   func(_ context.Context, _ string) (string, error) { return "svid", nil },
		TraTMinter:   m,
		TraTAudience: "spiffe://stigen.ai",
	}
	addr := startProxy(t, p)

	resp, err := http.Get("http://" + addr + "/x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if txn != "compact-trat" {
		t.Errorf("Txn-Token = %q, want compact-trat", txn)
	}
	if auth != "Bearer svid" {
		t.Errorf("Authorization = %q, want Bearer svid (agent identity preserved)", auth)
	}
	if m.gotScope != "github:repo:read" || m.gotAud != "spiffe://stigen.ai" {
		t.Errorf("minter got scope=%q aud=%q", m.gotScope, m.gotAud)
	}
}

// Credential resources mint a provider secret via the broker and inject it;
// the agent's identity never clobbers it and the authorizing TraT is never
// forwarded upstream. R-SEGR-INJECT-1 / R-SEGR-SEC-1.
func TestHTTPProxy_InjectsCredential_AgentBlind(t *testing.T) {
	var auth, txn string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, txn = r.Header.Get("Authorization"), r.Header.Get(trat.Header)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	var jwtCalls atomic.Int64
	m := &fakeTraTMinter{compact: "authz-trat"}
	b := &fakeBroker{value: []byte("ghs_installation_token")}
	p := &HTTPProxy{
		Resource: v1.ResourceTarget{
			Name: "github", Kind: "http", LocalPort: int32(freePort(t)),
			Gateway: upstream.URL, JWTAudience: "spiffe://stigen.ai/ns/x/sa/x",
			Credential: &v1.CredentialInjection{Name: "github", Scope: "github:repo:read"},
		},
		JWTFetcher: func(_ context.Context, _ string) (string, error) {
			jwtCalls.Add(1)
			return "svid", nil
		},
		TraTMinter:   m,
		Broker:       b,
		TraTAudience: "spiffe://stigen.ai",
	}
	addr := startProxy(t, p)

	resp, err := http.Get("http://" + addr + "/repos/stigen/app")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if auth != "Bearer ghs_installation_token" {
		t.Errorf("Authorization = %q, want the minted credential", auth)
	}
	if txn != "" {
		t.Errorf("internal TraT leaked upstream: Txn-Token = %q", txn)
	}
	if b.gotName != "github" || b.gotTraT != "authz-trat" {
		t.Errorf("broker got name=%q trat=%q", b.gotName, b.gotTraT)
	}
	if m.gotScope != "github:repo:read" {
		t.Errorf("authorizing TraT scope = %q", m.gotScope)
	}
	if jwtCalls.Load() != 0 {
		t.Errorf("agent JWT-SVID was minted for a credential resource (should be skipped)")
	}
}

// A broker mint failure must fail closed: 503, upstream never called, so a
// missing credential can never degrade into an anonymous request.
// R-SEGR-SEC-1.
func TestHTTPProxy_CredentialMintFailureFailsClosed(t *testing.T) {
	var called atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		_, _ = w.Write([]byte("should-not-reach"))
	}))
	defer upstream.Close()

	p := &HTTPProxy{
		Resource: v1.ResourceTarget{
			Name: "github", Kind: "http", LocalPort: int32(freePort(t)),
			Gateway: upstream.URL, JWTAudience: "spiffe://stigen.ai/ns/x/sa/x",
			Credential: &v1.CredentialInjection{Name: "github", Scope: "s"},
		},
		JWTFetcher:   func(_ context.Context, _ string) (string, error) { return "svid", nil },
		TraTMinter:   &fakeTraTMinter{compact: "authz-trat"},
		Broker:       &fakeBroker{err: fmt.Errorf("broker denied")},
		TraTAudience: "spiffe://stigen.ai",
	}
	addr := startProxy(t, p)

	resp, _ := http.Get("http://" + addr + "/x")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()
	if called.Load() {
		t.Error("upstream was called despite mint failure (must fail closed)")
	}
}

// A TraT mint failure on a credential resource also fails closed.
func TestHTTPProxy_TraTMintFailureFailsClosed(t *testing.T) {
	var called atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
	}))
	defer upstream.Close()

	b := &fakeBroker{value: []byte("never")}
	p := &HTTPProxy{
		Resource: v1.ResourceTarget{
			Name: "github", Kind: "http", LocalPort: int32(freePort(t)),
			Gateway: upstream.URL, JWTAudience: "spiffe://stigen.ai/ns/x/sa/x",
			Credential: &v1.CredentialInjection{Name: "github", Scope: "s"},
		},
		JWTFetcher:   func(_ context.Context, _ string) (string, error) { return "svid", nil },
		TraTMinter:   &fakeTraTMinter{err: fmt.Errorf("tts down")},
		Broker:       b,
		TraTAudience: "spiffe://stigen.ai",
	}
	addr := startProxy(t, p)

	resp, _ := http.Get("http://" + addr + "/x")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()
	if called.Load() {
		t.Error("upstream was called despite TraT failure (must fail closed)")
	}
	if b.gotTraT != "" {
		t.Error("broker was called despite TraT failure")
	}
}
