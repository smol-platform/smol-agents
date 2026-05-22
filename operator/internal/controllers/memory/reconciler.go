package memory

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// MemoryRetrieverReconciler reconciles MemoryRetriever CRs into the data-plane
// and MCP-gateway infrastructure.
//
// For each MemoryRetriever it ensures:
//   - a ServiceAccount (mr-<name>-sa)
//   - a retrieval-worker Deployment + headless Service (mr-<name>-worker)
//   - a memory-mcp Deployment + ClusterIP Service (mr-<name>-mcp)
//
// Owner references on all four resources ensure cascade deletion when the
// MemoryRetriever is removed. The shared MemoryStore is never touched.
//
// Implements R-MEM-CTRL-1, R-MEM-API-3.
type MemoryRetrieverReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// WorkerImage overrides the default memory-worker container image.
	// Useful in tests; leave empty for the production default.
	WorkerImage string

	// MCPImage overrides the default memory-mcp container image.
	MCPImage string
}

// SetupWithManager wires the controller. Owns Deployment and Service so that
// changes to managed resources trigger a reconcile. ServiceAccount is also
// owned but we do not watch it for changes since it is rarely mutated after
// creation.
func (r *MemoryRetrieverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&amv1.MemoryRetriever{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Complete(r)
}

// Reconcile is the per-MemoryRetriever entrypoint. It is idempotent: every
// invocation drives the cluster toward the desired state regardless of how many
// times it runs.
func (r *MemoryRetrieverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("memoryretriever", req.NamespacedName)

	retriever := &amv1.MemoryRetriever{}
	if err := r.Get(ctx, req.NamespacedName, retriever); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	prev := retriever.Status // value copy for dirty-check

	// Validate the spec before doing any work.
	if err := pure.ValidateMemoryRetriever(retriever.Spec); err != nil {
		r.setCondition(retriever, "Ready", metav1.ConditionFalse, "InvalidSpec", err.Error())
		retriever.Status.Phase = "Degraded"
		return ctrl.Result{}, r.statusUpdateIfChanged(ctx, retriever, prev)
	}

	// Resolve the referenced MemoryStores. Missing stores are a recoverable
	// error — we stay Pending and requeue so the controller retries when the
	// store appears.
	stores, missing, err := r.resolveStores(ctx, retriever)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list stores: %w", err)
	}
	if len(missing) > 0 {
		msg := fmt.Sprintf("MemoryStore(s) not found: %v", missing)
		logger.Info("stores missing, staying Pending", "missing", missing)
		r.setCondition(retriever, "StoresBound", metav1.ConditionFalse, "StoreMissing", msg)
		r.setCondition(retriever, "Ready", metav1.ConditionFalse, "StoreMissing", msg)
		retriever.Status.Phase = "Pending"
		retriever.Status.BoundWorkers = 0
		return ctrl.Result{RequeueAfter: 15 * time.Second}, r.statusUpdateIfChanged(ctx, retriever, prev)
	}
	r.setCondition(retriever, "StoresBound", metav1.ConditionTrue, "AllBound",
		fmt.Sprintf("%d store(s) resolved", len(stores)))

	// Reconcile all owned resources; any error lands us in Degraded.
	if err := r.reconcileResources(ctx, retriever, stores); err != nil {
		r.setCondition(retriever, "Ready", metav1.ConditionFalse, "ApplyFailed", err.Error())
		retriever.Status.Phase = "Degraded"
		if uerr := r.statusUpdateIfChanged(ctx, retriever, prev); uerr != nil {
			return ctrl.Result{}, uerr
		}
		return ctrl.Result{}, err
	}

	retriever.Status.Phase = "Ready"
	retriever.Status.BoundWorkers = int32(len(stores))
	r.setCondition(retriever, "Ready", metav1.ConditionTrue, "Reconciled", "")
	logger.Info("memory retriever ready", "stores", len(stores))
	return ctrl.Result{}, r.statusUpdateIfChanged(ctx, retriever, prev)
}

// resolveStores fetches each MemoryStore named in the retriever's spec.
// Returns the resolved list, the names of missing stores, and any hard error.
func (r *MemoryRetrieverReconciler) resolveStores(
	ctx context.Context,
	retriever *amv1.MemoryRetriever,
) (resolved []*amv1.MemoryStore, missing []string, err error) {
	for _, name := range retriever.Spec.Stores {
		store := &amv1.MemoryStore{}
		key := types.NamespacedName{Namespace: retriever.Namespace, Name: name}
		if err := r.Get(ctx, key, store); err != nil {
			if apierrors.IsNotFound(err) {
				missing = append(missing, name)
				continue
			}
			return nil, nil, fmt.Errorf("get store %q: %w", name, err)
		}
		resolved = append(resolved, store)
	}
	return resolved, missing, nil
}

