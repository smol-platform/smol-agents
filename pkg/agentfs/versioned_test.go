// Tests for the VersionedStore seam and the kopia-backed store. The
// fakeVersionedStore exercises the restore->checkpoint->history->diff lifecycle
// and the DB-exclusion contract with no kopia/S3/network; the KopiaStore tests
// inject `run` (or a fake kopia script) so they stay hermetic. ErrNoVersion and
// ErrRestoreNotFound are reused from the in-package types.go.
package agentfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeVersionedStore is an in-memory VersionedStore for tests: checkpoints are
// appended; restore returns the requested (or latest) checkpoint.
type fakeVersionedStore struct {
	connected bool
	snaps     []Checkpoint
	now       time.Time
}

var _ VersionedStore = (*fakeVersionedStore)(nil)

func (f *fakeVersionedStore) Connect(context.Context) error { f.connected = true; return nil }

func (f *fakeVersionedStore) Restore(_ context.Context, ref, _ string) (Checkpoint, error) {
	if len(f.snaps) == 0 {
		return Checkpoint{}, ErrNoVersion
	}
	if ref == "" || ref == "latest" {
		return f.snaps[len(f.snaps)-1], nil
	}
	for _, c := range f.snaps {
		if c.ID == ref {
			return c, nil
		}
	}
	return Checkpoint{}, ErrRestoreNotFound
}

func (f *fakeVersionedStore) Checkpoint(_ context.Context, _, msg string) (Checkpoint, error) {
	f.now = f.now.Add(time.Minute)
	c := Checkpoint{ID: msg, CreatedAt: f.now, Message: msg}
	f.snaps = append(f.snaps, c)
	return c, nil
}

func (f *fakeVersionedStore) History(context.Context) ([]Checkpoint, error) {
	out := make([]Checkpoint, len(f.snaps))
	for i, c := range f.snaps { // newest first
		out[len(f.snaps)-1-i] = c
	}
	return out, nil
}

func (f *fakeVersionedStore) Diff(context.Context, string, string) ([]FileChange, error) {
	return []FileChange{{Path: "main.go", Type: ChangeModified}}, nil
}

func (f *fakeVersionedStore) GC(_ context.Context, ret RetentionSpec) error {
	if ret.MaxVersions > 0 && len(f.snaps) > ret.MaxVersions {
		f.snaps = f.snaps[len(f.snaps)-ret.MaxVersions:] // keep newest N
	}
	return nil
}

func TestVersionedStore_Lifecycle(t *testing.T) {
	ctx := context.Background()
	st := &fakeVersionedStore{now: time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)}

	if err := st.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Empty repo: restore reports no version (maps to RestorePolicy ifMissing).
	if _, err := st.Restore(ctx, "latest", "/var/agentfs"); err != ErrNoVersion {
		t.Fatalf("Restore on empty: want ErrNoVersion, got %v", err)
	}

	// Three checkpoints (e.g. one per executor Step).
	for _, step := range []string{"step-1", "step-2", "step-3"} {
		if _, err := st.Checkpoint(ctx, "/var/agentfs", step); err != nil {
			t.Fatalf("Checkpoint %s: %v", step, err)
		}
	}

	// Latest restore picks the newest.
	got, err := st.Restore(ctx, "latest", "/var/agentfs")
	if err != nil {
		t.Fatalf("Restore latest: %v", err)
	}
	if got.ID != "step-3" {
		t.Errorf("Restore latest: got %q, want step-3", got.ID)
	}

	// Restore by explicit checkpoint ID (rollback).
	got, err = st.Restore(ctx, "step-1", "/var/agentfs")
	if err != nil {
		t.Fatalf("Restore by id: %v", err)
	}
	if got.ID != "step-1" {
		t.Errorf("Restore by id: got %q, want step-1", got.ID)
	}

	// History newest-first.
	hist, err := st.History(ctx)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 3 || hist[0].ID != "step-3" {
		t.Fatalf("History: got %d entries, head %v; want 3 newest-first", len(hist), hist)
	}

	// GC keeps the newest 2.
	if err := st.GC(ctx, RetentionSpec{MaxVersions: 2}); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if hist, _ := st.History(ctx); len(hist) != 2 || hist[0].ID != "step-3" {
		t.Errorf("after GC: want 2 newest-first, got %v", hist)
	}
}

