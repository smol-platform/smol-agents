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
	"crypto/sha256"
	"encoding/hex"
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

	// ForkedFrom is the name of the branch this was forked from.
	// Empty for root branches (e.g. "main").
	ForkedFrom string

	// ForkBase maps relative file path → sha256 hex hash of Content at fork
	// time. This is the merge base B used by the 3-way classifier. For root
	// branches this is nil (all files are considered "added with no base").
	ForkBase map[string]string
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
	// At the same time, build the ForkBase manifest: relPath → sha256(content).
	basePrefix := fileKey(filter.Tenant, filter.Namespace, baseBranch, "")
	forkBase := make(map[string]string)

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
			forkBase[relPath] = contentHash(doc.Content)
		}
	}

	now := b.now()
	info := BranchInfo{
		Name:      newBranch,
		Base:      baseBranch,
		CreatedAt: now,
	}
	b.branches[newKey] = &fsBranch{
		info:       info,
		storage:    &agentfs.FakeStorage{},
		ForkedFrom: baseBranch,
		ForkBase:   forkBase,
	}
	return info, nil
}

// contentHash returns the SHA-256 hex digest of b. Used as the merge-base
// fingerprint stored in ForkBase so the 3-way classifier can detect whether
// either side modified a file relative to the fork point.
func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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

// ── Backend.Merge ────────────────────────────────────────────────────────────

