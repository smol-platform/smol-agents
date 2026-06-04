# Spec: Wire AgentNetwork onto the run / session / serving datapath

> **✅ Decisions resolved 2026-06-03 — see [decisions.md](../design/decisions.md).** D1+D3: the SmolAgent serving-pod egress floor is **default-on** and mandatory P0; plus the validated apiserver-endpoint allow for A2A (cftest/AWS probes); eBPF datapath enforcement required. Where this doc still says OPEN/PROPOSED and conflicts, the decision log wins.

> **Status: DESIGN — not implemented. Target v0.2.x → v0.3.0.** Authored 2026-06-03 against the tree at HEAD.
>
> This is an implementation-grade spec. It extends, and does not duplicate, the honesty pass in
> [`docs/design/agentnetwork-agentpolicy-interaction.md`](../design/agentnetwork-agentpolicy-interaction.md)
> (which proves the *absence* of enforcement). That doc says **what is not wired and why**; this doc says
> **exactly what to build to wire it**, file by file. Where the two overlap, the design doc is the
> rationale and this spec is the construction plan.
>
> Category: `stub-impl` — the CRD schema, the eBPF map compiler, and the SPIFFE proxy are all real and
> tested; what is missing is the operator-side *datapath caller* that turns a bound `AgentNetwork` into
> pod mutations + a tightened egress policy. We are wiring existing machinery, not designing new
> transports.

---

## 1. Summary

`AgentNetwork` is the platform's per-agent egress CRD: an identity-aware proxy sidecar, an eBPF
cgroup allow-list / transparent-redirect, TraT + secretless-credential injection, and a userspace
WireGuard mesh. **Today it fires on zero pods.** The run/session datapath gets only a *static,
agent-independent* default-deny `NetworkPolicy` (`operator/internal/builders/run_sandbox.go:60-123`)
that allows all public 80/443 and **ignores** every `AgentNetwork.egress.allow` entry; the SmolAgent
*serving* datapath gets **no NetworkPolicy at all**. This spec wires `AgentNetwork` onto all three
datapaths in two shippable phases:

- **Phase 1 (M) — NetworkPolicy merge + serving floor.** The `AgentRun` / `AgentSession` reconcilers
  resolve bound `AgentNetwork`s by `agentSelector`, and a new `builders.AttachAgentNetwork` *narrows*
  the egress `NetworkPolicy` to the union of `egress.allow` CIDRs/ports (intersected with the static
  floor — a network can only tighten, never loosen). The SmolAgent serving path gets the same
  default-deny floor it lacks today.
- **Phase 2 (L) — proxy + eBPF injection.** `AttachAgentNetwork` injects the `identityProxy` sidecar
  (reusing `pkg/agentnet/proxy`) and, when `egress.enforcement` includes an eBPF mode, programs the
  cgroup maps from the operator via `pkg/agentnet/cgroup.MapDriver` (today only `cmd/ebpf-probe` does
  this), fail-closed if the `ebpf-loader` DaemonSet is absent.

The outcome: `agentSelector` becomes a real binding, not a status *count*; an agent that should only
reach `api.github.com` and an internal model gateway is actually caged to those destinations;
serving pods stop being silently uncaged; and the eBPF allow-list / redirect compiler that exists
only in a probe binary finally runs under the operator. **Composition is AND** — an agent matched by N
networks gets the intersection of all their allow-lists and every one of their sidecars.

---

## 2. Current state

### 2.1 What exists (real, tested)

