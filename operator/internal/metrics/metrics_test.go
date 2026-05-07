package metrics

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecord_GaugesAndCounter(t *testing.T) {
	Reset()
	Record("ns", "alice", "identity", true, true, nil)
	if v := testutil.ToFloat64(FeatureEnabled.WithLabelValues("ns", "alice", "identity")); v != 1 {
		t.Errorf("enabled=%v, want 1", v)
	}
	if v := testutil.ToFloat64(FeatureReady.WithLabelValues("ns", "alice", "identity")); v != 1 {
		t.Errorf("ready=%v, want 1", v)
	}

	// Disabled feature.
	Record("ns", "bob", "ebpf", false, false, nil)
	if v := testutil.ToFloat64(FeatureEnabled.WithLabelValues("ns", "bob", "ebpf")); v != 0 {
		t.Errorf("disabled enabled=%v, want 0", v)
	}

	// Error increments the counter.
	Record("ns", "alice", "ebpf", true, false, errors.New("boom"))
	if v := testutil.ToFloat64(FeatureReconcileErrors.WithLabelValues("ebpf")); v != 1 {
		t.Errorf("errors=%v, want 1", v)
	}
	Record("ns", "alice", "ebpf", true, false, errors.New("boom2"))
	if v := testutil.ToFloat64(FeatureReconcileErrors.WithLabelValues("ebpf")); v != 2 {
		t.Errorf("errors=%v, want 2", v)
	}
}

func TestRecord_NoErrorDoesNotIncrement(t *testing.T) {
	Reset()
	Record("ns", "x", "identity", true, true, nil)
	if v := testutil.ToFloat64(FeatureReconcileErrors.WithLabelValues("identity")); v != 0 {
		t.Errorf("errors=%v, want 0", v)
	}
}
