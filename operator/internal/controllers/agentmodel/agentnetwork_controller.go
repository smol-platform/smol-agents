package agentmodel

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// secretRequeueInterval replaces the Secret informer watch removed by
// knative-agents-5jy (the manager no longer caches/watches Secrets
// cluster-wide — RBAC only grants `get`, see operator/config/rbac/role.yaml).
// A CR parked in a missing-secret status requeues itself on this cadence
// instead of hours-later default-resync, so it still self-heals shortly
// after the referenced Secret is created. Matches the existing
// "SecretMissing" requeue interval used by AgentSessionReconciler
// (agentsession_controller.go writeStatus(..., "SecretMissing", ..., 10*time.Second)).
const secretRequeueInterval = 10 * time.Second

// AgentNetworkReconciler validates an AgentNetwork CR, resolves the
// secrets it references (so callers fail fast if a key is missing),
// counts how many Agents in the namespace match the selector, and
// reports Status.Phase. It does NOT inject sidecars or program egress.
//
// IMPORTANT (v0.2.0): AgentNetwork is NOT wired on any datapath. The AgentRun
// reconciler does not read AgentNetworks (zero references in
// agentrun_controller.go / builders/agentrun.go), so per-resource egress
// allow-lists never reach run pods; run-pod egress is the static default-deny
// NetworkPolicy in builders/run_sandbox.go, which ignores AgentNetwork. eBPF
// maps are programmed only by cmd/ebpf-probe (e2e). The prior claim that "the
// AgentRun reconciler reads bound AgentNetworks and renders them (R-AN-PROXY-3)"
// was aspirational and is not implemented — see
// docs/design/agentnetwork-agentpolicy-interaction.md.
type AgentNetworkReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager wires the controller. There is deliberately no Watches
// on Secrets (knative-agents-5jy: that would require a cluster-wide Secret
// informer, which the manager's cache no longer holds — see main.go and
// secretRequeueInterval); the `Pending: SecretMissing → Ready` transition
// instead self-heals via the periodic requeue set on that status path.
// Watching DynamicCredentialBackends does the equivalent job for the
// cross-object credential alignment check (knative-agents-13s): when a
// referenced backend appears or gains a scope mapping, the consuming
// AgentNetwork re-reconciles and flips Pending/BackendMissing → Ready.
func (r *AgentNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&amv1.AgentNetwork{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&amv1.DynamicCredentialBackend{}, handler.EnqueueRequestsFromMapFunc(r.backendToAgentNetworks)).
		Complete(r)
}

// backendToAgentNetworks maps a DynamicCredentialBackend event to the
// AgentNetworks in the same namespace whose identityProxy resources inject a
// credential with this backend's credentialName — so the cross-object alignment
// check (knative-agents-13s) re-runs when the backend is created, deleted, or
// its scopePermissions change.
func (r *AgentNetworkReconciler) backendToAgentNetworks(ctx context.Context, obj client.Object) []reconcile.Request {
	backend, ok := obj.(*amv1.DynamicCredentialBackend)
	if !ok {
		return nil
	}
	credName := backend.Spec.CredentialName
	list := &amv1.AgentNetworkList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		an := &list.Items[i]
		if an.Spec.IdentityProxy == nil {
			continue
		}
		for _, res := range an.Spec.IdentityProxy.Resources {
			if res.Credential != nil && res.Credential.Name == credName {
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
					Namespace: an.Namespace, Name: an.Name,
				}})
				break
			}
		}
	}
	return reqs
}

