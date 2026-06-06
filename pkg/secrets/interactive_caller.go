package secrets

import "strings"

// Interactive-caller policy (M4.12). When a human drives an agent's terminal
// (driver AttachGrant), the shell ttyd spawns runs in the SAME sandbox as the
// agent and can ask the broker for the agent's leased provider credentials. The
// broker distinguishes such a PTY-spawned caller from the agent process (via
// SO_PEERCRED PID → /proc ancestry) and applies a policy knob.
//
// This is defense-in-depth + audit, NOT airtight: within one sandbox a driver
// shell shares the agent's uid, so a credential cannot be fully hidden from it.
// The real controls are the microVM cage and the secretless broker (TraT mint),
// both out of M4 scope (see the M4.12 risk note).

// CallerClass is how the broker classifies a lease caller.
type CallerClass string

const (
	// CallerAgent is the agent process itself (the normal lease caller).
	CallerAgent CallerClass = "agent"
	// CallerInteractive is a PTY-spawned shell (ttyd → tmux → shell), i.e. a
	// human driver attached to the terminal.
	CallerInteractive CallerClass = "interactive"
)

// InteractiveCallerPolicy decides whether a PTY-spawned caller may lease.
type InteractiveCallerPolicy string

const (
	// InteractiveAllowAudited (the default) leases to a PTY caller but flags the
	// lease for audit — the operator can see a human used the agent's creds.
	InteractiveAllowAudited InteractiveCallerPolicy = "allow-audited"
	// InteractiveDeny refuses leases to a PTY-spawned caller (a hardened agent
	// whose creds a human driver must never obtain).
	InteractiveDeny InteractiveCallerPolicy = "deny"
)

// Decide returns whether to allow the lease and whether to audit it, for a
// caller of the given class under the policy. The agent itself always leases
// un-audited; the knob only affects interactive (PTY) callers. An empty policy
// means allow-audited (the documented default).
func (p InteractiveCallerPolicy) Decide(class CallerClass) (allow, audit bool) {
	if class != CallerInteractive {
		return true, false
	}
	if p == InteractiveDeny {
		return false, true
	}
	return true, true // allow-audited (and the empty default)
}

// ClassifyAncestry classifies a caller from its process-ancestry comm chain
// (caller process first, walking up toward PID 1). A PTY-spawned shell traces
// through ttyd or tmux; the agent process does not.
func ClassifyAncestry(ancestry []string) CallerClass {
	for _, comm := range ancestry {
		c := strings.ToLower(comm)
		if strings.Contains(c, "ttyd") || strings.Contains(c, "tmux") {
			return CallerInteractive
		}
	}
	return CallerAgent
}

// ProcAncestry returns a caller's process-ancestry comm chain (caller first).
// The production implementation reads /proc (Linux); tests inject a fake.
type ProcAncestry interface {
	Ancestry(pid int) ([]string, error)
}
