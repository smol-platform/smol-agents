# Requirements — smol-agents-memory

## Alignment with Product Vision

Give smol-agents a **first-class memory layer** that agents reach through the
**Model Context Protocol (MCP)** — standard tools/resources — instead of a custom
SDK. The operator is the **control plane**: it manages memory backends (vector
DB / graph / KV / event log) and provisions **retrieval workers** (the data
plane). A thin **MCP server** is the agent-facing **interface**: it translates
MCP calls into the internal retrieval API and enforces identity, tenant
isolation, quotas, audit, and policy. It owns **no indexing logic**.

```
Agent / IDE / runtime
   │  MCP (tools + resources)
   ▼
memory-mcp server         ← agent/tool interface (thin gateway: authz/tenant/quota/audit)
   │  internal retrieval API (gRPC, mTLS)
   ▼
retrieval workers         ← data plane (embed, chunk, index, retrieve)
   │
   ▼
Vector DB / graph / KV / event log   ← backends (declared by MemoryStore)
```

This composes with what already exists: the **operator** (control plane +
CRD reconciliation), the **agent-model** CRDs (`ModelProvider` for embeddings,
`Tool kind=mcp` for the client side), the **secret broker** (`pkg/secrets`, for
backend credentials), **SPIFFE identity** (`pkg/identity`), and the **agentnet**
egress proxy (agents reach the MCP server through their identity-aware sidecar).

The clean boundary: **Memory Operator = control plane; Retrieval Workers = data
plane; MCP Server = agent interface.**

Depends on: **smol-agents-operator** (reconciliation spine), **smol-agents-agent-model**
(`ModelProvider`, `AuthRef`→broker, the runtime CRD family). Optional compose:
**smol-agents-trat-egress** (TraT-authorized writes/deletes),
**smol-agents-secretless-egress** (broker-minted backend credentials).

## Requirements

### R-MEM-API: Configuration surface (CRDs)

#### R-MEM-API-1 — MemoryStore (backend declaration)
**User Story:** As a platform operator, I declare a memory backend once so
retrievers and workers can bind to it without embedding connection details in
agents.

**Acceptance Criteria:**
- The system SHALL provide a `MemoryStore` CRD (group `runtime.agents.stigen.ai`)
  declaring a backend `kind` ∈ {`vector`, `graph`, `kv`, `eventlog`, `filesystem`},
  an endpoint, a driver (e.g. `pgvector`, `qdrant`, `neo4j`, `redis`, `agentfs`),
  and a credential `AuthRef` resolved via the secret broker (never a literal
  secret). The `filesystem`/`agentfs` modality is specified in R-MEM-FS.
- A `MemoryStore` SHALL declare its `tenancy` model (`shared` with a tenant
  label key, or `dedicated`) so cross-tenant isolation can be enforced downstream.
- Validation SHALL reject a `MemoryStore` whose `kind`/`driver` pair is unknown
  or whose `AuthRef` is empty when the driver requires credentials.

#### R-MEM-API-2 — MemoryRetriever (named retrieval pipeline)
**User Story:** As a developer, I reference a named retriever (e.g.
`prod-agent-knowledge-default`) from an MCP call and get a consistent retrieval
pipeline.

**Acceptance Criteria:**
- The system SHALL provide a `MemoryRetriever` CRD that binds one or more
  `MemoryStore`s, an embedding `modelProviderRef` (existing `ModelProvider`),
  default `topK`, a `namespaces` allow-list, a `tenant` scope, and chunking
  parameters.
- A `MemoryRetriever`'s `metadata.name` (namespace-qualified) SHALL be the
  `retrieverRef` an MCP client names; the MCP server SHALL reject calls naming a
  non-existent or unauthorized retriever.
- `MemoryRetriever` SHALL carry a `policy` block (which SPIFFE identities may
  perform which operations on which namespaces) consumed by R-MEM-AUTH.

#### R-MEM-API-3 — Status + reproducibility
**Acceptance Criteria:**
- Both CRDs SHALL surface a `status` with `phase` (`Pending`/`Ready`/`Degraded`),
  bound worker count, and last-reconcile conditions.
- The CRDs SHALL be reproducible from Go source via controller-gen (kubebuilder
  markers, deepcopy), consistent with the existing CRD family.

### R-MEM-MCP: Agent-facing MCP server

#### R-MEM-MCP-1 — Tools
**User Story:** As an agent, I use standard MCP tools to read and write memory.

**Acceptance Criteria:**
- The MCP server SHALL expose the tools: `retrieve_memory(query, topK, filters,
  retrieverRef)`, `write_memory(content, namespace, metadata, retrieverRef)`,
  `list_memory_namespaces(retrieverRef)`, `get_memory(id)`, `delete_memory(id)`,
  `summarize_memory(query, retrieverRef)`.
