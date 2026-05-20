# Tasks — smol-agents-operator

Each task references the requirement IDs it satisfies (`R-OP-*`,
`R-IDN-*`, etc.) and the file(s) it will create/modify. Tasks are
ordered to keep main green at every step: API types → builders →
controllers → webhooks → wiring → e2e.

## Phase 1 — Scaffolding

- [x] 1. Add Kubebuilder scaffolding under `operator/`
  - File: `operator/PROJECT`, `operator/Dockerfile`, `operator/Makefile`,
    `operator/cmd/manager/main.go`
  - Action: `kubebuilder init --domain stigen.ai --repo
    github.com/stigen/smol-agents/operator --multigroup=false`
  - _Requirements: R-OP-API-1, R-OP-API-2_

- [x] 2. Generate API skeletons for `SmolAgent` and
  `SmolAgentPlatform`
  - Files: `operator/api/v1/smolagent_types.go`,
    `operator/api/v1/smolagentplatform_types.go`
  - Action: `kubebuilder create api --group agents --version v1 --kind SmolAgent`
    then `--kind SmolAgentPlatform --resource --controller`
  - _Requirements: R-OP-API-1, R-OP-API-2_

- [x] 3. Author feature constants table
  - File: `operator/pkg/features/features.go`,
    `operator/pkg/features/features_test.go`
  - Define `Feature` enum + `All()` + `ConditionType()`.
  - _Requirements: R-OP-FF-5_

## Phase 2 — API surface

- [x] 4. Define `Features` struct + per-feature config types
  - File: `operator/api/v1/features.go`
  - Add Kubebuilder markers (`+kubebuilder:validation:*`,
    `+kubebuilder:default:=...`).
  - _Requirements: R-OP-FF-1, R-OP-FF-2_

- [x] 5. Define `Status` types with conditions per feature
  - File: `operator/api/v1/status.go`
  - Add helpers `SetFeatureCondition(cr, feature, status, reason, msg)`.
  - _Requirements: R-OP-FF-3_

- [x] 6. Add printer columns + short names + categories
  - File: `operator/api/v1/smolagent_types.go` annotations
  - Columns: `MODE`, `READY`, `IDENTITY`, `MTLS`, `EBPF`, `SECRETS`,
    `RUNTIMECLASS`, `AGE`.
  - _Requirements: R-OP-API-1_

- [x] 7. Generate manifests + DeepCopy via `make generate manifests`
  - Verify CRD YAMLs in `operator/config/crd/bases/`.
  - _Requirements: R-OP-API-3_

## Phase 3 — Builders (pure functions)

- [x] 8. `BuildAgentConfigMap(spec)` from `pkg/config` shape
  - File: `operator/internal/builders/configmap.go`,
    `..._test.go` (golden fixtures)
  - _Requirements: R-IDN-*, R-MTL-*, R-SEC-*_

- [x] 9. `BuildClusterSPIFFEID(spec)` for identity feature
  - File: `operator/internal/builders/spiffeid.go`
  - _Requirements: R-IDN-1, R-IDN-3_

- [x] 10. `BuildSecretProxyConfig(spec)` and sidecar container
  - File: `operator/internal/builders/secretproxy.go`
  - _Requirements: R-SEC-1, R-SEC-2, R-SEC-3_

- [x] 11. `BuildEBPFLoaderDaemonSet(spec, platform)` with all 7 distro
  presets ported to Go
  - File: `operator/internal/builders/ebpfloader.go`
  - _Requirements: R-EBP-1, R-EBP-2_

- [x] 12. `BuildKnativeService(spec)` / `BuildDeployment(spec)` /
  `BuildStatefulSet(spec)` based on `mode`
  - File: `operator/internal/builders/workload.go`
  - _Requirements: R-DEP-1, R-DEP-2, R-SBX-1_

- [x] 13. `BuildRuntimeClass(spec)` if missing
  - File: `operator/internal/builders/runtimeclass.go`
  - _Requirements: R-SBX-1_

## Phase 4 — Per-feature reconcilers

