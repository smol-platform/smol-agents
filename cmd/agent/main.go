// Command agent is the smol-agents runtime entrypoint.
//
// Wires identity, transport, secrets client, eBPF, health, observability,
// and runs them under a Manager that gracefully drains on SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/stigen/smol-agents/internal/version"
	"github.com/stigen/smol-agents/pkg/config"
	"github.com/stigen/smol-agents/pkg/ebpf"
	"github.com/stigen/smol-agents/pkg/health"
	"github.com/stigen/smol-agents/pkg/identity"
	"github.com/stigen/smol-agents/pkg/observability"
	"github.com/stigen/smol-agents/pkg/runtime"
	"github.com/stigen/smol-agents/pkg/secrets"
	"github.com/stigen/smol-agents/pkg/transport"
)

func main() {
	configPath := flag.String("config", "", "path to agent.yaml")
	logLevel := flag.String("log-level", "info", "debug|info|warn|error")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		os.Stdout.WriteString(version.String() + "\n")
		return
	}

	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(*logLevel))
	logger := observability.MustLogger(level)
	slog.SetDefault(logger)

	cfg, err := config.LoadAgent(*configPath)
	if err != nil {
		logger.Error("config load", "err", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	obsShut, err := observability.Init(ctx, observability.Config{
		ServiceName:    cfg.Observability.ServiceName,
		ServiceVersion: version.Version,
		OTLPEndpoint:   cfg.Observability.OTLPEndpoint,
		Environment:    os.Getenv("SMOL_AGENTS_ENV"),
		Insecure:       true, // in-cluster collector typically uses insecure gRPC
	})
	if err != nil {
		logger.Error("observability init", "err", err)
		os.Exit(2)
	}
	defer func() {
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = obsShut(shutCtx)
	}()

	mgr := runtime.NewManager(logger)
	mgr.DrainTimeout = cfg.Runtime.DrainTimeout
	mgr.ShutdownTimeout = cfg.Runtime.ShutdownTimeout

	// Identity: blocking with bounded boot timeout (R-IDN-1 #1).
	idMode, err := identity.ParseMode(string(cfg.Mode))
	if err != nil {
		logger.Error("identity mode", "err", err)
		os.Exit(2)
	}
	idSrc, err := identity.Open(ctx, identity.SourceConfig{
		WorkloadAPIAddr: cfg.Identity.WorkloadAPI,
		BootTimeout:     cfg.Identity.BootTimeout,
		Mode:            idMode,
		TrustDomain:     cfg.TrustDomain,
	})
	if err != nil {
		logger.Error("identity open", "err", err)
		os.Exit(2)
	}
	defer idSrc.Close()
	logger.Info("identity ready", "td", idSrc.TrustDomain(), "mode", idSrc.Mode())

	// Transport: private listener (R-MTL-1).
	td := idSrc.TrustDomain()
	auth, err := identity.ParseAuthorizers(td, cfg.Transport.Private.Authorize)
	if err != nil {
		logger.Error("transport auth parse", "err", err)
		os.Exit(2)
	}
	privSvc := newPrivateService(idSrc, transport.PrivateConfig{
		Addr:      cfg.Transport.Private.Addr,
		Authorize: auth,
	}, logger)
	mgr.Register(privSvc)

	// Public listener (R-MTL-2) — only if configured.
	if cfg.Transport.Public.Addr != "" {
		pubSvc := newPublicService(transport.PublicConfig{
			Addr:     cfg.Transport.Public.Addr,
			CertPath: cfg.Transport.Public.CertPath,
			KeyPath:  cfg.Transport.Public.KeyPath,
		}, logger)
		mgr.Register(pubSvc)
	}

	// Secret broker client (R-SEC).
	sc := secrets.NewClient(cfg.Secrets.BrokerSocket)
	mgr.Register(&secretsService{client: sc, log: logger})

	// eBPF (R-EBP).
	bus := ebpf.NewMemoryBus()
	mgr.Register(&ebpfService{
		loader: ebpf.NewLoader(bus, cfg.EBPF.RingBufferSize),
		progs:  programsFromConfig(cfg),
		log:    logger,
	})
	_ = bus // bus is exposed if business logic wants to subscribe

	// Health endpoints (R-RUN-1).
	hc := health.New(cfg.Runtime.HealthAddr, healthAdapter{m: mgr})
	mgr.Register(hc)

	logger.Info("agent starting",
		"version", version.Version,
		"mode", cfg.Mode,
		"trustDomain", cfg.TrustDomain,
	)
	if err := mgr.Run(ctx); err != nil {
		logger.Error("run failed", "err", err)
		os.Exit(1)
	}
}

func programsFromConfig(cfg config.Agent) []ebpf.Program {
	out := make([]ebpf.Program, 0, len(cfg.EBPF.Programs))
	for _, name := range cfg.EBPF.Programs {
		out = append(out, ebpf.Program{Name: name}.Resolve(cfg.EBPF.ObjectsDir))
	}
	return out
}

// healthAdapter exposes a runtime.Manager as a health.Source.
type healthAdapter struct{ m *runtime.Manager }

func (h healthAdapter) Healthy() bool { return h.m.Healthy() }
func (h healthAdapter) Ready() bool   { return h.m.Ready() }

// privateService wraps a private mTLS listener as a runtime.Service.
type privateService struct {
	src  identity.Source
	cfg  transport.PrivateConfig
	log  *slog.Logger
	ln   net.Listener
	done chan struct{}
}

func newPrivateService(src identity.Source, cfg transport.PrivateConfig, log *slog.Logger) *privateService {
	return &privateService{src: src, cfg: cfg, log: log, done: make(chan struct{})}
}

func (s *privateService) Name() string { return "transport.private" }
func (s *privateService) Start(ctx context.Context) error {
	ln, err := transport.PrivateListener(ctx, s.src, s.cfg)
	if err != nil {
		return err
	}
	s.ln = ln
	go s.serveLoop()
	return nil
}
func (s *privateService) Drain(ctx context.Context) error {
	if s.ln != nil {
		_ = s.ln.Close()
	}
	return nil
}
func (s *privateService) Stop(ctx context.Context) error { return nil }
func (s *privateService) Ready() bool                    { return s.ln != nil }
func (s *privateService) serveLoop() {
	defer close(s.done)
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("private accept", "err", err)
			continue
		}
		go func(conn net.Conn) {
			defer conn.Close()
			// Default handler: just drop. Real apps wrap PrivateListener
			// with their own protocol (gRPC, HTTP/2). The listener is
			// exposed for embedding.
		}(c)
	}
}

