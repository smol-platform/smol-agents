# Requirements — smol-agents-operator

Each requirement carries a stable ID. Requirements are grouped by
operator concern (`R-OP-API`, `R-OP-FF` for feature flags, `R-OP-REC`
for reconciliation, `R-OP-WH` for webhooks, etc.) and reference the
platform-level requirements they expose (`R-IDN-*`, `R-MTL-*`, etc.).

## Alignment with Product Vision

These requirements implement `product.md`: feature-flagged from day
one, reality matches spec via Conditions, no silent fallbacks, reuse
existing Go packages, and verifiable.

## Requirements

### R-OP-API: Custom Resource Surface

#### R-OP-API-1 — `SmolAgent` namespaced CR
**User Story:** As a tenant, I want one CR that fully describes my
agent, so that I can submit it via GitOps.

**Acceptance Criteria:**
1. WHEN a `SmolAgent` is created in any namespace THEN the
   operator SHALL reconcile every enabled feature into the cluster.
2. WHEN required fields (e.g. `spec.trustDomain`) are missing THEN
   the validating webhook SHALL reject the CR with a typed message.
3. THE CR SHALL be served at `agents.stigen.ai/v1` and include
   `kubectl get` printer columns: `MODE`, `READY`, `IDENTITY`,
   `MTLS`, `EBPF`, `SECRETS`, `RUNTIMECLASS`, `AGE`.

#### R-OP-API-2 — `SmolAgentPlatform` cluster CR
**User Story:** As a cluster admin, I want one cluster-scoped CR
that controls platform defaults (RuntimeClass, ebpf-loader, default
trust domain), so tenants don't need duplicate config.

**Acceptance Criteria:**
1. EXACTLY ONE `SmolAgentPlatform` SHALL be permitted per cluster
   (validating webhook enforces).
2. WHEN the `SmolAgentPlatform` does not exist THEN every
   `SmolAgent` SHALL stay in `Pending` with reason
   `PlatformAbsent`.
3. THE platform CR SHALL declare which feature flags tenants are
   permitted to enable; tenant CRs setting a forbidden feature
   SHALL be rejected.

#### R-OP-API-3 — OpenAPI v3 schema with `kubectl explain`
**User Story:** As a tenant, I want to discover the API via
`kubectl explain` so I don't need to read a long manual.

**Acceptance Criteria:**
1. EVERY field in the CR types SHALL have a `// +kubebuilder:`
   doc comment that round-trips into the OpenAPI v3 schema served
   by the API server.
2. `kubectl explain smolagent.spec.features.identity` SHALL
   return the identity feature documentation.

### R-OP-FF: Feature Flags

#### R-OP-FF-1 — Per-feature flag with `enabled` boolean
**User Story:** As a platform engineer, I want to enable one feature
at a time so I can canary safely.

**Acceptance Criteria:**
1. THE following features SHALL each be a top-level field under
   `spec.features.<name>` with at minimum an `enabled: bool`:
   `identity`, `transport.private`, `transport.public`, `secrets`,
   `sandbox`, `ebpf`, `knative`, `observability`.
2. WHEN `enabled: false` THEN the operator SHALL NOT create
   resources for that feature, AND SHALL delete any previously
   created ones whose owner ref is the CR.
3. THE default value of every feature flag in `v1` SHALL be
   `enabled: true` to preserve current Helm-chart behaviour, except
   `transport.public` which defaults to `false`.

#### R-OP-FF-2 — Feature `mode` enum where applicable
**User Story:** As a tenant, I want to put a feature into
permissive/strict modes consistent with platform conventions.

**Acceptance Criteria:**
1. `spec.features.identity.mode` SHALL accept `insecure`,
   `permissive`, or `strict` (R-IDN-3).
2. `spec.features.sandbox.runtimeClass` SHALL accept any installed
   `RuntimeClass`; `runc` requires `allowHostEscape: true`
   (R-SBX-1).
3. `spec.features.ebpf.capabilities` SHALL accept `privileged` or
   `minimal` (multi-distro presets from `values.yaml`).

#### R-OP-FF-3 — Per-feature Status condition
**User Story:** As an operator, I want to know exactly which
features are ready.

**Acceptance Criteria:**
1. FOR EACH feature `F` in `spec.features` THE operator SHALL emit
   a Condition of type `<F>Ready` with `Status` (`True` | `False` |
   `Unknown`), `Reason`, `Message`, and `LastTransitionTime`.
