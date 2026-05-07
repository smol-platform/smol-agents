// Package metrics exposes Prometheus counters/gauges/histograms for
// the operator. Implements R-OP-OBS-1.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const subsystem = "knative_agents_operator"

var (
	// FeatureEnabled is 1 when a feature is enabled on a CR, 0 otherwise.
	FeatureEnabled = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Subsystem: subsystem,
			Name:      "feature_enabled",
			Help:      "1 if the feature is enabled on the CR, 0 otherwise.",
		},
		[]string{"namespace", "name", "feature"},
	)

	// FeatureReady is 1 when the operator has reconciled a feature into
	// its desired Ready state.
	FeatureReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Subsystem: subsystem,
			Name:      "feature_ready",
			Help:      "1 if the feature reports Ready, 0 otherwise.",
		},
		[]string{"namespace", "name", "feature"},
	)

	// FeatureReconcileErrors counts reconcile errors per feature.
	FeatureReconcileErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "feature_reconcile_errors_total",
			Help:      "Total number of reconcile errors per feature.",
		},
		[]string{"feature"},
	)

	// ReconcileDuration measures end-to-end reconcile latency.
	ReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Subsystem: subsystem,
			Name:      "reconcile_duration_seconds",
			Help:      "Reconcile loop duration.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"controller"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		FeatureEnabled,
		FeatureReady,
		FeatureReconcileErrors,
		ReconcileDuration,
	)
}

// Record updates the per-feature gauges + counter from a reconcile result.
// Pure — no controller-runtime types — so it's trivially unit-testable.
func Record(namespace, name, feature string, enabled, ready bool, err error) {
	bool01 := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	FeatureEnabled.WithLabelValues(namespace, name, feature).Set(bool01(enabled))
	FeatureReady.WithLabelValues(namespace, name, feature).Set(bool01(ready))
	if err != nil {
		FeatureReconcileErrors.WithLabelValues(feature).Inc()
	}
}

// Reset is a test helper.
func Reset() {
	FeatureEnabled.Reset()
	FeatureReady.Reset()
	FeatureReconcileErrors.Reset()
}
