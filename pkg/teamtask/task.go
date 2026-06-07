// Package teamtask is the shared task list for an AgentTeam (multi-agent
// orchestration, P1). Members claim tasks from a durable, per-team store; a
// claim is atomic (compare-and-swap) so no two members win the same task, and a
// task becomes claimable only once all its dependencies are completed (the
// durable analog of Claude Code's file-lock-on-claim + dependency graph).
//
// Two implementations: NATSStore (a NATS JetStream KV bucket per team —
// team_<ns>_<team> — CAS via key revision) and MemStore (in-memory, tests + dev).
package teamtask

import (
	"context"
	"errors"
)

// TaskState is a task's lifecycle. Transitions are monotonic:
// pending → inProgress → completed (completed is terminal).
type TaskState string

const (
	TaskPending    TaskState = "pending"
	TaskInProgress TaskState = "inProgress"
	TaskCompleted  TaskState = "completed"
)

// Task is one unit of work on a team's shared list.
type Task struct {
	ID    string    `json:"id"`
	Title string    `json:"title"`
	State TaskState `json:"state"`
	// Deps are task IDs that must be completed before this task is claimable.
	Deps []string `json:"deps,omitempty"`
	// Owner is the member name that claimed the task (set on claim).
	Owner string `json:"owner,omitempty"`
	// Result is the member-reported outcome (set on complete).
	Result string `json:"result,omitempty"`
}

var (
	// ErrNotFound is returned for an unknown task id.
	ErrNotFound = errors.New("teamtask: task not found")
	// ErrNotClaimable is returned when a task cannot be claimed: it is not
	// pending (already claimed/completed) or a dependency is incomplete.
	ErrNotClaimable = errors.New("teamtask: task not claimable")
	// ErrNotOwner is returned when a non-owner tries to complete a task.
	ErrNotOwner = errors.New("teamtask: caller does not own this task")
	// ErrConflict is returned when an atomic claim/complete lost the CAS race
	// after retries (a concurrent writer kept winning).
	ErrConflict = errors.New("teamtask: write conflict")
)

// Store is a team's durable shared task list. Implementations make Claim and
// Complete atomic (CAS) so concurrent members never corrupt a task's state.
type Store interface {
	// Create adds a task (state defaults to pending). A blank ID is assigned.
	Create(ctx context.Context, t Task) (id string, err error)
	// List returns every task on the list.
	List(ctx context.Context) ([]Task, error)
	// Get returns one task by id (ErrNotFound if absent).
	Get(ctx context.Context, id string) (Task, error)
	// Claim atomically transitions a pending, dependency-satisfied task to
	// inProgress owned by owner. ErrNotClaimable if not pending or a dep is
	// incomplete; ErrConflict if the CAS race was lost.
	Claim(ctx context.Context, id, owner string) (Task, error)
	// Complete atomically transitions an inProgress task owned by owner to
	// completed with the given result. ErrNotOwner / ErrNotClaimable otherwise.
	Complete(ctx context.Context, id, owner, result string) error
	// Close releases store resources.
	Close() error
}

// BucketName is the per-team KV bucket / namespace key: team_<ns>_<team>.
func BucketName(namespace, team string) string {
	return "team_" + namespace + "_" + team
}

// depsSatisfied reports whether every dependency of t is completed, given a
// snapshot of all tasks by id. A dangling dep (no such task) blocks the task —
// fail-closed, so a typo never silently unblocks work.
func depsSatisfied(t Task, byID map[string]Task) bool {
	for _, d := range t.Deps {
		dep, ok := byID[d]
		if !ok || dep.State != TaskCompleted {
			return false
		}
	}
	return true
}
