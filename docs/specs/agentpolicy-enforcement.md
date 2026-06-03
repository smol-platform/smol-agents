# Spec: AgentPolicy enforcement — admission gate, reconcile re-validation, output redaction

> **Status: DESIGN / NOT BUILT (v0.2.0).** As of HEAD (2026-06-02) `AgentPolicy` is a CRD + Go types with **no controller and zero enforcement**. This spec turns it into a real namespace guardrail: an admission webhook that rejects non-conforming `Agent`/`AgentRun` writes, an `AgentPolicyReconciler` that re-marks dependents when a policy tightens, and a redaction pass applied to `RunResult.Output`/`Steps` on the fold. Every "exists today" claim cites `file:line`; every proposed change is marked **(proposed)**.

> Builds on (read first, do not duplicate): [`docs/design/agentnetwork-agentpolicy-interaction.md`](../design/agentnetwork-agentpolicy-interaction.md) §6.1 + §7. That doc is the honest enforcement-reality survey; this is the implementation-grade plan for the `AgentPolicy` half. The `AgentNetwork` datapath half is [`docs/specs/agentnetwork-datapath-enforcement.md`](./agentnetwork-datapath-enforcement.md).

---

## 1. Summary

`AgentPolicy` is a namespace-scoped guardrail CRD with four declared knobs — `allowedProviders`, `allowedTools`, `maxBudget`, `redaction.patterns` (`pkg/agentmodel/v1/types.go:361-370`). Today **none of them does anything**: there is no `agentpolicy_controller.go`, no webhook, and `foldRunResult` copies output verbatim with no redaction step (`operator/internal/controllers/agentmodel/agentrun_controller.go:398-415`). The only `AgentPolicy` mention outside the API/validation packages is a doc comment (`agent_controller.go:2`).

"Full enforcement" of `AgentPolicy` means: (1) a **validating admission webhook** rejects an `Agent` whose `model.providerRef`/`tools[]` fall outside the in-namespace union of allow-lists, and rejects an `AgentRun` whose `budgetOverride` exceeds the effective `maxBudget`; (2) the existing **`AgentReconciler`** re-checks the same rules at reconcile time and marks `Status.Phase=Failed, Reason=PolicyViolation` (defense for objects created before a policy, or via a backdoor that bypasses the webhook); (3) an **`AgentPolicyReconciler`** watches policies and re-enqueues dependent Agents when a policy changes; (4) `foldRunResult` applies the **union of all `redaction.patterns`** to `Status.Output` and `Status.Steps` before persisting. The outcome: declaring an `AgentPolicy` actually constrains the namespace, the CRD's WARNING annotation can be removed, and the namespaced-vs-cluster doc contradiction is resolved in favor of **Namespaced**.

This spec deliberately scopes redaction as a **disclosure control for the cluster-facing record** (what lands in `kubectl get agentrun -o yaml` / audit), **not** a containment boundary — the harness has already seen the unredacted data and could exfiltrate it over the open 80/443 egress floor (`run_sandbox.go:110-119`). That caveat is load-bearing; see §7.

---

## 2. Current state

### 2.1 What exists

| Thing | Where | State |
|---|---|---|
| `AgentPolicy` pure type | `pkg/agentmodel/v1/types.go:355-370` | `Name` + `Spec{AllowedProviders, AllowedTools, MaxBudget, Redaction}` |
| `RedactionPolicy` pure type | `pkg/agentmodel/v1/types.go:368-370` | `Patterns []string` only |
| `AgentPolicy` K8s wrapper | `operator/api/agentmodel/v1/types.go:119-135` | `scope=Namespaced,shortName=apol`; registered in scheme at `:137-145` |
| `Budget` (the cap shape) | `pkg/agentmodel/v1/budget.go:16-32` | 4 axes: `MaxSteps int32`, `MaxTokens int64`, `MaxWallClockSeconds int32`, `MaxToolCalls int32`; `Validate()` at `:45-60` |
| `ValidateAgentPolicy` | `pkg/agentmodel/v1/validation.go:178-186` | validates `Name` + delegates to `MaxBudget.Validate()`; **no allow-list semantics** |
| CRD manifest | `operator/config/crd/runtime.agents.smol-agents.ai_agentpolicies.yaml` | full schema; every field description ends "NOT enforced yet" |
| Deepcopy | `pkg/agentmodel/v1/zz_generated.deepcopy.go:810-826` (RedactionPolicy), `:120-150` (AgentPolicySpec) | generated, complete |

### 2.2 What is stubbed / missing — the gap this spec closes

