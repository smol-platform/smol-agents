package v1

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// StorageKind selects a persistent storage backend for an Agent.
type StorageKind string

const (
	StorageNone    StorageKind = ""        // (no persistent storage)
	StorageAgentFS StorageKind = "agentfs" // Turso AgentFS (SQLite-backed)
)

// DefaultAgentFSMountPath is where the AgentFS volume is mounted — and where a
// harness runs by default when the Agent has AgentFS storage but no explicit
// CLI working dir — unless AgentFSSpec.MountPath overrides it. It is the single
// source of truth shared by AgentSpec.EffectiveWorkingDir (the harness CWD) and
// the operator's storage mount, so the two never drift. Keep in sync with the
// kubebuilder default on AgentFSSpec.MountPath below.
const DefaultAgentFSMountPath = "/var/agentfs"

// StorageSpec wires persistent state into an Agent. Today only AgentFS
// is implemented; the discriminator leaves room for future backends.
type StorageSpec struct {
	Kind StorageKind `json:"kind"`

	// +optional
	AgentFS *AgentFSSpec `json:"agentfs,omitempty"`
}

// AgentFSSpec configures a Turso AgentFS volume + its backup policy.
//
// AgentFS gives an agent a persistent, branchable, versioned filesystem
// backed by SQLite. We treat the SQLite database as the canonical
// state and back it up to S3 — full snapshots on a schedule plus
// continuous WAL frame uploads in between.
type AgentFSSpec struct {
	// SizeGiB is the requested PVC size for the AgentFS mount.
	// +kubebuilder:validation:Minimum=1
	SizeGiB int32 `json:"sizeGiB"`

	// MountPath where the harness sees the AgentFS root.
	// +kubebuilder:default:="/var/agentfs"
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// Image overrides the AgentFS sidecar image.
	// +optional
	Image string `json:"image,omitempty"`

	// Backend selects the durability engine. "" / "tar" = the legacy
	// full-snapshot gzip-tar to one S3 key (back-compat default). "kopia" =
	// content-addressed snapshots (dedup, incremental/streaming, history, diff,
	// rollback) — see docs/design/agentfs-fuse-plugin.md. kopia takes periodic
	// snapshots automatically on the Backup schedule plus on shutdown.
	// +kubebuilder:validation:Enum="";tar;kopia
	// +optional
	Backend string `json:"backend,omitempty"`

	// Backup configures S3 backups + WAL snapshotting + retention.
	// +optional
	Backup *BackupPolicy `json:"backup,omitempty"`

	// Restore configures bootstrap-from-S3 behaviour. When unset and a
	// Backup target exists, the runtime restores the latest version on
	// first start.
	// +optional
	Restore *RestorePolicy `json:"restore,omitempty"`
}

// BackupPolicy declares the backup schedule + S3 target + retention.
type BackupPolicy struct {
	// S3 is the destination. Versioning + SSE land here.
	S3 *S3BackupSpec `json:"s3"`

	// Schedule is a cron expression for full backups. Default is hourly.
	// +kubebuilder:default:="@hourly"
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// WALSnapshotInterval is the cadence for incremental WAL frame
	// uploads between full backups. Default is 30s.
	//
	// NOT YET ENFORCED: the FilesystemStorage sidecar does full snapshots only
	// (the serve loop hardcodes WALInterval=0); this field is reserved for a
	// future WAL-streaming storage driver.
	// +optional
	WALSnapshotInterval string `json:"walSnapshotInterval,omitempty"`

	// Retention bounds the version count and minimum age before a
	// version becomes a candidate for deletion.
	// +optional
	Retention RetentionPolicy `json:"retention,omitempty"`
}

// RetentionPolicy bounds how many versions we keep.
type RetentionPolicy struct {
	// MaxVersions caps the number of stored full snapshots.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default:=24
	// +optional
	MaxVersions int32 `json:"maxVersions,omitempty"`

	// MinAge is the minimum age of a version before it can be deleted.
	// +optional
	MinAge string `json:"minAge,omitempty"`
}

// RestorePolicy describes how a fresh Pod bootstraps state from S3.
type RestorePolicy struct {
	// Mode: latest | versionID | pointInTime.
	// +kubebuilder:validation:Enum=latest;versionID;pointInTime
	// +kubebuilder:default:=latest
	Mode string `json:"mode,omitempty"`

	// VersionID is consulted when Mode==versionID.
	// +optional
	VersionID string `json:"versionID,omitempty"`

	// PointInTime is an RFC3339 timestamp; the most recent version
	// older than this is selected. Consulted when Mode==pointInTime.
	// +optional
	PointInTime string `json:"pointInTime,omitempty"`

	// IfMissing controls behaviour when no backup exists in S3.
	// +kubebuilder:validation:Enum=fresh;fail
	// +kubebuilder:default:=fresh
	// +optional
	IfMissing string `json:"ifMissing,omitempty"`
}