// publicService is symmetrical to privateService for the public listener.
type publicService struct {
	cfg  transport.PublicConfig
	log  *slog.Logger
	ln   net.Listener
	done chan struct{}
}

func newPublicService(cfg transport.PublicConfig, log *slog.Logger) *publicService {
	return &publicService{cfg: cfg, log: log, done: make(chan struct{})}
}

func (s *publicService) Name() string { return "transport.public" }
func (s *publicService) Start(ctx context.Context) error {
	ln, err := transport.PublicListener(ctx, s.cfg)
	if err != nil {
		return err
	}
	s.ln = ln
	go s.serveLoop()
	return nil
}
func (s *publicService) Drain(ctx context.Context) error {
	if s.ln != nil {
		_ = s.ln.Close()
	}
	return nil
}
func (s *publicService) Stop(ctx context.Context) error { return nil }
func (s *publicService) Ready() bool                    { return s.ln != nil }
func (s *publicService) serveLoop() {
	defer close(s.done)
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) { _ = conn.Close() }(c)
	}
}

// secretsService probes the broker; reports Ready once it can dial.
type secretsService struct {
	client *secrets.Client
	log    *slog.Logger
	ready  bool
}

func (s *secretsService) Name() string { return "secrets.client" }
func (s *secretsService) Start(ctx context.Context) error {
	// Try to reach the broker; tolerate transient failure but log.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Probe with a request that should always be denied (empty name)
	// to avoid leaking real material in startup logs.
	_, err := s.client.Lease(ctx, "__probe__", time.Second)
	// We expect the broker to deny; what we want to know is "did we connect".
	if err != nil && errors.Is(err, secrets.ErrInvalidRequest) {
		s.ready = true
		return nil
	}
	if err != nil && (errors.Is(err, secrets.ErrUnauthorized) || errors.Is(err, secrets.ErrNotFound)) {
		s.ready = true
		return nil
	}
	if err != nil {
		s.log.Warn("secrets probe failed (continuing)", "err", err)
		s.ready = false
		return nil // non-fatal
	}
	s.ready = true
	return nil
}
func (s *secretsService) Drain(ctx context.Context) error { return nil }
func (s *secretsService) Stop(ctx context.Context) error  { return s.client.Close() }
func (s *secretsService) Ready() bool                     { return s.ready }

// ebpfService loads and detaches BPF programs.
type ebpfService struct {
	loader ebpf.Loader
	progs  []ebpf.Program
	log    *slog.Logger
	mgr    ebpf.Manager
}

func (s *ebpfService) Name() string { return "ebpf" }
func (s *ebpfService) Start(ctx context.Context) error {
	if len(s.progs) == 0 {
		return nil
	}
	mgr, err := s.loader.Load(ctx, s.progs...)
	if err != nil {
		if errors.Is(err, ebpf.ErrUnsupportedOS) {
			s.log.Warn("eBPF unsupported on this OS; running without observability")
			return nil
		}
		return err
	}
	s.mgr = mgr
	return nil
}
func (s *ebpfService) Drain(ctx context.Context) error { return nil }
func (s *ebpfService) Stop(ctx context.Context) error {
	if s.mgr != nil {
		return s.mgr.Detach()
	}
	return nil
}
func (s *ebpfService) Ready() bool { return true }

// _ helps avoid unused import for spiffeid in some edited builds.
var _ = spiffeid.ID{}
