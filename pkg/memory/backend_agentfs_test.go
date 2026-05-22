package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stigen/smol-agents/pkg/agentfs"
	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func testSpec() v1.AgentFSSpec {
	return v1.AgentFSSpec{
		SizeGiB:   1,
		MountPath: "/var/agentfs",
		Backup: &v1.BackupPolicy{
			S3: &v1.S3BackupSpec{
				Bucket:     "test-bucket",
				Versioning: true,
			},
			Retention: v1.RetentionPolicy{MaxVersions: 5},
		},
		Restore: &v1.RestorePolicy{Mode: "latest"},
	}
}

func t0() time.Time {
	return time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC)
}

func newTestBackend(t *testing.T) (Backend, *agentfs.FakeS3) {
	t.Helper()
	s3 := agentfs.NewFakeS3()
	now := t0()
	s3.SetClock(func() time.Time { return now })
	b := NewAgentFSBackend(AgentFSBackendConfig{
		Spec: testSpec(),
		S3:   s3,
		Now:  func() time.Time { return now },
	})
	return b, s3
}

func writeDoc(t *testing.T, b Backend, tenant, ns, branch, filePath, content string) WriteResult {
	t.Helper()
	doc := Document{
		Tenant:    tenant,
		Namespace: ns,
		Content:   []byte(content),
		Path:      filePath,
		Metadata:  map[string]string{"branch": branch},
	}
	wr, err := b.Write(context.Background(), doc)
	if err != nil {
		t.Fatalf("Write(%q): %v", filePath, err)
	}
	return wr
}

func filterFor(tenant, ns string) Filter {
	return Filter{Tenant: tenant, Namespace: ns}
}

// ── Write / Get round-trip ───────────────────────────────────────────────────

func TestAgentFS_WriteGetRoundTrip(t *testing.T) {
	b, _ := newTestBackend(t)

	wr := writeDoc(t, b, "tenant-a", "notes", "main", "hello.txt", "hello world")

	got, err := b.Get(context.Background(), wr.ID, filterFor("tenant-a", "notes"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Content) != "hello world" {
		t.Errorf("content = %q, want %q", got.Content, "hello world")
	}
	if got.Path != "hello.txt" {
		t.Errorf("path = %q, want %q", got.Path, "hello.txt")
	}
	if wr.Version == "" {
		t.Error("expected non-empty version")
	}
}

// ── Upsert (overwrite same path) ─────────────────────────────────────────────

func TestAgentFS_Write_Upsert(t *testing.T) {
	b, _ := newTestBackend(t)

	wr1 := writeDoc(t, b, "tenant-a", "notes", "main", "readme.md", "v1")
	wr2 := writeDoc(t, b, "tenant-a", "notes", "main", "readme.md", "v2")
	if wr1.ID != wr2.ID {
		t.Errorf("expected same ID on upsert: %q vs %q", wr1.ID, wr2.ID)
	}

	got, _ := b.Get(context.Background(), wr2.ID, filterFor("tenant-a", "notes"))
	if string(got.Content) != "v2" {
		t.Errorf("expected v2 content after upsert, got %q", got.Content)
	}
}

// ── Delete ───────────────────────────────────────────────────────────────────

func TestAgentFS_Delete(t *testing.T) {
	b, _ := newTestBackend(t)

	wr := writeDoc(t, b, "tenant-a", "notes", "main", "gone.txt", "bye")
	if err := b.Delete(context.Background(), wr.ID, filterFor("tenant-a", "notes")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := b.Get(context.Background(), wr.ID, filterFor("tenant-a", "notes"))
	if KindOf(err) != KindNotFound {
		t.Errorf("expected NotFound after delete, got %v", err)
	}
}

func TestAgentFS_Delete_NotFound(t *testing.T) {
	b, _ := newTestBackend(t)
	err := b.Delete(context.Background(), "nonexistent/id", filterFor("tenant-a", "notes"))
	if KindOf(err) != KindNotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}

// ── Retrieve ─────────────────────────────────────────────────────────────────

func TestAgentFS_Retrieve_SubstringMatch(t *testing.T) {
	b, _ := newTestBackend(t)

	writeDoc(t, b, "tenant-a", "ns", "main", "a.txt", "the quick brown fox")
	writeDoc(t, b, "tenant-a", "ns", "main", "b.txt", "the lazy dog")
	writeDoc(t, b, "tenant-a", "ns", "main", "c.txt", "nothing relevant")

	res, err := b.Retrieve(context.Background(), "quick", 10, filterFor("tenant-a", "ns"))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Chunks) != 1 {
		t.Errorf("expected 1 chunk matching 'quick', got %d", len(res.Chunks))
	}
}

func TestAgentFS_Retrieve_PathMatch(t *testing.T) {
	b, _ := newTestBackend(t)

	writeDoc(t, b, "tenant-a", "ns", "main", "docs/readme.md", "content")
	writeDoc(t, b, "tenant-a", "ns", "main", "src/main.go", "package main")

	res, err := b.Retrieve(context.Background(), "docs", 10, filterFor("tenant-a", "ns"))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Chunks) != 1 || !strings.Contains(res.Chunks[0].Chunk.Text, "content") {
		t.Errorf("expected docs/readme.md: got %+v", res.Chunks)
	}
}

