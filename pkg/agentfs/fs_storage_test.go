package agentfs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemStorage_RoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("world"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := (FilesystemStorage{Root: src}).SnapshotTo(&buf); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}

	dst := t.TempDir()
	if err := (FilesystemStorage{Root: dst}).RestoreFrom(&buf); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(b) != "hello" {
		t.Errorf("a.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "sub", "b.txt")); string(b) != "world" {
		t.Errorf("sub/b.txt = %q", b)
	}
}

func TestFilesystemStorage_WALNoop(t *testing.T) {
	frames, err := (FilesystemStorage{Root: t.TempDir()}).WALFrames()
	if err != nil || frames != nil {
		t.Errorf("WALFrames = %v, %v; want nil, nil", frames, err)
	}
}

func TestSafeJoin_RejectsTraversal(t *testing.T) {
	if _, err := safeJoin("/root", "../etc/passwd"); err == nil {
		t.Error("expected traversal rejection")
	}
	if got, err := safeJoin("/root", "ok/file"); err != nil || got != "/root/ok/file" {
		t.Errorf("safeJoin ok = %q, %v", got, err)
	}
}
