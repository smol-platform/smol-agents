package main

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/sessionqueue"
)

// spyQueue records the last UpdateRetention call.
type spyQueue struct {
	*sessionqueue.MemQueue
	mu   sync.Mutex
	last time.Duration
}

func (q *spyQueue) UpdateRetention(d time.Duration) error {
	q.mu.Lock()
	q.last = d
	q.mu.Unlock()
	return nil
}
func (q *spyQueue) lastRetention() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.last
}

func limitsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return sch
}

func session(ns, name string, maxBytes, retentionSec int32) *amv1.AgentSession {
	s := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	s.Spec.AgentRef = "a"
	s.Spec.MaxTurnInputBytes = maxBytes
	s.Spec.TurnRetentionSeconds = retentionSec
	return s
}

func TestInputCap_NoClientDefault(t *testing.T) {
	if got := (*sessionLimits)(nil).inputCap(context.Background(), "t", "s"); got != defaultInputCap {
		t.Errorf("nil limits cap = %d, want default %d", got, defaultInputCap)
	}
	if got := newSessionLimits(nil, nil).inputCap(context.Background(), "t", "s"); got != defaultInputCap {
		t.Errorf("no-client cap = %d, want default %d", got, defaultInputCap)
	}
}

func TestInputCap_PerSessionAndCeiling(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(limitsScheme(t)).WithObjects(
		session("t", "small", 4096, 0),
		session("t", "huge", 50<<20, 0), // 50 MiB → clamped to 10 MiB
	).Build()
	q := &spyQueue{MemQueue: sessionqueue.NewMemQueue()}
	l := newSessionLimits(c, q)

	if got := l.inputCap(context.Background(), "t", "small"); got != 4096 {
		t.Errorf("small cap = %d, want 4096", got)
	}
	if got := l.inputCap(context.Background(), "t", "huge"); got != hardInputCeiling {
		t.Errorf("huge cap = %d, want ceiling %d", got, hardInputCeiling)
	}
	// Unknown session → default.
	if got := l.inputCap(context.Background(), "t", "ghost"); got != defaultInputCap {
		t.Errorf("unknown session cap = %d, want default", got)
	}
}

func TestInputCap_CachedWithinTTL(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(limitsScheme(t)).WithObjects(session("t", "s", 4096, 0)).Build()
	l := newSessionLimits(c, &spyQueue{MemQueue: sessionqueue.NewMemQueue()})
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }

	_ = l.inputCap(context.Background(), "t", "s")
	// Delete the session; within the TTL the cached value still serves.
	_ = c.Delete(context.Background(), session("t", "s", 0, 0))
	if got := l.inputCap(context.Background(), "t", "s"); got != 4096 {
		t.Errorf("within TTL cap = %d, want cached 4096", got)
	}
	// After the TTL, the (now-absent) session falls back to default.
	now = now.Add(31 * time.Second)
	if got := l.inputCap(context.Background(), "t", "s"); got != defaultInputCap {
		t.Errorf("post-TTL cap = %d, want default after the session vanished", got)
	}
}

func TestInputCap_ReconcilesRetention(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(limitsScheme(t)).WithObjects(session("t", "s", 0, 7200)).Build()
	q := &spyQueue{MemQueue: sessionqueue.NewMemQueue()}
	l := newSessionLimits(c, q)

	_ = l.inputCap(context.Background(), "t", "s")
	if got := q.lastRetention(); got != 2*time.Hour {
		t.Errorf("reconciled retention = %v, want 2h (max turnRetentionSeconds)", got)
	}
}