| Gap | Evidence (`file:line`) |
|---|---|
| **No controller** reconciles `AgentPolicy` | no `agentpolicy_controller.go` in `operator/internal/controllers/agentmodel/` (dir listing: `agent_`, `agentnetwork_`, `agentrun_`, `agentsession_` controllers only) |
| **No webhook** validates `AgentPolicy` composition | `operator/cmd/manager/main.go:136-150` registers only SmolAgent / Platform / AgentNetwork webhooks; no AgentPolicy gate on `Agent`/`AgentRun` writes |
| `allowedProviders` **read by nobody** | `AgentReconciler.Reconcile` resolves `Model.ProviderRef` (`agent_controller.go:69-87`) with no policy check |
| `allowedTools` **read by nobody** | tool resolution loop `agent_controller.go:89-106` checks existence, not allow-list membership |
| `maxBudget` **capped by nobody** | run budget comes only from per-Agent `spec.budget` / per-run `budgetOverride` (`validation.go:133-137`); no namespace ceiling |
| `redaction.patterns` **applied nowhere** | `foldRunResult` copies `rr.Output`/`rr.Steps` verbatim (`agentrun_controller.go:403-404`); zero `redact`/`scrub` symbols in `pkg/`+`operator/` (grep: no non-test matches) |
| **Namespaced-vs-cluster contradiction** | doc comment says "cluster- or namespace-wide" (`operator/api/agentmodel/v1/types.go:119`) but marker says `scope=Namespaced` (`:122`) |

### 2.3 Assets we reuse (do not rebuild)

- **Webhook glue pattern** — `ctrl.NewWebhookManagedBy(mgr, obj).WithValidator(w).Complete()` with a `CustomValidator` whose methods delegate to a **pure** validation function. Established by `SetupAgentWebhook` / `SetupAgentNetworkWebhook` (`operator/internal/webhooks/setup.go:20-32`, `agentnetwork_webhook.go:20-39`). The platform webhook even shows the "fetch a sibling CR, fall through if absent" pattern (`setup.go:42-52`) that the policy webhook needs (list in-namespace policies; if none, allow).
- **`AgentReconciler` resolution flow** — already lists/validates provider + tools and stamps `Status.Phase/Reason/Message` via `setStatus` (`agent_controller.go:42-113,137-142`). The allow-list gate is two extra checks in this exact function, not a new controller.
- **Budget comparison** — `Budget` is a flat 4-field struct; a `min`/`exceeds` helper is trivial and pure.

---

## 3. External interface research

**N/A — internal-only.** `AgentPolicy` is a first-party CRD; there is no external tool whose wire format we must track. (Section retained for template parity per the canonical spec structure.)

---

## 4. Design

### 4.1 Two-layer enforcement: admission (fail-fast) + reconcile (fail-safe)

```
                          ┌─────────────────────────────────────────────┐
   kubectl apply Agent ──▶│ ValidatingWebhook (agentPolicyGate)          │  fail-fast:
   kubectl apply AgentRun │  list in-ns AgentPolicies → effective policy  │  reject the WRITE
                          │  Agent: provider ∈ ∪allowedProviders?         │  (denied: never stored)
                          │         tools[] ⊆ ∪allowedTools?              │
                          │  AgentRun: budgetOverride ≤ min(maxBudget)?   │
                          └───────────────────┬─────────────────────────┘
                                              │ admitted
                                              ▼
                          ┌─────────────────────────────────────────────┐
   Agent reconcile  ─────▶│ AgentReconciler (existing) + policy gate      │  fail-safe:
                          │  same checks → Status.Phase=Failed            │  catch objects that
                          │  Reason=PolicyViolation if violated           │  predate the policy /
                          └───────────────────┬─────────────────────────┘  bypass the webhook
                                              │
   AgentPolicy changed ──▶│ AgentPolicyReconciler                         │  re-enqueue every
                          │  enqueue all Agents in the policy's namespace  │  dependent Agent so a
                          └───────────────────────────────────────────────┘  tightened policy bites

   Run pod terminates ──▶ foldRunResult + redactRunResult                    redaction:
                          apply ∪redaction.patterns to Output + Steps        scrub before persist
                          before run.Status.Update                           (disclosure control)
```

**Why both layers.** The webhook is the primary UX (an apply that violates policy fails immediately with a clear message). But a webhook is not a hard security boundary: it can be bypassed (a write while the webhook pod is down with `failurePolicy: Ignore`, a direct etcd edit, or an `AgentPolicy` created *after* a conforming Agent that the policy now forbids). The reconcile-time gate is the backstop — it cannot reject the write, but it refuses to let the Agent go `Ready`, so no `AgentRun` will schedule against it (the Run reconciler resolves the parent Agent and a `Failed` Agent yields a Pending/Failed run). Redaction is orthogonal and lives on the run-result fold.

### 4.2 Composition semantics (multiple policies in a namespace)

This finalizes [`agentnetwork-agentpolicy-interaction.md`](../design/agentnetwork-agentpolicy-interaction.md) §7 into exact rules. All `AgentPolicy` objects in a namespace compose into **one effective policy**:

| Field | Composition | Rationale |
|---|---|---|
| `allowedProviders` | **union** | A provider allowed by *any* policy is allowed. Intersection would let an unrelated team's policy silently revoke a working Agent — surprising and fragile. Union = additive, predictable. |
| `allowedTools` | **union** | Same reasoning as providers. |
| `maxBudget` (each of 4 axes, independently) | **minimum of the set values** | The tightest cap wins. An unset axis (`0`) on a policy means "this policy does not constrain this axis" and is skipped in the min. |
| `redaction.patterns` | **union** | Every pattern from every policy is applied. More redaction is always the safe direction. |

