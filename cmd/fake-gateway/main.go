// Command fake-gateway is the SVID-aware echo gateway for the
// fullstack-e2e suite.
//
//	-tcp-addr  byte-for-byte echo over an mTLS listener that requires
//	           a peer SPIFFE SVID matching --authorize-tcp.
//	-http-addr HTTP server that echoes the request shape as JSON
//	           and validates a JWT-SVID Bearer token whose audience
//	           matches --jwt-audience.
//
// Both listeners record every accepted SPIFFE ID for assertion at
// GET /_observed.
//
// Implements R-E2E-L0-4.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

func main() {
	tcpAddr := flag.String("tcp-addr", ":8443", "mTLS echo TCP listener")
	httpAddr := flag.String("http-addr", ":8080", "HTTP echo + JWT validator")
	jwtAudience := flag.String("jwt-audience", "", "expected JWT-SVID audience for HTTP path")
	authorizeTCP := flag.String("authorize-tcp", "", "SPIFFE ID prefix that may dial the TCP echo (e.g. spiffe://smol-agents.ai/ns/tenant-a)")
	socketPath := flag.String("spire-socket", "unix:///run/spire/agent-sockets/api.sock", "SPIRE workload-API socket")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if *jwtAudience == "" {
		logger.Error("--jwt-audience is required")
		os.Exit(2)
	}
	if *authorizeTCP == "" {
		logger.Error("--authorize-tcp is required")
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	jwtSrc, err := workloadapi.NewJWTSource(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(*socketPath)))
	if err != nil {
		logger.Error("jwt source", "err", err)
		os.Exit(1)
	}
	defer jwtSrc.Close()

	obs := &observed{}
	go runTCP(ctx, *tcpAddr, *socketPath, *authorizeTCP, obs, logger)
	runHTTP(ctx, *httpAddr, *jwtAudience, jwtSrc, obs, logger)
}

// observed records every accepted SPIFFE ID for assertion.
type observed struct {
	mu  sync.Mutex
	IDs []string
}

func (o *observed) record(id string) { o.mu.Lock(); o.IDs = append(o.IDs, id); o.mu.Unlock() }
func (o *observed) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, len(o.IDs))
	copy(out, o.IDs)
	return out
}

// ----------------------------- TCP path -----------------------------

func runTCP(ctx context.Context, addr, socketPath, authorizePrefix string, obs *observed, logger *slog.Logger) {
	src, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(socketPath)))
	if err != nil {
		logger.Error("tcp x509 source", "err", err)
		return
	}
	defer src.Close()

	// Plain tls.NewListener so accepted conns are *tls.Conn — that
	// type exposes ConnectionState() so peerID can pull the SPIFFE
	// ID out of the URI SAN. tlsconfig.MTLSServerConfig handles
	// the mTLS verification against the trust bundle.
	tlsCfg := tlsconfig.MTLSServerConfig(src, src, tlsconfig.AuthorizeAny())
	rawLn, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("tcp listen", "err", err)
		return
	}
	ln := tls.NewListener(rawLn, tlsCfg)
	defer ln.Close()
	logger.Info("tcp echo", "addr", addr, "authorizePrefix", authorizePrefix)

	go func() { <-ctx.Done(); _ = ln.Close() }()

	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			logger.Warn("tcp accept", "err", err)
			continue
		}
		go handleTCP(c, authorizePrefix, obs, logger)
	}
}

func handleTCP(c net.Conn, authorizePrefix string, obs *observed, logger *slog.Logger) {
	defer c.Close()
	// Explicitly handshake — peerID needs PeerCertificates which
	// only populate after the TLS handshake completes. Read/Write
	// would auto-handshake, but we read peer state BEFORE either,
	// so without this the cert chain is empty.
	if tc, ok := c.(*tls.Conn); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tc.HandshakeContext(ctx); err != nil {
			logger.Warn("tcp handshake", "err", err)
			return
		}
	}
	id, ok := peerID(c)
	if !ok {
		logger.Warn("no peer SVID")
		return
	}
	if !strings.HasPrefix(id.String(), authorizePrefix) {
		logger.Warn("peer not authorized", "spiffeID", id, "want_prefix", authorizePrefix)
		return
	}
	obs.record(id.String())
	logger.Info("tcp peer", "spiffeID", id.String())
	_, _ = io.Copy(c, c)
}

// peerID extracts the peer SPIFFE ID. spiffetls returns its own
// connection wrapper exposing PeerID(); the standard *tls.Conn
// fallback parses the URI SAN. We try several known shapes since
// spiffetls' wrapper isn't exported as a public interface in 2.x.
type peerIDer interface{ PeerID() spiffeid.ID }

type connectionStater interface {
	ConnectionState() tls.ConnectionState
}

func peerID(c net.Conn) (spiffeid.ID, bool) {
	if p, ok := c.(peerIDer); ok {
		return p.PeerID(), true
	}
	if cs, ok := c.(connectionStater); ok {
		state := cs.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			for _, u := range state.PeerCertificates[0].URIs {
				if id, err := spiffeid.FromString(u.String()); err == nil {
					return id, true
				}
			}
		}
	}
	return spiffeid.ID{}, false
}

// ---------------------------- HTTP path -----------------------------

func runHTTP(ctx context.Context, addr, aud string, jwt *workloadapi.JWTSource, obs *observed, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("/", validateAndEcho(aud, jwt, obs, logger))
	mux.HandleFunc("GET /_observed", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(obs.snapshot())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	logger.Info("http echo", "addr", addr, "audience", aud)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http", "err", err)
	}
}

func validateAndEcho(aud string, src *workloadapi.JWTSource, obs *observed, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r.Header.Get("Authorization"))
		if tok == "" {
			http.Error(w, "missing Bearer JWT-SVID", http.StatusUnauthorized)
			return
		}
		svid, err := jwtsvid.ParseAndValidate(tok, src, []string{aud})
		if err != nil {
			logger.Warn("jwt validate", "err", err)
			http.Error(w, "invalid JWT-SVID: "+err.Error(), http.StatusUnauthorized)
			return
		}
		obs.record(svid.ID.String())

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"method":   r.Method,
			"path":     r.URL.Path,
			"spiffeID": svid.ID.String(),
			"audience": aud,
		})
	})
}

func bearer(h string) string {
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[len("Bearer "):])
}