// Merge performs a 3-way merge of srcBranch into dstBranch.
//
// The merge-base B is the ForkBase snapshot captured by Branch() when the
// source branch was created. For each path seen across any of the three
// versions (base B, destination O, source T), the classifier picks the
// outcome per the decision table in Backend.Merge's doc comment.
//
// The operation is atomic: all changes are staged in temporary maps and only
// swapped into b.files when the policy allows a commit (fail policy with
// conflicts → nothing written; DryRun → nothing written).
//
// Thread safety: the entire operation runs under b.mu.
func (b *agentFSBackend) Merge(ctx context.Context, srcBranch, dstBranch string, opts MergeOptions, filter Filter) (MergeResult, error) {
	if err := ctx.Err(); err != nil {
		return MergeResult{}, BackendUnavailable("context: " + err.Error())
	}
	if filter.Tenant == "" {
		return MergeResult{}, Invalid("filter.Tenant is required")
	}
	if filter.Namespace == "" {
		return MergeResult{}, Invalid("filter.Namespace is required")
	}
	if srcBranch == "" {
		return MergeResult{}, Invalid("srcBranch is required")
	}
	if dstBranch == "" {
		return MergeResult{}, Invalid("dstBranch is required")
	}
	if srcBranch == dstBranch {
		return MergeResult{}, Invalid("srcBranch and dstBranch must differ")
	}

	// Normalise policy: empty string = MergeFail (safe default).
	policy := opts.OnConflict
	if policy == "" {
		policy = MergeFail
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Both branches must exist within the exact tenant+namespace scope.
	srcKey := branchKey(filter.Tenant, filter.Namespace, srcBranch)
	srcBr, ok := b.branches[srcKey]
	if !ok {
		return MergeResult{}, NotFound(fmt.Sprintf("branch %q not found", srcBranch))
	}
	dstKey := branchKey(filter.Tenant, filter.Namespace, dstBranch)
	dstBr, ok := b.branches[dstKey]
	if !ok {
		return MergeResult{}, NotFound(fmt.Sprintf("branch %q not found", dstBranch))
	}

	// The merge base B is the ForkBase of the *source* branch — the snapshot
	// of dstBranch (or whatever was branched) at the time src was created.
	// If ForkBase is nil (root branch or legacy branch), treat every file as
	// having no base entry (B = absent).
	base := srcBr.ForkBase // map relPath → sha256; may be nil

	srcPrefix := fileKey(filter.Tenant, filter.Namespace, srcBranch, "")
	dstPrefix := fileKey(filter.Tenant, filter.Namespace, dstBranch, "")

	// Collect src and dst file maps indexed by relPath → Document.
	srcFiles := make(map[string]Document)
	for key, doc := range b.files {
		if !strings.HasPrefix(key, srcPrefix) {
			continue
		}
		if doc.Tenant != filter.Tenant || doc.Namespace != filter.Namespace {
			return MergeResult{}, PermissionDenied("cross-tenant file in srcBranch; merge aborted")
		}
		rel := strings.TrimPrefix(key, srcPrefix)
		srcFiles[rel] = doc
	}

	dstFiles := make(map[string]Document)
	for key, doc := range b.files {
		if !strings.HasPrefix(key, dstPrefix) {
			continue
		}
		if doc.Tenant != filter.Tenant || doc.Namespace != filter.Namespace {
			return MergeResult{}, PermissionDenied("cross-tenant file in dstBranch; merge aborted")
		}
		rel := strings.TrimPrefix(key, dstPrefix)
		dstFiles[rel] = doc
	}

	// Union of all paths across base/src/dst.
	allPaths := make(map[string]struct{})
	for rel := range srcFiles {
		allPaths[rel] = struct{}{}
	}
	for rel := range dstFiles {
		allPaths[rel] = struct{}{}
	}
	if base != nil {
		for rel := range base {
			allPaths[rel] = struct{}{}
		}
	}

	// ── Stage 1: classify every path ─────────────────────────────────────────

	// mergePlan records what should happen for each path.
	type action int
	const (
		actionKeep   action = iota // keep dstFiles[rel] unchanged
		actionTakeT                // write srcFiles[rel] into dst
		actionDelete               // delete from dst
		// actionConflict is handled inline via conflicts slice
	)

	type planned struct {
		act  action
		doc  Document // non-zero for actionTakeT
		path string   // relPath for actionDelete
	}

	var conflicts []ConflictInfo
	plan := make(map[string]planned, len(allPaths))

	for rel := range allPaths {
		srcDoc, inSrc := srcFiles[rel]
		dstDoc, inDst := dstFiles[rel]
		baseHash, inBase := "", false
		if base != nil {
			baseHash, inBase = base[rel]
		}

		oHash := ""
		if inDst {
			oHash = contentHash(dstDoc.Content)
		}
		tHash := ""
		if inSrc {
			tHash = contentHash(srcDoc.Content)
		}

		switch {
		// Both absent in src and dst — path is only in base (deleted on both sides).
		case !inSrc && !inDst:
			plan[rel] = planned{act: actionKeep}

		// Present only in dst (T deleted/absent) — check if T deleted.
		case !inSrc && inDst:
			if inBase {
				// T deleted this file relative to base.
				if oHash == baseHash {
					// O unchanged since base; take T's deletion.
					plan[rel] = planned{act: actionDelete, path: rel}
				} else {
					// O modified since base but T deleted it → edit/delete conflict.
					conflicts = append(conflicts, ConflictInfo{Path: rel, Kind: "edit/delete"})
					plan[rel] = planned{act: actionKeep} // resolved below by policy
				}
			} else {
				// Not in base: dst added it independently, src never had it. Keep dst.
				plan[rel] = planned{act: actionKeep}
			}

		// Present only in src (O deleted/absent).
		case inSrc && !inDst:
			if inBase {
				// O deleted this file. T still has it.
				if tHash == baseHash {
					// T unchanged; O deleted it → keep deleted (nothing to add).
					plan[rel] = planned{act: actionKeep}
				} else {
					// T modified but O deleted → edit/delete conflict.
					conflicts = append(conflicts, ConflictInfo{Path: rel, Kind: "edit/delete"})
					plan[rel] = planned{act: actionKeep} // resolved below by policy
				}
			} else {
				// Not in base: src added a new file that dst doesn't have. Add it.
				plan[rel] = planned{act: actionTakeT, doc: srcDoc}
			}

		// Present in both src and dst.
		default: // inSrc && inDst
			if oHash == tHash {
				// Both are identical (regardless of base): keep dst (no change needed).
				plan[rel] = planned{act: actionKeep}
			} else if !inBase {
				// No base: both added the same path with different content → add/add conflict.
				conflicts = append(conflicts, ConflictInfo{Path: rel, Kind: "add/add"})
				plan[rel] = planned{act: actionKeep} // resolved below by policy
			} else if oHash == baseHash && tHash != baseHash {
				// Only T changed: fast-forward take T.
				plan[rel] = planned{act: actionTakeT, doc: srcDoc}
			} else if oHash != baseHash && tHash == baseHash {
				// Only O changed: keep O.
				plan[rel] = planned{act: actionKeep}
			} else {
				// Both O and T differ from base and differ from each other: edit/edit conflict.
				conflicts = append(conflicts, ConflictInfo{Path: rel, Kind: "edit/edit"})
				plan[rel] = planned{act: actionKeep} // resolved below by policy
			}
		}
	}

	// ── Stage 2: apply conflict policy ───────────────────────────────────────

	// For each conflicting path, override the plan based on OnConflict policy.
	// "fail" path: just return conflicts without committing.
	if policy == MergeFail && len(conflicts) > 0 {
		return MergeResult{
			Conflicts: conflicts,
			Committed: false,
		}, nil
	}

	// For ours/theirs, override conflicting paths' plan entries.
	if len(conflicts) > 0 {
		for _, ci := range conflicts {
			rel := ci.Path
			switch policy {
			case MergeOurs:
				plan[rel] = planned{act: actionKeep}
			case MergeTheirs:
				// Take the src version; if src doesn't have it (edit/delete where
				// src deleted), delete from dst.
				if srcDoc, ok := srcFiles[rel]; ok {
					plan[rel] = planned{act: actionTakeT, doc: srcDoc}
				} else {
					plan[rel] = planned{act: actionDelete, path: rel}
				}
			}
		}
	}

	// ── Stage 3: count and (if not DryRun) apply ─────────────────────────────

	var mergedCount, addedCount, deletedCount int
	for rel, p := range plan {
		switch p.act {
		case actionTakeT:
			_, existsInDst := dstFiles[rel]
			if existsInDst {
				mergedCount++
			} else {
				addedCount++
			}
		case actionDelete:
			deletedCount++
		}
	}

	if opts.DryRun {
		return MergeResult{
			Conflicts: conflicts,
			Committed: false,
			Merged:    mergedCount,
			Added:     addedCount,
			Deleted:   deletedCount,
		}, nil
	}

	// Commit: apply the plan atomically.
	now := b.now()
	for rel, p := range plan {
		dstFileKey := fileKey(filter.Tenant, filter.Namespace, dstBranch, rel)
		switch p.act {
		case actionTakeT:
			srcDoc := p.doc
			// Preserve original CreatedAt if a dst file already exists at this path.
			createdAt := srcDoc.CreatedAt
			if existing, exists := b.files[dstFileKey]; exists {
				createdAt = existing.CreatedAt
			}
			b.files[dstFileKey] = Document{
				ID:        dstFileKey,
				Namespace: filter.Namespace,
				Tenant:    filter.Tenant,
				Content:   append([]byte(nil), srcDoc.Content...),
				Path:      srcDoc.Path,
				Metadata:  cloneMetadata(srcDoc.Metadata),
				Version:   fmt.Sprintf("%d", now.UnixNano()),
				CreatedAt: createdAt,
				UpdatedAt: now,
			}
		case actionDelete:
			delete(b.files, dstFileKey)
		case actionKeep:
			// Nothing to do.
		}
	}

	// Mark the destination branch as committed.
	dstBr.info.CommittedAt = now
	b.branches[dstKey] = dstBr

	return MergeResult{
		Branch:    dstBr.info,
		Conflicts: conflicts, // informational for ours/theirs/markers/union
		Committed: true,
		Merged:    mergedCount,
		Added:     addedCount,
		Deleted:   deletedCount,
	}, nil
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
