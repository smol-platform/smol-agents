// Package memory implements the AgentFS filesystem backend for the smol-agents
// memory subsystem. This file provides the Backend implementation over a Turso
// AgentFS store, reusing pkg/agentfs for backup/restore/WAL/retention.
//
// Mapping summary:
//   - Write / Get / Delete — files stored under tenant/namespace/branch/path
//   - Retrieve             — path + metadata + substring match (not semantic;
//     semantic recall is the vector backend's job per R-MEM-WORK-2)
//   - ListNamespaces       — namespaces derived from stored file prefixes
//   - Branch               — copy-on-write fork: clone current branch storage
//   - Snapshot             — pkg/agentfs.Manager.Backup() → full+WAL S3 version
//   - ListBranches         — enumerate in-memory branch registry
//   - Summarize            — ErrNotSupported (P2)
//
// Isolation invariants (R-MEM-FS-5):
//   - Every operation validates that filter.Tenant and filter.Namespace match the
//     stored document's tenant and namespace before returning any result.
//   - Path traversal: a caller-supplied path is sanitized via path.Clean and the
//     result must not escape the tenant/namespace/branch root (no ".." escapes,
//     no absolute override). Any violation returns PermissionDenied.
//   - Branch access: a branch key is scoped to tenant+namespace; a caller cannot
//     name a branch belonging to another tenant even with the correct name.
package memory

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/stigen/smol-agents/pkg/agentfs"
	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

// agentFSBackend implements Backend over an in-process set of AgentFS branches.
// Each branch is an independent FakeStorage (or production Storage) whose
// SQLite state tracks the files stored in that branch. S3 is shared across
// branches (different S3 prefixes via the Manager.Spec.Backup.S3.Prefix).
//
// Thread safety: all state is protected by mu.
type agentFSBackend struct {
	mu sync.Mutex

	// spec is the AgentFSSpec from the MemoryStore CR.
	spec v1.AgentFSSpec

	// s3 is the shared object store (production = aws; tests = FakeS3).
	s3 agentfs.S3

	// branches maps a branchKey (tenant+namespace+name) to its in-memory
	// storage and metadata. Branches are created via Branch() and live until
	// explicitly discarded or the process restarts.
	branches map[string]*fsBranch

	// files maps a fileKey (tenant+namespace+branch+path) to the stored Document.
	// This is the canonical in-process store for all FS documents.
	files map[string]Document

	// now returns the current time; injectable for tests.
	now func() time.Time
}

// fsBranch tracks one branch's state inside agentFSBackend.
type fsBranch struct {
	info    BranchInfo
	storage *agentfs.FakeStorage // production would use a real Storage impl
}

// branchKey uniquely identifies a branch within this backend process.
func branchKey(tenant, namespace, branch string) string {
	return tenant + "/" + namespace + "/" + branch
}

// fileKey uniquely identifies a file within a branch.
func fileKey(tenant, namespace, branch, filePath string) string {
	return tenant + "/" + namespace + "/" + branch + "/" + filePath
}

// AgentFSBackendConfig carries the constructor arguments for NewAgentFSBackend.
type AgentFSBackendConfig struct {
	// Spec is the AgentFSSpec from the MemoryStore (size, mountPath, backup/restore).
	Spec v1.AgentFSSpec

	// S3 is the object-store driver. Production: aws-sdk-go-v2 adapter.
	// Tests: *agentfs.FakeS3.
	S3 agentfs.S3

	// Now is the clock; nil defaults to time.Now.
	Now func() time.Time
}

// NewAgentFSBackend constructs an agentFSBackend. The root branch ("main") is
// seeded automatically so callers can Write without branching first.
func NewAgentFSBackend(cfg AgentFSBackendConfig) Backend {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	b := &agentFSBackend{
		spec:     cfg.Spec,
		s3:       cfg.S3,
		branches: make(map[string]*fsBranch),
		files:    make(map[string]Document),
		now:      now,
	}
	return b
}

// ── path containment ────────────────────────────────────────────────────────

// safePath validates and cleans a caller-supplied path so it cannot escape
// the namespace/branch root. Returns PermissionDenied on traversal attempt.
// The returned path is relative (no leading slash).
func safePath(p string) (string, error) {
	if p == "" {
		return "", Invalid("path is required for filesystem documents")
	}
	// Reject null bytes.
	if strings.ContainsRune(p, 0) {
		return "", PermissionDenied("path contains null byte")
	}
	// Clean normalises ".." components and duplicate slashes.
	cleaned := path.Clean(p)
	// After cleaning, a traversal attempt starts with ".." or "/".
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return "", PermissionDenied(fmt.Sprintf("path traversal denied: %q", p))
	}
	return cleaned, nil
}