**Empty-set semantics (the critical default).** If a namespace has **zero** `AgentPolicy` objects, the effective policy is **permissive** (everything allowed, no cap, no redaction) — identical to today's behavior. Adding the first policy is the opt-in. Within an existing policy, an **empty `allowedProviders`/`allowedTools` slice** means **"this policy imposes no provider/tool restriction"** (it contributes nothing to the union), **not** "deny all". This avoids the trap where `kubectl apply` of a policy that only sets `redaction` accidentally bricks every Agent in the namespace. Deny-all is expressible only by a future explicit `denyAll: true` flag (out of scope; §10).

> **Composition is over the union of policies, but a single policy's two list fields are independent.** A policy with `allowedProviders: [openai]` and empty `allowedTools` restricts providers to the union-with-others of `{openai}` and imposes **no** tool restriction from *this* policy.

### 4.3 Effective-policy computation is pure

The compositor is a pure function over a `[]AgentPolicy` — no client, fully unit-testable, lives in `pkg/agentmodel/v1` next to `ValidateAgentPolicy`. Both the webhook and the reconciler call it, so the two layers can never diverge.

```go
// EffectivePolicy is the composition of all AgentPolicies in a namespace.
type EffectivePolicy struct {
    Providers map[string]struct{} // empty (nil) = no provider restriction
    Tools     map[string]struct{} // empty (nil) = no tool restriction
    Budget    *Budget             // nil = no namespace cap; per-axis min where set
    Patterns  []string            // union of all redaction patterns (de-duped)
    Empty     bool                // true iff zero policies contributed any constraint
}

func ComposePolicies(policies []AgentPolicy) EffectivePolicy
func (e EffectivePolicy) AllowsProvider(name string) bool   // true if Providers nil/empty OR contains name
func (e EffectivePolicy) AllowsTool(name string) bool       // true if Tools nil/empty OR contains name
func (e EffectivePolicy) CapBudget(want Budget) (ok bool, axis string) // false + offending axis if want exceeds cap
```

---

## 5. Concrete changes

### 5.1 Pure package — `pkg/agentmodel/v1/`

**New file `policy.go` (proposed)** — the compositor + checks above. Keeps `types.go` (declarations) and `validation.go` (single-object validation) clean; this file is multi-object *semantics*.

- `ComposePolicies([]AgentPolicy) EffectivePolicy` — union the slices into sets (skip empties), min each budget axis over policies that set it (axis `0` = unset = skip), concat+dedupe patterns. Set `Empty=true` iff no policy contributed any of the four.
- `EffectivePolicy.AllowsProvider/AllowsTool/CapBudget` — as above.
- `Budget` axis-min helper: `func minBudget(a, b *Budget) *Budget` treating `0` as "unset/ignore" per axis (note: `MaxToolCalls=0` is legitimately "no tool calls allowed" in `budget.go:30-31`; for the *cap* min we treat the smaller of two **set** values, and a policy that omits `maxToolCalls` should serialize it as absent — see §10 open decision D3 on `0` ambiguity).

**Edit `validation.go:178-186`** — extend `ValidateAgentPolicy` to also reject obviously-broken redaction patterns: each `redaction.patterns[i]` must `regexp.Compile` (else the webhook would store a policy that panics the fold). Add:

```go
for i, p := range p.Spec.Redaction.Patterns {  // when Redaction != nil
    if _, err := regexp.Compile(p); err != nil {
        errs = append(errs, fmt.Errorf("spec.redaction.patterns[%d]: invalid regexp: %w", i, err))
    }
}
```

**New `redact.go` (proposed)** — the redaction engine, pure, reused by the fold:

```go
// RedactJSON masks every substring matching any compiled pattern inside a
// json.RawMessage, returning valid JSON. Patterns are applied to STRING VALUES
// only (it walks the decoded value; never to keys, never to numeric/bool tokens),
// so the result stays parseable. Non-JSON or undecodable input is masked wholesale.
func RedactJSON(raw json.RawMessage, pats []*regexp.Regexp) json.RawMessage

// RedactSteps applies pats to the redactable string-bearing fields of each Step:
//   Step.Error, ToolCallRecord.Arguments (json), ToolCallRecord.Result (json),
//   ToolCallRecord.Error. Index/Kind/timestamps/token counts are left intact.
func RedactSteps(steps []Step, pats []*regexp.Regexp) []Step

const RedactionMask = "[REDACTED]"
```

Rationale for string-value-only masking: `Output` and `ToolCallRecord.Arguments/Result` are `json.RawMessage` (`types.go:307-308,321`). Naively regex-replacing the raw bytes would corrupt JSON (mask a quote/brace → unparseable, breaks every downstream consumer of `Status.Output`). Walk → mask string leaves → re-marshal guarantees validity.

### 5.2 Webhook — `operator/internal/webhooks/`

**New file `agentpolicy_gate_webhook.go` (proposed)** — validating webhooks on **`Agent`** and **`AgentRun`** (NOT on `AgentPolicy` itself — that gets its own simple validator, §5.4). Mirror `agentWebhook` (`setup.go:34-88`): a struct holding `client.Client`, methods `ValidateCreate/Update/Delete`.

```go
type agentPolicyGate struct{ client client.Client }

func SetupAgentPolicyGateWebhook(mgr ctrl.Manager) error {
    g := &agentPolicyGate{client: mgr.GetClient()}
    if err := ctrl.NewWebhookManagedBy(mgr, &amv1.Agent{}).WithValidator(g.forAgent()).Complete(); err != nil {
        return err
    }
    return ctrl.NewWebhookManagedBy(mgr, &amv1.AgentRun{}).WithValidator(g.forRun()).Complete()
}
```

