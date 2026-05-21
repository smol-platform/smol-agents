// Command fake-github is a stand-in for the GitHub App API + the GitHub REST
// resource surface, used by the fullstack-e2e secretless-egress scenario.
//
// It serves three things:
//
//	GET  /repos/{owner}/{repo}/installation        → {"id": <installation-id>}
//	POST /app/installations/{id}/access_tokens      → mints {"token","expires_at"}
//	GET  /repos/{owner}/{repo}                       → the agent-facing resource;
//	                                                    200 ONLY if Authorization
//	                                                    carries a token THIS server
//	                                                    minted, else 401.
//
// The first two are what the broker's GitHubAppBackend calls to mint a scoped
// installation token. The third is what the agent reaches *through the sidecar*:
// because it accepts only a server-minted token, a 200 there proves the whole
// mint→inject→upstream chain ran and that the injected credential (not the
// agent's JWT-SVID) was presented. Every accepted token is recorded for
// assertion at GET /_observed.
//
// Implements R-E2E-SCN-SECRETLESS (upstream side).
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	appID := flag.String("app-id", "", "expected App-JWT issuer (iss); empty = don't check")
	installID := flag.String("installation-id", "42", "installation id returned for any repo")
	tokenTTL := flag.Duration("token-ttl", time.Hour, "minted installation-token lifetime")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	srv := &server{
		appID:     *appID,
		installID: *installID,
		tokenTTL:  *tokenTTL,
		now:       time.Now,
		logger:    logger,
		minted:    map[string]struct{}{},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	httpSrv := &http.Server{Addr: *addr, Handler: srv.routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = httpSrv.Shutdown(sctx)
	}()
	logger.Info("fake-github", "addr", *addr, "appID", *appID, "installationID", *installID)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http", "err", err)
		os.Exit(1)
	}
}

type server struct {
	appID     string
	installID string
	tokenTTL  time.Duration
	now       func() time.Time
	logger    *slog.Logger

	mu       sync.Mutex
	minted   map[string]struct{} // installation tokens this server issued
	observed []string            // minted tokens seen on the resource endpoint
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	// More specific pattern first; Go 1.22 ServeMux precedence handles overlap.
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", s.handleInstallation)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", s.handleAccessTokens)
	mux.HandleFunc("GET /repos/{owner}/{repo}", s.handleResource)
	mux.HandleFunc("GET /_observed", s.handleObserved)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// handleInstallation resolves the installation for a repo. The broker calls
// this with an App-JWT (iss=AppID); we optionally check the issuer.
func (s *server) handleInstallation(w http.ResponseWriter, r *http.Request) {
	if tok := bearer(r.Header.Get("Authorization")); tok == "" {
		http.Error(w, "missing App-JWT", http.StatusUnauthorized)
		return
	} else if s.appID != "" && issuerOf(tok) != s.appID {
		http.Error(w, "App-JWT issuer mismatch", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{"id": atoiOr(s.installID, 42)})
}

// handleAccessTokens mints a short-lived installation token and remembers it.
func (s *server) handleAccessTokens(w http.ResponseWriter, r *http.Request) {
	if tok := bearer(r.Header.Get("Authorization")); tok == "" {
		http.Error(w, "missing App-JWT", http.StatusUnauthorized)
		return
	}
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	token := "ghs_" + randHex(20)
	s.mu.Lock()
	s.minted[token] = struct{}{}
	s.mu.Unlock()

	writeJSON(w, map[string]any{
		"token":        token,
		"expires_at":   s.now().Add(s.tokenTTL).UTC().Format(time.RFC3339),
		"repositories": body.Repositories,
		"permissions":  body.Permissions,
	})
}

// handleResource is the agent-facing endpoint. It accepts ONLY a token this
// server minted, so a 200 proves a broker-minted credential was injected.
func (s *server) handleResource(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r.Header.Get("Authorization"))
	if tok == "" {
		http.Error(w, "missing credential", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	_, ok := s.minted[tok]
	if ok {
		s.observed = append(s.observed, tok)
	}
	s.mu.Unlock()
	if !ok {
		// Not a token we minted (e.g. the agent's JWT-SVID, or a forgery).
		http.Error(w, "not a minted installation token", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{
		"full_name": r.PathValue("owner") + "/" + r.PathValue("repo"),
		"private":   true,
	})
}

func (s *server) handleObserved(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	seen := make([]string, len(s.observed))
	copy(seen, s.observed)
	s.mu.Unlock()
	writeJSON(w, map[string]any{
		"sawMintedToken": len(seen) > 0,
		"count":          len(seen),
	})
}

// --- helpers ---

func bearer(h string) string {
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[len("Bearer "):])
}

// issuerOf decodes (without verifying) the iss claim of a compact JWT.
func issuerOf(compact string) string {
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var cl struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &cl) != nil {
		return ""
	}
	return cl.Iss
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "deadbeef"
	}
	return hex.EncodeToString(b)
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
