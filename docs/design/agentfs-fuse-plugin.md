# AgentFS versioned ("FUSE") storage plugin — design

Status: proposed · Owner: platform · Date: 2026-05-31
Supersedes nothing; implements `docs/research/agentfs-versioning.md` §6 (Tier 0) against the real `pkg/agentfs` seams.

## 1. Problem

The brief asks for a **versioned, git-like storage plugin for AgentFS**, framed as a "FUSE storage plugin." The agent workspace must support **checkpoints, history, diff, rollback** (and ideally branches) over S3, while respecting the platform's hard constraints. The honest finding — converging with three independently-critiqued designs and the prior research — is that **a real in-pod FUSE mount is infeasible for the default workload, and the blocker is the kata-fc VM boundary, not merely non-root/PSA.** This doc proves that, then specifies the no-FUSE design we should build.

### What ships today (verified against source)
- **Workspace = EmptyDir.** `AttachStorageFS` (`operator/internal/builders/storage_mount.go:79`) appends one `EmptyDir` volume `agentfs` (SizeLimit = `SizeGiB`) and mounts it into every execution container. No FUSE, no `/dev/fuse`, no CSI, no virtiofs — a real, fast, POSIX-correct kubelet FS.
- **Durability = native sidecar that tars to S3.** A regular init container (`init` verb → restore) plus a **native sidecar** (init container + `RestartPolicy=Always`, `serve` verb → backup) — both `RunAsNonRoot`, `AllowPrivilegeEscalation=false`, drop **ALL** caps (`storage_mount.go:148-153`). 120s grace floor (`agentFSShutdownGraceSeconds`, `storage_mount.go:44/117`).
- **Snapshot = whole-tree gzip-tar.** `FilesystemStorage.SnapshotTo` walks `Root` with `filepath.Walk` (`pkg/agentfs/fs_storage.go:28`) — including any live DB. `WALFrames()` is a hard no-op (`fs_storage.go:120`); `WALInterval` is hardcoded `0` (`cmd/agentfs-sidecar/main.go:73`).
- **Versioning = S3 bucket-versioning of one key.** `Manager.keyForFull` always PUTs `<prefix>/agentfs.sqlite` (`pkg/agentfs/backup.go:16-22`); history is S3 object versions; restore picks `latest|versionID|pointInTime` (`pkg/agentfs/restore.go:41-79`). No branches, no diff, no per-change history.
- **The whole tar is buffered in memory.** `Manager.Backup` reads into a `bytes.Buffer` before `S3.Put` (`backup.go:40`) — an OOM risk for large workspaces (F4 deferred-streaming).
- **The `Storage` interface is SQLite-shaped & aspirational.** `types.go:37-52` advertises `SnapshotTo(io.Writer)` "MUST hold a read lock" + SQLite online-backup + `WALFrames`, but the only impl is a file-tree tar. The package doc/comments describe Turso/branchable-SQLite/gRPC that **do not exist** — treat code as truth.
- **Run pod shape.** `RuntimeClassName` defaults to **kata-fc** (`workload.go:44`, `configmap.go:84`), Pod sec uid/gid/fsGroup 65532 + `RunAsNonRoot` + `RuntimeDefault` seccomp, agent container `ReadOnlyRootFilesystem` + drop ALL (`workload.go:82-110`). kata-fc → `configuration-fc.toml` + devmapper thin-pool (`kata_recipe.go:108-113`).
- **Namespace enforces `restricted` PSA** (`deploy/kustomize/base/namespace.yaml:6`).
- **F1/F2 seams.** Harness CWD = `AgentSpec.EffectiveWorkingDir()` (`pkg/agentmodel/v1/harness.go:272`), derived from `DefaultAgentFSMountPath=/var/agentfs` (`storage.go:24`), the same const the operator mount uses. Inputs are written by `MaterializeInputs` (`pkg/agentruntime/runonce.go:108`).