| Component | Where | What it does today |
|---|---|---|
| `AgentNetworkSpec` (full schema) | `pkg/agentmodel/v1/agentnetwork.go:26-180`; CRD wrapper `operator/api/agentmodel/v1/agentnetwork.go:21-27` | `kind` ∈ `{identityProxy, wireguardMesh}`; `agentSelector`; `identityProxy.{resources, egress, tts}`; `egress.{enforcement, allow, redirectCIDRs}`. Validated by `ValidateAgentNetwork` (`agentnetwork.go:253-361`). |
| `AgentNetworkReconciler` | `operator/internal/controllers/agentmodel/agentnetwork_controller.go:39-153` | Validates spec, resolves the WG private-key Secret, sets per-kind status counters, and writes `Status.BoundAgents` as a **count** of selector-matching Agents (`:116-127`). Injects nothing, mutates no pod (its own doc comment, `:30-38`, says so). |
| eBPF map compiler + driver | `pkg/agentnet/cgroup/maps.go` — `Compile` (`:72-100`), `EncodeAllow` (`:130-159`), `EncodeRedirect` (`:105-124`), `MapDriver` iface (`:47-51`), `FakeDriver` (`:163-198`) | Pure functions: turn `EgressPolicy` → `[]AllowEntry`/`[]RedirectEntry` → BPF LPM-trie + hash-map keys. **No `corev1` import — not a pod builder.** Driver writes pinned maps under `/sys/fs/bpf/smol-agents/` (`doc.go:1-11`). |
| SPIFFE identity proxy | `pkg/agentnet/proxy/sidecar.go:19-108` (`Sidecar`), `tcp.go`, `http.go` | TCP mTLS byte-forward + HTTP reverse-proxy with JWT-SVID minting, TraT injection (`inject_test.go:57`), agent-blind credential injection (`inject_test.go:99`). **No `corev1` import — `Sidecar` is a runtime, not a pod-injection builder.** |
| Static run/session egress floor | `BuildAgentRunEgressPolicy` / `BuildAgentSessionEgressPolicy` / `buildEgressPolicy` (`operator/internal/builders/run_sandbox.go:55-123`) | Default-deny egress: DNS + in-cluster RFC1918 (any port) + public 80/443, with `169.254.0.0/16` blocked. Created via `ensureRunEgressPolicy` (`agentrun_controller.go:336-347`); session via `agentsession_controller.go:124`. GC-owned (`agentrun_controller.go:112`). |
| eBPF probe (the only map writer) | `cmd/ebpf-probe/main.go:47,98,137,215` | Resolves the pod's own cgroup v2 path + inode (`selfCgroupPath`/`cgroupID`, `:74-84,264-285`), calls `cgroup.EncodeAllow`/`EncodeRedirect`, attaches the `cgroup_skb/egress` + `connect4` programs. e2e-only. |
| `ebpf-loader` DaemonSet | `operator/internal/builders/ebpfloader.go:81-239`; feature gate `operator/internal/controllers/features/ebpf.go:18-39` | Privileged/minimal per-distro DaemonSet that loads CO-RE programs and pins maps to `pinRoot` (default `/sys/fs/bpf/smol-agents`, `ebpfloader.go:59-62`). The operator never *writes* the pinned maps — it only loads them. |

### 2.2 What is NOT wired (the gap this spec closes)

- **`AgentNetwork` has zero refs on the run datapath.** `agentrun_controller.go` and
  `builders/agentrun.go` never mention `AgentNetwork`; `BuildAgentRunPod(run, agent)`
  (`agentrun.go:20`) takes no network parameter. Same for `agentsession_controller.go`.
- **The egress floor ignores allow-lists.** `BuildAgentRunEgressPolicy(run)` takes only
  `*amv1.AgentRun` (`run_sandbox.go:60-63`); its own comment admits "A tighter per-Agent allow-list
  (AgentNetwork CIDRs) can layer on top later" (`run_sandbox.go:58-59`). That seam is unrealized.
- **The eBPF compiler runs only in `cmd/ebpf-probe`.** Verified: zero non-test imports of any
  `pkg/agentnet/*` subpackage under `operator/` (the only importers are `cmd/ebpf-probe`,
  `cmd/spiffe-probe`, and `test/e2e/...`).
- **The proxy `Sidecar` has no pod-injection builder.** `pkg/agentnet/proxy` is a *runtime* (it
  `Run(ctx)`s in-process); nothing renders it as a `corev1.Container` on a pod.
- **SmolAgent serving pods get NO NetworkPolicy.** `BuildAgentPodSpec` (`workload.go:41-111`) emits no
  egress policy, and no SmolAgent controller creates one (grep confirms `run_sandbox.go` +
  `agentrun`/`agentsession` controllers are the *only* NetworkPolicy emitters). A long-running served
  agent can reach `169.254.169.254` and any public host — strictly weaker than the ephemeral run path.
- **`Status.BoundAgents` is a count, not a binding** (`agentnetwork_controller.go:127`). Nothing
  downstream consumes it.

> **NOT built, do not claim otherwise:** WireGuard mesh injection on the datapath, TraT/credential
> injection on the run path, per-destination L7 filtering. WireGuard injection is explicitly
> **out of scope** for this spec (see §10) — it shares the sidecar seam but needs the userspace
> netstack device wired separately.

---

## 3. External interface research

Not applicable — this is internal-only datapath wiring (operator ↔ its own CRDs ↔ its own
`pkg/agentnet` packages). No external API surface to confirm.

---

## 4. Design

### 4.1 Two enforcement tiers, one resolver

```
                          AgentNetwork resolution (shared)
                          ┌───────────────────────────────────────────┐
   AgentRun / AgentSession│  resolveBoundNetworks(ctx, agent)          │
   / SmolAgent reconciler │  → list AgentNetworks in ns                │
        │                 │  → filter: agentSelector ⊆ agent.Labels    │
        │                 │  → AND-compose into a NetworkPlan          │
        ▼                 └───────────────────────────────────────────┘
   NetworkPlan = { AllowRules[], RedirectCIDRs[], ProxyResources[], Enforcement, ProxyNeeded, EbpfNeeded }
        │
        ├────────────── Phase 1 (M) ────────────────────────────────────────────────┐
        │   builders.AttachAgentNetwork(pod, plan)  +  egress NetworkPolicy merge    │
        │     • NetworkPolicy: floor ∩ plan.AllowRules  (narrow only)                │
        │     • serving path: emit the floor it lacks today                          │
        └────────────────────────────────────────────────────────────────────────────┘
        │
        └────────────── Phase 2 (L) ────────────────────────────────────────────────┐
            • inject identityProxy sidecar (pkg/agentnet/proxy → corev1.Container)    │
            • if Enforcement ⊇ ebpf*: program cgroup maps via cgroup.MapDriver        │
              from a per-node operator agent, fail-closed if ebpf-loader absent       │
            └────────────────────────────────────────────────────────────────────────┘
```

