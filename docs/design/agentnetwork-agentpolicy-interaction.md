# AgentNetwork + AgentPolicy: What They Guard, and What Is Actually Enforced

> **Status: both CRDs are largely DECLARED-BUT-NOT-ENFORCED on the run datapath as of v0.2.0. Do not assume protection that the code does not provide.**

> Verified against the tree at HEAD (2026-06-02). Every "NOT WIRED / NO CONTROLLER" claim below cites the `file:line` that proves the absence. This document exists because the CRD schemas are rich and aspirational while the runtime honors almost none of them — an operator who reads only the CRD reference will badly over-estimate the platform's containment. Effort/impact estimates reflect adversarial review, not the optimistic first pass.

---

## 1. Executive summary

`AgentNetwork` and `AgentPolicy` are the platform's two declared guardrail CRDs:

- **`AgentNetwork`** is a *per-agent egress* CRD — an identity-aware proxy sidecar (`identityProxy`), a userspace WireGuard mesh (`wireguardMesh`), TraT / secretless-credential injection, and an eBPF allow-list. Its spec is the most fully-realized of the two.
- **`AgentPolicy`** is a *namespace guardrail* CRD — `allowedProviders`, `allowedTools`, `maxBudget`, and a `redaction` pattern set.

The honest state at v0.2.0:

1. **There is exactly one egress control that actually fires on a run pod: a static, agent-independent default-deny `NetworkPolicy`** built in `operator/internal/builders/run_sandbox.go`. It is genuinely created, GC-owned, and blocks the cloud instance-metadata endpoint. That is a real floor and it is the *only* one.
2. **`AgentNetwork` is not wired onto the run/session datapath at all.** The `AgentRun` reconciler and pod builder contain zero `AgentNetwork` references — no proxy sidecar, no eBPF programming, no per-resource egress, no merge of the CRD's allow-list into the static policy.
3. **`AgentPolicy` has no controller whatsoever.** No `agentpolicy_controller.go` exists; no reconciler reads it; `allowedProviders` / `allowedTools` / `maxBudget` / `redaction.patterns` have zero runtime effect. **Redaction is applied nowhere.**
4. **The eBPF egress cage runs only in the e2e probe** (`cmd/ebpf-probe`), never under the operator.

If you are deploying agents that handle untrusted input or sensitive credentials, treat the platform as having a coarse default-deny egress floor and **nothing else** from these two CRDs. The rest is declared surface awaiting the enforcement work designed in §5.

---

## 2. Enforcement reality table (v0.2.0)

Status legend: **ENFORCED** = the code path runs on every run; **NOT WIRED** = the machinery exists but no datapath caller invokes it; **NO CONTROLLER** = nothing reconciles the CRD; **IGNORED** = the field is read by nobody on the datapath; **E2E-ONLY** = exercised exclusively by a probe/test binary.

