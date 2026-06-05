# Implementation Task List — finishing M1–M5

> **Source of truth:** the 17 specs in this directory + the canonical
> [decisions.md](../design/decisions.md) (authoritative where a spec still says
> OPEN/PROPOSED). This is the execution companion to [README §4](README.md) — README §4
> is the milestone *overview*; this is the per-task *breakdown*. Generated 2026-06-03 by
> decomposing each milestone's specs against the resolved decisions.
>
> **~107 active tasks, ~199 planning points** (S=1 M=2 L=3 XL=5). Effort is a rough
> weight, not an estimate. Post-GA items (D6) are parked at the end, not numbered as active.

## How to read

Each task is `- [ ] **M<milestone>.<n> — title** · `<effort>`` with:
- **Build** — what to implement (concrete packages / files / types / funcs).
- **Accept** — the verifiable bar (a test, a `kubectl` observation, an exit criterion).
- **Deps** — prerequisite task IDs (`M1.4`), decisions (`D3`), or `none`.
- **Risk** — only when the spec flags one.

Check a box when its **Accept** bar is green. A milestone ships when all its boxes are checked **and** its exit criteria hold.

## Cross-cutting invariants (decide once, honored in every task)

These recurred across every milestone — get them wrong once and many tasks regress:

1. **CRD generation drift.** Every CRD-touching task **hand-edits** the YAML under `operator/config/crd/`; do **not** blind-run `make manifests` (the tree isn't reproducible from Go source). Verify each with `kubectl apply --dry-run=client`. Deepcopy *is* generated (`make -C operator deepcopy`).
2. **Cost = integer milli-USD, observability-only.** Surfaced in `Status` for humans; **never** a budget/enforcement axis.
3. **No gate/oracle ever reads `usage.toolCalls`.** It is structurally 0 on the harness path; prove tool execution via side-effects (nonce, access log), never a count.
4. **Field-wise usage roll-up, never `Usage.Add`.** `Usage.Add` phantom-increments `Steps`; child-run / per-turn roll-up sums `Tokens`/`ToolCalls` field-wise and **excludes** `WallClock` + `Steps`.
5. **Resume-key ownership is decided once in M2.** The `Request.SessionID` / `ResumeSessionID` shape is owned by response-richness / agentsession-scaling; M3 claude-code + codex *consume* it — don't re-invent per-harness.
6. **`spec.session{required,interactive}` (D4)** is the explicit session gate (built M4.3). `interactive ⇒ required ⇒ non-knative`. M3's session-ness references it — coordinate the field early.
7. **Per-namespace NATS ACLs (D1)** are a governance floor item (M1) and enforced on the session datapath (M2.20). The ACL *shape* is undesigned — see M1 open items.
8. **Permission/sandbox danger flags (D3)** (`--dangerously-skip-permissions`, `danger-full-access`, `--ask-for-approval never`) are **opt-in-only** and **admission-refused unless the resolved RuntimeClass is a microVM**. `HarnessCLISpec.ExtraFlags` **already exists and is wired** — build the typed mappings + the gate, not the flag mechanism.
9. **Redaction is disclosure-control, not containment.** Built + mandatory in M1, but the harness already saw raw data and can exfil over the egress floor — never documented as DLP.
10. **kata enforced in prod (D3).** Prod must not run `--allow-host-runtime`; cftest's runc is a dev-only exception. Serving path already defaults kata-fc; the run path gains placement in M1.

## Critical path & sequencing

```
M1 (governance floor, mostly self-contained)
   └─> M2 (capability wire; needs M1 egress floor so tool tokens aren't usable)
          └─> M3 (agents + A2A; needs M1 datapath + M2 invokers/wire/resume-key)
                 └─> M4 (interactive; needs M1 egress+broker, M2 sessions+artifacts, M3 per-kind CLI seam)
M5 (near-term pre-run gate) — INDEPENDENT: D6 severed its form-B deps; ship any time after its own CRD plumbing.
```

- **M1 is the gate for everything** — it makes the proven runtime fail-closed and multi-tenant. Start here.
- **The turn-model/runtime split (M4.1)** is tagged M4 but is a *foundation*; pulling it forward (right after M1) de-risks M2's session work and M3's resume-key plumbing. Recommended early.
- **M5 near-term** (the pre-run approval gate) has no executor/runtime change and no cross-spec dependency — a good parallel track for a second pair of hands.

## Effort roll-up

| Milestone | Active tasks | S | M | L | XL | ~pts |
|---|---:|---:|---:|---:|---:|---:|
| M1 — Containment + Governance floor | 23 | 10 | 11 | 2 | 0 | 38 |
| M2 — Capability wire | 28 | 12 | 13 | 3 | 0 | 47 |
| M3 — Agent composition + per-agent | 25 | 6 | 14 | 5 | 0 | 49 |
| M4 — Interactive + daemons | 24 | 4 | 11 | 7 | 2 | 57 |
| M5 — Human-in-the-loop (near-term) | 7 | 6 | 1 | 0 | 0 | 8 |
| **Total** | **107** | **38** | **50** | **17** | **2** | **199** |

---

# M1 — Containment + Governance floor

**Goal:** Make the control plane around the proven kata-fc/default-deny runtime real and fail-closed for multi-tenant: per-namespace AgentPolicy (allow-lists + budget caps + mandatory redaction), AgentNetwork allow-lists on the datapath + a default-ON serving egress floor, run governance (kata placement + hard deadlines + per-tenant caps + admission queue + node autoscaling), and the operator-granted DynamicCredentialBackend producer surface. Mandatory P0 (D1).

**Decision deltas:** governance is mandatory not optional (D1); redaction is **built** in M1 (D1); webhook `failurePolicy: Fail` overrides the spec's recommended `Ignore` (D3); serving egress floor is **default-ON** not opt-in (D3); kata placement fail-closed `PlacementFallback=Pending` (D3); run-governance ships **caps + fairness queue + node autoscaling**, not just soft caps (D10); credentials become an operator-granted CRD (D8); per-namespace NATS ACLs required (D1); cost = milli-USD obs-only; the apiserver EndpointSlice egress allow is probe-validated (cftest + AWS Graviton).

### Tasks

- [ ] **M1.1 — Pure AgentPolicy compositor + checks** · `M`
  - *Build:* `pkg/agentmodel/v1/policy.go`: `EffectivePolicy`, `ComposePolicies([]AgentPolicy)` (union allow-lists, per-axis `minBudget`, dedupe patterns, `Empty` iff nothing contributed), `AllowsProvider`/`AllowsTool`/`CapBudget`. Resolve `maxToolCalls=0` ambiguity (pointer-gate; 0 = unset).
  - *Accept:* `policy_test.go`: zero policies → all-allow; union; empty-slice contributes nothing; per-axis min with unset-skip; `CapBudget` returns offending axis; equal-to-cap allowed.
  - *Deps:* D1, D3; foundation `AgentPolicySpec`/`Budget`.
- [ ] **M1.2 — Pure redaction engine** · `M`
  - *Build:* `pkg/agentmodel/v1/redact.go`: `RedactJSON` (mask string values only — never keys/numbers/bools; whole-blob mask non-JSON), `RedactSteps` (mask `Step.Error`, `ToolCallRecord.Arguments/Result/Error`), `const RedactionMask`.
  - *Accept:* `redact_test.go`: masks string values, leaves numbers/keys, re-parses valid; empty-patterns = identity; property test "no substring matches any pattern".
  - *Deps:* none (Go `regexp` is RE2, no backtracking-DoS).
  - *Risk:* disclosure control, NOT containment — the harness already saw raw data (spec R1); never sell as DLP.
- [ ] **M1.3 — Regex compile-check in `ValidateAgentPolicy`** · `S`
  - *Build:* edit `validation.go:178-186` — `regexp.Compile` each redaction pattern, field-error on failure so a bad pattern is rejected at admission, never panics the fold.
  - *Accept:* `validation_test.go`: rejects `"("`; accepts valid; `MaxBudget` path unchanged.
  - *Deps:* none.
- [ ] **M1.4 — Apply redaction on the run-result fold** · `S`
  - *Build:* `agentrun_controller.go`: thread `ctx` into `foldRunResult`; `compileRedaction(ctx, ns)` (list policies → compose → compile, skip+log non-compiling); apply `RedactJSON`/`RedactSteps` to `Status.Output`/`.Steps`. Leave `TerminationReason` unredacted.
  - *Accept:* envtest: secret-shaped `Output` + a `Step` with a secret in `ToolCallRecord.Result` + matching policy → masked, valid JSON; no-policy → byte-identical (zero-overhead).
  - *Deps:* M1.1, M1.2.
- [ ] **M1.5 — Reconcile-time AgentPolicy gate + policy watch** · `M`
  - *Build:* `agent_controller.go`: after tool resolution, compose policies; on a disallowed resolved provider/tool → `setStatus(Failed, PolicyViolation)`. `toPurePolicies` helper. `Watches(&AgentPolicy{}, …agentsInNamespace)` so a tightened policy re-evaluates dependents.
  - *Accept:* envtest: `allowedProviders:[openai]` + anthropic Agent → `Failed/PolicyViolation`; conforming → `Ready`; tighten-after-Ready re-enqueues → `Failed`; symmetric tool case.
  - *Deps:* M1.1.
- [ ] **M1.6 — AgentPolicy admission webhook (Agent + AgentRun), failurePolicy=Fail** · `M`
  - *Build:* `operator/internal/webhooks/agentpolicy_gate_webhook.go`: validators on Agent (`field.Forbidden` on disallowed provider/tools) + AgentRun (resolve effective budget, `CapBudget`). Fail-open only on transient list error / `Empty`. Register in `main.go`. `ValidatingWebhookConfiguration` **`failurePolicy: Fail`** + `timeoutSeconds: 5` (D3).
  - *Accept:* webhook envtest: forbidden provider → `IsInvalid`; no policies → nil; API error → error (no silent admit); budget over/under cap. cftest: `kubectl apply` exits non-zero with Forbidden.
  - *Deps:* M1.1, M1.5, D3.
  - *Risk:* `Fail` on a single-replica operator blocks writes if the webhook is down — mitigated by the M1.5 reconcile backstop + short timeout + HA roadmap (overrides spec's `Ignore`).
- [ ] **M1.7 — AgentPolicy self-validation webhook + namespaced doc/CRD fix** · `S`
  - *Build:* (a) optional `CustomValidator` on AgentPolicy → `ValidateAgentPolicy`; (b) drop "cluster- or" from `types.go:119` doc; (c) hand-edit the agentpolicies CRD removing the "NOT enforced yet" warning, document compose semantics.
  - *Accept:* invalid regexp rejected at apply; CRD no longer claims "NOT enforced"; build + vet green.
  - *Deps:* M1.3, M1.4/5/6 landed (so "enforced" is truthful).
- [ ] **M1.8 — Class-string placement resolver core** · `S`
  - *Build:* `features/placement.go`: extract `ResolvePlacementForClass(ctx, reader, rc)` (default kata-fc; non-KVM/nil → `(nil,false)`; match `AgentNodePool.Spec.Isolation`, lowest-name-wins). Make `ResolvePlacement` a wrapper. Check import direction acyclic.
  - *Accept:* existing `placement_test.go` unchanged; new table-tests: match-by-isolation, lowest-name determinism, gvisor/runc/nil → `(nil,false)`.
  - *Deps:* none.
- [ ] **M1.9 — Run/session placement + deadline builders** · `S`
  - *Build:* `builders/run_governance.go`: `ApplyRunPodPlacement(pod, *NodePlacement)` (nodeAffinity + toleration + do-not-disrupt; no-op nil), `ApplyRunDeadline(pod, maxWallClockSeconds, multiplier)` (`ceil(W×mult)`, default 1.5, min 1).
  - *Accept:* golden: placement sets affinity/toleration/annotation, idempotent; deadline `30×1.5=45`, ceil, no-op ≤0.
  - *Deps:* M1.8.
- [ ] **M1.10 — Wire placement + deadline into AgentRun reconciler (kata fail-closed)** · `M`
  - *Build:* `agentrun_controller.go`: `RunDeadlineMultiplier` field; after `resolveRunSandbox` call `ResolvePlacementForClass`; if `nil && RequiresKVM && fallback!=Schedule` → `markPending(NoKVMCapacity)` requeue 30s; else `ApplyRunPodPlacement` + override-aware `ApplyRunDeadline`. Add `AgentRunSpec.PlacementFallback` (`Enum=Pending;Schedule`, default Pending).
  - *Accept:* fake-client: held Pending on kata+no-pool+default; created on `Schedule`; deadline from override else budget. cftest: kata run schedules + folds with placement labels + deadline; overrun → `Failed/DeadlineExceeded`.
  - *Deps:* M1.8, M1.9, D3.
- [ ] **M1.11 — Wire placement + session resources into AgentSession reconciler** · `S`
  - *Build:* `agentsession_controller.go`: `ResolvePlacementForClass`; Pending on kata+no-pool; `ApplyRunPodPlacement`; apply `session.Spec.Resources` to container; **no deadline** (idle-timeout bounds it). Add `AgentSessionSpec.Resources` (resolve the pure-package `core/v1` import question — see open items).
  - *Accept:* worker gets Resources + no `activeDeadlineSeconds`; placement propagates through the verbatim pod-spec copy.
  - *Deps:* M1.8, M1.9; resolve `core/v1`-in-pure question.
- [ ] **M1.12 — AgentRunQuota CRD + per-Agent/per-namespace concurrency gate (Layer 1)** · `M`
  - *Build:* pure `runquota.go` + wrapper: `AgentRunQuotaSpec{MaxConcurrentRuns, MaxPriority}`, status, `shortName=arq`. `AgentSpec.MaxConcurrentRuns`. In the pod-absent branch: `admitRun` + `countLiveRuns` (label `agents.smol-agents.ai/run`, drop terminal, bucket by Agent + total); over cap → `markPending(ConcurrencyLimited)` requeue 10s; **fail-open on count error**. `DefaultNamespaceRunConcurrency` flag + `resolveNamespaceCap`. RBAC for agentrunquotas.
  - *Accept:* under cap → admit; at per-Agent / namespace cap → `ConcurrencyLimited`; count-error → admit. Envtest: quota=2 + 3 runs → 2 Running + 1 Pending; complete one → third admits.
  - *Deps:* D1, D10.
  - *Risk:* soft/eventually-consistent — boundary overshoot bounded by `MaxConcurrentReconciles`.
- [ ] **M1.13 — Per-namespace fairness/priority admission queue (Layer 2)** · `L`
  - *Build:* `agentrun_controller.go`: `EnableAdmissionQueue` + per-namespace in-memory priority queues (mutex; `priority desc, creationTimestamp asc`); admit head up to free capacity; `isQueueHead` gate. `AgentRunSpec.Priority` (clamped to `MaxPriority`). Rebuild from Pending on leader failover.
  - *Accept:* higher-priority Pending admitted first; ties by creation; priority clamped; queue rebuilds after simulated leader change.
  - *Deps:* M1.12, D10.
- [ ] **M1.14 — Run-path node autoscaling for kata runs** · `M`
  - *Build:* ensure kata pods that hold `Pending/NoKVMCapacity` drive `AgentNodePool` (Karpenter) provisioning — verify do-not-disrupt + isolation toleration + nodeAffinity interplay with the `AgentNodePool` reconciler so kata runs trigger metal scale-up.
  - *Accept:* envtest/integration: a kata run + matching pool produces a pod whose placement the provisioner selects on. cftest single-pool caveat: assert placement + do-not-disrupt present.
  - *Deps:* M1.9, M1.10, D10.
  - *Risk:* real multi-pool autoscaling is only provable on the L2 ring, not the single-node box.
- [ ] **M1.15 — Pure NetworkPlan compositor** · `S`
  - *Build:* `pkg/agentnet/plan/plan.go`: `NetworkPlan` + `BuildNetworkPlan([]AgentNetworkSpec)` (AND-compose: union allow/redirect, concat proxy resources, first/unique TTS, strongest enforcement, error on cross-network localPort/localAddr/TTS conflict) + `ProxyNeeded`/`EbpfNeeded`/`Empty`.
  - *Accept:* `plan_test.go`: empty/single/multi compose; localPort conflict → error; conflicting TTS → error; strongest-enforcement; metadata-CIDR rejection.
  - *Deps:* none.
- [ ] **M1.16 — Egress-policy merge + network resolver + run/session wiring (Tier 1)** · `M`
  - *Build:* `agentnetwork_bind.go`: `resolveBoundNetworks` (match `agentSelector ⊆ labels` → `BuildNetworkPlan`). `builders/agentnetwork.go`: `AttachAgentNetwork` (Phase-1 no-op) + `BuildEgressPolicyWithPlan` (keep DNS + RFC1918; **replace** public `0.0.0.0/0:{80,443}` with per-allow rules when present; drop allow CIDRs overlapping `169.254/16`). Make existing builders thin wrappers; wire into run + session reconcilers; conflict → `markPending(NetworkConflict)`; `Watches(&AgentNetwork{})`.
  - *Accept:* golden: empty plan byte-identical to today; non-empty replaces public rule, keeps floor; metadata-overlap dropped. Envtest: bound run has allow CIDR, no `0.0.0.0/0`; edit re-reconciles a live session, not a running run pod.
  - *Deps:* M1.15.
- [ ] **M1.17 — Default-ON SmolAgent serving-pod egress floor** · `S`
  - *Build:* SmolAgent reconciler: alongside the workload, create a default-deny egress NetworkPolicy via `BuildSmolAgentEgressPolicy(cr, plan)` reusing `BuildEgressPolicyWithPlan`. **Default-ON** (D3).
  - *Accept:* envtest: serving pod gets the policy by default (no flag). cftest: `169.254.169.254` unreachable after the floor; a bound allow opens exactly one extra host.
  - *Deps:* M1.16, D3.
  - *Risk:* behavior change — served agents on non-80/443 break under the floor (D3 accepts; documented, no opt-in grace). Tier-1 strength depends on the CNI honoring egress policy — confirm on cftest.
- [ ] **M1.18 — Probe-validated apiserver EndpointSlice egress allow** · `S`
  - *Build:* `buildEgressPolicy`/`BuildEgressPolicyWithPlan`: add an allow for the `kubernetes` EndpointSlice address(es)+port (single-node k0s `<node-ip>/32:6443`; multi-node each control-plane), resolved at reconcile. Recommended **unconditional** (no-op on private-IP clusters; required on public-node-IP like cftest/Hetzner).
  - *Accept:* rendered policy includes the endpoint allow; cftest regression: run pod reaches apiserver (`401`) while metadata + non-80/443 stay blocked.
  - *Deps:* M1.16; live probe (2026-06-03).
- [ ] **M1.19 — AgentNetwork validation hardening + run/session network status** · `S`
  - *Build:* `ValidateAgentNetwork`: reject any `egress.allow[].cidr` subnet of `169.254.0.0/16`. Add `Status.Networks []string` + `EgressEnforcement string` to AgentRun/AgentSession.
  - *Accept:* `169.254.x/y` allow fails admission; status fields populate on a bound run.
  - *Deps:* M1.16, D3.
- [ ] **M1.20 — DynamicCredentialBackend CRD types + validation** · `M`
  - *Build:* pure `dynamiccredentialbackend.go`: `DynamicCredentialBackendSpec{CredentialName, Provider(githubApp), GitHubApp, MaxLeaseTTL, Grants}`, `GitHubAppBackendSpec`, `CredentialGrantSpec`, status, `ValidateDynamicCredentialBackend`. K8s wrapper, **Namespaced** (recommend `platform-secrets` ns, RBAC-locked), registered. Deepcopy + hand-verified CRD + RBAC.
  - *Accept:* validation rejects empty credentialName / unknown provider / missing block / unparseable grant principal / empty scope. Deepcopy + CRD verified.
  - *Deps:* D8, D1.
- [ ] **M1.21 — DynamicCredentialBackend controller (validate + readiness)** · `M`
  - *Build:* `dynamiccredentialbackend_controller.go` (model on AgentNetworkReconciler): validate, resolve `privateKeyRef` (fail-fast `Pending: SecretMissing`), set status, `Watches(&Secret{})` to flip Pending→Ready. Validates + reports readiness only; does NOT inject pods. Grants operator-owned, RBAC-locked (D8).
  - *Accept:* envtest: missing key → `Pending/SecretMissing`; create Secret → `Ready` via watch; `grantCount` reflects spec.
  - *Deps:* M1.20, D8.
- [ ] **M1.22 — secret-proxy dynamic config dialect + peerAuth=spire guard** · `M`
  - *Build:* `cmd/secret-proxy/main.go`: independent `backend.dynamic` block + `tts{}` + `credentialPolicy[]`; `buildDynamic` (load PEM → `GitHubAppBackend`, `JWKSVerifier`, `StaticCredentialPolicy`). **Refuse to start if `backend.dynamic` set but `peerAuth != spire`** (sender-constraint binds TraT to the SVID).
  - *Accept:* table-test: dynamic block builds the backend/verifier/policy; **negative:** dynamic + `peerAuth:local` → startup error. In-process: a Server from generated config + `Client.Mint` with a TraT returns a token.
  - *Deps:* M1.20, D8; `pkg/secrets` library unchanged.
- [ ] **M1.23 — Operator dynamic-broker config + mount builder (producer)** · `L`
  - *Build:* `builders/dynamic_broker.go`: `BuildDynamicBrokerConfigSecret` (AppID + JWKS URL, **never the private key**); `AttachDynamicBroker(pod, backendRef, privateKeyRef)` adding SPIRE CSI socket + config Secret + root-key volume mounted **only** into the broker container + the SPIRE-backed secret-proxy sidecar.
  - *Accept:* config Secret has no key bytes; key mounted only into the broker (not the agent) + SPIRE CSI socket + `peerAuth: spire`; agent-blindness asserted.
  - *Deps:* M1.22, D8.
  - *Risk:* end-to-end mint is **deferred to M2's proxy+SPIRE-broker injection** — producer here, consumer there.

**Exit criteria:** AgentPolicy denies a disallowed provider at admission (`Fail`) + the reconcile backstop flips non-conforming Agents, and a folded `Status.Output` is redacted on cftest; an AgentNetwork allow-list opens exactly one extra host (CNI-honored) and the serving egress floor is on by default (metadata unreachable); a kata run gets affinity+toleration+do-not-disrupt + a deadline that hard-kills an overrun; per-Agent + per-namespace caps hold and the priority queue admits by priority; a DynamicCredentialBackend validates + reaches Ready + mints in-process; per-namespace NATS ACLs enforced.

**Open / unverified:** NATS per-namespace ACL *shape* is undesigned (no spec body covers it); `core/v1` in the pure package (blocks M1.11's exact field type); whether the "required" eBPF Tier-2 must land *within* M1 or co-land with M2's proxy injection; `AgentRunQuota` CRD long-term home (standalone vs folded into AgentPolicy); the dynamic-cred datapath consumer is out of M1 (no end-to-end mint on a real cluster until M2).

---

# M2 — Capability wire: tools, observability, files, replay

**Goal:** Animate loop-mode tool execution (HTTP + Streamable-HTTP MCP), make every run's record rich + size-budgeted (real tokens, milli-USD cost, tool-call trace, overflow ref), scale durable sessions for mid-scale + retention, capture files out to S3 with a manifest, and land best-effort seed + an `agent eval` runner — replay engine parked post-GA.

**Decision deltas:** replay engine **deferred post-GA** (D6) — ship best-effort seed + N-sample distributions only; MCP = Streamable-HTTP per-agent + stdio via operator cluster allow-list (D7/D11), hand-rolled client; cost = milli-USD obs-only + no `usage.toolCalls` gating; session scaling ships concurrency + retention for mid-scale (D10); per-namespace NATS ACLs (D1); artifact prefixes per-tenant-scoped, collected in the sidecar (D1); fail-closed admission (D3).

### Tasks

- [ ] **M2.1 — Widen Response/Usage/ToolCallRecord contract + rewrite richness doc** · `S`
  - *Build:* `harness/iface.go`: add `Response.CostUSD`, rewrite the "always zero" contract to "best-effort". `budget.go`: `Usage.CostUSD` (obs-only, not a budget axis). `types.go`: `ToolCallRecord.{ArgsBytes,ResultBytes}`. Update `harness-authoring.md` §4.
  - *Accept:* `go build` both modules; contract text no longer asserts always-zero; deepcopy untouched for scalars.
  - *Deps:* none.
- [ ] **M2.2 — `RunResult.Trace`/`TraceSummary` + CRD `status.trace` + fold (milli-USD)** · `S`
  - *Build:* `runonce.go`: `RunResult.Trace *TraceSummary` + the type; `ResultToWire` computes counts pre-clamp. `types.go`: `RunStatus.Trace`. Hand-edit agentruns CRD (`status.usage.costUSD` integer milli-USD + `status.trace`). `foldRunResult` sets it. `make -C operator deepcopy`.
  - *Accept:* deepcopy diff touches only RunStatus/RunResult; `kubectl explain agentrun.status.trace`; CRD dry-run applies; fold test maps cost + trace.
  - *Deps:* M2.1; CRD drift.
- [ ] **M2.3 — Information-preserving termination-message clamp** · `S`
  - *Build:* `cmd/agent/run.go` `clampForTerminationMessage`/`elideStepPayloads`: stamp `ArgsBytes`/`ResultBytes` before niling; when steps dropped set `Trace.Truncated`+`DroppedBytes` and KEEP `Trace`. Preserve shed order; 3072B budget unchanged.
  - *Accept:* unit: N steps × large bodies → ≤3072B AND exact counts, bytes populated, `Truncated`/`DroppedBytes` set; small result → not truncated, steps intact.
  - *Deps:* M2.2.
- [ ] **M2.4 — Refactor `runCLI` to per-kind Response construction** · `S`
  - *Build:* `harness/cli.go`: `runCLI` returns raw bytes + duration; each `Run` builds its own Response (keep bounded `capWriter` + ctx + stderr). No behavior change yet.
  - *Accept:* existing cli tests pass; `runCLI` no longer builds Response; generic-cli still raw stdout + zero tokens.
  - *Deps:* M2.1.
- [ ] **M2.5 — claude-code structured-output parser + `OutputFormat`** · `M`
  - *Build:* `harness/parse_claude.go`: `parseClaudeJSON` (`--output-format json` envelope → Anthropic-named tokens, `total_cost_usd`→CostUSD, text→Output; stream-json `tool_use`/`tool_result`→ToolCallRecord). `HarnessCLISpec.OutputFormat` enum + CRD. Malformed → raw + zeros, never panic.
  - *Accept:* golden JSON → tokens+cost+output; stream-json → records; malformed → raw+zeros; `text` → raw. Cost as milli-USD downstream.
  - *Deps:* M2.4, M2.1.
  - *Risk:* claude JSON schema drifts weekly — ignore unknown fields, pin to the bundle image.
- [ ] **M2.6 — codex JSONL parser** · `M`
  - *Build:* `harness/parse_codex.go`: `parseCodexJSONL` (usage event→Usage, exec/tool→ToolCallRecord, cost best-effort). `CodexHarness.Run` appends `--json` + parses; tolerate partial last line.
  - *Accept:* usage→Usage; exec/tool→ToolCalls; partial line tolerated; malformed → raw+zeros.
  - *Deps:* M2.5 (shared seam).
- [ ] **M2.7 — Hermes Responses parser + dual-shape `parseUsage`** · `M`
  - *Build:* `harness/hermes.go`: `parseResponsesOutput` (walk `output[]`, pair calls/outputs); widen `parseUsage` to read BOTH chat + responses token shapes with explicit precedence (non-zero wins; a 0 never zeroes the other) + `costUSD`. Set ToolCalls/CostUSD when `API=responses`.
  - *Accept:* chat-only, responses-only, both-present → correct; interleaved message + call/output → paired records.
  - *Deps:* M2.1.
  - *Risk:* mis-parse silently zeroing the token budget is the top correctness risk — zero on ambiguity.
- [ ] **M2.8 — Fold harness richness into Step/Usage; confirm post-hoc cap sees real numbers** · `S`
  - *Build:* verify/extend `executor.go` fold: `Response{CostUSD,ToolCalls}`→`Usage`; post-hoc token cap now has real CLI tokens; confirm `CostUSD` does NOT enter `AllowsStep`.
  - *Accept:* unit: Response → Usage.CostUSD + ToolCalls=len + carried Step.ToolCalls; assert no path feeds cost/toolCalls into a budget/gate decision.
  - *Deps:* M2.1, M2.5, M2.6, M2.7.
- [ ] **M2.9 — Overflow trace store to AgentFS/S3 with `Trace.OverflowRef`** · `M`
  - *Build:* `cmd/agent/run.go` clamp path: when detail drops and an overflow sink is configured, `pkg/agentfs.S3.Put` the full RunResult JSON to `runs/<ns>/<run>/trace.json` (per-tenant), set `OverflowRef`. No-op without creds. SSE from bucket config; ref is a pointer.
  - *Accept:* `FakeS3`: `Put` with tenant-scoped key + ref set; no creds → no Put. E2E: >4 KiB trace → Status keeps usage/trace, `truncated`, detail recoverable via ref.
  - *Deps:* M2.3.
  - *Risk:* overflow keys must inherit SSE, never world-readable; tool bodies unredacted (residual).
- [ ] **M2.10 — Ship `tools.json` into the run ConfigMap + resolve tool specs** · `S`
  - *Build:* `builders/runspec.go`: `runSpecToolsFile`, marshal `[]Tool` into the ConfigMap (~1 MiB guard → `ToolSpecTooLarge`). `agentrun_controller.go`: `resolveAgentTools` (honor `ref.Namespace`), thread into `ensureRunSpec`; missing → `Pending/ToolMissing`.
  - *Accept:* unit: ConfigMap has `tools.json`; absent when none; oversized → error. envtest: loop run renders it.
  - *Deps:* none.
- [ ] **M2.11 — Real JSON-Schema validator for tool args/results** · `S`
  - *Build:* `pkg/agentruntime/schema.go`: `ValidateAgainstSchema` (compiled `santhosh-tekuri/jsonschema`). Swap executor input/output calls from `v1.MatchesSchema`; keep `MatchesSchema` as the admission-time shape-check.
  - *Accept:* `schema_test.go`: `required:["q"]` rejects `{}`, accepts `{"q":"x"}`; executor records `StepToolCallRejected`.
  - *Deps:* none.
- [ ] **M2.12 — `invokers/` package + `HTTPInvoker` + registry, broker-leased** · `M`
  - *Build:* `pkg/agentruntime/invokers/{iface,http,registry,http_test}.go`. `HTTPInvoker` (POST args, headers, `applyAuth` leases `Auth.SecretName`→Bearer, ~256KiB cap, return raw Observation — no pre-validate). `registry.Default`. Import rule: `invokers`→runtime, never reverse. Wire in `cmd/agent/run.go`.
  - *Accept:* httptest: body==args; headers; Bearer from fake leaser; non-2xx/non-JSON → error; cap; assert no `outputSchema` pre-validate.
  - *Deps:* M2.10, M2.11.
- [ ] **M2.13 — Populate executor `Tools`/`Invokers` via `RunConfig`; lease tool Auth** · `M`
  - *Build:* `runonce.go`: `ToolsSpecFile`, `LoadTools(dir)`, `RunConfig{Tools,Invokers}` threaded through `RunOnce`/`RunTurn` (replace the empty-map NOTE). `secrets.go`: extend `gatherRunSecrets` with a tools loop leasing `HTTP.Auth`/`MCP.Auth` by `SecretName`.
  - *Accept:* unit: tool Auth collected by name; nil skipped; missing → error. envtest: http-tool run attaches the broker even with no provider secret. executor: full plan→toolcall→observation→trace; `StepToolCallRejected` on no-invoker.
  - *Deps:* M2.10, M2.12.
- [ ] **M2.14 — `MCPInvoker` — Streamable-HTTP per-agent (hand-rolled JSON-RPC)** · `L`
  - *Build:* `invokers/mcp.go`: require `http(s)` (reject non-http loudly); `initialize`→`notifications/initialized`→`tools/call`; headers (`MCP-Protocol-Version`, `Accept: …event-stream`, `MCP-Session-Id` echo, Bearer); decode from JSON body OR terminal SSE event; map `CallToolResult`→Observation (prefer `structuredContent`). Re-init per call (stateless).
  - *Accept:* httptest speaking Streamable HTTP: four headers + session-id echo + Bearer; params correct; decodes JSON AND SSE-terminal; JSON-RPC error → Go error; non-http rejected.
  - *Deps:* M2.13.
  - *Risk:* content→single-value mapping lossy; `Tool.Name` must equal the server tool name (`RemoteName` deferred).
- [ ] **M2.15 — Operator cluster allow-list type/gate for stdio MCP (D7/D11)** · `S`
  - *Build:* allow-list config type + admission/reconciler check: an `mcp` tool requesting a stdio server must resolve to an approved image; non-allow-listed stdio → reject fail-closed. (In-pod launcher is a thin follow-up.)
  - *Accept:* unit: non-allow-listed stdio rejected; allow-listed passes; HTTP MCP unaffected. Documented that arbitrary tenant stdio is denied.
  - *Deps:* M2.14, D7, D11.
  - *Risk:* launcher itself deferred; this lands the policy seam only.
- [ ] **M2.16 — Fail-closed admission guard for unwired loop-tool kinds** · `S`
  - *Build:* `validation.go`: `SupportedLoopToolKinds()` (`{HTTP,MCP}`). Enforce only for `mode:loop`: (1) webhook rejects unsupported kind (`Fail`, D3); (2) reconciler `Failed/ToolKindUnsupported`. Don't false-positive harness-mode inert refs.
  - *Accept:* unit: loop+`kind:agent` rejected; same in harness admitted. E2E: `kubectl apply` loop+`kind:agent` → rejection/Failed.
  - *Deps:* none, D3.
- [ ] **M2.17 — AgentSession scaling: fields, accessors, CRD, deepcopy (behavior-preserving)** · `M`
  - *Build:* `types.go`: add `AgentSessionSpec` knobs (`MaxConcurrentTurns`, `TurnBatchSize`, `TurnPollIntervalMs`, `TurnDeliveryTimeoutSeconds`, `TurnRetentionSeconds`, `MaxTurnInputBytes`, `TurnHistoryLimit`, defaults=today) + `Resources`; status (`Usage`, `Turns`, `FailedTurns`, `LastTurnTime`). `agentsession_defaults.go` accessors (0→today). Pure `ResourceRequirements{Limits,Requests map[string]string}`. Hand-edit CRD + `make deepcopy`.
  - *Accept:* `0→default`, in-range passthrough; deepcopy round-trips Resources+LastTurnTime; CRD applies; `maxConcurrentTurns` clamped to 1 (no semaphore yet).
  - *Deps:* none; CRD drift.
- [ ] **M2.18 — AgentSession concurrency core: semaphore + mutex + per-turn deadline + compaction** · `L`
  - *Build:* `session_worker.go`: `MaxConcurrentTurns`/`TurnTimeout`/`HistoryLimit`/`mu`. Replace serial `processTurns` with a width-N semaphore + wg; `handleTurn` runs `runTurn` OUTSIDE the lock, takes `mu` only for phase/index/Append/compact; `turnCtx = min(TurnTimeout, budget)`; `compact` drops oldest beyond limit. `session.go`: `TotalTurns`/`FailedTurns`. `serve_session.go` flags; controller renders them.
  - *Accept:* `-race`: width-1 identical to today; width-N folds each turn once, no duplicate Index, `TotalTurns==#turns`; compact correctness; `turnCtx`=min; resume continues with correct `TotalTurns`.
  - *Deps:* M2.17.
  - *Risk:* concurrency correctness is load-bearing — `-race` mandatory; FIFO lost under concurrency (documented; default 1 keeps serial).
- [ ] **M2.19 — AgentSession status roll-up (field-wise, NOT `Usage.Add`)** · `M`
  - *Build:* mirror `CumulativeUsage` verbatim into `status.usage`; `status.turns=TotalTurns` (monotonic), `failedTurns`, `lastTurnTime`. Worker writes `status-summary.json`; controller reads it; bump Running requeue to ~30s. No new worker RBAC.
  - *Accept:* `status.usage==CumulativeUsage` (NOT via `Usage.Add`); `turns` monotonic across compaction. E2E: 8 turns@4 → `turns==8`, usage==sum.
  - *Deps:* M2.17.
- [ ] **M2.20 — Gateway per-session limits + NATS retention + per-namespace ACLs** · `L`
  - *Build:* `sessionqueue`: `NewNATSQueue` gains `MaxAge` + `UpdateRetention` (interface + NATS `UpdateStream`, Mem no-op). `cmd/agentgateway`: add a k8s client (RBAC get/list agentsessions), per-session `maxInputBytes` lookup (TTL cache, `min(perSession,10MiB)`); gateway reconciles retention (`max(RetentionSec())`). **Per-namespace NATS ACLs (D1)** isolate subjects per tenant.
  - *Accept:* `UpdateRetention`→`UpdateStream`; Mem no-op. E2E: 4096 cap → 5KiB `400`, 2KiB ok; two sessions → MaxAge=max; cross-namespace subject denied.
  - *Deps:* M2.17, D1.
  - *Risk:* gateway→apiserver coupling (TTL-cache-bounded); cluster-wide retention max is coarse (ok v1).
- [ ] **M2.21 — AgentSession `spec.resources` worker override** · `S`
  - *Build:* `builders/agentrun.go`: `ApplyResources(pod, *ResourceRequirements)` (`resource.MustParse`); `agentsession_controller.go` applies after `BuildAgentRunPod`.
  - *Accept:* pure→corev1 correct; nil→defaults. envtest: override lands on the worker container.
  - *Deps:* M2.17.
- [ ] **M2.22 — AgentSession admission webhook (agentRef + cross-field)** · `M`
  - *Build:* `agentsession_webhook.go`: `agentRef` exists in-ns (kills 15s Pending limbo); `turnDeliveryTimeoutSeconds ≤ turnRetentionSeconds`. Register + webhooks.yaml (`failurePolicy: Fail`, D3). Reconciler `NotFound→Pending` stays as belt-and-suspenders.
  - *Accept:* unit: dangling ref → reject; valid → admit; timeout>retention → reject. E2E: apply with bad ref → immediate error.
  - *Deps:* M2.17, D3.
- [ ] **M2.23 — Artifact egress: types, validation, CRD** · `S`
  - *Build:* `storage.go`: `ArtifactSpec`/`ArtifactRule`. `types.go`: `AgentSpec.Artifacts`, `ArtifactRef`, `RunStatus.Artifacts`+`ArtifactsState` (never affects `State`). `ValidateAgent`: artifacts ⇒ agentfs + S3 target, unique names, relative globs (no `..`). Hand-edit both CRDs + `make deepcopy`.
  - *Accept:* unit: no-agentfs → error; no-target → error; dup names → error; `..` → error. CRD applies.
  - *Deps:* none; CRD drift.
- [ ] **M2.24 — `CollectArtifacts` collector in `pkg/agentfs`** · `M`
  - *Build:* `agentfs/artifacts.go`: `ArtifactManifest` + `CollectArtifacts(ctx, workspace, rules, dest)` — doublestar glob (reject `..`), `MaxBytes` over-budget → `Skipped`, stream via `TeeReader`→sha256→`S3.Put`, record size + real version id. Per-file error → `Skipped`+`Partial`; total outage → `Failed` (exit 0). Key = per-tenant `prefix/ns/run/relpath` (D1); sorted lexically.
  - *Accept:* in-memory S3: exact match → one ref (sha256/size/tenant key); glob → ordered refs; over-budget → Skipped+Partial; one Put error → Partial; outage → Failed no panic; unversioned → `""`; traversal rejected; ContentType sniff/override.
  - *Deps:* M2.23, D1.
- [ ] **M2.25 — Sidecar wires artifact collection into shutdown** · `M`
  - *Build:* `cmd/agentfs-sidecar/main.go`: after final SIGTERM backup (RPO preserved), if configured `CollectArtifacts` on a fresh bg ctx bounded by `AGENTFS_ARTIFACT_TIMEOUT` (60s), `writeManifest` to stdout + `<run>-artifacts` ConfigMap. Parse `AGENTFS_ARTIFACTS*` + downward-API `POD_*`.
  - *Accept:* collects + logs manifest end-to-end; backup before collection; bounded. Integration: manifest to stdout + ConfigMap.
  - *Deps:* M2.24.
- [ ] **M2.26 — Operator artifact wiring: env, scoped RBAC, fold** · `M`
  - *Build:* `builders/storage_mount.go`: `agentFSArtifactEnv` on the **serve sidecar only** (harness container gets ZERO AWS creds); `artifact_rbac.go`: Role scoped to `configmaps` on `<run>-artifacts`. `agentrun_controller.go`: ensure RBAC, `ArtifactsState=Pending` on create, `foldArtifacts` reads the manifest CM (NotFound on terminal → Pending+requeue→`Failed`). **Never touch `State`.**
  - *Accept:* env on serve only; `POD_*` present; dest creds via `secretKeyRef`; harness has no AWS env. RBAC scoped to one CM. Controller: present → set; absent terminal → Pending; Failed run still folds.
  - *Deps:* M2.23, M2.25.
  - *Risk:* sidecar gains one tightly-scoped CM authority (only trust widening); 120s grace contention; artifacts unredacted.
- [ ] **M2.27 — Seed determinism: regression test + honesty doc (best-effort)** · `S`
  - *Build:* Increment A only. `hermes_test.go` + `openaillm/client_test.go`: `Seed=N`→`seed:N` in body, `Seed=0` omits. Tighten the seed doc to "best-effort; bit-exact NOT guaranteed". Add a "Determinism" subsection to a tenant doc.
  - *Accept:* tests green; doc states seed is a hint, exact reproduction is post-GA.
  - *Deps:* none, D6.
- [ ] **M2.28 — `agent eval` subcommand (live / N-sample; no replay)** · `M`
  - *Build:* `cmd/agent/eval.go`: walk a `--suite` of cases, run each via `RunOnce` (genuine datapath) **live**, `--samples N` reports the distribution. Compare vs `expected.json`: phase always, output exact when present, opt-in `outputContains`. **Comparator must NOT read `usage.toolCalls`.** Non-zero exit on FAIL; `--format json`. Not wired into `make test` yet.
  - *Accept:* 2-case testdata (PASS + CHANGED) → exit 1 + json classification; assert no toolCalls comparison; `--samples N` aggregates a distribution.
  - *Deps:* M2.27, D10.
  - *Risk:* live mode is non-deterministic — N-sample distribution is the near-term substitute; don't gate CI day one.

**Exit criteria:** a loop agent with an HTTP tool + a Streamable-HTTP MCP tool completes a turn against a fake MCP with schema-validated args, the token broker-leased (never in Status/ConfigMap/env), folded into `status.steps`; an unwired kind is rejected fail-closed at apply; a non-allow-listed stdio MCP rejected. A run reports real CLI tokens + milli-USD cost (no gate reads it); a >4 KiB trace survives the clamp with detail recoverable via a tenant-scoped overflow ref. A durable session sums usage field-wise under `-race`, honors `maxConcurrentTurns>1`, enforces input caps + retention + per-namespace ACLs, rejects a dangling agentRef; an un-annotated session is bit-for-bit unchanged. An artifacts-declared run uploads to per-tenant S3 keys + folds refs via the sidecar (harness has zero AWS creds), `artifactsState` never affects Phase. Seed is pinned + honestly documented; `agent eval` runs `RunOnce` live with N-sample reporting and never keys on tool-call counts. **Replay is NOT delivered (post-GA).**

**Open / unverified:** stdio MCP launcher depth (M2.15 lands the gate, not the in-pod transport); Hermes `API=responses` selector lives in M3 (no live producer until then); CLI `OutputFormat` per-kind default unratified; artifact manifest channel (ConfigMap chosen vs termination-message fallback); CRD drift on every CRD-touching task.

### Deferred to post-GA (do NOT build now)
- **M2.X1 — Record/replay harness engine** `[POST-GA]` — `recording.go`/`replay.go`, `kind=replay` enum + validation + CRD, registry record-wrap. The decorator seam exists; revisit post-GA (D6).
- **M2.X2 — `agent eval --mode replay`** `[POST-GA]` — replay-registry + fixture-miss-as-error + record mode; depends on M2.X1.
- **M2.X3 — Per-turn artifact capture for sessions** `[POST-GA]` — turn-boundary hook + per-turn key namespace.
- **M2.X4 — ToolCalls round-trip through fixtures** `[POST-GA]` — blocked by M2.X1 + the M3 producers; fixtures capture `toolCalls` as `[]` until then.

---

# M3 — Agent composition + per-agent full support

**Goal:** With the M2 wire + tools live, deliver synchronous A2A delegation and "full support" for the pure-batch agents (Hermes, Claude Code, Codex), folding the per-harness richness parsers and per-kind CLI permission seams into each, inside the kata-fc + default-deny envelope.

**Decision deltas:** A2A "highest-risk unverified" is **resolved** (cftest + AWS-kata probes; the explicit kubernetes-EndpointSlice egress allow is now required); namespaced A2A RBAC is mandatory (D1); claude/codex danger flags opt-in-only + microVM-gated + never default (D3) — `ExtraFlags` already exists, build the typed mappings + the gate; Hermes cross-turn memory = provider-session via a stable session id (D6); codex requires the gateway to speak the OpenAI Responses API; `HermesGateway` CRD stays roadmap (URL-only now); claude resumable batch lands now, resident/loop-resume post-GA (D6); `spec.session` (D4) is the gate; cost = milli-USD obs-only.

### Tasks

- [ ] **M3.1 — Thread `RunClient` + `RunIdentity` into the run-once seam** · `M`
  - *Build:* `pkg/agentruntime/invoker_agent.go`: `RunClient` (CreateRun/GetRun), `ChildRunRequest`, `RunSnapshot`, `RunIdentity`; extend `RunOnceWithClient`/`RunTurn` to accept `tools`, `rc RunClient`, `self RunIdentity`; register `Invokers[ToolAgent]` only when `rc != nil`.
  - *Accept:* compiles; `Invokers[ToolAgent]` set iff non-nil rc; loop-tools still load from `tools.json`; nil + non-nil tested.
  - *Deps:* M2 (loop-invokers seam closed).
- [ ] **M3.2 — In-pod kube client + downward-API self-identity** · `L`
  - *Build:* `cmd/agent/run.go`: `InClusterConfig` + `client.New`; build `RunIdentity` from env (downward `POD_*` + literal `AGENT_RUN_*`/`AGENT_A2A_DEPTH`); `k8sRunClient` hard-scoped to `self.Namespace`. Operator: `BuildAgentRunPod` injects the downward-API + parent-identity env.
  - *Accept:* envtest: container has `POD_NAMESPACE` fieldRef + `AGENT_RUN_UID==run.UID`; pod can Get its own ns + reach apiserver (no invoker yet).
  - *Deps:* M3.1.
- [ ] **M3.3 — `AgentRunInvoker` core + budget roll-up** · `M`
  - *Build:* `Invoke` (depth guard → `min(parent-remaining, toolCap)` child budget → CreateRun with inherited SessionRef + OwnerUID + depth+1 → poll GetRun until terminal → Observation). `Observation.Usage`; `Usage.AddTokensTools`; executor roll-up adds child Tokens/ToolCalls **field-wise** (NOT `Usage.Add`), excluding WallClock+Steps.
  - *Accept:* fake RunClient (Pending→Running→Completed): correct child Input/SessionRef/OwnerUID/budget; poll terminates; parent Tokens/ToolCalls grow, WallClock/Steps don't double-count.
  - *Deps:* M3.1, M3.2.
- [ ] **M3.4 — A2A cancellation + child-failure semantics** · `S`
  - *Build:* ctx.Done → best-effort delete child (fresh ctx) + return `ctx.Err()`; child Failed/Expired → tool error (executor records `StepToolCall` w/ error, loop continues, not a parent failure).
  - *Accept:* unit: cancel mid-poll deletes + returns err; Failed snapshot → error + loop continues; OwnerReference GC on parent delete.
  - *Deps:* M3.3.
- [ ] **M3.5 — A2A depth/cycle bounding + `AgentTargetSpec` CRD fields** · `S`
  - *Build:* `AgentTargetSpec.MaxTokens`/`TimeoutSeconds` + validation ≥0; `--a2a-max-depth` flag (default 4); depth-label chain + env; per-call timeout from `TimeoutSeconds` or parent-remaining; hand-edit tools CRD.
  - *Accept:* `Depth>=MaxDepth` → error observation, no CreateRun; negative fields rejected; depth+1 stamped; A→B→A terminates via depth+budget.
  - *Deps:* M3.3, D1; CRD drift.
- [ ] **M3.6 — A2A namespaced RBAC builder + operator grant** · `M`
  - *Build:* `AgentA2ARole`/`RoleBinding` (namespaced: agentruns create/get/list/watch + status; bound to `AgentSAName`); gate on "declares a `kind:agent` tool?"; controller-ref'd. Hand-edit operator role.yaml for escalate/bind. Default: omit `delete`, rely on owner-GC.
  - *Accept:* envtest: agent-tool Agent gets the Role/RB (agentruns+status only); non-A2A Agent gets ZERO authority. Live: own-ns `201`, cross-ns `403`.
  - *Deps:* M3.3, D1; RBAC hand-edit.
- [ ] **M3.7 — A2A apiserver/kube-dns egress allow invariant** · `M`
  - *Build:* when an Agent is A2A-capable (from `Status.ResolvedTools`), the egress builder allows the `kubernetes` EndpointSlice addr+port (and, under eBPF enforcement, apiserver ClusterIP + kube-dns). Fixes the host-network-apiserver gap.
  - *Accept:* cftest: A2A child-spawn Create/Get succeeds under the cage (validated: `<node-ip>/32:6443` → `401`). Added as an acceptance of the M1 datapath; M3 asserts it.
  - *Deps:* M1 (owns the datapath), M3.6.
  - *Risk:* eBPF auto-allow-list is unimplemented on the run datapath; A2A works under the static policy meanwhile.
- [ ] **M3.8 — A2A live e2e on cftest** · `M`
  - *Build:* two Agents (parent loop w/ z.ai declaring a `kind:agent` tool → child deterministic transform); depth-2 tree.
  - *Accept:* child run appears w/ parent label + OwnerRef, own kata-fc pod, Completed; parent Output embeds child result; parent Usage.Tokens includes child (excl. wall-clock); parent Steps shows the agent ToolCall; delete parent GCs child.
  - *Deps:* M3.3–M3.7.
- [ ] **M3.9 — Hermes `HarnessHTTPSpec.API` discriminator + endpoint resolution** · `S`
  - *Build:* `API` enum (`chat;responses;runs`, default chat) + `Stream` + `PollIntervalMs`; `resolveEndpoint(base, api)` (path append + back-compat passthrough); `ValidateHarness` rejects `API!="" && Kind!=Hermes`; CRD; split `Run` into `runChat`/`runResponses`/`runAsync`.
  - *Accept:* back-compat: `/v1/chat/completions`+`API=""` byte-identical to today; `API` on non-hermes rejected.
  - *Deps:* M2 (richness wire); CRD drift.
- [ ] **M3.10 — Hermes `/v1/responses` parser + dual-usage + capabilities gate** · `L`
  - *Build:* `parseResponsesOutput` (concat message text, pair function_call/output by call_id); generalize `parseUsage` (both token shapes, no cross-zeroing); `probeCapabilities` (`GET /v1/capabilities`, cached, post-auth) gating `API=responses` fail-loud; doc that `Response.ToolCalls` are the gateway's internal calls (audit, not StepToolCalls).
  - *Accept:* fixture → concat output + paired record; **dual-usage table test** (no cross-zeroing) — non-negotiable; `responses_api:false` → fail-loud, no request; chat/"" → zero capability calls.
  - *Deps:* M3.9, M2.
- [ ] **M3.11 — Hermes `/v1/runs` async + stop-on-cancel (orphan fix)** · `M`
  - *Build:* `hermes_runs.go`: `runAsync` submit→poll; on ANY ctx-done fire `POST /v1/runs/{id}/stop` on a **fresh** 3s ctx; prefer SSE events when `Stream`; post-hoc tool-call cap in executor (gateway runs its own loop → post-hoc verdict).
  - *Accept:* happy path; cancel mid-poll fires stop on a live ctx; `status:failed` → error; `ToolCalls>MaxToolCalls`→`Expired/budget:toolcalls`. E2E: kill pod mid-flight → gateway run cancelled.
  - *Deps:* M3.9, M3.10.
- [ ] **M3.12 — Hermes AgentSession-driven stable session id (cross-turn memory, D6)** · `M`
  - *Build:* controller injection: when `SessionRef!="" && Kind==Hermes`, deep-copy spec, `SessionPolicy=persistent`, `HERMES_SESSION_ID="sess-"+AgentSession.UID` (**UID**, immutable) + optional `HERMES_SESSION_KEY=MemoryScope`. `AgentSessionSpec.MemoryScope`. Rewrite the sample to use `sessionRef`.
  - *Accept:* unit: `sessionRef` → copy has persistent + `sess-<UID>`, original untouched; no ref → no mutation. E2E: two runs sharing a ref → second sees memory; a third with a different ref doesn't.
  - *Deps:* M2 (agentsession-scaling), D6, D4.
- [ ] **M3.13 — Hermes admission relax + `reasoning_effort` honesty** · `S`
  - *Build:* relax `persistent ⇒ spec.storage` for Hermes (gateway-side memory; CLI still requires storage); doc that deleting a session doesn't purge gateway memory; rewrite the `reasoning_effort` comment (server-side config knob, not a working `BODY_` example).
  - *Accept:* unit: Hermes+persistent+no-storage passes; claude+persistent+no-storage still fails.
  - *Deps:* none.
- [ ] **M3.14 — Shared per-kind CLI permission/flag seam (typed mappings around existing `ExtraFlags`)** · `M`
  - *Build:* `HarnessCLISpec` extensions: `OutputFormat`, `ApprovalMode` (`safe;acceptEdits;never`), `AllowedTools`/`DisallowedTools`; REMOVE dead `PassthroughEnv`. `ExtraFlags` already exists/wired — do NOT rebuild; confirm `runCLI` appends it after curated args. CRDs.
  - *Accept:* unit: enum rejections; `PassthroughEnv` removed (grep clean); `ExtraFlags` still appends verbatim.
  - *Deps:* M2, D3.
- [ ] **M3.15 — MicroVM-gated admission for danger permission/sandbox flags (D3 gate)** · `M`
  - *Build:* when the resolved RuntimeClass is NOT a microVM, **refuse** `codex.sandbox=danger-full-access`, `codex.approval∈{untrusted,on-request}`, `approvalMode=never`/`--dangerously-skip-permissions` (fail-closed, mirror `resolveSandbox`). Opt-in, never default. Wire into the admission path that resolves sandbox class.
  - *Accept:* envtest: runc + `danger-full-access` → rejected/downgraded; kata-fc → permitted. Codex C5 negative e2e.
  - *Deps:* M3.14, D3.
- [ ] **M3.16 — Claude Code: `buildClaudeArgs` + JSON parser + richness** · `L`
  - *Build:* `harness/claude.go`: `buildClaudeArgs` (`--print`, `--output-format json` default, `--append-system-prompt` for instructions, model, mcp-config, permission flags, `--resume/--continue`, then ExtraFlags, then prompt); `parseClaudeResultJSON` → Output (result text only), tokens, cache tokens, `total_cost_usd`→`CostUSDMilli`(×1000), `session_id`→SessionID, `num_turns`, `is_error`→error+TerminationReason. `Usage.CostUSDMilli` folded.
  - *Accept:* fake claude: golden argv per mode/format/resume/MCP/ExtraFlags; instructions via `--append-system-prompt`; fields parse; `is_error`→error; malformed → zeros no panic. E2E R1: real run reports tokens + cost + session id.
  - *Deps:* M2, M3.14.
- [ ] **M3.17 — Claude Code: permission posture mapping (microVM-gated)** · `M`
  - *Build:* `approvalMode`→flags (default/safe → `--permission-mode dontAsk`; acceptEdits → that; never → `--dangerously-skip-permissions`); `AllowedTools`/`DisallowedTools`→`--allowedTools`/`--disallowedTools`. Default safe (D3); `never` opt-in + M3.15-gated.
  - *Accept:* golden argv: default emits dontAsk; never emits skip-permissions + rejected on non-microVM. E2E R2: `never` writes a file in AgentFS (no hang); safe constrained.
  - *Deps:* M3.16, M3.15, D3.
- [ ] **M3.18 — Claude Code: MCP server passthrough** · `L`
  - *Build:* `MCPServerSpec{Name,Transport,Command,URL,Env}` + `MCPConfigInline` (mutually exclusive); `builders/mcp_config.go` renders Claude MCP JSON to a ConfigMap, MCP secrets via broker into env (never inline); harness appends `--mcp-config` + auto-allows `mcp__<server>__*`; validation (transport enum, internal-host URL rejected). stdio only from the cluster allow-list (D7/D11).
  - *Accept:* unit: renders JSON; secretRef → env placeholder; stdio vs http/sse; internal-host rejected; both set rejected. E2E R3: in-pod stdio MCP reachable; remote at a non-allowed host blocked, allowed once added.
  - *Deps:* M3.16, M1 (egress), D7/D11.
  - *Risk:* remote MCP over 443 works today only by the public-443 allowance; tightens with M1.
- [ ] **M3.19 — Claude Code: resumable sessions (`--resume` + HOME on AgentFS)** · `L`
  - *Build:* `runspec.go` overrides `HOME=<mount>/.claude-home` when persistent+AgentFS; capture `session_id`→SessionID; `Request.ResumeSessionID` threaded by `RunTurn`/session worker; `--resume <id>` when stored, `--continue` only first-turn-no-id, ephemeral omits. Persist via `AgentSessionStatus.HarnessSessionID`. UUIDv4.
  - *Accept:* E2E R4: turn 1 sets HarnessSessionID; turn 2 `--resume` answers a question needing turn-1 context (proves conversation, not just files). Serialize turns.
  - *Deps:* M3.16, M2 (durable worker + resume-key), D4, D6.
- [ ] **M3.20 — Claude Code: `apiKeyHelper` short-lived creds** · `M`
  - *Build:* render a broker-backed helper script + `--settings '{"apiKeyHelper":…}'` when opted in; Claude re-invokes on TTL/401; static `ANTHROPIC_API_KEY` stays default.
  - *Accept:* unit: helper + settings only when opted in. E2E R5: a run longer than a short broker lease succeeds (re-fetch).
  - *Deps:* M3.16, M1 (broker).
  - *Risk:* known TTL-cache bug (claude-code#11639) — verify on the live box.
- [ ] **M3.21 — Codex: `HarnessCodexSpec` + non-interactive argv + `config.toml`** · `M`
  - *Build:* `HarnessCodexSpec{Model,BaseURL,Sandbox,Approval,NetworkAccess}`; rewrite `CodexHarness.Run` argv (`codex exec --json --sandbox <default danger-full-access> --ask-for-approval <default never> --skip-git-repo-check --output-last-message <tmp> -C <wd> [exec resume]` + extraArgs); `runspec_codex.go` renders `~/.codex/config.toml` (`[model_providers.platform]` + `wire_api="responses"` only when BaseURL set) into the ConfigMap; `/agent` copies to writable `/tmp/.codex` + `CODEX_HOME`; pin `CODEX_VERSION`. Defaults gated by M3.15.
  - *Accept:* argv golden (default flags + resume + `-C`); TOML golden (`wire_api`, env_key name only). Output improves via last-message file pre-parse.
  - *Deps:* M3.14, M3.15; CRD drift.
- [ ] **M3.22 — Codex: `parseCodexEvents` JSONL parser** · `M`
  - *Build:* `codex.go`: `parseCodexEvents` (peek `Type`; sum usage across turns; ToolCallRecord only for actionable items — command/file_change/MCP/web_search, NOT plan/reasoning/agent_message; Output prefers last-message file → last agent_message → raw; **never errors** — drift → baseline). Bump codex `MaxOutputBytes`.
  - *Accept:* against a **captured real `--json` fixture**: tokens summed; only actionable items → records; Output from file; truncated → baseline no error. No executor change (fields already folded).
  - *Deps:* M3.21.
  - *Risk:* `--json` schema not frozen (openai/codex#14736) — pin field paths against the bundled version.
- [ ] **M3.23 — Codex: resumable session** · `S`
  - *Build:* consume shared `Request.SessionID`; emit `codex exec resume <id>` when persistent + id present; thread the thread id from the session worker.
  - *Accept:* unit: resume subcommand under SessionID+persistent. E2E: two turns share a thread; second references first's context.
  - *Deps:* M3.21, M2 (resume-key), D6.
- [ ] **M3.24 — Codex: gateway speaks OpenAI Responses API (deployment/verification, D5)** · `S`
  - *Build:* verification (not Go): confirm the gateway at `codex.baseURL` serves `wire_api=responses`; document the per-cluster check; if the raw upstream doesn't, route Codex through the Responses-capable hermes-gateway or accept direct OpenAI.
  - *Accept:* documented confirmation the deployment's baseURL answers `/v1/responses` (in-cluster hermes-gateway exposes it; raw upstream is per-deployment). Gates M3.25.
  - *Deps:* decisions baked, codex §10 D5.
  - *Risk:* whether the raw upstream speaks Responses directly is a per-deployment check.
- [ ] **M3.25 — Codex: live e2e + sandbox-collision regression on cftest** · `M`
  - *Build:* publish pinned multiarch harness-codex; run a real `kind=codex` AgentRun against the Responses endpoint.
  - *Accept:* Status: Completed, non-empty Output, Tokens>0, ≥1 ToolCalls; no bubblewrap attempt under restricted PSA (confirms danger-full-access takes effect); C5 negative (runc + danger → rejected).
  - *Deps:* M3.21, M3.22, M3.24, M3.15; multiarch publish.

**Exit criteria:** a depth-2 A2A tree completes with correct child-usage roll-up (field-wise, excl. wall-clock + phantom Steps); each child a first-class kata-fc run with its own cage/broker/SPIFFE; parent delete GCs the subtree; A2A authority namespaced (`201`/`403`) + the apiserver allow honored. Hermes surfaces a tool-call trace from `/v1/responses`, parses dual-shape usage without zeroing, fails loud without `responses_api`, stops orphaned runs on cancel, carries memory via a session-UID id. claude-code + codex each complete a batch coding run reporting real tokens + milli-USD cost with MCP/extra-flags honored; danger flags opt-in + admission-refused on non-microVM; claude resumes conversation via `--resume`, codex via `exec resume`. The shared CLI seam lands once; dead `PassthroughEnv` removed. Codex verified against a Responses gateway. Richness parsers land here per the co-evolution edge.

**Open / unverified:** codex `--json` schema drift; raw upstream Responses support (per-deployment); claude `apiKeyHelper` TTL bug; eBPF auto-allow-list unimplemented (A2A runs under static policy); `HermesGateway` CRD deferred; resume-key ownership decided in M2, consumed here.

---

# M4 — Interactive: terminal exposure + long-running daemons

**Goal:** Make agents human-attachable and persistently resident: the turn-model/runtime seam, the `spec.session{required,interactive}` field, a ttyd loopback terminal fronted by `cmd/agentterminal` with a bundled OIDC IdP + `AttachGrant` driver-mode authz, plus first-class `pi-mono` (HTTP via in-pod `pi-bridge`) and `openclaw` (WebSocket daemon) on the hardened serving path (warm, no Knative scale-to-zero).

**Decision deltas:** attach is **driver-mode in v1** (D5); human identity is now decided — **bundled self-hosted OIDC (Dex/Keycloak)**, a real new infra task (D9); sessions/attach gated by `spec.session{required,interactive}` (D4); `pi`→`inflection-pi` + `pi-mono` is the CLI; the turn-model/runtime split (`pkg/turnmodel`, `TurnExecutor`) underpins it; `AttachGrant` = CRD policy record + short-TTL signed token; serving egress floor default-ON (D1/D3); kata enforced (no weaker posture for OpenClaw); HITL mid-run continuation that terminal §8-P4 leans on is **deferred** (D6).

### Tasks

- [ ] **M4.1 — Extract `TurnExecutor` + create `pkg/turnmodel`** · `L`
  - *Build:* new `pkg/turnmodel/` (sibling to `pkg/agentruntime`): `TurnExecutor interface { Execute(ctx, Turn)→(Result, error) }`, `Turn`, `Result`(=RunResult). Make `RunTurn` the reference implementation. Move `session.go`/`session_worker.go`/`session_queue_source.go` into turnmodel; worker holds a `TurnExecutor`. Runtime exports **only** `TurnExecutor`.
  - *Accept:* build/vet/test green both modules, behavior-preserving; worker test passes against a fake `TurnExecutor`; dependency direction turnmodel→runtime only.
  - *Deps:* D2. **Recommended to pull forward right after M1.**
- [ ] **M4.2 — Cross-turn memory as an explicit Turn-Model policy** · `M`
  - *Build:* `TurnMemory{ProviderSessionID, PriorOutput, History}` + a strategy selector: Hermes → provider-session, CLI → workspace-only (empty Turn, AgentFS persists), loop → history-replay stub (flagged needs-resume). Wire into `Turn` construction.
  - *Accept:* each runtime gets its correct shape; Hermes carries a stable id across N turns; CLI carries empty Memory; loop stub feature-flagged (D6).
  - *Deps:* M4.1, D6.
- [ ] **M4.3 — Add the `spec.session{required,interactive}` Agent CRD field** · `M`
  - *Build:* `AgentSpec.Session *SessionSpec{Required, Interactive}` (D4), validated + `kubectl explain`-able; `make deepcopy` + CRD. Webhook arm: `interactive ⇒ required` + `deploymentKind∈{deployment,statefulset}` (reject knative); `required` rejects scale-to-zero.
  - *Accept:* `kubectl explain agent.spec.session`; round-trips; rejects interactive+knative and interactive-without-required.
  - *Deps:* M4.1, D3, D4; CRD gen.
- [ ] **M4.4 — Resident-pod lifecycle for "requires a session"** · `L`
  - *Build:* wire `spec.session.required` to a resident pod (no `RestartPolicy=Never`), reusing the StatefulSet warm posture + state PVC; connect to the Turn-Model worker so turns flow into the resident pod; honor `IdleTimeoutSeconds`.
  - *Accept:* `required:true` schedules a warm pod surviving a turn; idle-park honored; one-shot path unchanged.
  - *Deps:* M4.1, M4.3, M2 (agentsession-scaling).
- [ ] **M4.5 — Bundle a self-hosted OIDC IdP (Dex or Keycloak)** · `L`
  - *Build:* `deploy/oidc/` bundling **Dex** (recommended; Keycloak the heavier alt) as the human-identity rail (D9), multiarch; issuer URL + a static-user connector + an OIDC client for `cmd/agentterminal`; document trust-domain alignment vs SPIFFE.
  - *Accept:* Dex deploys on cftest, serves discovery, issues a token for a static test user; config consumable by M4.10. Mark `UNVERIFIED` if cftest rollout can't be confirmed.
  - *Deps:* D9.
  - *Risk:* new infra the platform never had; Dex-vs-Keycloak is a maintainer call (default Dex).
- [ ] **M4.6 — `AttachGrant` CRD + signed-token mint** · `M`
  - *Build:* namespaced `AttachGrant{agentRef, subject, role:viewer|driver, expiresAt}` (durable record) + a signed short-TTL bearer minter with `aud=spiffe://…/<ns>/<name>/terminal` (no cross-agent replay). RBAC gates grant creation (no cross-tenant driver grant).
  - *Accept:* CRD round-trips; a minted token is audience-bound (replay at another agent fails a unit test); RBAC denies cross-tenant driver-grant.
  - *Deps:* M4.3, D5, D8.
- [ ] **M4.7 — `SmolAgent.spec.features.terminal` block + webhook guards** · `S`
  - *Build:* `TerminalFeature` (Enabled, Web, SSH, Multiplex, Record, ReadOnlyDefault, IdleTimeoutSeconds) added to `Features`; deepcopy + CRD. Webhook: `terminal.ssh ⇒ non-knative`; `terminal.enabled ⇒ default-deny egress posture` (D3) + warn/deny scale-to-zero.
  - *Accept:* `kubectl explain …features.terminal` shows defaults; rejects ssh+knative and enabled-without-egress; unit tests both.
  - *Deps:* M4.3, D3; CRD gen.
- [ ] **M4.8 — ttyd loopback web-terminal sidecar + tmux bootstrap** · `L`
  - *Build:* `builders/terminal.go`: `terminalSidecar` (ttyd, restricted PSA like the secret-proxy, args `-i 127.0.0.1 -p 7681 -O --auth-header X-Smol-Attach -W tmux -S /tmp/tmux/agent.sock attach -t main`); `WireTerminal(pod, cr)` appends sidecar + shared tmux emptyDir + port, gated on `Features.Terminal.Enabled`. tmux bootstrap runs the agent *inside* tmux (persist + multi-viewer). Driver = writable ttyd; viewer = a second read-only ttyd (no `-W`).
  - *Accept:* `terminal_test.go`: drop-ALL/RO-root/non-root, `-i 127.0.0.1`, `-O`, no plaintext creds, shared emptyDir, port 7681; absent when disabled. ttyd ≥1.7.7.
  - *Deps:* M4.7.
- [ ] **M4.9 — `terminal-sidecar` image + `BuildAgentTerminalService` + ingress** · `M`
  - *Build:* `deploy/docker/terminal-sidecar.Dockerfile` (ttyd ≥1.7.7 + tmux + asciinema, USER 65532, multiarch); `BuildAgentTerminalService` (ClusterIP — first serving-path Service builder); terminal egress builder INGRESS-allowed from the gateway selector, egress default-deny. Add to build-images.sh.
  - *Accept:* image builds multiarch, `ttyd --version` ≥1.7.7; Service golden targets 7681; egress test = default-deny + ingress-allow only from gateway.
  - *Deps:* M4.8; multiarch.
- [ ] **M4.10 — `cmd/agentterminal` attach gateway (authN + authZ + WS proxy)** · `XL`
  - *Build:* new binary (separate from `cmd/agentgateway`): `GET …/terminal` (WSS→ttyd 127.0.0.1:7681), `POST …/terminal/grants`, `/healthz`. AuthN: OIDC bearer (M4.5) + SPIFFE mTLS via `pkg/transport`. AuthZ: resolve `AttachGrant` (viewer/driver, unexpired, audience-bound). Reverse-proxy injects `X-Smol-Attach`, enforces Origin + `frame-ancestors 'none'`. **Driver→writable ttyd; viewer→read-only ttyd** (gateway-enforced). Audit each attach/detach. Deploy `deploy/agentterminal/` — NOT through the agent's Knative path.
  - *Accept:* missing/expired/wrong-aud → 401/403; bad Origin rejected; viewer can't reach writable ttyd; happy-path WSS pipe via httptest. E2E: driver token types a command + PTY echoes; viewer keystrokes don't reach the PTY.
  - *Deps:* M4.5, M4.6, M4.8, M4.9.
  - *Risk:* crown-jewel gateway — hardened, SPIFFE-identified, namespace-RBAC-scoped; bypass the Knative activator.
- [ ] **M4.11 — asciinema session recording → AgentFS** · `M`
  - *Build:* `recorderSidecar` recording tmux `main` to `/tmp` then streaming `.cast` to AgentFS; uid 65532 can't delete shipped casts; gated on `terminal.record`; correlate cast id with the audit event.
  - *Accept:* `record:true` → cast lands in AgentFS; audit references the cast id; absent when false. Mandatory for driver grants.
  - *Deps:* M4.8, M4.10, M2 (artifact-egress path).
- [ ] **M4.12 — Broker policy for interactive (PTY-spawned) callers** · `M`
  - *Build:* extend the broker to distinguish a ttyd-spawned shell PID from the agent PID via `SO_PEERCRED`; a policy knob (default: driver attach gets creds but flagged/audited; optional deny for interactive callers). The cheap part of terminal §8-P4; the continuation engine it references is **deferred** (D6).
  - *Accept:* unit: broker identifies a PTY-spawned caller distinct from the agent; knob toggles lease behavior; default documented.
  - *Deps:* M4.10, M1 (agentpolicy + dynamic-cred).
  - *Risk:* a driver shell inherits the agent's leased creds — within one sandbox a credential can't be fully hidden; secretless-broker (TraT) is the only strong control, out of M4 scope.
- [ ] **M4.13 — (Phase 2) SSH via sshpiper + in-pod sshd** · `L`
  - *Build:* sshpiper (`restful` plugin) routing `ssh <ns>-<name>@gw` → pod sshd, authorizing via a new `POST /v1/ssh/authorize` on `cmd/agentterminal` (pubkey + SPIFFE-bound AttachGrant); in-pod sshd (StatefulSet-only, uid 65532, non-priv port, pubkey-only, host keys as a Secret) running `tmux attach -t main`; gated on `terminal.ssh`.
  - *Accept:* E2E: authorized pubkey lands in tmux `main`; unauthorized rejected by the callback. Webhook already enforces SSH⇒non-knative.
  - *Deps:* M4.10, M2 (artifact-egress).
  - *Risk:* SSH is real key/host-key surface; keep behind `terminal.ssh` default-false; web-terminal is the v1 primary.
- [ ] **M4.14 — Resolve the pi naming collision: `pi-mono` + rename `pi`→`inflection-pi`** · `S`
  - *Build:* `harness.go`: add `HarnessPiMono="pi-mono"`; rename `HarnessPi`/`"pi"`→`HarnessInflectionPi`/`"inflection-pi"`; `deprecatedKindAliases{"pi":InflectionPi}` consulted at admission with a warning; `Valid()` accepts all three; rename the Inflection HTTP harness registration.
  - *Accept:* `kind:pi` still admits (mapped + deprecation warning); `inflection-pi` + `pi-mono` valid; `pi` behavior unchanged.
  - *Deps:* baked default.
- [ ] **M4.15 — `PiMonoHarness` (HTTP kind) + `HarnessPiMonoSpec`** · `M`
  - *Build:* `HarnessPiMonoSpec{Model,Provider,Mode,BridgePort,ExtraArgs,ActiveDeadlineSeconds}` + `HarnessSpec.PiMono`; `ValidateHarness` arm (default url `http://127.0.0.1:8848/run`); `harness/pi.go` `PiMonoHarness` (POST `{prompt,system,model,seed}`, `doWithRetry` for bridge-startup, parse `{output,tokensIn,tokensOut,toolCalls}` — first CLI-family honest tokens, ctx-cancel aborts). Register in `Default()`.
  - *Accept:* fake HTTPClient bridge: request body asserted, JSONL→Response with non-zero tokens+toolCalls, cancel aborts; validation accepts `pi-mono` with no url.
  - *Deps:* M4.14, M2 (richness wire).
- [ ] **M4.16 — `cmd/pi-bridge` HTTP server (wraps pi CLI)** · `M`
  - *Build:* `cmd/pi-bridge/main.go`: `POST /run` → spawn `pi --mode json --no-session -p <prompt>` (+ system/model/extraArgs); stream-parse JSONL **splitting on `\n` only**; accumulate text + last usage + tool events. Key isolation: read provider key from the bridge's own env, write `~/.pi/agent/models.json` (0600) once, spawn pi with env scrubbed of `*_API_KEY`. Bind 127.0.0.1, bounded output, never echo the key. `/agent run` spawns the bridge, waits `/healthz`, SIGTERMs on exit.
  - *Accept:* fake `pi` script: argv asserted, `\n`-only split, env scrub (no `*_API_KEY` in child), bounded output, 0600 write. E2E: `printenv` in the harness container shows no provider key.
  - *Deps:* M4.15, M1 (broker static-lease).
  - *Risk:* scrub is defense-in-depth, NOT airtight (same-uid bash can read the config) — microVM + egress cage are the real containment. pi moves weekly — re-pin + re-verify `--mode json`.
- [ ] **M4.17 — `harness-pi-mono` bundle image + per-kind image map** · `M`
  - *Build:* `deploy/docker/harness-pi-mono.Dockerfile` (build `/agent` + `/pi-bridge`; runtime `node:22-slim`; `npm i -g --ignore-scripts @earendil-works/pi-coding-agent@${PI_VERSION}` + smoke; USER 65532). Add `HarnessPiMono:"harness-pi-mono"` to `perKindHarnessImage`. Add the build arg to build-images.sh (multiarch).
  - *Accept:* `harness_image_test.go`: `pi-mono`→`harness-pi-mono:<tag>`, explicit image wins, version pins; image builds multiarch + `pi --version`.
  - *Deps:* M4.15, M4.16; multiarch.
- [ ] **M4.18 — Generic `PodSpec.ActiveDeadlineSeconds` + `DeadlineExceeded` mapping** · `M`
  - *Build:* `ActiveDeadlineSeconds` is absent today. Add generically in `BuildAgentRunPod` from a resolver: explicit `PiMono.ActiveDeadlineSeconds` (or a generic field, coordinate with run-governance) → else `MaxWallClockSeconds + grace` (budget ctx fires first) → else a flag default. Map a `DeadlineExceeded` pod failure to a terminal run with a clear `TerminationReason`.
  - *Accept:* `agentrun_test.go`: deadline set from budget+grace / explicit / default; pod still hardened. E2E: a looping pi prompt killed → `reason=DeadlineExceeded`.
  - *Deps:* M4.15; coordinate with M1 (run-governance field home).
- [ ] **M4.19 — pi-mono SessionPolicy + interactive-terminal wiring** · `S`
  - *Build:* map pi session flags: `ephemeral`→`--no-session`; `persistent`→`--session <id>` reusing the AgentFS mount. Interactive pi: when `spec.session.interactive`, run pi inside tmux (M4.8), CWD = AgentFS, same broker key + cage.
  - *Accept:* unit: persistent passes `--session` + binds AgentFS CWD; ephemeral `--no-session`. E2E: a coding prompt's `fib.py` persists; an interactive pi-mono Agent is attachable via M4.10.
  - *Deps:* M4.16, M4.8, M4.3.
- [ ] **M4.20 — `HarnessKind=openclaw` WebSocket RPC adapter** · `L`
  - *Build:* add `HarnessOpenClaw="openclaw"` + `Valid()` + HTTP-kind validation (require url); `harness/openclaw.go` `OpenClawHarness.Run` opens a WS, session-open→send→await-reply RPC (WS-first, the single-POST GenericHTTPHarness can't speak it), folds reply into Output; honor budget + retry; register. Honest: tokens 0 unless returned, ToolCalls unset.
  - *Accept:* fake WS server: session-open→send→reply fold; budget timeout cancels; retry on transient WS errors. Webhook: `openclaw` requires url.
  - *Deps:* M2 (Response contract).
  - *Risk:* OpenClaw's agent-loop WS envelope is under-documented — confirm against the running binary; pin the npm version.
- [ ] **M4.21 — SmolAgent serving-path egress + resource overrides** · `L`
  - *Build:* `SmolAgentSpec`: `spec.resources` (replace compiled 500m/512Mi — OpenClaw needs ~1CPU/2Gi req, 4CPU/8Gi limit), `spec.egress` (reuse `EgressSpec`), `spec.ports[]` (declare `control:18789`). `BuildSmolAgentEgressPolicy` (generalize `buildEgressPolicy`). Floor **default-ON** whenever `terminal.enabled` or `session.required` (D1/D3), allow-lists layered.
  - *Accept:* egress golden: DNS + tenant CIDRs + 80/443, metadata blocked. CRD round-trips egress/resources/ports. E2E: daemon reaches an allowed provider but not a non-allowed host nor 169.254.
  - *Deps:* M4.3, D1, D3, M1 (share `EgressSpec`).
- [ ] **M4.22 — OpenClaw reference image + `openclaw.json` rendering** · `M`
  - *Build:* `deploy/docker/agent-openclaw.Dockerfile` (node:24-slim + git + headless-browser, `npm i -g openclaw@<pin>`, a `:8080` probe shim since OpenClaw binds `:18789`, USER 65532, multiarch). Operator renders `~/.openclaw/openclaw.json` (gateway.bind off-loopback, **force `sandbox.mode!=off` + `tools.elevated:false`** per D3, `${VAR}` broker-filled). Shim links the config at start.
  - *Accept:* renderer unit: forced sandbox + elevated:false + `${VAR}` broker-resolvable. E2E: builds multiarch; schedules under kata-fc, passes `/readyz`, answers `:18789` in-pod; not reachable outside the selector.
  - *Deps:* M4.20, M4.21, M1 (broker `${VAR}`).
  - *Risk:* kata + long Node + headless browser is heavy, not broadly live-proven — smoke-test; `:18789` is the full control plane (in-pod/mesh only).
- [ ] **M4.23 — (Option B) agentgateway OpenClaw route + session→pod routing** · `XL`
  - *Build:* extend `cmd/agentgateway` so a turn targeting an OpenClaw daemon dispatches to its WS endpoint (M4.20 client) keyed by session; a session→pod resolver (Endpoints/Service — **session→pod, not round-robin**); `pkg/sessionqueue` for durability; single-instance default (statefulset replicas:1 + state PVC reloads sessions; AgentFS snapshots back up `~/.openclaw`).
  - *Accept:* E2E: two concurrent sessions survive a pod delete (reload from PVC); each session key maps to a stable worker.
  - *Deps:* M4.20, M4.22, M2 (agentsession-scaling), M4.4.
  - *Risk:* scale long-tail (ship Option A first); heap session state lost on pod loss (only files durable).
- [ ] **M4.24 — OpenClaw interactive canvas/terminal access** · `S`
  - *Build:* route human access to OpenClaw's WebChat/debug UI (`:18789/openclaw`) + canvas through `cmd/agentterminal` (M4.10) — authenticated reverse path gated by OIDC + AttachGrant, never a public `:18789`. Set `spec.session.interactive:true`.
  - *Accept:* E2E: an authorized human reaches `:18789/openclaw` through the gateway (audited, OIDC/SPIFFE-gated); unauthorized rejected; `:18789` not directly routable.
  - *Deps:* M4.10, M4.22, M4.3.

**Exit criteria:** `pkg/turnmodel` exists with `TurnExecutor` as the sole runtime coupling; AgentRun (1 turn) + AgentSession (N turns) both flow through it; build/vet/tests green. `spec.session` is live (validated, resident pod on `required`, attach plane on `interactive⇒non-knative`); one-shot never gets one. Driver-mode attach works end-to-end on cftest: a human authenticates against the **bundled OIDC IdP**, an `AttachGrant` authorizes, they attach read-only then driver to a live PTY through `cmd/agentterminal` (SPIFFE-scoped, Origin-checked, audited, recorded); ttyd is loopback-only; viewer keystrokes don't reach the PTY; default-deny egress holds. pi-mono runs over HTTP (drives `pi-bridge`, real tokens + tool-calls, no provider key in the harness env, looping prompt killed by `activeDeadlineSeconds`); `pi`→`inflection-pi` rename + alias in place. OpenClaw serves a multi-turn WS session under kata-fc behind a default-on egress allow-list (`tools.elevated:false`, no nested Docker), `:18789` in-pod only.

**Open / unverified:** OpenClaw agent-loop WS envelope (confirm vs the binary); Dex vs Keycloak (recommend Dex); driver-shell credential inheritance (residual — secretless-broker out of scope); kata on warm long-running daemons under-proven (+ the cftest kata_fc_snapshotter gotcha); SSH gated/optional; `ActiveDeadlineSeconds` field home (coordinate with run-governance); pi-mono `Mode=rpc` + OpenClaw durable-multi-session are the long-tail (loop-resume/HITL deferred — D6).

---

# M5 — Human-in-the-loop + polish

**Goal:** Bring `Phase=RequiresAction` to life as a real approval valve — but per **D6**, ship **only** the cheap, self-contained **harness pre-run gate** (block the pod until a human approves) + its Quint proof. The loop mid-run continuation/resume engine is **explicitly deferred post-GA**, collapsing this from XL to ~M+S.

**Decision deltas:** the entire loop "form B" is **deferred post-GA** (D6) — build the pre-run gate (spec §5.4, P1) + its lifecycle proof (§5.7, P2) only; drop the cross-spec deps that only fed form B (near-term M5 is independent of M2/M3); the README critical-path edge `agent-to-agent-invoker → human-in-the-loop → terminal-exposure` is severed near-term; `RequiresAction` TTL/Expired still ships (a paused pre-run must not hang); the continuation pod's containment is moot near-term (the gate pauses *before any pod exists*).

### Tasks (near-term, active)

- [ ] **M5.1 — Pre-run approval Go types (`ApprovalPolicy`, `Decision`, `PendingAction`)** · `S`
  - *Build:* `types.go`: `ApprovalPolicy{RequireApprovalBeforeRun, ApprovalTimeoutSeconds}` + `AgentSpec.Approval`; `Decision{Token, Approve, Reason, DecidedBy}` + `AgentRunSpec.Decision` + `RequireApprovalBeforeRun *bool`; `PendingAction{Kind, Token, RequestedAt, Reason}` + `RunStatus.PendingAction`. **Omit** form-B-only fields (`RequireApprovalForKinds/Tools`, `Tool/Arguments/StepIndex`, `StepAwaitingApproval`). `make deepcopy`.
  - *Accept:* deepcopy regenerates clean; compiles; only the pre-run subset exists.
  - *Deps:* D6.
- [ ] **M5.2 — Validation for approval/decision inputs** · `S`
  - *Build:* `validation.go`: `ValidateAgentRun` requires `Decision.Token != ""` when Decision set; `ValidateAgent` requires `ApprovalTimeoutSeconds >= 0`.
  - *Accept:* table tests: Decision-without-token rejected; negative timeout rejected; valid passes.
  - *Deps:* M5.1.
- [ ] **M5.3 — Controller pre-run gate + TTL→Expired** · `M`
  - *Build:* `agentrun_controller.go` before the pod block (cancel still wins): compute `effectiveGate` (Agent overridden by run); if gated + not terminal + not approved: no Decision → mint token + `markRequiresAction(run, *PendingAction)` (new helper; non-terminal so requeue guards fire) requeue on TTL; mismatch → wait; deny → `markTerminal(Cancelled, decision:denied)`; approve → clear PendingAction + fall through. TTL → `markTerminal(Expired, approval:timeout)`. `--default-approval-timeout` flag. (Spec patch bumps generation → existing predicate wakes reconcile; `run.json` already marshaled.)
  - *Accept:* envtest: `requireApprovalBeforeRun=true` → `RequiresAction` with **no pod**; approve → pod + Completed; deny → Cancelled; no decision past TTL → Expired; stale token ignored.
  - *Deps:* M5.1, M5.2.
- [ ] **M5.4 — CRD YAML edits (pre-run subset) + PENDING printcolumn** · `S`
  - *Build:* hand-edit agentruns CRD: `spec.decision` (token+approve required, reason, decidedBy), `spec.requireApprovalBeforeRun`, `status.pendingAction` (kind/token/requestedAt/reason — **omit** tool/arguments/stepIndex), a `PENDING` printer column. Hand-edit agents CRD: `spec.approval{requireApprovalBeforeRun, approvalTimeoutSeconds min 0}` — **omit** the form-B arrays.
  - *Accept:* both dry-run apply; `kubectl explain` shows the fields; `kubectl get agentrun` renders PENDING.
  - *Deps:* M5.1.
- [ ] **M5.5 — Quint pre-run approval actions + `ApprovalFreezesBudget`** · `S`
  - *Build:* `agent_execution.qnt`: `awaitApproval` (Running→RequiresAction, all four counters frozen), `approve` (→Running, frozen), `denyOrExpire` (→{Cancelled,Expired,Failed}, frozen); add to the `step` disjunction; invariant `ApprovalFreezesBudget` (pause/resume leaves counters unchanged).
  - *Accept:* `quint run --invariant=Safety` passes; a `humanInTheLoop` scenario holds `ApprovalFreezesBudget` + `BudgetNeverExceeded`.
  - *Deps:* M5.1.
- [ ] **M5.6 — Lifecycle unit tests for the RequiresAction edges** · `S`
  - *Build:* `lifecycle_test.go`: assert `CanTransition` legal for `(Running,RequiresAction)`, `(RequiresAction,{Running,Cancelled,Expired,Failed})`, and `RequiresAction` is non-terminal (edges already exist; lock them).
  - *Accept:* tests pass.
  - *Deps:* none (pairs with M5.3).
- [ ] **M5.7 — Form A e2e on cftest** · `S`
  - *Build:* no new code — deploy a harness Agent with `approval.requireApprovalBeforeRun=true`; assert `STATE=RequiresAction`, `PENDING=pre-run`, no pod; `kubectl patch …spec.decision{approve:true}`; assert pod runs → Completed. Exercise deny→Cancelled + TTL→Expired.
  - *Accept:* generation-bump → reconcile → pod-create completes for approve/deny/timeout on cftest. (Mark `UNVERIFIED` if kubectl flaky after 3 retries.)
  - *Deps:* M5.3, M5.4; cftest.
  - *Risk:* requires live cftest — not laptop-provable.

**Exit criteria:** an Agent with `requireApprovalBeforeRun=true` holds its run in `RequiresAction` with **no pod and zero cost**, shows `PENDING=pre-run`, and on a token-matched `kubectl patch` of `spec.decision{approve:true}` proceeds through the unchanged pod-create path to Completed; deny → Cancelled; TTL → Expired; stale token ignored. Quint `--invariant=Safety` passes with the pre-run actions and `ApprovalFreezesBudget` + `BudgetNeverExceeded` hold. Verified live on cftest (or `UNVERIFIED`). **Explicitly NOT required:** any mid-loop pause, continuation pod, budget carry across a pause, or persisted-Steps-survive-the-clamp behavior.

**Open / unverified:** approval authz model (plain RBAC on `patch agentruns` vs a dedicated `agentruns/decision` subresource for approver separation — decisions.md doesn't address; D9's human OIDC doesn't scope the K8s patch permission); `--default-approval-timeout` default value (~1h suggested). The spec's other open decisions are form-B-specific and moot near-term.

### Deferred to post-GA (do NOT build now)
- **`Executor.Resume(...)` + `runLoop` refactor** `[POST-GA]` — stateful resume across a `Never` pod boundary (D6).
- **Continuation-pod spawn + naming + `runResultFromPod` broadening** `[POST-GA]` — `<run>-c<N>` pods, attempt counter, idempotent get-or-create (D6).
- **`PendingAction{kind:"tool-call"}` + Tool/Arguments/StepIndex + `StepKind=AwaitingApproval`** `[POST-GA]` — the mid-run pause payload (D6).
- **`ApprovalPolicy.RequireApprovalForKinds/Tools` + `gatedTool()` + executor pause emission** `[POST-GA]` — the per-tool gate (D6; needs a production loop invoker).
- **Budget-carry-across-pause + the loop half of `ApprovalFreezesBudget`** `[POST-GA]` — the XL correctness core (a naive resume could consume 2× MaxTokens) (D6).
- **`resume.json` plumbing** (`BuildRunSpecConfigMap` + `ResumeFile` load + `RunResult.PendingAction` wire + clamp-preserve + fold clear-EndedAt) `[POST-GA]` (D6).
- **PendingAction.Arguments redaction** `[POST-GA]` — depends on the M1 redaction engine + form B existing.
- **Session mid-turn HITL** `[POST-GA]` — `RequiresAction` is overloaded with the session-worker's idle-parked meaning; out of scope even in the spec.

---

# Consolidated post-GA deferrals (D6 + scope)

Parked until after GA — tracked so they aren't silently lost:

- **Loop-mode conversational resume** (turn-model history-replay; M4.2 stub) and **HITL mid-run continuation** (all of M5 form B above).
- **Record/replay** (M2.X1/X2) — `kind=replay`, fixtures, `agent eval --mode replay`.
- **Per-turn artifact capture for sessions** (M2.X3); **ToolCalls round-trip through fixtures** (M2.X4).
- **`HermesGateway` operator-managed CRD** — URL-only until a second tenant needs a gateway (RCE blast-radius).
- **pi-mono `Mode=rpc`** (persistent pi process) and **OpenClaw Option B durable-multi-session at scale** (M4.23 is the seam; full scale is the long-tail).
- **Sharded-NATS / 1000s+ concurrency** — out of near-term scope (D10 is mid-scale ~100s).
- **secretless-broker (TraT mint) for interactive driver shells** — the only strong control for credential inheritance; out of M4 scope.

# Consolidated open questions for the maintainer

Genuinely unresolved after decisions.md (each is noted in its milestone above):

1. **NATS per-namespace ACL shape** (M1) — undesigned by any spec body; where it's configured (NATS server config vs operator-rendered per-namespace accounts) + its test plan.
2. **`core/v1` in the pure `pkg/agentmodel/v1`** (M1.11) — allowed, or fall back to a minimal string `ResourceRequirements`?
3. **eBPF Tier-2 datapath placement** (M1) — must it land *within* M1, or co-land with M2's proxy injection? (Tier-1 NetworkPolicy ships in M1 for certain.)
4. **`AgentRunQuota` CRD home** (M1.12) — standalone vs folded into `AgentPolicySpec`.
5. **stdio MCP launcher mechanics** (M2.15) — the in-pod subprocess transport for *approved* images (the allow-list policy is decided; the launcher isn't).
6. **CLI `OutputFormat` per-kind default** (M2.5) — claude/codex `json`, generic `text` (recommended, unratified).
7. **Artifact manifest channel** (M2.26) — ConfigMap (chosen, +1 scoped RBAC) vs sidecar termination-message (zero RBAC, hard ref cap).
8. **Dex vs Keycloak** for the bundled IdP (M4.5) — recommend Dex.
9. **`ActiveDeadlineSeconds` field home** (M4.18) — pi-specific vs a generic `Budget`/`AgentRunSpec` field (coordinate with run-governance; recommend generic).
10. **Approval authz model** (M5) — plain RBAC on `patch agentruns` vs a dedicated `agentruns/decision` subresource for editor/approver separation; and the `--default-approval-timeout` default.
