# Dependency reduction — lighter self-hosted / smaller-surface alternatives

> Source: an 8-analyst agent-team analysis (one per heavyweight dependency, grounded in
> the code) + a ranked synthesis, 2026-06-08. Tracked in beads epic **`knative-agents-7fr`**.
> Goal: a self-host operator shouldn't have to run a fleet of heavy control planes —
> without trading away the platform's safety properties.

## The hard floor (the disqualifying test applied to every alternative)
Never trade away **per-Pod / per-tenant cryptographic isolation** for simplicity. Any
"lighter" option must preserve: per-Pod SVID, two-stage tenant checks, fail-closed-on-missing-credential
(D3), and JWT/crypto-enforced (not code-enforced) ACLs + TraT sender-constraint (D1).
Three options were **rejected** on this basis: drop-SPIFFE→UID identity, cert-manager
per-SA certs, and an etcd-backed memory store (shared-namespace identity breaks TraT/D1).

## Cross-cutting themes
1. **Lighter path = DEFAULT, heavy path = OPT-IN.** Kata, MinIO, memory, Knative, Karpenter
   are all justified at the high end but overkill for most self-hosts. Flip the default.
2. **Embed-in-operator / single-binary self-host profile.** SPIRE issuer, NATS server, and
   the sqlite-vec memory backend all converge on: co-locate the control function inside the
   always-running operator pod instead of a separate stateful system.
3. **Move safety gates from infra-selection to explicit policy.** The kata danger-flag gate
   (`--dangerously-skip-permissions` requires a microVM) should be an explicit policy, not
   an implicit consequence of choosing kata — so gVisor can be the default safely.
4. **Defer-don't-rip behind clean interfaces.** `identity.Source` (5 points), the NATS
   `Queue/Store/Mailbox`, kopia's `VersionedStore`, and the memory `Backend` are all cleanly
   abstracted — which is exactly why the swaps are tractable and the safety-critical ones can wait.

## Ranked roadmap

| # | Bead | Dependency | Action | Effort | Impact |
|---|---|---|---|---|---|
| P0 ⚡ | `7fr.1` | Kata/Firecracker | **Default to gVisor** (already wired + proven j77.1); kata opt-in | S | high |
| P0 ⚡ | `7fr.2` | Memory (Neo4j/Redis) | **Drop graph+KV backends** from default (P2, never used live) | S | high |
| P1 ⚡ | `7fr.3` | kopia + MinIO | **MinIO single-pod (--fs)** reference + expose ephemeral-kopia | S | high |
| P1 ⚡ | `7fr.4` | Knative + Kourier | **Replace with KEDA or fixed-replica** Deployment for agentgateway | M | high |
| P1 | `7fr.5` | Memory (pgvector) | **Embedded sqlite + sqlite-vec** vector backend (default self-host) | L | high |
| P2 | `7fr.6` | Karpenter/EKS | **Default to ClusterAutoscaler/static pools** (variant already exists); Karpenter opt-in | M | high |
| P2 | `7fr.7` | NATS JetStream | **Embed nats-server in the operator** pod (single-binary); external opt-in | M | medium |
| P2 ⚡ | `7fr.8` | Dex | **Trim reference config** to static-password + GitHub (keep Dex per D9) | S | low |
| P3 | `7fr.9` | SPIRE/SPIFFE | **POST-GA embedded SVID issuer** (opt-in); keep SPIRE default through GA | L | medium |

⚡ = quick win.

## Per-dependency recommendations
- **Kata → gVisor (default).** gVisor is already wired (`runtimeclass.go` handler=runsc,
  `AllowGvisorFallback`, `dangerFlagViolation`) and proven without KVM/metal (j77.1). Biggest
  single self-host blocker removed. Keep kata-fc for the top isolation tier; keep danger-flags microVM-gated.