- **`forAgent()` validator** — on create/update: list `AgentPolicyList` in `agent.Namespace`, `ComposePolicies`, then:
  - if `agent.Spec.Model.ProviderRef != "" && !eff.AllowsProvider(ref)` → `field.Forbidden(model.providerRef, msg)`.
  - for each `agent.Spec.Tools[i].Name` not `eff.AllowsTool(name)` → `field.Forbidden(spec.tools[i], msg)`.
  - aggregate into `apierrors.NewInvalid(GK, name, fieldErrs)` so `kubectl` prints all violations at once.
- **`forRun()` validator** — on create: resolve the parent Agent (to get the *effective* budget = `budgetOverride ?? agent.Spec.Budget`), list policies, `CapBudget`. If the effective run budget exceeds the cap on any axis → reject naming the axis. (Update is a no-op for budget: `budgetOverride` is immutable post-create in practice; validate create only.)
- **Fail-open on list error / no policies** — exactly like `fetchPlatform` returning `nil` (`setup.go:46-48`): if listing policies errors transiently, **do not** block writes (return the error only if it is a real API failure, not NotFound); if `eff.Empty`, admit. This keeps the namespace usable when no policy is declared.

**Register in `operator/cmd/manager/main.go`** — inside the existing `if os.Getenv("ENABLE_WEBHOOKS") != "false"` block (`main.go:137-150`), after the AgentNetwork webhook:

```go
if err := webhooks.SetupAgentPolicyGateWebhook(mgr); err != nil {
    setupLog.Error(err, "unable to register AgentPolicy gate webhook")
    os.Exit(1)
}
```

**Webhook manifest** — add `ValidatingWebhookConfiguration` rules for `agents` and `agentruns` (verbs create/update) under `operator/config/webhook/` (the existing kustomize webhook config that backs the other three). `failurePolicy: Fail` for the gate is the secure choice **only if** the webhook is HA; for a single-replica operator use `Fail` with `timeoutSeconds: 5` and rely on the reconcile backstop, OR `Ignore` + backstop. **Decision D1 (§10).**

### 5.3 Reconcile-time gate — `operator/internal/controllers/agentmodel/agent_controller.go`

Insert the same checks into the existing `Reconcile`, after tool resolution succeeds (`agent_controller.go:108`) and before the final `setStatus(..., "Ready", ...)` at `:110`. Reuse the resolved data already in hand:

```go
// (proposed) Policy gate: compose in-namespace AgentPolicies and verify the
// resolved provider + tools are allowed. A violation fails the Agent so no
// AgentRun schedules against it (defense for objects predating the policy or
// bypassing the webhook).
var pols amv1.AgentPolicyList
if err := r.List(ctx, &pols, client.InNamespace(agent.Namespace)); err != nil {
    return ctrl.Result{}, err
}
eff := pure.ComposePolicies(toPurePolicies(pols.Items))
if providerName != "" && !eff.AllowsProvider(providerName) {
    r.setStatus(agent, "Failed", "PolicyViolation",
        fmt.Sprintf("provider %q not in namespace allowedProviders", providerName))
    return ctrl.Result{}, r.Status().Update(ctx, agent)
}
for _, t := range resolved {
    if !eff.AllowsTool(t) {
        r.setStatus(agent, "Failed", "PolicyViolation",
            fmt.Sprintf("tool %q not in namespace allowedTools", t))
        return ctrl.Result{}, r.Status().Update(ctx, agent)
    }
}
```

`toPurePolicies` unwraps `[]amv1.AgentPolicy` → `[]pure.AgentPolicy` (mirrors existing `toPure` at `agent_controller.go:133-135`).

**Wire the watch** so a policy change re-evaluates Agents — edit `AgentReconciler.SetupWithManager` (`agent_controller.go:34-39`):

```go
return ctrl.NewControllerManagedBy(mgr).
    For(&amv1.Agent{}).
    Owns(&corev1.ServiceAccount{}).
    Watches(&amv1.AgentPolicy{}, handler.EnqueueRequestsFromMapFunc(r.agentsInNamespace)). // (proposed)
    Complete(r)
```

`agentsInNamespace(ctx, obj)` lists Agents in `obj.GetNamespace()` and returns one `reconcile.Request` each. This makes a tightened policy bite existing Agents within one reconcile, satisfying §6.1 of the design doc ("re-mark dependent Agents"). **This subsumes the need for a separate `AgentPolicyReconciler` for the Agent-marking job** — see D2 (§10): a dedicated reconciler is only needed if `AgentPolicy.Status` must report bound/violating-Agent counts. Recommended: skip the standalone reconciler for v1; the watch + Agent gate covers enforcement. (If we want `AgentPolicy.Status.violatingAgents`, add the reconciler in a follow-up; it is observability, not enforcement.)

### 5.4 `AgentPolicy` self-validation webhook (proposed, optional, S)

