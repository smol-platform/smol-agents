# Product Overview — smol-agents-fullstack-e2e

## Product Purpose

Today the project has three working test layers — unit, envtest
(controller-runtime fake apiserver), and `kind-verify` (real apiserver
+ reconciler status). These exercise control-plane behavior but
**nothing actually runs the data plane**: the agent runtime never
executes a plan-act-observe loop, the eBPF programs never load on a
real kernel, the WireGuard userspace adapter never starts, the
identity proxy sidecars never serve traffic, and SPIRE never issues
an SVID. The kind run pod stays in `Pending` because the placeholder
image isn't loaded.

This spec proposes a **fullstack-e2e** layer that spins up the actual
data-plane components in containers / lightweight VMs and exercises
them end-to-end, on a developer's macOS laptop and in CI, without
requiring a managed cloud cluster.

## Why now

1. The agent runtime, identity proxy, eBPF programs, WireGuard
   adapter, and SPIRE-binding paths are all individually tested — but
   **no test crosses the boundaries between them**. A regression in
   how the agent talks to the proxy, or how the proxy authenticates
   to the gateway, would not be caught by anything we run today.
2. CI today can verify "the operator wrote the right Pod spec." It
   cannot verify "the Pod actually executes a Run, the sidecar
   successfully proxies it through SPIFFE mTLS, the eBPF allow-list
   blocks unrelated egress, and the audit ringbuf records the call."
3. Kata-FC microVM sandboxing — the production sandbox — is unverified
   anywhere in the test suite.

## Scope

### In scope (what fullstack-e2e WILL prove)

1. **Identity-proxy data plane**: a real agent dials `127.0.0.1:5432`,
   the TCP proxy upgrades to SPIFFE mTLS using a SPIRE-issued SVID,
   reaches a fake gateway authenticated by its own SVID, payload
   round-trips. HTTP variant: a JWT-SVID with the right audience is
   minted and attached as a Bearer token.
2. **eBPF egress on a real Linux kernel**: an agent process runs in
   a cgroup whose `connect4` map redirects `10.42.0.0/16` to the
   sidecar; a connect to `1.1.1.1:443` is dropped by the
   `cgroup_skb/egress` allow-list and emits a ringbuf audit event.
3. **WireGuard userspace**: the AgentNetwork wireguardMesh adapter
   starts a real netstack-backed device, completes a handshake with a
   peer, and forwards an end-to-end IP packet.
4. **SPIRE binding**: a real SPIRE server issues X509-SVID and
   JWT-SVID to the agent Pod; rotation works.
5. **Plan-act-observe loop**: the agent runtime executes a Run
   against a deterministic fake LLM, calls a fake tool through the
   identity proxy, records steps, and produces a Result whose
   `Output` matches expectations.
6. **AgentRun lifecycle through Pod**: AgentRun CR → reconciler →
   Pod creates → real agent runs → Pod terminates → reconciler stamps
   `Completed` (or `Failed`/`Cancelled`).
7. **Webhook admission**: the operator's validating webhook actually
   admits good specs and rejects bad ones (today we run with webhooks
   disabled in the kind overlay).
8. **Per-feature SmolAgent reconciliation**: the
   identity/sandbox/transport/secrets/ebpf feature pipeline reaches
   `Ready` end-to-end against a live SPIRE.

### Out of scope (deferred)

- **Production-grade Kata-FC microVM sandbox** when the spec verdict
  on Apple Silicon nested-virt is "no go" — covered by a separate
  Linux-CI-only target if needed.
- **Knative Serving feature** (auto-scaling agent pods) — Knative is
  heavy to install and the `deploymentKind=knative` path is rarely
  the critical regression target.
- **Performance / load benchmarks** — covered by a separate perf
  spec, not by fullstack-e2e.
- **Multi-tenant chaos / failure injection** — separate chaos spec.

## Success Criteria

A `make e2e-fullstack` target that, on a developer's Apple Silicon
Mac with OrbStack installed, will:

1. Provision (or reuse) a Linux VM with a current kernel + cgroup v2
   + bpf().
2. Build all Linux artifacts (operator, agent, ebpf-loader,
   secret-proxy, agentctl, BPF objects) for the VM's architecture.
3. Bring up a kind cluster *inside* the VM with privileged DaemonSet
   admission so the eBPF loader can pin maps.
4. Deploy SPIRE (server + agent), the operator, and a fake gateway
   (echo TCP + echo HTTP).
5. Apply a sample CR chain (Platform + SmolAgent + ModelProvider +
   Tool + Agent + AgentNetwork + AgentRun) where the agent uses a
   deterministic fake-LLM and the gateway is the fake echo.
6. Assert ≥ ten end-to-end invariants (status fields, traffic
   actually flows, SVIDs were issued, eBPF maps populated, audit
   events emitted).
7. Tear down cleanly, returning a single PASS / FAIL with structured
   per-step output.

Targets:
- **Cold run** (fresh VM, full image build): ≤ 8 minutes.
- **Warm run** (reuse VM + image cache): ≤ 90 seconds.
- **CI run** (Linux runner, no VM needed): ≤ 5 minutes.
- **Flake rate**: < 1% over 100 runs (timeouts generously sized).

## Layered approach

Three concentric rings, each runnable independently:

