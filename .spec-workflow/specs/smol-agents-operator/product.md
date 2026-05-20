# Product Overview — smol-agents-operator

## Product Purpose

`smol-agents-operator` is a Kubebuilder-built controller manager that
replaces the static Helm chart with a declarative, feature-flagged
Kubernetes API. Tenants describe what they want — identity, transport,
sandbox, eBPF, secrets, Knative — as fields on a `SmolAgent` custom
resource, and the operator reconciles the underlying primitives
(Deployment / StatefulSet / Knative Service, DaemonSet, ClusterSPIFFEID,
ConfigMap, secret-proxy sidecar, RuntimeClass) into the cluster.

The operator inherits every guarantee of the platform's hand-installed
form: gVisor sandboxing (R-SBX-1), SPIFFE workload identity (R-IDN-*),
dual-rail mTLS (R-MTL-*), kloak-style secret broker (R-SEC-*), the host
ebpf-loader DaemonSet (R-EBP-*), and the formal lifecycle invariants
(R-RUN-*). It adds a control-plane life-cycle: every feature is a
*flag* that can be turned on, off, or upgraded independently, with an
auditable Status condition per feature reflecting reality.

## Target Users

- **Platform engineers** who want one-CR-per-tenant instead of
  `helm install` per tenant.
- **Security engineers** who need GitOps-friendly drift detection
  (the operator continuously reconciles, so divergence is auto-healed
  or flagged).
- **Product teams** who want to enable a single capability (e.g.
  "give me identity + private mTLS but not yet eBPF") without forking
  values.yaml.
- **SREs** running fleet-scale upgrades, who want canaries,
  feature-flagged rollouts, and rollback per capability.

## Key Features

1. **`SmolAgent` namespaced CR** — describes a single agent
   workload. Fields mirror `values.yaml` but are typed and validated.
2. **`SmolAgentPlatform` cluster CR** — describes cluster-wide
   defaults (RuntimeClass, ebpf-loader DaemonSet, default trust
   domain) that one or more `SmolAgent` CRs inherit.
3. **Per-feature flags with status conditions** — every capability
   (identity, transport.private, transport.public, secrets, sandbox,
   ebpf, knative, observability) is a `Feature` with `enabled`,
   `mode`, optional `config`, and a matching `Status.Conditions`
   entry the operator updates on every reconcile.
4. **Feature gates with safe-by-default semantics** — a feature is
   enabled only if its prerequisites (CRDs, RuntimeClass, host
   capabilities) are satisfied. Otherwise the operator surfaces a
   `FeaturePrerequisitesUnmet` condition rather than silently
   degrading.
5. **Progressive rollout primitives** — each `SmolAgent` carries a
   `spec.rollout` block: canary percentage, paused state, and a
   per-feature `rolloutPolicy` (`Immediate` | `Canary` | `Manual`).
6. **Conversion webhooks** — vN ⇆ vN+1 conversion lets tenants stay
   on stable APIs across operator upgrades.
7. **Validating + mutating admission webhooks** — block insecure
   combinations (e.g., `mode: insecure` without
   `SMOL_AGENTS_ALLOW_INSECURE=true` annotation) at submit time
   instead of at reconcile time.
8. **OpenAPI v3 + kubectl plumbing** — printer columns, short names,
   `kubectl explain` documentation per field.

## Business Objectives

- **Reduce per-tenant onboarding friction**: today an operator
  `helm install`s a chart; with the operator, a tenant submits a
  ~30-line YAML.
- **Enable feature canaries**: roll a new feature (say, public mTLS)
  to 5% of agents, observe, then promote — without forking a chart
  per feature.
- **Surface drift**: the operator's status conditions become the
  single source of truth for "is this tenant healthy?" — replacing
  ad-hoc `kubectl get pods -l ...` queries.
- **Make GitOps-native deployments first-class**: ArgoCD / Flux can
  reconcile against `SmolAgent` CRs without dealing with chart
  values templating.

## Success Metrics

- **API stability**: no breaking changes within a major version;
  conversion webhooks bridge minor versions.
- **Reconcile p95**: ≤ 500 ms for a no-op reconcile (steady state)
  on a cluster with 200 `SmolAgent` CRs.
- **Drift heal time**: ≤ 30 s P95 from manual edit of a managed
  resource to operator restoration.
- **Feature flag granularity**: every requirement R-IDN/MTL/SBX/EBP/
  SEC/RUN/DEP/VRF maps to *exactly one* feature flag in the API.
- **Test coverage**: every feature flag toggled on AND off in
  envtest; conversion webhook covered by round-trip property tests.

## Product Principles

1. **Feature-flagged from day one** — no field is required beyond
   `metadata` and `spec.trustDomain`. Every feature defaults to a
   safe state.
2. **Reality matches Spec via Conditions** — each feature gets one
   `Status.Conditions` entry with `Type=<Feature>Ready`,
   `Reason=<Reason>`, and `Message=` explaining drift.
3. **No silent fallbacks** — if a prerequisite is missing, surface
   `FeaturePrerequisitesUnmet` instead of running degraded.
4. **Reuse the existing Go packages** — the operator is a thin
   reconcile loop on top of the validated `pkg/identity`,
   `pkg/transport`, `pkg/secrets`, `pkg/ebpfloader`, etc. Library
   code is unchanged.
5. **Verifiable**: the operator's reconcile semantics are modelled
   in Quint (extends `spec/quint/agent_lifecycle.qnt`) so we can
   prove that "every enabled feature eventually reaches Ready, or
   `Status.Reason` is non-empty".

## Monitoring & Visibility

- **`kubectl get smolagents -A -o wide`** — printer columns:
  `MODE`, `READY`, `IDENTITY`, `MTLS`, `EBPF`, `SECRETS`,
  `RUNTIMECLASS`, `AGE`.
- **`kubectl describe smolagent <name>`** — per-feature
  Conditions, including the prerequisites the operator checked.
- **Prometheus metrics** — controller-runtime built-ins plus custom
  `feature_enabled{feature=…}`, `feature_ready{feature=…}`,
  `reconcile_errors_total{feature=…}`, `reconcile_duration_seconds`.
- **OTel traces** — every reconcile is a trace; child spans per
  feature.
- **Events**: `Normal FeatureEnabled`, `Warning FeaturePrereqMissing`,
  `Normal RolloutPromoted`, `Warning RolloutPaused`.

## Future Vision

- **`SmolAgentSet`** (analogous to `Deployment` over `Pod`) for
  rolling fleets of identical agents with managed canaries.
- **`AgentPolicy` CR** for cluster-wide allow-lists of features
  tenants are *permitted* to enable.
- **Federated SPIFFE** controlled via `spec.features.identity.federation`.
- **Operator Hub / OLM bundle** for one-click cluster install on
  OpenShift and the OperatorHub catalog.
- **`knactl`** companion CLI that wraps `kubectl` with feature-aware
  commands (`knactl features enable mtls --canary 10%`).
