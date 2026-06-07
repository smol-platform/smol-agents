package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/smol-platform/smol-agents/pkg/attachtoken"
)

// Gateway is the terminal attach front door (M4.10): a hardened, out-of-pod
// reverse proxy that authenticates a human (OIDC), authorizes them against an
// AttachGrant, mints an audience-bound attach token, and then proxies a
// WebSocket to the agent's ttyd — the writable (driver) or read-only (viewer)
// one, chosen from the SIGNED token's role so a viewer can never reach the
// writable PTY. It bypasses the Knative activator entirely (dials the agent's
// terminal Service directly).
type Gateway struct {
	SigningKey  []byte             // HMAC key for attach tokens (pkg/attachtoken)
	TrustDomain string             // SPIFFE trust domain, for the token audience
	OIDC        OIDCVerifier       // human authN for grant→token minting
	Grants      GrantResolver      // resolves a live AttachGrant
	Target      TargetResolver     // agent+role → ttyd base URL
	TokenTTL    time.Duration      // minted attach-token lifetime
	AllowOrigin map[string]bool    // permitted browser Origin hosts (besides none)
	Now         func() time.Time   // injectable clock
	Audit       func(e AuditEvent) // attach/detach/deny audit sink
}

// AuditEvent records an authorization decision for the audit log.
type AuditEvent struct {
	Action  string // "mint" | "attach" | "detach" | "deny"
	Subject string
	Agent   string // <ns>/<name>
	Role    string
	Grant   string
	Reason  string
}

func (g *Gateway) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Gateway) audit(e AuditEvent) {
	if g.Audit != nil {
		g.Audit(e)
	}
}

// Handler wires the routes. Go 1.22+ method+wildcard patterns.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /v1/terminal/{ns}/{agent}/grants", g.handleMint)
	mux.HandleFunc("GET /v1/terminal/{ns}/{agent}", g.handleAttach)
	return mux
}

// handleMint authenticates the human via OIDC, resolves an AttachGrant for
// (subject, agent), and returns an audience-bound, short-TTL attach token. No
// grant → 403 (no token is ever minted without a durable authorization record).
func (g *Gateway) handleMint(w http.ResponseWriter, r *http.Request) {
	ns, agent := r.PathValue("ns"), r.PathValue("agent")
	ref := ns + "/" + agent

	subject, err := g.OIDC.Verify(r.Context(), bearer(r))
	if err != nil {
		g.audit(AuditEvent{Action: "deny", Agent: ref, Reason: "oidc:" + err.Error()})
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	role, grant, ok := g.Grants.Resolve(r.Context(), ns, agent, subject, g.now())
	if !ok {
		g.audit(AuditEvent{Action: "deny", Subject: subject, Agent: ref, Reason: "no-grant"})
		http.Error(w, "no attach grant", http.StatusForbidden)
		return
	}
	exp := g.now().Add(g.tokenTTL())
	tok, err := attachtoken.Mint(g.SigningKey, attachtoken.Claims{
		Subject:  subject,
		Role:     attachtoken.Role(role),
		Audience: attachtoken.Audience(g.TrustDomain, ns, agent),
		AgentRef: ref,
		Expiry:   exp.Unix(),
		GrantID:  grant,
	})
	if err != nil {
		http.Error(w, "mint failed", http.StatusInternalServerError)
		return
	}
	g.audit(AuditEvent{Action: "mint", Subject: subject, Agent: ref, Role: role, Grant: grant})
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "role": role, "expiresAt": exp.UTC().Format(time.RFC3339)})
}

// handleAttach verifies the attach token (audience-bound to THIS agent), checks
// Origin, then reverse-proxies the WebSocket to the role-selected ttyd, injecting
// the auth header ttyd requires. The port is chosen from the signed role, so a
// viewer token can never reach the writable ttyd.
func (g *Gateway) handleAttach(w http.ResponseWriter, r *http.Request) {
	ns, agent := r.PathValue("ns"), r.PathValue("agent")
	ref := ns + "/" + agent
	aud := attachtoken.Audience(g.TrustDomain, ns, agent)

	claims, err := attachtoken.Verify(g.SigningKey, attachToken(r), aud, g.now().Unix())
	if err != nil {
		g.audit(AuditEvent{Action: "deny", Agent: ref, Reason: "token:" + err.Error()})
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !g.originAllowed(r) {
		g.audit(AuditEvent{Action: "deny", Subject: claims.Subject, Agent: ref, Reason: "origin"})
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	target, err := g.Target.TTYD(ns, agent, string(claims.Role))
	if err != nil {
		http.Error(w, "no terminal endpoint", http.StatusBadGateway)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// ttyd --auth-header: present the authenticated identity. Strip any
			// client-supplied value first so it can't be forged.
			req.Header.Del(authHeaderName)
			req.Header.Set(authHeaderName, claims.Subject)
		},
	}
	g.audit(AuditEvent{Action: "attach", Subject: claims.Subject, Agent: ref, Role: string(claims.Role), Grant: claims.GrantID})
	proxy.ServeHTTP(w, r)
	g.audit(AuditEvent{Action: "detach", Subject: claims.Subject, Agent: ref, Role: string(claims.Role), Grant: claims.GrantID})
}

// originAllowed permits requests with no Origin (non-browser CLI clients, which
// authenticate by token) and browser requests whose Origin host is allow-listed;
// it rejects any other cross-origin browser request.
func (g *Gateway) originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true // non-browser client; the bearer token is the control
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return g.AllowOrigin[u.Host]
}

func (g *Gateway) tokenTTL() time.Duration {
	if g.TokenTTL > 0 {
		return g.TokenTTL
	}
	return 2 * time.Minute
}

// authHeaderName mirrors builders.TerminalAuthHeader (the header ttyd requires).
const authHeaderName = "X-Smol-Attach"

// bearer extracts a bearer token from the Authorization header.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if v, ok := strings.CutPrefix(h, "Bearer "); ok {
		return v
	}
	return ""
}

// attachToken extracts the attach token from the Authorization bearer or, for a
// browser WebSocket that cannot set headers, the ?token= query parameter.
func attachToken(r *http.Request) string {
	if b := bearer(r); b != "" {
		return b
	}
	return r.URL.Query().Get("token")
}

// OIDCVerifier authenticates a human bearer (an OIDC ID token), returning the
// subject. Pluggable so the gateway can be unit-tested without a live issuer.
type OIDCVerifier interface {
	Verify(ctx context.Context, bearer string) (subject string, err error)
}

// GrantResolver resolves a live (unexpired) AttachGrant for (ns, agent, subject)
// to its role + name, or ok=false.
type GrantResolver interface {
	Resolve(ctx context.Context, ns, agent, subject string, now time.Time) (role, grantName string, ok bool)
}

// TargetResolver maps an agent + role to the base URL of its ttyd (driver or
// viewer port on the terminal Service).
type TargetResolver interface {
	TTYD(ns, agent, role string) (*url.URL, error)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
