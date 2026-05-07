package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Phase enumerates the agent's overall lifecycle state. Mirrors the Quint
// model in spec/quint/agent_lifecycle.qnt.
type Phase int32

const (
	PhaseBooting Phase = iota
	PhaseReady
	PhaseDraining
	PhaseStopped
)

func (p Phase) String() string {
	switch p {
	case PhaseBooting:
		return "Booting"
	case PhaseReady:
		return "Ready"
	case PhaseDraining:
		return "Draining"
	case PhaseStopped:
		return "Stopped"
	default:
		return "Unknown"
	}
}

// Service is anything the manager can drive through the lifecycle.
type Service interface {
	Name() string
	// Start brings the service online. Must return nil before Ready
	// is observed by the manager.
	Start(ctx context.Context) error
	// Drain gives the service a chance to finish in-flight work.
	Drain(ctx context.Context) error
	// Stop releases resources. Called after Drain (or directly if Drain
	// fails). MUST be idempotent.
	Stop(ctx context.Context) error
	// Ready reports whether the service is healthy and accepting work.
	Ready() bool
}

// Manager owns a set of Services and drives them through the lifecycle.
type Manager struct {
	Logger          *slog.Logger
	DrainTimeout    time.Duration
	ShutdownTimeout time.Duration

	mu       sync.Mutex
	services []Service
	phase    atomic.Int32
}

// NewManager returns a Manager with sensible defaults.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		Logger:          log,
		DrainTimeout:    30 * time.Second,
		ShutdownTimeout: 5 * time.Second,
	}
}

// Register adds a service. Must be called before Run.
func (m *Manager) Register(s Service) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.services = append(m.services, s)
}

// Phase reports the current lifecycle phase.
func (m *Manager) Phase() Phase {
	return Phase(m.phase.Load())
}

// Ready returns true iff the manager is in Ready and every service
// reports Ready. Implements R-RUN-1 acceptance #1.
func (m *Manager) Ready() bool {
	if m.Phase() != PhaseReady {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.services {
		if !s.Ready() {
			return false
		}
	}
	return true
}

// Healthy returns true unless any service is in failure mode while we
// are supposed to be Ready. Implements R-RUN-1 acceptance #2.
func (m *Manager) Healthy() bool {
	switch m.Phase() {
	case PhaseBooting, PhaseDraining:
		return true
	case PhaseStopped:
		return false
	}
	return m.Ready()
}

// Run drives every Service through Start; once all return without error
// and report Ready, transitions to PhaseReady. Run blocks until ctx is
// cancelled, then drains and stops in reverse order.
func (m *Manager) Run(ctx context.Context) error {
	m.phase.Store(int32(PhaseBooting))
	for _, s := range m.servicesSnapshot() {
		startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.Start(startCtx)
		cancel()
		if err != nil {
			m.Logger.Error("service start failed", "name", s.Name(), "err", err)
			m.shutdownAfterError(ctx)
			return fmt.Errorf("runtime: start %s: %w", s.Name(), err)
		}
	}
	// Wait for all services to become Ready (R-RUN-1 #1).
	if err := m.waitReady(ctx, 30*time.Second); err != nil {
		m.shutdownAfterError(ctx)
		return err
	}
	m.phase.Store(int32(PhaseReady))
	m.Logger.Info("agent ready", "services", m.serviceNames())

	<-ctx.Done()

	return m.drainThenStop()
}

func (m *Manager) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		all := true
		for _, s := range m.servicesSnapshot() {
			if !s.Ready() {
				all = false
				break
			}
		}
		if all {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("runtime: timed out waiting for services to be Ready")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// drainThenStop runs Drain in registration order, then Stop in reverse.
func (m *Manager) drainThenStop() error {
	m.phase.Store(int32(PhaseDraining))
	m.Logger.Info("draining", "timeout", m.DrainTimeout)
	drainCtx, cancel := context.WithTimeout(context.Background(), m.DrainTimeout)
	defer cancel()
	var firstErr error
	for _, s := range m.servicesSnapshot() {
		if err := s.Drain(drainCtx); err != nil {
			m.Logger.Warn("drain failed", "name", s.Name(), "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	stopCtx, cancel2 := context.WithTimeout(context.Background(), m.ShutdownTimeout)
	defer cancel2()
	services := m.servicesSnapshot()
	for i := len(services) - 1; i >= 0; i-- {
		s := services[i]
		if err := s.Stop(stopCtx); err != nil {
			m.Logger.Warn("stop failed", "name", s.Name(), "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	m.phase.Store(int32(PhaseStopped))
	m.Logger.Info("stopped")
	return firstErr
}

func (m *Manager) shutdownAfterError(_ context.Context) {
	// Use background contexts because the parent may already be cancelled.
	m.drainThenStop()
}

func (m *Manager) servicesSnapshot() []Service {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Service, len(m.services))
	copy(out, m.services)
	return out
}

func (m *Manager) serviceNames() []string {
	out := make([]string, 0, len(m.services))
	for _, s := range m.servicesSnapshot() {
		out = append(out, s.Name())
	}
	return out
}
