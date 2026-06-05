package main

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/smol-platform/smol-agents/pkg/agentfs"
)

// terminationMsgPath is the container termination-message file; the operator
// folds the manifest from pod status (no k8s client / RBAC in the sidecar).
const terminationMsgPath = "/dev/termination-log"

// terminationMsgCap keeps the manifest well under k8s's 4096-byte termination
// message limit (per-container), leaving margin for encoding.
const terminationMsgCap = 3072

// writeArtifactTerminationMessage records the collection manifest to the
// sidecar's termination-message file so the operator can fold it from pod
// status. Best-effort: never fatal (the run already completed; artifacts are
// observability-only). Refs are dropped from the tail if the manifest would
// exceed the termination-message cap — the files are still in S3; this only
// bounds what surfaces in AgentRun.status.
func writeArtifactTerminationMessage(m agentfs.ArtifactManifest) {
	if b := capArtifactManifest(m); b != nil {
		_ = os.WriteFile(terminationMsgPath, b, 0o644)
	}
}

// capArtifactManifest marshals the manifest, dropping refs from the tail until
// it fits terminationMsgCap. State is always preserved; a truncated ref list is
// acceptable (the files remain in S3). Returns nil only if marshaling fails.
func capArtifactManifest(m agentfs.ArtifactManifest) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	for len(b) > terminationMsgCap && len(m.Refs) > 0 {
		m.Refs = m.Refs[:len(m.Refs)-1]
		if b, err = json.Marshal(m); err != nil {
			return nil
		}
	}
	return b
}

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