| Mechanism | Status | Evidence (`file:line`) |
|---|---|---|
| Run-pod default-deny egress `NetworkPolicy` (DNS + in-cluster + public 80/443, metadata blocked) | **ENFORCED** | `operator/internal/builders/run_sandbox.go:60-123`; created via `ensureRunEgressPolicy` at `operator/internal/controllers/agentmodel/agentrun_controller.go:214-216,336-347`, GC-owned via `Owns(&networkingv1.NetworkPolicy{})` at `agentrun_controller.go:112` |
| Run-pod sandbox `RuntimeClass` pin (defense-in-depth, not from these CRDs) | **ENFORCED** | `operator/internal/builders/run_sandbox.go:45-53`; applied at `agentrun_controller.go:189` |
| AgentNetwork `identityProxy` sidecar injected onto a run/session pod | **NOT WIRED** | No `AgentNetwork` reference in `operator/internal/builders/agentrun.go` (whole file) or `operator/internal/controllers/agentmodel/agentrun_controller.go`; the proxy code (`pkg/agentnet/proxy`) is imported only by `cmd/spiffe-probe/secretless.go`, never the operator |
| AgentNetwork `egress.allow` CIDRs honored by the run egress policy | **IGNORED** | `BuildAgentRunEgressPolicy` takes only `*amv1.AgentRun` and never reads any AgentNetwork — `run_sandbox.go:60-63`; its own comment admits "A tighter per-Agent allow-list (AgentNetwork CIDRs) can layer on top later" (`run_sandbox.go:59`) |
| AgentNetwork `egress.enforcement` / eBPF cgroup allow-list on a run | **E2E-ONLY** | The map driver `pkg/agentnet/cgroup` is imported only by `cmd/ebpf-probe/main.go:47`; the operator imports no `agentnet` subpackage (verified: zero non-test matches under `operator/`) |
| AgentNetwork `wireguardMesh` device on a run/session pod | **NOT WIRED** | `pkg/agentnet/wireguard` is imported only by `test/e2e/fullstack/shared/scenarios.go`; no operator datapath caller |
| AgentNetwork TraT / `credential` injection on agent egress | **NOT WIRED** | Same as the proxy row — injection lives in `pkg/agentnet/proxy` (driven by the e2e probe), not rendered by any builder on the run path |
| `AgentNetworkReconciler` binds networks to runs | **NOT WIRED** | The reconciler only validates the spec, resolves the WireGuard secret, counts matching Agents, and sets Status — `operator/internal/controllers/agentmodel/agentnetwork_controller.go:66-129`. (Its doc comment claims otherwise; see §3.) |
| AgentPolicy `allowedProviders` enforcement | **NO CONTROLLER** | No `agentpolicy_controller.go` in `operator/internal/controllers/agentmodel/`; the only `AgentPolicy` reference outside the API package is a comment at `agent_controller.go:2` |
| AgentPolicy `allowedTools` enforcement | **NO CONTROLLER** | Same — no reconciler reads `AgentPolicySpec.AllowedTools` (`pkg/agentmodel/v1/types.go:314`) |
| AgentPolicy `maxBudget` cap | **NO CONTROLLER** | Same — `AgentPolicySpec.MaxBudget` (`types.go:315`) is read by no controller; the run's only budget is the per-Agent `Budget` (`types.go:68`) enforced inside the runtime |
| AgentPolicy `redaction.patterns` applied to output | **NOT APPLIED** | `RedactionPolicy` (`types.go:319-321`) has type + deepcopy only and is applied nowhere; `foldRunResult` copies `rr.Output` verbatim (`agentrun_controller.go:398-415`) with no redaction step |

**Reading the table:** the only row that is **ENFORCED** *and* sourced from these CRDs is the static egress policy — and that row does not actually consult `AgentNetwork` either; it is a hard-coded floor that happens to live in the same problem space. Everything labelled with an `R-AN-*` / `R-AM-API-6` requirement ID in the Go source is, at v0.2.0, a declared API contract without a runtime.

---

## 3. The false `R-AN-PROXY-3` comment (being corrected separately)

The `AgentNetworkReconciler` doc comment asserts a binding that does not exist:

> `// It does NOT inject sidecars itself — the AgentRun reconciler reads bound`
> `// AgentNetworks and renders them via builders.BuildAgentRunPod (R-AN-PROXY-3).`
> — `operator/internal/controllers/agentmodel/agentnetwork_controller.go:25-30`

This claim is **false**. The `AgentRun` reconciler (`agentrun_controller.go`) does *not* read bound `AgentNetwork`s, and `builders.BuildAgentRunPod` (`agentrun.go:20-82`) renders no proxy — it takes `(*amv1.AgentRun, *amv1.Agent)` only and has no `AgentNetwork` parameter or reference. What `AgentNetworkReconciler.Reconcile` actually does, end to end:

1. Validates the spec via `pure.ValidateAgentNetwork` (`agentnetwork_controller.go:75`).
2. For `wireguardMesh`, fetches the `privateKeyRef` Secret to fail fast if missing (`:92-101`).
3. Sets per-kind status counters — `ProxyResourceCount` / `WGPeerCount` (`:81-105`).
4. Counts Agents whose labels match `agentSelector` and writes `Status.BoundAgents` (`:108-119`).
5. Sets `Status.Phase = Ready` (`:121`).

There is no sidecar injection, no pod mutation, no write to any AgentRun, and no handoff to a builder. `Status.BoundAgents` is a *count*, not a binding that anything downstream consumes. The comment is a leftover aspiration and is being corrected in a separate change; it is reproduced here so readers who find the comment first are not misled.

---

## 4. What each CRD is INTENDED to guard

The specs are real and well-shaped — this section describes the *design intent* encoded in the types, none of which is enforced on the run datapath today.

### 4.1 AgentNetwork — per-agent egress

`AgentNetworkSpec` (`pkg/agentmodel/v1/agentnetwork.go:26-42`) is a discriminated union on `kind`:

- **`identityProxy`** (`IdentityProxySpec`, `agentnetwork.go:44-55`): a SPIFFE-aware sidecar that fronts a list of upstream `resources`. Each `ResourceTarget` (`:106-146`) is `tcp` (SPIFFE mTLS byte-forward, `authorize` SVID list required) or `http` (reverse proxy minting a JWT-SVID for `jwtAudience`). On `http` resources it can additionally inject:
  - **`trat`** (`TraTInjection`, `:75-86`) — a Transaction Token (`Txn-Token` header) carrying an RFC 8693 `scope` (the transaction intent), for internal backends.
  - **`credential`** (`CredentialInjection`, `:88-104`) — secretless injection of a broker-minted provider credential (e.g. GitHub), authorized by an internal-only TraT the broker verifies before minting. The agent never sees the value.
  - **`tts`** (`TTSRef`, `:57-73`) — the Tokenetes token-exchange + JWKS endpoints required when any resource uses `trat`/`credential`.
  - The capabilities of the proxy machinery itself are real: `pkg/agentnet/proxy` (`doc.go:1-14`) implements the TCP/HTTP forwarders with SPIFFE auth. It is simply never instantiated by the operator on a run pod.
- **`egress`** (`EgressPolicy`, `:148-168`): the eBPF-driven host policy.
  - `enforcement` ∈ `{none, ebpfRedirect, ebpfAllowList, ebpfBoth}` (default `ebpfBoth`, `:152-154`).
  - `allow []EgressRule` (`:170-180`) — per-`(cidr, ports, proto)` allow-list; everything outside is dropped by the `cgroup_skb/egress` program **when `enforcement` includes `ebpfAllowList`**.
  - `redirectCIDRs` — destinations the `cgroup/connect4` program transparently rewrites to the sidecar.
  - The compiler that turns this into LPM-trie + hash-map entries is real (`pkg/agentnet/cgroup/doc.go:1-11`) but, per §2, is invoked only by `cmd/ebpf-probe`.
- **`wireguardMesh`** (`WireGuardSpec`, `:182-213`): a userspace WireGuard adapter (client/server, peers, `privateKeyRef` from the broker). Real implementation in `pkg/agentnet/wireguard`; no operator datapath caller.

`agentSelector` (`:35`) is intended to pick which Agents get the network injected; today it only feeds the `BoundAgents` *count* (§3).

### 4.2 AgentPolicy — namespace guardrails

`AgentPolicySpec` (`pkg/agentmodel/v1/types.go:312-321`):

| Field | Intended guard |
|---|---|
| `allowedProviders []string` | Restrict which `ModelProvider`s an Agent in the namespace may reference. |
| `allowedTools []string` | Restrict which `Tool`s an Agent may bind. |
| `maxBudget *Budget` | Cap the per-run budget below the Agent's own `Budget`. |
| `redaction.patterns []string` | Regex patterns to scrub from agent output (`RedactionPolicy`, `:319-321`). |

None of these has a reader. The CRD is registered in the scheme (`operator/api/agentmodel/v1/types.go:123-128,137-145`) so `kubectl apply` succeeds and stores the object — which is precisely the trap: the object persists and looks authoritative while changing nothing.

---

## 5. The static egress floor that DOES exist

This is the one real control, and it is worth stating exactly so operators know its shape and its limits. `buildEgressPolicy` (`operator/internal/builders/run_sandbox.go:73-123`) renders a `NetworkPolicy` with `PolicyTypes: [Egress]` and three egress rules:

1. **DNS** — UDP/TCP `53` to anywhere (`run_sandbox.go:104-107`).
2. **In-cluster, any port** — to the RFC1918 ranges treated as pod/service networks: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` (`clusterInternalCIDRs`, `run_sandbox.go:35`; rule at `:108-109`).
3. **Public internet, HTTP(S) only** — TCP `443`/`80` to `0.0.0.0/0` **except** the metadata/link-local range and the in-cluster ranges (`run_sandbox.go:110-119`). The `Except` set is `["169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]` (`publicExcept`, `:87`).

The block that matters most: **`169.254.0.0/16` is never reachable** (`metadataBlockedCIDR`, `run_sandbox.go:37-39`). That covers `169.254.169.254`, the AWS/GCP/Azure instance-metadata endpoint — the canonical SSRF / credential-theft target. A compromised harness cannot reach it.

The same builder backs long-running session pods via `BuildAgentSessionEgressPolicy` (`run_sandbox.go:67-69`).

**Honest limits of the floor:**

- It is **coarse** — any pod may reach *any* public host on 80/443. There is no per-destination allow-list, no L7 inspection, no SNI/host filtering. Exfiltration to an arbitrary HTTPS endpoint is wide open.
- It is **agent-independent** — every run gets the identical policy regardless of its `AgentNetwork`. Two agents with very different egress needs are caged identically.
- It is **enforced by the CNI** — a cluster whose CNI does not honor `NetworkPolicy` (or lacks one entirely) gets *no* egress containment at all. The operator creates the object; the CNI must enforce it.
- It does **not** authenticate egress, inject credentials, or mint TraTs — all of that is the (unwired) `AgentNetwork` job.

---

## 6. Proposed enforcement design (DESIGN)

> Everything in this section is **DESIGN**, not current behavior. It is the home for the enforcement work the CRDs imply.

### 6.1 AgentPolicyReconciler + validating admission

**Goal:** make `AgentPolicy` reject non-conforming Agents/Runs at admission time, and apply redaction on the output fold.

1. **Validating admission webhook** (preferred over a reconciler-only check, since it fails the write rather than letting a bad Agent sit `Ready`):
   - On `Agent` create/update: reject if `spec.model.providerRef` ∉ union of `allowedProviders` across in-namespace `AgentPolicy`s; reject if any `spec.tools[].name` ∉ union of `allowedTools`.
   - On `AgentRun` create: reject if `spec.budgetOverride` exceeds the effective `maxBudget`.
2. **`AgentPolicyReconciler`** for the parts admission cannot do statically:
   - Recompute effective policy when an `AgentPolicy` changes and re-mark dependent Agents (`Status.Phase=Failed`, reason `PolicyViolation`) so existing objects don't silently violate a newly-tightened policy.
3. **Redaction on the fold (DESIGN, with a hard caveat):** apply `redaction.patterns` to `rr.Output` inside `foldRunResult` (`agentrun_controller.go:398-415`) before persisting `run.Status.Output`. **Caveat:** redaction at the fold only scrubs the cluster-facing `Status.Output`; the harness has already seen and could have already exfiltrated the unredacted data over the open 80/443 egress (§5). Redaction is a defense for *what lands in `kubectl`/audit*, not a containment boundary. Document it as such or it will be mis-sold the same way the current comments are.

### 6.2 AgentRun reconciler resolves bound AgentNetworks

**Goal:** make `agentSelector` an actual binding, not a count.

In `AgentRunReconciler.Reconcile`, before `BuildAgentRunPod` (`agentrun_controller.go:186`):

1. List in-namespace `AgentNetwork`s whose `agentSelector` matches the parent Agent's labels (multiple → **AND** the agent must satisfy each, see §7).
2. For each matched `identityProxy` network: render the proxy sidecar (reuse `pkg/agentnet/proxy`) and the SPIFFE volume; wire `localAddr`/`localPort` so the agent reaches upstreams through the sidecar.
3. **Merge `egress.allow` CIDRs into the egress policy** rather than the current static-only floor: extend `buildEgressPolicy` to take an extra peer/port set and intersect it with the public rule so a bound network can *tighten* (never loosen) the default. The existing `run_sandbox.go:59` comment ("can layer on top later") is the intended seam.
4. **Honor `egress.enforcement`:** when it includes `ebpfAllowList`/`ebpfRedirect`, program the cgroup maps via `pkg/agentnet/cgroup` from the operator (today only `cmd/ebpf-probe` does this). This requires the `ebpf-loader` DaemonSet to be present and is a hard prerequisite — fail closed (hold the run `Pending`) if `enforcement` demands eBPF on a node without it, mirroring the existing fail-closed sandbox resolution (`agentrun_controller.go:149-161`).

**Effort:** the proxy-sidecar + CIDR-merge path is **M** (machinery exists; it is plumbing). The operator-side eBPF programming is **L** and node-coupled (loader DaemonSet, pinned-map lifecycle, per-pod cgroup path resolution).

---

## 7. Binding + composition semantics (DESIGN)

These rules are not implemented; they define how the §6 work should compose so the behavior is predictable.

- **AgentNetwork → Agent (`agentSelector`):** an Agent is bound by *every* `AgentNetwork` whose selector it matches. Multiple matches **compose by AND** — the agent gets every matched network's sidecars/devices. Conflicting `localPort`/`localAddr` across matched networks is a validation error surfaced on the Run (`Pending`, reason `NetworkConflict`). An empty `agentSelector` binds nothing (already the documented `R-AN-API-2` semantics, `agentnetwork_controller.go:107`).
- **AgentPolicy → namespace:** policies are namespace-scoped and **compose across all policies in the namespace**:
  - `allowedProviders` / `allowedTools` → **union** (a provider/tool allowed by *any* policy is allowed). Union is the safe default: a stricter intersection would let an unrelated policy silently revoke a working Agent.
  - `maxBudget` → **minimum** (the tightest cap wins).
  - `redaction.patterns` → **union of all patterns** (every pattern from every policy is applied).
- **Egress composition** (when §6.2 lands): the static floor (§5) is the *ceiling* of openness; bound `AgentNetwork.egress.allow` can only **narrow** it. A network can never grant an agent reach the static policy denies (e.g. it can never re-open `169.254.0.0/16`).

### Resolving the namespaced-vs-cluster contradiction

The operator `AgentPolicy` type carries a contradictory doc comment:

> `// AgentPolicy declares cluster- or namespace-wide guards.`  *(types.go:119)*
> `// +kubebuilder:resource:scope=Namespaced,shortName=apol`  *(types.go:122)*
> — `operator/api/agentmodel/v1/types.go:119-123`

The **kubebuilder marker (line 122) is authoritative: `AgentPolicy` is Namespaced.** The CRD generated from this type is namespaced, the (future) reconciler will list per-namespace, and §7's composition rules are namespace-scoped. The "cluster- or" phrasing in the comment is incorrect and should be dropped. There is no cluster-scoped policy mechanism at v0.2.0; if a cluster-wide guard is needed later it is a separate `ClusterAgentPolicy` kind, not this one.

---

## 8. Decision / debug flow

Given a run that you expect to be constrained, here is which control would (or, today, would not) reject it and how to tell:

```
A run pod was created. Why did / didn't it get caged?

1. Egress NetworkPolicy
   $ kubectl get networkpolicy <run-name>-egress -n <ns>
   - Present?  The static default-deny floor IS applied (run_sandbox.go:60).
     - Run still reached 169.254.169.254?  → your CNI does not enforce
       NetworkPolicy. The operator's job ends at creating the object.
     - Run reached an arbitrary public HTTPS host?  → EXPECTED. The floor
       allows all 0.0.0.0/0 on 80/443 (run_sandbox.go:110-119). This is not
       a bug; per-destination filtering is the unwired AgentNetwork job.
   - Absent?  resolveRunSandbox/ensureRunEgressPolicy failed before pod
     create (agentrun_controller.go:214) — check the run's status reason.

2. "I bound an AgentNetwork with egress.allow / a proxy — it had no effect."
   - EXPECTED at v0.2.0. AgentNetwork is NOT wired onto runs (§2,§3). Confirm:
     $ kubectl get agentnetwork <name> -n <ns> -o jsonpath='{.status.boundAgents}'
     A non-zero count is ONLY a count — it injects nothing. The proxy/eBPF
     machinery runs only under cmd/ebpf-probe / cmd/spiffe-probe, not the
     operator.

3. "I created an AgentPolicy (allowedProviders / maxBudget / redaction) —
    the run ignored it."
   - EXPECTED at v0.2.0. AgentPolicy has NO controller (§2). The apply
     succeeds and stores the object, but nothing reads it. A run using a
     disallowed provider, an over-budget override, or emitting data that
     matches a redaction pattern will NOT be rejected or scrubbed.

4. "The run was budget-capped." 
   - That came from the per-Agent spec.budget (runtime-enforced), NOT from
     AgentPolicy.maxBudget. The TerminationReason on the run
     (status.terminationReason, e.g. "budget:tokens") tells you which cap
     fired (foldRunResult, agentrun_controller.go:398-415).
```

The single most important takeaway for debugging: **if you expected `AgentNetwork` or `AgentPolicy` to constrain a run and it didn't, that is the documented v0.2.0 behavior, not a misconfiguration.** Only the static egress `NetworkPolicy` is live.

---

## 9. Cross-links

- [`docs/features/agentnet.md`](../features/agentnet.md) — the AgentNetwork feature/usage doc (proxy, WireGuard, eBPF).
- [`docs/features/egress-credentials.md`](../features/egress-credentials.md) — secretless egress credential injection (TraT-authorized broker mint) the `credential` field targets.
- [`docs/research/agent-runtime-fit-analysis-v0.2.0.md`](../research/agent-runtime-fit-analysis-v0.2.0.md) — the runtime-fit report this honesty pass derives from.
- `docs/design/custom-agent-images.md` *(planned — not yet written)* — long-running / daemon agents need per-workload egress, which is exactly the §6.2 AgentNetwork-on-runs work; the two designs should land together.
