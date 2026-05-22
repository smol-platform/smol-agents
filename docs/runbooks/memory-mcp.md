# Runbook — Memory over MCP (operator + retrieval workers + AgentFS)

How to give agents a **memory layer over MCP**: declare backends and a retriever,
connect an agent, and verify tenant isolation. Architecture (see
[`.spec-workflow/specs/smol-agents-memory`](../../.spec-workflow/specs/smol-agents-memory)):

```
agent / IDE ──MCP──▶ memory-mcp (gateway: authz · tenant · quota · audit)
                        │ internal API (HTTP/JSON over mTLS)
                        ▼
                   memory-worker (embed · chunk · index · rank)
                        ▼
              vector backend  ·  AgentFS filesystem (Turso, branchable + mountable)
```

- **Operator = control plane** — reconciles `MemoryStore` + `MemoryRetriever` into
  worker + gateway Deployments.
- **memory-worker = data plane** — owns embedding/indexing/retrieval + the
  backend adapters.
- **memory-mcp = interface** — a thin gateway; **no index logic**. It
  authenticates the caller's JWT-SVID, derives the tenant, enforces
  deny-by-default policy + quotas, audits, and forwards to the worker.

## 1. Declare a backend (`MemoryStore`)

```yaml
apiVersion: runtime.agents.smol-agents.ai/v1
kind: MemoryStore
metadata: { name: team-vectors, namespace: team-alpha }
spec:
  kind: vector          # vector | graph | kv | eventlog | filesystem
  driver: pgvector      # pgvector | qdrant | neo4j | redis | agentfs
  endpoint: postgres://pgvector.data.svc:5432/mem
  auth: { secretRef: { name: pgvector-dsn } }   # broker-resolved; never a literal
  tenancy: { model: shared, tenantLabelKey: tenant }
```

Filesystem (Turso AgentFS) backend — reuses `AgentFSSpec` (PVC + S3 backup):

```yaml
spec:
  kind: filesystem
  driver: agentfs
  tenancy: { model: dedicated }
  agentfs:
    sizeGiB: 5
    mountPath: /var/memory-agentfs
    backup: { s3: { ... }, schedule: "@hourly" }   # see AgentFS runbook
```

## 2. Declare a retriever (`MemoryRetriever`)

The `metadata` name (namespace-qualified) is the **`retrieverRef`** an MCP client
names (e.g. `team-alpha/prod-knowledge`).

```yaml
apiVersion: runtime.agents.smol-agents.ai/v1
kind: MemoryRetriever
metadata: { name: prod-knowledge, namespace: team-alpha }
spec:
  stores: [team-vectors]
  modelProviderRef: openai-embeddings    # existing ModelProvider CR (broker creds)
  topK: 8
  namespaces: [kb, runbooks]
  tenant: team-alpha
  chunking: { size: 512, overlap: 64, strategy: sentence }
  mount: { enabled: true, mountPath: /var/memory-agentfs }   # filesystem retrievers
  quota: { maxTopK: 50, requestsPerMinute: 120, maxWriteBytes: 1048576 }
  mutationsTraT: false                   # set true to require a TraT for write/delete
  policy:                                # DENY-BY-DEFAULT
    - identity: spiffe://nixfleet.smol-agents.ai/ns/team-alpha/sa/agent
      operations: [read, write]
      namespaces: [kb]
    - identity: spiffe://nixfleet.smol-agents.ai/ns/team-alpha/sa/reviewer
      operations: [read]
      namespaces: ["*"]
```

The operator reconciles this into a `memory-worker` + `memory-mcp` Deployment and
sets `status.phase=Ready`.

## 3. Connect an agent (MCP tool)

Point a `Tool kind=mcp` at the gateway; the agentnet sidecar injects the agent's
JWT-SVID, which the gateway uses to derive the tenant.

