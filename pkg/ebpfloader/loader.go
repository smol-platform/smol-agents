package ebpfloader

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/stigen/knative-agents/pkg/ebpf"
)

// Config drives the host loader.
type Config struct {
	// PinRoot is the directory under bpffs where programs and maps are
	// pinned (default /sys/fs/bpf/knative-agents).
	PinRoot string

	// Programs is the list of CO-RE BPF objects to load.
	Programs []ebpf.Program

	// MountBPFFS controls whether the loader will attempt to mount bpffs
	// at /sys/fs/bpf if it isn't already mounted. Set false in CI.
	MountBPFFS bool

	// EventForwardSocket is the optional UDS path where the loader will
	// publish ring-buffer events for unprivileged consumers. If empty,
	// agents read events directly from pinned maps.
	EventForwardSocket string

	// HealthAddr is the optional HTTP listen address for /healthz/readyz.
	HealthAddr string
}

// Result is what Run returns once the loader is steady-state.
type Result struct {
	Features       KernelFeatures
	LoadedPrograms []string
	PinnedMaps     []string // absolute paths under PinRoot
	PinnedPrograms []string
}

// ErrPlatformUnsupported is returned on non-Linux when Run is called.
var ErrPlatformUnsupported = errors.New("ebpfloader: only Linux is supported in production")

// Loader is the host-level loader.
type Loader struct {
	cfg Config
}

// New returns a Loader. It does no I/O; call Run to attach programs.
func New(cfg Config) *Loader {
	if cfg.PinRoot == "" {
		cfg.PinRoot = "/sys/fs/bpf/knative-agents"
	}
	return &Loader{cfg: cfg}
}

// PinPath returns the conventional pin path for a program by name.
func (l *Loader) PinPath(name string) string {
	return filepath.Join(l.cfg.PinRoot, name)
}

// Run is implemented per-platform in loader_linux.go / loader_other.go.

// retry runs fn up to attempts times with exponential backoff,
// returning the last error if none succeed. Used to tolerate transient
// races between the bpffs init container and the loader process.
func retry(ctx context.Context, attempts int, base time.Duration, fn func() error) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(base * (1 << i)):
		}
	}
	return fmt.Errorf("ebpfloader: gave up after %d attempts: %w", attempts, lastErr)
}
