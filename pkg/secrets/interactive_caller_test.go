package secrets

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// M4.12: the broker classifies a caller from its process ancestry — a
// PTY-spawned shell (ttyd/tmux in the chain) is interactive; the agent is not.
func TestClassifyAncestry(t *testing.T) {
	cases := []struct {
		name     string
		ancestry []string
		want     CallerClass
	}{
		{"agent", []string{"agent", "containerd-shim"}, CallerAgent},
		{"ttyd-shell", []string{"bash", "tmux: server", "ttyd", "containerd-shim"}, CallerInteractive},
		{"tmux-only", []string{"sh", "tmux"}, CallerInteractive},
		{"empty", nil, CallerAgent},
	}
	for _, c := range cases {
		if got := ClassifyAncestry(c.ancestry); got != c.want {
			t.Errorf("%s: ClassifyAncestry = %q, want %q", c.name, got, c.want)
		}
	}
}

// M4.12: the knob toggles lease behavior — the agent always leases un-audited;
// an interactive caller is audited (default) or denied (the opt-in knob).
func TestInteractiveCallerPolicy_Decide(t *testing.T) {
	cases := []struct {
		policy       InteractiveCallerPolicy
		class        CallerClass
		allow, audit bool
	}{
		{"", CallerAgent, true, false},
		{InteractiveDeny, CallerAgent, true, false}, // agent unaffected
		{"", CallerInteractive, true, true},         // default = allow-audited
		{InteractiveAllowAudited, CallerInteractive, true, true},
		{InteractiveDeny, CallerInteractive, false, true},
	}
	for _, c := range cases {
		allow, audit := c.policy.Decide(c.class)
		if allow != c.allow || audit != c.audit {
			t.Errorf("policy %q class %q: (allow=%v audit=%v), want (%v %v)", c.policy, c.class, allow, audit, c.allow, c.audit)
		}
	}
}

// M4.12: end-to-end through the broker dispatch — a PTY caller is refused under
// deny, leases (audited) under allow-audited, and the agent is never affected.
func TestServer_InteractiveCallerGate(t *testing.T) {
	mk := func(p InteractiveCallerPolicy) *Server {
		backend := NewStaticBackend()
		backend.Set(idA, "db-cred", []byte("super-secret"))
		policy := NewStaticPolicy()
		policy.Grant(idA, "db-cred")
		return &Server{
			MaxLeaseTTL: time.Minute, DefaultTTL: time.Minute,
			Backend: backend, Policy: policy, Attestor: FixedPeerAttestor{ID: idA},
			Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
			Now:               func() time.Time { return time.Unix(1000, 0) },
			InteractivePolicy: p,
			issued:            map[string]Lease{},
		}
	}
	req := request{Kind: reqLease, Name: "db-cred", TTL: time.Minute}

	deny := mk(InteractiveDeny)
	if r := deny.dispatch(context.Background(), idA, CallerInteractive, req); r.Lease != nil {
		t.Error("interactive caller must be denied under the deny policy")
	}
	if r := deny.dispatch(context.Background(), idA, CallerAgent, req); r.Lease == nil {
		t.Errorf("the agent itself must still lease under the deny policy: %+v", r)
	}
	allow := mk(InteractiveAllowAudited)
	if r := allow.dispatch(context.Background(), idA, CallerInteractive, req); r.Lease == nil {
		t.Errorf("interactive caller must lease (audited) under allow-audited: %+v", r)
	}
}
