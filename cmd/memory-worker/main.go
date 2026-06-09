// Command memory-worker is the retrieval worker data plane for smol-agents-memory.
//
// It serves the internal retrieval API (HTTP+JSON over mTLS) defined in
// pkg/memory/api, wired to a configurable memory.Backend. The P1 default is the
// in-memory VectorBackend (no external DB), so the e2e probe and unit tests work
// without standing up pgvector or Qdrant.
//
// Flags:
//
//	-addr                  listener address (default :8444)
//	-backend               backend kind: vector-inmem|agentfs|pgvector|qdrant|redis|neo4j|eventlog (default: vector-inmem)
//	-backend-endpoint      backend connection string / DSN / address (driver-specific)
//	-backend-auth-secret   broker secret name for backend credentials
//	-backend-dims          embedding vector dimensions for pgvector/qdrant (default 1536)
//	-agentfs-mount         mount path for the agentfs backend (default /var/memory-agentfs)
//	-spire-socket          SPIRE workload-API socket path
//	-identity-mode         insecure|permissive|strict (default: permissive)
//	-allowed-tenants       comma-separated list of tenants this worker serves (empty = all)
//	-embed-endpoint        OpenAI-compatible embeddings endpoint URL (optional)
//	-embed-model           embedding model name (required with -embed-endpoint)
//	-embed-secret          secret broker name for the embedding API key
//	-embed-dims            embedding vector dimensions (default 1536)
//	-embed-fake-dims       use FakeEmbedder with this many dims (0 = disabled; takes priority)
//	-embed-cache-size      LRU embedding cache size in entries (0 = disabled, default 1024)
//	-broker-socket         secrets broker socket path (required when using -embed-endpoint)
//	-chunk-max-bytes       max chunk size in bytes (0 = no chunking, default 1024)
//	-chunk-overlap         overlap bytes between chunks (default 128)
//	-summarize-endpoint    OpenAI-compatible chat completions URL (enables summarize_memory)
//	-summarize-model       LLM model name for summarization
//	-summarize-secret      broker secret name for summarization API key
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

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/smol-platform/smol-agents/pkg/agentfs"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/identity"
	"github.com/smol-platform/smol-agents/pkg/memory"
	"github.com/smol-platform/smol-agents/pkg/memory/api"
	apigrpc "github.com/smol-platform/smol-agents/pkg/memory/api/grpc"
	"github.com/smol-platform/smol-agents/pkg/memory/worker"
	"github.com/smol-platform/smol-agents/pkg/secrets"
)