```yaml
apiVersion: runtime.agents.smol-agents.ai/v1
kind: Tool
metadata: { name: memory, namespace: team-alpha }
spec:
  kind: mcp
  mcp: { url: http://memory-mcp.team-alpha.svc.cluster.local:8080 }
```

### Tools

| Tool | Purpose |
|---|---|
| `retrieve_memory(query, topK, filters, retrieverRef)` | semantic/keyword recall (`topK` clamped to `quota.maxTopK`) |
| `write_memory(content, namespace, metadata, retrieverRef)` | store a document |
| `get_memory(id)` | fetch one document (tenant-ownership checked) |
| `delete_memory(id)` | remove a document |
| `list_memory_namespaces(retrieverRef)` | namespaces the caller may see |
| `summarize_memory(query, retrieverRef)` | LLM summary over matches (P2) |

### Resources

`memory://namespaces/{ns}`, `memory://retrievers/{ref}`,
`memory://documents/{id}`, `memory://episodes/{agentId}`,
`memory://files/{ref}/{path}`, `memory://branches/{agentId}` (filesystem).

The caller-supplied `filters.tenant` is **ignored/validated** against the
attested SPIFFE tenant — a caller can never read another tenant's memory.

## 4. Filesystem memory + mounting

For a `filesystem` retriever with `mount.enabled`, the operator attaches the
AgentFS volume at `mount.mountPath` into the agent pod (reusing the AgentFS mount
mechanism), so the agent does **normal file I/O** *and* the same SQLite-canonical
state is reachable over MCP (`memory://files/…`). Branch/snapshot:
`branch_memory_fs(base, name)`, `snapshot_memory_fs(branch)`,
`list_memory_branches(ref)` — cheap copy-on-write branches per run/session;
snapshots map to AgentFS S3 versions.

## 5. Verify tenant isolation

```bash
# As team-alpha/agent (granted read+write on ns "kb"):
#   write_memory(content="GPU scheduling notes", namespace="kb")  → ok
#   retrieve_memory(query="GPU scheduling", topK=8)               → returns it
# As a DIFFERENT tenant (team-beta/agent):
#   retrieve_memory(query="GPU scheduling", filters={tenant:"team-alpha"})
#     → returns NOTHING (tenant from SVID, not from filters)
#   get_memory(<the team-alpha doc id>)  → 404/denied (cross-tenant id gate)
```

The e2e probe `spiffe-probe --scenarios=memory` exercises exactly this
(write → retrieve → cross-tenant denial) against real SPIRE.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| MCP call → `unauthenticated` | no/invalid JWT-SVID | ensure the agentnet sidecar injects `Authorization: Bearer <JWT-SVID>`, or the client presents one |
| `permission_denied` | deny-by-default policy or tenant mismatch | add a `MemoryGrant` for the identity/op/namespace |
| `quota_exceeded` | topK/rate/write-size over limit | raise `spec.quota.*` (topK is clamped, not silently truncated) |
| retriever `status.phase=Degraded` | backend creds/endpoint or ModelProvider unresolved | check the broker secret + `MemoryStore.endpoint`; failures stay on `status`, never the agent path |
| agentfs snapshots not durable | no production S3 adapter yet (P1 uses in-memory) | tracked P2; file ops + mount are unaffected |

## Security invariants
- Fail-closed: any authz/tenant/quota/policy failure denies + audits — never an
  unscoped or anonymous query.
- Backend credentials are broker-resolved and never appear in the gateway,
  responses, env, or logs.
- Audit records carry who/what/when/decision/result-count — never content,
  embeddings, or credentials.
- Proven in `spec/quint/memory_access.qnt` (`make verify-formal`) and the gateway
  security tests (`pkg/memory/mcp`).

Related: [`docs/runbooks/secretless-egress.md`](secretless-egress.md) (TraT-gated
mutations), [`docs/runbooks/k0s-local-cluster.md`](k0s-local-cluster.md),
[`docs/INSTALL.md`](../INSTALL.md).
