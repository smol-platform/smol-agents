// Command fake-tts is a stand-in Tokenetes Transaction Token Service for the
// secretless-egress fullstack-e2e scenario. It implements just enough of RFC
// 8693 token-exchange to mint pkg/trat-compatible Transaction Tokens and serve
// the JWKS the broker verifies them against.
//
//	POST /token   form: grant_type, subject_token, subject_token_type,
//	              requested_token_type, audience, scope
//	              → {"access_token": <TraT>, "issued_token_type": txn_token,
//	                 "token_type": "N_A"}
//	GET  /jwks    → the RS256 public key set
//
// The minted TraT binds req_wl to the subject_token's `sub` (the agent's SPIFFE
// id) so the broker's sender-constraint check passes, and carries a configured
// rctx.repo (P1 has no request-derived rctx — the TTS authorizes a fixed repo
// per deployment). Implements R-E2E-SCN-SECRETLESS (TTS side).
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"

	"github.com/stigen/smol-agents/pkg/trat"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	repo := flag.String("repo", "stigen/app", "rctx.repo authorized for minted TraTs")
	ttl := flag.Duration("ttl", 5*time.Minute, "minted TraT lifetime")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	srv, err := newServer(*repo, *ttl)
	if err != nil {
		logger.Error("init", "err", err)
		os.Exit(1)
	}
	srv.logger = logger

	logger.Info("fake-tts", "addr", *addr, "repo", *repo, "ttl", ttl.String())
	httpSrv := &http.Server{Addr: *addr, Handler: srv.routes(), ReadHeaderTimeout: 5 * time.Second}
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http", "err", err)
		os.Exit(1)
	}
}

type server struct {
	repo   string
	ttl    time.Duration
	now    func() time.Time
	logger *slog.Logger

	key    *rsa.PrivateKey
	kid    string
	signer jose.Signer
}

func newServer(repo string, ttl time.Duration) (*server, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	kid := "fake-tts-1"
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("txntoken+jwt").WithHeader("kid", kid),
	)
	if err != nil {
		return nil, err
	}
	return &server{repo: repo, ttl: ttl, now: time.Now, key: key, kid: kid, signer: signer}, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", s.handleToken)
	mux.HandleFunc("GET /jwks", s.handleJWKS)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func (s *server) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &s.key.PublicKey, KeyID: s.kid, Algorithm: "RS256", Use: "sig",
	}}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(set)
}

func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("grant_type") != trat.GrantTokenExchange ||
		r.PostForm.Get("requested_token_type") != trat.TokenTypeTxn {
		http.Error(w, "unsupported grant/token type", http.StatusBadRequest)
		return
	}
	subjectToken := r.PostForm.Get("subject_token")
	sub := subjectOf(subjectToken)
	if sub == "" {
		http.Error(w, "subject_token missing sub", http.StatusBadRequest)
		return
	}
	audience := r.PostForm.Get("audience")
	scope := r.PostForm.Get("scope")

	now := s.now()
	reg := josejwt.Claims{
		Subject:  sub,
		Audience: josejwt.Audience{audience},
		Expiry:   josejwt.NewNumericDate(now.Add(s.ttl)),
		IssuedAt: josejwt.NewNumericDate(now),
	}
	custom := map[string]any{
		"scope":  scope,
		"req_wl": sub, // sender-constraint: TraT is bound to the requesting workload
		"rctx":   map[string]any{"repo": s.repo},
		"txn":    "txn-" + randHex(8),
	}
	compact, err := josejwt.Signed(s.signer).Claims(reg).Claims(custom).Serialize()
	if err != nil {
		s.logger.Error("sign", "err", err)
		http.Error(w, "sign failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":      compact,
		"issued_token_type": trat.TokenTypeTxn,
		"token_type":        "N_A",
	})
}

// subjectOf decodes (without verifying) the sub claim of a compact JWT. The
// fake TTS trusts the subject_token; a real TTS validates it against SPIRE.
func subjectOf(compact string) string {
	parts := splitDot(compact)
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var cl struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(payload, &cl) != nil {
		return ""
	}
	return cl.Sub
}

func splitDot(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "deadbeef"
	}
	const hexd = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, x := range b {
		out[i*2] = hexd[x>>4]
		out[i*2+1] = hexd[x&0xf]
	}
	return string(out)
}
