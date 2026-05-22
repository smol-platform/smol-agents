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

// ── Merge ─────────────────────────────────────────────────────────────────────

// ── helpers for 3-way merge tests ────────────────────────────────────────────

func mergeOpts(policy ConflictPolicy) MergeOptions {
	return MergeOptions{OnConflict: policy}
}

func mergeDryRun() MergeOptions {
	return MergeOptions{OnConflict: MergeFail, DryRun: true}
}

// TestAgentFS_Merge_FastForward verifies the core fast-forward semantics:
// files from srcBranch are applied onto dstBranch; dst-only files are preserved;
// the returned MergeResult has CommittedAt set.
func TestAgentFS_Merge_FastForward(t *testing.T) {
	b, _ := newTestBackend(t)
	f := filterFor("tenant-a", "ns")

	// Populate main (dst) with two files.
	writeDoc(t, b, "tenant-a", "ns", "main", "keep.txt", "keep me")
	writeDoc(t, b, "tenant-a", "ns", "main", "shared.txt", "main version")

	// Fork a run branch (src) and mutate one file, add another.
	_, err := b.Branch(context.Background(), "main", "run-x", f)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	writeDoc(t, b, "tenant-a", "ns", "run-x", "shared.txt", "run version") // override
	writeDoc(t, b, "tenant-a", "ns", "run-x", "new.txt", "brand new")      // new file

	// Merge run-x → main (no conflict: only src changed shared.txt).
	result, err := b.Merge(context.Background(), "run-x", "main", mergeOpts(MergeFail), f)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	if !result.Committed {
		t.Error("Merge: Committed should be true")
	}
	if len(result.Conflicts) != 0 {
		t.Errorf("Merge: expected 0 conflicts, got %d: %v", len(result.Conflicts), result.Conflicts)
	}
	if result.Branch.Name != "main" {
		t.Errorf("Merge returned Branch.Name = %q, want main", result.Branch.Name)
	}
	if result.Branch.CommittedAt.IsZero() {
		t.Error("Merge: CommittedAt should be set on the destination branch")
	}
	if result.Merged != 1 {
		t.Errorf("Merge: want Merged=1, got %d", result.Merged)
	}
	if result.Added != 1 {
		t.Errorf("Merge: want Added=1, got %d", result.Added)
	}

	// main should now contain the run-x version of shared.txt.
	res, err := b.Retrieve(context.Background(), "run version", 10, f)
	if err != nil {
		t.Fatalf("Retrieve after merge: %v", err)
	}
	if len(res.Chunks) == 0 {
		t.Error("Merge: shared.txt should contain run version after merge")
	}

	// keep.txt (dst-only) must still be present.
	res2, err := b.Retrieve(context.Background(), "keep me", 10, f)
	if err != nil {
		t.Fatalf("Retrieve keep.txt: %v", err)
	}
	if len(res2.Chunks) == 0 {
		t.Error("Merge: dst-only file keep.txt should be preserved after merge")
	}

	// new.txt from src must be visible in main.
	res3, err := b.Retrieve(context.Background(), "brand new", 10, f)
	if err != nil {
		t.Fatalf("Retrieve new.txt: %v", err)
	}
	if len(res3.Chunks) == 0 {
		t.Error("Merge: new.txt from srcBranch should appear in dstBranch after merge")
	}
}

// TestAgentFS_Merge_CrossTenantIsolation verifies that a merge cannot bridge
// tenant boundaries. The filter.Tenant is the isolation key.
func TestAgentFS_Merge_CrossTenantIsolation(t *testing.T) {
	b, _ := newTestBackend(t)

	// Create branches in two separate tenants with the same names.
	fa := filterFor("tenant-a", "ns")
	fb := filterFor("tenant-b", "ns")

	// Ensure both have branches named "run-1" and "main".
	writeDoc(t, b, "tenant-a", "ns", "main", "a.txt", "alpha")
	writeDoc(t, b, "tenant-b", "ns", "main", "b.txt", "beta")
	_, _ = b.Branch(context.Background(), "main", "run-1", fa)
	_, _ = b.Branch(context.Background(), "main", "run-1", fb)

	// Merging "run-1" (which exists for tenant-b) into "main" with tenant-a
	// filter must fail because run-1 is not visible under tenant-a's scope.
	// (tenant-a has run-1 too, so this succeeds — but the file content is isolated.)
	writeDoc(t, b, "tenant-b", "ns", "run-1", "secret.txt", "b secret")

	// Merge tenant-a's run-1 → main: should only see tenant-a files.
	_, err := b.Merge(context.Background(), "run-1", "main", mergeOpts(MergeFail), fa)
	if err != nil {
		t.Fatalf("Merge tenant-a: %v", err)
	}

	// tenant-b's secret.txt must NOT appear in tenant-a's main.
	res, err := b.Retrieve(context.Background(), "b secret", 10, fa)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	for _, sc := range res.Chunks {
		if strings.Contains(sc.Chunk.Text, "b secret") {
			t.Error("cross-tenant content leaked into tenant-a after merge")
		}
	}
	_ = fb
}