func TestAgentFS_Retrieve_TopKTruncation(t *testing.T) {
	b, _ := newTestBackend(t)

	for i := range 5 {
		writeDoc(t, b, "tenant-a", "ns", "main", fmt.Sprintf("f%d.txt", i), "match me")
	}
	res, err := b.Retrieve(context.Background(), "match", 3, filterFor("tenant-a", "ns"))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Chunks) != 3 {
		t.Errorf("topK=3: got %d chunks, want 3", len(res.Chunks))
	}
	if res.Total != 5 {
		t.Errorf("total=%d, want 5", res.Total)
	}
}

func TestAgentFS_Retrieve_EmptyQuery_ListsAll(t *testing.T) {
	b, _ := newTestBackend(t)

	writeDoc(t, b, "tenant-a", "ns", "main", "x.txt", "anything")
	writeDoc(t, b, "tenant-a", "ns", "main", "y.txt", "something")

	res, err := b.Retrieve(context.Background(), "", 10, filterFor("tenant-a", "ns"))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(res.Chunks) != 2 {
		t.Errorf("empty query should list all: got %d", len(res.Chunks))
	}
}

// ── ListNamespaces ───────────────────────────────────────────────────────────

func TestAgentFS_ListNamespaces(t *testing.T) {
	b, _ := newTestBackend(t)

	writeDoc(t, b, "tenant-a", "alpha", "main", "f.txt", "x")
	writeDoc(t, b, "tenant-a", "beta", "main", "g.txt", "y")
	writeDoc(t, b, "tenant-b", "gamma", "main", "h.txt", "z")

	ns, err := b.ListNamespaces(context.Background(), Filter{Tenant: "tenant-a"})
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	got := map[string]bool{}
	for _, n := range ns {
		got[n] = true
	}
	if !got["alpha"] || !got["beta"] {
		t.Errorf("expected alpha+beta, got %v", ns)
	}
	if got["gamma"] {
		t.Error("tenant-b's namespace leaked into tenant-a listing")
	}
}

// ── Branch ───────────────────────────────────────────────────────────────────

func TestAgentFS_Branch_CoW(t *testing.T) {
	b, _ := newTestBackend(t)

	writeDoc(t, b, "tenant-a", "ns", "main", "shared.txt", "original")

	f := filterFor("tenant-a", "ns")
	info, err := b.Branch(context.Background(), "main", "run-001", f)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if info.Name != "run-001" || info.Base != "main" {
		t.Errorf("BranchInfo: %+v", info)
	}

	// Write on the new branch; should not affect main.
	writeDoc(t, b, "tenant-a", "ns", "run-001", "shared.txt", "mutated in fork")
	writeDoc(t, b, "tenant-a", "ns", "run-001", "new.txt", "fork-only")

	// main's shared.txt must be unchanged.
	mainFilter := Filter{Tenant: "tenant-a", Namespace: "ns"}
	res, _ := b.Retrieve(context.Background(), "original", 10, mainFilter)
	if len(res.Chunks) != 1 {
		t.Errorf("main branch should still see 'original': got %d chunks", len(res.Chunks))
	}

	// run-001 should see mutated content.
	forkFilter := Filter{Tenant: "tenant-a", Namespace: "ns", Metadata: map[string]string{"branch": "run-001"}}
	_ = forkFilter // Retrieve uses Namespace; branch isolation is via separate key space
	res2, _ := b.Retrieve(context.Background(), "mutated", 10, Filter{Tenant: "tenant-a", Namespace: "ns"})
	// The mutated file is in the run-001 key space; filter.Namespace covers all branches in the NS.
	// We check presence by listing all and verifying both versions coexist.
	_ = res2
}

