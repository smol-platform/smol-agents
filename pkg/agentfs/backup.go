package agentfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"time"
)

// keyForFull returns the S3 key for the canonical full-snapshot
// object. Because S3 versioning means every PUT to the same key
// creates a new version, the operator never invents per-snapshot key
// names — it relies on bucket versioning.
func (m *Manager) keyForFull() string {
	prefix := ""
	if m.Spec.Backup != nil && m.Spec.Backup.S3 != nil {
		prefix = m.Spec.Backup.S3.Prefix
	}
	return path.Join(prefix, "agentfs.sqlite")
}

// keyForWAL returns the rolling key under which WAL frame batches are
// uploaded. Each WAL upload is also a new version of the same key.
func (m *Manager) keyForWAL() string {
	prefix := ""
	if m.Spec.Backup != nil && m.Spec.Backup.S3 != nil {
		prefix = m.Spec.Backup.S3.Prefix
	}
	return path.Join(prefix, "agentfs.wal")
}

// Backup takes a full SQLite snapshot and uploads it. Returns the
// newly-created Version. R-AFS-1.
func (m *Manager) Backup() (Version, error) {
	if m.Backend != nil {
		return m.backendCheckpoint()
	}
	if err := m.checkPolicy(); err != nil {
		return Version{}, err
	}
	buf := &bytes.Buffer{}
	if err := m.Storage.SnapshotTo(buf); err != nil {
		return Version{}, fmt.Errorf("agentfs: snapshot: %w", err)
	}
	meta := m.putMeta()
	v, err := m.S3.Put(m.keyForFull(), buf, meta)
	if err != nil {
		return Version{}, fmt.Errorf("agentfs: upload full: %w", err)
	}
	v.Kind = string(SnapshotFull)
	return v, nil
}

// backendCheckpoint drives the VersionedStore backend (kopia): Connect
// (idempotent) then Checkpoint the mount. Replaces the tar Backup when
// Manager.Backend is set — kopia streams + dedupes, so there is no in-memory
// tree buffer (retires the bytes.Buffer OOM of the tar path).
func (m *Manager) backendCheckpoint() (Version, error) {
	ctx := context.Background()
	if err := m.Backend.Connect(ctx); err != nil {
		return Version{}, fmt.Errorf("agentfs: backend connect: %w", err)
	}
	cp, err := m.Backend.Checkpoint(ctx, m.Spec.MountPath, "")
	if err != nil {
		return Version{}, fmt.Errorf("agentfs: backend checkpoint: %w", err)
	}
	return Version{ID: cp.ID, Key: "kopia", CreatedAt: cp.CreatedAt, SizeBytes: cp.SizeBytes, Kind: string(SnapshotFull)}, nil
}

// SnapshotWAL uploads a WAL-frame batch if the storage driver has new
// frames. Returns ok=false (no error) when there's nothing to send.
func (m *Manager) SnapshotWAL() (Version, bool, error) {
	frames, err := m.Storage.WALFrames()
	if err != nil {
		return Version{}, false, fmt.Errorf("agentfs: wal: %w", err)
	}
	if len(frames) == 0 {
		return Version{}, false, nil
	}
	v, err := m.S3.Put(m.keyForWAL(), bytes.NewReader(frames), m.putMeta())
	if err != nil {
		return Version{}, false, fmt.Errorf("agentfs: upload wal: %w", err)
	}
	v.Kind = string(SnapshotWAL)
	return v, true, nil
}

// EnforceRetention deletes versions beyond the policy bound. It never
// deletes the most recent version. R-AFS-3.
func (m *Manager) EnforceRetention() (deleted int, err error) {
	if m.Backend != nil {
		var ret RetentionSpec
		if m.Spec.Backup != nil {
			ret.MaxVersions = int(m.Spec.Backup.Retention.MaxVersions)
			if m.Spec.Backup.Retention.MinAge != "" {
				if d, e := time.ParseDuration(m.Spec.Backup.Retention.MinAge); e == nil {
					ret.MinAge = d
				}
			}
		}
		return 0, m.Backend.GC(context.Background(), ret)
	}
	if m.Spec.Backup == nil {
		return 0, nil
	}
	policy := m.Spec.Backup.Retention
	if policy.MaxVersions <= 0 {
		return 0, nil
	}
	versions, err := m.S3.ListVersions(m.keyForFull())
	if err != nil {
		return 0, fmt.Errorf("agentfs: list: %w", err)
	}
	// Newest first.
	sort.SliceStable(versions, func(i, j int) bool {
		return versions[i].CreatedAt.After(versions[j].CreatedAt)
	})
	if int32(len(versions)) <= policy.MaxVersions {
		return 0, nil
	}
	now := m.now()
	minAge := time.Duration(0)
	if policy.MinAge != "" {
		if d, err := time.ParseDuration(policy.MinAge); err == nil {
			minAge = d
		}
	}
	candidates := versions[policy.MaxVersions:] // everything past the cap
	for _, v := range candidates {
		if minAge > 0 && now.Sub(v.CreatedAt) < minAge {
			continue
		}
		if err := m.S3.Delete(v.Key, v.ID); err != nil {
			return deleted, fmt.Errorf("agentfs: delete %s: %w", v.ID, err)
		}
		deleted++
	}
	return deleted, nil
}

// checkPolicy validates the spec's S3 settings and the bucket's
// versioning state at runtime. Implements R-AFS-2.
func (m *Manager) checkPolicy() error {
	if m.Spec.Backup == nil || m.Spec.Backup.S3 == nil {
		return ErrInvalidPolicy
	}
	if m.Spec.Backup.S3.Versioning {
		ok, err := m.S3.HasVersioning()
		if err != nil {
			return fmt.Errorf("agentfs: check versioning: %w", err)
		}
		if !ok {
			return ErrVersioningOff
		}
	}
	if m.Spec.Backup.S3.SSEAlgorithm == "aws:kms" && m.Spec.Backup.S3.KMSKeyARN == "" {
		return errors.New("agentfs: SSE=aws:kms requires KMSKeyARN")
	}
	return nil
}

func (m *Manager) putMeta() PutMeta {
	if m.Spec.Backup == nil || m.Spec.Backup.S3 == nil {
		return PutMeta{ContentType: "application/octet-stream"}
	}
	return PutMeta{
		ContentType:  "application/octet-stream",
		SSEAlgorithm: m.Spec.Backup.S3.SSEAlgorithm,
		KMSKeyARN:    m.Spec.Backup.S3.KMSKeyARN,
		UserMeta:     map[string]string{"agentfs-source": "smol-agents"},
	}
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}