| Ring | Hosts | Coverage | Inner-loop time | Cost |
|---|---|---|---|---|
| **L0 docker-compose** | macOS Docker | userspace only: SPIRE + agent runtime + identity proxy + fake gateway. WireGuard userspace works here too. | ~30s | $0 |
| **L1 kind-on-VM** | OrbStack Linux VM | adds eBPF programs + AgentRun Pod sandbox + cgroup-driven networking | ~90s | $0 |
| **L2 single-EC2 + k0s** | AWS Spot bare-metal (`c6gd.metal`) | adds Kata-FC microVM sandbox + production-shape kernel + cleanup-safe lifecycle | ~12 min cold, ~5 min warm | ~$0.20-0.30 / run |

Tests live in `test/e2e/fullstack/` with build tags
`e2e_l0`, `e2e_l1`, `e2e_l2` so each ring can be selected
independently. L0 and L1 run on every PR; L2 runs on `/test-l2` PR
comment, on `main`, and nightly.

### L2 detail — single-EC2 + k0s

A bare-metal EC2 Spot instance bootstrapped via cloud-init into a
single-node k0s cluster with containerd + Kata + Firecracker
registered. **Not EKS** — EKS adds 15+ min of cluster lifecycle and
$0.10/hr we don't need. **Not multi-node** — every test we care about
is kernel-level, one node is sufficient. **Bare-metal mandatory** —
`/dev/kvm` is exposed only on `*.metal` instance types; without it
Firecracker silently falls back to runc and Kata isn't actually
running.

Per-run shape:
- Provision (`run-instances` + Spot): ~30 s API + ~3 min boot
- cloud-init installs: containerd, Kata, k0s, helm, our images: ~3 min
- k0s + manifests apply: ~1 min
- Test scenarios: ~3-5 min
- `terminate-instances`: ~30 s
- **~12 min cold, ~$0.22 at $1.10/hr Spot**

Cleanup belt-and-suspenders (per the e2e_architecture memory):
1. `EXIT` trap calls `terminate-instances`.
2. Spot interruption auto-terminates.
3. CloudWatch Events rule on instances tagged
   `smol-agents-e2e=L2` older than 1 hour → Lambda → terminate.
4. AWS Budget alarm at $X/month → Lambda → nuke all `*-e2e=*` tagged.

## Product Principles

- **Real binaries, fake services**. Every binary the operator ships
  runs as itself; the things we don't own (LLM, real Postgres, real
  WireGuard hub) are replaced with deterministic local fakes.
- **L0 + L1 work offline.** A developer at a coffee shop must be able
  to validate every code path that doesn't require Kata-FC. AWS is
  reachable only via L2.
- **Bisectable failures**. L0 failure → userland code bug. L1 failure
  → kernel/cgroup/eBPF. L2 failure → sandbox/AWS-shape integration.
  Higher rings only run after lower rings are green.
- **Idempotent**. Re-running picks up cached VM, cached image layers,
  cached BPF objects. L2's Spot instance is ephemeral by design — no
  state survives between runs, which is the safety property we want.
- **Cleanup is non-negotiable**. L2 must never leave a stranded EC2
  instance. Four-layer defense (EXIT trap, Spot interruption, time-
  based sweeper, budget alarm) is the minimum bar.
- **Test-as-doc**. Each scenario file is also the executable
  reference for "how does X actually work end-to-end?"

## Decided (since the AWS conversation)

- L2 = single-EC2 + k0s + Spot bare-metal, not EKS, not multi-node.
- Cleanup model: 4-layer (EXIT trap + Spot self-terminate + time
  sweeper + budget alarm).
- LLM: deterministic fake at every ring. No real-LLM tier in this spec.
- SPIRE: real SPIRE server + agent at every ring. Mocking the
  workload-API socket loses too much fidelity.
- Test driver: Go tests using testcontainers-go for L0; Go tests +
  k0s/kubectl for L1+L2. Single typed driver.
- L2 trigger: `/test-l2` PR comment, `main`, nightly. Not on every PR.
- Build target: `linux/arm64` everywhere (OrbStack is arm64; bare-metal
  Graviton is arm64; no cross-build needed).
- **AWS account**: `stigen` sandbox profile.
- **Region**: `us-east-2` (Ohio).
- **Monthly cap**: $50/month enforced by AWS Budget alarm + nuke Lambda
  at 100% threshold.
- **Lifecycle**: per-run terminate. No warm pool. Cleaner state, ~3 min
  cost per run accepted.

## Open questions for the user

Remaining decisions for `requirements.md` (smaller scope now):

1. **L1 also support Linux native** (i.e. dev box without OrbStack)?
   Default: yes — make kind-cluster setup detect-and-reuse OrbStack vs
   native docker. Low cost.
2. **Existing test/integration vs fullstack-e2e**: keep separate tiers
   (`test/integration/` for subsystem, fullstack-e2e for cross-cutting)
   or fold in? Default: keep separate.
3. **Real network egress in L2**: dial `1.1.1.1:443` to assert eBPF
   actually drops it (real kernel path), or only assert BPF map
   contents? Default: dial-and-drop — negligible cost, more authentic.
4. **Knative Serving in L2**: install + test the `deploymentKind=knative`
   path, or punt? Default: punt — separate spec.
5. **Image distribution to L2**: build locally + push to ECR (faster,
   ~$0.10/GB-month) vs build on the EC2 instance (slow but no
   registry). Default: ECR — cleaner CI artifact + faster cold runs.
   _(closed; decided as Go)_