A trivial `CustomValidator` on `AgentPolicy` delegating to `pure.ValidateAgentPolicy` (now regex-checking, §5.1) — identical shape to `agentNetworkWebhook` (`agentnetwork_webhook.go`). Surfaces a bad regexp / bad budget at apply time instead of at the first fold. Fold-time redaction (§5.5) compiles defensively regardless, so this is UX polish.

### 5.5 Redaction on the fold — `operator/internal/controllers/agentmodel/agentrun_controller.go`

The redaction needs the effective policy for the run's namespace. Compute it inside the fold path. **Edit `foldRunResult` (`agentrun_controller.go:398-415`)** to take a `ctx` (it currently does not) and apply redaction after copying:

```go
// signature change: foldRunResult(ctx, run, pod)  — call sites at :240 and :243
func (r *AgentRunReconciler) foldRunResult(ctx context.Context, run *amv1.AgentRun, pod *corev1.Pod) {
    rr, ok := runResultFromPod(pod)
    if !ok {
        return
    }
    pats := r.compileRedaction(ctx, run.Namespace) // (proposed) list policies → union patterns → compile
    run.Status.Output = pure.RedactJSON(rr.Output, pats)
    run.Status.Steps  = pure.RedactSteps(rr.Steps, pats)
    run.Status.Usage  = rr.Usage
    switch { // unchanged
    case rr.Error != "":
        run.Status.TerminationReason = rr.Error
    case rr.TerminationReason != "":
        run.Status.TerminationReason = rr.TerminationReason
    }
    if rr.Phase != "" {
        run.Status.State = rr.Phase
    }
}
```

`compileRedaction(ctx, ns)` lists `AgentPolicyList` in `ns`, composes, compiles each pattern (skipping any that fail to compile, logging a warning — they were rejected at admission but a backdoor write could slip one in; the fold must never panic). When `pats` is empty, `RedactJSON`/`RedactSteps` return their input unchanged (zero overhead — the common no-policy path).

Note: the `TerminationReason` is **not** redacted — it is a controlled enum-ish string (`"budget:tokens"`, `"sandbox:..."`, runtime error) and redacting it could hide why a run failed. If a runtime error string can carry secrets, that is a separate hardening item (D4, §10).

### 5.6 Operator-side type doc fix — `operator/api/agentmodel/v1/types.go:119`

Resolve the contradiction. The kubebuilder marker is authoritative (`scope=Namespaced`, `:122`). **Edit the doc comment** at `:119`:

```go
// AgentPolicy declares namespace-scoped guardrails (allow-lists, a per-run
// budget ceiling, and output redaction) for every Agent/AgentRun in its
// namespace. Composition across multiple policies: union of allow-lists,
// minimum of budget caps, union of redaction patterns. There is no
// cluster-scoped variant.
```

Drop "cluster- or" entirely. No cluster-scoped policy exists; if needed later it is a separate `ClusterAgentPolicy` kind (§10).

### 5.7 CRD manifest — `operator/config/crd/runtime.agents.smol-agents.ai_agentpolicies.yaml`

Once enforcement lands, **remove the "NO controller / NOT enforced" WARNING** from the top-level description (`:20-25`) and the per-field "NOT enforced yet" / "NOT applied yet" suffixes (`:34,38,42,48,50,54`). Replace with the composition semantics (union/min/union) and the empty-set behavior. Per [MEMORY: CRD generation drift], **edit this file by hand** — do not blindly `make manifests`; the tree's CRDs are not reproducibly regenerated and a regen risks the `smolagents.stigen.ai` vs `agents.stigen.ai` group churn noted there. Add an `xref` validation hint only if a kubebuilder marker is added to the Go type.

### 5.8 Files touched — summary

| File | Change | New/Edit |
|---|---|---|
| `pkg/agentmodel/v1/policy.go` | compositor + `EffectivePolicy` + checks | **new** |
| `pkg/agentmodel/v1/redact.go` | `RedactJSON`, `RedactSteps`, mask const | **new** |
| `pkg/agentmodel/v1/validation.go` | regex-compile check in `ValidateAgentPolicy` (`:178`) | edit |
| `operator/internal/webhooks/agentpolicy_gate_webhook.go` | gate on Agent + AgentRun (+ optional AgentPolicy self-validate) | **new** |
| `operator/internal/webhooks/setup.go` | (optional) add `SetupAgentPolicyWebhook` helper for self-validation | edit |
| `operator/cmd/manager/main.go` | register gate webhook (`:149`) | edit |
| `operator/internal/controllers/agentmodel/agent_controller.go` | policy gate after tool resolution (`:108`); `Watches(AgentPolicy)` (`:36`); `toPurePolicies` helper | edit |
| `operator/internal/controllers/agentmodel/agentrun_controller.go` | redaction in `foldRunResult` (`:398`) + `compileRedaction`; thread `ctx` to call sites (`:240,:243`) | edit |
| `operator/api/agentmodel/v1/types.go` | fix doc comment (`:119`) | edit |
| `operator/config/crd/..._agentpolicies.yaml` | drop "NOT enforced" warnings; document composition | edit (by hand) |
| `operator/config/webhook/` (kustomize) | add ValidatingWebhookConfiguration rules for agents+agentruns | edit |

---

## 6. Data / control flow

### 6.1 Apply-time (webhook)

