package main

import (
	"context"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/sessionqueue"
)

const (
	// defaultInputCap is the per-turn body cap used when no AgentSession lookup is
	// available (no in-cluster client, or the session is unreadable).
	defaultInputCap = 1 << 20 // 1 MiB
	// hardInputCeiling bounds any per-session cap — a tenant may lower the limit
	// but never raise it past this (defense).
	hardInputCeiling = 10 << 20 // 10 MiB
)

// sessionLimits resolves a session's per-turn input cap from its AgentSession
// spec, with a short TTL cache so a hot session doesn't hit the apiserver on
// every turn (M2.20). As a side effect it reconciles the durable turn-stream
// retention to the max turnRetentionSeconds it has observed (cluster-wide max —
// coarse, ok for v1). When Client is nil (dev / no in-cluster access) every
// session falls back to defaultInputCap and retention is left at its default.
type sessionLimits struct {
	Client client.Client
	Queue  sessionqueue.Queue
	TTL    time.Duration

	mu           sync.Mutex
	cache        map[string]limitEntry
	maxRetention time.Duration
	now          func() time.Time
}

type limitEntry struct {
	cap     int64
	expires time.Time
}

func newSessionLimits(c client.Client, q sessionqueue.Queue) *sessionLimits {
	return &sessionLimits{
		Client: c, Queue: q, TTL: 30 * time.Second,
		cache: map[string]limitEntry{}, now: time.Now,
	}
}

// inputCap returns the per-turn input byte cap for a session: min(spec
// maxTurnInputBytes, 10 MiB), defaultInputCap when the session can't be read.
// Cached per TTL; a cache-miss read also reconciles stream retention.
func (s *sessionLimits) inputCap(ctx context.Context, ns, name string) int64 {
	if s == nil || s.Client == nil {
		return defaultInputCap
	}
	key := ns + "/" + name
	s.mu.Lock()
	if e, ok := s.cache[key]; ok && s.now().Before(e.expires) {
		s.mu.Unlock()
		return e.cap
	}
	s.mu.Unlock()

	capBytes := int64(defaultInputCap)
	var retention time.Duration
	var sess amv1.AgentSession
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err == nil {
		capBytes = int64(sess.Spec.InputBytesCap())
		if capBytes > hardInputCeiling {
			capBytes = hardInputCeiling
		}
		retention = time.Duration(sess.Spec.RetentionSeconds()) * time.Second
	}

	s.mu.Lock()
	s.cache[key] = limitEntry{cap: capBytes, expires: s.now().Add(s.TTL)}
	reconcile := retention > s.maxRetention
	if reconcile {
		s.maxRetention = retention
	}
	maxRet := s.maxRetention
	s.mu.Unlock()

	if reconcile && s.Queue != nil {
		_ = s.Queue.UpdateRetention(maxRet) // best-effort; never blocks a turn
	}
	return capBytes
}
