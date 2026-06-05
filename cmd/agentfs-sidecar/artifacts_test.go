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