func main() {
	addr := flag.String("addr", ":8444", "listener address")
	transport := flag.String("transport", "http", "internal transport: http|grpc (both over mTLS in non-insecure mode)")
	backendKind := flag.String("backend", "vector-inmem", "backend kind: vector-inmem|agentfs|pgvector|qdrant|redis|neo4j|eventlog")
	backendEndpoint := flag.String("backend-endpoint", "", "backend DSN/address (pgvector: DSN, qdrant: host:port, redis: host:port, neo4j: bolt URI)")
	backendAuthSecret := flag.String("backend-auth-secret", "", "broker secret for backend credentials (format: user:password for neo4j, password-only for redis)")
	backendDims := flag.Int("backend-dims", 1536, "vector dimensions for pgvector/qdrant backends")
	backendMTLSSpiffeID := flag.String("backend-mtls-spiffe-id", "", "if set, dial the qdrant backend over SPIFFE-mTLS authorizing this peer SPIFFE ID (the mTLS sidecar, x9i.2/x9i.3); empty = insecure dial (Qdrant Cloud / dev only)")
	agentfsMount := flag.String("agentfs-mount", "/var/memory-agentfs", "mount path for the agentfs backend")
	spireSocket := flag.String("spire-socket", "unix:///run/spire/agent-sockets/api.sock", "SPIRE workload-API socket")
	identityMode := flag.String("identity-mode", "permissive", "insecure|permissive|strict")
	allowedTenants := flag.String("allowed-tenants", "", "comma-separated tenants (empty = all)")
	embedEndpoint := flag.String("embed-endpoint", "", "embeddings endpoint URL")
	embedModel := flag.String("embed-model", "", "embedding model name")
	embedSecret := flag.String("embed-secret", "", "broker secret name for embedding API key")
	embedDims := flag.Int("embed-dims", 1536, "embedding vector dimensions")
	embedFakeDims := flag.Int("embed-fake-dims", 0, "use FakeEmbedder with this many dims (takes priority over -embed-endpoint)")
	embedCacheSize := flag.Int("embed-cache-size", 1024, "LRU embedding cache size (0 = disabled)")
	brokerSocket := flag.String("broker-socket", "/run/secrets/broker.sock", "secrets broker socket path")
	chunkMaxBytes := flag.Int("chunk-max-bytes", 1024, "max chunk size in bytes (0 = no chunking)")
	chunkOverlap := flag.Int("chunk-overlap", 128, "overlap bytes between chunks")
	summarizeEndpoint := flag.String("summarize-endpoint", "", "OpenAI-compatible chat completions URL (enables summarize_memory)")
	summarizeModel := flag.String("summarize-model", "", "LLM model name for summarization")
	summarizeSecret := flag.String("summarize-secret", "", "broker secret name for summarization API key")
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

	case "agentfs":
		// Resolve S3 adapter: use real AWS S3 when a backend-endpoint (bucket)
		// is configured; otherwise fall back to in-memory FakeS3.
		var s3driver agentfs.S3
		if *backendEndpoint != "" {
			awsS3, s3Err := agentfs.NewAWSS3(ctx, agentfs.AWSS3Config{Bucket: *backendEndpoint})
			if s3Err != nil {
				logger.Error("agentfs: init S3", "err", s3Err)
				os.Exit(1)
			}
			s3driver = awsS3
			logger.Info("agentfs S3", "bucket", *backendEndpoint)
		} else {
			s3driver = agentfs.NewFakeS3()
			logger.Warn("agentfs snapshots are ephemeral (in-memory S3); set -backend-endpoint to a bucket name for durable snapshots")
		}
		backend = memory.NewAgentFSBackend(memory.AgentFSBackendConfig{
			Spec: v1.AgentFSSpec{MountPath: *agentfsMount, SizeGiB: 1},
			S3:   s3driver,
		})
		logger.Info("backend", "kind", "agentfs", "mount", *agentfsMount)

	case "pgvector":
		if *backendEndpoint == "" {
			logger.Error("pgvector backend requires -backend-endpoint (DSN)")
			os.Exit(2)
		}
		dsn := resolveDSN(ctx, *backendEndpoint, *backendAuthSecret, *brokerSocket, logger)
		pgb, pgErr := memory.NewPgvectorBackend(ctx, memory.PgvectorConfig{
			DSN:           dsn,
			EmbeddingDims: *backendDims,
		})
		if pgErr != nil {
			logger.Error("pgvector backend", "err", pgErr)
			os.Exit(1)
		}
		if schemaErr := pgb.EnsureSchema(ctx); schemaErr != nil {
			logger.Error("pgvector EnsureSchema", "err", schemaErr)
			os.Exit(1)
		}
		backend = pgb
		logger.Info("backend", "kind", "pgvector", "dims", *backendDims)

	case "qdrant":
		if *backendEndpoint == "" {
			logger.Error("qdrant backend requires -backend-endpoint (host:port)")
			os.Exit(2)
		}
		qCfg := memory.QdrantConfig{
			Addr:          *backendEndpoint,
			Collection:    "memory",
			EmbeddingDims: uint64(*backendDims),
		}
		// x9i.3: dial the Qdrant SPIFFE-mTLS sidecar when an authorized peer ID is
		// given (the in-cluster secure path). Without the flag we dial insecure —
		// Qdrant Cloud (its own TLS + API key) or dev only. The source lives for
		// the process (closed at exit), mirroring the server source below.
		if *backendMTLSSpiffeID != "" {
			peerID, idErr := spiffeid.FromString(*backendMTLSSpiffeID)
			if idErr != nil {
				logger.Error("backend-mtls-spiffe-id", "err", idErr)
				os.Exit(2)
			}
			src, srcErr := identity.Open(ctx, identity.SourceConfig{
				WorkloadAPIAddr: *spireSocket,
				BootTimeout:     30 * time.Second,
				Mode:            identity.ModeStrict,
			})
			if srcErr != nil {
				logger.Error("qdrant mTLS identity source", "err", srcErr)
				os.Exit(1)
			}
			defer func() { _ = src.Close() }()
			qCfg.TLS = tlsconfig.MTLSClientConfig(src.X509Source(), src.X509Source(), tlsconfig.AuthorizeID(peerID))
			logger.Info("qdrant mTLS enabled", "peer", peerID.String())
		}
		qb, qErr := memory.NewQdrantBackend(ctx, qCfg)
		if qErr != nil {
			logger.Error("qdrant backend", "err", qErr)
			os.Exit(1)
		}
		if collErr := qb.EnsureCollection(ctx); collErr != nil {
			logger.Error("qdrant EnsureCollection", "err", collErr)
			os.Exit(1)
		}
		backend = qb
		logger.Info("backend", "kind", "qdrant", "addr", *backendEndpoint)

	case "redis":
		if *backendEndpoint == "" {
			logger.Error("redis backend requires -backend-endpoint (host:port)")
			os.Exit(2)
		}
		redisPwd := resolveSecret(ctx, *backendAuthSecret, *brokerSocket, logger)
		rb, rErr := memory.NewRedisBackend(ctx, memory.RedisConfig{
			Addr:     *backendEndpoint,
			Password: redisPwd,
		})
		if rErr != nil {
			logger.Error("redis backend", "err", rErr)
			os.Exit(1)
		}
		backend = rb
		logger.Info("backend", "kind", "redis", "addr", *backendEndpoint)

	case "neo4j":
		if *backendEndpoint == "" {
			logger.Error("neo4j backend requires -backend-endpoint (bolt URI)")
			os.Exit(2)
		}
		user, pass := resolveNeo4jCreds(ctx, *backendAuthSecret, *brokerSocket, logger)
		nb, nErr := memory.NewNeo4jBackend(ctx, memory.Neo4jConfig{
			URI:      *backendEndpoint,
			Username: user,
			Password: pass,
		})
		if nErr != nil {
			logger.Error("neo4j backend", "err", nErr)
			os.Exit(1)
		}
		if schemaErr := nb.EnsureSchema(ctx); schemaErr != nil {
			logger.Error("neo4j EnsureSchema", "err", schemaErr)
			os.Exit(1)
		}
		backend = nb
		logger.Info("backend", "kind", "neo4j", "uri", *backendEndpoint)

	case "eventlog":
		backend = memory.NewEventLogBackend()
		logger.Info("backend", "kind", "eventlog")

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
	// Wrap embedder in LRU cache when requested.
	if emb != nil && *embedCacheSize > 0 {
		emb = worker.NewCachedEmbedder(emb, *embedCacheSize)
		logger.Info("embedder cache", "size", *embedCacheSize)
	}

	// ── Summarizer ───────────────────────────────────────────────────────────
	var summ worker.Summarizer
	if *summarizeEndpoint != "" {
		if *summarizeModel == "" {
			logger.Error("-summarize-model is required with -summarize-endpoint")
			os.Exit(2)
		}
		if *summarizeSecret == "" {
			logger.Error("-summarize-secret is required with -summarize-endpoint")
			os.Exit(2)
		}
		ms, summErr := worker.NewModelProviderSummarizer(worker.SummarizerConfig{
			Endpoint:   *summarizeEndpoint,
			Model:      *summarizeModel,
			SecretName: *summarizeSecret,
		}, *brokerSocket)
		if summErr != nil {
			logger.Error("summarizer", "err", summErr)
			os.Exit(1)
		}
		summ = ms
		logger.Info("summarizer", "kind", "model-provider", "model", *summarizeModel)
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
	if summ != nil {
		svc.WithSummarizer(summ)
	}

	if *transport != "http" && *transport != "grpc" {
		logger.Error("--transport must be http or grpc", "got", *transport)
		os.Exit(2)
	}

	// ── Listener + (optional) mTLS ─────────────────────────────────────────────
	mode, err := identity.ParseMode(*identityMode)
	if err != nil {
		logger.Error("identity mode", "err", err)
		os.Exit(2)
	}
	rawLn, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Error("listen", "addr", *addr, "err", err)
		os.Exit(1)
	}
	var tlsCfg *tls.Config // nil ⇒ insecure (no mTLS)
	if mode == identity.ModeInsecure {
		logger.Warn("running without mTLS — insecure mode", "addr", *addr)
	} else {
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
		tlsCfg = tlsconfig.MTLSServerConfig(
			idSrc.X509Source(), idSrc.X509Source(),
			tlsconfig.AuthorizeAny(), // peer identity gated at deploy time (NetworkPolicy/RBAC)
		)
		logger.Info("mTLS enabled", "mode", mode)
	}

	// ── Serve (gRPC or HTTP, same RetrievalService over the same mTLS) ─────────
	if *transport == "grpc" {
		var opts []grpc.ServerOption
		if tlsCfg != nil {
			opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
		}
		grpcSrv := apigrpc.NewGRPCServer(svc, opts...)
		go func() { <-ctx.Done(); logger.Info("shutting down (grpc)"); grpcSrv.GracefulStop() }()
		logger.Info("memory-worker ready", "addr", *addr, "transport", "grpc", "backend", *backendKind)
		if serveErr := grpcSrv.Serve(rawLn); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			logger.Error("serve grpc", "err", serveErr)
			os.Exit(1)
		}
		return
	}

	ln := rawLn
	if tlsCfg != nil {
		ln = tls.NewListener(rawLn, tlsCfg)
	}
	srv := &http.Server{Handler: api.NewHTTPServer(svc), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		logger.Info("shutting down (http)")
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
	}()
	logger.Info("memory-worker ready", "addr", *addr, "transport", "http", "backend", *backendKind)
	if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		logger.Error("serve http", "err", serveErr)
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