- [x] 14. `IdentityReconciler`
  - File: `operator/internal/controllers/features/identity.go`,
    `..._test.go` (envtest)
  - Prereqs check: SPIFFE CRD `clusterspiffeids.spire.spiffe.io` installed.
  - _Requirements: R-OP-FF-3, R-OP-REC-4, R-IDN-*_

- [x] 15. `TransportReconciler` (private + public sub-features)
  - File: `operator/internal/controllers/features/transport.go`,
    `..._test.go`
  - _Requirements: R-MTL-1, R-MTL-2_

- [x] 16. `SecretsReconciler`
  - File: `operator/internal/controllers/features/secrets.go`
  - _Requirements: R-SEC-*_

- [x] 17. `SandboxReconciler`
  - File: `operator/internal/controllers/features/sandbox.go`
  - Prereqs check: RuntimeClass exists OR can be created.
  - _Requirements: R-SBX-1_

- [x] 18. `EBPFReconciler`
  - File: `operator/internal/controllers/features/ebpf.go`
  - Renders DaemonSet using preset selected by platform CR.
  - Prereqs: kernel-feature ConfigMap published by previous run, OR
    optimistic apply with degraded condition until first probe.
  - _Requirements: R-EBP-1, R-EBP-2_

- [x] 19. `KnativeReconciler`
  - File: `operator/internal/controllers/features/knative.go`
  - Prereqs: Knative `serving.knative.dev` CRDs.
  - _Requirements: R-DEP-1_

- [x] 20. `ObservabilityReconciler`
  - File: `operator/internal/controllers/features/observability.go`
  - _Requirements: usability_

## Phase 5 — Top-level controller

- [x] 21. Implement `SmolAgentReconciler` dispatching to features
  - File: `operator/internal/controllers/smolagent_controller.go`
  - Wire: cache, owner refs, SSA via `client.Apply`.
  - _Requirements: R-OP-REC-1, R-OP-REC-2_

- [x] 22. Implement aggregation: per-feature condition → `Status.Phase`
  - File: `operator/internal/controllers/aggregator.go`
  - _Requirements: R-OP-FF-3_

- [x] 23. Implement `SmolAgentPlatformReconciler`
  - File: `operator/internal/controllers/platform_controller.go`
  - Manages: ebpf-loader DaemonSet, RuntimeClass.
  - _Requirements: R-OP-API-2_

- [x] 24. Implement Drift watcher → trigger reconcile
  - File: `operator/internal/controllers/watch.go`
  - Owns: ConfigMap, Service, Deployment, DaemonSet, ClusterSPIFFEID,
    Knative Service.
  - _Requirements: R-OP-REC-3_

## Phase 6 — Webhooks

- [x] 25. Validating webhook
  - File: `operator/internal/webhooks/smolagent_webhook.go`
  - Tests: `..._test.go` covering each rejection rule.
  - _Requirements: R-OP-WH-1_

- [x] 26. Mutating webhook (defaulting from platform CR)
  - File: `operator/internal/webhooks/smolagent_defaulter.go`
  - _Requirements: R-OP-WH-2_

- [ ] 27. Conversion webhook scaffolding (no-op v1 → v1 today)
  - File: `operator/internal/webhooks/smolagent_conversion.go`
  - _Requirements: R-OP-WH-3_

## Phase 7 — Observability + Security

- [x] 28. Custom Prometheus metrics
  - File: `operator/internal/metrics/metrics.go`
  - `feature_enabled`, `feature_ready`, `feature_reconcile_errors_total`.
  - _Requirements: R-OP-OBS-1_

- [x] 29. Event recorder helpers
  - File: `operator/internal/events/events.go`
  - _Requirements: R-OP-OBS-2_

- [x] 30. RBAC manifests via Kubebuilder markers (no `*` verbs)
  - File: `operator/config/rbac/role.yaml` (generated)
  - _Requirements: R-OP-SEC-1_

## Phase 8 — Verification

- [ ] 31. Per-feature envtest cases
  - File: `operator/internal/controllers/features/<feature>_test.go`
  - _Requirements: R-OP-VRF-1_

- [ ] 32. End-to-end test against kind
  - File: `operator/test/e2e/operator_e2e_test.go`,
    `Makefile target test-e2e-operator`
  - _Requirements: R-OP-VRF-2_

