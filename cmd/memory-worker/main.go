// Command memory-worker is the retrieval worker data plane for smol-agents-memory.
//
// It serves the internal retrieval API (HTTP+JSON over mTLS) defined in
// pkg/memory/api, wired to a configurable memory.Backend. The P1 default is the
// in-memory VectorBackend (no external DB), so the e2e probe and unit tests work
// without standing up pgvector or Qdrant.
//
// Flags:
//
//	-addr               listener address (default :8444)
//	-backend            backend kind: vector-inmem (default; more to follow)
//	-spire-socket       SPIRE workload-API socket path
//	-identity-mode      insecure|permissive|strict (default: permissive)
//	-allowed-tenants    comma-separated list of tenants this worker serves (empty = all)
//	-embed-endpoint     OpenAI-compatible embeddings endpoint URL (optional)
//	-embed-model        embedding model name (required with -embed-endpoint)
//	-embed-secret       secret broker name for the embedding API key
//	-embed-dims         embedding vector dimensions (default 1536)
//	-embed-fake-dims    use FakeEmbedder with this many dims (0 = disabled; takes priority)
//	-broker-socket      secrets broker socket path (required when using -embed-endpoint)
//	-chunk-max-bytes    max chunk size in bytes (0 = no chunking, default 1024)
//	-chunk-overlap      overlap bytes between chunks (default 128)
//
// The binary is modelled on cmd/fake-gateway/main.go (graceful shutdown via
// signal context, slog JSON logger).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/stigen/smol-agents/pkg/identity"
	"github.com/stigen/smol-agents/pkg/memory"
	"github.com/stigen/smol-agents/pkg/memory/api"
	"github.com/stigen/smol-agents/pkg/memory/worker"
)

func main() {
	addr := flag.String("addr", ":8444", "listener address")
	backendKind := flag.String("backend", "vector-inmem", "backend kind: vector-inmem")
	spireSocket := flag.String("spire-socket", "unix:///run/spire/agent-sockets/api.sock", "SPIRE workload-API socket")
	identityMode := flag.String("identity-mode", "permissive", "insecure|permissive|strict")
	allowedTenants := flag.String("allowed-tenants", "", "comma-separated tenants (empty = all)")
	embedEndpoint := flag.String("embed-endpoint", "", "embeddings endpoint URL")
	embedModel := flag.String("embed-model", "", "embedding model name")
	embedSecret := flag.String("embed-secret", "", "broker secret name for embedding API key")
	embedDims := flag.Int("embed-dims", 1536, "embedding vector dimensions")
	embedFakeDims := flag.Int("embed-fake-dims", 0, "use FakeEmbedder with this many dims (takes priority over -embed-endpoint)")
	brokerSocket := flag.String("broker-socket", "/run/secrets/broker.sock", "secrets broker socket path")
	chunkMaxBytes := flag.Int("chunk-max-bytes", 1024, "max chunk size in bytes (0 = no chunking)")
	chunkOverlap := flag.Int("chunk-overlap", 128, "overlap bytes between chunks")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// ── Backend ─────────────────────────────────────────────────────────────
	var backend memory.Backend
	switch *backendKind {
	case "vector-inmem":
		backend = memory.NewVectorBackend()
		logger.Info("backend", "kind", "vector-inmem")
	default:
		logger.Error("unknown backend kind", "kind", *backendKind)
		os.Exit(2)
	}

	// ── Embedder ────────────────────────────────────────────────────────────
	var emb worker.Embedder
	if *embedFakeDims > 0 {
		fe, err := worker.NewFakeEmbedder(*embedFakeDims)
		if err != nil {
			logger.Error("fake embedder", "err", err)
			os.Exit(1)
		}
		emb = fe
		logger.Info("embedder", "kind", "fake", "dims", *embedFakeDims)
	} else if *embedEndpoint != "" {
		if *embedModel == "" {
			logger.Error("-embed-model is required with -embed-endpoint")
			os.Exit(2)
		}
		if *embedSecret == "" {
			logger.Error("-embed-secret is required with -embed-endpoint")
			os.Exit(2)
		}
		me, err := worker.NewModelProviderEmbedder(worker.ModelProviderConfig{
			Endpoint:   *embedEndpoint,
			Model:      *embedModel,
			SecretName: *embedSecret,
			Dims:       *embedDims,
		}, *brokerSocket)
		if err != nil {
			logger.Error("model provider embedder", "err", err)
			os.Exit(1)
		}
		emb = me
		logger.Info("embedder", "kind", "model-provider", "model", *embedModel, "dims", *embedDims)
	} else {
		logger.Info("embedder", "kind", "none (text-scoring fallback)")
	}

	// ── Worker ──────────────────────────────────────────────────────────────
	var tenants []string
	if *allowedTenants != "" {
		for _, t := range strings.Split(*allowedTenants, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tenants = append(tenants, t)
			}
		}
	}

	cfg := worker.Config{
		Chunk: worker.ChunkSpec{
			MaxBytes:     *chunkMaxBytes,
			OverlapBytes: *chunkOverlap,
		},
		AllowedTenants: tenants,
	}
	svc, err := worker.New(cfg, worker.StaticSelector(backend), emb, logger)
	if err != nil {
		logger.Error("create worker", "err", err)
		os.Exit(1)
	}

	// ── mTLS listener ────────────────────────────────────────────────────────
	mode, err := identity.ParseMode(*identityMode)
	if err != nil {
		logger.Error("identity mode", "err", err)
		os.Exit(2)
	}

	var ln net.Listener
	switch mode {
	case identity.ModeInsecure:
		ln, err = net.Listen("tcp", *addr)
		if err != nil {
			logger.Error("listen (insecure)", "addr", *addr, "err", err)
			os.Exit(1)
		}
		logger.Warn("running without mTLS — insecure mode", "addr", *addr)

	default: // permissive or strict
		idSrc, srcErr := identity.Open(ctx, identity.SourceConfig{
			WorkloadAPIAddr: *spireSocket,
			BootTimeout:     30 * time.Second,
			Mode:            mode,
		})
		if srcErr != nil {
			logger.Error("identity source", "err", srcErr)
			os.Exit(1)
		}
		defer func() { _ = idSrc.Close() }()

		tlsCfg := tlsconfig.MTLSServerConfig(
			idSrc.X509Source(),
			idSrc.X509Source(),
			tlsconfig.AuthorizeAny(), // gateway identity is checked at deploy time via NetworkPolicy/RBAC
		)
		rawLn, listenErr := net.Listen("tcp", *addr)
		if listenErr != nil {
			logger.Error("listen", "addr", *addr, "err", listenErr)
			os.Exit(1)
		}
		ln = tls.NewListener(rawLn, tlsCfg)
		logger.Info("mTLS listener ready", "addr", *addr, "mode", mode)
	}

	// ── HTTP server ──────────────────────────────────────────────────────────
	handler := api.NewHTTPServer(svc)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Shut down the listener when the signal context is cancelled.
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
	}()

	logger.Info("memory-worker ready", "addr", *addr, "backend", *backendKind)
	if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		logger.Error("serve", "err", serveErr)
		os.Exit(1)
	}
}

// flagUsage wraps flag.Usage to print a hint.
func init() {
	orig := flag.Usage
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: memory-worker [flags]\n\n")
		orig()
	}
}