// TestExcludeGlobs_CoversSQLiteArtifacts guards the live-DB exclusion contract:
// every SQLite sidecar-file family that must NOT enter the file checkpoint is
// listed (the DB is routed to a separate WAL-stream lane instead).
func TestExcludeGlobs_CoversSQLiteArtifacts(t *testing.T) {
	want := []string{"*.db", "*.db-wal", "*.db-shm", "*.sqlite-wal", "*.sqlite-shm"}
	have := make(map[string]bool, len(ExcludeGlobs))
	for _, g := range ExcludeGlobs {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("ExcludeGlobs missing %q (torn-write hazard)", w)
		}
	}
}

// findCall returns the first recorded kopia invocation whose leading args match
// prefix, or nil.
func findCall(calls [][]string, prefix ...string) []string {
	for _, c := range calls {
		if len(c) < len(prefix) {
			continue
		}
		ok := true
		for i := range prefix {
			if c[i] != prefix[i] {
				ok = false
				break
			}
		}
		if ok {
			return c
		}
	}
	return nil
}

// TestKopiaStore_Commands exercises the real KopiaStore against an injected
// `run`: it asserts command/flag construction (S3 connect/create, endpoint
// scheme stripping + --disable-tls, the create-on-connect-failure flow, the
// DB-ignore policy) and the JSON/diff parsing — without a real kopia binary.
func TestKopiaStore_Commands(t *testing.T) {
	var calls [][]string
	k := &KopiaStore{
		Bucket: "b", Prefix: "agent-x", Region: "us-east-1",
		Endpoint: "http://minio:9000", AccessKeyID: "AK", SecretAccessKey: "SK", Password: "pw",
	}
	k.run = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		switch {
		case args[0] == "repository" && args[1] == "connect":
			return nil, errors.New("repository not found") // force the create path
		case args[0] == "snapshot" && args[1] == "create":
			return []byte(`{"id":"snap-1","startTime":"2026-05-31T00:00:00Z","stats":{"totalSize":1234}}`), nil
		case args[0] == "snapshot" && args[1] == "list":
			return []byte(`[{"id":"snap-1","startTime":"2026-05-31T00:00:00Z","stats":{"totalSize":1234}}]`), nil
		case args[0] == "diff":
			return []byte("added file ./a.txt\nmodified ./b.txt\nremoved ./c.txt\n"), nil
		default:
			return nil, nil // create, policy, restore, expire, maintenance
		}
	}
	ctx := context.Background()

	if err := k.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	create := findCall(calls, "repository", "create")
	if create == nil {
		t.Fatal("expected `repository create` after connect failed")
	}
	joined := strings.Join(create, " ")
	for _, want := range []string{"--bucket=b", "--prefix=agent-x/kopia/", "--region=us-east-1", "--endpoint=minio:9000", "--disable-tls", "--access-key=AK"} {
		if !strings.Contains(joined, want) {
			t.Errorf("create args missing %q: %s", want, joined)
		}
	}
	if findCall(calls, "policy", "set") == nil {
		t.Error("expected DB-ignore policy set at creation")
	}

	cp, err := k.Checkpoint(ctx, "/var/agentfs", "step-1")
	if err != nil || cp.ID != "snap-1" || cp.SizeBytes != 1234 || cp.Message != "step-1" {
		t.Fatalf("Checkpoint: err=%v cp=%+v", err, cp)
	}

	hist, err := k.History(ctx)
	if err != nil || len(hist) != 1 || hist[0].ID != "snap-1" {
		t.Fatalf("History: err=%v hist=%+v", err, hist)
	}

	rcp, err := k.Restore(ctx, "latest", "/dst")
	if err != nil || rcp.ID != "snap-1" {
		t.Fatalf("Restore latest: err=%v cp=%+v", err, rcp)
	}
	if findCall(calls, "snapshot", "restore", "snap-1") == nil {
		t.Error("expected `snapshot restore snap-1`")
	}

	d, err := k.Diff(ctx, "snap-0", "snap-1")
	if err != nil || len(d) != 3 || d[0].Type != ChangeAdded || d[1].Type != ChangeModified || d[2].Type != ChangeDeleted {
		t.Fatalf("Diff: err=%v d=%+v", err, d)
	}

	if err := k.GC(ctx, RetentionSpec{MaxVersions: 3}); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if findCall(calls, "maintenance", "run") == nil {
		t.Error("expected `maintenance run` in GC")
	}
}

