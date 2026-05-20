# Product Overview — smol-agents

## Product Purpose

smol-agents is an open Go-based eBPF agent platform that lets independent
agent workloads (one binary, one identity, one job) run safely on Kubernetes
using Knative Serving, Deployments, and StatefulSets. The platform fuses four
otherwise-disjoint concerns into a single coherent runtime:

1. **eBPF observability and policy** — agents may attach syscall, network,
   and security probes to the host kernel without leaving the safe path
   (cilium/ebpf, CO-RE, BTF).
2. **Hardware-virtualized sandboxing** — every agent process executes
   inside a Kata Containers + Firecracker microVM (`runtimeClassName:
   kata-fc`) so a compromised agent is bounded by the KVM hypervisor,
   not just process namespaces. gVisor (`runsc`) remains a supported
   fallback for managed K8s environments without `/dev/kvm` (notably
   GKE Sandbox).
3. **Workload identity (SPIFFE)** — every agent has a verifiable SPIFFE ID
   issued by SPIRE, with auto-rotated X.509-SVIDs and JWT-SVIDs.
4. **Two-rail mTLS** — public (gateway-fronted) mTLS for ingress and private
   SPIFFE mTLS for in-mesh. A secret-proxy sidecar (kloak-style) brokers
   credentials over a Unix domain socket so raw key material never enters
   agent memory in the steady state.

The platform is *verifiable*: requirements are written in EARS form, the
critical state machines (SVID rotation, secret broker handshake, agent
lifecycle) are modelled in Quint with safety/liveness invariants, and runtime
properties are exercised by `pgregory.net/rapid` property tests. Every step
of the build pipeline emits a check that the user can re-run locally.

## Target Users

- **Platform engineers** running a Knative tenant who want a hardened,
  drop-in agent runtime instead of writing their own.
- **Security engineers** who need verifiable workload identity, sandbox
  containment, and on-host eBPF visibility.
- **Application teams** writing event-driven agents (LLM tools, scrapers,
  background workers) that need scale-to-zero, mTLS, and short-lived secrets
  without rolling their own plumbing.

## Key Features

1. **eBPF Runtime (`pkg/ebpf`)** — Loads CO-RE BPF programs, manages maps and
   ring buffers, exposes events to Go via a typed bus. Ships with sample
   syscall and network probes.
2. **Sandbox Abstraction (`pkg/sandbox`)** — Defaults to Kata + Firecracker
   (`kata-fc`) with a typed `Kind` enum that classifies runtimes as
   microVM-backed (kata-fc / kata-qemu / kata-clh), userspace (gvisor),
   or unsandboxed (runc — guarded by R-SBX-1). The Helm chart ships
   nine distro presets so the right RuntimeClass lands per environment.
3. **Identity (`pkg/identity`)** — `go-spiffe/v2` X.509Source and JWTSource
   with auto-rotation, SPIFFE-aware authorizers, and three modes
   (insecure / permissive / strict) matching the existing infra-blocks
   convention.
4. **Transport (`pkg/transport`)** — Two listener types out of the box:
   `PrivateMTLS` (SPIFFE peers) and `PublicMTLS` (X.509 from a public CA,
   pinned to a SPIFFE-bound server identity). Both speak gRPC and HTTP/2.
5. **Secret Proxy (`cmd/secret-proxy`, `pkg/secrets`)** — A sidecar listening
   on a UDS, authenticating callers via `SO_PEERCRED` plus the SPIRE
   workload API, and brokering Vault/OpenBao-issued ephemeral secrets.
6. **Knative + K8s Deployment (`deploy/`)** — Helm chart, Kustomize overlays,
   Knative `Service`, `Deployment`, `StatefulSet`, Kata/gVisor
   `RuntimeClass` (preset-driven), `ClusterSPIFFEID` CRD bindings.
7. **Verification Harness** — Quint specs, rapid property tests, kind-based
   integration tests, e2e runner against a real Knative install.

## Business Objectives

- Provide a hardened agent runtime that is correct *by construction* —
  every safety property checkable in CI.
- Cut the time-to-first-agent for a new tenant from days (rolling SPIRE,
  sandbox, mTLS, secret integration by hand) to under an hour.
- Reduce the blast radius of a compromised agent to "zero host kernel
  access, zero plaintext secrets, zero credential reuse outside lease TTL."

## Success Metrics

- **First-agent-up time**: ≤ 15 minutes from `kubectl apply -f deploy/`
  on a clean kind cluster.
- **Cold-start latency** (Knative scale-to-zero → first request):
  - ≤ 2.0 s P95 with `kata-fc` (microVM boot dominates).
  - ≤ 1.2 s P95 with `gvisor` fallback.
- **SVID rotation correctness**: 100 % of certificates rotate before
  expiry under chaos tests; no observable handshake failure.
- **Sandbox containment**: zero successful syscalls bypassing the
  configured sandbox boundary (Firecracker KVM exit gates for kata-fc;
  gVisor allow-list for gvisor) across the e2e suite.
- **Formal coverage**: every requirement tagged in `requirements.md` is
  cited by at least one Quint invariant or rapid property.

## Product Principles

1. **Verifiable by default** — If a property is critical, it is also
   model-checked or property-tested. Comments are not specifications;
   Quint specs are.
2. **Two layers of containment** — eBPF visibility on the host *and*
   Kata + Firecracker microVM isolation of the agent (gVisor where
   KVM is unavailable). Defense in depth.
3. **Zero plaintext credentials in agent memory** — Secrets enter only via
   the broker and only as short-lived leases.
4. **Boring interfaces, sharp edges hidden** — Agents see plain `net.Conn`,
   `grpc.ClientConn`, and `secrets.Lease`. SPIFFE, mTLS, and Vault are
   behind those.
5. **One binary per concern** — Agent, secret-proxy, and agentctl are
   separate binaries; tests can exercise them independently.

## Monitoring & Visibility

- **Dashboard**: Web-based via OpenTelemetry → Prometheus + Grafana.
  Default dashboard ships with the Helm chart.
- **Real-time**: OTLP gRPC export; ring-buffer events fan out to a local
  metrics collector and to traces.
- **Key Metrics**: SVID expiry headroom, secret-broker p99 latency,
  eBPF map saturation, sandbox cold-starts, mTLS handshake errors.

## Future Vision

### Potential Enhancements

- **wasm sandbox**: Add a WASI runtime as a second sandbox option for
  cold-start-sensitive agents.
- **Federated SPIFFE**: Cross-trust-domain federation for multi-cluster
  agents.
- **Policy-as-code agents**: Cedar / OPA agents that consume eBPF events.
- **Verifiable SBOM + SLSA L3**: signed builds, in-toto attestations.
