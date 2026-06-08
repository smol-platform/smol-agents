// kopia-backed VersionedStore (Tier 0): a content-addressed, encrypted,
// deduplicating snapshot engine over an S3 repository. Shells out to a `kopia`
// binary (baked into the agentfs-sidecar image) — no FUSE, no privilege, runs as
// the non-root sidecar uid. The kopia commands are exercised in tests via the
// injectable `run` hook; live S3 interop (exact flags / JSON) is confirmed
// against minio. See docs/design/agentfs-fuse-plugin.md.
package agentfs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// KopiaStore is a VersionedStore over a kopia repository in S3.
//
// Config travels via the same AGENTFS_S3_* env + CredentialsRef the operator
// already wires; the repo lives under "<prefix>/kopia/" so it never collides
// with the legacy "<prefix>/agentfs.sqlite" tar key. The repo password and AWS
// creds come from env (broker/secret-projected), never inlined in the spec.
type KopiaStore struct {
	Bucket   string
	Prefix   string
	Region   string
	Endpoint string // may include a scheme; http:// → --disable-tls

	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string

	// RepoPath, when non-empty, hosts the kopia repo on the local filesystem
	// instead of S3 — the ephemeral backend (no durable destination; the repo
	// lives and dies with the pod, giving in-pod history/diff/rollback without
	// configuring S3). When set it supersedes the S3 fields above.
	RepoPath string

	// Password protects the kopia repo (kopia encrypts at rest). Required.
	Password string

	// ConfigDir holds kopia's per-process config + cache; must be writable by
	// the non-root sidecar uid. Defaults to <tmp>/kopia.
	ConfigDir string

	// Binary is the kopia executable (default "kopia", resolved on PATH).
	Binary string

	// run executes a kopia subcommand. nil → defaultRun (exec). Injected by
	// tests so the package stays hermetic.
	run func(ctx context.Context, args ...string) ([]byte, error)
}

var _ VersionedStore = (*KopiaStore)(nil)

func (k *KopiaStore) configDir() string {
	if k.ConfigDir != "" {
		return k.ConfigDir
	}
	return filepath.Join(os.TempDir(), "kopia")
}

func (k *KopiaStore) binary() string {
	if k.Binary != "" {
		return k.Binary
	}
	return "kopia"
}

func (k *KopiaStore) exec(ctx context.Context, args ...string) ([]byte, error) {
	if k.run != nil {
		return k.run(ctx, args...)
	}
	return k.defaultRun(ctx, args...)
}

// defaultRun runs the real kopia binary with a writable config/cache dir and
// repo password + AWS creds injected via env (never on the argv).
func (k *KopiaStore) defaultRun(ctx context.Context, args ...string) ([]byte, error) {
	cd := k.configDir()
	cmd := exec.CommandContext(ctx, k.binary(), args...)
	env := append(os.Environ(),
		"KOPIA_PASSWORD="+k.Password,
		"KOPIA_CONFIG_PATH="+filepath.Join(cd, "repository.config"),
		"KOPIA_CACHE_DIRECTORY="+filepath.Join(cd, "cache"),
		"KOPIA_LOG_DIR="+filepath.Join(cd, "logs"),
		"KOPIA_CHECK_FOR_UPDATES=false",
	)
	if k.AccessKeyID != "" {
		env = append(env, "AWS_ACCESS_KEY_ID="+k.AccessKeyID)
	}
	if k.SecretAccessKey != "" {
		env = append(env, "AWS_SECRET_ACCESS_KEY="+k.SecretAccessKey)
	}
	if k.SessionToken != "" {
		env = append(env, "AWS_SESSION_TOKEN="+k.SessionToken)
	}
	cmd.Env = env
	// kopia writes the --json manifest to stdout and progress/log lines to
	// stderr. Capture them separately: merging (CombinedOutput) interleaves the
	// log lines into the JSON and breaks parseManifests ("no manifest in output").
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		verb := ""
		if len(args) > 0 {
			verb = args[0]
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return stdout.Bytes(), fmt.Errorf("kopia %s: %w: %s", verb, err, detail)
	}
	return stdout.Bytes(), nil
}