**Tier 1 — NetworkPolicy floor (CNI-enforced, always present).** A coarse default-deny that *any*
cluster with a NetworkPolicy-honoring CNI enforces. Bound `AgentNetwork.egress.allow` rules
**narrow** the public rule's destination set. This is the portable, no-prereq tier and ships first.

**Tier 2 — eBPF cgroup cage (kernel-enforced, opt-in).** When `egress.enforcement` includes
`ebpfAllowList`/`ebpfRedirect`/`ebpfBoth`, the operator additionally programs the pinned BPF maps
keyed by the pod's cgroup inode. This is per-IP/port-precise, survives a CNI that doesn't honor
NetworkPolicy, and supports transparent redirect to the proxy sidecar — but it requires the
`ebpf-loader` DaemonSet and is node-coupled. **Tier 2 layers on top of Tier 1; it never replaces it.**

### 4.2 The `NetworkPlan` (composition result)

A pure, AND-composed projection of every matched `AgentNetwork`, computed once per reconcile:

```go
// pkg/agentnet/plan (new) — pure, no k8s client, no corev1.
type NetworkPlan struct {
    AllowRules     []v1.EgressRule   // union of every matched egress.allow
    RedirectCIDRs  []string          // union of every matched redirectCIDRs
    ProxyResources []v1.ResourceTarget // concat of every identityProxy.resources
    TTS            *v1.TTSRef        // first non-nil TTS (validated unique, see §7)
    Enforcement    string            // strongest of the matched enforcement modes
    Networks       []string          // names of contributing AgentNetworks (for status/events)
}
func (p NetworkPlan) ProxyNeeded() bool { return len(p.ProxyResources) > 0 }
func (p NetworkPlan) EbpfNeeded() bool  { /* Enforcement ∈ {ebpfAllowList,ebpfRedirect,ebpfBoth} */ }
func (p NetworkPlan) Empty() bool       { return len(p.Networks) == 0 }
```

`BuildNetworkPlan(nets []v1.AgentNetworkSpec) (NetworkPlan, error)` is pure → unit-testable without
a cluster, and lets the operator hash the plan to detect drift (the same property `cgroup.Compile`
was designed for, `maps.go:69-71`).

### 4.3 Why NetworkPolicy-only floor first

NetworkPolicy is the only egress control that works on *every* target cluster with no node prereq
(it is already the v0.2.0 floor). The eBPF tier needs the loader DaemonSet, cgroup-path resolution,
and a privileged per-node agent — all node-coupled and only present where `ebpfLoader.enabled`. By
shipping Tier 1 first we close the largest gap (serving pods uncaged; allow-lists ignored) with pure
plumbing, and we make the eBPF tier a *strict* defense-in-depth add-on rather than a hard
dependency.

---

## 5. Concrete changes

### 5.1 New package: `pkg/agentnet/plan`

| File | Contents |
|---|---|
| `pkg/agentnet/plan/plan.go` (new) | `NetworkPlan` struct + `BuildNetworkPlan([]v1.AgentNetworkSpec) (NetworkPlan, error)`. AND-composition (§4.2/§7). Pure: imports only `pkg/agentmodel/v1`. |
| `pkg/agentnet/plan/plan_test.go` (new) | Table tests: empty, single, multi-AND, conflicting `localPort` → error, strongest-enforcement selection. |

### 5.2 Shared resolver (operator side)

New file `operator/internal/controllers/agentmodel/agentnetwork_bind.go`:

```go
// resolveBoundNetworks lists in-namespace AgentNetworks whose agentSelector
// matches the agent's labels and AND-composes them into a NetworkPlan.
// An empty selector binds nothing (R-AN-API-2). Returns (plan, conflictErr).
func resolveBoundNetworks(ctx context.Context, c client.Reader, agent *amv1.Agent) (plan.NetworkPlan, error)
```

- Lists `amv1.AgentNetworkList` `InNamespace(agent.Namespace)`.
- For each, match `spec.agentSelector` ⊆ `agent.Labels` (a network binds an agent iff *every*
  selector key/value is present — `labels.SelectorFromSet(...).Matches(...)`).
- Passes the matched `[]pure.AgentNetworkSpec` to `plan.BuildNetworkPlan`.
- Used identically by the AgentRun, AgentSession, and (Phase 1) SmolAgent reconcilers.

