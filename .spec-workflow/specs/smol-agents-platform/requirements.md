# Requirements Document — smol-agents

## Introduction

smol-agents delivers a Go-based eBPF agent runtime hardened with gVisor
sandboxing, SPIFFE workload identity, dual-rail (public + private) mTLS,
and a kloak-style secret-broker sidecar — deployable on Kubernetes via
Knative Serving, Deployments, and StatefulSets. Every requirement below
carries a stable identifier (e.g. `R-IDN-1`) so the design, code, formal
model, and tests can cite it.

## Alignment with Product Vision

These requirements implement the principles in `product.md`: verifiable by
default, two layers of containment, zero plaintext credentials, boring
interfaces, and one binary per concern.

## Requirements

### R-IDN: Workload Identity (SPIFFE)

#### R-IDN-1 — X.509-SVID source with auto-rotation
**User Story:** As a platform engineer, I want every agent to obtain an
auto-rotating X.509-SVID via the SPIRE workload API, so that no static
private key ever lives on disk or in env vars.

**Acceptance Criteria:**
1. WHEN the agent process starts THEN the identity package SHALL block
   until a valid X.509-SVID has been received from the workload API or a
   bounded timeout elapses.
2. WHEN an SVID is within 50 % of its remaining lifetime THEN the source
   SHALL begin rotation and SHALL emit the new bundle to all subscribers
   before the old one expires.
3. IF the workload API is unreachable for longer than the configured grace
   period THEN the agent SHALL transition to `Unhealthy` and refuse new
   transport connections.

#### R-IDN-2 — JWT-SVID issuance and validation
**User Story:** As a service author, I want short-lived JWT-SVIDs with a
specific audience, so that I can authenticate to non-mTLS services without
leaking credentials.

**Acceptance Criteria:**
1. WHEN code requests a JWT-SVID for audience `aud` THEN the identity
   package SHALL return a token whose `aud` claim equals `aud` and whose
   `exp` is no further than `MaxJWTLifetime` in the future.
2. WHEN code validates a JWT-SVID THEN it SHALL reject tokens whose
   issuer is outside the configured trust domain set.

#### R-IDN-3 — Three operating modes
**User Story:** As an operator, I want `insecure / permissive / strict`
modes consistent with the existing infra-blocks platform.

**Acceptance Criteria:**
1. WHEN mode is `strict` THEN all transports SHALL require a valid
   peer SPIFFE ID matching the configured authorizer.
2. WHEN mode is `permissive` THEN transports MAY accept connections
   from peers without an SVID but SHALL log them as `legacy_peer`.
3. WHEN mode is `insecure` THEN transports MAY accept plaintext but
   SHALL refuse to start unless an explicit env var
   `SMOL_AGENTS_ALLOW_INSECURE=1` is set.

### R-MTL: mTLS Transport

#### R-MTL-1 — Private SPIFFE mTLS (in-mesh)
**User Story:** As a service author, I want a `PrivateMTLS` listener that
authenticates peers by SPIFFE ID, so that intra-mesh calls require no
shared secrets.

**Acceptance Criteria:**
1. WHEN a peer presents an SVID outside the authorizer set THEN the
   handshake SHALL fail with `tls: bad certificate`.
2. WHEN a peer presents a valid SVID THEN the connection SHALL expose
   the peer SPIFFE ID via `transport.PeerID(ctx)`.
3. WHEN the SVID rotates underneath an open connection THEN existing
   connections SHALL remain valid; new connections SHALL use the new
   bundle.

#### R-MTL-2 — Public mTLS (gateway-fronted)
**User Story:** As a tenant, I want a `PublicMTLS` listener using a public
CA-issued cert chain, so that external clients can reach the agent through
a Knative gateway.

**Acceptance Criteria:**
1. WHEN the public listener starts THEN it SHALL load a cert chain from
   a configured path or a `Secret` mount and refuse to start on
   missing/invalid material.
2. WHEN both client cert and SPIFFE attestation are configured THEN the
   listener SHALL require both — public chain validation AND SPIFFE-
   bound server identity — and SHALL emit the bound SPIFFE ID in
   request logs.

### R-SBX: Sandbox

#### R-SBX-1 — Hardened RuntimeClass enforcement
**User Story:** As a security engineer, I want every agent Pod to run
under a hardened RuntimeClass — Kata + Firecracker by default, gVisor
where KVM is unavailable — so that no agent has direct host kernel
access.

