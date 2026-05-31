// versioned.go — the VersionedStore seam: a content-addressed, git-like backend
// (kopia today; lakeFS later) that supersedes the legacy single-key tar
// FilesystemStorage when Manager.Backend is set. A repo-shaped engine owns its
// own multi-object layout, so this is a sibling capability rather than another
// io.Writer/io.Reader Storage impl (the shipped Storage PUTs one S3 key). See
// docs/design/agentfs-fuse-plugin.md.
package agentfs

import (
	"context"
	"time"
)

// Checkpoint is one immutable, content-addressed point-in-time of the
// workspace tree — the git-like "commit". For kopia this is a snapshot
// manifest ID; for lakeFS a commit ID on a branch.
type Checkpoint struct {
	// ID is the engine-native identity (kopia snapshot ID / lakeFS commit ID).
	// It is the handle passed to Restore and Diff and is what RestorePolicy
	// "versionID" maps onto for a versioned backend.
	ID string

	// CreatedAt is when the checkpoint was written. RestorePolicy
	// "pointInTime" selects the most recent Checkpoint with CreatedAt <= T,
	// mirroring pickRestoreTarget in restore.go.
	CreatedAt time.Time

	// Message is an optional human/agent-supplied label (e.g. a Step index),
	// surfaced up to RunStatus / an AgentFS history API.
	Message string

	// SizeBytes is the logical tree size at checkpoint time (engine-reported,
	// best-effort). Physical bytes uploaded are typically far smaller due to
	// content-defined chunking + dedup.
	SizeBytes int64
}

// FileChange is one entry in a diff between two checkpoints (requirement #4:
// "diff between checkpoints"). Content-hash based for kopia; for lakeFS via
// lakectl-local the diff is heuristic (size+mtime+perms) unless checksum
// comparison is forced — see the design doc's lakeFS caveat.
type FileChange struct {
	Path string
	Type ChangeType
}

// ChangeType enumerates the kinds of per-file change in a diff.
type ChangeType string

const (
	ChangeAdded    ChangeType = "added"
	ChangeModified ChangeType = "modified"
	ChangeDeleted  ChangeType = "deleted"
)

// RetentionSpec is the engine-agnostic projection of v1.RetentionPolicy used by
// GC. The single-key ListVersions/Delete retention in backup.go does NOT
// transfer to a multi-object repo, so a versioned backend owns its own GC
// (kopia maintenance / lakeFS GC) against these bounds.
type RetentionSpec struct {
	MaxVersions int
	MinAge      time.Duration
}

// VersionedStore is the seam a content-addressed, git-like engine implements.
//
// Lifecycle mapping onto the shipped sidecar (cmd/agentfs-sidecar):
//   - init  verb  -> Connect, then Restore(ctx, ref, dst)
//   - serve verb  -> Connect, then Checkpoint on each Scheduler tick AND on
//     SIGTERM (the F4 final-backup-on-shutdown hook), then GC.
//
// All methods operate on a LOCAL directory (the EmptyDir workspace). The engine
// reaches S3 as an ordinary HTTPS client with broker-projected creds — NO FUSE,
// NO mount(2), NO /dev/fuse, NO privilege. The "dst"/"src" dirs are the same
// EmptyDir bound to the harness CWD via DefaultAgentFSMountPath, so F1/F2 are
// unaffected.
type VersionedStore interface {
	// Connect opens (creating if absent) the engine's repo against the
	// configured S3 destination. Idempotent. Must be called before any other
	// method.
	Connect(ctx context.Context) error

	// Restore materializes the tree referenced by ref into dst. ref is "" or
	// "latest" for the newest checkpoint, an engine checkpoint ID, or a
	// pointInTime token resolved by the caller (Manager.Restore). It returns
	// the Checkpoint actually materialized.
	Restore(ctx context.Context, ref, dst string) (Checkpoint, error)

	// Checkpoint creates a new immutable checkpoint of the tree rooted at src
	// and uploads only changed content. msg is an optional label. This
	// replaces Manager.Backup's tar+Put — it MUST stream (no whole-tree
	// buffer in memory), retiring the bytes.Buffer OOM risk in backup.go:40.
	//
	// The caller is responsible for excluding live-DB artifacts
	// (*.db/*-wal/*-shm/*.sqlite*) before invoking; see ExcludeGlobs.
	Checkpoint(ctx context.Context, src, msg string) (Checkpoint, error)

	// History lists checkpoints, newest first.
	History(ctx context.Context) ([]Checkpoint, error)

	// Diff reports per-file changes between two checkpoints (a -> b).
	Diff(ctx context.Context, a, b string) ([]FileChange, error)

	// GC prunes checkpoints beyond the retention bound. Replaces
	// Manager.EnforceRetention for the versioned path; the backend owns its
	// own object-lifecycle (an interrupted prune can leak objects, so this
	// must be safe to re-run).
	GC(ctx context.Context, ret RetentionSpec) error
}

// ExcludeGlobs are the workspace patterns a versioned FILE backend must never
// capture, because a live WAL-mode SQLite DB tars/chunks into torn pages. These
// are routed to a separate DB-aware lane (Litestream/Turso/Dolt) per the
// platform constraint and docs/research/agentfs-versioning.md Tier 2. The
// shipped FilesystemStorage ignores this and is the source of the current
// torn-write hazard (fs_storage.go:28 walks everything).
var ExcludeGlobs = []string{
	"*.db", "*.db-wal", "*.db-shm",
	"*.sqlite", "*.sqlite-wal", "*.sqlite-shm",
	"*.sqlite3", "*.sqlite3-wal", "*.sqlite3-shm",
}