> Reuses the *exact* selector semantics the `AgentNetworkReconciler` already uses to compute
> `BoundAgents` (`agentnetwork_controller.go:117-126`). After this lands, the controller's count and
> the datapath's binding are guaranteed consistent — they call the same matcher.

### 5.3 New builder: `builders.AttachAgentNetwork`

New file `operator/internal/builders/agentnetwork.go`:

```go
// AttachAgentNetwork mutates a run/session/serving pod per the resolved plan.
// Phase 1: no-op on the pod itself (egress is the merged NetworkPolicy, below).
// Phase 2: injects the identityProxy sidecar + SPIFFE CSI volume when
// plan.ProxyNeeded(); the run/loop container reaches upstreams via the
// sidecar's localAddr/localPort.
func AttachAgentNetwork(pod *corev1.Pod, plan plan.NetworkPlan)
```

and the egress-merge entrypoint, extending `run_sandbox.go`:

```go
// BuildEgressPolicyWithPlan renders the default-deny egress NetworkPolicy and,
// when plan has AllowRules, REPLACES the coarse "public 80/443 to 0.0.0.0/0"
// rule with a per-(CIDR,ports,proto) allow-list. The static floor's blocks
// (metadata 169.254/16, in-cluster carve-out, DNS) are preserved unconditionally
// so a plan can only NARROW, never loosen.
func BuildEgressPolicyWithPlan(name, ns, component string, sel map[string]string, plan plan.NetworkPlan) *networkingv1.NetworkPolicy
```

Existing `BuildAgentRunEgressPolicy` / `BuildAgentSessionEgressPolicy` become thin wrappers that pass
an empty plan (identical bytes to today → no behavior change when nothing is bound). The new public
entrypoint takes the plan.

**Merge rule (the load-bearing part):**

- Keep rules 1 (DNS) and 2 (in-cluster RFC1918 any-port) from `buildEgressPolicy`
  (`run_sandbox.go:104-109`) **verbatim** — these are infra reachability the agent always needs.
- For rule 3 (public): if `plan.AllowRules` is empty, keep the current `0.0.0.0/0` except
  metadata/in-cluster on 80/443. If non-empty, **replace** it with one egress rule per allow entry:
  `{To: [{IPBlock: {CIDR: rule.CIDR}}], Ports: [rule.Ports × rule.Protocol]}`. Validation already
  forbids `169.254.0.0/16` re-opening only at admission (§5.6); as a hard belt-and-suspenders, the
  builder drops any allow CIDR that overlaps `metadataBlockedCIDR` and emits an event.
- An empty `Ports` on an allow rule means "any port to that CIDR" (mirrors
  `cgroup.Compile`’s `ports == 0 → wildcard`, `maps.go:86-89`).

> NetworkPolicy can express CIDR + port but **not** SNI/host. `egress.allow` is CIDR-based already
> (`agentnetwork.go:170-180`), so the merge is lossless for Tier 1. Hostnames in proxy `resources`
> are a Tier-2 / proxy concern, not a NetworkPolicy concern.

### 5.4 AgentRun reconciler wiring

In `agentrun_controller.go`, inside the pod-creation branch (after `resolveRunSandbox`
succeeds, `:153-161`, before `ensureRunEgressPolicy` `:214`):

```go
netPlan, planErr := resolveBoundNetworks(ctx, r.Client, agent)
if planErr != nil {                      // e.g. localPort conflict across networks
    r.markPending(run, "NetworkConflict", planErr.Error())
    return r.updateRunStatus(ctx, run, ctrl.Result{RequeueAfter: 15 * time.Second})
}
// Phase 2 only: hold Pending if the plan needs eBPF but the node has no loader.
if netPlan.EbpfNeeded() && !r.ebpfAvailable(ctx, /* node */) {
    r.markPending(run, "EbpfLoaderMissing", "egress.enforcement requires the ebpf-loader DaemonSet")
    return r.updateRunStatus(ctx, run, ctrl.Result{RequeueAfter: 15 * time.Second})
}
```

- After `BuildAgentRunPod` + `ApplyRunSandbox` (`:186-189`): `builders.AttachAgentNetwork(desired, netPlan)`.
- Change `ensureRunEgressPolicy` (`:336-347`) to call `BuildEgressPolicyWithPlan(run.Name+"-egress",
  …, netPlan)`.
- Add a `Watches(&amv1.AgentNetwork{}, …)` mapping so editing an `AgentNetwork` re-reconciles runs of
  matching agents (mirrors the Secret→AgentNetwork watch at `agentnetwork_controller.go:50`). For the
  *ephemeral* run path this only affects *future* runs (a running pod's NetworkPolicy is not hot-swapped
  mid-run — see §10); for `AgentSession` it re-renders the live policy.

### 5.5 AgentSession + SmolAgent serving wiring

- **AgentSession** (`agentsession_controller.go`): identical pattern — call `resolveBoundNetworks`
  before the session pod is built (around `:124-135`), `AttachAgentNetwork(pod, plan)`, and switch
  `BuildAgentSessionEgressPolicy` → `BuildEgressPolicyWithPlan`. Because sessions are long-lived, an
  `AgentNetwork` edit re-renders the live policy on the next reconcile.
