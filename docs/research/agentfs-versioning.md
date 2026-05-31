# AgentFS Versioning: Git-like Checkpoints, Diff, Rollback + S3 — Research & Recommendation

> Status: research / decision-support, 2026-05-31 (HEAD `7be83a1`). Scope: what a
> versioned, git-like AgentFS could be — checkpoints, recovery, per-change history,
> diff, rollback, and remote S3 backup — and which existing systems fit our model
> (per-run pods on an EmptyDir, kata-sandboxed, non-root) vs. what we'd build.

---

## 1. The problem

**AgentFS today** (after F4): an agent's workspace is a Kubernetes **EmptyDir** mounted into the run pod. Durability is a **native sidecar** (`agentfs-sidecar`, `pkg/agentfs.FilesystemStorage`) that:

- on `init`: downloads the latest snapshot from S3 and extracts it into the mount (or starts fresh);
- on a schedule **and** on SIGTERM (F4): **gzip-tars the whole mount** and `PutObject`s it to one S3 key (`agentfs.sqlite`), relying on **S3 bucket versioning** for history.

That gives crude *checkpoint + recovery* but **none of the git-like properties**: no per-change history, no diff between versions, no branches, no merge, no rollback-to-an-arbitrary-point beyond "restore an S3 object version." It also has two known limitations (see §2): it **buffers the whole tar in memory** (F4 deferred streaming), and it **cannot safely snapshot a live SQLite/WAL database**.

**The goal.** Six properties:

1. **Checkpoints** — capture state at meaningful points.
2. **Recovery** — restore to a checkpoint.
3. **Every change visible** — a history/audit log of what changed.
4. **Diff** — see *what* changed between two points.
5. **Rollback** — revert to a prior state (ideally with branches).
6. **Remote backup to S3.**

---

## 2. Two tensions that shape every option

**A. Granularity: snapshot vs. every-write.** Snapshot tools (tar, restic, kopia, ZFS) capture at checkpoints — you lose intra-checkpoint history. Capturing *every* change means either **WAL streaming** (for a DB) or **commit-per-step** (git after each agent action). Requirements #3 ("every change") and #4 ("diff") are the hard ones; most backup tools satisfy #1/#2/#5/#6 but not #3/#4.

**B. Data model: files vs. structured.** "See the diff" is **object/line-level** for files but **cell/row-level** for a database. The agent workspace is usually *both*: arbitrary working files (code, outputs) **and** sometimes a SQLite `state.db`. No single tool versions both well.

**The live-SQLite hazard (load-bearing).** Tarring — or restic/kopia/git-adding — a **live** WAL-mode SQLite DB (`*.db` + `-wal` + `-shm`) mid-write captures a torn, inconsistent set → restore yields a **corrupt DB**. This is the same failure surfaced in the Hermes↔AgentFS review. **A correct design separates the DB from the files:** a DB-aware tool (WAL stream / branchable DB) for `*.db`, a file tool (snapshot / git) for everything else.

---

## 3. Landscape

| System | Model | Checkpoint | Every change | Diff | Rollback / branch | S3 | License |
|---|---|---|---|---|---|---|---|
| **git** (+lfs/annex) | files | commits | ✅ per-commit | **line-level** | branch + revert | via remote (DVC / git-remote-s3) | OSS |
| **lakeFS** | objects on S3 | commits | per-commit | object-level | branch + **merge** + revert | **native (S3 *is* the store)** | Apache-2.0 (mount = Enterprise) |
| **Dolt** | SQL tables | commits | per-commit | **cell-level** | branch + merge + push/pull | remotes | Apache-2.0 |
| **Turso / libSQL** | SQLite DB | branch + PITR | WAL (bottomless) | — | **COW branch + point-in-time restore** | bottomless → S3 | OSS + managed |
| **Litestream** | SQLite DB | continuous WAL | ✅ **WAL frames** | — | PITR (no branch) | **native** | Apache-2.0 |
| **restic / kopia** | files | snapshots | per-snapshot | file-level (`diff`) | restore any snapshot (no branch) | **native** (kopia best) | OSS |
| **borg** | files | snapshots | per-snapshot | file-level | restore | ✗ native (needs rclone) | OSS |
| **ZFS / btrfs** | block FS | **instant COW** | per-snapshot | `zfs diff` | **rollback + clone (branch)** | send/recv (not native S3) | OSS |
| **JuiceFS** | POSIX FS on S3 | snapshot/clone | ❌ | ❌ | restore + COW clone | **native** | Apache-2.0 |
| **Pachyderm / DVC** | files/repos | commits | per-commit | file-level / provenance | versioned / git revert | S3-backed | OSS |

