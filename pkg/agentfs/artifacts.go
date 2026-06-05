package agentfs

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Artifact collection states (never affect the run Phase — observability only).
const (
	ArtifactComplete = "Complete"
	ArtifactPartial  = "Partial"
	ArtifactFailed   = "Failed"
)

// ArtifactRule mirrors the agentmodel ArtifactSpec rule. It is declared locally
// so this low-level package stays decoupled from the operator/agentmodel types
// (the sidecar translates the CRD rules into these).
type ArtifactRule struct {
	Name        string
	Glob        string
	MaxBytes    int64
	ContentType string
}

// ArtifactRef records one uploaded — or skipped — artifact.
type ArtifactRef struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	S3Key       string `json:"s3Key,omitempty"`
	S3VersionID string `json:"s3VersionID,omitempty"`
	SizeBytes   int64  `json:"sizeBytes,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Skipped     string `json:"skipped,omitempty"` // reason when not uploaded
}

// ArtifactManifest is the result of a CollectArtifacts run.
type ArtifactManifest struct {
	State string        `json:"state"`
	Refs  []ArtifactRef `json:"refs"`
}

const (
	catOK = iota
	catSkip
	catUploadErr
)

// CollectArtifacts globs each rule against the workspace, uploads matches to s3
// under keyPrefix (which the caller scopes per-tenant, e.g.
// "artifacts/<ns>/<run>"), and returns a manifest. Best-effort: an over-budget
// or unreadable file is recorded as Skipped (→ Partial); a total upload outage
// (every Put failed, none succeeded) yields Failed; a clean run is Complete.
// Matches upload in lexical order for determinism. Never returns a hard error —
// the caller logs the manifest and exits 0.
func CollectArtifacts(workspace string, rules []ArtifactRule, s3 S3, keyPrefix string) ArtifactManifest {
	m := ArtifactManifest{State: ArtifactComplete}
	wsfs := os.DirFS(workspace)
	var ok, skip, uploadErr int

	for _, r := range rules {
		if strings.HasPrefix(r.Glob, "/") || strings.Contains(r.Glob, "..") {
			m.Refs = append(m.Refs, ArtifactRef{Name: r.Name, Skipped: "invalid glob (not workspace-relative)"})
			skip++
			continue
		}
		matches, err := doublestar.Glob(wsfs, r.Glob)
		if err != nil {
			m.Refs = append(m.Refs, ArtifactRef{Name: r.Name, Skipped: "glob error: " + err.Error()})
			skip++
			continue
		}
		sort.Strings(matches)
		for _, rel := range matches {
			ref, cat := collectOne(workspace, rel, r, s3, keyPrefix)
			m.Refs = append(m.Refs, ref)
			switch cat {
			case catOK:
				ok++
			case catUploadErr:
				uploadErr++
			default:
				skip++
			}
		}
	}

	switch {
	case uploadErr > 0 && ok == 0:
		m.State = ArtifactFailed
	case uploadErr > 0 || skip > 0:
		m.State = ArtifactPartial
	}
	return m
}

func collectOne(workspace, rel string, r ArtifactRule, s3 S3, keyPrefix string) (ArtifactRef, int) {
	full := filepath.Join(workspace, filepath.FromSlash(rel))
	// Defend against a symlink/.. escape that survived the glob.
	if !strings.HasPrefix(filepath.Clean(full), filepath.Clean(workspace)+string(os.PathSeparator)) {
		return ArtifactRef{Name: r.Name, Path: rel, Skipped: "path escapes workspace"}, catSkip
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		return ArtifactRef{Name: r.Name, Path: rel, Skipped: "not a regular file"}, catSkip
	}
	if r.MaxBytes > 0 && info.Size() > r.MaxBytes {
		return ArtifactRef{Name: r.Name, Path: rel, SizeBytes: info.Size(), Skipped: "over-budget"}, catSkip
	}
	f, err := os.Open(full)
	if err != nil {
		return ArtifactRef{Name: r.Name, Path: rel, Skipped: "open: " + err.Error()}, catSkip
	}
	defer f.Close()

	h := sha256.New()
	key := path.Join(keyPrefix, r.Name, rel)
	v, err := s3.Put(key, io.TeeReader(f, h), PutMeta{ContentType: r.ContentType})
	if err != nil {
		return ArtifactRef{Name: r.Name, Path: rel, S3Key: key, Skipped: "upload: " + err.Error()}, catUploadErr
	}
	return ArtifactRef{
		Name:        r.Name,
		Path:        rel,
		S3Key:       key,
		S3VersionID: v.ID,
		SizeBytes:   info.Size(),
		SHA256:      hex.EncodeToString(h.Sum(nil)),
		ContentType: r.ContentType,
	}, catOK
}