- **SmolAgent serving** (Phase 1, the new floor): the SmolAgent reconciler must, alongside the
  Deployment/StatefulSet/KSvc from `BuildAgentPodSpec` (`workload.go:41`), create a default-deny
  egress `NetworkPolicy` selecting the agent's serving pods. New
  `builders.BuildSmolAgentEgressPolicy(cr *v1.SmolAgent, plan)` reusing `BuildEgressPolicyWithPlan`
  with the serving pod selector. **This is behavior change** — served agents that today reach
  arbitrary hosts will be capped to DNS + in-cluster + public 80/443 (or the bound allow-list).
  Gate it behind `spec.features` so existing deployments opt in (see §10 open decision).

### 5.6 Validation (CRD + admission)

- `ValidateAgentNetwork` already validates `egress.allow` CIDRs/proto (`agentnetwork.go:342-359`).
  **Add:** reject any `egress.allow[].cidr` that is a subnet of `169.254.0.0/16` (the floor must be
  inviolable). One-liner in `validateIdentityProxy`.
- **Add (Phase 2):** when `enforcement` ⊇ `ebpfAllowList`, reject `allow` CIDRs coarser than `/32`
  — the eBPF hash-map only supports `/32` today (`EncodeAllow`, `maps.go:136-137`). NetworkPolicy
  (Tier 1) accepts any prefix, so this constraint applies **only** when eBPF is requested. Surface it
  at admission so a `/24` allow rule under `ebpfAllowList` fails the write, not at pod-create time.