**Common substrate:** content-addressed storage (git, restic, kopia, Dolt's prolly/Merkle trees, Noms) is what makes dedup + integrity + cheap diffs fall out — the right foundation if we ever build our own.

### Mapping to the six requirements

| | checkpoint | recovery | every change | diff | rollback | branch | S3 |
|---|---|---|---|---|---|---|---|
| **git + remote** | ✅ | ✅ | ✅ | ✅ line | ✅ | ✅ | ✅ |
| **lakeFS** | ✅ | ✅ | ✅ | ✅ object | ✅ | ✅ +merge | ✅ |
| **kopia / restic** | ✅ | ✅ | ⚠️ per-snapshot | ✅ file | ✅ | ❌ | ✅ |
| **Litestream** (DB) | ✅ | ✅ | ✅ WAL | ❌ | ✅ PITR | ❌ | ✅ |
| **Turso/libSQL** (DB) | ✅ | ✅ | ✅ WAL | ❌ | ✅ | ✅ | ✅ |
| **Dolt** (DB) | ✅ | ✅ | ✅ | ✅ cell | ✅ | ✅ +merge | ✅ |
| **JuiceFS** | ✅ | ✅ | ❌ | ❌ | ✅ | ~clone | ✅ |
| **AgentFS today** | ✅ | ✅ | ❌ | ❌ | ⚠️ S3 obj ver | ❌ | ✅ |

---

## 4. lakeFS vs. JuiceFS (the two most-asked-about)

They share a substrate (S3 + a metadata DB) but do **opposite jobs**:

> **lakeFS *versions* your data (git for the bucket). JuiceFS *stores* your data (a filesystem made of the bucket).**

- **lakeFS** is a metadata overlay giving **git semantics** (commit/branch/merge/diff/revert) over object storage. Your **real objects stay in the bucket** (pointers + zero-copy branches). **No POSIX in OSS** — you use the S3 gateway, `lakectl`, the SDK, or **`lakectl local`** (clone → work locally → commit back; the free, mount-free workflow). A **FUSE mount exists but is Enterprise-only**. It's a **server + metadata store** (Postgres/KV) to operate.
- **JuiceFS** is a **POSIX filesystem** on S3: files are **sharded into ~4 MiB opaque blocks** (not readable as your files in the bucket) with a **metadata engine on the hot path** (Redis/MySQL/TiKV…, must be HA). Strong POSIX (locking, rename, mmap), FUSE/CSI/S3-gateway. Versioning is **snapshot + COW clone only — no diff, no merge, no commit history.**

**Punchline for our git-like goal:** JuiceFS is **not a substitute** for the version-control part — it lacks **diff** and **per-change history** (our two hardest requirements). lakeFS hits all six. JuiceFS's value is the *mount* (POSIX-over-S3), the one thing lakeFS OSS doesn't give. They're conceptually complementary but not stackable (lakeFS wants real objects; JuiceFS's blocks are opaque).

---

## 5. Why **not** a hand-rolled (or any) FUSE mount

Tempting because lakeFS's mount is paid — but FUSE fights our model on three fronts:

1. **Sandbox/privilege.** Mounting FUSE needs `/dev/fuse` + `CAP_SYS_ADMIN` (or fiddly unprivileged-FUSE). Run pods are `RunAsNonRoot:65532`, **drop ALL caps**, restricted PSA, inside a **kata-fc microVM**. The only way in is a **privileged node-level CSI driver** that mounts on the host and bind-propagates into the pod — exactly the privilege-widening the platform avoids, and under kata it must cross the VM boundary (virtiofs). "Our own FUSE mount" really means "a privileged host daemon."
2. **Live-SQLite fragility.** WAL/`-shm`/mmap/locking over FUSE is unreliable — the §2 hazard again.
3. **We don't need it.** The workspace is **already** a real, fast, POSIX-correct local FS (the EmptyDir). Versioning is a **commit layer on top**. FUSE would only buy *lazy, live, write-through S3*, which the bounded run model (**restore-at-start → work → commit-at-end**) doesn't require.

**If a live mount is ever genuinely needed** (a dataset too big to stage in the EmptyDir): **reuse JuiceFS** (Apache-2.0, has a k8s **CSI driver** + snapshots) rather than writing our own — accepting the privileged CSI component + metadata engine + opaque blocks. Do not hand-roll FUSE.

---

## 6. Recommendation — tiered, lightest-first

The model to keep: **EmptyDir workspace (real local FS) + a commit/snapshot layer that ships to S3.** Improve the commit layer; don't change the workspace into a network FS.