// repoPrefix is where the kopia repo lives within the bucket: "<prefix>/kopia/".
func (k *KopiaStore) repoPrefix() string {
	p := strings.Trim(k.Prefix, "/")
	if p == "" {
		return "kopia/"
	}
	return p + "/kopia/"
}

// s3Args builds `repository {connect|create} s3 …` flags.
func (k *KopiaStore) s3Args(create bool) []string {
	verb := "connect"
	if create {
		verb = "create"
	}
	a := []string{"repository", verb, "s3",
		"--bucket=" + k.Bucket,
		"--prefix=" + k.repoPrefix(),
		"--access-key=" + k.AccessKeyID,
		"--secret-access-key=" + k.SecretAccessKey,
	}
	if k.Region != "" {
		a = append(a, "--region="+k.Region)
	}
	if ep := k.Endpoint; ep != "" {
		a = append(a, "--endpoint="+stripScheme(ep))
		if strings.HasPrefix(ep, "http://") {
			a = append(a, "--disable-tls")
		}
	}
	if k.SessionToken != "" {
		a = append(a, "--session-token="+k.SessionToken)
	}
	return a
}

// fsArgs builds `repository {connect|create} filesystem --path=<RepoPath>` for
// the ephemeral (no-S3) backend: a kopia repo on the pod's own local storage.
func (k *KopiaStore) fsArgs(create bool) []string {
	verb := "connect"
	if create {
		verb = "create"
	}
	return []string{"repository", verb, "filesystem", "--path=" + k.RepoPath}
}

// repoArgs selects the repo backend: a local filesystem repo when RepoPath is
// set (ephemeral), else the S3 repo (durable).
func (k *KopiaStore) repoArgs(create bool) []string {
	if k.RepoPath != "" {
		return k.fsArgs(create)
	}
	return k.s3Args(create)
}

// Connect connects to the kopia repo, creating it on first use, then (on
// creation) installs the DB-exclusion ignore policy. Idempotent.
func (k *KopiaStore) Connect(ctx context.Context) error {
	if _, err := k.exec(ctx, k.repoArgs(false)...); err == nil {
		return nil // existing repo
	}
	if _, err := k.exec(ctx, k.repoArgs(true)...); err != nil {
		return fmt.Errorf("agentfs: kopia connect/create: %w", err)
	}
	// Install the live-DB exclusion once at creation (kept out of the file
	// snapshot; the DB is routed to a separate WAL-stream lane).
	policy := []string{"policy", "set", "--global"}
	for _, g := range ExcludeGlobs {
		policy = append(policy, "--add-ignore="+g)
	}
	if _, err := k.exec(ctx, policy...); err != nil {
		return fmt.Errorf("agentfs: kopia set ignore policy: %w", err)
	}
	return nil
}

// Checkpoint creates a new snapshot of src and returns its manifest. kopia
// streams + dedupes; only changed content is uploaded.
func (k *KopiaStore) Checkpoint(ctx context.Context, src, msg string) (Checkpoint, error) {
	args := []string{"snapshot", "create", "--json"}
	if msg != "" {
		args = append(args, "--description="+msg)
	}
	args = append(args, src)
	out, err := k.exec(ctx, args...)
	if err != nil {
		return Checkpoint{}, err
	}
	cps := parseManifests(out)
	if len(cps) == 0 {
		return Checkpoint{}, fmt.Errorf("agentfs: kopia snapshot create: no manifest in output")
	}
	cp := cps[0]
	cp.Message = msg
	return cp, nil
}

// History lists snapshots newest-first.
func (k *KopiaStore) History(ctx context.Context) ([]Checkpoint, error) {
	out, err := k.exec(ctx, "snapshot", "list", "--json")
	if err != nil {
		return nil, err
	}
	cps := parseManifests(out)
	sort.SliceStable(cps, func(i, j int) bool { return cps[i].CreatedAt.After(cps[j].CreatedAt) })
	return cps, nil
}

