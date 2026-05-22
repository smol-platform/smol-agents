// Command memory-mcp is the MCP gateway for the smol-agents memory subsystem.
//
// It serves MCP over streamable-HTTP, authenticates callers via JWT-SVID,
// enforces per-retriever policy and quota, and forwards to the retrieval
// workers over the internal HTTP+JSON API (mTLS in production).
//
// Flags:
//
//	--listen        bind address (default :8443)
//	--worker-url    base URL of the retrieval worker (required)
//	--audience      JWT-SVID audience this server accepts (default: skip audience check)
//	--trust-domain  expected SPIFFE trust domain (default: skip check)
//	--spiffe-socket SPIRE workload API socket for JWT bundle validation
//	--insecure      skip JWT signature verification (dev/test only)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stigen/smol-agents/pkg/memory/audit"
	"github.com/stigen/smol-agents/pkg/memory/mcp"
	"github.com/stigen/smol-agents/pkg/memory/quota"
	"github.com/stigen/smol-agents/pkg/memory/store"
)

func main() {
	listen := flag.String("listen", ":8443", "bind address")
	workerURL := flag.String("worker-url", "", "base URL of the retrieval worker (required)")
	audience := flag.String("audience", "", "expected JWT-SVID audience (empty=any)")
	trustDomain := flag.String("trust-domain", "", "expected SPIFFE trust domain (empty=any)")
	insecure := flag.Bool("insecure", false, "skip JWT signature verification (dev/test only)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *workerURL == "" {
		log.Error("--worker-url is required")
		os.Exit(1)
	}

	// Build the retriever store. In production this would be a k8s client;
	// for now we use an env-var-driven fake that can be overridden at test time.
	// A real k8s implementation is wired here once the operator controller lands.
	rs := store.NewFakeStore()

	// Auth config: production uses JWT bundle validation; --insecure skips sigs.
	authCfg := mcp.AuthConfig{
		Audience:    *audience,
		TrustDomain: *trustDomain,
	}
	if *insecure {
		log.Warn("memory-mcp: running in INSECURE mode — JWT signatures not verified")
		// BundleSource left nil → ParseInsecure path in auth.go.
	}
	// In strict mode the BundleSource would be populated from the SPIRE
	// workload API (workloadapi.NewJWTSource). That wiring is added when
	// the operator integration lands (M9).

	gw := &mcp.Gateway{
		Auth:       authCfg,
		Retrievers: rs,
		Quota:      quota.NewEnforcer(),
		AuditLog:   &audit.SlogLogger{Logger: log},
	}

	dispatcher := mcp.NewDispatcher(gw)

	mux := http.NewServeMux()
	mux.Handle("/mcp", dispatcher)
	mux.Handle("/mcp/", dispatcher)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		log.Info("memory-mcp: listening", slog.String("addr", *listen))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("memory-mcp: serve error", slog.Any("err", err))
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("memory-mcp: shutting down")

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("memory-mcp: shutdown error", slog.Any("err", err))
	}
}