```
kubectl apply Agent{provider: anthropic, tools: [shell]}
  → APIServer → ValidatingWebhook /validate-agents
      → agentPolicyGate.forAgent.ValidateCreate
          → List AgentPolicy in ns  →  [pol-A{allowedProviders:[openai]}, pol-B{allowedTools:[shell]}]
          → ComposePolicies → eff{Providers:{openai}, Tools:{shell}}
          → eff.AllowsProvider("anthropic") == false
          → return apierrors.NewInvalid(... field.Forbidden("spec.model.providerRef",
                "provider \"anthropic\" not in namespace allowedProviders {openai}"))
  → APIServer rejects; kubectl prints the Forbidden message; object NEVER stored
```

### 6.2 Reconcile-time (backstop) — policy created after the Agent

```
t0: Agent{provider: openai} applied, no policies → webhook admits → Reconcile → Ready
t1: kubectl apply AgentPolicy{allowedProviders:[anthropic]}
      → Watches(AgentPolicy) map fn → enqueue all Agents in ns
      → AgentReconciler.Reconcile(Agent)
          → resolve provider "openai" (exists) + tools
          → ComposePolicies → eff{Providers:{anthropic}}
          → eff.AllowsProvider("openai") == false
          → setStatus(Failed, PolicyViolation, "provider \"openai\" not in namespace allowedProviders")
t2: any new AgentRun referencing this Agent → AgentRunReconciler resolves a Failed Agent
      → (existing) the run cannot proceed to a healthy pod; surfaces the parent's failure
```

### 6.3 Fold-time (redaction)

```
run pod terminates → AgentRunReconciler.Reconcile → PodSucceeded
  → markTerminal(Completed) → foldRunResult(ctx, run, pod)
      → runResultFromPod → rr{Output:{"token":"sk-live-abc123"}, Steps:[...]}
      → compileRedaction(ctx, ns): List AgentPolicy → union patterns [`sk-live-[A-Za-z0-9]+`] → compile
      → RedactJSON(rr.Output, pats)  → {"token":"[REDACTED]"}
      → RedactSteps(rr.Steps, pats)  → tool args/results/errors masked
      → run.Status.Output/Steps = redacted
  → Status().Update  →  kubectl get agentrun -o yaml shows [REDACTED]
```

---

## 7. Security model

### 7.1 How it composes with the existing stack

| Layer | Role | This spec's interaction |
|---|---|---|
| **kata-fc sandbox** (`run_sandbox.go:45-53`, applied `agentrun_controller.go:189`) | microVM kernel isolation per run | Orthogonal. AgentPolicy does not change the sandbox; it gates *which* Agent/provider/tool/budget is permitted to run inside it. |
| **Static egress NetworkPolicy** (`run_sandbox.go:60-123`) | default-deny floor; blocks `169.254/16` | Orthogonal, and the reason redaction is disclosure-only: the floor allows arbitrary HTTPS, so a malicious harness can exfil unredacted data *before* the fold. Redaction protects the record, not the data-in-flight. |
| **Secrets broker** (`AttachSecretBroker`, `agentrun_controller.go:209`) | leases provider/harness secrets into the pod at runtime | `allowedProviders` limits which `ModelProvider` (hence which brokered key) an Agent may bind — a coarse pre-broker gate. Redaction is the *last line* if a secret value leaks into `Output` (it should not — the agent never sees the raw key, only the broker does — but defense in depth). |
| **SPIFFE identity** (`IdentitySpec`, `types.go` SPIFFE prefix) | per-run SVID | Unaffected. AgentPolicy is an authorization-policy layer above identity; it does not mint or consume SVIDs. |

### 7.2 New attack surface + mitigations