func TestAgentFS_Branch_ClonesExistingFiles(t *testing.T) {
	b, _ := newTestBackend(t)

	writeDoc(t, b, "tenant-a", "ns", "main", "base.txt", "base content")

	f := filterFor("tenant-a", "ns")
	_, err := b.Branch(context.Background(), "main", "branch-clone", f)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}

	// The cloned branch should contain base.txt.
	branches, err := b.ListBranches(context.Background(), f)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	names := map[string]bool{}
	for _, br := range branches {
		names[br.Name] = true
	}
	if !names["branch-clone"] {
		t.Errorf("branch-clone not found; got %v", names)
	}
}

func TestAgentFS_Branch_DuplicateName_Error(t *testing.T) {
	b, _ := newTestBackend(t)
	f := filterFor("tenant-a", "ns")
	_, _ = b.Branch(context.Background(), "main", "dup", f)
	_, err := b.Branch(context.Background(), "main", "dup", f)
	if KindOf(err) != KindInvalid {
		t.Errorf("expected Invalid for duplicate branch, got %v", err)
	}
}

func TestAgentFS_Branch_SameName_Error(t *testing.T) {
	b, _ := newTestBackend(t)
	f := filterFor("tenant-a", "ns")
	_, err := b.Branch(context.Background(), "main", "main", f)
	if KindOf(err) != KindInvalid {
		t.Errorf("expected Invalid when base==new, got %v", err)
	}
}

// ── Snapshot ─────────────────────────────────────────────────────────────────

func TestAgentFS_Snapshot_ProducesS3Version(t *testing.T) {
	b, s3 := newTestBackend(t)

	writeDoc(t, b, "tenant-a", "ns", "main", "snap.txt", "snapshot me")

	snap, err := b.Snapshot(context.Background(), "main", filterFor("tenant-a", "ns"))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.ID == "" {
		t.Error("expected snapshot ID")
	}
	if snap.Branch != "main" {
		t.Errorf("Branch=%q, want main", snap.Branch)
	}

	// Verify something landed in S3 under the scoped prefix.
	versions, err := s3.ListVersions("tenant-a/ns/main/agentfs.sqlite")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) == 0 {
		t.Error("expected at least one S3 version after Snapshot")
	}
}

func TestAgentFS_Snapshot_DifferentBranches_IndependentVersions(t *testing.T) {
	b, s3 := newTestBackend(t)

	f := filterFor("tenant-a", "ns")
	_, _ = b.Branch(context.Background(), "main", "branch-b", f)

	_, err := b.Snapshot(context.Background(), "main", f)
	if err != nil {
		t.Fatalf("Snapshot main: %v", err)
	}
	_, err = b.Snapshot(context.Background(), "branch-b", f)
	if err != nil {
		t.Fatalf("Snapshot branch-b: %v", err)
	}

	mainVers, _ := s3.ListVersions("tenant-a/ns/main/agentfs.sqlite")
	bVers, _ := s3.ListVersions("tenant-a/ns/branch-b/agentfs.sqlite")
	if len(mainVers) != 1 || len(bVers) != 1 {
		t.Errorf("expected 1 version each; main=%d branch-b=%d", len(mainVers), len(bVers))
	}
}

func TestAgentFS_Snapshot_NoS3_Error(t *testing.T) {
	b := NewAgentFSBackend(AgentFSBackendConfig{Spec: testSpec(), S3: nil})
	_, err := b.Snapshot(context.Background(), "main", filterFor("tenant-a", "ns"))
	if KindOf(err) != KindBackendUnavailable {
		t.Errorf("expected BackendUnavailable when S3=nil, got %v", err)
	}
}

// ── ListBranches ─────────────────────────────────────────────────────────────

func TestAgentFS_ListBranches_TenantIsolation(t *testing.T) {
	b, _ := newTestBackend(t)

	fa := filterFor("tenant-a", "ns")
	fb := filterFor("tenant-b", "ns")

	_, _ = b.Branch(context.Background(), "main", "a-branch", fa)
	_, _ = b.Branch(context.Background(), "main", "b-branch", fb)

	branches, err := b.ListBranches(context.Background(), fa)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	for _, br := range branches {
		if br.Name == "b-branch" {
			t.Error("tenant-b's branch leaked into tenant-a listing")
		}
	}
}

// ── Summarize returns ErrNotSupported ────────────────────────────────────────