- Each tool SHALL advertise a JSON Schema for its arguments and results
  (discoverable via MCP `tools/list`).
- `topK` SHALL be clamped to the retriever's quota ceiling (R-MEM-QUOTA);
  `filters.tenant` supplied by the caller SHALL be overridden/validated against
  the caller's attested tenant (R-MEM-AUTH), never trusted as-is.

#### R-MEM-MCP-2 — Resources
**Acceptance Criteria:**
- The MCP server SHALL expose resources: `memory://namespaces/{namespace}`,
  `memory://retrievers/{retrieverRef}`, `memory://documents/{id}`,
  `memory://episodes/{agentId}` — each scoped to what the caller is authorized
  to see.
- Resource reads SHALL go through the same authz/tenant/audit path as tools.

#### R-MEM-MCP-3 — Thin gateway (no indexing logic)
**User Story:** As an architect, I want the MCP server to be a swappable
interface, not a second copy of the retrieval engine.

**Acceptance Criteria:**
- The MCP server SHALL translate every MCP call into the internal retrieval API
  and SHALL NOT perform embedding, chunking, ranking, or storage itself.
- Replacing the MCP server (or adding a second protocol front-end) SHALL require
  no change to the workers or backends.

### R-MEM-CTRL: Control plane (operator)

#### R-MEM-CTRL-1 — Reconcile to data plane + interface
**Acceptance Criteria:**
- WHEN a `MemoryStore`/`MemoryRetriever` is applied, the operator SHALL reconcile
  the **retrieval-worker** Deployment(s) and the **memory-mcp** Deployment +
  Service that serve it, wiring `ModelProvider` (embeddings) and broker-resolved
  backend credentials.
- The operator SHALL own backend lifecycle config (schema/index bootstrap hooks,
  connection params) and SHALL surface failures on the CRD `status`, never on the
  agent path.
- Deleting a `MemoryRetriever` SHALL tear down only its worker/MCP resources,
  leaving shared `MemoryStore` backends intact unless unreferenced.

### R-MEM-WORK: Data plane (retrieval workers)

#### R-MEM-WORK-1 — Internal retrieval API
**Acceptance Criteria:**
- Workers SHALL implement an internal API (gRPC over mTLS) with operations
  matching the MCP tools: `Retrieve`, `Write`, `Get`, `Delete`, `ListNamespaces`,
  `Summarize`.
- Workers SHALL own embedding (via the bound `ModelProvider`), chunking, indexing,
  ranking, and the backend adapters; backends SHALL sit behind a pluggable
  `Backend` interface (one adapter per `kind`).
- Workers SHALL enforce the namespace + tenant scoping passed by the gateway as a
  defense-in-depth second check (not solely trust the gateway).

#### R-MEM-WORK-2 — Backend adapters
**Acceptance Criteria:**
- The system SHALL ship at least one vector adapter and the `filesystem`/`agentfs`
  adapter (P1) behind the `Backend` interface; graph/KV/eventlog adapters SHALL be
  addable without changing the gateway or the internal API.

### R-MEM-FS: Filesystem memory (Turso AgentFS) + mounting

#### R-MEM-FS-1 — AgentFS-backed filesystem memory
**User Story:** As an agent (esp. a coding agent), I want a persistent,
branchable, versioned **filesystem** as memory — not just vector recall — so I can
keep working files, notes, and artifacts across runs.

**Acceptance Criteria:**
- The system SHALL support a `filesystem` `MemoryStore` with driver `agentfs`
  backed by **Turso AgentFS** (https://github.com/tursodatabase/agentfs;
  SQLite-canonical, branchable, versioned), **reusing the existing**
  `pkg/agentfs` engine and `AgentFSSpec` (PVC `sizeGiB`, `mountPath` default
  `/var/agentfs`, sidecar `image`, `BackupPolicy`/`RestorePolicy`) — not a new
  filesystem.
- A filesystem `MemoryRetriever` SHALL carry the `agentfs` config (size, mount
  path, S3 backup/WAL/retention) and bind it to a tenant/namespace, leveraging
  the existing R-AFS backup/restore/retention guarantees.

#### R-MEM-FS-2 — Mounting into the agent sandbox
**User Story:** As a developer, when my agent uses filesystem memory I want it
**mounted** so the agent does normal file I/O, while tools/IDEs can also reach it
over MCP.

**Acceptance Criteria:**
- WHEN an `Agent`/`AgentRun` references a filesystem `MemoryRetriever` with
  mounting enabled, the operator SHALL attach the AgentFS volume to the agent
  sandbox at the configured `mountPath` (reusing the AgentFS mount mechanism in
  `operator/internal/builders/agentrun.go`), so the agent sees a POSIX filesystem.
- The SAME filesystem SHALL be reachable over MCP (R-MEM-FS-4); the mounted view
  and the MCP view SHALL be the same SQLite-canonical state (no divergent copies).