**Acceptance Criteria:**
1. WHEN the Helm chart is installed with defaults THEN every agent
   `Deployment` / `StatefulSet` / Knative `Service` SHALL set
   `runtimeClassName: kata-fc`.
2. WHEN `sandbox.preset` is one of {`generic`, `bare-metal`,
   `eks-bottlerocket`, `aks`, `openshift-sandboxed`, `k3s`, `talos`,
   `gke`, `generic-gvisor`} THE chart SHALL render the corresponding
   `runtimeClassName` from a fixed mapping (kata-fc / kata-cc-isolation
   / kata / gvisor) and SHALL refuse unknown presets at template time.
3. THE set of "hardened" RuntimeClass names accepted without an
   `allowHostRuntime=true` override is exactly:
   `{kata-fc, kata-qemu, kata-clh, kata-cc-isolation, kata, gvisor}`.
   Any other value SHALL be rejected by the chart's
   `validateSandbox` helper.
4. IF `sandbox.runtimeClass=runc` (or any other non-hardened class)
   AND `sandbox.allowHostRuntime` is false THEN the chart SHALL fail
   `helm template` with a clear error citing R-SBX-1.

#### R-SBX-2 — eBPF host visibility into sandbox
**User Story:** As an SRE, I want host-side eBPF probes to still observe
sandboxed agent syscalls (whether the sandbox is a Firecracker microVM
via Kata's vsock + virtiofs path, or gVisor's Sentry process), so that
we keep audit visibility.

**Acceptance Criteria:**
1. WHEN an agent makes a syscall inside the sandbox THEN the host eBPF
   syscall tracer SHALL emit at least one event tagged with the
   sandbox/Pod ID. For kata-fc the host visibility surface is the
   `kata-runtime` containerd shim and the firecracker process; for
   gvisor it is the `runsc` Sentry.

#### R-SBX-3 — KVM prerequisite for microVM kinds
**User Story:** As an operator, I want to know up front whether my
nodes can run Kata.

**Acceptance Criteria:**
1. THE `pkg/sandbox.Kind.IsMicroVM()` SHALL return true exactly for
   `kata-fc`, `kata-qemu`, and `kata-clh`.
2. THE `INSTALL.md` documentation SHALL list `/dev/kvm` availability
   (bare metal, nested virt, or KVM-capable cloud VM family) as a
   prerequisite for any microVM Kind, and SHALL point `gke` users at
   the gVisor preset which has no KVM dependency.

### R-EBP: eBPF Runtime

#### R-EBP-1 — CO-RE program loading
**User Story:** As a developer, I want to ship CO-RE BPF programs loaded
via cilium/ebpf with minimal kernel-version branching.

**Acceptance Criteria:**
1. WHEN the agent starts THEN `pkg/ebpf` SHALL load all configured BPF
   programs and SHALL refuse to start if any required program fails
   verification.
2. WHEN BTF info is available on the host THEN programs SHALL be
   relocated automatically; otherwise the loader SHALL log a warning
   and either fall back to a vmlinux.h-bundled BTF or fail per config.

#### R-EBP-2 — Ring-buffer event delivery
**User Story:** As a consumer, I want a typed Go channel of events from
each ring buffer.

**Acceptance Criteria:**
1. WHEN events arrive on a ring buffer THEN the corresponding `EventBus`
   SHALL deliver them to all subscribers; a slow subscriber SHALL NOT
   block other subscribers (drop-or-block policy is configurable).
2. WHEN the agent shuts down THEN all ring buffer readers SHALL be
   closed within `ShutdownTimeout`.

### R-SEC: Secret Proxy (kloak-style)

#### R-SEC-1 — UDS handshake with SPIFFE peer authn
**User Story:** As a security engineer, I want the broker to authenticate
the *calling process* by SPIFFE ID, so that one compromised agent cannot
read another agent's secrets.

**Acceptance Criteria:**
1. WHEN a client connects to the broker UDS THEN the broker SHALL read
   `SO_PEERCRED`, resolve the PID's SPIFFE ID via the SPIRE workload API
   and a process-tree walk, and SHALL reject the connection if no SVID
   matches.
2. WHEN the resolved SPIFFE ID is not in the policy for the requested
   secret THEN the broker SHALL deny with a typed error.

