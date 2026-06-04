# Spec: Run Artifacts — Capture Files OUT to S3 with a Manifest in Status

> **✅ Decisions resolved 2026-06-03 — see [decisions.md](../design/decisions.md).** D1: artifact S3 prefixes must be per-tenant-scoped; collect/upload in the agentfs **sidecar** (which holds creds+mount), not the untrusted harness container. Where this doc still says OPEN/PROPOSED and conflicts, the decision log wins.

> **Status:** DESIGN / proposal. Implementation-grade spec. Grounded against
> v0.2.0 source (HEAD `0f64158`, read 2026-06-03). Nothing in §4–§6 is built
> yet unless explicitly marked "(landed)".
>
> **Feature key:** `artifact-egress`
> **Category:** stub-impl (net-new capability; reuses landed AgentFS plumbing)
> **Effort:** L (down from the F3 estimate of XL — its two hard prerequisites,
> F1 `EffectiveWorkingDir` and F4 native-sidecar, have since landed).
>
> Extends [framework-enhancements.md](../design/framework-enhancements.md) §F3
> and [custom-agent-images.md](../design/custom-agent-images.md). Read those
> first; this spec deepens F3 and does not restate it.

---

## 1. Summary

An `AgentRun` today can return exactly one thing: a JSON `Output` blob that the
runtime writes to the pod termination message under a hard ~3 KiB budget
(`cmd/agent/run.go:94`). There is **no concept of a file artifact** anywhere in
the tree (grep-confirmed: the only `Artifact` hits are unrelated e2e cluster
fixtures and a kopia test). A coding harness — claude-code, codex, aider,
openclaw — does its real work by *writing files*: edited source, a diff, a
generated report, a rendered image. All of that is invisible to the caller. The
files either die with the ephemeral `/tmp` workspace, or — if the Agent has
durable AgentFS — get buried inside a single full-volume snapshot with no
per-file discovery, no content type, and no integrity hash.

This spec adds **artifact egress**: the Agent author declares which workspace
files to capture (`AgentSpec.Artifacts.Outputs[]` glob rules with size and
content-type bounds); on run shutdown the platform globs the workspace, uploads
each matching file to S3, and folds a compact **manifest of references** —
`RunStatus.Artifacts[] {Name, Path, S3Bucket/Key/VersionID, SizeBytes, SHA256}`
— into the AgentRun status. The manifest holds *refs, not bytes*, so the
termination-message budget is untouched.

The single load-bearing design decision: **collection and upload run in the
AgentFS sidecar — which already holds the S3 credentials and the workspace
mount — not in the untrusted harness/agent container.** This composes naturally
with the native-sidecar + final-backup-on-SIGTERM machinery that already landed
(F4). A post-`Completed` upload failure is surfaced through a *separate*
`ArtifactsState` condition so it can never clobber a run's terminal `Phase` or
its already-valid `Output`.

The outcome: claude-code/codex/aider/openclaw runs become first-class
file-producing workloads with a discoverable, hashed, versioned output
manifest — without widening the trust boundary and without touching the proven
result-fold path.

---

## 2. Current state

### What exists (and that this builds on)

| Capability | Where | Status |
|---|---|---|
| `EffectiveWorkingDir()` — harness CWD bound to the AgentFS mount | `pkg/agentmodel/v1/harness.go:291-304` | **Landed** (was F1). Resolves `CLI.WorkingDir` → AgentFS `MountPath` → `DefaultAgentFSMountPath` (`/var/agentfs`) → `""`. |
| Run input files materialized into the workspace | `pkg/agentruntime/runonce.go:124-184` (`MaterializeInputs`) + `AgentRunSpec.Inputs` (`pkg/agentmodel/v1/types.go:248-278`) | **Landed** (was F2, inline + base64 + secretRef). The *symmetric inbound* of this spec. |
| AgentFS sidecar is a **native sidecar** (init container, `RestartPolicy:Always`) | `operator/internal/builders/storage_mount.go:105-114` | **Landed** (was F4). Kubelet SIGTERMs it *after* the main container exits → the pod can reach `PodSucceeded`. |
| Final backup on SIGTERM, bounded by a grace floor | `cmd/agentfs-sidecar/main.go:69-85`, `pkg/agentfs/scheduler.go:32-99` (`BackupOnShutdown`, `ShutdownBackupTimeout=90s`); pod grace floor `agentFSShutdownGraceSeconds=120` (`storage_mount.go:44`) | **Landed.** The exact shutdown hook this spec extends. |
| `pkg/agentfs.S3.Put(key, body, meta) → (Version{ID,Key,SizeBytes,…}, error)` | `pkg/agentfs/types.go:56-58`, AWS impl `pkg/agentfs/s3_aws.go:94-136` (returns S3 `VersionId`) | **Landed.** The upload primitive. `S3BackupSpec` (bucket/prefix/region/SSE/KMS/credentialsRef/versioning) at `pkg/agentmodel/v1/storage.go:142-184`. |
| The sidecar holds S3 creds via `secretKeyRef` projection | `storage_mount.go:183-191` (`AWS_*` from `BackupPolicy.S3.CredentialsRef`) | **Landed.** Creds live in the *sidecar*, never the harness container. |
| `RunResult` wire type already carries `Steps`; termination-message budgeting exists | `runonce.go:24-38`, clamp in `cmd/agent/run.go:94-143` | **Landed.** The pattern (refs/skeleton in status, full detail in logs/S3) this spec follows. |

### What is missing (the gap this spec closes)

- **No artifact type, field, or code path at all.** `AgentSpec` (`types.go:51-90`)
  has no `Artifacts`. `RunStatus` (`types.go:313-322`) has `State/StartedAt/
  EndedAt/Steps/Usage/TerminationReason/Output` and no `Artifacts`. The
  agents/agentruns CRDs have no artifact schema.