- Mounting SHALL be optional: a filesystem retriever MAY be MCP-only (no mount)
  for clients that aren't co-located (e.g. an external IDE).

#### R-MEM-FS-3 — Branch + snapshot semantics
**Acceptance Criteria:**
- The system SHALL support **branching** a filesystem memory: forking a branch
  from a base so a run/session works in isolation, then **commit** (publish) or
  **rollback** (discard). Branches SHALL be cheap (AgentFS copy-on-write).
- Point-in-time **snapshots** SHALL map to AgentFS/`pkg/agentfs` S3 versions
  (full snapshot + WAL frames), so a branch can be restored to a prior version.
- A per-`AgentRun` ephemeral branch SHALL be discardable on run completion
  without affecting the base.

#### R-MEM-FS-4 — MCP filesystem surface
**Acceptance Criteria:**
- The MCP server SHALL expose filesystem memory via resources
  `memory://files/{retrieverRef}/{path}` and `memory://branches/{agentId}`, and
  tools `branch_memory_fs(base, name)`, `snapshot_memory_fs(branch)`,
  `list_memory_branches(retrieverRef)` (plus read/write of files through the
  existing `get_memory`/`write_memory` with a filesystem namespace).
- All MCP filesystem reads/writes/branch ops SHALL pass through the same
  identity/tenant/policy/quota/audit path as the other modalities (R-MEM-AUTH,
  R-MEM-QUOTA, R-MEM-AUDIT).

#### R-MEM-FS-5 — Filesystem isolation
**Acceptance Criteria:**
- A mounted or MCP-accessed branch SHALL be confined to the caller's tenant and
  authorized branch; path traversal SHALL NOT escape the namespace/branch root or
  reach another tenant's files.
- AgentFS S3 backup credentials SHALL be broker-resolved and SHALL never appear
  in the agent, the mount, or logs.

### R-MEM-AUTH: Identity, tenancy, policy

#### R-MEM-AUTH-1 — Attested identity → tenant
**User Story:** As a security owner, I require every memory call to be attributed
to a verified workload identity and confined to its tenant.

**Acceptance Criteria:**
- The MCP server SHALL authenticate every call via the caller's **JWT-SVID**
  (validated against SPIRE/the trust domain) — either presented directly or
  injected by the agentnet sidecar — and SHALL reject unauthenticated calls.
- The caller's **tenant** SHALL be derived from its SPIFFE identity (path), and
  the gateway SHALL inject that tenant filter into every internal call; a
  caller-supplied `tenant` that doesn't match SHALL be rejected.
- A caller SHALL never receive results from another tenant's namespaces, even if
  it names them explicitly (cross-tenant isolation).

#### R-MEM-AUTH-2 — Deny-by-default operation policy
**Acceptance Criteria:**
- Each `MemoryRetriever.policy` SHALL be **deny-by-default**: a (SPIFFE identity,
  operation, namespace) is allowed only if explicitly granted.
- Read vs. write vs. delete SHALL be independently grantable.

#### R-MEM-AUTH-3 — Authorized mutations (optional TraT)
**Acceptance Criteria:**
- `write_memory`/`delete_memory` MAY require a **TraT** (transaction token) whose
  scope authorizes the mutation, composing with smol-agents-trat-egress; when a
  retriever marks mutations `trat-required`, the gateway SHALL reject mutations
  lacking a valid, identity-bound TraT.

### R-MEM-QUOTA: Quotas + rate limits

#### R-MEM-QUOTA-1
**Acceptance Criteria:**
- The gateway SHALL enforce per-identity (and per-retriever) limits: `topK`
  ceiling, request rate, and `write_memory` payload size.
- Exceeding a limit SHALL fail the call with a typed quota error and SHALL be
  audited; it SHALL NOT degrade into a silently-truncated or unscoped result.

### R-MEM-AUDIT: Audit logging

#### R-MEM-AUDIT-1
**Acceptance Criteria:**
- The gateway SHALL emit a structured audit record for every call: caller SPIFFE
  id, tenant, retrieverRef, operation, namespace, filter summary, result count,
  decision (allow/deny), and latency.
- Audit records SHALL NOT contain retrieved content, written content, embeddings,
  or backend credentials.

### R-MEM-SEC: Security invariants (the whole point)

#### R-MEM-SEC-1 — Fail-closed + isolation
**Acceptance Criteria:**
- Any authz/tenant/quota/policy failure SHALL fail closed (deny + audit), never
  fall back to an unscoped or anonymous query.
- Backend credentials SHALL be broker-resolved and SHALL never appear in the
  gateway, in agent-visible responses, in env, or in logs.
- The gateway SHALL never return another tenant's document via `get_memory`/
  `memory://documents/{id}` even on a direct id (id ownership is checked).