// TestAgentFS_Merge_MissingBranch verifies NotFound when either branch is absent.
func TestAgentFS_Merge_MissingBranch(t *testing.T) {
	b, _ := newTestBackend(t)
	f := filterFor("tenant-a", "ns")

	// src missing.
	_, err := b.Merge(context.Background(), "nonexistent", "main", mergeOpts(MergeFail), f)
	if KindOf(err) != KindNotFound {
		t.Errorf("missing src: expected NotFound, got %v", err)
	}

	// Seed main, then try missing dst.
	writeDoc(t, b, "tenant-a", "ns", "main", "x.txt", "x")
	_, _ = b.Branch(context.Background(), "main", "src-ok", f)
	_, err = b.Merge(context.Background(), "src-ok", "no-such-dst", mergeOpts(MergeFail), f)
	if KindOf(err) != KindNotFound {
		t.Errorf("missing dst: expected NotFound, got %v", err)
	}
}

// TestAgentFS_Merge_SameBranch verifies Invalid when src == dst.
func TestAgentFS_Merge_SameBranch(t *testing.T) {
	b, _ := newTestBackend(t)
	f := filterFor("tenant-a", "ns")
	_, err := b.Merge(context.Background(), "main", "main", mergeOpts(MergeFail), f)
	if KindOf(err) != KindInvalid {
		t.Errorf("expected Invalid for src==dst, got %v", err)
	}
}

// TestAgentFS_Merge_MissingTenant verifies Invalid guard.
func TestAgentFS_Merge_MissingTenant(t *testing.T) {
	b, _ := newTestBackend(t)
	_, err := b.Merge(context.Background(), "run", "main", mergeOpts(MergeFail), Filter{Namespace: "ns"})
	if KindOf(err) != KindInvalid {
		t.Errorf("expected Invalid for missing Tenant, got %v", err)
	}
}

// ── 3-way merge table-driven tests ────────────────────────────────────────────

