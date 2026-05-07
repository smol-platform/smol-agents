// Package features hosts one FeatureReconciler implementation per
// platform feature. The top-level controller dispatches to each in turn
// and folds the returned FeatureResult into Status.
package features

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/stigen/knative-agents/operator/api/v1"
	"github.com/stigen/knative-agents/operator/pkg/features"
)

// Result aliases the canonical features.Result so reconciler bodies stay
// short.
type Result = features.Result

// FeatureReconciler is implemented once per Feature. Implementations
// MUST be deterministic: given the same CR and platform, the same set
// of objects is produced and the same FeatureResult is returned.
type FeatureReconciler interface {
	Name() features.Feature

	// Reconcile evaluates prerequisites, builds owned objects, and
	// returns a Result. The returned Owned slice is applied via SSA by
	// the dispatcher; reconcilers MUST NOT call client themselves
	// except to read prereqs (e.g. RuntimeClass existence).
	Reconcile(ctx context.Context, env Env) (Result, []client.Object, error)
}

// Env is what the dispatcher injects into each reconciler. It carries
// only what reconcilers genuinely need; nothing else, to keep the unit
// surface small.
type Env struct {
	CR       *v1.KnativeAgent
	Platform *v1.KnativeAgentPlatform
	Reader   client.Reader   // for reading prereqs only; nil-ok in unit tests
	Scheme   *runtime.Scheme // for SetControllerReference
}