// Reconcile is the per-AgentNetwork entrypoint.
func (r *AgentNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agentnetwork", req.NamespacedName)

	an := &amv1.AgentNetwork{}
	if err := r.Get(ctx, req.NamespacedName, an); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	prev := an.Status // value copy — Status is primitives only

	if err := pure.ValidateAgentNetwork(an.Spec); err != nil {
		r.setStatus(an, "Failed", "InvalidSpec", err.Error())
		return ctrl.Result{}, r.statusUpdateIfChanged(ctx, an, prev)
	}

	// Per-kind status fields.
	switch an.Spec.Kind {
	case pure.NetworkIdentityProxy:
		an.Status.ProxyResourceCount = int32(len(an.Spec.IdentityProxy.Resources))
		an.Status.WGPeerCount = 0
	case pure.NetworkWireGuardMesh:
		an.Status.WGPeerCount = int32(len(an.Spec.WireGuardMesh.Peers))
		an.Status.ProxyResourceCount = 0

		// WireGuard requires the broker private key — fail fast if
		// the Secret is missing rather than waiting until the agent
		// boots and crashloops on Start().
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: an.Namespace,
			Name:      an.Spec.WireGuardMesh.PrivateKeyRef.SecretName,
		}, secret)
		if apierrors.IsNotFound(err) {
			r.setStatus(an, "Pending", "SecretMissing",
				fmt.Sprintf("secret %q not found", an.Spec.WireGuardMesh.PrivateKeyRef.SecretName))
			return ctrl.Result{RequeueAfter: secretRequeueInterval}, r.statusUpdateIfChanged(ctx, an, prev)
		}
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Cross-object credential alignment (knative-agents-13s): every
	// identityProxy resource that injects a broker-minted credential references a
	// DynamicCredentialBackend by credentialName and requests a scope the backend
	// must map to permissions. A missing backend or an unmapped scope produces a
	// runtime mint failure that crash-loops the broker sidecar (the same class of
	// bug c5r.22 closed intra-object); surface it here on the consuming
	// AgentNetwork instead. Consumer-side keeps a shared backend's status clean.
	if an.Spec.Kind == pure.NetworkIdentityProxy {
		phase, reason, msg, ok, err := r.checkCredentialAlignment(ctx, an)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ok {
			r.setStatus(an, phase, reason, msg)
			return ctrl.Result{}, r.statusUpdateIfChanged(ctx, an, prev)
		}
	}

	// Empty selector binds zero (network available but unbound) per R-AN-API-2.
	bound := int32(0)
	if len(an.Spec.AgentSelector) > 0 {
		agents := &amv1.AgentList{}
		if err := r.List(ctx, agents,
			client.InNamespace(an.Namespace),
			client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(an.Spec.AgentSelector)},
		); err != nil {
			return ctrl.Result{}, fmt.Errorf("list agents: %w", err)
		}
		bound = int32(len(agents.Items))
	}
	an.Status.BoundAgents = bound

	r.setStatus(an, "Ready", "Reconciled", "")
	logger.Info("agentnetwork ready",
		"kind", an.Spec.Kind,
		"bound", bound,
		"resources", an.Status.ProxyResourceCount,
		"peers", an.Status.WGPeerCount,
	)
	return ctrl.Result{}, r.statusUpdateIfChanged(ctx, an, prev)
}

// checkCredentialAlignment validates each identityProxy resource that injects a
// credential against the DynamicCredentialBackends in the namespace. It returns
// ok=true when every credential aligns; otherwise it returns the status to set:
//
//   - a referenced credentialName with no backend in-namespace is a (possibly
//     creation-ordered) missing dependency → Pending/BackendMissing, which
//     self-heals when the backend appears (the DCB watch requeues us);
//   - a backend that exists but does not map the requested scope is a genuine
//     misalignment that fails the mint → Failed/InvalidSpec, naming the field.
//
// Grant-existence (grant.scope + grant.principal↔Agent SVID) is deferred — it
// needs the operator trust domain and the bound Agents' SVIDs (knative-agents-13s).
// A transient list error is returned so the caller requeues rather than
// reporting a status computed from an incomplete backend view.
func (r *AgentNetworkReconciler) checkCredentialAlignment(ctx context.Context, an *amv1.AgentNetwork) (phase, reason, msg string, ok bool, err error) {
	if an.Spec.IdentityProxy == nil {
		return "", "", "", true, nil
	}
	needsCredential := false
	for _, res := range an.Spec.IdentityProxy.Resources {
		if res.Credential != nil {
			needsCredential = true
			break
		}
	}
	if !needsCredential {
		return "", "", "", true, nil
	}

	backends := &amv1.DynamicCredentialBackendList{}
	if err := r.List(ctx, backends, client.InNamespace(an.Namespace)); err != nil {
		return "", "", "", false, fmt.Errorf("list dynamiccredentialbackends: %w", err)
	}
	byName := make(map[string]*amv1.DynamicCredentialBackend, len(backends.Items))
	for i := range backends.Items {
		b := &backends.Items[i]
		byName[b.Spec.CredentialName] = b
	}

	for i, res := range an.Spec.IdentityProxy.Resources {
		if res.Credential == nil {
			continue
		}
		cred := res.Credential
		backend, found := byName[cred.Name]
		if !found {
			return "Pending", "BackendMissing", fmt.Sprintf(
				"resources[%d].credential.name %q has no DynamicCredentialBackend in namespace %q",
				i, cred.Name, an.Namespace), false, nil
		}
		if backend.Spec.GitHubApp == nil {
			continue // non-githubApp backend has no scope map to validate against
		}
		if _, mapped := backend.Spec.GitHubApp.ScopePermissions[cred.Scope]; !mapped {
			return "Failed", "InvalidSpec", fmt.Sprintf(
				"resources[%d].credential.scope %q is not mapped by backend %q "+
					"(not a key of its githubApp.scopePermissions — the mint would fail)",
				i, cred.Scope, cred.Name), false, nil
		}
	}
	return "", "", "", true, nil
}

func (r *AgentNetworkReconciler) setStatus(an *amv1.AgentNetwork, phase, reason, msg string) {
	an.Status.Phase = phase
	an.Status.Reason = reason
	an.Status.Message = msg
	an.Status.ObservedGeneration = an.Generation
}

// statusUpdateIfChanged skips the API write when status is byte-identical.
// Saves apiserver load on requeues that don't actually change anything.
func (r *AgentNetworkReconciler) statusUpdateIfChanged(ctx context.Context, an *amv1.AgentNetwork, prev pure.AgentNetworkStatus) error {
	if equality.Semantic.DeepEqual(prev, an.Status) {
		return nil
	}
	return r.Status().Update(ctx, an)
}
