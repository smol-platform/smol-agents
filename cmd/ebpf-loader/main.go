// Command ebpf-loader is the privileged DaemonSet binary that loads
// CO-RE eBPF programs on every Kubernetes node, pins their maps to
// bpffs, and exposes /healthz + /readyz over HTTP.
//
// On graceful shutdown the loader leaves pinned objects in place so the
// next Pod replica can re-adopt them without dropping events. Because
// rolling DaemonSet upgrades reach every node serially, this is the
// difference between observability-on-disk and observability-with-gaps.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/stigen/knative-agents/internal/version"
	"github.com/stigen/knative-agents/pkg/ebpf"
	"github.com/stigen/knative-agents/pkg/ebpfloader"
	"github.com/stigen/knative-agents/pkg/observability"
)

type loaderConfig struct {
	PinRoot    string   `yaml:"pinRoot"`
	ObjectsDir string   `yaml:"objectsDir"`
	Programs   []string `yaml:"programs"`
	MountBPFFS bool     `yaml:"mountBPFFS"`
	HealthAddr string   `yaml:"healthAddr"`
}

func main() {
	configPath := flag.String("config", "/etc/ebpf-loader/config.yaml", "loader config")
	logLevel := flag.String("log-level", "info", "debug|info|warn|error")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(*logLevel))
	logger := observability.MustLogger(level)
	slog.SetDefault(logger)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(2)
	}

	progs := make([]ebpf.Program, 0, len(cfg.Programs))
	for _, name := range cfg.Programs {
		progs = append(progs, ebpf.Program{Name: name}.Resolve(cfg.ObjectsDir))
	}

	loader := ebpfloader.New(ebpfloader.Config{
		PinRoot:    cfg.PinRoot,
		Programs:   progs,
		MountBPFFS: cfg.MountBPFFS,
		HealthAddr: cfg.HealthAddr,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var ready atomic.Bool
	healthSrv := startHealth(cfg.HealthAddr, &ready, logger)
	defer healthSrv.shutdown()

	logger.Info("ebpf-loader starting",
		"version", version.Version,
		"node", os.Getenv("NODE_NAME"),
		"pinRoot", cfg.PinRoot,
		"programs", cfg.Programs,
	)

	res, err := loader.Run(ctx)
	if err != nil {
		if errors.Is(err, ebpfloader.ErrPlatformUnsupported) {
			logger.Warn("non-Linux host detected; loader is a no-op", "err", err)
			ready.Store(true)
			<-ctx.Done()
			return
		}
		logger.Error("loader.Run", "err", err)
		os.Exit(1)
	}
	logger.Info("loaded",
		"features", res.Features.String(),
		"programs", res.LoadedPrograms,
		"pinnedMaps", res.PinnedMaps,
	)
	ready.Store(true)

	<-ctx.Done()
	logger.Info("shutdown requested; pinned objects retained for hand-off")
}

func loadConfig(path string) (loaderConfig, error) {
	cfg := loaderConfig{
		PinRoot:    "/sys/fs/bpf/knative-agents",
		ObjectsDir: "/usr/share/knative-agents/bpf",
		Programs:   []string{"syscalls", "network"},
		MountBPFFS: true,
		HealthAddr: "0.0.0.0:8081",
	}
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Config is optional; defaults are sensible.
			return cfg, nil
		}
		return cfg, err
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	if cfg.PinRoot == "" {
		cfg.PinRoot = "/sys/fs/bpf/knative-agents"
	}
	if cfg.ObjectsDir == "" {
		cfg.ObjectsDir = "/usr/share/knative-agents/bpf"
	}
	if cfg.HealthAddr == "" {
		cfg.HealthAddr = "0.0.0.0:8081"
	}
	if !filepath.IsAbs(cfg.PinRoot) {
		return cfg, fmt.Errorf("pinRoot must be absolute: %q", cfg.PinRoot)
	}
	return cfg, nil
}

type healthServer struct {
	srv *http.Server
}

func (h *healthServer) shutdown() {
	if h.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = h.srv.Shutdown(ctx)
}

func startHealth(addr string, ready *atomic.Bool, log *slog.Logger) *healthServer {
	if addr == "" {
		return &healthServer{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("health server", "err", err)
		}
	}()
	return &healthServer{srv: srv}
}
