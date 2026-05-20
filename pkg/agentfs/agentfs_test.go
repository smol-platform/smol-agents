package agentfs

import (
	"errors"
	"testing"
	"time"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

func mkManager(t *testing.T) (*Manager, *FakeStorage, *FakeS3) {
	t.Helper()
	storage := &FakeStorage{Data: []byte("INITIAL_DB_v1")}
	s3 := NewFakeS3()
	now := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	s3.SetClock(func() time.Time { return now })

	m := &Manager{
		Spec: v1.AgentFSSpec{
			SizeGiB:   1,
			MountPath: "/var/agentfs",
			Backup: &v1.BackupPolicy{
				S3: &v1.S3BackupSpec{
					Bucket:     "test",
					Versioning: true,
				},
				Retention: v1.RetentionPolicy{MaxVersions: 3},
			},
			Restore: &v1.RestorePolicy{Mode: "latest"},
		},
		Storage: storage,
		S3:      s3,
		Now:     func() time.Time { return now },
	}
	return m, storage, s3
}

func TestBackup_UploadsFullSnapshot(t *testing.T) {
	m, _, s3 := mkManager(t)
	v, err := m.Backup()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if v.ID == "" {
		t.Error("expected version id")
	}
	versions, _ := s3.ListVersions(m.keyForFull())
	if len(versions) != 1 {
		t.Errorf("got %d versions, want 1", len(versions))
	}
}

func TestBackup_RefusesUnversionedBucket(t *testing.T) {
	m, _, s3 := mkManager(t)
	s3.SetVersioning(false)
	_, err := m.Backup()
	if !errors.Is(err, ErrVersioningOff) {
		t.Errorf("expected ErrVersioningOff, got %v", err)
	}
}

func TestSnapshotWAL_NoFramesNoUpload(t *testing.T) {
	m, _, s3 := mkManager(t)
	_, ok, err := m.SnapshotWAL()
	if err != nil || ok {
		t.Errorf("expected no-op: ok=%v err=%v", ok, err)
	}
	versions, _ := s3.ListVersions(m.keyForWAL())
	if len(versions) != 0 {
		t.Errorf("unexpected wal version: %d", len(versions))
	}
}

func TestSnapshotWAL_WithFramesUploads(t *testing.T) {
	m, storage, s3 := mkManager(t)
	storage.Wal = []byte("WAL_FRAMES_v1")
	v, ok, err := m.SnapshotWAL()
	if err != nil || !ok {
		t.Fatalf("expected upload: ok=%v err=%v", ok, err)
	}
	if v.Kind != string(SnapshotWAL) {
		t.Errorf("kind=%s", v.Kind)
	}
	versions, _ := s3.ListVersions(m.keyForWAL())
	if len(versions) != 1 {
		t.Errorf("expected 1 wal version: %d", len(versions))
	}
}

func TestEnforceRetention_TrimsToMax(t *testing.T) {
	m, storage, _ := mkManager(t)
	// 5 backups; cap is 3.
	for i := 0; i < 5; i++ {
		storage.Data = []byte("DB_v" + itoa(int64(i)))
		if _, err := m.Backup(); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := m.EnforceRetention()
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Errorf("deleted=%d, want 2", deleted)
	}
	versions, _ := m.S3.ListVersions(m.keyForFull())
	if len(versions) != 3 {
		t.Errorf("after retention: %d versions, want 3", len(versions))
	}
}

func TestEnforceRetention_HonoursMinAge(t *testing.T) {
	m, storage, _ := mkManager(t)
	// Mutable clock so we can age versions.
	t0 := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	current := t0
	clock := func() time.Time { return current }
	m.S3.(*FakeS3).SetClock(clock)
	m.Now = clock

	// Create 5 versions, all "now" — none should be deletable yet.
	for i := 0; i < 5; i++ {
		storage.Data = []byte("DB_" + itoa(int64(i)))
		if _, err := m.Backup(); err != nil {
			t.Fatal(err)
		}
	}
	m.Spec.Backup.Retention.MaxVersions = 2
	m.Spec.Backup.Retention.MinAge = "24h"
	deleted, _ := m.EnforceRetention()
	if deleted != 0 {
		t.Errorf("min-age guard ineffective: deleted %d", deleted)
	}
	// Advance past 24h and try again.
	current = t0.Add(48 * time.Hour)
	deleted, _ = m.EnforceRetention()
	if deleted == 0 {
		t.Error("expected deletions after MinAge elapsed")
	}
}

func TestRestore_LatestRoundTrip(t *testing.T) {
	m, storage, _ := mkManager(t)
	storage.Data = []byte("snapshot_at_t1")
	if _, err := m.Backup(); err != nil {
		t.Fatal(err)
	}
	storage.Data = []byte("DB_MUTATED_LOCALLY")
	v, err := m.Restore()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if v.ID == "" {
		t.Error("expected version id")
	}
	if string(storage.Data) != "snapshot_at_t1" {
		t.Errorf("restore did not overwrite local DB: %q", storage.Data)
	}
}

func TestRestore_PointInTime(t *testing.T) {
	m, storage, _ := mkManager(t)
	t0 := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	cur := t0
	clk := func() time.Time { return cur }
	m.S3.(*FakeS3).SetClock(clk)
	m.Now = clk

	storage.Data = []byte("v1")
	_, _ = m.Backup()
	cur = t0.Add(time.Hour)
	storage.Data = []byte("v2")
	_, _ = m.Backup()
	cur = t0.Add(2 * time.Hour)
	storage.Data = []byte("v3")
	_, _ = m.Backup()

	m.Spec.Restore = &v1.RestorePolicy{Mode: "pointInTime", PointInTime: t0.Add(90 * time.Minute).Format(time.RFC3339)}
	storage.Data = nil
	if _, err := m.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if string(storage.Data) != "v2" {
		t.Errorf("expected v2 (most recent ≤ t+90m), got %q", storage.Data)
	}
}

func TestRestore_VersionID(t *testing.T) {
	m, storage, _ := mkManager(t)
	storage.Data = []byte("first")
	v1Backup, _ := m.Backup()
	storage.Data = []byte("second")
	_, _ = m.Backup()
	storage.Data = []byte("third")
	_, _ = m.Backup()

	m.Spec.Restore = &v1.RestorePolicy{Mode: "versionID", VersionID: v1Backup.ID}
	storage.Data = nil
	if _, err := m.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if string(storage.Data) != "first" {
		t.Errorf("expected first, got %q", storage.Data)
	}
}

func TestRestore_MissingBucket_Fresh(t *testing.T) {
	m, _, _ := mkManager(t)
	m.Spec.Restore = &v1.RestorePolicy{Mode: "latest", IfMissing: "fresh"}
	v, err := m.Restore()
	if err != nil {
		t.Errorf("expected nil err with IfMissing=fresh: %v", err)
	}
	if v.ID != "" {
		t.Error("expected zero Version on fresh path")
	}
}

func TestRestore_MissingBucket_Fail(t *testing.T) {
	m, _, _ := mkManager(t)
	m.Spec.Restore = &v1.RestorePolicy{Mode: "latest", IfMissing: "fail"}
	_, err := m.Restore()
	if err == nil {
		t.Error("expected error with IfMissing=fail")
	}
}

func TestCheckPolicy_KMSRequiresKey(t *testing.T) {
	m, _, _ := mkManager(t)
	m.Spec.Backup.S3.SSEAlgorithm = "aws:kms"
	if err := m.checkPolicy(); err == nil {
		t.Error("expected KMSKeyARN error")
	}
}

func TestRestore_NoPolicy_NoOp(t *testing.T) {
	m, _, _ := mkManager(t)
	m.Spec.Restore = nil
	v, err := m.Restore()
	if err != nil || v.ID != "" {
		t.Errorf("nil policy should no-op: err=%v v=%+v", err, v)
	}
}