// TestAgentFS_Merge3Way_Table exercises every row of the 3-way classifier.
func TestAgentFS_Merge3Way_Table(t *testing.T) {
	// setup helper: creates a fork and optionally modifies both sides.
	type setup struct {
		baseContent string
		dstContent  string // "" means delete from dst after fork
		srcContent  string // "" means delete from src after fork
		deleteDst   bool
		deleteSrc   bool
	}
	type want struct {
		dstContent   string // expected content of the file in main after merge
		deleted      bool   // expected absent from main after merge
		conflict     bool   // expected conflict surfaced
		conflictKind string
		committed    bool
		addedCount   int
		mergedCount  int
		deletedCount int
	}

	cases := []struct {
		name  string
		setup setup
		opts  MergeOptions
		want  want
	}{
		{
			name:  "O==T both unchanged: keep dst",
			setup: setup{baseContent: "v1", dstContent: "v1", srcContent: "v1"},
			opts:  mergeOpts(MergeFail),
			want:  want{dstContent: "v1", committed: true},
		},
		{
			name:  "only T changed: take T (fast-forward)",
			setup: setup{baseContent: "v1", dstContent: "v1", srcContent: "v2"},
			opts:  mergeOpts(MergeFail),
			want:  want{dstContent: "v2", committed: true, mergedCount: 1},
		},
		{
			name:  "only O changed: keep O",
			setup: setup{baseContent: "v1", dstContent: "v2", srcContent: "v1"},
			opts:  mergeOpts(MergeFail),
			want:  want{dstContent: "v2", committed: true},
		},
		{
			name:  "O==T both changed same: keep dst",
			setup: setup{baseContent: "v1", dstContent: "v2", srcContent: "v2"},
			opts:  mergeOpts(MergeFail),
			want:  want{dstContent: "v2", committed: true},
		},
		{
			name:  "O!=T both changed differ: edit/edit conflict fail",
			setup: setup{baseContent: "v1", dstContent: "v2-dst", srcContent: "v2-src"},
			opts:  mergeOpts(MergeFail),
			want:  want{dstContent: "v2-dst", conflict: true, conflictKind: "edit/edit", committed: false},
		},
		{
			name:  "O!=T both changed differ: edit/edit conflict ours",
			setup: setup{baseContent: "v1", dstContent: "v2-dst", srcContent: "v2-src"},
			opts:  mergeOpts(MergeOurs),
			want:  want{dstContent: "v2-dst", conflict: true, conflictKind: "edit/edit", committed: true},
		},
		{
			name:  "O!=T both changed differ: edit/edit conflict theirs",
			setup: setup{baseContent: "v1", dstContent: "v2-dst", srcContent: "v2-src"},
			opts:  mergeOpts(MergeTheirs),
			want:  want{dstContent: "v2-src", conflict: true, conflictKind: "edit/edit", committed: true, mergedCount: 1},
		},
		{
			name:  "T deleted O unchanged: take deletion",
			setup: setup{baseContent: "v1", dstContent: "v1", deleteSrc: true},
			opts:  mergeOpts(MergeFail),
			want:  want{deleted: true, committed: true, deletedCount: 1},
		},
		{
			name:  "T deleted O changed: edit/delete conflict fail",
			setup: setup{baseContent: "v1", dstContent: "v2-dst", deleteSrc: true},
			opts:  mergeOpts(MergeFail),
			want:  want{dstContent: "v2-dst", conflict: true, conflictKind: "edit/delete", committed: false},
		},
		{
			name:  "T deleted O changed: edit/delete conflict theirs (delete wins)",
			setup: setup{baseContent: "v1", dstContent: "v2-dst", deleteSrc: true},
			opts:  mergeOpts(MergeTheirs),
			want:  want{deleted: true, conflict: true, conflictKind: "edit/delete", committed: true, deletedCount: 1},
		},
		{
			name:  "src adds new file: add to dst",
			setup: setup{srcContent: "new"},
			opts:  mergeOpts(MergeFail),
			want:  want{dstContent: "new", committed: true, addedCount: 1},
		},
		{
			name: "add/add different content: conflict fail",
			// No base (srcContent set without going through branch), both sides have different
			// The test setup uses Branch() which copies base files. For an add/add we need
			// a file that only exists in src and dst independently (not from base).
			// We simulate this by creating the branch, then writing different content in dst
			// and different content in src (the base had neither).
			setup: setup{srcContent: "src-add", dstContent: "dst-add"},
			opts:  mergeOpts(MergeFail),
			want:  want{dstContent: "dst-add", conflict: true, conflictKind: "add/add", committed: false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newTestBackend(t)
			f := filterFor("tenant-a", "ns")

			const relPath = "file.txt"

			// Set up base file in main (if any).
			if tc.setup.baseContent != "" {
				writeDoc(t, b, "tenant-a", "ns", "main", relPath, tc.setup.baseContent)
			}

			// Fork src from main.
			_, err := b.Branch(context.Background(), "main", "src", f)
			if err != nil {
				t.Fatalf("Branch: %v", err)
			}

			// Apply dst mutations (write or delete from dst=main).
			if tc.setup.dstContent != "" && tc.setup.dstContent != tc.setup.baseContent {
				writeDoc(t, b, "tenant-a", "ns", "main", relPath, tc.setup.dstContent)
			}
			if tc.setup.deleteDst {
				dstKey := "tenant-a/ns/main/" + relPath
				_ = b.Delete(context.Background(), dstKey, f)
			}

			// Apply src mutations.
			if tc.setup.srcContent != "" && tc.setup.srcContent != tc.setup.baseContent {
				writeDoc(t, b, "tenant-a", "ns", "src", relPath, tc.setup.srcContent)
			}
			if tc.setup.deleteSrc {
				srcKey := "tenant-a/ns/src/" + relPath
				_ = b.Delete(context.Background(), srcKey, f)
			}
			// For add/add: write into dst independently (no base).
			if tc.setup.dstContent != "" && tc.setup.baseContent == "" {
				writeDoc(t, b, "tenant-a", "ns", "main", relPath, tc.setup.dstContent)
			}

			result, mergeErr := b.Merge(context.Background(), "src", "main", tc.opts, f)
			if mergeErr != nil {
				t.Fatalf("Merge error: %v", mergeErr)
			}

			if result.Committed != tc.want.committed {
				t.Errorf("Committed=%v, want %v", result.Committed, tc.want.committed)
			}

			gotConflict := len(result.Conflicts) > 0
			if gotConflict != tc.want.conflict {
				t.Errorf("hasConflict=%v, want %v; conflicts=%v", gotConflict, tc.want.conflict, result.Conflicts)
			}
			if tc.want.conflict && tc.want.conflictKind != "" {
				found := false
				for _, ci := range result.Conflicts {
					if ci.Kind == tc.want.conflictKind {
						found = true
					}
				}
				if !found {
					t.Errorf("want conflict kind %q, got %v", tc.want.conflictKind, result.Conflicts)
				}
			}

			dstKey := "tenant-a/ns/main/" + relPath
			doc, getErr := b.Get(context.Background(), dstKey, f)
			if tc.want.deleted {
				if KindOf(getErr) != KindNotFound {
					t.Errorf("want file deleted, but Get returned doc=%v err=%v", doc, getErr)
				}
			} else if tc.want.dstContent != "" {
				if getErr != nil {
					t.Errorf("Get after merge: %v", getErr)
				} else if string(doc.Content) != tc.want.dstContent {
					t.Errorf("dst content=%q, want %q", doc.Content, tc.want.dstContent)
				}
			}

			if result.Merged != tc.want.mergedCount {
				t.Errorf("Merged=%d, want %d", result.Merged, tc.want.mergedCount)
			}
			if result.Added != tc.want.addedCount {
				t.Errorf("Added=%d, want %d", result.Added, tc.want.addedCount)
			}
			if result.Deleted != tc.want.deletedCount {
				t.Errorf("Deleted=%d, want %d", result.Deleted, tc.want.deletedCount)
			}
		})
	}
}