| Surface | Risk | Mitigation |
|---|---|---|
| **Webhook bypass** (pod down + `failurePolicy: Ignore`, direct etcd) | a forbidden Agent gets stored | The reconcile-time gate (§5.3) re-checks and marks `Failed`; runs won't schedule against a `Failed` Agent. Webhook is fail-fast UX, not the boundary. |
| **Redaction regex DoS / catastrophic backtracking** | a pathological pattern hangs the fold (which holds the reconcile worker) | Compile-validate at admission (`ValidateAgentPolicy`); run redaction with a wall-clock guard (`context` deadline or `regexp` is RE2 — Go's `regexp` is **linear-time, no backtracking**, so catastrophic backtracking is impossible; the residual risk is huge inputs, bounded by the 4 KiB termination-message cap on `RunResult` anyway). |
| **Redaction false sense of security** | operator believes redacted output ⇒ data contained | Documented explicitly (this section + §1 + CRD description): redaction is a **disclosure control for the cluster record**, not a containment boundary. The honest framing mirrors `agentnetwork-agentpolicy-interaction.md` §6.1's hard caveat. |
| **Incomplete redaction** (secret in a JSON *key*, or split across tokens) | secret leaks despite a pattern | `RedactJSON` masks string *values* only; a secret used as a key is not masked. Documented limitation; recommend patterns target known value shapes (`sk-…`, `ghp_…`). Whole-blob mask fallback for non-JSON output. |
| **Allow-list union too permissive** | adding any policy widens, never narrows, provider/tool sets | By design (predictability over strictness); deny-all needs explicit future opt-in (D5). The cap (`maxBudget`) uses **min** precisely because budgets should tighten. |
| **TOCTOU: policy tightened between Agent-Ready and Run-create** | a run starts under a now-forbidden config | The AgentRun webhook re-composes at run-create; and the Agent watch flips the parent to `Failed` promptly. A run already-running is not retroactively killed (no live re-eval) — documented; a "terminate-on-policy-violation" sweep is out of scope (D6). |

---

## 8. Phasing & effort

Dependencies: this spec consumes the `Step`/`RunResult` shapes — none of the other specs block it. [`response-richness`](./response-richness.md) (folding richer Steps) and the 4 KiB termination-cap fix increase what redaction must traverse but do not block this work; redaction handles whatever Steps are present.

| Phase | Scope | Effort | Depends on |
|---|---|---|---|
| **P1 — Pure compositor + redaction engine** | `policy.go` (`ComposePolicies`/`EffectivePolicy`/checks), `redact.go` (`RedactJSON`/`RedactSteps`), `validation.go` regex check. 100% unit-tested, no K8s. | **M** | — |
| **P2 — Redaction on the fold** | thread `ctx`, `compileRedaction`, apply in `foldRunResult`; the highest-value, lowest-risk slice (it is additive and the no-policy path is a no-op). | **S** | P1 |
| **P3 — Reconcile-time gate + watch** | Agent gate in `AgentReconciler`, `Watches(AgentPolicy)`, `toPurePolicies`. This alone makes policies *enforce* (Agents go `Failed`) even without a webhook. | **M** | P1 |
| **P4 — Admission webhook** | `agentpolicy_gate_webhook.go` (Agent + AgentRun), main.go registration, kustomize `ValidatingWebhookConfiguration`, optional AgentPolicy self-validation. Fail-fast UX. | **M** | P1, P3 (shares the compositor) |
| **P5 — Docs/CRD cleanup** | drop "NOT enforced" warnings, fix the namespaced doc comment, update `agentnetwork-agentpolicy-interaction.md` §2 table rows from NO CONTROLLER → ENFORCED. | **S** | P2–P4 landed |

Recommended ship order: **P1 → P2 → P3 → P4 → P5.** P2 delivers redaction immediately; P3 delivers enforcement without webhook infra risk; P4 adds the polished apply-time rejection. A standalone `AgentPolicyReconciler` (for `Status` counts) is **deferred** (D2) and not in any phase above.

---

## 9. Test plan

### 9.1 Unit (pure, `pkg/agentmodel/v1/`)

- **`policy_test.go`** — `ComposePolicies`: zero policies → `Empty=true`, all-allow; union of two `allowedProviders`; empty-slice policy contributes nothing (does not deny-all); `maxBudget` per-axis min across policies, unset axis skipped; pattern union de-dupes. `AllowsProvider/AllowsTool`: nil set ⇒ allow; populated set ⇒ membership. `CapBudget`: returns offending axis name; equal-to-cap is allowed (boundary).
- **`redact_test.go`** — `RedactJSON`: masks a string value matching a pattern; leaves numbers/bools/keys intact; output re-parses as valid JSON; nested objects/arrays; non-JSON input → whole-blob mask; empty patterns → identity (byte-equal). `RedactSteps`: masks `Step.Error`, `ToolCallRecord.Arguments/Result/Error`; leaves `Index/Kind/timestamps/TokensIn/Out` intact. Property: redacted output never contains a substring matching any pattern.
- **`validation_test.go`** — `ValidateAgentPolicy` rejects an uncompilable regexp (`"("`); accepts a valid one; existing `MaxBudget.Validate` path unchanged.

### 9.2 Controller (envtest, `operator/internal/controllers/agentmodel/`)

Reuse the envtest harness the existing `agent_controller_test.go` / `agentrun_controller_test.go` use.

- **`agentpolicy_gate_test.go`** (reconcile path) — apply `AgentPolicy{allowedProviders:[openai]}` + Agent referencing an `anthropic` provider → Agent reconciles to `Phase=Failed, Reason=PolicyViolation`. Apply a conforming Agent → `Ready`. Tighten the policy after the Agent is `Ready` → Watch re-enqueues → Agent flips to `Failed` within the test's eventual-consistency window. Tool allow-list symmetric case.
- **`agentrun_redaction_test.go`** (fold path) — drive a run pod whose container termination message is a `RunResult` with a secret-shaped `Output` and a `Step` carrying a secret in `ToolCallRecord.Result`; apply a policy with the matching pattern; assert `AgentRun.Status.Output`/`.Steps[].ToolCalls[].Result` are masked and `Status` is valid JSON. No-policy case → output byte-identical to the RunResult.

### 9.3 Webhook (envtest with webhook server)

- `ValidateCreate` on Agent with a forbidden provider returns `apierrors.IsInvalid`; with no policies returns nil (fail-open); with a list error returns the error (does not silently admit on a real API failure). AgentRun `budgetOverride` exceeding `maxBudget` → Invalid; within → admit.

### 9.4 e2e (cftest single-node k0s, per [MEMORY: hermes_zai_e2e_proven / cf_tunnel_deploy])

The live box at `~/.ssh/agent_claude_workspace` is the integration target. One scenario:

1. Deploy operator (`operator:0.1.x+1`) with the gate webhook enabled.
2. `kubectl apply` an `AgentPolicy{redaction.patterns: ["glm-secret-[0-9]+"]}` and a Hermes/z.ai Agent whose run echoes a secret-shaped string into its output.
3. Run it; assert `kubectl get agentrun -o jsonpath='{.status.output}'` shows `[REDACTED]`, and the pod logs (pre-fold) still show the raw value (proving redaction is fold-only / disclosure-scoped).
4. `kubectl apply` an `AgentPolicy{allowedProviders:[openai]}` then an `anthropic`-bound Agent → apply is **rejected** by the webhook (assert non-zero `kubectl apply` exit + Forbidden text).

Per [MEMORY: e2e_progress] mark the commit `UNVERIFIED: kubectl connectivity failed` if the box is unreachable, never silently.

---

## 10. Risks & open decisions

**Risks**

- **Redaction is disclosure-only, and that is easy to mis-sell.** The single largest documentation risk is repeating the `R-AN-PROXY-3` mistake (a comment that over-claims). Every surface (CRD description, type doc, this spec) must state redaction protects the *record*, not the *data*. Calling it "DLP" would be a lie.
- **Webhook + reconcile drift.** Mitigated by both layers calling the *same* pure `ComposePolicies`. If a future change adds a check to one layer only, behavior diverges. Keep all semantics in `pkg/agentmodel/v1/policy.go`.
- **`make manifests` drift.** Per [MEMORY: crd_generation_drift], editing the CRD by hand is required; a regen could revert the group or re-introduce the warning. Note this in the PR.
- **Performance of per-fold policy list.** `foldRunResult` runs on every terminal run; it now does a `List(AgentPolicy)`. Cheap (cached client, small per-ns set) but non-zero. The no-policy fast path (empty `pats` ⇒ identity) keeps it negligible for namespaces with no policy.

**Open decisions (maintainer must choose)**

- **D1 — Webhook `failurePolicy`.** `Fail` (secure, but a down single-replica operator blocks all Agent/AgentRun writes) vs `Ignore` (available, relies on the reconcile backstop). **Recommendation:** `Ignore` + the §5.3 backstop for a single-replica operator; revisit to `Fail` when the operator is HA. The other webhooks (`main.go:137-150`) don't expose their policy here — confirm the existing kustomize default.
- **D2 — Standalone `AgentPolicyReconciler`?** The §5.3 Agent watch covers *enforcement*. A dedicated reconciler is only for `AgentPolicy.Status` (e.g. `violatingAgents`/`boundAgents` counts), purely observability. **Recommendation:** defer; ship enforcement first. (Note: `AgentPolicy` currently has **no Status field** — adding one is part of that follow-up.)
- **D3 — `maxToolCalls: 0` ambiguity.** In `Budget` (`budget.go:30`), `0` legitimately means "no tool calls". In a *cap*, `0` is ambiguous between "unset" and "cap at zero". **Recommendation:** make `MaxBudget`'s axes pointer-typed in a future CRD revision, or document that an omitted axis = unset and `0` = cap-at-zero only for `maxToolCalls`. Lowest-churn: treat `0` as "unset" in the min for the three axes whose floor is `1` (they can't legitimately be `0`), and pointer-gate `maxToolCalls`. Needs a decision before P1 finalizes the compositor.
- **D4 — Redact `TerminationReason`?** Currently no (it's a controlled string). If runtime error strings can carry user/secret data, that changes. **Recommendation:** leave unredacted; audit error-string provenance separately.
- **D5 — Deny-all expressibility.** Union semantics mean policies only widen allow-lists. A namespace that wants "no provider unless explicitly listed" cannot express it today (the first policy with a non-empty list *is* the allow-list, but an *empty* list = no restriction). **Recommendation:** add an explicit `denyUnlistedProviders/Tools bool` (or a `denyAll` policy) in a follow-up; out of scope here.
- **D6 — Live policy violation on a running run.** Tightening a policy does not kill in-flight runs. **Recommendation:** out of scope; document. A terminate-on-violation sweep is a separate run-governance concern (see [`run-governance`](./run-governance.md)).
- **D7 — Cluster-scoped policy.** Confirmed out: `AgentPolicy` is Namespaced (§5.6). A `ClusterAgentPolicy` is a future, separate kind if cluster-wide guards are needed.

---

## 11. Cross-links

- [`docs/design/agentnetwork-agentpolicy-interaction.md`](../design/agentnetwork-agentpolicy-interaction.md) — the enforcement-reality survey this spec implements (§6.1 AgentPolicyReconciler/admission, §7 composition + namespaced-vs-cluster).
- [`docs/specs/agentnetwork-datapath-enforcement.md`](./agentnetwork-datapath-enforcement.md) — the sibling `AgentNetwork`-on-runs work; the two CRDs' enforcement should ship aware of each other.
- [`docs/specs/response-richness.md`](./response-richness.md) — richer `Steps` folding; redaction must traverse whatever Steps carry.
- [`docs/specs/run-governance.md`](./run-governance.md) — budget/quota/termination governance; `maxBudget` here is one input.
- [`docs/research/agent-runtime-fit-analysis-v0.2.0.md`](../research/agent-runtime-fit-analysis-v0.2.0.md) — the runtime-fit report flagging AgentPolicy as a P1 unenforced gap.
