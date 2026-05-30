package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// maxResponseBytes caps how much of a response body we read (matches the prior
// per-call limit). A larger body is truncated.
const maxResponseBytes = 16 * 1024 * 1024

// httpResponse is the transport-level result doWithRetry returns on success.
type httpResponse struct {
	Status     int
	Body       []byte
	DurationMs int64
}

// doWithRetry executes newReq() and returns its response, retrying transient
// failures — network errors, HTTP 429, and 5xx — up to retry.Attempts() times
// with capped exponential backoff that honors a server Retry-After. It always
// respects ctx: a cancelled/expired ctx stops the loop immediately (returning
// ErrCancelled / ErrTimeout). 4xx client errors are never retried.
//
// newReq MUST build a FRESH *http.Request each call: a retried request needs an
// undrained body, and any per-request state the caller wants stable across
// attempts (e.g. a minted session id) must be captured OUTSIDE newReq.
//
// On a non-2xx final outcome (or exhausted retries) the returned error is
// classified: its message begins with a stable reason token (auth, bad_request,
// rate_limited, overloaded, network) so the folded TerminationReason reads e.g.
// "harness:rate_limited: http 429: ...".
func doWithRetry(ctx context.Context, client HTTPClient, newReq func() (*http.Request, error), retry *v1.RetrySpec) (httpResponse, error) {
	attempts := retry.Attempts()
	base, maxBackoff := retry.BackoffBase(), retry.MaxBackoff()

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if ce := ctxError(ctx); ce != nil {
			return httpResponse{}, ce
		}

		req, err := newReq()
		if err != nil {
			return httpResponse{}, fmt.Errorf("request: %w", err)
		}

		startedAt := time.Now()
		resp, err := client.Do(req)
		dur := time.Since(startedAt).Milliseconds()

		if err != nil {
			if ce := ctxError(ctx); ce != nil {
				return httpResponse{DurationMs: dur}, ce
			}
			lastErr = fmt.Errorf("network: %w", err)
			if attempt < attempts && backoffSleep(ctx, attempt, base, maxBackoff, 0) {
				continue
			}
			return httpResponse{DurationMs: dur}, lastErr
		}

		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		resp.Body.Close()

		if rerr != nil {
			lastErr = fmt.Errorf("network: read response: %w", rerr)
			if attempt < attempts && backoffSleep(ctx, attempt, base, maxBackoff, 0) {
				continue
			}
			return httpResponse{DurationMs: dur}, lastErr
		}

		if resp.StatusCode < 400 {
			return httpResponse{Status: resp.StatusCode, Body: body, DurationMs: dur}, nil
		}

		reason, retryable := classifyHTTP(resp.StatusCode)
		lastErr = fmt.Errorf("%s: http %d: %s", reason, resp.StatusCode, snippet(body))
		if retryable && attempt < attempts && backoffSleep(ctx, attempt, base, maxBackoff, retryAfter) {
			continue
		}
		return httpResponse{Status: resp.StatusCode, DurationMs: dur}, lastErr
	}
	return httpResponse{}, lastErr
}

// ctxError maps a finished ctx to the harness sentinel: ErrTimeout on a deadline,
// ErrCancelled on an explicit cancel. Returns nil while the ctx is still live.
func ctxError(ctx context.Context) error {
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return ErrTimeout
	case context.Canceled:
		return ErrCancelled
	default:
		return nil
	}
}

// classifyHTTP maps an HTTP status to a stable reason token and whether the
// request is worth retrying. Retryable: 429 + 5xx (transient overload/outage).
// Not retryable: other 4xx (auth, malformed request) — a retry won't help.
func classifyHTTP(status int) (reason string, retryable bool) {
	switch {
	case status == 401 || status == 403:
		return "auth", false
	case status == 429:
		return "rate_limited", true
	case status >= 500:
		return "overloaded", true
	case status >= 400:
		return "bad_request", false
	default:
		return "http", false
	}
}

// backoffSleep waits before the next attempt: max(exponential, Retry-After),
// capped at maxWait. Returns false if ctx ends during the wait (caller stops).
func backoffSleep(ctx context.Context, attempt int, base, maxWait, retryAfter time.Duration) bool {
	d := base << (attempt - 1) // base, 2*base, 4*base, ...
	if d <= 0 || d > maxWait {
		d = maxWait
	}
	if retryAfter > d {
		d = retryAfter
	}
	if d > maxWait {
		d = maxWait
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// parseRetryAfter reads a Retry-After header (delta-seconds or HTTP-date) as a
// duration. Returns 0 when absent or unparseable.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// snippet returns at most 512 bytes of body for an error message, so a large
// error page can't bloat the run's TerminationReason (which the termination-
// message size clamp does not elide).
func snippet(b []byte) string {
	const n = 512
	b = bytes.TrimSpace(b)
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