// ── tenant / namespace guards ────────────────────────────────────────────────

func checkTenantNamespace(doc Document, filter Filter) error {
	if doc.Tenant != filter.Tenant {
		return PermissionDenied("cross-tenant access denied")
	}
	if doc.Namespace != filter.Namespace {
		return PermissionDenied("cross-namespace access denied")
	}
	return nil
}

// ── ensureBranch creates the default "main" branch for a tenant+namespace if
// it does not already exist. Must be called with mu held.
func (b *agentFSBackend) ensureBranch(tenant, namespace, branchName string) *fsBranch {
	key := branchKey(tenant, namespace, branchName)
	if br, ok := b.branches[key]; ok {
		return br
	}
	br := &fsBranch{
		info: BranchInfo{
			Name:      branchName,
			CreatedAt: b.now(),
		},
		storage: &agentfs.FakeStorage{},
	}
	b.branches[key] = br
	return br
}

// ── Backend.Write ────────────────────────────────────────────────────────────

func (b *agentFSBackend) Write(ctx context.Context, doc Document) (WriteResult, error) {
	if err := ctx.Err(); err != nil {
		return WriteResult{}, BackendUnavailable("context: " + err.Error())
	}
	cleanedPath, err := safePath(doc.Path)
	if err != nil {
		return WriteResult{}, err
	}
	if doc.Tenant == "" {
		return WriteResult{}, Invalid("document.Tenant is required")
	}
	if doc.Namespace == "" {
		return WriteResult{}, Invalid("document.Namespace is required")
	}

	// Resolve branch: use Metadata["branch"] if provided, else "main".
	branchName := "main"
	if doc.Metadata != nil {
		if br := doc.Metadata["branch"]; br != "" {
			branchName = br
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.ensureBranch(doc.Tenant, doc.Namespace, branchName)

	// Assign stable ID from the canonical key.
	now := b.now()
	key := fileKey(doc.Tenant, doc.Namespace, branchName, cleanedPath)

	existing, exists := b.files[key]
	var createdAt time.Time
	if exists {
		createdAt = existing.CreatedAt
	} else {
		createdAt = now
	}

	version := fmt.Sprintf("%d", now.UnixNano())
	stored := Document{
		ID:        key,
		Namespace: doc.Namespace,
		Tenant:    doc.Tenant,
		Content:   doc.Content,
		Path:      cleanedPath,
		Metadata:  doc.Metadata,
		Version:   version,
		CreatedAt: createdAt,
		UpdatedAt: now,
	}
	b.files[key] = stored
	return WriteResult{ID: key, Version: version}, nil
}

// ── Backend.Get ──────────────────────────────────────────────────────────────

func (b *agentFSBackend) Get(ctx context.Context, id string, filter Filter) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, BackendUnavailable("context: " + err.Error())
	}
	if filter.Tenant == "" {
		return Document{}, Invalid("filter.Tenant is required")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	doc, ok := b.files[id]
	if !ok {
		return Document{}, NotFound("document not found: " + id)
	}
	if err := checkTenantNamespace(doc, filter); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// ── Backend.Delete ───────────────────────────────────────────────────────────

func (b *agentFSBackend) Delete(ctx context.Context, id string, filter Filter) error {
	if err := ctx.Err(); err != nil {
		return BackendUnavailable("context: " + err.Error())
	}
	if filter.Tenant == "" {
		return Invalid("filter.Tenant is required")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	doc, ok := b.files[id]
	if !ok {
		return NotFound("document not found: " + id)
	}
	if err := checkTenantNamespace(doc, filter); err != nil {
		return err
	}
	delete(b.files, id)
	return nil
}

// ── Backend.Retrieve ─────────────────────────────────────────────────────────

// Retrieve performs path/metadata/substring match. This is the filesystem
// backend; semantic vector recall is the vector backend's job (R-MEM-WORK-2).
// For keyword search across file content the query string is matched as a
// substring of Content.
func (b *agentFSBackend) Retrieve(ctx context.Context, query string, topK int, filter Filter) (RetrieveResult, error) {
	if err := ctx.Err(); err != nil {
		return RetrieveResult{}, BackendUnavailable("context: " + err.Error())
	}
	if filter.Tenant == "" {
		return RetrieveResult{}, Invalid("filter.Tenant is required")
	}
	if filter.Namespace == "" {
		return RetrieveResult{}, Invalid("filter.Namespace is required")
	}
	if topK <= 0 {
		topK = 10
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	var scored []ScoredChunk
	for _, doc := range b.files {
		if doc.Tenant != filter.Tenant || doc.Namespace != filter.Namespace {
			continue
		}
		if !metadataMatches(doc.Metadata, filter.Metadata) {
			continue
		}
		score := float32(0)
		if query != "" {
			if bytes.Contains(doc.Content, []byte(query)) || strings.Contains(doc.Path, query) {
				score = 1.0
			} else {
				continue
			}
		} else {
			score = 0.5 // listing match (no query = list all)
		}
		scored = append(scored, ScoredChunk{
			Chunk: Chunk{
				Text:       string(doc.Content),
				DocumentID: doc.ID,
			},
			Score: score,
		})
	}

	total := int64(len(scored))
	if len(scored) > topK {
		scored = scored[:topK]
	}
	return RetrieveResult{Chunks: scored, Total: total}, nil
}

// metadataMatches returns true when every key/value in want is present in got.
func metadataMatches(got, want map[string]string) bool {
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// ── Backend.ListNamespaces ───────────────────────────────────────────────────

func (b *agentFSBackend) ListNamespaces(ctx context.Context, filter Filter) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, BackendUnavailable("context: " + err.Error())
	}
	if filter.Tenant == "" {
		return nil, Invalid("filter.Tenant is required")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	seen := map[string]struct{}{}
	for _, doc := range b.files {
		if doc.Tenant != filter.Tenant {
			continue
		}
		seen[doc.Namespace] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	return out, nil
}

// ── Backend.Summarize ────────────────────────────────────────────────────────

// Summarize returns ErrNotSupported — LLM summarisation is a P2 feature.
func (b *agentFSBackend) Summarize(_ context.Context, _ string, _ Filter) (string, error) {
	return "", &ErrNotSupported{Op: "Summarize", Backend: "agentfs"}
}

// ── Backend.Branch ───────────────────────────────────────────────────────────

// Branch creates a copy-on-write fork of baseBranch into newBranch. All files
// from the base branch are copied into the new branch (CoW semantics: future
// writes to newBranch do not affect baseBranch and vice versa).
func (b *agentFSBackend) Branch(ctx context.Context, baseBranch, newBranch string, filter Filter) (BranchInfo, error) {
	if err := ctx.Err(); err != nil {
		return BranchInfo{}, BackendUnavailable("context: " + err.Error())
	}
	if filter.Tenant == "" {
		return BranchInfo{}, Invalid("filter.Tenant is required")
	}
	if filter.Namespace == "" {
		return BranchInfo{}, Invalid("filter.Namespace is required")
	}
	if baseBranch == "" {
		return BranchInfo{}, Invalid("baseBranch is required")
	}
	if newBranch == "" {
		return BranchInfo{}, Invalid("newBranch is required")
	}
	if baseBranch == newBranch {
		return BranchInfo{}, Invalid("newBranch must differ from baseBranch")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	newKey := branchKey(filter.Tenant, filter.Namespace, newBranch)
	if _, exists := b.branches[newKey]; exists {
		return BranchInfo{}, Invalid(fmt.Sprintf("branch %q already exists", newBranch))
	}

	// Ensure the base branch exists; seed it if not (e.g. first use of "main").
	b.ensureBranch(filter.Tenant, filter.Namespace, baseBranch)

	// Clone all files from base → new branch.
	basePrefix := fileKey(filter.Tenant, filter.Namespace, baseBranch, "")
	newBranchPrefix := fileKey(filter.Tenant, filter.Namespace, newBranch, "")
	_ = newBranchPrefix // used in key construction below

	for key, doc := range b.files {
		if strings.HasPrefix(key, basePrefix) {
			relPath := strings.TrimPrefix(key, basePrefix)
			newFileKey := fileKey(filter.Tenant, filter.Namespace, newBranch, relPath)
			cloned := Document{
				ID:        newFileKey,
				Namespace: doc.Namespace,
				Tenant:    doc.Tenant,
				Content:   append([]byte(nil), doc.Content...),
				Path:      doc.Path,
				Metadata:  cloneMetadata(doc.Metadata),
				Version:   doc.Version,
				CreatedAt: doc.CreatedAt,
				UpdatedAt: doc.UpdatedAt,
			}
			b.files[newFileKey] = cloned
		}
	}

	now := b.now()
	info := BranchInfo{
		Name:      newBranch,
		Base:      baseBranch,
		CreatedAt: now,
	}
	b.branches[newKey] = &fsBranch{
		info:    info,
		storage: &agentfs.FakeStorage{},
	}
	return info, nil
}

// cloneMetadata returns a shallow copy of a metadata map.
func cloneMetadata(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ── Backend.Snapshot ─────────────────────────────────────────────────────────

// Snapshot captures a point-in-time snapshot of the named branch by calling
// pkg/agentfs.Manager.Backup(). The Manager uploads the branch's Storage
// state to S3 and returns a Version which maps to SnapshotInfo.
//
// For the snapshot to be meaningful in production, the branch's Storage must
// be a real SQLite DB backed by the AgentFS mount; in tests, FakeStorage
// captures whatever bytes were set on it.
func (b *agentFSBackend) Snapshot(ctx context.Context, branch string, filter Filter) (SnapshotInfo, error) {
	if err := ctx.Err(); err != nil {
		return SnapshotInfo{}, BackendUnavailable("context: " + err.Error())
	}
	if filter.Tenant == "" {
		return SnapshotInfo{}, Invalid("filter.Tenant is required")
	}
	if filter.Namespace == "" {
		return SnapshotInfo{}, Invalid("filter.Namespace is required")
	}
	if branch == "" {
		return SnapshotInfo{}, Invalid("branch is required")
	}
	if b.s3 == nil {
		return SnapshotInfo{}, BackendUnavailable("agentfs: S3 driver not configured; cannot snapshot")
	}

	b.mu.Lock()
	key := branchKey(filter.Tenant, filter.Namespace, branch)
	br, ok := b.branches[key]
	if !ok {
		// Auto-seed so the caller gets a consistent empty snapshot.
		br = b.ensureBranch(filter.Tenant, filter.Namespace, branch)
	}
	storage := br.storage
	b.mu.Unlock()

	// Build a Manager scoped to this branch's S3 prefix so each branch has
	// its own version history under <basePrefix>/<tenant>/<namespace>/<branch>/.
	spec := b.branchSpec(filter.Tenant, filter.Namespace, branch)
	mgr := &agentfs.Manager{
		Spec:    spec,
		Storage: storage,
		S3:      b.s3,
		Now:     b.now,
	}

	v, err := mgr.Backup()
	if err != nil {
		return SnapshotInfo{}, BackendUnavailable("agentfs snapshot: " + err.Error())
	}

	return SnapshotInfo{
		ID:        v.ID,
		Branch:    branch,
		CreatedAt: v.CreatedAt,
		SizeBytes: v.SizeBytes,
	}, nil
}

// branchSpec returns an AgentFSSpec scoped to a specific branch by prefixing
// the base S3 prefix with tenant/namespace/branch. This ensures each branch
// has independent version history in S3.
func (b *agentFSBackend) branchSpec(tenant, namespace, branch string) v1.AgentFSSpec {
	spec := b.spec
	if spec.Backup == nil {
		// Callers must have a backup policy for snapshots to work; Snapshot
		// checks for s3 != nil before calling branchSpec but the Manager's
		// checkPolicy will catch a nil Backup.
		return spec
	}
	// Deep-copy the backup spec so we can mutate the prefix safely.
	bk := *spec.Backup
	if bk.S3 != nil {
		s3 := *bk.S3
		s3.Prefix = path.Join(bk.S3.Prefix, tenant, namespace, branch)
		bk.S3 = &s3
	}
	spec.Backup = &bk
	return spec
}

// ── Backend.ListBranches ─────────────────────────────────────────────────────

func (b *agentFSBackend) ListBranches(ctx context.Context, filter Filter) ([]BranchInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, BackendUnavailable("context: " + err.Error())
	}
	if filter.Tenant == "" {
		return nil, Invalid("filter.Tenant is required")
	}
	if filter.Namespace == "" {
		return nil, Invalid("filter.Namespace is required")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	prefix := filter.Tenant + "/" + filter.Namespace + "/"
	var out []BranchInfo
	for key, br := range b.branches {
		if strings.HasPrefix(key, prefix) {
			out = append(out, br.info)
		}
	}
	return out, nil
}