- **Memory: drop Neo4j+Redis; add sqlite-vec.** The graph/KV backend kinds are P2 and never
  used in M1–M5 live e2e — drop from the default (interface retained). A sqlite + sqlite-vec
  `VectorBackend` (~500 LOC) collapses the vector tier into the operator binary + 1 PVC; pgvector stays opt-in.
- **kopia/MinIO: keep the VersionedStore contract, lighten the topology.** kopia's checkpoint/
  History/Diff/GC is load-bearing for D6 recovery — keep it. Make the reference backup a
  single-pod MinIO (`--fs`) and surface ephemeral-kopia (no object store at all, u9k.5).
- **Knative → KEDA / fixed-replica.** agentgateway is ~200 LOC, stateless, uses no
  Knative-specific features — a whole serving control plane + Kourier for one service. (Also
  clears the `u9k.7` agentgateway-image-404 by publishing a normal image.)
- **Karpenter → ClusterAutoscaler/static (self-host default).** The operator already emits a
  CAS node-group spec (`clusterautoscaler.go`; `AgentNodePool.spec.provider`). Karpenter stays
  the cloud/EKS opt-in. Pairs with gVisor-default so most pools need no metal.
- **NATS: embed, don't rip.** The platform is NATS-coupled via JWT/account ACLs — embed
  `nats-server` in the operator pod (file-backed) so self-hosts need no separate NATS; external NATS opt-in.
- **Dex: keep (D9), trim config.** JWKS-verified attach tokens are safety-relevant; Dex is a
  single binary. Just ship a minimal connector config (static-password + GitHub).
- **SPIRE: keep through GA; embedded issuer post-GA.** Safety-critical (D3, R-SEC-1 broker
  boot gate, TraT sender-constraint). Post-GA, an opt-in embedded RS256+X.509 minter in the
  operator can retire the SPIRE server/agent/CSI — **only if** it preserves per-Pod SVIDs +
  crypto tenant isolation + rotation (the hard floor).

## Realized: the lightweight self-host profile

`deploy/helm/values-lightweight.yaml` is a non-breaking overlay (the chart's hard
defaults stay kata-fc + knative) that flips a self-host install to the lighter paths
already supported by the chart — verified to `helm lint` clean and render a plain
**Deployment** (no Knative/Kourier) + a **gvisor** RuntimeClass (handler `runsc`):

```bash
helm install smol-agents ./deploy/helm -f deploy/helm/values-lightweight.yaml
```

It covers the two biggest reductions:
- **gVisor instead of Kata** (`7fr.1`) — no `/dev/kvm`, no bare-metal, no devmapper.
  Danger-flags stay kata-only (D3), so those harnesses opt into a kata RuntimeClass.
- **Deployment instead of Knative** (`7fr.4`) — no Knative Serving + Kourier control
  plane (trades scale-to-zero for a fixed/HPA-scalable Deployment; add KEDA if you want
  scale-to-zero back without Knative).

### Companion self-host configs (separate deploys)
- **Memory (`7fr.2`)** — the reference memory deploy uses the in-memory vector backend
  (`test/e2e/manifests/memory.yaml`: `-backend=vector-inmem`); the Neo4j graph + Redis KV
  backends are opt-in code paths, NOT deployed by default. Keep them out of self-host.
  (Embedded sqlite-vec for durable vectors is `7fr.5`.)
- **Storage (`7fr.3`)** — use single-pod MinIO (`minio server --fs /data` on a PVC) for
  `storage.agentfs.backup.s3`, or the ephemeral-kopia path (no object store) on HEAD.
- **Node provisioning (`7fr.6`)** — set `AgentNodePool.spec.provider: ClusterAutoscaler`
  (emits a node-group spec for your IaC/ASG) or use static node pools; reserve
  `provider: Karpenter` (the CRD default) for EKS/cloud.
- **OIDC (`7fr.8`)** — `deploy/oidc/dex.yaml` is already minimal (static-password + a
  kubernetes connector; no LDAP/SAML/Google). Add a GitHub connector for real users.