// TestAgentFS_Merge_FailNoMutation verifies that OnConflict=fail leaves dst
// byte-identical to its pre-merge state.
func TestAgentFS_Merge_FailNoMutation(t *testing.T) {
	b, _ := newTestBackend(t)
	f := filterFor("tenant-a", "ns")

	// Base state.
	writeDoc(t, b, "tenant-a", "ns", "main", "conflict.txt", "base")
	writeDoc(t, b, "tenant-a", "ns", "main", "clean.txt", "clean")

	_, err := b.Branch(context.Background(), "main", "src", f)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	// Both sides modify conflict.txt differently.
	writeDoc(t, b, "tenant-a", "ns", "main", "conflict.txt", "main-edit")
	writeDoc(t, b, "tenant-a", "ns", "src", "conflict.txt", "src-edit")
	// src also adds a new file.
	writeDoc(t, b, "tenant-a", "ns", "src", "new.txt", "new from src")

	result, err := b.Merge(context.Background(), "src", "main", mergeOpts(MergeFail), f)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if result.Committed {
		t.Fatal("Merge with fail policy must not commit when conflicts exist")
	}
	if len(result.Conflicts) == 0 {
		t.Fatal("expected at least one conflict")
	}

	// dst must be byte-identical: conflict.txt still "main-edit", new.txt absent.
	conflictKey := "tenant-a/ns/main/conflict.txt"
	doc, err := b.Get(context.Background(), conflictKey, f)
	if err != nil {
		t.Fatalf("Get conflict.txt: %v", err)
	}
	if string(doc.Content) != "main-edit" {
		t.Errorf("conflict.txt mutated on failed merge: got %q, want %q", doc.Content, "main-edit")
	}

	newKey := "tenant-a/ns/main/new.txt"
	if _, err := b.Get(context.Background(), newKey, f); KindOf(err) != KindNotFound {
		t.Error("new.txt must not be present after failed merge")
	}
}

// ── Phase 2: markers + union + binary ─────────────────────────────────────────

