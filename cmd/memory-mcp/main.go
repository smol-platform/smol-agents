// Command memory-mcp is the MCP gateway for the smol-agents memory subsystem.
//
// It serves MCP over streamable-HTTP or stdio JSON-RPC, authenticates callers
// via JWT-SVID, enforces per-retriever policy and quota, and forwards to the
// retrieval workers over the internal HTTP+JSON API (mTLS in production).
//
// Flags:
//
//	--transport          "http" (default) or "stdio"
//	--listen             bind address for HTTP transport (default :8443)
//	--worker-url         base URL of the retrieval worker (required)
//	--audience           JWT-SVID audience this server accepts (default: skip check)
//	--trust-domain       expected SPIFFE trust domain (default: skip check)
//	--spiffe-socket      SPIRE workload-API socket (used unless --insecure)
//	--insecure           skip JWT signature verification + plain HTTP to worker
//	--retrievers-config  path to JSON file mapping retrieverRef → MemoryRetrieverSpec
//	--stdio-spiffe-id    SPIFFE ID injected as synthetic identity in stdio mode
//
// Transport notes
// ───────────────
// http (default): streamable-HTTP MCP, JWT-SVID auth, mTLS to worker.
//
// stdio: newline-delimited JSON-RPC on stdin/stdout, for local IDE tooling
// (VS Code, Claude Desktop, Zed). Requires --insecure; the OS process boundary
// is the security perimeter. Pass --stdio-spiffe-id with your actual workload
// SPIFFE ID so policy, quota, and audit records reflect the real identity.
// The synthetic token uses ParseInsecure (no signature check). Do NOT use stdio
// transport over a network socket.
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
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1 "github.com/stigen/smol-agents/operator/api/agentmodel/v1"
	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/identity"
	"github.com/stigen/smol-agents/pkg/memory/api"
	"github.com/stigen/smol-agents/pkg/memory/audit"
	"github.com/stigen/smol-agents/pkg/memory/mcp"
	"github.com/stigen/smol-agents/pkg/memory/quota"
	"github.com/stigen/smol-agents/pkg/memory/store"
)

func main() {
	transport := flag.String("transport", "http", `transport mode: "http" or "stdio"`)
	listen := flag.String("listen", ":8443", "bind address (HTTP transport)")
	workerURL := flag.String("worker-url", "", "base URL of the retrieval worker (required when not using k8s store with per-retriever URLs)")
	audience := flag.String("audience", "", "expected JWT-SVID audience (empty=any)")
	trustDomain := flag.String("trust-domain", "", "expected SPIFFE trust domain (empty=any)")
	insecure := flag.Bool("insecure", false, "skip JWT signature verification + call worker over plain HTTP (dev/test only)")
	spireSocket := flag.String("spire-socket", "unix:///run/spire/agent-sockets/api.sock", "SPIRE workload-API socket (used unless --insecure)")
	retrieversConfig := flag.String("retrievers-config", "", "path to a JSON file mapping retrieverRef -> MemoryRetrieverSpec (dev/e2e; mutually exclusive with --use-k8s-store)")
	useK8sStore := flag.Bool("use-k8s-store", false, "resolve MemoryRetriever CRs from the Kubernetes API (production)")
	k8sStoreCacheTTL := flag.Duration("k8s-store-cache-ttl", 30*time.Second, "how long to cache resolved MemoryRetriever entries from Kubernetes")
	stdioSPIFFEID := flag.String("stdio-spiffe-id", "spiffe://local/ns/local/sa/ide", "synthetic SPIFFE ID for stdio transport identity")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *workerURL == "" {
		log.Error("--worker-url is required")
		os.Exit(1)
	}

	switch *transport {
	case "http", "stdio":
	default:
		log.Error("--transport must be http or stdio", slog.String("got", *transport))
		os.Exit(1)
	}

	// stdio transport requires --insecure (no SPIRE in local IDE mode).
	if *transport == "stdio" && !*insecure {
		log.Error("--transport=stdio requires --insecure (stdio mode does not support SPIRE JWT-SVID validation)")
		os.Exit(1)
	}

	// ── Build the retriever store ──────────────────────────────────────────
	// --use-k8s-store: resolve MemoryRetriever CRs from the Kubernetes API
	//   (in-cluster or kubeconfig). The operator writes WorkerURLAnnotation on
	//   each CR when the worker Deployment + Service are ready.
	// --retrievers-config: seed a FakeStore from a JSON file (dev/e2e/stdio).
	// Both flags are mutually exclusive.
	if *useK8sStore && *retrieversConfig != "" {
		log.Error("--use-k8s-store and --retrievers-config are mutually exclusive")
		os.Exit(1)
	}

	var rs store.RetrieverStore
	switch {
	case *useK8sStore:
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			log.Error("build scheme", "err", err)
			os.Exit(1)
		}
		if err := operatorv1.AddToScheme(scheme); err != nil {
			log.Error("add operator scheme", "err", err)
			os.Exit(1)
		}
		cfg, err := ctrl.GetConfig()
		if err != nil {
			log.Error("get k8s config (in-cluster or kubeconfig)", "err", err)
			os.Exit(1)
		}
		k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			log.Error("build k8s client", "err", err)
			os.Exit(1)
		}
		k8sStore, err := store.NewK8sStore(store.K8sStoreConfig{
			Client:            k8sClient,
			CacheTTL:          *k8sStoreCacheTTL,
			WorkerURLFallback: *workerURL,
		})
		if err != nil {
			log.Error("build k8s store", "err", err)
			os.Exit(1)
		}
		rs = k8sStore
		log.Info("memory-mcp: using Kubernetes MemoryRetriever store",
			slog.Duration("cacheTTL", *k8sStoreCacheTTL))

	default:
		fakeRS := store.NewFakeStore()
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
				fakeRS.Add(ref, store.RetrieverInfo{Spec: spec, WorkerURL: *workerURL})
			}
			log.Info("loaded retrievers from config", "count", len(specs), "path", *retrieversConfig)
		}
		rs = fakeRS
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// ── Dispatch by transport ──────────────────────────────────────────────

	if *transport == "stdio" {
		log.Info("memory-mcp: stdio transport", slog.String("spiffeID", *stdioSPIFFEID))
		stdioRunner(ctx, dispatcher, os.Stdin, os.Stdout, *stdioSPIFFEID, log)
		log.Info("memory-mcp: stdio transport exiting")
		return
	}

	// HTTP transport (default).
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