// reconcileResources creates or updates the four owned objects. It uses
// controllerutil.CreateOrUpdate so the operation is idempotent: an existing
// resource is updated in-place only when the spec has drifted.
func (r *MemoryRetrieverReconciler) reconcileResources(
	ctx context.Context,
	retriever *amv1.MemoryRetriever,
	stores []*amv1.MemoryStore,
) error {
	// ServiceAccount.
	sa := BuildServiceAccount(retriever)
	if err := r.ownAndApply(ctx, retriever, sa, func() error { return nil }); err != nil {
		return fmt.Errorf("serviceaccount: %w", err)
	}

	// Worker Deployment.
	workerDeploy := BuildWorkerDeployment(retriever, stores, r.WorkerImage)
	if err := r.ownAndApply(ctx, retriever, workerDeploy, func() error { return nil }); err != nil {
		return fmt.Errorf("worker deployment: %w", err)
	}

	// Worker Service (headless).
	workerSvc := BuildWorkerService(retriever)
	if err := r.ownAndApply(ctx, retriever, workerSvc, func() error { return nil }); err != nil {
		return fmt.Errorf("worker service: %w", err)
	}

	// MCP Deployment.
	mcpDeploy := BuildMCPDeployment(retriever, r.MCPImage)
	if err := r.ownAndApply(ctx, retriever, mcpDeploy, func() error { return nil }); err != nil {
		return fmt.Errorf("mcp deployment: %w", err)
	}

	// MCP Service.
	mcpSvc := BuildMCPService(retriever)
	if err := r.ownAndApply(ctx, retriever, mcpSvc, func() error { return nil }); err != nil {
		return fmt.Errorf("mcp service: %w", err)
	}

	return nil
}

// ownAndApply sets the controller reference on desired and then calls
// CreateOrUpdate. The mutateFn is applied inside CreateOrUpdate and may
// additionally mutate the live object before the update is issued.
func (r *MemoryRetrieverReconciler) ownAndApply(
	ctx context.Context,
	owner *amv1.MemoryRetriever,
	desired client.Object,
	mutateFn func() error,
) error {
	if err := ctrl.SetControllerReference(owner, desired, r.Scheme); err != nil {
		return fmt.Errorf("set controller ref on %T %s: %w",
			desired, desired.GetName(), err)
	}

	// CreateOrUpdate fetches the live object, calls mutateFn (which may
	// update spec fields on the live copy), then creates or updates.
	//
	// For Deployments and Services we perform a selective merge: we
	// overwrite Spec but preserve any fields the apiserver manages (e.g.
	// clusterIP for Services, resourceVersion). The generic path here just
	// uses the desired object as the mutation target — sufficient for our
	// simple, operator-owned resources.
	existing := desired.DeepCopyObject().(client.Object)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		// Merge desired spec into the live object.
		switch d := desired.(type) {
		case *appsv1.Deployment:
			live := existing.(*appsv1.Deployment)
			live.Spec = d.Spec
			live.Labels = d.Labels
		case *corev1.Service:
			live := existing.(*corev1.Service)
			// Preserve ClusterIP (immutable) and NodePort assignments.
			clusterIP := live.Spec.ClusterIP
			ports := live.Spec.Ports
			live.Spec = d.Spec
			if live.Spec.ClusterIP == "" {
				live.Spec.ClusterIP = clusterIP
			}
			if len(live.Spec.Ports) == 0 && len(ports) > 0 {
				live.Spec.Ports = ports
			}
			live.Labels = d.Labels
		case *corev1.ServiceAccount:
			live := existing.(*corev1.ServiceAccount)
			live.Labels = d.Labels
		}
		if mutateFn != nil {
			return mutateFn()
		}
		return nil
	})
	return err
}

// setCondition upserts a metav1.Condition-style entry in the retriever's
// status, preserving LastTransitionTime when the status is unchanged.
//
// Note: MemoryRetrieverStatus uses the pure MemoryCondition type (no
// metav1.ConditionStatus) so we store the status as a plain string.
func (r *MemoryRetrieverReconciler) setCondition(
	retriever *amv1.MemoryRetriever,
	condType string,
	status metav1.ConditionStatus,
	reason, msg string,
) {
	now := metav1.Now().UTC().Format(time.RFC3339)
	c := pure.MemoryCondition{
		Type:               condType,
		Status:             string(status),
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
	}
	for i, existing := range retriever.Status.Conditions {
		if existing.Type == c.Type {
			if existing.Status == c.Status {
				c.LastTransitionTime = existing.LastTransitionTime
			}
			retriever.Status.Conditions[i] = c
			retriever.Status.ObservedGeneration = retriever.Generation
			return
		}
	}
	retriever.Status.Conditions = append(retriever.Status.Conditions, c)
	retriever.Status.ObservedGeneration = retriever.Generation
}

// statusUpdateIfChanged skips the API write when status is byte-identical,
// mirroring the pattern in AgentNetworkReconciler.
func (r *MemoryRetrieverReconciler) statusUpdateIfChanged(
	ctx context.Context,
	retriever *amv1.MemoryRetriever,
	prev pure.MemoryRetrieverStatus,
) error {
	if equality.Semantic.DeepEqual(prev, retriever.Status) {
		return nil
	}
	return r.Status().Update(ctx, retriever)
}