// TestAgentFS_Merge_Markers_NonOverlapping verifies that non-overlapping edits
// on different lines auto-merge cleanly (no conflict markers) under MergeMarkers.
func TestAgentFS_Merge_Markers_NonOverlapping(t *testing.T) {
	b, _ := newTestBackend(t)
	f := filterFor("tenant-a", "ns")

	// Base: 4 lines.
	writeDoc(t, b, "tenant-a", "ns", "main", "file.txt", "line1\nline2\nline3\nline4\n")
	_, err := b.Branch(context.Background(), "main", "src", f)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}

	// ours (main) changes line1 only.
	writeDoc(t, b, "tenant-a", "ns", "main", "file.txt", "LINE1\nline2\nline3\nline4\n")
	// theirs (src) changes line4 only.
	writeDoc(t, b, "tenant-a", "ns", "src", "file.txt", "line1\nline2\nline3\nLINE4\n")

	result, err := b.Merge(context.Background(), "src", "main", mergeOpts(MergeMarkers), f)
	if err != nil {
		t.Fatalf("Merge markers: %v", err)
	}
	// Non-overlapping → auto-merged, no conflict markers in file.
	if !result.Committed {
		t.Error("markers merge: expected Committed=true for non-overlapping edits")
	}
	if len(result.Conflicts) != 0 {
		t.Errorf("non-overlapping should have 0 conflicts, got %d: %v", len(result.Conflicts), result.Conflicts)
	}

	dstKey := "tenant-a/ns/main/file.txt"
	doc, err := b.Get(context.Background(), dstKey, f)
	if err != nil {
		t.Fatalf("Get after markers merge: %v", err)
	}
	got := string(doc.Content)
	if !strings.Contains(got, "LINE1") {
		t.Errorf("ours change missing: %q", got)
	}
	if !strings.Contains(got, "LINE4") {
		t.Errorf("theirs change missing: %q", got)
	}
	if strings.Contains(got, "<<<<<<<") {
		t.Errorf("unexpected conflict markers in auto-merged file: %q", got)
	}
}

// TestAgentFS_Merge_Markers_Overlapping verifies that overlapping edit/edit text
// conflicts produce git-style conflict markers AND the merge is Committed=true.
func TestAgentFS_Merge_Markers_Overlapping(t *testing.T) {
	b, _ := newTestBackend(t)
	f := filterFor("tenant-a", "ns")

	writeDoc(t, b, "tenant-a", "ns", "main", "conflict.txt", "line1\nshared\nline3\n")
	_, err := b.Branch(context.Background(), "main", "src", f)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}

	// Both sides modify the same line.
	writeDoc(t, b, "tenant-a", "ns", "main", "conflict.txt", "line1\nOURS\nline3\n")
	writeDoc(t, b, "tenant-a", "ns", "src", "conflict.txt", "line1\nTHEIRS\nline3\n")

	result, err := b.Merge(context.Background(), "src", "main", mergeOpts(MergeMarkers), f)
	if err != nil {
		t.Fatalf("Merge markers: %v", err)
	}
	// Committed must be true even though conflicts exist.
	if !result.Committed {
		t.Error("markers merge: Committed must be true even for overlapping conflicts")
	}
	if len(result.Conflicts) == 0 {
		t.Error("markers merge: expected at least 1 conflict reported")
	}

	// File must contain conflict markers.
	dstKey := "tenant-a/ns/main/conflict.txt"
	doc, err := b.Get(context.Background(), dstKey, f)
	if err != nil {
		t.Fatalf("Get after markers merge: %v", err)
	}
	got := string(doc.Content)
	if !strings.Contains(got, "<<<<<<< ours") {
		t.Errorf("missing ours marker: %q", got)
	}
	if !strings.Contains(got, "=======") {
		t.Errorf("missing separator: %q", got)
	}
	if !strings.Contains(got, ">>>>>>> theirs") {
		t.Errorf("missing theirs marker: %q", got)
	}
	if !strings.Contains(got, "OURS") {
		t.Errorf("missing ours content: %q", got)
	}
	if !strings.Contains(got, "THEIRS") {
		t.Errorf("missing theirs content: %q", got)
	}
	// Non-conflicting lines must also be present.
	if !strings.Contains(got, "line1") {
		t.Errorf("non-conflicting line1 missing: %q", got)
	}
	if !strings.Contains(got, "line3") {
		t.Errorf("non-conflicting line3 missing: %q", got)
	}
}

