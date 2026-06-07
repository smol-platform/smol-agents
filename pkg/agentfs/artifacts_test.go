package agentfs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectArtifacts_GlobUploadOrdered(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "out/b.json", `{"b":1}`)
	writeFile(t, ws, "out/a.json", `{"a":1}`)
	writeFile(t, ws, "out/note.txt", "ignore me")

	s3 := NewFakeS3()
	m := CollectArtifacts(ws, []ArtifactRule{{Name: "json", Glob: "out/**/*.json"}}, s3, "artifacts/tenant-a/run-1")

	if m.State != ArtifactComplete {
		t.Fatalf("state = %s, want Complete (refs=%+v)", m.State, m.Refs)
	}
	if len(m.Refs) != 2 {
		t.Fatalf("want 2 refs (a.json, b.json), got %d: %+v", len(m.Refs), m.Refs)
	}
	// lexical order: a.json before b.json.
	if m.Refs[0].Path != "out/a.json" || m.Refs[1].Path != "out/b.json" {
		t.Errorf("refs not lexically ordered: %s, %s", m.Refs[0].Path, m.Refs[1].Path)
	}
	r0 := m.Refs[0]
	if r0.S3Key != "artifacts/tenant-a/run-1/json/out/a.json" {
		t.Errorf("tenant-scoped key wrong: %s", r0.S3Key)
	}
	if r0.SHA256 == "" || r0.SizeBytes == 0 || r0.S3VersionID == "" {
		t.Errorf("ref missing sha256/size/version: %+v", r0)
	}
	// content actually uploaded.
	rc, err := s3.Get(r0.S3Key, "")
	if err != nil {
		t.Fatalf("get uploaded object: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != `{"a":1}` {
		t.Errorf("uploaded content = %q", body)
	}
}

func TestCollectArtifacts_OverBudgetPartial(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "big.bin", strings.Repeat("x", 100))
	writeFile(t, ws, "small.txt", "ok")

	s3 := NewFakeS3()
	m := CollectArtifacts(ws, []ArtifactRule{
		{Name: "big", Glob: "big.bin", MaxBytes: 10},
		{Name: "small", Glob: "small.txt"},
	}, s3, "p")
	if m.State != ArtifactPartial {
		t.Fatalf("state = %s, want Partial", m.State)
	}
	var bigRef, smallRef *ArtifactRef
	for i := range m.Refs {
		switch m.Refs[i].Name {
		case "big":
			bigRef = &m.Refs[i]
		case "small":
			smallRef = &m.Refs[i]
		}
	}
	if bigRef == nil || bigRef.Skipped != "over-budget" {
		t.Errorf("big.bin should be skipped over-budget: %+v", bigRef)
	}
	if smallRef == nil || smallRef.Skipped != "" || smallRef.S3VersionID == "" {
		t.Errorf("small.txt should upload: %+v", smallRef)
	}
}

func TestCollectArtifacts_TraversalRejected(t *testing.T) {
	ws := t.TempDir()
	m := CollectArtifacts(ws, []ArtifactRule{{Name: "esc", Glob: "../escape/*"}}, NewFakeS3(), "p")
	if len(m.Refs) != 1 || m.Refs[0].Skipped == "" {
		t.Fatalf("traversal glob must be skipped: %+v", m.Refs)
	}
}

type failS3 struct{ *FakeS3 }

func (failS3) Put(string, io.Reader, PutMeta) (Version, error) {
	return Version{}, errors.New("s3 down")
}

func TestCollectArtifacts_TotalOutageFailed(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.json", `{}`)
	m := CollectArtifacts(ws, []ArtifactRule{{Name: "j", Glob: "a.json"}}, failS3{NewFakeS3()}, "p")
	if m.State != ArtifactFailed {
		t.Fatalf("total upload outage must be Failed, got %s", m.State)
	}
}