// Restore materializes ref ("" / "latest" / a snapshot ID) into dst.
func (k *KopiaStore) Restore(ctx context.Context, ref, dst string) (Checkpoint, error) {
	id := ref
	var meta Checkpoint
	if ref == "" || ref == "latest" {
		hist, err := k.History(ctx)
		if err != nil {
			return Checkpoint{}, err
		}
		if len(hist) == 0 {
			return Checkpoint{}, ErrNoVersion
		}
		meta = hist[0]
		id = hist[0].ID
	}
	if _, err := k.exec(ctx, "snapshot", "restore", id, dst); err != nil {
		return Checkpoint{}, err
	}
	if meta.ID == "" {
		meta = Checkpoint{ID: id}
	}
	return meta, nil
}

// Diff reports per-file changes between snapshots a and b (best-effort parse of
// kopia's diff output; confirmed against a live repo).
func (k *KopiaStore) Diff(ctx context.Context, a, b string) ([]FileChange, error) {
	out, err := k.exec(ctx, "diff", a, b)
	if err != nil {
		return nil, err
	}
	return parseDiff(out), nil
}

// GC applies retention (keep-latest) and runs maintenance to reclaim space.
func (k *KopiaStore) GC(ctx context.Context, ret RetentionSpec) error {
	if ret.MaxVersions > 0 {
		if _, err := k.exec(ctx, "policy", "set", "--global", fmt.Sprintf("--keep-latest=%d", ret.MaxVersions)); err != nil {
			return err
		}
	}
	// Apply retention to existing snapshots, then reclaim unreferenced blobs.
	if _, err := k.exec(ctx, "snapshot", "expire", "--all", "--delete"); err != nil {
		return err
	}
	_, err := k.exec(ctx, "maintenance", "run", "--full")
	return err
}

func stripScheme(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	return strings.TrimSuffix(u, "/")
}

// kopiaManifest is the subset of a kopia snapshot manifest we consume.
type kopiaManifest struct {
	ID        string    `json:"id"`
	StartTime time.Time `json:"startTime"`
	Stats     struct {
		TotalSize int64 `json:"totalSize"`
	} `json:"stats"`
	RootEntry struct {
		Summary struct {
			Size int64 `json:"size"`
		} `json:"summ"`
	} `json:"rootEntry"`
}

// parseManifests decodes `snapshot create/list --json`, which may be a single
// object or an array. Returns Checkpoints (unsorted).
func parseManifests(out []byte) []Checkpoint {
	trimmed := strings.TrimSpace(string(out))
	var manifests []kopiaManifest
	if strings.HasPrefix(trimmed, "[") {
		_ = json.Unmarshal([]byte(trimmed), &manifests)
	} else if strings.HasPrefix(trimmed, "{") {
		var m kopiaManifest
		if json.Unmarshal([]byte(trimmed), &m) == nil {
			manifests = []kopiaManifest{m}
		}
	}
	out2 := make([]Checkpoint, 0, len(manifests))
	for _, m := range manifests {
		if m.ID == "" {
			continue
		}
		size := m.Stats.TotalSize
		if size == 0 {
			size = m.RootEntry.Summary.Size
		}
		out2 = append(out2, Checkpoint{ID: m.ID, CreatedAt: m.StartTime, SizeBytes: size})
	}
	return out2
}

// parseDiff turns kopia's diff lines ("added file …", "modified …", "removed …")
// into FileChanges. Best-effort: unknown lines are skipped.
func parseDiff(out []byte) []FileChange {
	var changes []FileChange
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var t ChangeType
		switch fields[0] {
		case "added":
			t = ChangeAdded
		case "modified", "changed":
			t = ChangeModified
		case "removed", "deleted":
			t = ChangeDeleted
		default:
			continue
		}
		changes = append(changes, FileChange{Path: fields[len(fields)-1], Type: t})
	}
	return changes
}