2. WHEN `enabled: false` THE Condition SHALL be `Type=<F>Ready,
   Status=False, Reason=Disabled`.
3. WHEN prerequisites are unmet THE Condition SHALL be
   `Type=<F>Ready, Status=False, Reason=PrerequisitesUnmet,
   Message=<which prereq>`.

#### R-OP-FF-4 — Feature flag matrix on the platform CR
**User Story:** As a security admin, I want to disable a feature
cluster-wide regardless of tenant CRs.

**Acceptance Criteria:**
1. `SmolAgentPlatform.spec.featurePolicy` SHALL accept a list of
   `{feature: <name>, allowed: <bool>, defaultEnabled: <bool>}`
   entries.
2. WHEN `allowed: false` AND a `SmolAgent` enables that feature
   THEN the validating webhook SHALL reject the CR.

#### R-OP-FF-5 — Rolling feature gates use feature constant table
**User Story:** As a developer, I want one place to add a new
feature flag.

**Acceptance Criteria:**
1. ALL feature names SHALL be declared in a single Go constant
   table (`pkg/features/features.go`) referenced by the API types,
   webhook, and reconciler.

### R-OP-REC: Reconciliation

#### R-OP-REC-1 — Owner-ref discipline
**User Story:** As an operator, I want managed resources to be
garbage-collected when the CR is deleted.

**Acceptance Criteria:**
1. EVERY resource the operator creates SHALL set
   `metadata.ownerReferences` to the parent `SmolAgent` (or
   `SmolAgentPlatform`) with `controller: true`.
2. WHEN the parent CR is deleted THEN Kubernetes garbage collection
   SHALL remove all owned resources.

#### R-OP-REC-2 — Idempotent server-side apply
**User Story:** As an operator, I want reconciles to be safe to
repeat.

**Acceptance Criteria:**
1. ALL writes SHALL use `client.Apply` (server-side apply) with
   the manager's field manager identity.
2. CONCURRENT edits by other field managers (e.g. a human
   debugging) SHALL NOT cause infinite reconcile loops.

#### R-OP-REC-3 — Drift detection ≤ 30 s
**User Story:** As an SRE, I want manual edits to managed objects
healed promptly.

**Acceptance Criteria:**
1. WHEN any managed resource is mutated by another actor THEN the
   operator SHALL re-apply within 30 s P95.
2. CHURN of unrelated fields (e.g. defaulted annotations) SHALL
   NOT trigger reconciles.

#### R-OP-REC-4 — Per-feature reconcilers
**User Story:** As a developer, I want one reconciler per feature
so I can test it in isolation.

**Acceptance Criteria:**
1. THE main controller SHALL dispatch to a `FeatureReconciler`
   interface implementation per feature.
2. WHEN one feature reconciler returns an error THE other features
   SHALL still reconcile in the same loop.

### R-OP-WH: Admission Webhooks

#### R-OP-WH-1 — Validating webhook
**User Story:** As an admin, I want bad configs rejected at submit
time.

**Acceptance Criteria:**
1. THE validating webhook SHALL reject:
   - missing `spec.trustDomain`
   - `mode: insecure` without `annotations.smol-agents.stigen.ai/allow-insecure: "true"`
   - `runtimeClass: runc` without `sandbox.allowHostEscape: true`
   - feature combinations forbidden by `SmolAgentPlatform.spec.featurePolicy`

#### R-OP-WH-2 — Mutating webhook
**User Story:** As a tenant, I want sane defaults so I don't have
to write boilerplate.

**Acceptance Criteria:**
1. WHEN a field is unset THE mutating webhook SHALL fill in the
   default from `SmolAgentPlatform.spec.defaults` (or the
   compiled-in defaults if no platform CR exists).

#### R-OP-WH-3 — Conversion webhook
**User Story:** As a tenant, I want my v1 CR to keep working after
the operator upgrades to vN.

**Acceptance Criteria:**
1. THE operator SHALL implement conversion between every supported
   API version pair.
2. ROUND-TRIP conversion (`v1 → v2 → v1`) SHALL be the identity
   function for any value valid in v1.

### R-OP-OBS: Observability

#### R-OP-OBS-1 — Controller metrics
**User Story:** As an SRE, I want to observe operator health.