- **The only output channel is `Output` → termination message.** `RunResult`
  (`runonce.go:27-38`) has `Phase/Output/Steps/Usage/TerminationReason/Error`;
  `clampForTerminationMessage` (`run.go:102-115`) trims `Output` to 2 KiB and can
  shed `Steps` entirely under the 3 KiB budget. A file cannot ride this channel.
- **The sidecar does only a whole-volume snapshot.** `Manager.Backup()`
  (`backup.go:37-55`) tars the mount to one key (`agentfs.sqlite`) or does a kopia
  checkpoint. There is no per-file, per-run-keyed `Put` and no manifest produced.
- **The run controller folds only `runResultFromPod`.** `foldRunResult`
  (`agentrun_controller.go:398-415`) reads the *execution* container's termination
  message (`agent`/`harness`, `:420-434`). It never reads anything from the
  sidecar, and there is no `ArtifactsState`/condition on the run.

### Honest constraints carried from F3

- Artifact collection only sees files the harness actually *wrote to the bound
  workspace*. For an HTTP harness (Hermes) that never touches the filesystem,
  there is nothing to collect — artifacts are a **CLI-harness / loop-mode**
  feature (same boundary as `MaterializeInputs`, `runonce.go:120-123`).
- Per-run-keyed S3 `Put` lives alongside the fixed `agentfs.sqlite` backup key. If
  artifacts share the AgentFS bucket, `S3VersionID` is only meaningful when that
  bucket has versioning enabled; the manifest must record the *actual* returned
  `VersionId` (which is `""` on an unversioned bucket) rather than implying one.
- Operator-side `AgentNetwork` egress enforcement is unimplemented (per project
  memory + [agentnetwork-datapath-enforcement.md](agentnetwork-datapath-enforcement.md)).
  Do **not** claim "S3 must be in the egress allow-list" as a live mitigation —
  the run pod's static default-deny NetworkPolicy already permits public 443
  (`operator/internal/builders/run_sandbox.go`), which is the channel S3 uses.

---

## 3. External interface research

Not applicable — this is an internal-only capability (no external tool
interface to confirm). Skipped per the spec brief.

---

## 4. Design

### 4.1 Where collection runs: the sidecar, not the harness (the central decision)

F3 flagged the credential-boundary problem and recommended the sidecar; this
spec commits to it concretely, because the landed F4 work makes it clean.

```
   AgentRun pod  (RestartPolicy: Never)
   ┌──────────────────────────────────────────────────────────────────┐
   │  initContainers:                                                   │
   │    agentfs-init    (restore from S3, then exit)                    │
   │    agentfs-sidecar (NATIVE: RestartPolicy=Always) ◄── holds AWS_*  │
   │        • serves AgentFS / periodic snapshots                       │   creds +
   │        • on SIGTERM: final backup  ── THEN ──► CollectArtifacts    │   /var/agentfs
   │                                                  glob+hash+Put      │   mount
   │                                                  write manifest CM  │
   │  containers:                                                       │
   │    agent | harness  (untrusted; ReadOnlyRootFS; NO AWS creds)      │
   │        • runs `agent run`, writes files into /var/agentfs          │
   │        • exits → kubelet SIGTERMs the native sidecar               │
   └──────────────────────────────────────────────────────────────────┘
                              │ pod → Succeeded
                              ▼
   AgentRunReconciler.foldRunResult     (existing: Output/Steps/Usage/Phase)
   AgentRunReconciler.foldArtifacts     (NEW: read manifest, set Status.Artifacts
                                          + ArtifactsState condition)
```

Why the sidecar and not `RunOnce` (the F3 proposal weighed but did not pick):

1. **Credential containment.** The harness/agent container is the least-trusted
   thing in the pod (it runs claude-code, arbitrary tool subprocesses). It has
   *no* AWS credentials today (`storage_mount.go:183-191` projects them only into
   the sidecar). Doing the upload in `RunOnce` would require projecting S3 creds
   into that container — a direct regression of the credential model.
2. **The mount + the hook already live there.** The sidecar already mounts the
   workspace (`storage_mount.go:154`) and already runs code on SIGTERM after the
   main container exits (`scheduler.go:51-53`). Artifact collection is one more
   step in that exact shutdown path.
3. **Timing is already solved.** A native sidecar's SIGTERM fires *after* the main
   container exits (`storage_mount.go:106-111`), so the workspace is quiescent —
   no race between "harness still writing" and "collector reading".

**Trade-off this accepts:** artifacts require durable AgentFS storage
(`storage.kind=agentfs`), because that is what attaches the sidecar. An Agent
that wants artifacts but no long-term durability still declares `storage.agentfs`
with a backup S3 target; the artifacts ride that target's bucket by default. This
is the same shape `MaterializeInputs` already demands of non-inline inputs (a
defined workspace). Loop/CLI agents with *no* AgentFS get no artifacts — call
that out in validation, fail loud.

> **Alternative considered and rejected:** a third "artifact-collector" init
> container distinct from the AgentFS sidecar. Rejected — it would need its own
> copy of the S3 creds and mount, duplicating `storage_mount.go` wiring for no
> isolation gain (it is exactly as trusted as the sidecar). Simplicity-first:
> extend the sidecar.

### 4.2 Manifest channel: a run-owned ConfigMap, not the termination message

The harness/agent termination message is budget-capped at 3 KiB and is owned by
`foldRunResult`. The sidecar cannot write to *that* container's termination
message. Two viable channels for the sidecar→controller manifest:

- **(A) The sidecar's own termination message.** A native sidecar (init
  container) also has a `terminationMessagePath`. The controller would read
  container status for `agentfs-sidecar`. **Same 4 KiB cap** — and a manifest of N
  refs (each ~200 B of bucket/key/versionID/sha256) blows it at ~15 files.
- **(B) A run-owned ConfigMap the sidecar writes via the apiserver.** No size
  cap worth worrying about (1 MiB ConfigMap limit ≈ thousands of refs). But it
  requires the sidecar to have a kube client + RBAC to write one ConfigMap.

