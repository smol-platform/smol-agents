package team

import "context"

// WidthLimiter caps how many team members run concurrently — the width analog of
// the A2A MaxDepth recursion guard (orchestrator fan-out). A coordinator
// Acquires a slot before spawning a member and Releases it when the member
// finishes. A non-positive cap means unlimited.
type WidthLimiter struct {
	ch chan struct{}
}

// NewWidthLimiter caps concurrency at max (≤ 0 → unlimited).
func NewWidthLimiter(max int) *WidthLimiter {
	if max <= 0 {
		return &WidthLimiter{}
	}
	return &WidthLimiter{ch: make(chan struct{}, max)}
}

// Acquire takes a slot, blocking until one frees or ctx is cancelled. Unlimited
// limiters return immediately.
func (w *WidthLimiter) Acquire(ctx context.Context) error {
	if w.ch == nil {
		return nil
	}
	select {
	case w.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release frees a previously acquired slot (no-op if none held / unlimited).
func (w *WidthLimiter) Release() {
	if w.ch == nil {
		return
	}
	select {
	case <-w.ch:
	default:
	}
}

// InUse reports the number of slots currently held (observability/tests).
func (w *WidthLimiter) InUse() int {
	if w.ch == nil {
		return 0
	}
	return len(w.ch)
}
