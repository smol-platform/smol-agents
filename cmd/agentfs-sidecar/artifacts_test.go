package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/smol-platform/smol-agents/pkg/agentfs"
)

func TestCollectArtifactsOnShutdown(t *testing.T) {
	// Not configured → reports false.
	if _, ok := collectArtifactsOnShutdown(t.TempDir(), agentfs.NewFakeS3()); ok {
		t.Fatalf("no AGENTFS_ARTIFACTS must report not-configured")
	}

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "result.json"), []byte(`{"ok":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rules, _ := json.Marshal([]agentfs.ArtifactRule{{Name: "res", Glob: "result.json"}})
	t.Setenv("AGENTFS_ARTIFACTS", string(rules))
	t.Setenv("AGENTFS_ARTIFACT_PREFIX", "artifacts/tenant-a/run-1")

	m, ok := collectArtifactsOnShutdown(ws, agentfs.NewFakeS3())
	if !ok || m.State != agentfs.ArtifactComplete || len(m.Refs) != 1 {
		t.Fatalf("expected Complete with 1 ref, got ok=%v %+v", ok, m)
	}
	if m.Refs[0].S3Key != "artifacts/tenant-a/run-1/res/result.json" {
		t.Errorf("tenant-scoped key wrong: %s", m.Refs[0].S3Key)
	}

	// Malformed rules → Failed, but still "configured".
	t.Setenv("AGENTFS_ARTIFACTS", "not json")
	if m, ok := collectArtifactsOnShutdown(ws, agentfs.NewFakeS3()); !ok || m.State != agentfs.ArtifactFailed {
		t.Errorf("malformed rules → want Failed, got ok=%v %s", ok, m.State)
	}
}

// M2.26: the termination-message manifest fits under the cap by dropping refs
// from the tail, while always preserving State (the files stay in S3).
func TestCapArtifactManifest(t *testing.T) {
	// Small manifest passes through whole.
	small := agentfs.ArtifactManifest{State: agentfs.ArtifactComplete, Refs: []agentfs.ArtifactRef{{Name: "a", S3Key: "k/a"}}}
	if b := capArtifactManifest(small); len(b) == 0 || len(b) > terminationMsgCap {
		t.Fatalf("small manifest: len=%d (cap %d)", len(b), terminationMsgCap)
	}

	// A manifest with many bulky refs is capped; refs drop but State survives.
	big := agentfs.ArtifactManifest{State: agentfs.ArtifactPartial}
	for i := 0; i < 400; i++ {
		big.Refs = append(big.Refs, agentfs.ArtifactRef{
			Name: "file-with-a-fairly-long-name", Path: "/workspace/outputs/dir", S3Key: "artifacts/ns/run/file-with-a-fairly-long-name", SHA256: "0123456789abcdef0123456789abcdef",
		})
	}
	b := capArtifactManifest(big)
	if len(b) == 0 || len(b) > terminationMsgCap {
		t.Fatalf("big manifest not capped: len=%d (cap %d)", len(b), terminationMsgCap)
	}
	var got agentfs.ArtifactManifest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("capped manifest must stay valid JSON: %v", err)
	}
	if got.State != agentfs.ArtifactPartial {
		t.Errorf("State must survive capping, got %q", got.State)
	}
	if len(got.Refs) >= len(big.Refs) {
		t.Errorf("expected refs dropped to fit, kept %d of %d", len(got.Refs), len(big.Refs))
	}
}