// resolveDSN resolves a backend DSN. When backendAuthSecret is set, fetches
// the credential from the broker and appends it to the endpoint as the DSN
// password (PostgreSQL DSN format). When the secret is empty the endpoint
// is returned as-is.
func resolveDSN(ctx context.Context, endpoint, secretName, brokerSocket string, logger *slog.Logger) string {
	if secretName == "" {
		return endpoint
	}
	cred := resolveSecret(ctx, secretName, brokerSocket, logger)
	if cred == "" {
		return endpoint
	}
	// Append password to the DSN if not already present.
	if strings.Contains(endpoint, "password=") || strings.Contains(endpoint, ":@") {
		return endpoint
	}
	// Naive append for the postgres://user@host/db form.
	return endpoint + "&password=" + cred
}

// resolveSecret fetches a secret value from the broker. Returns "" on error
// (logged as a warning; backends may still work without auth).
func resolveSecret(ctx context.Context, secretName, brokerSocket string, logger *slog.Logger) string {
	if secretName == "" {
		return ""
	}
	client := secrets.NewClient(brokerSocket)
	lease, err := client.Lease(ctx, secretName, 0)
	if err != nil {
		logger.Warn("resolve secret", "name", secretName, "err", err)
		return ""
	}
	return string(lease.Value)
}

// resolveNeo4jCreds resolves Neo4j username:password from the broker.
// The secret value is expected to be in "username:password" format.
func resolveNeo4jCreds(ctx context.Context, secretName, brokerSocket string, logger *slog.Logger) (string, string) {
	raw := resolveSecret(ctx, secretName, brokerSocket, logger)
	if raw == "" {
		return "neo4j", ""
	}
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "neo4j", raw
}