- **Add:** cross-network `localPort`/`localAddr` conflict detection lives in `BuildNetworkPlan`
  (returns error) and is surfaced as the run/session `NetworkConflict` Pending reason (§5.4); it is
  also worth a validating-webhook check across all networks bound to one agent (deferred to the
  agentpolicy-enforcement spec's webhook, which already proposes the admission plumbing).

### 5.7 Phase 2 — eBPF programming from the operator

The compiler + driver exist (`maps.go`); the missing piece is *who calls the driver with the pod's
cgroup id, on the node*. Two options (decision in §10):

1. **Extend the `ebpf-loader` DaemonSet** to expose a small node-local gRPC/UDS API
   (`ProgramAllow(cgroupID, entries)`), called by the operator after the pod is `Running` and its
   cgroup inode is known. The loader already runs privileged on every node, mounts bpffs
   (`ebpfloader.go:99-101`), and owns the pinned maps — it is the natural map-writer. The operator
   resolves the pod's cgroup inode the way the probe does (`cmd/ebpf-probe/main.go:264-285` is the
   reference: `/sys/fs/cgroup` + the pod's cgroup path → `syscall.Stat` inode).
2. A separate `agentnet-agent` DaemonSet that watches pods + AgentNetworks and writes maps directly
   via `cgroup.MapDriver`. More moving parts; clearer separation.

Either way the new code is: a `MapDriver` production impl (cilium/ebpf wrapper around the pinned
maps — the `doc.go:1-11` contract), the cgroup-inode resolver lifted from the probe, and the
fail-closed gate `ebpfAvailable` (§5.4) that checks the loader DaemonSet is `Ready` on the target
node before admitting an `ebpf*` plan.

### 5.8 Status surfacing

- `AgentRun.Status` / `AgentSession.Status`: record the bound network names + active enforcement tier
  (e.g. a `Networks []string` and `EgressEnforcement string` field) so `kubectl get agentrun -o yaml`
  shows *which* networks shaped the cage. (New status fields; CRD change.)
- Keep `AgentNetworkStatus.BoundAgents` (`agentnetwork.go:245`) — now consistent with the datapath
  since both use `resolveBoundNetworks`’ matcher.

---

## 6. Data / control flow

End-to-end for an `AgentRun` whose `Agent` is matched by an `AgentNetwork{kind: identityProxy,
egress.allow: [api.github.com-CIDR/32:443], enforcement: ebpfBoth}`:

```
1. AgentNetworkReconciler validates the network, sets BoundAgents (count). [exists today]
2. AgentRun created → AgentRunReconciler.Reconcile.
3. resolveRunSandbox → kata-fc (fail-closed).                  [exists, :153]
4. resolveBoundNetworks(agent) → NetworkPlan{                  [NEW]
     AllowRules:[{github/32, 443, tcp}], Enforcement:"ebpfBoth",
     ProxyResources:[], Networks:["gh-egress"] }
5. EbpfNeeded() && loader Ready on node? no → Pending(EbpfLoaderMissing). [NEW, Phase 2]
                                          yes ↓
6. prepareRun / ensureRunSpec / ensureBrokerConfig.            [exists, :166-184]
7. BuildAgentRunPod + ApplyRunSandbox.                         [exists, :186-189]
8. AttachAgentNetwork(pod, plan):                              [NEW]
     Phase1: no pod change. Phase2: inject proxy sidecar (none here — no proxy resources).
9. ensureRunEgressPolicy → BuildEgressPolicyWithPlan:          [CHANGED, :214/:336]
     egress = [DNS] + [in-cluster any-port] + [github/32:443]   (public-all rule REPLACED).
10. Create pod.  CNI enforces the NetworkPolicy (Tier 1).      [exists]
11. Pod Running → operator resolves cgroup inode → loader.ProgramAllow(cgID,[github/32:443]). [NEW, Phase 2]
     Kernel cgroup_skb/egress now drops anything but github:443 (Tier 2, defense-in-depth).
12. Pod completes → foldRunResult.                             [exists, :398]
```

Without any bound network (the common case), steps 4-5/8/11 are no-ops and step 9 renders the
identical floor as today — **zero behavior change for unbound agents.**

---

## 7. Composition semantics

These rules are normative for `BuildNetworkPlan` and match the design doc §7
([`agentnetwork-agentpolicy-interaction.md`](../design/agentnetwork-agentpolicy-interaction.md)):

- **Binding (`agentSelector`):** an Agent is bound by *every* `AgentNetwork` whose selector it
  matches. Empty selector binds nothing (`agentnetwork_controller.go:115-117`).
- **Multiple matches compose by AND:** the agent gets every matched network's sidecars and the
  **union** of their `egress.allow` rules and `redirectCIDRs`. (Union of allow-lists is the correct
  AND for *reachability*: each network grants the destinations it lists; the agent may reach the
  union of what all its networks permit, still bounded above by the static floor.)
- **`localPort` / `localAddr` conflict** across matched proxy resources → `BuildNetworkPlan` returns
  an error → run/session held `Pending` reason `NetworkConflict` (§5.4). Two networks cannot both
  claim the same local listener.
- **`TTS` uniqueness:** if two matched `identityProxy` networks set conflicting `tts.url`,
  `BuildNetworkPlan` errors (one proxy sidecar, one TTS). Identical TTS refs collapse.
- **`enforcement` strength order:** `none < ebpfAllowList ≈ ebpfRedirect < ebpfBoth`; the plan takes
  the strongest among matched networks (a network requesting `ebpfBoth` upgrades the whole plan).
- **Egress floor is the ceiling of openness:** the merged NetworkPolicy can only *narrow* the static
  floor; an allow rule can never re-open `169.254.0.0/16` (enforced in builder + admission, §5.3/§5.6).

---

## 8. Security model

How this composes with the existing controls:

| Layer | Today | After this spec |
|---|---|---|
| **kata-fc sandbox** | Run/session pods pinned to a hardened RuntimeClass, fail-closed (`agentrun_controller.go:153-161`; `ApplyRunSandbox` `run_sandbox.go:45-53`). | Unchanged. `AttachAgentNetwork` runs *after* `ApplyRunSandbox` and never relaxes it. |
| **Egress (NetworkPolicy)** | Static floor: all public 80/443 open; metadata blocked (`run_sandbox.go:110-119`). Serving pods: **none**. | Floor narrowed to bound allow-lists; **serving pods get the floor they lack** (§5.5). Still CNI-dependent. |
| **Egress (eBPF)** | e2e-probe only. | Operator programs the cgroup cage as defense-in-depth; survives a non-NetworkPolicy CNI (Tier 2). |
| **Broker / secretless** | Broker serves secrets over UDS; agent-blind credential injection lives in `pkg/agentnet/proxy` HTTP path (`inject_test.go:99`) but is unwired on runs. | Once the proxy sidecar is injected (Phase 2), TraT + agent-blind credential injection finally fire on the run path. The proxy fails closed on mint failure (`inject_test.go:153,185`). |
| **SPIFFE identity** | Serving pods mount the SPIFFE CSI volume (`workload.go:155-158`); run pods do not. | The injected proxy `Sidecar` needs an `identity.Source` (`sidecar.go:21,35`) → Phase 2 must add the SPIFFE CSI volume to run/session pods (new dependency, called out in §10). |

**New attack surface + mitigations:**

1. *A tenant crafts an `egress.allow` re-opening metadata.* → Rejected at admission and dropped in the
   builder (§5.3/§5.6). The floor is inviolable by construction.
2. *eBPF map poisoning via the loader API (Phase 2).* → The `ProgramAllow` API is node-local
   (UDS/in-cluster gRPC), keyed by cgroup id the operator resolves (not tenant-supplied), and the
   loader only the operator's SA may call. Treat parity with the existing privileged DaemonSet trust
   boundary.
3. *Selector spoofing — an agent adds labels to attract a permissive network.* → `agentSelector` is
   AND-union; matching *more* networks only ever *adds* allowed destinations, never removes the floor.
   A permissive network is a tenant-authored object in the same namespace; this is RBAC-bounded, not a
   privilege escalation across tenants. (Cross-namespace networks are not matched — resolver is
   `InNamespace`.)
4. *NetworkPolicy gives a false sense of containment on a CNI that ignores it.* → Documented limit
   (design doc §5); Tier 2 (eBPF) is the kernel-enforced answer for clusters that need it.

---

## 9. Phasing & effort

| Phase | Scope | Effort | Depends on |
|---|---|---|---|
| **P1a** | `pkg/agentnet/plan` (`NetworkPlan` + `BuildNetworkPlan`, pure, tested) | **S** | — |
| **P1b** | `resolveBoundNetworks` + `BuildEgressPolicyWithPlan` merge + wire AgentRun **and** AgentSession egress; `AttachAgentNetwork` as no-op stub; `AgentNetwork` watch | **M** | P1a |
| **P1c** | SmolAgent serving default-deny floor (feature-gated) | **S–M** | P1b; coordinate w/ [`custom-agent-images`](../design/custom-agent-images.md) |
| **P1d** | Validation: forbid metadata re-open; status fields (`Networks`, `EgressEnforcement`) | **S** | P1b |
| **P2a** | `identityProxy` sidecar injection in `AttachAgentNetwork` + SPIFFE CSI volume on run/session pods | **L** | P1b; SPIFFE-on-run wiring |
| **P2b** | Operator-side eBPF: production `MapDriver`, cgroup-inode resolver, loader `ProgramAllow` API, `ebpfAvailable` fail-closed gate, `/32`-under-eBPF admission rule | **L** | P2a; `ebpf-loader` DaemonSet (`ebpfloader.go`) |

**Dependencies on sibling specs (see §11 cross-links):**

- `agentpolicy-enforcement` — owns the validating-admission-webhook plumbing this spec reuses for the
  metadata-block + `/32`-under-eBPF + cross-network conflict checks. Land the webhook there; register
  the AgentNetwork rules from here.
- `egress-credentials` (existing feature) — P2a turns its secretless credential injection from
  unwired-on-runs into live on the run path.
- `dynamic-credential-backends` — the broker the injected proxy mints from.

Recommended cut line: **ship P1 (a–d) as one increment** (closes the two biggest honest gaps:
allow-lists ignored + serving pods uncaged, with no node prereq), then P2 as a follow-up gated on
eBPF being a hard requirement.

---

## 10. Risks & open decisions

1. **Serving-pod floor is behavior change.** Adding a default-deny egress NetworkPolicy to existing
   SmolAgent deployments can break agents that legitimately call third-party APIs on non-80/443 ports
   or rely on now-blocked destinations. **Decision needed:** opt-in feature flag (safe, slow rollout)
   vs. default-on with a documented migration. *Recommendation: feature-gated opt-in for one minor,
   then default-on.*
2. **eBPF map-writer placement (§5.7).** Extend `ebpf-loader` with a programming API vs. a new
   `agentnet-agent` DaemonSet. *Recommendation: extend the loader* — it already owns the pinned maps
   and runs privileged; a second privileged DaemonSet duplicates the trust boundary.
3. **Hot-swap on AgentNetwork edit.** For ephemeral runs, editing a network mid-run does **not**
   re-cage the running pod (NetworkPolicy is re-evaluated by the CNI on change, but eBPF maps keyed by
   a now-gone cgroup are not). *Decision: document that network changes apply to future runs and to
   live sessions only; do not attempt mid-run eBPF re-program in v1.*
4. **SPIFFE on run/session pods (P2a prereq).** The proxy `Sidecar` needs an `identity.Source`
   (`sidecar.go:21`). Run pods don't mount the SPIFFE CSI volume today. *Decision: P2a must add the
   CSI volume to run/session pods — confirm the SPIRE `ClusterSPIFFEID` covers the run SA, or runs
   will get no SVID and the proxy fails closed.*
5. **WireGuard mesh injection is out of scope here.** It shares the sidecar seam but needs the
   userspace netstack device + secret wiring; track it as a separate increment, not folded into P2.
6. **`/32`-only eBPF allow-list.** `EncodeAllow` rejects CIDRs coarser than `/32` (`maps.go:136-137`).
   For Tier 1 (NetworkPolicy) any prefix works; the `/32` constraint only bites under `ebpfAllowList`.
   *Decision: keep `/32`-only for eBPF v1 and surface it at admission, or extend the BPF map to LPM
   for allow (larger map, more work) — defer the LPM extension.*
7. **CNI dependency remains.** Tier 1 is only as strong as the cluster's CNI. On the cftest single-node
   k0s box, confirm the CNI honors egress NetworkPolicy before claiming Tier-1 enforcement in e2e.

---

## 11. Test plan

**Unit (no cluster):**

- `pkg/agentnet/plan`: empty/single/multi-AND composition; `localPort` conflict → error; conflicting
  `TTS` → error; strongest-enforcement selection; metadata-CIDR rejection.
- `builders.BuildEgressPolicyWithPlan`: empty plan == byte-identical to today's
  `BuildAgentRunEgressPolicy` (golden); non-empty plan replaces the public rule with per-allow rules
  and **preserves** DNS + in-cluster + metadata block; an allow CIDR overlapping `169.254.0.0/16` is
  dropped.
- `builders.AttachAgentNetwork`: Phase 1 no-ops the pod; Phase 2 injects exactly one proxy container
  + SPIFFE volume when `ProxyNeeded()`, and the listener port matches the resource's `localPort`.
- `resolveBoundNetworks` (envtest): selector match/no-match; cross-namespace networks ignored; count
  agrees with `AgentNetworkReconciler.Status.BoundAgents`.

**Integration (envtest):**

- AgentRun with a bound `egress.allow` network → the created NetworkPolicy contains the allow CIDR and
  *not* the `0.0.0.0/0` public rule; unbound AgentRun → unchanged floor.
- AgentNetwork edit re-reconciles a live AgentSession's policy; does not mutate a running AgentRun's
  pod.
- `EbpfNeeded()` plan with no loader → run held `Pending(EbpfLoaderMissing)`.

**e2e (cftest single-node k0s — exists, see MEMORY):**

- Reuse the `cmd/ebpf-probe` proof harness as the Tier-2 assertion: deploy an AgentRun bound to an
  `ebpfBoth` network allowing only one CIDR, exec into the pod, confirm the allowed CIDR connects and
  a disallowed one is dropped at the kernel (the probe already proves `EncodeAllow` works end-to-end,
  `cmd/ebpf-probe/main.go:98-126`).
- Confirm the cftest CNI honors egress NetworkPolicy (Tier 1) by asserting `169.254.169.254` is
  unreachable from a serving pod *after* the floor lands (it is reachable today).

---

## 12. Cross-links

- [`docs/design/agentnetwork-agentpolicy-interaction.md`](../design/agentnetwork-agentpolicy-interaction.md) — the honesty/rationale doc this spec implements (§6.2 is the seam).
- [`docs/features/agentnet.md`](../features/agentnet.md) — AgentNetwork feature/usage (proxy, WireGuard, eBPF).
- [`docs/features/egress-credentials.md`](../features/egress-credentials.md) — secretless credential injection P2a turns on for runs.
- [`docs/research/agent-runtime-fit-analysis-v0.2.0.md`](../research/agent-runtime-fit-analysis-v0.2.0.md) — the runtime-fit report this gap derives from.
- [`docs/design/custom-agent-images.md`](../design/custom-agent-images.md) — long-running/daemon agents need the per-workload egress P1c provides.
- Sibling specs *(this run, under `docs/specs/`)*: `agentpolicy-enforcement` (shared admission webhook), `dynamic-credential-backends` (broker mint), `run-governance`, `agentsession-scaling-impl`.

---

## Live probe finding — 2026-06-03 (cftest)

**The current run egress cage silently blocks the apiserver.** A live probe confirmed that on a k0s cluster the `kubernetes` Service DNATs to the node **host IP `:6443`** (host-network apiserver), so the existing default-deny egress NetworkPolicy (DNS + RFC1918 in-cluster `10/8,172.16/12,192.168/16` + public `{80,443}`) **drops apiserver traffic** — `:6443` is not in the public-allow ports and the node host IP is not in the RFC1918 set. This is invisible until a workload needs the apiserver (A2A child-run creation, any in-pod kube client).

**Design requirement:** `buildEgressPolicy` must, when apiserver access is requested (A2A, or any in-pod kube client), add an egress rule for the `kubernetes` EndpointSlice addresses on their port — resolved at reconcile time (single-node k0s = `<node-ip>/32:6443`; multi-node = each control-plane address). **Validated:** adding `159.69.185.87/32:6443` made the apiserver reachable (`401`). In the same probe the metadata block (`169.254.0.0/16`→`000`), the non-`{80,443}` block (`:8443`→`000`), the public-443 allow, and in-cluster pod-IP reachability (hermes-gateway `401`) all behaved correctly — so the cage shape is right; it just lacks the apiserver-endpoint exception. See [agent-to-agent-invoker](agent-to-agent-invoker.md) and [README §8](README.md).

**Update — apiserver-block is conditional on node IP (AWS Graviton probe, 2026-06-03):** the "cage blocks the apiserver" behavior depends on whether the apiserver endpoint (= the node host IP, for host-network apiservers like k0s) is RFC1918 or public. On the AWS node the endpoint was `172.31.10.119:6443` (private, ∈ `172.16/12`), so the **existing** RFC1918 egress allow already permitted the apiserver and a kata-qemu microVM created a child AgentRun through the cage with no extra rule. On cftest the node IP was **public** (`159.69.185.87`) and the same cage dropped it. **Recommendation stands and sharpens:** when apiserver access is required, `buildEgressPolicy` should add the explicit `kubernetes` EndpointSlice allow **unconditionally** — it's a harmless no-op on private-IP clusters and the necessary fix on public-IP ones, so the policy is correct regardless of where it's deployed.
