// Package operator hosts the Kubebuilder-built control plane for the
// knative-agents platform.
//
// The operator watches two CRs:
//
//   - KnativeAgent (namespaced) — describes a tenant agent's identity,
//     transport, secret broker, eBPF, sandbox, Knative deployment, and
//     observability features. Each feature is independently flagged.
//   - KnativeAgentPlatform (cluster) — declares cluster-wide defaults,
//     the ebpf-loader DaemonSet config, and a feature allow/deny policy.
//
// Each feature reconciler returns a FeatureResult that the top-level
// controller folds into per-feature Status.Conditions. The reconciler
// loop never crashes the data plane: a feature failure is surfaced in
// status, never propagated.
package operator