**Decision: (B), a manifest ConfigMap** named `<run>-artifacts`, written by the
sidecar, owned by the AgentRun (GC'd with it). Rationale: refs are unbounded in
count by design (a coding run can touch dozens of files), so the cap in (A) is a
real ceiling, not a corner case. The sidecar already needs *no* apiserver access
today, so this adds a tightly-scoped Role (create/update one ConfigMap in its own
namespace) — see §5.6. If the maintainer prefers zero new sidecar RBAC, fall back
to (A) with a hard cap (e.g. first 10 artifacts in-band, rest "see logs") — noted
as an open decision (§10).

> The manifest is *also* mirrored to the sidecar's stdout (pod logs) regardless,
> matching the existing "full detail in logs" pattern (`run.go:64`).

### 4.3 The collection contract

`AgentSpec.Artifacts.Outputs[]` is an ordered list of `ArtifactRule`s. On
shutdown the sidecar, for each rule in order:

1. Globs `rule.Glob` relative to the workspace root (the AgentFS mount).
2. For each match (a regular file), enforces `rule.MaxBytes`; over-budget files
   are **skipped with a recorded reason** (not silently dropped, not fatal).
3. Streams the file through a SHA-256 hasher into `S3.Put` under
   `artifacts/<run-namespace>/<run-name>/<relpath>`.
4. Appends an `ArtifactRef` to the manifest.

Determinism: rules are processed in declaration order; within a rule, matches are
sorted lexicographically so the manifest is stable across runs (aids
[determinism-and-replay.md](determinism-and-replay.md)).

Naming: `ArtifactRef.Name` defaults to `rule.Name` when a rule matches exactly
one file, else `"<rule.Name>/<relpath>"` so multi-match rules stay addressable.

---

## 5. Concrete changes

### 5.1 CRD / Go types — `AgentSpec.Artifacts`

New types in `pkg/agentmodel/v1/storage.go` (sibling to `S3BackupSpec`, which is
reused):

```go
// ArtifactSpec declares which workspace files to capture OUT of a run and where
// to upload them. Applies to CLI-harness and loop-mode agents with a bound
// workspace (storage.kind=agentfs); HTTP harnesses (e.g. Hermes) never write the
// filesystem, so artifact collection is a no-op for them.
type ArtifactSpec struct {
    // Outputs are the capture rules, processed in declaration order.
    // +kubebuilder:validation:MinItems=1
    Outputs []ArtifactRule `json:"outputs"`

    // S3 is the upload destination. When nil, artifacts reuse
    // Storage.AgentFS.Backup.S3 (the durable-storage bucket) under the
    // "artifacts/<ns>/<run>/" prefix. An explicit S3 here lets artifacts land in
    // a separate bucket from the AgentFS snapshots.
    // +optional
    S3 *S3BackupSpec `json:"s3,omitempty"`
}

// ArtifactRule selects workspace files to capture.
type ArtifactRule struct {
    // Name identifies this rule's output(s) in RunStatus.Artifacts. Must be
    // unique within Outputs. When the rule matches exactly one file the ref is
    // named Name; multi-match rules name refs "<Name>/<relpath>".
    Name string `json:"name"`

    // Glob is a workspace-relative doublestar pattern (e.g. "out/**/*.json",
    // "report.md", "diff/*.patch"). Must be relative, no ".." segment.
    Glob string `json:"glob"`

    // MaxBytes caps a single matched file; larger files are skipped and recorded
    // with reason "over-budget". Default 10 MiB. 0 means use the default.
    // +kubebuilder:validation:Minimum=0
    // +optional
    MaxBytes int64 `json:"maxBytes,omitempty"`

    // ContentType sets the uploaded object's Content-Type (S3 PutMeta). Empty =
    // sniff by extension, falling back to application/octet-stream.
    // +optional
    ContentType string `json:"contentType,omitempty"`
}
```

Add to `AgentSpec` (`pkg/agentmodel/v1/types.go`, after `Storage` at `:81`):

```go
    // Artifacts declares workspace files to capture OUT of each run and upload to
    // S3, surfaced as RunStatus.Artifacts. Requires storage.kind=agentfs (the
    // sidecar that performs the upload). +optional
    Artifacts *ArtifactSpec `json:"artifacts,omitempty"`
```

**Validation** (`pkg/agentmodel/v1/validation.go`, extend `ValidateAgent`):

- If `Artifacts != nil`: require `Storage != nil && Storage.Kind == StorageAgentFS`
  (else "spec.artifacts requires storage.kind=agentfs"). This is the fail-loud
  gate that makes the sidecar-only design honest.
- The effective S3 target (`Artifacts.S3` else `Storage.AgentFS.Backup.S3`) must
  be non-nil with a bucket (else "spec.artifacts needs an S3 target: set
  artifacts.s3 or storage.agentfs.backup.s3").
- Each `ArtifactRule.Name` non-empty + unique; each `Glob` relative + no `..`
  (reuse the `safeWorkspacePath` traversal logic from `runonce.go:167-184` —
  export it or duplicate the segment check).

### 5.2 CRD / Go types — `RunStatus.Artifacts`

New type in `pkg/agentmodel/v1/types.go` (after `RunStatus` at `:322`):

```go
// ArtifactRef is one captured file's manifest entry — a reference, never the
// bytes, so it stays compact in status.
type ArtifactRef struct {
    Name        string `json:"name"`              // rule name (or "<rule>/<relpath>")
    Path        string `json:"path"`              // workspace-relative source path
    S3Bucket    string `json:"s3Bucket"`
    S3Key       string `json:"s3Key"`
    S3VersionID string `json:"s3VersionID,omitempty"` // "" on an unversioned bucket
    SizeBytes   int64  `json:"sizeBytes"`
    ContentType string `json:"contentType,omitempty"`
    SHA256      string `json:"sha256"`            // hex of the uploaded bytes
    Skipped     string `json:"skipped,omitempty"` // reason a match was NOT uploaded
}
```

Extend `RunStatus`:

```go
    // Artifacts is the manifest of files captured out of the run (refs, not
    // bytes). Populated from the sidecar's manifest after the run completes;
    // upload failures surface via ArtifactsState, not the run Phase.
    // +optional
    Artifacts []ArtifactRef `json:"artifacts,omitempty"`

    // ArtifactsState is the independent collection outcome:
    // "" (not requested) | Pending | Complete | Partial | Failed. It NEVER
    // affects State/Phase — a post-Complete upload failure must not regress a
    // run that already produced valid Output.
    // +optional
    ArtifactsState string `json:"artifactsState,omitempty"`
```

> **Why a string field, not a `metav1.Condition`.** `RunStatus` uses bare string
> states throughout (`State`, `TerminationReason`) and embeds `pure` types
> directly into the operator API; a string mirrors that and avoids a conditions
> machinery the run status has never used. A richer `[]Condition` is an open
> decision (§10) if the maintainer wants generic condition tooling.

### 5.3 CRD YAML (hand-edited — CRD generation drifts; see project memory)

**Agents** — `operator/config/crd/runtime.agents.smol-agents.ai_agents.yaml`,
add an `artifacts` block as a sibling of `storage` (insert after line 257, before
`instructions:` at `:258`). It reuses the exact `s3` sub-schema already defined
for `storage.agentfs.backup.s3` (`:223-243`):

```yaml
                artifacts:
                  type: object
                  description: |-
                    Workspace files to capture OUT of each run and upload to S3,
                    surfaced as status.artifacts on the AgentRun. Requires
                    storage.kind=agentfs (the sidecar performs the upload).
                  required: [outputs]
                  properties:
                    outputs:
                      type: array
                      minItems: 1
                      items:
                        type: object
                        required: [name, glob]
                        properties:
                          name: { type: string, description: 'Unique rule name; identifies the output in status.artifacts.' }
                          glob: { type: string, description: 'Workspace-relative doublestar glob (e.g. out/**/*.json). No ".." segment.' }
                          maxBytes: { type: integer, format: int64, minimum: 0, description: 'Per-file cap; larger files are skipped (recorded). 0 = default 10MiB.' }
                          contentType: { type: string, description: 'Uploaded object Content-Type; empty sniffs by extension.' }
                    s3:
                      # identical shape to storage.agentfs.backup.s3 above
                      type: object
                      description: Upload target; when omitted, reuses storage.agentfs.backup.s3 under artifacts/<ns>/<run>/.
                      required: [bucket]
                      properties:
                        bucket: { type: string }
                        prefix: { type: string }
                        region: { type: string }
                        endpointURL: { type: string }
                        sseAlgorithm: { type: string, enum: [AES256, "aws:kms", ""] }
                        kmsKeyARN: { type: string }
                        versioning: { type: boolean }
                        credentialsRef:
                          type: object
                          properties:
                            secretName: { type: string }
                            key: { type: string }
```

**AgentRuns** — `operator/config/crd/runtime.agents.smol-agents.ai_agentruns.yaml`,
add to `status.properties` (after `steps:` ends at `:148`):

```yaml
                artifactsState:
                  type: string
                  description: 'Artifact collection outcome (independent of state): "" | Pending | Complete | Partial | Failed.'
                  enum: ["", Pending, Complete, Partial, Failed]
                artifacts:
                  type: array
                  description: Manifest of files captured out of the run (references, not bytes).
                  items:
                    type: object
                    properties:
                      name: { type: string }
                      path: { type: string }
                      s3Bucket: { type: string }
                      s3Key: { type: string }
                      s3VersionID: { type: string }
                      sizeBytes: { type: integer, format: int64 }
                      contentType: { type: string }
                      sha256: { type: string }
                      skipped: { type: string, description: 'Reason a matched file was not uploaded (e.g. over-budget).' }
```

Then regenerate deepcopy: `make -C operator deepcopy` (controller-gen v0.16.5,
`operator/Makefile:22-30`). The operator API embeds `pure` types directly, so the
new fields flow to `operator/api/agentmodel/v1` via deepcopy with no manual
bridge — same mechanism the F2 `Inputs` and the `Steps` work relied on.

### 5.4 Runtime — `CollectArtifacts` in `pkg/agentfs`

New file `pkg/agentfs/artifacts.go`. It is a method/function over the existing
`Manager` (which already holds `S3` + `Spec`), kept pure-ish (filesystem + S3
I/O at the edges, the glob/hash logic testable against `fakes.go`):

```go
// ArtifactManifest is the sidecar→controller payload (JSON in the manifest CM).
type ArtifactManifest struct {
    State string        `json:"state"` // Complete | Partial | Failed
    Refs  []ArtifactRef `json:"refs"`
}

// ArtifactRef mirrors v1.ArtifactRef (kept here to avoid the sidecar importing
// the operator API; the controller maps one→one).
type ArtifactRef struct { /* Name, Path, S3Bucket, S3Key, S3VersionID,
                              SizeBytes, ContentType, SHA256, Skipped */ }

// CollectArtifacts globs workspace per rules, uploads each match via m.S3.Put,
// and returns the manifest. It NEVER returns a fatal error for an individual
// file (over-budget / unreadable → Skipped + State=Partial); it returns an error
// only if it cannot talk to S3 at all (→ State=Failed, caller still exits 0 so
// the pod succeeds).
func (m *Manager) CollectArtifacts(ctx context.Context, workspace string,
    rules []ArtifactRule, dest *S3BackupTarget) (ArtifactManifest, error)
```

Key mechanics:

- **Streaming hash + upload.** Open the file, wrap in `io.TeeReader` →
  `sha256.New()`, pass to `S3.Put`. Note `S3.Put` currently `io.ReadAll`s the body
  (`s3_aws.go:95`) — acceptable per-file under `MaxBytes` (default 10 MiB), unlike
  the multi-GiB whole-volume tar. Record `SizeBytes` from the returned `Version`
  and `S3VersionID` from `Version.ID` (which is `""` on an unversioned bucket —
  the honest value, no fabrication).
- **Glob.** Use a doublestar matcher (`github.com/bmatcuk/doublestar/v4` — check
  go.mod; the stdlib `filepath.Glob` lacks `**`). Walk relative to `workspace`,
  reject any match that escapes via the existing traversal guard.
- **Dest resolution.** `dest` is `Artifacts.S3` if set, else
  `Storage.AgentFS.Backup.S3`. Key = `path.Join(dest.Prefix or "artifacts",
  ns, run, relpath)`. The sidecar already builds an `AWSS3` from
  `AGENTFS_S3_*` env (`main.go:121-128`); add an artifact-dest S3 client only when
  the dest bucket differs from the backup bucket.

### 5.5 Sidecar — wire collection into shutdown (`cmd/agentfs-sidecar/main.go`)

The `serve` verb already runs the scheduler and does a final backup on SIGTERM.
Extend the post-SIGTERM path to *then* collect artifacts and write the manifest.
Order matters: **final backup first** (preserves the F4 RPO guarantee), **then**
artifact collection (a best-effort overlay).

```
serve:
  sched.Run(ctx)              // returns on SIGTERM after the final backup
  if ARTIFACTS configured:
      manifest := mgr.CollectArtifacts(bgctx, mount, rules, dest)  // bgctx, not the cancelled ctx
      writeManifest(manifest) // → manifest CM via kube client, AND stdout
```

- The collection runs on a *fresh background context* (like `finalBackup`'s own
  internal context, `scheduler.go:74-77`), bounded by a new
  `AGENTFS_ARTIFACT_TIMEOUT` (default 60s) that fits inside the 120 s pod grace
  alongside the backup.
- New env the operator injects (mirroring `AGENTFS_*`):
  `AGENTFS_ARTIFACTS` (JSON-encoded `[]ArtifactRule`), and when the artifact dest
  differs from the backup target, `AGENTFS_ARTIFACTS_S3_*` (bucket/prefix/region/
  endpoint/sse/kms) + the dest's `AWS_*`/creds via the same `secretKeyRef`
  projection. `AGENTFS_ARTIFACT_TIMEOUT`.
- `POD_NAMESPACE`/`POD_NAME` via downward API (the sidecar needs them for the
  manifest CM name + the S3 key prefix). The run pod has **no** downward-API env
  today — add it in `storage_mount.go` to the sidecar container only.

### 5.6 Operator — wiring

- **`storage_mount.go` (`AttachStorageFS` / `agentFSContainer`):** when
  `agent.Spec.Artifacts != nil`, append the artifact env to the *serve* sidecar
  container (not init): the `AGENTFS_ARTIFACTS` JSON, the optional dest S3 env, the
  dest creds via `awsCredEnv`, and the `POD_NAMESPACE`/`POD_NAME` downward-API
  vars. New helper `agentFSArtifactEnv(spec *pure.AgentSpec) []corev1.EnvVar`.
- **Sidecar Role/RoleBinding (new builder).** The sidecar must create/update the
  `<run>-artifacts` ConfigMap. Add `BuildArtifactManifestRole(run)` +
  binding in a new `operator/internal/builders/artifact_rbac.go`, bound to the
  run pod's ServiceAccount (`AgentSAName`, `agentrun.go:58`), scoped to
  `configmaps` `create;update;get` in the run namespace, name-restricted where the
  RBAC model allows. Add the matching `+kubebuilder:rbac` marker on the
  AgentRun reconciler. (CRD/RBAC YAML is hand-edited per project memory.)
- **`agentrun_controller.go`:**
  - In the pod-creation branch (after `ensureRunSpec`, `:174`), when
    `agent.Spec.Artifacts != nil`, gather the artifact-dest S3 `credentialsRef`
    into the existing secret-projection path and ensure the new Role/RoleBinding.
  - When `agent.Spec.Artifacts != nil`, set `run.Status.ArtifactsState =
    "Pending"` as the pod is created.
  - New `foldArtifacts(ctx, run)` called from the terminal branches alongside
    `foldRunResult` (`:240,243`): read the `<run>-artifacts` ConfigMap; on
    NotFound and a terminal run, leave `ArtifactsState=Pending` and requeue once
    (the sidecar may still be writing it within the grace window); on present,
    set `run.Status.Artifacts` + `ArtifactsState` from the manifest. **Never touch
    `run.Status.State`.**

### 5.7 File targets (summary)

| File | Change |
|---|---|
| `pkg/agentmodel/v1/storage.go` | + `ArtifactSpec`, `ArtifactRule`; validation helpers |
| `pkg/agentmodel/v1/types.go` | + `AgentSpec.Artifacts`; + `ArtifactRef`, `RunStatus.Artifacts`, `RunStatus.ArtifactsState` |
| `pkg/agentmodel/v1/validation.go` | extend `ValidateAgent`: artifacts ⇒ agentfs + S3 target; glob/name rules |
| `pkg/agentfs/artifacts.go` *(new)* | `CollectArtifacts`, `ArtifactManifest`, glob/hash/upload |
| `pkg/agentfs/types.go` | (no change to `S3.Put`; reused as-is) |
| `cmd/agentfs-sidecar/main.go` | post-SIGTERM collect + write manifest CM; new `AGENTFS_ARTIFACTS*` env; kube client |
| `operator/internal/builders/storage_mount.go` | `agentFSArtifactEnv`; downward-API env on the sidecar |
| `operator/internal/builders/artifact_rbac.go` *(new)* | `BuildArtifactManifestRole` + binding |
| `operator/internal/controllers/agentmodel/agentrun_controller.go` | gather dest creds; ensure RBAC; set `ArtifactsState=Pending`; `foldArtifacts` |
| `operator/config/crd/runtime.agents.smol-agents.ai_agents.yaml` | + `spec.artifacts` schema |
| `operator/config/crd/runtime.agents.smol-agents.ai_agentruns.yaml` | + `status.artifacts`, `status.artifactsState` |
| `operator/api/agentmodel/v1/*` | regenerated via `make -C operator deepcopy` |

---

## 6. Data / control flow

End-to-end for a claude-code agent that writes `report.md` + `out/result.json`:

```
1. Author: Agent{ mode:harness, harness:{kind:claude-code},
                  storage:{kind:agentfs, agentfs:{backup:{s3:{bucket:B}}}},
                  artifacts:{ outputs:[
                     {name:report, glob:"report.md"},
                     {name:results, glob:"out/**/*.json", contentType:application/json}]}}
   → ValidateAgent: artifacts present ⇒ agentfs present ✓, S3 target=B ✓.

2. AgentRun created. Controller:
   - resolveRunSandbox (kata-fc) → ensureRunSpec → AttachStorageFS:
       initContainers = [agentfs-init, agentfs-sidecar(native, AWS_* + AGENTFS_ARTIFACTS + POD_* )]
       containers     = [harness]   (mounts /var/agentfs; NO creds)
   - ensureArtifactRBAC (Role: configmaps create/update on <run>-artifacts)
   - Status.ArtifactsState = "Pending"; create pod.

3. Pod runs. harness (`agent run`) executes in EffectiveWorkingDir = /var/agentfs,
   claude-code writes /var/agentfs/report.md and /var/agentfs/out/result.json,
   then the harness container exits → RunResult to its termination message.

4. Kubelet SIGTERMs the native sidecar (main container has exited):
   - scheduler.finalBackup()                         (existing F4 RPO snapshot)
   - mgr.CollectArtifacts(/var/agentfs, rules, B):
       rule report  → report.md         → Put artifacts/<ns>/<run>/report.md       → ref{sha,ver,size}
       rule results → out/result.json   → Put .../out/result.json (CT json)        → ref{...}
   - write ConfigMap <run>-artifacts = {state:Complete, refs:[...]}  (+ stdout)
   - sidecar exits → pod reaches Succeeded.

5. Controller (pod Succeeded):
   - foldRunResult(pod): Output/Steps/Usage/Phase=Completed   (unchanged path)
   - foldArtifacts(run): read <run>-artifacts CM → Status.Artifacts=[2 refs],
                         Status.ArtifactsState="Complete".     (Phase untouched)

6. `kubectl get agentrun ... -o yaml` shows status.output (the harness summary)
   AND status.artifacts[] with bucket/key/versionID/sha256/size per file.
```

Failure-isolation flow (upload partially fails):

```
4'. CollectArtifacts: report.md uploads; out/result.json is 50 MiB > MaxBytes
    → ref{Skipped:"over-budget"}; or S3 rejects one Put → ref recorded with
    Skipped:"upload error: <…>"; manifest.state = Partial.
5'. foldArtifacts: Status.ArtifactsState="Partial", Status.Artifacts has the
    successful ref(s) + the skipped entries. Status.State stays Completed.
```

---

## 7. Security model

How artifact egress composes with the existing controls, and the new surface.

### Composition with the existing stack

- **kata-fc sandbox (R-SBX-1).** Unchanged. Collection runs in the sidecar, which
  is inside the same microVM as the harness. The sidecar does not weaken the
  boundary; it is `RunAsNonRoot`, drops `ALL` caps (`storage_mount.go:148-153`).
- **Credential broker / SPIFFE.** *Unchanged and deliberately not extended.* S3
  credentials reach the sidecar via `secretKeyRef` env projection
  (`storage_mount.go:183-191`), exactly as the AgentFS backup creds do today — the
  broker is not involved (this is the AgentFS storage credential path, not the
  per-agent provider secret path). The harness container still gets **zero** AWS
  credentials. This is the whole point of putting collection in the sidecar.
- **Egress NetworkPolicy.** The run pod's static default-deny policy already
  permits public 443 (`run_sandbox.go`), so the sidecar's S3 `PutObject` works
  with no policy change. Note (honest): this is the same channel a *malicious*
  harness could already use for arbitrary 443 exfil — artifacts do not widen the
  network surface, because the harness never holds the creds and the sidecar's
  egress was already open to 443. `AgentNetwork` allow-lists are **not** enforced
  on this datapath today, so do not represent them as a control here.

### New attack surface + mitigations

| Surface | Risk | Mitigation |
|---|---|---|
| Harness writes a huge/zip-bomb file into the workspace | Sidecar OOM / runaway upload exhausting the grace window | Per-file `MaxBytes` (default 10 MiB) enforced *before* upload; over-budget → Skipped, not uploaded. `AGENTFS_ARTIFACT_TIMEOUT` (60s) bounds total collection inside the 120s grace. `EmptyDir.SizeLimit` (`storage_mount.go:91-95`) already bounds the workspace. |
| Glob escapes the workspace (`../../etc/...`) | Reads host/sidecar files outside the agent's scope | Glob is workspace-relative; the traversal guard (`safeWorkspacePath`, `runonce.go:167-184`) rejects `..`; validated at admission *and* re-checked at collect time. |
| Harness exfiltrates secrets by writing them to a matched file | Secret material leaves the sandbox via the artifact bucket | This is **inherent to giving an agent file output** — same exposure as the existing AgentFS backup (a harness can already write a secret into the volume that gets snapshotted to S3). Mitigation is policy-level: `RedactionPolicy` is the intended lever but is **applied nowhere today** (`types.go:368-370`); track redaction-on-artifacts under [agentpolicy-enforcement.md](agentpolicy-enforcement.md). Do not claim artifacts are redacted. |
| Sidecar's new ConfigMap-write RBAC | A compromised sidecar could write other ConfigMaps | Role scoped to `configmaps` `create;update;get`, name-restricted to `<run>-artifacts` where the RBAC model permits, namespace-local, bound only to the run SA. The sidecar still has **no** read on secrets and no other verbs. |
| Artifact manifest in broadly-readable `status` | Paths/keys reveal workspace structure | Manifest holds refs only (no file bytes). `S3Key` discloses the path — acceptable (the same path is in the agent's own workspace); the *contents* require S3 read on the bucket, governed by the bucket's IAM, not by the AgentRun reader. |
| Cross-tenant key collision in a shared bucket | Tenant A reads tenant B's artifacts | Key prefix is `artifacts/<namespace>/<run-name>/…`; per-tenant bucket or per-tenant prefix isolation is the operator's IAM responsibility, same as the AgentFS backup bucket today. |

---

## 8. Phasing & effort

Total **L**. Sequenced so each increment is independently shippable and testable.

| # | Increment | Effort | Depends on | Ships |
|---|---|---|---|---|
| 1 | **Types + validation + CRD.** `ArtifactSpec`/`ArtifactRule`/`ArtifactRef`, `RunStatus.Artifacts`/`ArtifactsState`, `ValidateAgent` gates, CRD YAML, deepcopy. No runtime. | **S** | — | The API surface; `kubectl apply` of an artifact-bearing Agent validates. |
| 2 | **`CollectArtifacts` in `pkg/agentfs`.** Glob + per-file `MaxBytes` + streaming SHA-256 + `S3.Put` + manifest, against the in-memory `fakes.go` S3. Pure-logic, fully unit-testable, no k8s. | **M** | 1 | The collector library, green under `go test ./pkg/agentfs`. |
| 3 | **Sidecar wiring.** `AGENTFS_ARTIFACTS*` env parse, post-SIGTERM collect (after final backup), manifest to stdout + (behind a flag initially) ConfigMap. Kube client + downward-API env. | **M** | 2 | The sidecar collects and logs the manifest end-to-end. |
| 4 | **Operator wiring + fold.** `agentFSArtifactEnv`, dest-cred projection, `BuildArtifactManifestRole`, `ArtifactsState=Pending` on create, `foldArtifacts` on terminal. | **M** | 1, 3 | `status.artifacts[]` populated on a real run. |
| 5 | **Hardening.** `MaxBytes`/timeout edge cases, partial-failure semantics, unversioned-bucket `S3VersionID=""` correctness, multi-match naming, retention interaction (artifact keys vs the `agentfs.sqlite` retention sweep — ensure `EnforceRetention` does **not** prune artifact keys). | **S–M** | 4 | Production-ready. |

**Dependencies on sibling specs:**

- **`agentpolicy-enforcement`** ([agentpolicy-enforcement.md](agentpolicy-enforcement.md)):
  redaction of artifact contents and an `AgentPolicy`-level allow/deny on artifact
  egress depend on `RedactionPolicy`/`AgentPolicy` actually being enforced (today
  neither is). Artifact egress ships *without* redaction; policy-gated redaction
  is a follow-up that lands when that spec does.
- **`agentsession-scaling-impl`** ([agentsession-scaling-impl.md](agentsession-scaling-impl.md)):
  durable sessions run many turns against one long-lived workspace + sidecar
  (`RunTurn`, `runonce.go:56-84`). Per-*turn* artifact capture (vs per-*pod*-
  shutdown) needs a turn-boundary collection hook in the session worker rather
  than only SIGTERM. This spec covers the per-run (one-shot AgentRun) case; the
  per-turn extension is called out in §10 and tracked with that spec.

No hard dependency on the in-flight `loop-mode-tools-and-invokers` or
`response-richness` specs — artifacts are orthogonal to tool wiring and Step
richness (they share only the "refs in status, detail elsewhere" philosophy).

---

## 9. Test plan

### Unit (no cluster)

- **`pkg/agentfs/artifacts_test.go`** (the bulk; uses `fakes.go` in-memory S3):
  - Single glob, exact-match → one ref with the rule name; SHA-256 matches the
    file bytes; `SizeBytes` matches; key = `artifacts/<ns>/<run>/<path>`.
  - Doublestar glob (`out/**/*.json`) → multiple refs named `<rule>/<relpath>`,
    lexicographically ordered (determinism).
  - `MaxBytes` exceeded → `Skipped:"over-budget"`, no `Put` call, `state=Partial`.
  - Unreadable file / S3 `Put` error on one of N → that ref `Skipped`, others
    upload, `state=Partial`; total S3 outage → `state=Failed`, no panic.
  - Unversioned bucket (fake returns `Version.ID=""`) → `S3VersionID` empty, not
    fabricated.
  - Traversal: a glob/match resolving outside the workspace is rejected.
  - `ContentType` sniff-by-extension vs explicit override → correct `PutMeta`.
- **`pkg/agentmodel/v1/validation_test.go`:** artifacts without agentfs → error;
  artifacts without any S3 target → error; duplicate rule names → error; `..` in
  glob → error; valid spec → nil.
- **`operator/internal/builders/storage_mount_test.go`:** `agentFSArtifactEnv`
  produces `AGENTFS_ARTIFACTS` JSON on the *serve* sidecar only; downward-API
  `POD_*` present; dest creds projected via `secretKeyRef` (never inlined);
  no artifact env when `Artifacts==nil`. Assert the harness container has **no**
  AWS env (the credential-containment invariant).
- **`operator/internal/builders/artifact_rbac_test.go`:** Role verbs/resources
  scoped to `configmaps` create/update/get; bound to the run SA.
- **Controller `foldArtifacts`:** manifest CM present → `Status.Artifacts` +
  `ArtifactsState` set, `State` unchanged; CM absent on terminal run →
  `ArtifactsState=Pending`, one requeue; `State=Failed` run still folds artifacts
  (collection is independent of run success).

### e2e (the cftest single-node k0s box exists for live verification — see project memory)

- **L1/local (kind + MinIO):** an Agent with `storage.agentfs` + `artifacts` and
  a CLI harness whose entrypoint writes two files; assert (a) pod reaches
  Succeeded (regression guard for the native-sidecar hang), (b) the two objects
  exist in MinIO under `artifacts/<ns>/<run>/…`, (c) `status.artifacts[]` has 2
  refs with matching `sha256`/`sizeBytes`, (d) `status.output` is still folded
  (the result path is untouched), (e) `artifactsState=Complete`.
- **L2/cftest (real S3 on the k0s box, kata-fc):** the proven Hermes-style stack
  but with a CLI harness (claude-code or a fixture) producing a `report.md`;
  verify the full path under kata-fc, including that the 120 s grace covers
  backup + collection, and that a deliberately over-budget file is `Skipped` with
  `artifactsState=Partial` while the run is `Completed`.

---

## 10. Risks & open decisions

**Risks**

1. **Grace-window contention.** The native sidecar must, within 120 s, do the
   final backup *and* collect+upload artifacts. A large workspace backup plus many
   artifacts could exceed it, truncating collection (→ `Partial`/`Pending`). The
   `AGENTFS_ARTIFACT_TIMEOUT` (60s) caps collection, and backup runs first
   (RPO-protected), but a maintainer with large workspaces may need to raise
   `agentFSShutdownGraceSeconds`. The kopia backend (streaming, no in-memory tar)
   reduces backup time and de-risks this materially.
2. **Sidecar gains a kube client + RBAC it never had.** This is the only trust
   widening. Scoped tightly (one ConfigMap, create/update/get), but it is net-new
   apiserver authority for the sidecar. If unacceptable, fall back to manifest-via-
   sidecar-termination-message with a hard ref cap (open decision below).
3. **Manifest ↔ fold race.** The sidecar writes the manifest CM *after* the main
   container exits but possibly *after* the controller first observes
   `PodSucceeded`. `foldArtifacts` tolerates this with `ArtifactsState=Pending` +
   one requeue, but a sidecar that dies before writing the manifest leaves a run
   permanently `Pending`-on-artifacts (run itself is `Completed`). Bound it with a
   max requeue count → `ArtifactsState=Failed` "manifest never written".
4. **Retention sweep eating artifacts.** `EnforceRetention` (`backup.go:93-142`)
   lists/prunes versions of the `agentfs.sqlite` key only — but if artifacts share
   the bucket, a future broadened retention could prune them. Increment 5 must
   ensure retention is key-scoped and never touches `artifacts/…` keys.
5. **HTTP-harness silent no-op.** A Hermes agent with `artifacts` set produces
   zero files (it never writes the FS). Validation can't catch this (mode is
   orthogonal to a workspace). Surface `ArtifactsState=Complete` with an empty
   manifest + a doc note, so it's visibly "nothing matched" rather than broken.

**Open decisions for the maintainer**

1. **Manifest channel: ConfigMap (new sidecar RBAC) vs sidecar termination
   message (capped, zero RBAC)?** This spec recommends the ConfigMap because
   artifact counts are unbounded by design; the cap in the term-message option is
   a real ceiling. But it is the only new authority added — your call on the
   trade.
2. **`ArtifactsState` string vs a generic `[]metav1.Condition` on `RunStatus`?**
   The string matches existing `RunStatus` style; conditions buy generic tooling
   but introduce machinery the run status has never used. Recommend string for v1.
3. **Default S3 target when `Artifacts.S3` is omitted: reuse the AgentFS backup
   bucket (this spec's default) vs require an explicit artifact bucket?** Reusing
   is convenient but mixes snapshot and artifact objects in one bucket (mitigated
   by the `artifacts/` prefix). Require-explicit is cleaner isolation at authoring
   cost.
4. **Per-turn artifacts for durable sessions.** Capture only on pod shutdown
   (this spec) vs also at each session turn boundary? Per-turn needs a hook in the
   session worker (`RunTurn`) and a per-turn key namespace; defer to land with
   [agentsession-scaling-impl.md](agentsession-scaling-impl.md).
5. **Should collection failure ever be visible in the run's primary verdict?**
   This spec hard-isolates it (`ArtifactsState` only). If a class of agents treats
   artifacts as the *primary* deliverable (the run is pointless without them),
   they may want `ArtifactsState=Failed` to surface more loudly (event? printer
   column?). Recommend: keep `Phase` clean, add a printer column for
   `ArtifactsState` + a Warning event on `Failed`/`Partial`.

---

## See also

- [framework-enhancements.md](../design/framework-enhancements.md) — §F3 (this
  spec's origin), §F1/§F2/§F4 (the landed prerequisites).
- [custom-agent-images.md](../design/custom-agent-images.md) — the restricted PSA
  and writable-`/tmp` reality a harness image lives under (artifacts capture what
  it writes into the *bound workspace*, not `/tmp`).
- [agentfs-versioning.md](../research/agentfs-versioning.md) — the kopia backend
  and S3 versioning semantics the manifest's `S3VersionID` rides on.
- [agentpolicy-enforcement.md](agentpolicy-enforcement.md) — where redaction /
  policy-gated artifact egress will land (today unenforced).
- [agentnetwork-datapath-enforcement.md](agentnetwork-datapath-enforcement.md) —
  why "S3 in the egress allow-list" is *not* a live control on this datapath.
- [determinism-and-replay.md](determinism-and-replay.md) — the stable-ordering
  guarantee the manifest provides.
- [agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md)
  — where the file-egress gap was first scored.
- `pkg/agentfs.S3.Put` — `pkg/agentfs/types.go:56-58`, AWS impl
  `pkg/agentfs/s3_aws.go:94-136`.
- AgentFS native sidecar + final-backup hook — `operator/internal/builders/storage_mount.go:105-117`,
  `cmd/agentfs-sidecar/main.go:69-85`, `pkg/agentfs/scheduler.go:32-99`.
