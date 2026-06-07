package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/smol-platform/smol-agents/pkg/attachtoken"
)

const (
	td  = "smol-agents.ai"
	key = "test-signing-key"
)

type fakeOIDC struct {
	subject string
	err     error
}

func (f fakeOIDC) Verify(context.Context, string) (string, error) { return f.subject, f.err }

type fakeGrants struct {
	role, name string
	ok         bool
}

func (f fakeGrants) Resolve(context.Context, string, string, string, time.Time) (string, string, bool) {
	return f.role, f.name, f.ok
}

// fakeTarget routes driver/viewer to distinct backends so a test can prove the
// gateway sends a viewer to the read-only ttyd and a driver to the writable one.
type fakeTarget struct{ driver, viewer *url.URL }

func (f fakeTarget) TTYD(_, _, role string) (*url.URL, error) {
	if role == "driver" {
		return f.driver, nil
	}
	return f.viewer, nil
}

func newGateway(t *testing.T, tgt TargetResolver) *Gateway {
	t.Helper()
	return &Gateway{
		SigningKey:  []byte(key),
		TrustDomain: td,
		OIDC:        fakeOIDC{subject: "alice@example.com"},
		Grants:      fakeGrants{role: "driver", name: "g1", ok: true},
		Target:      tgt,
		TokenTTL:    time.Minute,
		AllowOrigin: map[string]bool{"console.example.com": true},
		Now:         func() time.Time { return time.Unix(1000, 0) },
	}
}

func mint(t *testing.T, role, ns, agent string, exp int64) string {
	t.Helper()
	tok, err := attachtoken.Mint([]byte(key), attachtoken.Claims{
		Subject: "alice@example.com", Role: attachtoken.Role(role),
		Audience: attachtoken.Audience(td, ns, agent), AgentRef: ns + "/" + agent, Expiry: exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func attachReq(tok, origin string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/terminal/tenant-a/claude", nil)
	r.SetPathValue("ns", "tenant-a")
	r.SetPathValue("agent", "claude")
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

func TestAttach_RejectsBadTokens(t *testing.T) {
	g := newGateway(t, fakeTarget{})
	cases := map[string]string{
		"missing":   "",
		"wrong-aud": mint(t, "driver", "tenant-a", "codex", 2000), // another agent
		"cross-ns":  mint(t, "driver", "tenant-b", "claude", 2000),
		"expired":   mint(t, "driver", "tenant-a", "claude", 999), // <= now(1000)
	}
	for name, tok := range cases {
		w := httptest.NewRecorder()
		g.handleAttach(w, attachReq(tok, ""))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: code = %d, want 401", name, w.Code)
		}
	}
}

func TestAttach_RejectsBadOrigin(t *testing.T) {
	g := newGateway(t, fakeTarget{})
	w := httptest.NewRecorder()
	g.handleAttach(w, attachReq(mint(t, "driver", "tenant-a", "claude", 2000), "https://evil.example.com"))
	if w.Code != http.StatusForbidden {
		t.Errorf("bad origin: code = %d, want 403", w.Code)
	}
}

func TestAttach_RoleSelectsTtydPort(t *testing.T) {
	var got string
	backend := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = label + "|" + r.Header.Get("X-Smol-Attach")
			w.WriteHeader(http.StatusOK)
		}))
	}
	driver := backend("driver")
	viewer := backend("viewer")
	defer driver.Close()
	defer viewer.Close()
	du, _ := url.Parse(driver.URL)
	vu, _ := url.Parse(viewer.URL)
	g := newGateway(t, fakeTarget{driver: du, viewer: vu})

	// A viewer token must reach ONLY the read-only ttyd, never the writable one.
	got = ""
	g.handleAttach(httptest.NewRecorder(), attachReq(mint(t, "viewer", "tenant-a", "claude", 2000), "https://console.example.com"))
	if !strings.HasPrefix(got, "viewer|") {
		t.Fatalf("viewer token reached %q, want the viewer backend", got)
	}
	if !strings.HasSuffix(got, "|alice@example.com") {
		t.Errorf("auth header not injected as subject: %q", got)
	}

	// A driver token reaches the writable ttyd.
	got = ""
	g.handleAttach(httptest.NewRecorder(), attachReq(mint(t, "driver", "tenant-a", "claude", 2000), ""))
	if !strings.HasPrefix(got, "driver|") {
		t.Fatalf("driver token reached %q, want the driver backend", got)
	}
}

// A client-supplied X-Smol-Attach must be stripped (not forwarded) — only the
// gateway sets it, from the verified subject.
func TestAttach_StripsForgedAuthHeader(t *testing.T) {
	var got string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Smol-Attach")
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)
	g := newGateway(t, fakeTarget{driver: bu, viewer: bu})
	r := attachReq(mint(t, "driver", "tenant-a", "claude", 2000), "")
	r.Header.Set("X-Smol-Attach", "root") // forged
	g.handleAttach(httptest.NewRecorder(), r)
	if got != "alice@example.com" {
		t.Errorf("auth header = %q, want the verified subject (forged value must be stripped)", got)
	}
}

func mintReq() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/terminal/tenant-a/claude/grants", nil)
	r.SetPathValue("ns", "tenant-a")
	r.SetPathValue("agent", "claude")
	r.Header.Set("Authorization", "Bearer fake-oidc")
	return r
}

func TestMint_GrantToToken(t *testing.T) {
	g := newGateway(t, fakeTarget{})
	w := httptest.NewRecorder()
	g.handleMint(w, mintReq())
	if w.Code != http.StatusOK {
		t.Fatalf("mint code = %d, want 200", w.Code)
	}
	var resp struct {
		Token, Role string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// The minted token verifies against THIS agent's audience.
	claims, err := attachtoken.Verify([]byte(key), resp.Token, attachtoken.Audience(td, "tenant-a", "claude"), 1001)
	if err != nil {
		t.Fatalf("minted token must verify: %v", err)
	}
	if claims.Subject != "alice@example.com" || claims.Role != "driver" {
		t.Errorf("claims = %+v", claims)
	}
}

func TestMint_DeniesWithoutGrant(t *testing.T) {
	g := newGateway(t, fakeTarget{})
	g.Grants = fakeGrants{ok: false}
	w := httptest.NewRecorder()
	g.handleMint(w, mintReq())
	if w.Code != http.StatusForbidden {
		t.Errorf("no grant: code = %d, want 403", w.Code)
	}
}

func TestMint_DeniesUnauthenticated(t *testing.T) {
	g := newGateway(t, fakeTarget{})
	g.OIDC = fakeOIDC{err: context.DeadlineExceeded}
	w := httptest.NewRecorder()
	g.handleMint(w, mintReq())
	if w.Code != http.StatusUnauthorized {
		t.Errorf("oidc fail: code = %d, want 401", w.Code)
	}
}
