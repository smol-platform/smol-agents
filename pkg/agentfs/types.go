package agentfs

import (
	"errors"
	"io"
	"time"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

// Version identifies a single snapshot in the backup target.
type Version struct {
	// ID is the S3 object Version ID (or driver-equivalent).
	ID string

	// Key is the S3 key under which the snapshot lives.
	Key string

	// CreatedAt is when the snapshot was uploaded.
	CreatedAt time.Time

	// SizeBytes is the on-wire (post-compression) size.
	SizeBytes int64

	// Kind is "full" or "wal".
	Kind string
}

// SnapshotKind enumerates upload kinds.
type SnapshotKind string

const (
	SnapshotFull SnapshotKind = "full" // a full SQLite copy via online backup
	SnapshotWAL  SnapshotKind = "wal"  // an incremental WAL-frame batch
)

// Storage is the SQLite-side driver. Production: modernc.org/sqlite via
// the standard backup API; tests use an in-memory file copy.
type Storage interface {
	// SnapshotTo writes a consistent point-in-time copy of the SQLite
	// DB to w. The driver MUST hold a read lock for the duration of
	// the copy so the snapshot is internally consistent.
	SnapshotTo(w io.Writer) error

	// WALFrames yields any WAL frames since the last call. An empty
	// return signals "no new frames".
	WALFrames() ([]byte, error)

	// RestoreFrom replaces the DB with the contents of r. The caller
	// must ensure no other process has the DB open.
	RestoreFrom(r io.Reader) error
}

// S3 is the object-store driver. Implementations: an aws-sdk-go-v2
// adapter for production, an in-memory map for tests.
type S3 interface {
	// Put uploads an object. Returns the new VersionID and size.
	Put(key string, body io.Reader, meta PutMeta) (Version, error)

	// ListVersions returns the versions of the given key, newest first.
	ListVersions(key string) ([]Version, error)

	// Get fetches a specific version (or latest when versionID is "").
	Get(key, versionID string) (io.ReadCloser, error)

	// Delete removes one version.
	Delete(key, versionID string) error

	// HasVersioning reports whether the bucket has versioning enabled.
	// The driver verifies once at startup so we can refuse to operate
	// on an unversioned bucket when spec.versioning==true (R-AFS-2).
	HasVersioning() (bool, error)
}

// PutMeta carries the per-upload metadata.
type PutMeta struct {
	ContentType  string
	SSEAlgorithm string // "" | "AES256" | "aws:kms"
	KMSKeyARN    string
	UserMeta     map[string]string
}

// Errors visible to callers.
var (
	ErrNoVersion       = errors.New("agentfs: no versions found")
	ErrVersioningOff   = errors.New("agentfs: bucket versioning disabled but required")
	ErrInvalidPolicy   = errors.New("agentfs: invalid backup policy")
	ErrRestoreNotFound = errors.New("agentfs: restore target not found in S3")
)

// Manager orchestrates backups, WAL snapshotting, and retention. It is
// the public face of the package for the agent runtime.
type Manager struct {
	Spec    v1.AgentFSSpec
	Storage Storage
	S3      S3
	Now     func() time.Time
}
