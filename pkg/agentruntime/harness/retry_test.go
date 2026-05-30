package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// seqStep is one canned outcome for seqClient.
type seqStep struct {
	status int
	body   string
	header http.Header
	err    error
}

// seqClient returns canned outcomes in order (repeating the last when called
// beyond the list) and records each request's body, so tests can prove the
// factory rebuilds an undrained body on every retry.
type seqClient struct {
	steps     []seqStep
	calls     int
	gotBodies []string
}

func (c *seqClient) Do(req *http.Request) (*http.Response, error) {
	i := c.calls
	c.calls++
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		c.gotBodies = append(c.gotBodies, string(b))
	} else {
		c.gotBodies = append(c.gotBodies, "")
	}
	if i >= len(c.steps) {
		i = len(c.steps) - 1
	}
	s := c.steps[i]
	if s.err != nil {
		return nil, s.err
	}
	h := s.header
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: s.status, Body: io.NopCloser(strings.NewReader(s.body)), Header: h}, nil
}

func reqFactory(body string) func() (*http.Request, error) {
	return func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, "http://example.invalid", strings.NewReader(body))
	}
}

// fastRetry keeps backoff sub-millisecond so retry tests don't actually sleep.
func fastRetry(maxAttempts int32) *v1.RetrySpec {
	return &v1.RetrySpec{MaxAttempts: maxAttempts, BackoffBaseMs: 1, MaxBackoffMs: 2}
}

func TestClassifyHTTP(t *testing.T) {
	cases := []struct {
		status    int
		reason    string
		retryable bool
	}{
		{401, "auth", false},
		{403, "auth", false},
		{400, "bad_request", false},
		{422, "bad_request", false},
		{429, "rate_limited", true},
		{500, "overloaded", true},
		{503, "overloaded", true},
	}
	for _, c := range cases {
		r, retryable := classifyHTTP(c.status)
		if r != c.reason || retryable != c.retryable {
			t.Errorf("classifyHTTP(%d) = (%q,%v), want (%q,%v)", c.status, r, retryable, c.reason, c.retryable)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("2"); d != 2*time.Second {
		t.Errorf("seconds: got %v", d)
	}
	for _, v := range []string{"", "garbage", "0", "-5"} {
		if d := parseRetryAfter(v); d != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", v, d)
		}
	}
}

func TestDoWithRetry_SingleAttemptByDefault(t *testing.T) {
	c := &seqClient{steps: []seqStep{{status: 500, body: "boom"}}}
	_, err := doWithRetry(context.Background(), c, reqFactory(`{"a":1}`), nil) // nil => 1 attempt
	if err == nil || !strings.HasPrefix(err.Error(), "overloaded:") {
		t.Fatalf("err = %v, want overloaded:", err)
	}
	if c.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry by default)", c.calls)
	}
}

func TestDoWithRetry_RetriesThenSucceeds(t *testing.T) {
	c := &seqClient{steps: []seqStep{
		{status: 429, body: "slow down"},
		{status: 503, body: "down"},
		{status: 200, body: `{"ok":true}`},
	}}
	res, err := doWithRetry(context.Background(), c, reqFactory(`{"a":1}`), fastRetry(3))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Status != 200 || string(res.Body) != `{"ok":true}` {
		t.Errorf("res = %+v", res)
	}
	if c.calls != 3 {
		t.Errorf("calls = %d, want 3", c.calls)
	}
	// The factory must rebuild the (undrained) body on every attempt.
	for i, b := range c.gotBodies {
		if b != `{"a":1}` {
			t.Errorf("attempt %d body = %q, want full body (factory must rebuild)", i, b)
		}
	}
}

func TestDoWithRetry_StopsAtMaxAttempts(t *testing.T) {
	c := &seqClient{steps: []seqStep{{status: 500, body: "boom"}}} // always 500
	_, err := doWithRetry(context.Background(), c, reqFactory(`{}`), fastRetry(3))
	if err == nil || !strings.HasPrefix(err.Error(), "overloaded:") {
		t.Fatalf("err = %v", err)
	}
	if c.calls != 3 {
		t.Errorf("calls = %d, want 3 (capped)", c.calls)
	}
}

func TestDoWithRetry_NonRetryableNoRetry(t *testing.T) {
	c := &seqClient{steps: []seqStep{{status: 400, body: "bad"}}}
	_, err := doWithRetry(context.Background(), c, reqFactory(`{}`), fastRetry(5))
	if err == nil || !strings.HasPrefix(err.Error(), "bad_request:") {
		t.Fatalf("err = %v", err)
	}
	if c.calls != 1 {
		t.Errorf("calls = %d, want 1 (4xx is not retried)", c.calls)
	}
}

func TestDoWithRetry_NetworkErrorRetried(t *testing.T) {
	c := &seqClient{steps: []seqStep{
		{err: errors.New("connection refused")},
		{status: 200, body: "ok"},
	}}
	res, err := doWithRetry(context.Background(), c, reqFactory(`{}`), fastRetry(2))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(res.Body) != "ok" {
		t.Errorf("body = %q", res.Body)
	}
	if c.calls != 2 {
		t.Errorf("calls = %d, want 2", c.calls)
	}
}

func TestDoWithRetry_CtxCancelStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &seqClient{steps: []seqStep{{status: 200, body: "ok"}}}
	_, err := doWithRetry(ctx, c, reqFactory(`{}`), fastRetry(3))
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	if c.calls != 0 {
		t.Errorf("calls = %d, want 0 (ctx cancelled before any attempt)", c.calls)
	}
}

// End-to-end through HermesHarness: a transient 503 is retried and the run
// recovers, proving spec.http.retry is wired into the harness Run path.
func TestHermesHarness_RetriesTransient(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte("overloaded"))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"recovered"}}]}`))
	}))
	defer srv.Close()

	h := &HermesHarness{Client: srv.Client()}
	resp, err := h.Run(context.Background(), Request{
		Spec: v1.HarnessSpec{
			Kind: v1.HarnessHermes,
			HTTP: &v1.HarnessHTTPSpec{URL: srv.URL, Retry: &v1.RetrySpec{MaxAttempts: 3, BackoffBaseMs: 1, MaxBackoffMs: 2}},
		},
		Input:  json.RawMessage(`{"prompt":"hi"}`),
		Budget: v1.Budget{MaxWallClockSeconds: 5},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(resp.Output) != "recovered" {
		t.Errorf("output = %q, want recovered (after retry)", resp.Output)
	}
	if calls != 2 {
		t.Errorf("server calls = %d, want 2 (503 then success)", calls)
	}
}

// Without a retry spec, a transient 503 is fatal on the first attempt — the
// classified reason surfaces so the run's TerminationReason is actionable.
func TestHermesHarness_NoRetryByDefault(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(503)
		_, _ = w.Write([]byte("down"))
	}))
	defer srv.Close()

	h := &HermesHarness{Client: srv.Client()}
	_, err := h.Run(context.Background(), Request{
		Spec:   v1.HarnessSpec{Kind: v1.HarnessHermes, HTTP: &v1.HarnessHTTPSpec{URL: srv.URL}},
		Input:  json.RawMessage(`{"prompt":"hi"}`),
		Budget: v1.Budget{MaxWallClockSeconds: 5},
	})
	if err == nil || !strings.HasPrefix(err.Error(), "overloaded:") {
		t.Fatalf("err = %v, want overloaded: classification", err)
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want 1 (no retry by default)", calls)
	}
}
