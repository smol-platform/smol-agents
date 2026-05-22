// Package quota enforces per-identity, per-retriever resource limits.
//
// Implements R-MEM-QUOTA-1:
//   - topK is clamped to QuotaSpec.MaxTopK; never silently truncated.
//   - Request rate (RequestsPerMinute) is tracked per caller SPIFFE ID.
//   - Write payload size (MaxWriteBytes) is checked before forwarding.
//
// Any limit breach returns a typed memory.QuotaExceeded error that the caller
// must audit and surface. Silent truncation of topK is explicitly prohibited.
package quota

import (
	"fmt"
	"sync"
	"time"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/memory"
)

// Enforcer enforces per-retriever quotas. It is safe for concurrent use.
// One Enforcer is typically shared across all requests to the same gateway.
type Enforcer struct {
	mu       sync.Mutex
	windows  map[string]*rateWindow // keyed by callerSPIFFEID
	windowSz time.Duration          // sliding window size; default 1 minute
}

// NewEnforcer returns a ready Enforcer.
func NewEnforcer() *Enforcer {
	return &Enforcer{
		windows:  make(map[string]*rateWindow),
		windowSz: time.Minute,
	}
}

// rateWindow tracks the call timestamps for one caller within the current
// sliding window.
type rateWindow struct {
	calls []time.Time
}

// ClampTopK returns the effective topK to use for a retrieve call.
// If quota.MaxTopK > 0 and requestedTopK > quota.MaxTopK, it returns an error
// (never silently truncates). R-MEM-QUOTA-1.
func ClampTopK(requested int32, quota v1.QuotaSpec) (int32, error) {
	if quota.MaxTopK <= 0 {
		// No ceiling configured; use the requested value as-is.
		return requested, nil
	}
	if requested <= quota.MaxTopK {
		return requested, nil
	}
	return 0, memory.QuotaExceeded(fmt.Sprintf(
		"topK %d exceeds quota ceiling %d", requested, quota.MaxTopK,
	))
}

// CheckWriteSize returns QuotaExceeded if the write payload exceeds the
// configured limit. A limit of 0 means unlimited. R-MEM-QUOTA-1.
func CheckWriteSize(payloadBytes int64, quota v1.QuotaSpec) error {
	if quota.MaxWriteBytes <= 0 {
		return nil
	}
	if payloadBytes <= quota.MaxWriteBytes {
		return nil
	}
	return memory.QuotaExceeded(fmt.Sprintf(
		"write payload %d bytes exceeds quota limit %d", payloadBytes, quota.MaxWriteBytes,
	))
}

// CheckRate records a call for callerSPIFFEID and returns QuotaExceeded if
// the caller has exceeded their per-minute request rate. A limit of 0 means
// unlimited. R-MEM-QUOTA-1.
//
// Uses a sliding window; old entries older than the window are evicted.
func (e *Enforcer) CheckRate(callerSPIFFEID string, quota v1.QuotaSpec) error {
	if quota.RequestsPerMinute <= 0 {
		return nil
	}

	now := time.Now()
	cutoff := now.Add(-e.windowSz)

	e.mu.Lock()
	defer e.mu.Unlock()

	w := e.windows[callerSPIFFEID]
	if w == nil {
		w = &rateWindow{}
		e.windows[callerSPIFFEID] = w
	}

	// Evict entries outside the window.
	live := w.calls[:0]
	for _, t := range w.calls {
		if t.After(cutoff) {
			live = append(live, t)
		}
	}
	w.calls = live

	if int32(len(w.calls)) >= quota.RequestsPerMinute {
		return memory.QuotaExceeded(fmt.Sprintf(
			"rate limit %d req/min exceeded for caller %s",
			quota.RequestsPerMinute, callerSPIFFEID,
		))
	}

	w.calls = append(w.calls, now)
	return nil
}