// S3BackupSpec carries the S3 destination + crypto + auth.
//
// Versioning here means *S3 bucket versioning* — the operator does not
// rename keys per snapshot. Each PutObject creates a new version of
// the same key, and listing object versions is how we enumerate
// snapshots for retention.
type S3BackupSpec struct {
	Bucket string `json:"bucket"`

	// Prefix groups multiple agents in one bucket.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Region is the AWS region. Required even for S3-compatible
	// services that ignore it (signed requests need a region label).
	// +optional
	// +kubebuilder:default:="us-east-1"
	Region string `json:"region,omitempty"`

	// EndpointURL targets a non-AWS S3 service (e.g. minio, R2,
	// GCS-S3-compat).
	// +optional
	EndpointURL string `json:"endpointURL,omitempty"`

	// CredentialsRef points at a broker secret with AWS-style
	// access-key-id + secret-access-key (and optional session-token).
	// When unset, the SDK's ambient credential chain is used.
	// +optional
	CredentialsRef *AuthRef `json:"credentialsRef,omitempty"`

	// SSEAlgorithm is one of "AES256" or "aws:kms". Empty disables
	// server-side encryption (not recommended).
	// +kubebuilder:validation:Enum=AES256;"aws:kms"
	// +kubebuilder:default:="AES256"
	// +optional
	SSEAlgorithm string `json:"sseAlgorithm,omitempty"`

	// KMSKeyARN is required when SSEAlgorithm=="aws:kms".
	// +optional
	KMSKeyARN string `json:"kmsKeyARN,omitempty"`

	// Versioning expresses the operator's expectation that the target
	// bucket has S3 Versioning enabled. The backup driver verifies
	// at startup and refuses to operate on a bucket that lacks it
	// when Versioning==true (R-AFS-2).
	// +kubebuilder:default:=true
	// +optional
	Versioning bool `json:"versioning,omitempty"`
}

// ValidateStorage checks the discriminator + nested specs.
func ValidateStorage(s *StorageSpec) error {
	if s == nil {
		return nil
	}
	switch s.Kind {
	case StorageNone:
		return nil
	case StorageAgentFS:
		if s.AgentFS == nil {
			return errors.New("storage.agentfs is required when kind=agentfs")
		}
		return validateAgentFS(*s.AgentFS)
	default:
		return fmt.Errorf("storage.kind=%q is invalid", s.Kind)
	}
}

func validateAgentFS(a AgentFSSpec) error {
	var errs []error
	if a.SizeGiB <= 0 {
		errs = append(errs, errors.New("agentfs.sizeGiB must be > 0"))
	}
	if a.MountPath != "" && !strings.HasPrefix(a.MountPath, "/") {
		errs = append(errs, errors.New("agentfs.mountPath must be absolute"))
	}
	switch a.Backend {
	case "", "tar", "kopia":
	default:
		errs = append(errs, fmt.Errorf("agentfs.backend=%q is invalid (want tar|kopia)", a.Backend))
	}
	if a.Backend == "kopia" && (a.Backup == nil || a.Backup.S3 == nil) {
		errs = append(errs, errors.New("agentfs.backend=kopia requires backup.s3 (the repo destination)"))
	}
	if a.Backup != nil {
		errs = append(errs, validateBackup(*a.Backup)...)
	}
	return errors.Join(errs...)
}

func validateBackup(b BackupPolicy) []error {
	var errs []error
	if b.S3 == nil || strings.TrimSpace(b.S3.Bucket) == "" {
		errs = append(errs, errors.New("backup.s3.bucket is required"))
	}
	if b.S3 != nil && b.S3.SSEAlgorithm == "aws:kms" && b.S3.KMSKeyARN == "" {
		errs = append(errs, errors.New("backup.s3.kmsKeyARN is required when sseAlgorithm=aws:kms"))
	}
	if b.WALSnapshotInterval != "" {
		if _, err := time.ParseDuration(b.WALSnapshotInterval); err != nil {
			errs = append(errs, fmt.Errorf("backup.walSnapshotInterval: %w", err))
		}
	}
	if b.Retention.MinAge != "" {
		if _, err := time.ParseDuration(b.Retention.MinAge); err != nil {
			errs = append(errs, fmt.Errorf("backup.retention.minAge: %w", err))
		}
	}
	if b.Retention.MaxVersions < 0 {
		errs = append(errs, errors.New("backup.retention.maxVersions must be ≥ 0"))
	}
	return errs
}