- [x] 33. Quint model for operator lifecycle
  - File: `spec/quint/operator_lifecycle.qnt`
  - Invariants: `FeatureProgresses`, `OwnerSafety`,
    `ReadyImpliesAllEnabledReady`, `NoForbiddenFeatures`.
  - _Requirements: R-OP-VRF-3_

- [x] 34. Property tests for feature flag round-trip
  - File: `operator/api/v1/features_property_test.go`
  - rapid: marshal → unmarshal → equal.
  - _Requirements: R-OP-FF-5_

## Phase 9 — Packaging + Migration

- [x] 35. Operator container image + Dockerfile
  - File: `operator/Dockerfile`, `deploy/docker/operator.Dockerfile`
  - _Requirements: packaging_

- [x] 36. Operator install manifests
  - File: `operator/config/default/kustomization.yaml`
  - `make deploy` produces a single yaml that installs everything.

- [x] 37. Sample CRs
  - Files: `operator/config/samples/smolagent_minimal.yaml`,
    `..._everything.yaml`
  - Used by e2e tests and INSTALL.md.

- [ ] 38. Migration guide chapter in INSTALL.md
  - Append `## 11. Migrating from Helm to Operator` to
    `docs/INSTALL.md`.

- [x] 39. CI matrix update
  - File: `.github/workflows/ci.yaml`
  - Add `operator-build`, `operator-envtest`, `operator-e2e` jobs.

## Validation Matrix (operator-side)

| Requirement      | Code reference                                     | Test reference                                       |
|------------------|----------------------------------------------------|------------------------------------------------------|
| R-OP-API-1       | operator/api/v1/smolagent_types.go              | apply + kubectl get printer columns                  |
| R-OP-API-2       | operator/api/v1/smolagentplatform_types.go      | webhook test: 2nd platform CR rejected               |
| R-OP-API-3       | kubebuilder markers                                | kubectl explain smolagent.spec.features.identity  |
| R-OP-FF-1        | api/v1/features.go                                 | envtest each feature on/off                          |
| R-OP-FF-2        | api/v1/features.go enums                           | webhook tests: invalid enum rejected                 |
| R-OP-FF-3        | controllers/aggregator.go                          | envtest reads Conditions                             |
| R-OP-FF-4        | webhooks/smolagent_webhook.go                   | webhook test: forbidden feature rejected             |
| R-OP-FF-5        | pkg/features/features.go                           | property test                                        |
| R-OP-REC-1       | controllers/smolagent_controller.go             | envtest: deletion cascades                           |
| R-OP-REC-2       | controllers/* uses client.Apply                    | envtest: human edit ignored                          |
| R-OP-REC-3       | controllers/watch.go                               | e2e: edit, observe heal time                         |
| R-OP-REC-4       | controllers/features/*                             | envtest: one feature failure → others succeed        |
| R-OP-WH-1        | webhooks/smolagent_webhook.go                   | unit + envtest                                       |
| R-OP-WH-2        | webhooks/smolagent_defaulter.go                 | unit                                                 |
| R-OP-WH-3        | webhooks/smolagent_conversion.go                | property round-trip                                  |
| R-OP-OBS-1       | internal/metrics/metrics.go                        | unit + scrape test                                   |
| R-OP-OBS-2       | internal/events/events.go                          | envtest                                              |
| R-OP-SEC-1       | config/rbac/role.yaml (generated)                  | grep '\*' fails                                      |
| R-OP-SEC-2       | controllers/* checks namespace                     | envtest: cross-ns refused                            |
| R-OP-ROLL-1      | api/v1/features.go RolloutPolicy                   | envtest                                              |
| R-OP-ROLL-2      | controllers/smolagent_controller.go             | envtest: paused = no apply                           |
| R-OP-VRF-1       | features/<feature>_test.go                         | envtest                                              |
| R-OP-VRF-2       | test/e2e/operator_e2e_test.go                      | make test-e2e-operator                               |
| R-OP-VRF-3       | spec/quint/operator_lifecycle.qnt                  | quint run --invariant=Safety                         |