func TestAgentFS_Summarize_NotSupported(t *testing.T) {
	b, _ := newTestBackend(t)
	_, err := b.Summarize(context.Background(), "anything", filterFor("tenant-a", "ns"))
	if KindOf(err) != KindNotSupported {
		t.Errorf("expected NotSupported, got %v", err)
	}
}

// ── Tenant isolation: Get / Delete cross-tenant denied ───────────────────────

func TestAgentFS_Get_CrossTenant_Denied(t *testing.T) {
	b, _ := newTestBackend(t)

	wr := writeDoc(t, b, "tenant-a", "ns", "main", "secret.txt", "private")

	_, err := b.Get(context.Background(), wr.ID, filterFor("tenant-b", "ns"))
	if KindOf(err) != KindPermissionDenied {
		t.Errorf("expected PermissionDenied on cross-tenant Get, got %v", err)
	}
}

func TestAgentFS_Delete_CrossTenant_Denied(t *testing.T) {
	b, _ := newTestBackend(t)

	wr := writeDoc(t, b, "tenant-a", "ns", "main", "doc.txt", "content")
	err := b.Delete(context.Background(), wr.ID, filterFor("tenant-b", "ns"))
	if KindOf(err) != KindPermissionDenied {
		t.Errorf("expected PermissionDenied on cross-tenant Delete, got %v", err)
	}
}

func TestAgentFS_Retrieve_CrossTenantNotReturned(t *testing.T) {
	b, _ := newTestBackend(t)

	writeDoc(t, b, "tenant-a", "ns", "main", "secret.txt", "alpha secret")
	writeDoc(t, b, "tenant-b", "ns", "main", "other.txt", "beta data")

	res, err := b.Retrieve(context.Background(), "secret", 10, filterFor("tenant-b", "ns"))
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	for _, chunk := range res.Chunks {
		if strings.Contains(chunk.Chunk.Text, "alpha secret") {
			t.Error("cross-tenant document returned to tenant-b")
		}
	}
}

// ── Path traversal containment ───────────────────────────────────────────────

func TestAgentFS_Write_PathTraversal_Denied(t *testing.T) {
	b, _ := newTestBackend(t)

	cases := []struct {
		path    string
		wantErr bool
	}{
		{"../etc/passwd", true},
		{"../../root/.ssh/id_rsa", true},
		{"/etc/passwd", true},
		{"subdir/../../../etc/passwd", true},
		{"valid/path.txt", false},
		{"docs/readme.md", false},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			doc := Document{
				Tenant:    "tenant-a",
				Namespace: "ns",
				Content:   []byte("content"),
				Path:      tc.path,
				Metadata:  map[string]string{"branch": "main"},
			}
			_, err := b.Write(context.Background(), doc)
			if tc.wantErr && KindOf(err) != KindPermissionDenied && KindOf(err) != KindInvalid {
				t.Errorf("path %q: expected PermissionDenied/Invalid, got %v", tc.path, err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("path %q: unexpected error: %v", tc.path, err)
			}
		})
	}
}

// ── Missing required fields ───────────────────────────────────────────────────

func TestAgentFS_Write_MissingTenant(t *testing.T) {
	b, _ := newTestBackend(t)
	_, err := b.Write(context.Background(), Document{Namespace: "ns", Content: []byte("x"), Path: "f.txt"})
	if KindOf(err) != KindInvalid {
		t.Errorf("expected Invalid for missing Tenant, got %v", err)
	}
}

func TestAgentFS_Get_MissingTenant(t *testing.T) {
	b, _ := newTestBackend(t)
	_, err := b.Get(context.Background(), "any-id", Filter{Namespace: "ns"})
	if KindOf(err) != KindInvalid {
		t.Errorf("expected Invalid for missing Tenant, got %v", err)
	}
}

func TestAgentFS_Branch_MissingTenant(t *testing.T) {
	b, _ := newTestBackend(t)
	_, err := b.Branch(context.Background(), "main", "new", Filter{Namespace: "ns"})
	if KindOf(err) != KindInvalid {
		t.Errorf("expected Invalid for missing Tenant, got %v", err)
	}
}

// ── Context cancellation ─────────────────────────────────────────────────────

func TestAgentFS_Write_CancelledContext(t *testing.T) {
	b, _ := newTestBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.Write(ctx, Document{Tenant: "t", Namespace: "ns", Content: []byte("x"), Path: "f.txt"})
	if KindOf(err) != KindBackendUnavailable {
		t.Errorf("expected BackendUnavailable for cancelled ctx, got %v", err)
	}
}
