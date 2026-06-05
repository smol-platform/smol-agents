package main

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/smol-platform/smol-agents/pkg/agentfs"
)

// collectArtifactsOnShutdown runs the artifact collector when AGENTFS_ARTIFACTS
// is configured. It is called AFTER the scheduler's final SIGTERM backup so the
// AgentFS RPO is preserved first, then declared workspace files are published.
// Returns (manifest, true) when artifacts were configured, (zero, false) when
// not. Best-effort: a malformed rules env yields a Failed manifest rather than
// crashing the sidecar (the run already succeeded; artifacts never gate it).
//
// AGENTFS_ARTIFACTS        — JSON-encoded []agentfs.ArtifactRule
// AGENTFS_ARTIFACT_PREFIX  — per-tenant key prefix (operator sets "artifacts/<ns>/<run>")
func collectArtifactsOnShutdown(mount string, s3 agentfs.S3) (agentfs.ArtifactManifest, bool) {
	raw := strings.TrimSpace(os.Getenv("AGENTFS_ARTIFACTS"))
	if raw == "" {
		return agentfs.ArtifactManifest{}, false
	}
	var rules []agentfs.ArtifactRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return agentfs.ArtifactManifest{State: agentfs.ArtifactFailed}, true
	}
	prefix := os.Getenv("AGENTFS_ARTIFACT_PREFIX")
	if prefix == "" {
		prefix = "artifacts"
	}
	return agentfs.CollectArtifacts(mount, rules, s3, prefix), true
}