### Requirements
1. Checkpoints (named point-in-time). 2. History. 3. Rollback/recovery. 4. Diff between checkpoints. 5. (Stretch) branches/merge. 6. Live-DB safety. Plus: zero new privilege, broker-only creds, AgentNet egress, multiarch images.

## 2. The three approaches and the sandbox analysis

The "FUSE plugin" question decomposes into: where do `open(/dev/fuse)` + `mount(2)` (CAP_SYS_ADMIN) run, who serves the FUSE event loop, and how does the mount reach the **agent** container?

### Approach A — Privileged CSI node-driver + host FUSE, propagated into the pod (JuiceFS/Everest model)
A privileged node `DaemonSet` (the shape of `ebpf-loader`: `Privileged:true` `ebpfloader.go:152`, `HostPID:true` `:193`, `MountPropagation:Bidirectional` `:129`, hostPath `/dev`) opens `/dev/fuse`, `mount(2)`s a versioned-FS daemon (`hanwen/go-fuse/v2`, never `bazil/fuse`) at a hostPath, surfaces it via `Bidirectional` rshared propagation, and bind-mounts it into the kubelet pod dir; the app declares `mountPropagation: HostToContainer`.

**Sandbox verdict: INFEASIBLE under kata-fc.** The agent runs in a **separate Firecracker guest kernel**. Firecracker implements only virtio-block/net/vsock — **no virtio-fs/9p** (verified: zero `virtiofs|virtio_fs|9p` matches in `operator/` and `pkg/`). A host mount created at `NodePublishVolume` (after the guest boots) does **not** propagate into the running guest (kata #10502); kata *copies* non-block volumes host→guest. So the privileged driver pays a persistent **node-wide root-equivalent** blast radius and **still cannot** make the mount appear in the kata pod. lakeFS's own Everest CSI (a privileged host-systemd `everest` + Bidirectional propagation) fails for the identical reason, and its mount is Enterprise-only anyway.

### Approach B — In-pod FUSE sidecar (the literal request), incl. fd-passing
Either a privileged sidecar in the run pod (`privileged:true` + `/dev/fuse` hostPath), or the **fd-passing** split (meta-fuse-csi / gcsfuse CSI): a privileged node DaemonSet does `open(/dev/fuse)`+`mount(2)` and `SCM_RIGHTS`-passes the fd over a UDS to an **unprivileged** sidecar that runs the FUSE server on `/dev/fd/N`.

**Sandbox verdict: INFEASIBLE under kata-fc.** A privileged/CAP_SYS_ADMIN sidecar is rejected at admission by `restricted` PSA (`namespace.yaml:6`) and by the pod's drop-ALL/`RunAsNonRoot` (`workload.go:82-110`). fd-passing is the *only* known non-root in-pod FUSE technique on **runc**, but an fd is a **kernel-local** handle: it cannot cross the Firecracker VM boundary, and there is no `/dev/fuse` + CAP_SYS_ADMIN inside the guest to re-mount it. Running the whole FUSE stack *inside* the guest would require shipping `fuse.ko` + exposing `/dev/fuse` + granting CAP_SYS_ADMIN + a relaxed PSA in every microVM — exactly the privilege/blast-radius widening the platform forbids.

### Approach C — No-FUSE EmptyDir + userspace commit layer (RECOMMENDED)
Keep the workspace as the kubelet **EmptyDir** (already present in the guest as an *initial* volume, devmapper-backed — the only sharing Firecracker supports). Replace only the **durability/versioning backend** behind a new `VersionedStore` seam in `pkg/agentfs`, driven by the **same unprivileged native sidecar**: `init` restores a checkpoint into the EmptyDir → agent works on a real local FS → `serve` checkpoints on each Scheduler tick and on SIGTERM.

**Sandbox verdict: WORKS. Privilege added = zero.** The agent (uid 65532) reads/writes a real local FS — `rename(2)`, `flock`, `mmap`, append, hardlinks all work (strictly better than mountpoint-s3/GeeseFS, which lack most of these). The engine binary (kopia) is pure userspace: `open/read/write` on the mounted dir + HTTPS to S3 with broker-projected creds. No `mount(2)`, no `/dev/fuse`, no caps, no node DaemonSet, no mount propagation. The init+sidecar keep `storage_mount.go:148-153`'s SecurityContext byte-for-byte. The trade-off — no lazy/live write-through of an arbitrarily-large dataset — is irrelevant to the bounded restore→work→checkpoint run model.

## 3. Comparison

| Dimension | A: Privileged CSI host-FUSE | B: In-pod FUSE (incl. fd-pass) | C: No-FUSE EmptyDir + commit (REC) |
|---|---|---|---|
| Works under kata-fc | **No** (no virtio-fs; kata #10502) | **No** (fd is kernel-local; PSA blocks privileged) | **Yes** |
| New privilege | Node-wide root-equiv DaemonSet | Privileged sidecar / privileged node helper | **Zero** |
| Passes `restricted` PSA on agent ns | Driver no (separate ns) | **No** | **Yes** |
| POSIX correctness | Full (but unreachable in kata) | S3-FUSE gaps (no rename/lock/mmap) | **Full (real local FS)** |
| Checkpoints / history / rollback | Engine-dependent | Engine-dependent | **Yes (kopia)** |
| Diff | Engine-dependent | Engine-dependent | **Yes, content-hash (kopia)** |
| Branches/merge | lakeFS-dependent | — | Deferred (lakeFS Tier 3) |
| Live-DB safe | No (mount worsens it) | **No (worse)** | **Yes (exclude + WAL lane)** |
| Fixes whole-tar-in-memory OOM | n/a | n/a | **Yes (streaming)** |
| Reuses shipped wiring | New CSI + webhook | New privileged path | **EmptyDir + native sidecar + 120s grace verbatim** |
| Effort | L (and dead-ends) | L (and dead-ends) | **M (kopia v1); L with lakeFS** |

## 4. Recommendation

**Build Approach C with kopia as the single v1 backend; defer lakeFS.** kopia delivers 5 of 6 requirements for files at sidecar-level complexity with a single static multiarch binary and **no server**, reuses the entire shipped pod contract, and retires two real bugs (whole-tar-in-memory OOM `backup.go:40`; live-DB torn-write `fs_storage.go:28`). lakeFS is the only path to cross-agent branches/merge but adds a server + Postgres/KV and has a heuristic diff that can miss same-size/same-second edits — gate it behind the same seam until cross-agent branching is a confirmed need. **Approaches A and B are documented as infeasible for the default workload and are out of scope.** A true live mount, if ever required for an unstageably-large dataset, is a deliberate, security-reviewed exception on a **non-kata runc node pool** (reuse JuiceFS CSI / meta-fuse fd-passing) — never the default, never inside kata-fc.

## 5. Interface into `pkg/agentfs` + the operator

The shipped `Storage` is `SnapshotTo(io.Writer)/RestoreFrom(io.Reader)` (`types.go:39-52`) and `Manager` always PUTs **one** key (`backup.go:16`). A repo-shaped engine (kopia repo / lakeFS commit graph) owns its own multi-object layout and **cannot** be a single `io.Writer`. So we add a **sibling** capability, not a contorted `Storage` impl.

### New seam (`pkg/agentfs/versioned.go`)
```go
type VersionedStore interface {
    Connect(ctx) error
    Restore(ctx, ref, dst string) (Checkpoint, error)         // init verb
    Checkpoint(ctx, src, msg string) (Checkpoint, error)      // serve tick + SIGTERM; streams
    History(ctx) ([]Checkpoint, error)
    Diff(ctx, a, b string) ([]FileChange, error)
    GC(ctx, RetentionSpec) error                              // replaces EnforceRetention
}
```
- `Manager` (`types.go:91-98`) grows an optional `Backend VersionedStore`. When non-nil it **supersedes** the tar path: `Manager.Backup → Backend.Checkpoint`, `Manager.Restore → Backend.Restore`, `EnforceRetention → Backend.GC`. When nil, the legacy `FilesystemStorage` tar path stays (back-compat).
- `RestorePolicy` (`restore.go:41-79`) maps onto `VersionedStore`: `versionID`→`ref=<checkpoint ID>`, `pointInTime`→caller resolves to the most-recent `Checkpoint.CreatedAt <= T`, `latest`/`""`→`ref="latest"`. `ifMissing fresh|fail` unchanged.
- The `S3` interface (`types.go:54-73`) is unused on the engine path (kopia owns its object I/O); `checkPolicy`'s `HasVersioning` gate becomes advisory.
- **Live-DB:** the file backend applies `ExcludeGlobs` (`*.db/*-wal/*-shm/*.sqlite*`) so the file history never contains a torn DB; the DB is routed to a Tier-2 lane. The existing dead `Storage.WALFrames()` (`fs_storage.go:120`) + reserved `WALSnapshotInterval` (`storage.go:79-83`) + `Scheduler.WALInterval` (`scheduler.go:13`) are the natural home for a future `LitestreamStore`.

### kopia backend (`pkg/agentfs/kopia_store.go`)
Shell out to a multiarch `kopia` binary baked into the `agentfs-sidecar` image (simplest; matches research §6.1), or embed `github.com/kopia/kopia`. Config from the existing `AGENTFS_S3_*` env + `CredentialsRef` (`storage_mount.go:160-188`); repo under `<prefix>/kopia/` (legacy `agentfs.sqlite` kept only as defense-in-depth). Method→command map: `Connect`→`repository connect/create s3`, `Restore`→`restore <id|latest> <dst>`, `Checkpoint`→`snapshot create --description=<msg> <src>` (streams; `.kopiaignore` from `ExcludeGlobs`), `History`→`snapshot list --json`, `Diff`→`diff <a> <b>`, `GC`→`policy set --keep-latest N` + `maintenance run`.

### Operator (`operator/internal/builders/storage_mount.go`) — reused verbatim
EmptyDir volume, regular-init(`init`)+native-sidecar(`serve`), `ensureGracePeriod(120s)`, env-for-dest + `secretKeyRef`-for-creds, idempotent attach on `agentfs`, mount into every execution container — **all unchanged**. Only additions: a new `AGENTFS_BACKEND` env + per-agent repo prefix. **No pod-shape change**, so the kata/non-root SecurityContext is untouched.

### CRD (`pkg/agentmodel/v1/storage.go`)
Add `AgentFSSpec.Backend` enum `{tar|kopia|lakefs}`, `+kubebuilder:default:="tar"` (preserve current behavior). `cmd/agentfs-sidecar/main.go:92` (`buildManager`) selects the `VersionedStore` from `AGENTFS_BACKEND`; `init`→`Restore`, `serve`→`Scheduler` with `FullInterval` driving `Checkpoint` + `BackupOnShutdown` on SIGTERM (existing F4 wiring, `scheduler.go:51-54`). `WALInterval` stays 0 until the DB lane lands. Fix the aspirational SQLite/WAL comments in `storage.go`/`types.go`/`doc.go`. Add an optional `DB` sub-spec (`engine: litestream|turso|dolt` + creds ref) for the Tier-2 lane.

## 6. Composition with F1/F2/F4 and the kopia/git tiers

- **F1** (`EffectiveWorkingDir`=/var/agentfs, `harness.go:272`): unchanged — the engine restores/commits the **same** dir; the single `DefaultAgentFSMountPath` const keeps harness CWD and operator mount from drifting.
- **F2** (`MaterializeInputs`, `runonce.go:108`): unchanged — inputs overlay onto the restored tree (with Tier-1 git they'd land in the initial commit).
- **F4** (native sidecar + final-backup-on-SIGTERM + 120s grace): the lifecycle the kopia sidecar reuses; the final `Checkpoint` replaces the final tar, **and** streaming retires F4's deferred buffering risk.
- **Tiers** (`docs/research/agentfs-versioning.md` §6): this is **Tier 0** (kopia) done concretely. **Tier 1** (git commit-per-Step, mapping to `RunStatus.Steps`) and **Tier 2** (Litestream/Turso/Dolt for the DB) compose behind the same `VersionedStore`/`WALFrames` seams. **Tier 3** (lakeFS shared data plane) is the deferred branch/merge option — a separate cluster service (server + Postgres/KV), reached via `lakectl local` clone/commit, never a sidecar in a `RestartPolicy=Never` pod.
- **Which tree:** only the storage `agentfs` volume; the memory `memory-agentfs` mount (`memory_mount.go`) shares the image but is a distinct volume — the backend must target `storageFSVolumeName`'s mount to avoid double-committing.

## 7. Privilege & integration accounting (brutally honest)
- **Added privilege on the run-pod path: zero.** init+sidecar keep drop-ALL / `AllowPrivilegeEscalation=false` / `RunAsNonRoot` (`storage_mount.go:148-153`); only relaxation is `ReadOnlyRootFilesystem=false` on helpers (already true today) so the engine writes its cache. The agent container stays `ReadOnlyRootFilesystem=true`.
- The platform's **one** privileged component is `ebpf-loader` (`Privileged:true` `ebpfloader.go:152`, `HostPID` `:193`, `Bidirectional` `:129`) — orthogonal infra this design neither adds to nor changes. It is also the proof that even a maximally-privileged node DaemonSet cannot cross the kata-fc boundary.
- **Creds:** S3 auth is projected via `secretKeyRef` from `CredentialsRef` (`storage_mount.go:180-187`). NB: as wired this is a static long-lived key from a referenced Secret, **not** a SPIFFE-broker dynamic mint — the design should either align with the broker or stop calling it "broker-minted." kopia/lakeFS must source creds the same way.
- **Egress:** kopia/lakeFS talk straight to S3 (lakeFS via presigned URLs). AgentNet eBPF egress is **CIDR-based**, not DNS/FQDN, and S3 lives behind rotating DNS / large dynamic prefixes — pin down the S3 CIDR / VPC-endpoint allow-list **before** enabling, or egress is silently blocked.
- **Images:** the `kopia` binary must be built/baked **multiarch (amd64+arm64)** (deploy targets span Graviton + amd64 gti).

## 8. Open questions
1. **kopia: shell-out vs. embed?** Shell-out is simplest and matches §6.1; embedding `github.com/kopia/kopia` is a large dep but avoids a second binary. (See build notes.)
2. **Do agents keep a live `*.db`?** If yes, Tier 2 (Litestream) is mandatory and shapes v1; if "just files," exclusion alone suffices for correctness but the DB is then not durably versioned.
3. **Branches now or later?** kopia has none; only lakeFS (Tier 3) or git (Tier 1) do. Is per-session/speculative branching a near-term need (A2A / human-in-the-loop forks)?
4. **Who checkpoints — runtime or agent?** Runtime-driven (tick + SIGTERM, optionally per-Step) is automatic and uniform; agent-driven is flexible but unreliable. Recommend runtime-driven.
5. **Credential story:** align S3 creds with the actual SPIFFE broker, or document that AgentFS uses a static `secretKeyRef` key.
6. **S3 egress allow-list:** which CIDRs / VPC endpoint does AgentNet permit, given eBPF is CIDR-only?
7. **Redaction:** history/diff surfaces file contents into a versioned store; the retention/redaction story (cf. `RedactionPolicy` stub) needs answering before enabling history if runs handle secrets-in-files.
8. **RPO:** checkpoints are boundary-based; a hard crash (not graceful SIGTERM) loses work since the last tick. Accept, or add commit-per-Step (Tier 1) / continuous WAL (Tier 2)?