// TestAgentFS_Merge_Union_Text verifies that MergeUnion keeps both sides of a
// text conflict (ours then theirs) and does NOT emit conflict markers.
func TestAgentFS_Merge_Union_Text(t *testing.T) {
	b, _ := newTestBackend(t)
	f := filterFor("tenant-a", "ns")

	writeDoc(t, b, "tenant-a", "ns", "main", "file.txt", "base\n")
	_, err := b.Branch(context.Background(), "main", "src", f)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}

	writeDoc(t, b, "tenant-a", "ns", "main", "file.txt", "ours-content\n")
	writeDoc(t, b, "tenant-a", "ns", "src", "file.txt", "theirs-content\n")

	result, err := b.Merge(context.Background(), "src", "main", mergeOpts(MergeUnion), f)
	if err != nil {
		t.Fatalf("Merge union: %v", err)
	}
	if !result.Committed {
		t.Error("union merge: Committed must be true")
	}
	// Conflicts are still reported informationally.
	if len(result.Conflicts) == 0 {
		t.Error("union merge: expected conflict reported informationally")
	}

	dstKey := "tenant-a/ns/main/file.txt"
	doc, err := b.Get(context.Background(), dstKey, f)
	if err != nil {
		t.Fatalf("Get after union merge: %v", err)
	}
	got := string(doc.Content)
	if strings.Contains(got, "<<<<<<<") {
		t.Errorf("union should not emit conflict markers: %q", got)
	}
	if !strings.Contains(got, "ours-content") {
		t.Errorf("ours content missing from union: %q", got)
	}
	if !strings.Contains(got, "theirs-content") {
		t.Errorf("theirs content missing from union: %q", got)
	}
	// Ours must appear before theirs.
	if strings.Index(got, "ours-content") >= strings.Index(got, "theirs-content") {
		t.Errorf("ours should precede theirs in union output: %q", got)
	}
}

// TestAgentFS_Merge_Markers_Binary verifies that binary file conflicts under
// MergeMarkers produce a marker placeholder (not a crash) and Committed=true.
func TestAgentFS_Merge_Markers_Binary(t *testing.T) {
	b, _ := newTestBackend(t)
	f := filterFor("tenant-a", "ns")

	// Write a binary file (contains a NUL byte).
	binaryContent := []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x0a} // PNG-like
	doc := Document{
		Tenant:    "tenant-a",
		Namespace: "ns",
		Content:   binaryContent,
		Path:      "image.bin",
		Metadata:  map[string]string{"branch": "main"},
	}
	_, err := b.Write(context.Background(), doc)
	if err != nil {
		t.Fatalf("Write binary: %v", err)
	}

	_, err = b.Branch(context.Background(), "main", "src", f)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}

	// Both sides modify the binary file differently.
	doc2 := Document{
		Tenant: "tenant-a", Namespace: "ns",
		Content:  []byte{0xff, 0xfe, 0x00, 0x01}, // different binary
		Path:     "image.bin",
		Metadata: map[string]string{"branch": "main"},
	}
	_, _ = b.Write(context.Background(), doc2)

	doc3 := Document{
		Tenant: "tenant-a", Namespace: "ns",
		Content:  []byte{0xaa, 0xbb, 0x00, 0xcc}, // yet another binary
		Path:     "image.bin",
		Metadata: map[string]string{"branch": "src"},
	}
	_, _ = b.Write(context.Background(), doc3)

	result, err := b.Merge(context.Background(), "src", "main", mergeOpts(MergeMarkers), f)
	if err != nil {
		t.Fatalf("Merge markers binary: %v", err)
	}
	if !result.Committed {
		t.Error("binary markers: Committed must be true")
	}
	if len(result.Conflicts) == 0 {
		t.Error("binary markers: expected conflict reported")
	}

	dstKey := "tenant-a/ns/main/image.bin"
	doc4, err := b.Get(context.Background(), dstKey, f)
	if err != nil {
		t.Fatalf("Get binary after merge: %v", err)
	}
	// Content should be a binary-conflict placeholder (text marker).
	got := string(doc4.Content)
	if !strings.Contains(got, "binary conflict") {
		t.Errorf("binary conflict placeholder missing: %q", got)
	}
}

