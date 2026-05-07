package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSvc struct {
	name      string
	startErr  error
	startedAt int64
	ready     atomic.Bool
	drainCnt  atomic.Int32
	stopCnt   atomic.Int32
}

func (f *fakeSvc) Name() string { return f.name }
func (f *fakeSvc) Start(ctx context.Context) error {
	if f.startErr != nil {
		return f.startErr
	}
	atomic.StoreInt64(&f.startedAt, time.Now().UnixNano())
	f.ready.Store(true)
	return nil
}
func (f *fakeSvc) Drain(ctx context.Context) error { f.drainCnt.Add(1); return nil }
func (f *fakeSvc) Stop(ctx context.Context) error  { f.stopCnt.Add(1); return nil }
func (f *fakeSvc) Ready() bool                     { return f.ready.Load() }

func TestManager_HappyPath(t *testing.T) {
	m := NewManager(nil)
	m.DrainTimeout = time.Second
	m.ShutdownTimeout = time.Second
	a := &fakeSvc{name: "a"}
	b := &fakeSvc{name: "b"}
	m.Register(a)
	m.Register(b)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && m.Phase() != PhaseReady {
		time.Sleep(10 * time.Millisecond)
	}
	if m.Phase() != PhaseReady {
		t.Fatalf("never reached Ready; phase=%s", m.Phase())
	}
	if !m.Ready() {
		t.Error("Ready() = false after PhaseReady")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
	if a.drainCnt.Load() != 1 || b.drainCnt.Load() != 1 {
		t.Errorf("expected drain on both: a=%d b=%d", a.drainCnt.Load(), b.drainCnt.Load())
	}
	if a.stopCnt.Load() != 1 || b.stopCnt.Load() != 1 {
		t.Errorf("expected stop on both: a=%d b=%d", a.stopCnt.Load(), b.stopCnt.Load())
	}
	if m.Phase() != PhaseStopped {
		t.Errorf("phase=%s, want Stopped", m.Phase())
	}
}

func TestManager_StartError(t *testing.T) {
	m := NewManager(nil)
	m.DrainTimeout = time.Second
	m.ShutdownTimeout = time.Second
	a := &fakeSvc{name: "a", startErr: errors.New("boom")}
	m.Register(a)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := m.Run(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestManager_HealthyAcrossPhases(t *testing.T) {
	m := NewManager(nil)
	if !m.Healthy() {
		t.Error("booting should be healthy")
	}
	m.phase.Store(int32(PhaseStopped))
	if m.Healthy() {
		t.Error("stopped should not be healthy")
	}
}

func TestPhaseString(t *testing.T) {
	cases := map[Phase]string{
		PhaseBooting:  "Booting",
		PhaseReady:    "Ready",
		PhaseDraining: "Draining",
		PhaseStopped:  "Stopped",
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Errorf("Phase(%d).String() = %q, want %q", p, got, want)
		}
	}
}
