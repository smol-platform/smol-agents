package controllers

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
)

// FeatureReconcilerLite mirrors features.FeatureReconciler without the
// import to avoid a cycle: the features package imports this package
// for FeatureResult, so reconcilers must conform structurally.
type FeatureReconcilerLite interface {
	Name() any
	Reconcile(ctx context.Context, env any) (FeatureResult, []client.Object, error)
}

// DispatchEnv is the bag of dependencies passed to each feature
// reconciler. We define it here so both the dispatcher and the
// features sub-package can reference it without cycles via interface
// shape.
type DispatchEnv struct {
	CR       *v1.SmolAgent
	Platform *v1.SmolAgentPlatform
	Reader   client.Reader
	Scheme   *runtime.Scheme
}

// DispatchResult bundles the output of one feature dispatch — the
// aggregator-ready FeatureResult and the owned objects to apply.
type DispatchResult struct {
	Result FeatureResult
	Owned  []client.Object
	Err    error
}

// Dispatch is a tiny helper that lets a top-level controller iterate over
// reconcilers regardless of where they live, calling each one and
// translating its (Result, Owned, Err) tuple back into DispatchResult.
//
// Concrete reconcilers (in the features package) are wrapped through
// the small adapter ReconcilerFunc.
type ReconcilerFunc struct {
	Run func(ctx context.Context, env DispatchEnv) (FeatureResult, []client.Object, error)
}

// Dispatch invokes the function and returns a DispatchResult.
func (f ReconcilerFunc) Dispatch(ctx context.Context, env DispatchEnv) DispatchResult {
	res, owned, err := f.Run(ctx, env)
	return DispatchResult{Result: res, Owned: owned, Err: err}
}
