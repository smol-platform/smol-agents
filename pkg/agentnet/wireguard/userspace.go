package wireguard

import (
	"context"
	"errors"
	"sync"
)

// UserspaceDevice is the production Adapter — a thin wrapper that
// delegates to wireguard-go + tun/netstack at run time. The build is
// guarded behind a tag so unit tests don't pull the dep.
//
// In v1 we ship the type + a placeholder implementation so the
// operator can register the adapter at startup; the actual netstack
// device is wired in pkg/agentnet/wireguard/netstack.go (separate
// build tag) so that file can import golang.zx2c4.com/wireguard
// without dragging the dependency into the lightweight test path.
//
// The unit tests in this package use FakeAdapter exclusively.
type UserspaceDevice struct {
	mu      sync.Mutex
	cfg     Config
	running bool
	state   map[string]PeerState

	// Hooks for the real implementation to plug in. Production wiring
	// sets these via wireguard.WithNetstack(); tests leave them nil.
	startFn func(ctx context.Context, cfg Config) (statefulDevice, error)
	current statefulDevice
}

// statefulDevice is the minimal surface a real wireguard-go device
// must offer. The real implementation in netstack.go satisfies it;
// tests don't need it.
type statefulDevice interface {
	IpcSet(uapi string) error
	Close()
	PeerStates() []PeerState
}

// NewUserspaceDevice returns an Adapter that, when Start is called,
// will boot a real userspace WireGuard device IF a startFn was
// installed (production wiring). Otherwise returns ErrNotWired so the
// operator can fall back to FakeAdapter in test environments.
//
// When the package is built with -tags=wgnetstack, defaultStartFn is
// populated by netstack.go's init() and installed automatically.
func NewUserspaceDevice() *UserspaceDevice {
	d := &UserspaceDevice{state: map[string]PeerState{}}
	if defaultStartFn != nil {
		d.startFn = defaultStartFn
	}
	return d
}

// defaultStartFn is populated by netstack.go's init() under the
// wgnetstack build tag; nil otherwise.
var defaultStartFn func(ctx context.Context, cfg Config) (statefulDevice, error)

// ErrNotWired indicates the netstack-backed implementation isn't
// linked in this build. Production builds use the netstack tag.
var ErrNotWired = errors.New("agentnet/wireguard: netstack-backed device not wired (build with -tags=wgnetstack)")

func (u *UserspaceDevice) Start(ctx context.Context, cfg Config) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.running {
		return errors.New("agentnet/wireguard: already running")
	}
	if u.startFn == nil {
		return ErrNotWired
	}
	dev, err := u.startFn(ctx, cfg)
	if err != nil {
		return err
	}
	if err := dev.IpcSet(cfg.RenderUAPI()); err != nil {
		dev.Close()
		return err
	}
	u.current = dev
	u.cfg = cfg
	u.running = true
	return nil
}

func (u *UserspaceDevice) Stop() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.running {
		return nil
	}
	u.current.Close()
	u.current = nil
	u.running = false
	return nil
}

func (u *UserspaceDevice) Peers() []PeerState {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.current == nil {
		return nil
	}
	return u.current.PeerStates()
}

// withStartFn is a private hook used by netstack.go (under the
// `wgnetstack` build tag) to install the real implementation without
// the test path needing to know.
func (u *UserspaceDevice) withStartFn(fn func(ctx context.Context, cfg Config) (statefulDevice, error)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.startFn = fn
}

// WithStartFn is the exported pointer that netstack.go (or a
// production main) calls during init to swap in the real device.
func (u *UserspaceDevice) WithStartFn(fn func(ctx context.Context, cfg Config) (statefulDevice, error)) {
	u.withStartFn(fn)
}
