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
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/identity"
	"github.com/stigen/smol-agents/pkg/memory/api"
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
	insecure := flag.Bool("insecure", false, "skip JWT signature verification + call worker over plain HTTP (dev/test only)")
	spireSocket := flag.String("spire-socket", "unix:///run/spire/agent-sockets/api.sock", "SPIRE workload-API socket (used unless --insecure)")
	retrieversConfig := flag.String("retrievers-config", "", "path to a JSON file mapping retrieverRef -> MemoryRetrieverSpec (dev/e2e until the k8s store lands)")
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
	if *retrieversConfig != "" {
		raw, err := os.ReadFile(*retrieversConfig)
		if err != nil {
			log.Error("read retrievers-config", "err", err)
			os.Exit(1)
		}
		var specs map[string]v1.MemoryRetrieverSpec
		if err := json.Unmarshal(raw, &specs); err != nil {
			log.Error("parse retrievers-config", "err", err)
			os.Exit(1)
		}
		for ref, spec := range specs {
			rs.Add(ref, store.RetrieverInfo{Spec: spec, WorkerURL: *workerURL})
		}
		log.Info("loaded retrievers from config", "count", len(specs), "path", *retrieversConfig)
	}

	authCfg := mcp.AuthConfig{
		Audience:    *audience,
		TrustDomain: *trustDomain,
	}
	gw := &mcp.Gateway{
		Retrievers: rs,
		Quota:      quota.NewEnforcer(),
		AuditLog:   &audit.SlogLogger{Logger: log},
	}

	if *insecure {
		log.Warn("memory-mcp: INSECURE mode — JWT signatures not verified, worker called over plain HTTP")
		// BundleSource nil → ParseInsecure; default WorkerFactory → plain HTTP.
	} else {
		// Secure transport: validate inbound JWT-SVIDs against the SPIRE JWT
		// bundle (R-MEM-AUTH-1) and call the worker over mTLS with our X509-SVID.
		src, err := identity.Open(context.Background(), identity.SourceConfig{
			WorkloadAPIAddr: *spireSocket,
			BootTimeout:     30 * time.Second,
			Mode:            identity.ModeStrict,
		})
		if err != nil {
			log.Error("identity source (pass --insecure for dev without SPIRE)", "err", err)
			os.Exit(1)
		}
		defer src.Close()
		authCfg.BundleSource = src.JWTSource()
		mtls := &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: tlsconfig.MTLSClientConfig(src.X509Source(), src.X509Source(), tlsconfig.AuthorizeAny()),
			},
		}
		gw.WorkerFactory = func(url string) api.RetrievalService { return api.NewHTTPClient(url, mtls) }
		log.Info("memory-mcp: secure transport (JWT-SVID validation + mTLS to worker)")
	}
	gw.Auth = authCfg

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
