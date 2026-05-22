# Technology Stack — smol-agents

## Project Type
Multi-binary Go platform (agent runtime, secret-proxy sidecar, agentctl)
plus eBPF programs in C, deployed on Kubernetes via Knative, Deployment,
and StatefulSet.

## Core Technologies

### Primary Language(s)
- **Go 1.26** — primary application language. CGO enabled only where the
  `cilium/ebpf` loader requires it on certain kernels.
- **C (BPF subset)** — eBPF programs in `bpf/programs/`, compiled with
  clang/llvm to BPF bytecode and embedded via `go:generate`.
- **Quint** — formal modelling of safety/liveness invariants.

### Key Dependencies/Libraries
- **github.com/cilium/ebpf v0.16.0** — BPF loader, map/program API.
- **github.com/spiffe/go-spiffe/v2 v2.5.0** — workload identity, mTLS,
  JWT-SVID.
- **github.com/hashicorp/vault/api v1.15.0** — Vault/OpenBao client used
  by the secret broker backend.
- **google.golang.org/grpc v1.68.0** — transport for in-mesh APIs.
- **go.opentelemetry.io/otel v1.32.0** — tracing + metrics.
- **pgregory.net/rapid v1.1.0** — property-based tests.
- **gopkg.in/yaml.v3** — config.

### Application Architecture
- **Hexagonal**: each `pkg/<concern>` exports an interface and a default
  implementation. The agent main wires them via constructor injection.
- **Two binaries co-located in a Pod**: `agent` and `secret-proxy`,
  communicating over a shared UDS volume. `agentctl` is local CLI.

### Data Storage
- Stateless agents: no on-disk state.
- StatefulSet variant: one PVC per replica for agent-managed state
  (used only when business logic requires it).

### External Integrations
- **SPIRE** (existing in stigen.ai infra) — workload API on a CSI volume.
- **Vault / OpenBao** — secret backend.
- **Knative Serving** — for scale-to-zero deployments.

## Development Environment

### Build & Development Tools
- **Build**: `make` + `go build` + `go generate` (for BPF objects).
- **devenv.sh** declarative dev shell pulling Go, clang/llvm, kubectl,
  helm, kind, kubectl, kustomize, knative-client, quint, etc.

### Code Quality Tools
- **Static analysis**: `go vet`, `golangci-lint` (default + revive,
  errcheck, gosec, gocyclo).
- **Formatting**: `gofumpt`.
- **Testing**: `go test`, `pgregory.net/rapid` for property tests,
  `kind` + `spiretest` containers for integration.

### Version Control
- Git, trunk-based, signed commits.
- PRs require: green `make verify`, at least one reviewer.

## Deployment & Distribution
- **Containers**: distroless base, multi-arch (linux/amd64, linux/arm64).
- **Charts**: Helm in `deploy/helm`, Kustomize overlays in
  `deploy/kustomize`.
- **Knative manifests**: `deploy/knative/`.
- **SPIRE bindings**: `deploy/spire/cluster-spiffe-ids.yaml` rendered
  alongside the chart.

## Technical Requirements & Constraints

### Performance
- Knative cold-start P95 ≤ 1.2 s.
- Secret broker p99 lease ≤ 25 ms @ 100 RPS.
- eBPF event delivery ≤ 5 % CPU @ 10 k events/sec.

### Compatibility
- Linux kernel 5.8+ (CO-RE / BTF).
- Kubernetes 1.28+ (RuntimeClass and Knative dependencies).
- gVisor `release-20240916.0` or newer.

### Security & Compliance
- Trust domain: `stigen.ai` (existing convention).
- All transports require valid SVIDs in `strict` mode.
- Reproducible builds (`-trimpath`, `-ldflags '-s -w -buildid='`).
- Cosign signing in CI.

## Decision Log
1. **cilium/ebpf over libbpf-go** — pure Go, no cgo for the common path,
   strong CO-RE story.
2. **gVisor over Kata** — no nested virtualization; first-class Knative
   compatibility; faster cold start.
3. **Quint over TLA+** — readable syntax, Apalache backend, simulator,
   modern tooling.
4. **rapid over gopter** — better generators, simpler API.
5. **Kloak-style UDS broker over Vault Agent injector** — explicit per-
   call SPIFFE attestation, simpler audit trail.

## Known Limitations
- gVisor cannot run all Linux syscalls; agents needing
  `userfaultfd` or some `io_uring` ops will fail. We document the
  allow-list and ship a CI test that probes for surprises.
- `cilium/ebpf` ring buffer requires kernel ≥ 5.8; older kernels fall
  back to perf events with a documented overhead delta.