// TestKopiaStore_FilesystemRepo asserts the ephemeral (no-S3) backend uses a
// local filesystem repo: Connect issues `repository create filesystem
// --path=<RepoPath>` (after the connect probe fails) and emits no S3 flags.
func TestKopiaStore_FilesystemRepo(t *testing.T) {
	var calls [][]string
	k := &KopiaStore{RepoPath: "/var/run/agentfs-kopia", Password: "pw"}
	k.run = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "repository" && args[1] == "connect" {
			return nil, errors.New("repository not found") // force the create path
		}
		return nil, nil
	}
	if err := k.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	create := findCall(calls, "repository", "create", "filesystem")
	if create == nil {
		t.Fatal("expected `repository create filesystem` for the ephemeral backend")
	}
	joined := strings.Join(create, " ")
	if !strings.Contains(joined, "--path=/var/run/agentfs-kopia") {
		t.Errorf("filesystem repo missing --path: %s", joined)
	}
	if strings.Contains(joined, "--bucket") || strings.Contains(joined, " s3") {
		t.Errorf("ephemeral repo must not emit S3 flags: %s", joined)
	}
}

// TestManager_BackendRoutes verifies Manager.Backup/Restore/EnforceRetention
// route to the VersionedStore backend when set (instead of the tar path).
func TestManager_BackendRoutes(t *testing.T) {
	m, _, _ := mkManager(t)
	st := &fakeVersionedStore{now: time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)}
	m.Backend = st
	m.Spec.MountPath = "/var/agentfs"

	if _, err := m.Backup(); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(st.snaps) != 1 {
		t.Errorf("Backup did not route to Backend.Checkpoint: %d snaps", len(st.snaps))
	}
	if _, err := m.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := m.EnforceRetention(); err != nil {
		t.Fatalf("EnforceRetention: %v", err)
	}
}

// TestKopiaStore_DefaultRunStdoutOnly is a regression guard for the live bug
// where defaultRun used CombinedOutput: kopia writes the --json manifest to
// stdout and progress lines to stderr, so merging them broke parseManifests
// ("no manifest in output"). defaultRun must return stdout only. Uses a fake
// kopia shell script to stay hermetic; skipped where /bin/sh is unavailable.
func TestKopiaStore_DefaultRunStdoutOnly(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh for the fake kopia")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "kopia")
	script := "#!/bin/sh\n" +
		`printf '%s' '{"id":"deadbeef","startTime":"2026-01-01T00:00:00Z","stats":{"totalSize":42}}'` + "\n" +
		`echo 'Snapshotting nonroot@host:/var/agentfs ...' 1>&2` + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	k := &KopiaStore{Binary: fake, ConfigDir: dir, Password: "x"}
	out, err := k.exec(context.Background(), "snapshot", "create", "--json", "/var/agentfs")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.Contains(string(out), "Snapshotting") {
		t.Fatalf("stderr leaked into stdout: %q", out)
	}
	if cps := parseManifests(out); len(cps) != 1 || cps[0].ID != "deadbeef" || cps[0].SizeBytes != 42 {
		t.Fatalf("parseManifests(%q) = %+v, want one cp id=deadbeef size=42", out, cps)
	}
}