**Tier 0 — replace the tar backend with `kopia` (or `restic`). _Effort: M. Do this first._**
Biggest bang, still a sidecar. Immediately gains over the current `FilesystemStorage`:
- **dedup + incremental** (content-defined chunking) instead of full gzip tars;
- **snapshot history** + **restore any version** (recovery, rollback);
- **`diff` between snapshots** (file-level — requirement #4 for files);
- **encryption** + native S3 (kopia has the best S3 support);
- **streaming** (chunks, no whole-tar-in-memory) — this **also fixes F4's buffering risk**.
Delivers 5 of 6 requirements for the *files* (all but git-branches). Sketch in §6.1.

**Tier 1 — `git` in the workspace, commit-per-Step. _Effort: M. The truest "git-like."_**
Have the runtime `git commit` after each executor Step and push to an S3-backed remote (DVC or git-remote-s3). Gains **per-change history + line-level diff + branches + rollback** for code/text — and it maps 1:1 onto the executor's Step loop (commit ↔ Step) and `RunStatus.Steps` (O1). Branches enable speculative / parallel agent work. Weak for large binaries (needs lfs/annex) and live DBs — so pair with Tier 2.

**Tier 2 — handle the DB separately. _Effort: M–L. Required if agents keep a live `*.db`._**
Never tar/snapshot a live DB. Options: **Litestream** (continuous WAL→S3, PITR — every change, recover to any point; no diff) or move structured state to **Turso/libSQL** (branch + PITR) or **Dolt** (cell-level diff + branch/merge).

**Tier 3 — lakeFS as a shared, multi-agent versioned data plane. _Effort: L–XL. Only if warranted._**
If AgentFS becomes a *shared* S3-resident data layer across agents (cross-agent branches/merge), lakeFS is the only natively-git-over-S3 option. Heavier (a service + metadata store). Use **`lakectl local`** (clone-commit) in the run lifecycle — the OSS, mount-free equivalent of the Enterprise mount.

**Sweet spot: Tier 0 (kopia) + Tier 1 (workspace git) + Tier 2 (Litestream for any DB).** Genuine git-like checkpoints, line diffs, rollback, and per-change history at sidecar-level complexity, composing with the shipped F1/F2/F4 work — without FUSE, a server, or the live-DB hazard.

### 6.1 Tier-0 kopia design sketch (drop-in for the existing sidecar)

Today (`pkg/agentfs`): `Storage` interface = `SnapshotTo(io.Writer)` / `RestoreFrom(io.Reader)` (tar); `Manager.Backup()` buffers the tar and `S3.Put`s it; `Scheduler` ticks + (F4) final-backup-on-SIGTERM; `cmd/agentfs-sidecar` runs `init`/`serve`.

Change: add a `KopiaStorage` (or a `Manager` backend) that shells out to / embeds kopia against the same S3 destination the operator already wires (`AGENTFS_S3_*` env, `CredentialsRef`):

- `init` → `kopia repository connect s3 …` then `kopia restore <latest>` into the mount (replaces tar download+extract).
- `serve` → on tick **and** SIGTERM → `kopia snapshot create <mount>` (replaces `Backup()`'s tar+Put). Kopia handles dedup/incremental/streaming/encryption.
- New capabilities exposed up the stack: `kopia snapshot list` → `RunStatus`/an `AgentFS` history; `kopia diff <a> <b>` → a diff API; `kopia restore <id>` → rollback to any checkpoint.
- Keep S3 bucket versioning as defense-in-depth; keep the native-sidecar lifecycle + 120s grace from F4 (kopia's final snapshot replaces the final tar). Exclude `*.db`/`*-wal`/`*-shm` from the file snapshot and route the DB through Tier 2.

CRD: optionally add `storage.agentfs.backend: tar|kopia` (default `tar` for back-compat) so it's opt-in.

---

## 7. How it composes with shipped work

- **F1** (`EffectiveWorkingDir`) — already gives a single workspace path the commit layer versions.
- **F2** (`MaterializeInputs`) — input files seed the workspace; with Tier-1 git they'd land in the initial commit (visible in the run's history).
- **F4** (native sidecar + final-backup-on-SIGTERM + 120s grace) — the lifecycle a kopia/Litestream sidecar reuses verbatim; Tier-0 also retires F4's deferred "stream the snapshot" risk.
- **O1** (`Steps` on the wire) — the natural trigger for Tier-1 commit-per-Step (one commit per `Step`).

---

## 8. Open questions / decisions

1. **Do agents keep a live `*.db`?** If yes, Tier 2 (Litestream/Turso/Dolt) is mandatory and shapes everything; if the workspace is "just files," Tier 0+1 suffice.
2. **Branches now, or later?** kopia (Tier 0) has no branches; git (Tier 1) and lakeFS (Tier 3) do. Is per-session/speculative branching a near-term need (e.g. for A2A / human-in-the-loop forks)?
3. **Who commits — runtime or agent?** Tier-1 commit-per-Step in the runtime is automatic + uniform; letting the agent commit is more flexible but unreliable. Recommend runtime-driven.
4. **Build vs. buy for diff/history.** kopia/git give it for free; a bespoke content-addressed store on S3 (restic-style) is more control but real work — not worth it unless kopia/git prove insufficient.
5. **Redaction.** History/diff surfaces file contents into a versioned store; if runs handle secrets-in-files, the retention/redaction story (cf. the `RedactionPolicy` stub) needs answering before enabling history.

---

## 9. Sources

[lakeFS](https://github.com/treeverse/lakeFS) · [lakectl local](https://lakefs.io/blog/work-with-data-locally/) · [Dolt](https://github.com/dolthub/dolt) · [Litestream](https://litestream.io/how-it-works/) · [Turso branching + PITR](https://turso.tech/blog/turso-now-supports-database-branching-and-point-in-time-restore-eaadb8c4dce5) · [restic/kopia/borg comparison](https://computingforgeeks.com/borg-restic-kopia-comparison/) · [JuiceFS](https://github.com/juicedata/juicefs) · [OpenZFS send/receive](https://openzfs.github.io/openzfs-docs/man/master/8/zfs-receive.8.html) · [DVC](https://doc.dvc.org/start) · [Pachyderm](https://www.pachyderm.com/blog/data-versioning-comparing-dvc-with-pachyderm/)