**Acceptance Criteria:**
1. THE operator SHALL expose Prometheus metrics:
   `controller_runtime_reconcile_total`,
   `controller_runtime_reconcile_errors_total`,
   `controller_runtime_reconcile_time_seconds`,
   plus custom `feature_enabled`, `feature_ready`, `feature_reconcile_errors_total`.

#### R-OP-OBS-2 — Events for state changes
**User Story:** As a tenant, I want `kubectl describe` to show
human-readable history.

**Acceptance Criteria:**
1. THE operator SHALL emit `Normal FeatureEnabled`,
   `Warning FeaturePrereqMissing`, `Normal RolloutPromoted`,
   `Warning RolloutPaused`, and `Warning ReconcileFailed` events.

### R-OP-SEC: Security

#### R-OP-SEC-1 — Least privilege RBAC
**User Story:** As a security admin, I want the operator to hold
only the permissions it needs.

**Acceptance Criteria:**
1. THE operator's ClusterRole SHALL be generated by Kubebuilder
   markers and include only the resource verbs the controllers
   actually call.
2. NO `*` verb SHALL appear in the ClusterRole.

#### R-OP-SEC-2 — Tenant isolation via namespace scoping
**User Story:** As an admin, I want tenants in different namespaces
to be isolated.

**Acceptance Criteria:**
1. `SmolAgent` SHALL be namespace-scoped.
2. THE operator SHALL refuse to manage resources outside the CR's
   namespace, except for cluster-scoped resources owned by
   `SmolAgentPlatform`.

### R-OP-ROLL: Rollouts

#### R-OP-ROLL-1 — Per-feature rollout policy
**User Story:** As a platform engineer, I want to canary a feature
to a subset of agents.

**Acceptance Criteria:**
1. EACH feature SHALL accept `rolloutPolicy: Immediate | Canary |
   Manual`.
2. WHEN `Canary` THE platform CR's `spec.canary.percent` selects
   what fraction of `SmolAgent` CRs (by hash of name) actually
   apply the change; the rest stay on the previous setting.

#### R-OP-ROLL-2 — Pause and resume
**User Story:** As an SRE, I want to pause rollouts during an
incident.

**Acceptance Criteria:**
1. SETTING `spec.rollout.paused: true` SHALL stop the operator
   from applying new changes; existing resources remain.
2. RESUMING SHALL drain the pending change queue.

### R-OP-VRF: Verification

#### R-OP-VRF-1 — Envtest coverage
**User Story:** As a maintainer, I want every reconciler unit-tested.

**Acceptance Criteria:**
1. EVERY feature reconciler SHALL have envtest cases for:
   `enabled=false → no resources`,
   `enabled=true,prereqs unmet → Conditions reflect`,
   `enabled=true,prereqs met → all resources reconciled`.

#### R-OP-VRF-2 — E2E test against kind + operator
**User Story:** As a maintainer, I want a single command that
proves the operator works end-to-end.

**Acceptance Criteria:**
1. `make test-e2e-operator` SHALL bring up a kind cluster, install
   the operator and CRDs, apply sample CRs, and assert each
   feature flag's effect is observable.

#### R-OP-VRF-3 — Formal lifecycle model
**User Story:** As a reviewer, I want a Quint model proving every
enabled feature eventually reaches Ready or surfaces a non-empty
Reason.

**Acceptance Criteria:**
1. `spec/quint/operator_lifecycle.qnt` SHALL define a
   `FeatureProgresses` invariant verified via
   `quint run --invariant=Safety`.

## Non-Functional Requirements

### Code Architecture
- Single binary `manager` built with Kubebuilder ≥ v4.5.
- One Go module shared with the rest of the repo
  (`github.com/stigen/smol-agents`); operator code lives under
  `operator/`.
- Reconciler logic delegates to the existing `pkg/...` libraries.

### Performance
- Reconcile p95 ≤ 500 ms steady state at 200 CRs.
- Operator memory ≤ 256 MiB at 200 CRs.
- Webhook P99 latency ≤ 50 ms.

### Reliability
- Operator deploys as a `Deployment` with 2 replicas + leader
  election.
- Crash loop within 60 s SHALL still allow the platform's data
  plane to keep running (operator is control-plane only).

### Compatibility
- Kubernetes ≥ 1.28.
- Kubebuilder ≥ 4.5.
- Helm chart and operator SHALL coexist for one minor version to
  ease migration.