#### R-SEC-2 — Ephemeral leases
**User Story:** As an operator, I want every secret to leave the broker
as a short-lived lease.

**Acceptance Criteria:**
1. WHEN the broker issues a lease THEN the lease's TTL SHALL be ≤
   `MaxLeaseTTL` (default 15 min).
2. WHEN a lease expires THEN any cached value held by clients SHALL no
   longer be considered valid; the broker SHALL refuse to refresh
   without a fresh authentication.

#### R-SEC-3 — Backend-pluggable
**User Story:** As an operator, I want to point the broker at Vault,
OpenBao, or a static map for testing.

**Acceptance Criteria:**
1. WHEN configuring the broker THEN it SHALL accept any backend
   implementing the `secrets.Backend` interface.

### R-RUN: Agent Runtime

#### R-RUN-1 — Health and readiness
**User Story:** As a Knative operator, I want `/healthz` and `/readyz`
endpoints that match Knative semantics.

**Acceptance Criteria:**
1. WHEN the agent is starting up THEN `/readyz` SHALL return 503 until
   identity, transport, and eBPF subsystems all report ready.
2. WHEN a subsystem fails THEN `/healthz` SHALL return 503 within one
   probe interval.

#### R-RUN-2 — Graceful shutdown
**User Story:** As an operator, I want SIGTERM to drain in flight
connections and detach BPF programs cleanly.

**Acceptance Criteria:**
1. WHEN SIGTERM is received THEN open requests SHALL be allowed to
   complete up to `DrainTimeout`; all listeners SHALL stop accepting.
2. WHEN draining is complete THEN BPF programs SHALL be detached and
   maps closed before exit.

### R-DEP: Deployment

#### R-DEP-1 — Knative Serving
**User Story:** As a tenant, I want to run an agent as a Knative `Service`
with scale-to-zero.

**Acceptance Criteria:**
1. WHEN deployed via the Helm chart with `mode: knative` THEN a Knative
   `Service` SHALL be created with `runtimeClassName: gvisor`,
   `serviceAccountName` bound to a `ClusterSPIFFEID`, and the secret
   broker as a sidecar.

#### R-DEP-2 — Deployment and StatefulSet
**User Story:** As a tenant, I want long-running agents on Deployment or
StatefulSet (e.g. for stable identity and storage).

**Acceptance Criteria:**
1. WHEN `mode: deployment` or `mode: statefulset` THEN the chart SHALL
   render the equivalent K8s resources with the same identity and
   sandbox guarantees.

### R-VRF: Verification

#### R-VRF-1 — Formal model
**User Story:** As a reviewer, I want a Quint model that proves SVID
rotation never produces a window without a valid cert.

**Acceptance Criteria:**
1. WHEN `make verify-formal` runs THEN every `.qnt` file in
   `spec/quint/` SHALL type-check and all named invariants SHALL hold
   under `quint test`.

#### R-VRF-2 — Property tests
**User Story:** As a developer, I want runtime properties of the secret
broker (no secret material returned to a non-authorized SPIFFE ID, lease
TTL ≤ max) checked with rapid.

**Acceptance Criteria:**
1. WHEN `go test ./...` runs THEN rapid-driven properties for the
   broker SHALL pass with at least 1000 generated cases.

## Non-Functional Requirements

### Code Architecture and Modularity
- Single Responsibility per package; `pkg/` contains library code,
  `cmd/` contains binaries, `internal/` contains private helpers.
- Public packages export small interfaces; private types are unexported.
- No package may import from `cmd/`.

### Performance
- Cold start (Knative scale-to-zero → first response): P95 ≤ 1200 ms.
- Secret broker p99 lease latency ≤ 25 ms with 100 RPS.
- eBPF event delivery overhead ≤ 5 % CPU at 10 k events/sec.

### Security
- Zero static secrets in agent memory beyond a current lease.
- All transports require valid X.509-SVIDs in `strict` mode.
- gVisor RuntimeClass mandatory unless explicitly overridden.
- All binaries built reproducibly, with `-trimpath`, no cgo where
  avoidable, signed via cosign in CI.

### Reliability
- SVID rotation under chaos: no test run produces a TLS handshake
  failure caused by expired SVIDs.
- Graceful shutdown completes in ≤ 30 s.

### Usability
- `agentctl status` returns a readable table of subsystem health.
- One-line install on kind: `make kind-up && helm install agents
  deploy/helm`.
