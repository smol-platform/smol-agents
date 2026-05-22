# Project Structure — smol-agents

## Directory Organization

```
smol-agents/
├── cmd/                          # Binaries
│   ├── agent/                    # Main agent runtime
│   ├── secret-proxy/             # Kloak-style sidecar
│   └── agentctl/                 # Local CLI
├── pkg/                          # Public Go packages
│   ├── identity/                 # SPIFFE workload identity
│   ├── transport/                # Public + private mTLS transports
│   ├── secrets/                  # Secret broker client + backends
│   ├── sandbox/                  # Sandbox abstraction (gVisor default)
│   ├── ebpf/                     # eBPF runtime + EventBus
│   ├── runtime/                  # Lifecycle, ready/health, drain
│   ├── config/                   # Typed config loader
│   ├── observability/            # OTel wiring
│   └── health/                   # Probes
├── internal/                     # Private helpers
│   └── version/
├── bpf/                          # eBPF C source
│   └── programs/
│       ├── syscalls.bpf.c
│       └── network.bpf.c
├── deploy/                       # Deployment artifacts
│   ├── helm/                     # Helm chart
│   ├── kustomize/                # Kustomize overlays
│   ├── knative/                  # Knative Service manifests
│   ├── spire/                    # ClusterSPIFFEID, SPIRE bindings
│   └── docker/                   # Dockerfiles
├── spec/                         # Formal specs
│   ├── quint/                    # Quint models
│   └── proofs/                   # Optional Lean/Coq proofs
├── test/                         # Integration + e2e tests
│   ├── integration/
│   └── e2e/
├── docs/                         # Operator + dev docs
└── .spec-workflow/               # spec-workflow MCP artifacts
    ├── specs/
    │   └── smol-agents-platform/
    │       ├── product.md
    │       ├── requirements.md
    │       ├── design.md
    │       └── tasks.md
    └── steering/
        ├── tech.md
        └── structure.md
```

## Naming Conventions

### Files
- Go files: `snake_case.go` is **not** Go convention; use `lower.go`
  (no underscores unless suffix `_test.go`, `_linux.go`).
- BPF C: `<feature>.bpf.c`.
- YAML: `kebab-case.yaml`.

### Code
- Types: `PascalCase` (e.g. `IdentitySource`).
- Functions/methods: `PascalCase` exported, `camelCase` unexported.
- Constants: `PascalCase`; environment-style constants use
  `UPPER_SNAKE_CASE` only inside string values.

## Import Patterns

### Order
1. Standard library
2. External (`github.com/...`, `google.golang.org/...`)
3. Module-internal (`github.com/smol-platform/smol-agents/...`)

`goimports` enforces with sections.

### Module Organization
- Module path: `github.com/smol-platform/smol-agents`.
- No package may import `cmd/`.
- `internal/` packages are private to this module.

## Code Structure Patterns

### Module/Package Organization
1. License + package doc comment
2. Imports (grouped)
3. Constants
4. Types (interfaces first, then structs)
5. Constructors
6. Methods (receivers grouped)
7. Helpers (unexported)

### Function Organization
- Validate inputs early, return before doing work.
- Pure helpers live below the function that calls them.
- Side effects pushed to package edges; `pkg/...` cores stay testable.

## Code Organization Principles
1. **Single Responsibility** per file (`identity_source.go`,
   `mtls_listener.go`).
2. **Small interfaces** at package boundaries — `Backend`, `Source`,
   `Listener`.
3. **Pure cores, side-effecting edges** — eBPF, file I/O, syscalls live
   in clearly-marked files.
4. **Cite the requirement** — when a file enforces an EARS rule, its
   doc comment cites the requirement ID (e.g. `// R-IDN-1`).

## Module Boundaries
- `pkg/identity` ← `pkg/transport`, `pkg/secrets`, `cmd/agent`
- `pkg/transport` ← `cmd/agent`
- `pkg/secrets` ← `cmd/agent`, `cmd/secret-proxy`
- `pkg/sandbox` ← `cmd/agent` (and `cmd/secret-proxy` indirectly)
- `pkg/ebpf` ← `cmd/agent`
- `pkg/runtime` ← `cmd/agent`, `cmd/secret-proxy`
- `pkg/config`, `pkg/observability`, `pkg/health` ← any other package

No package below `pkg/identity` may import any package above it; this
keeps the dependency DAG acyclic.

## Code Size Guidelines
- File ≤ 400 lines (split otherwise).
- Function ≤ 60 lines.
- Cyclomatic complexity ≤ 10.
- Max nesting depth 4.

## Documentation Standards
- Every exported symbol has a doc comment, with a leading symbol name.
- A file enforcing a requirement starts with `// Implements R-XXX-N: ...`.
- Public packages have a `doc.go` summarising the package's role.
