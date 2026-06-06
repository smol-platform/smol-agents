package agentruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// SessionTurn is one processed turn in a durable session: the input, the folded
// result, and the resources it consumed. Turns accumulate across the life of an
// AgentSession and are checkpointed, so a restarted worker resumes the
// conversation + usage exactly where it left off.
type SessionTurn struct {
	Index             int             `json:"index"`
	Input             json.RawMessage `json:"input,omitempty"`
	Output            json.RawMessage `json:"output,omitempty"`
	Phase             v1.Phase        `json:"phase"`
	Usage             v1.Usage        `json:"usage"`
	TerminationReason string          `json:"terminationReason,omitempty"`
	Error             string          `json:"error,omitempty"`
	StartedAt         time.Time       `json:"startedAt"`
	EndedAt           time.Time       `json:"endedAt"`
}

// SessionState is the durable state of a long-running AgentSession: the turn
// log + cumulative usage + phase. It lives as a JSON file inside the AgentFS
// workspace, so the existing kopia/S3 backup sidecar snapshots it alongside the
// agent's files — one consistent checkpoint of "the whole session" that a
// restarted (or migrated) worker restores to resume.
type SessionState struct {
	AgentRef        string        `json:"agentRef,omitempty"`
	Phase           v1.Phase      `json:"phase"`
	Turns           []SessionTurn `json:"turns,omitempty"`
	CumulativeUsage v1.Usage      `json:"cumulativeUsage"`
	CreatedAt       time.Time     `json:"createdAt"`
	UpdatedAt       time.Time     `json:"updatedAt"`

	// TotalTurns is the monotonic count of turns ever appended — the source of a
	// turn's Index, so indices stay correct (and status counts stay monotonic)
	// even after history compaction drops old entries from Turns (M2.18/M2.19).
	TotalTurns int `json:"totalTurns,omitempty"`
	// FailedTurns counts turns that ended Failed or carried an error.
	FailedTurns int `json:"failedTurns,omitempty"`
}

// Append records a completed turn, advancing its index, cumulative usage, and
// UpdatedAt. now is injected so callers (and tests) control the clock.
func (s *SessionState) Append(t SessionTurn, now time.Time) {
	t.Index = s.TotalTurns // monotonic — survives history compaction
	s.TotalTurns++
	if t.Phase == v1.PhaseFailed || t.Error != "" {
		s.FailedTurns++
	}
	s.Turns = append(s.Turns, t)
	s.CumulativeUsage.Steps += t.Usage.Steps
	s.CumulativeUsage.Tokens += t.Usage.Tokens
	s.CumulativeUsage.ToolCalls += t.Usage.ToolCalls
	s.CumulativeUsage.WallClockUsed += t.Usage.WallClockUsed
	s.UpdatedAt = now
}

// SessionSummary is the compact, status-shaped projection of a session that the
// worker checkpoints alongside the full state so the operator can mirror it into
// AgentSession.status without reading (or RBAC for) the whole turn log. Usage is
// CumulativeUsage verbatim — field-wise roll-up done in Append, never Usage.Add
// (which phantom-increments Steps). Turns is monotonic (TotalTurns), so it keeps
// climbing even after history compaction drops entries from the in-memory log.
type SessionSummary struct {
	Phase        v1.Phase   `json:"phase"`
	Usage        v1.Usage   `json:"usage"`
	Turns        int        `json:"turns"`
	FailedTurns  int        `json:"failedTurns,omitempty"`
	LastTurnTime *time.Time `json:"lastTurnTime,omitempty"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// Summary projects the session into its status-shaped form. LastTurnTime is the
// EndedAt of the most recent retained turn (nil before any turn completes).
func (s *SessionState) Summary() SessionSummary {
	sum := SessionSummary{
		Phase:       s.Phase,
		Usage:       s.CumulativeUsage,
		Turns:       s.TotalTurns,
		FailedTurns: s.FailedTurns,
		UpdatedAt:   s.UpdatedAt,
	}
	if n := len(s.Turns); n > 0 {
		t := s.Turns[n-1].EndedAt
		sum.LastTurnTime = &t
	}
	return sum
}

// DefaultSessionSummaryPath is where the worker checkpoints the status summary
// within the workspace, beside the full state checkpoint.
func DefaultSessionSummaryPath(workspace string) string {
	return filepath.Join(workspace, ".smol-session", "status-summary.json")
}

// SessionStatusPort is the worker's read-only status endpoint (GET /status →
// SessionSummary JSON). The operator scrapes it to mirror usage/turn counters
// into AgentSession.status (M2.19) — a single source of truth shared by the
// worker (cmd/agent serve-session) and the operator controller.
const SessionStatusPort = ":9091"

// SessionStore persists SessionState to Path atomically. Path should live under
// the AgentFS workspace so checkpoints ride the existing durable-storage
// sidecar (kopia/S3) and are restored by its init container on a fresh pod.
type SessionStore struct{ Path string }

// DefaultSessionStatePath is where the worker checkpoints session state within
// the workspace. Kept out of the agent's visible tree under a dotfile dir.
func DefaultSessionStatePath(workspace string) string {
	return filepath.Join(workspace, ".smol-session", "state.json")
}

// Load reads the checkpointed state, returning a fresh Pending state (not an
// error) when no checkpoint exists yet — a brand-new session "resumes" empty.
func (st SessionStore) Load() (*SessionState, error) {
	b, err := os.ReadFile(st.Path)
	if os.IsNotExist(err) {
		return &SessionState{Phase: v1.PhasePending}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agentruntime: load session state: %w", err)
	}
	var s SessionState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("agentruntime: decode session state: %w", err)
	}
	return &s, nil
}

// Save atomically writes the state (temp file + rename) so a crash mid-write
// never leaves a torn checkpoint that a resuming worker would fail to decode.
func (st SessionStore) Save(s *SessionState) error {
	if err := os.MkdirAll(filepath.Dir(st.Path), 0o700); err != nil {
		return fmt.Errorf("agentruntime: session dir: %w", err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("agentruntime: encode session state: %w", err)
	}
	tmp := st.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("agentruntime: write session state: %w", err)
	}
	return os.Rename(tmp, st.Path)
}