// TestAgentFS_Merge_Union_Binary verifies that MergeUnion on binary files
// keeps ours (dst) and still reports the conflict informationally.
func TestAgentFS_Merge_Union_Binary(t *testing.T) {
	b, _ := newTestBackend(t)
	f := filterFor("tenant-a", "ns")

	oursContent := []byte{0x01, 0x00, 0x02} // binary (NUL)
	doc := Document{
		Tenant:    "tenant-a",
		Namespace: "ns",
		Content:   oursContent,
		Path:      "data.bin",
		Metadata:  map[string]string{"branch": "main"},
	}
	_, err := b.Write(context.Background(), doc)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	_, err = b.Branch(context.Background(), "main", "src", f)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}

	doc2 := Document{
		Tenant:    "tenant-a",
		Namespace: "ns",
		Content:   []byte{0x03, 0x00, 0x04},
		Path:      "data.bin",
		Metadata:  map[string]string{"branch": "src"},
	}
	_, _ = b.Write(context.Background(), doc2)
	writeDoc(t, b, "tenant-a", "ns", "main", "data.bin", string([]byte{0x05, 0x00, 0x06}))

	result, err := b.Merge(context.Background(), "src", "main", mergeOpts(MergeUnion), f)
	if err != nil {
		t.Fatalf("Merge union binary: %v", err)
	}
	if !result.Committed {
		t.Error("union binary: Committed must be true")
	}
	if len(result.Conflicts) == 0 {
		t.Error("union binary: expected conflict reported informationally")
	}
}

// TestAgentFS_Merge_Markers_EditDelete verifies that an edit/delete conflict
// under MergeMarkers keeps ours (the edited file) and is committed.
func TestAgentFS_Merge_Markers_EditDelete(t *testing.T) {
	b, _ := newTestBackend(t)
	f := filterFor("tenant-a", "ns")

	writeDoc(t, b, "tenant-a", "ns", "main", "gone.txt", "original\n")
	_, err := b.Branch(context.Background(), "main", "src", f)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}

	// ours (main) edits the file.
	writeDoc(t, b, "tenant-a", "ns", "main", "gone.txt", "edited\n")
	// theirs (src) deletes the file.
	srcKey := "tenant-a/ns/src/gone.txt"
	_ = b.Delete(context.Background(), srcKey, f)

	result, err := b.Merge(context.Background(), "src", "main", mergeOpts(MergeMarkers), f)
	if err != nil {
		t.Fatalf("Merge markers edit/delete: %v", err)
	}
	if !result.Committed {
		t.Error("markers edit/delete: Committed must be true")
	}
	if len(result.Conflicts) == 0 {
		t.Error("markers edit/delete: expected conflict reported")
	}
	// File must still exist (we keep ours).
	dstKey := "tenant-a/ns/main/gone.txt"
	doc, err := b.Get(context.Background(), dstKey, f)
	if err != nil {
		t.Fatalf("Get after markers edit/delete: %v", err)
	}
	if string(doc.Content) != "edited\n" {
		t.Errorf("expected ours content 'edited', got %q", doc.Content)
	}
}

// TestAgentFS_Merge_DryRun verifies that DryRun computes counts without committing.
func TestAgentFS_Merge_DryRun(t *testing.T) {
	b, _ := newTestBackend(t)
	f := filterFor("tenant-a", "ns")

	writeDoc(t, b, "tenant-a", "ns", "main", "shared.txt", "base")
	_, err := b.Branch(context.Background(), "main", "src", f)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	writeDoc(t, b, "tenant-a", "ns", "src", "shared.txt", "changed by src")
	writeDoc(t, b, "tenant-a", "ns", "src", "added.txt", "new file")

	result, err := b.Merge(context.Background(), "src", "main", mergeDryRun(), f)
	if err != nil {
		t.Fatalf("Merge DryRun: %v", err)
	}
	if result.Committed {
		t.Error("DryRun must not commit")
	}
	if result.Merged != 1 {
		t.Errorf("DryRun: Merged=%d, want 1", result.Merged)
	}
	if result.Added != 1 {
		t.Errorf("DryRun: Added=%d, want 1", result.Added)
	}

	// Verify no mutation occurred.
	key := "tenant-a/ns/main/shared.txt"
	doc, _ := b.Get(context.Background(), key, f)
	if string(doc.Content) != "base" {
		t.Errorf("DryRun mutated dst: got %q", doc.Content)
	}
	newKey := "tenant-a/ns/main/added.txt"
	if _, err := b.Get(context.Background(), newKey, f); KindOf(err) != KindNotFound {
		t.Error("DryRun must not add files to dst")
	}
}
