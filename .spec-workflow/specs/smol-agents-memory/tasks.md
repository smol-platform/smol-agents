# Tasks — smol-agents-memory

Builds on **smol-agents-operator** (reconcile spine), **smol-agents-agent-model**
(`ModelProvider`, `AuthRef`, `AgentFSSpec`/`pkg/agentfs`). Optional compose:
**smol-agents-trat-egress** (mutation authz), **smol-agents-secretless-egress**
(broker-minted backend creds).

- [x] 1. Author spec (requirements/design/tasks)
  - Files: this directory
  - _Requirements: all_

- [x] 2. CRDs: MemoryStore + MemoryRetriever
  - Files: `pkg/agentmodel/v1/memory.go` (pure types incl. `MemoryStoreSpec`,
    `MemoryRetrieverSpec`, `TenancySpec`, `ChunkSpec`, `MountSpec`, `MemoryGrant`,
    `QuotaSpec`, status), validation + tests; `operator/api/agentmodel/v1`
    wrappers (+kubebuilder markers, status subresource, printcolumns); deepcopy;
    regenerate CRD yaml (scope controller-gen to the new package + copy only the
    new files — the committed CRDs aren't fully reproducible from source);
    `operator/config/samples/memory_*.yaml`
  - `filesystem` kind reuses `AgentFSSpec`/`BackupPolicy`/`RestorePolicy` verbatim.
  - _Requirements: R-MEM-API-1, R-MEM-API-2, R-MEM-API-3, R-MEM-FS-1_

- [x] 3. Internal retrieval API + Backend interface
  - Files: `pkg/memory/api/` (gRPC proto + generated: Retrieve/Write/Get/Delete/
    ListNamespaces/Summarize/BranchFS/SnapshotFS/ListBranches; request carries
    tenant+namespace+caller id), `pkg/memory/backend.go` (`Backend` interface),
    `pkg/memory/types.go`
  - _Requirements: R-MEM-WORK-1, R-MEM-MCP-3_

- [x] 4. Vector backend adapter
  - Files: `pkg/memory/backend_vector.go` (+ fake-backed test); pgvector/qdrant
    driver behind `Backend`; creds via broker.
  - _Requirements: R-MEM-WORK-2_

- [x] 5. AgentFS backend adapter + branch/snapshot
  - Files: `pkg/memory/backend_agentfs.go` (reuse `pkg/agentfs.Manager` +
    Storage/S3 drivers; file read/write/list; `Branch` = CoW fork; `Snapshot` =
    full+WAL S3 version; `Restore`), tests with `pkg/agentfs` fakes
  - _Requirements: R-MEM-FS-1, R-MEM-FS-3, R-MEM-WORK-2_

- [x] 6. Retrieval worker (data plane)
  - Files: `cmd/memory-worker/` + `pkg/memory/worker/` (serve internal API;
    embed via `ModelProvider`; chunk/rank; dispatch to `Backend`; re-check
    tenant/namespace), tests
  - _Requirements: R-MEM-WORK-1, R-MEM-AUTH-1 (worker re-check)_

- [x] 7. MCP gateway (agent interface)
  - Files: `cmd/memory-mcp/` + `pkg/memory/mcp/` (streamable-HTTP MCP; tools
    `retrieve_memory`/`write_memory`/`list_memory_namespaces`/`get_memory`/
    `delete_memory`/`summarize_memory`; resources `memory://{namespaces,
    retrievers,documents,episodes}/…`; JWT-SVID auth + tenant derivation
    (`pkg/identity`); deny-by-default policy; quota clamp; **thin** — gRPC to
    worker, no index logic), tests (MCP↔RPC mapping, tenant injection)
  - _Requirements: R-MEM-MCP-1, R-MEM-MCP-2, R-MEM-MCP-3, R-MEM-AUTH-1, R-MEM-AUTH-2, R-MEM-QUOTA-1_

- [x] 8. MCP filesystem surface + mounting
  - Files: `pkg/memory/mcp` (resources `memory://files/{ref}/{path}`,
    `memory://branches/{agentId}`; tools `branch_memory_fs`/`snapshot_memory_fs`/
    `list_memory_branches`); operator mount wiring in
    `operator/internal/builders/` (attach AgentFS volume at `mountPath` to agents
    referencing a mountable filesystem retriever — reuse `agentrun.go`)
  - _Requirements: R-MEM-FS-2, R-MEM-FS-4_

- [x] 9. Operator control plane
  - Files: `operator/internal/controllers/memory/` (reconcile MemoryStore +
    MemoryRetriever → worker Deployment + memory-mcp Deployment/Service + RBAC +
    broker cred resolution + status; per-retriever teardown), envtest
  - _Requirements: R-MEM-CTRL-1, R-MEM-API-3_

- [x] 10. Auth, audit, quota (the whole point)
  - Files: `pkg/memory/policy` (deny-by-default grants, tenant from SPIFFE path),
    `pkg/memory/audit` (structured record, NO content/creds), quota enforcement;
    security tests: cross-tenant denial (incl. direct `get_memory` id),
    deny-by-default, fail-closed, audit-no-leak, fs path-traversal containment
  - _Requirements: R-MEM-AUTH-1, R-MEM-AUTH-2, R-MEM-QUOTA-1, R-MEM-AUDIT-1, R-MEM-SEC-1, R-MEM-FS-5_

- [x] 11. Quint invariant
  - File: `spec/quint/memory_access.qnt` — "a document is returned only to an
    identity granted read on its namespace within its tenant; mutation only with
    granted write (+ TraT when required)"; wire into `make verify-formal`
  - _Requirements: R-MEM-AUTH-1, R-MEM-AUTH-2, R-MEM-SEC-1_

- [x] 12. Multiarch images
  - Files: `deploy/docker/memory-mcp.Dockerfile`, `deploy/docker/memory-worker.Dockerfile`;
    add both to `scripts/aws-l2/build-images.sh` (amd64+arm64; local single-arch
    `--load` builds need explicit `--build-arg TARGETARCH` or they mislabel the arch)
  - _Requirements: all (deploy)_

- [x] 13. e2e: memory + R-E2E-SCN-MEMORY
  - Files: `test/e2e/manifests/` (memory-mcp + worker + vector + agentfs branch),
    `cmd/spiffe-probe` `memory` scenario (attested agent: write→retrieve→**mount
    + read file back**; second tenant denied), `shared/scenarios.go` +
    `coverage.go` + fullstack-e2e requirements entry
  - _Requirements: R-MEM-MCP-1, R-MEM-FS-2, R-MEM-AUTH-1, R-MEM-SEC-1_

- [x] 14. Docs
  - Files: `docs/runbooks/memory-mcp.md` (declare a MemoryStore/MemoryRetriever,
    connect an agent via `Tool kind=mcp`, mount AgentFS, verify tenant isolation),
    INSTALL prereqs (vector backend, embedding ModelProvider, S3 for AgentFS)
  - _Requirements: all_

## P2 (now implemented)
- [x] `summarize_memory` (worker retrieve+LLM via ModelProvider; fake for tests)
- [x] graph / KV / eventlog backend adapters (neo4j/redis/in-mem); `memory://episodes/{agentId}`
- [x] TraT-required mutations (R-MEM-AUTH-3) — gateway verifies a `trat` arg, subject-bound, fail-closed
- [x] AgentFS branch merge (fast-forward publish) + the `Merge` Backend op + `merge_memory_fs` tool
- [x] stdio MCP transport; LRU embedding cache
- [x] gRPC transport (proto + buf + adapter + round-trip tests) — drop-in for the RetrievalService contract

## Remaining polish (tracked)
- [ ] gRPC runtime selector in the cmd binaries (transport is built+tested; binaries default to HTTP/mTLS, the e2e-verified path)
- [ ] 3-way branch merge + conflict policy (only fast-forward today)
- [ ] live-infra integration tests for pgvector/qdrant/neo4j/redis/S3/LLM (adapters unit-tested with fakes; integration tests build-tagged + skipped without endpoints)
