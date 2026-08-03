package agentbench

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// tenantSecretLabels marks a Secret as tenant-owned per the operator's
// tenant-boundary check (5vr) — required or the operator refuses to
// read/project the Secret.
var tenantSecretLabels = map[string]string{pure.TenantSecretLabel: "true"}

// Fleet deploys a plan's CR fleet into a per-run namespace and tears it down.
type Fleet struct {
	client    client.Client
	scheme    *runtime.Scheme
	namespace string
	planDir   string
	// substitutions are applied to manifest bytes (e.g. nonce, namespace).
	subs map[string]string
	// nsOwner is the created namespace, used as the GC owner for fleet objects
	// applied with NewRunDriver.
	nsOwner *metav1.OwnerReference
	// readyTimeout caps awaitReady.
	readyTimeout time.Duration
}

// NewFleet builds a Fleet bound to namespace ns (agentbench-<runID>).
func NewFleet(c client.Client, scheme *runtime.Scheme, ns, planDir string, subs map[string]string) *Fleet {
	return &Fleet{
		client:       c,
		scheme:       scheme,
		namespace:    ns,
		planDir:      planDir,
		subs:         subs,
		readyTimeout: 5 * time.Minute,
	}
}

// Namespace returns the run namespace.
func (f *Fleet) Namespace() string { return f.namespace }

// Client returns the underlying controller-runtime client (for drivers).
func (f *Fleet) Client() client.Client { return f.client }

// Owner returns an ownerReference to the run namespace for GC, or nil before
// Deploy creates it.
func (f *Fleet) Owner() *metav1.OwnerReference { return f.nsOwner }

// Deploy creates the run namespace, its secrets, and applies the fleet
// manifests. Objects get an ownerReference to the namespace so Teardown's
// namespace delete cascades.
func (f *Fleet) Deploy(ctx context.Context, plan *BenchPlan) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   f.namespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "agentbench"},
		},
	}
	if err := f.client.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("fleet: create namespace %s: %w", f.namespace, err)
	}
	// Re-read to capture UID for the owner reference.
	if err := f.client.Get(ctx, client.ObjectKey{Name: f.namespace}, ns); err != nil {
		return fmt.Errorf("fleet: get namespace %s: %w", f.namespace, err)
	}
	owner := metav1.NewControllerRef(ns, corev1.SchemeGroupVersion.WithKind("Namespace"))
	// A namespace owning namespaced children is unusual; we instead rely on the
	// namespace delete cascade for GC, but keep the ref for traceability.
	owner.Controller = nil
	owner.BlockOwnerDeletion = nil
	f.nsOwner = owner

	// Secrets first (the broker/harness env references them). Tenant-boundary
	// opt-in (5vr): the operator refuses to read a CR-referenced Secret unless
	// it carries agents.smol-agents.ai/tenant-secret=true.
	for _, s := range plan.Fleet.Secrets {
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: f.namespace, Labels: tenantSecretLabels},
			StringData: f.substituteMap(s.StringData),
		}
		if err := f.client.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("fleet: create secret %s: %w", s.Name, err)
		}
	}

	// Copy real provider secrets from the source namespace (values never committed
	// to the repo). The operator bakes these values into the per-run broker config,
	// so they must exist in the run namespace before the Agents reconcile. The
	// copy is stamped with the tenant-boundary label regardless of whether the
	// source secret carries it — the destination secret is what the operator
	// reads.
	for _, name := range plan.Fleet.CopySecrets {
		src := &corev1.Secret{}
		if err := f.client.Get(ctx, client.ObjectKey{Namespace: plan.Fleet.SecretSourceNamespace, Name: name}, src); err != nil {
			return fmt.Errorf("fleet: copy secret %s/%s: %w", plan.Fleet.SecretSourceNamespace, name, err)
		}
		dst := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.namespace, Labels: tenantSecretLabels},
			Type:       src.Type,
			Data:       src.Data,
		}
		if err := f.client.Create(ctx, dst); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("fleet: create copied secret %s: %w", name, err)
		}
	}

	// Manifests.
	for _, rel := range plan.Fleet.Manifests {
		objs, err := f.loadManifest(rel)
		if err != nil {
			return err
		}
		for _, obj := range objs {
			obj.SetNamespace(f.namespace)
			if err := f.client.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("fleet: apply %s %s: %w", obj.GetKind(), obj.GetName(), err)
			}
		}
	}

	if err := f.awaitReady(ctx, plan.Fleet.AwaitReady); err != nil {
		return err
	}
	return nil
}

// Teardown deletes the run namespace, cascading all fleet objects + pods + CRs.
// Always safe to call (idempotent); intended for a defer.
func (f *Fleet) Teardown(ctx context.Context) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.namespace}}
	if err := f.client.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("fleet: delete namespace %s: %w", f.namespace, err)
	}
	return nil
}

// HarnessKind resolves the referenced Agent's harness kind (or "loop" for
// loop-mode agents). Used to thread kind-awareness into oracles + metrics.
// Returns "" when the Agent can't be read.
func (f *Fleet) HarnessKind(ctx context.Context, agentRef string) string {
	var ag amv1.Agent
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: f.namespace, Name: agentRef}, &ag); err != nil {
		return ""
	}
	if ag.Spec.Mode == pure.ModeHarness && ag.Spec.Harness != nil {
		return string(ag.Spec.Harness.Kind)
	}
	return "loop"
}

// loadManifest reads + decodes a (possibly multi-doc) YAML manifest into
// unstructured objects, applying substitutions.
func (f *Fleet) loadManifest(rel string) ([]*unstructured.Unstructured, error) {
	path := filepath.Join(f.planDir, rel)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fleet: read manifest %s: %w", path, err)
	}
	raw = []byte(f.substitute(string(raw)))
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var out []*unstructured.Unstructured
	for {
		var m map[string]interface{}
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("fleet: decode manifest %s: %w", path, err)
		}
		if len(m) == 0 {
			continue
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
	return out, nil
}

// awaitReady blocks until every "Kind/name" ref reports Ready/Running, or the
// readyTimeout elapses. Refs are resolved in the run namespace.
func (f *Fleet) awaitReady(ctx context.Context, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	deadline := time.Now().Add(f.readyTimeout)
	for {
		allReady := true
		var pending string
		for _, ref := range refs {
			ready, err := f.refReady(ctx, ref)
			if err != nil {
				return err
			}
			if !ready {
				allReady = false
				pending = ref
				break
			}
		}
		if allReady {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("fleet: %q not ready within %s", pending, f.readyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// refReady checks a single "Kind/name" reference. Agent → status.phase==Ready;
// AgentSession → status.phase==Running; Deployment → availableReplicas>=1;
// anything else → existence.
func (f *Fleet) refReady(ctx context.Context, ref string) (bool, error) {
	kind, name, ok := strings.Cut(ref, "/")
	if !ok {
		return false, fmt.Errorf("fleet: awaitReady ref %q must be Kind/name", ref)
	}
	key := client.ObjectKey{Namespace: f.namespace, Name: name}
	switch kind {
	case "Agent":
		var ag amv1.Agent
		if err := f.client.Get(ctx, key, &ag); err != nil {
			return false, ignoreNotFound(err)
		}
		return ag.Status.Phase == "Ready", nil
	case "AgentSession":
		var as amv1.AgentSession
		if err := f.client.Get(ctx, key, &as); err != nil {
			return false, ignoreNotFound(err)
		}
		return as.Status.Phase == pure.PhaseRunning, nil
	case "Deployment":
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
		if err := f.client.Get(ctx, key, u); err != nil {
			return false, ignoreNotFound(err)
		}
		avail, found, _ := unstructured.NestedInt64(u.Object, "status", "availableReplicas")
		return found && avail >= 1, nil
	default:
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: "runtime.agents.smol-agents.ai", Version: "v1", Kind: kind})
		if err := f.client.Get(ctx, key, u); err != nil {
			return false, ignoreNotFound(err)
		}
		return true, nil
	}
}

func (f *Fleet) substitute(s string) string {
	for k, v := range f.subs {
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}

func (f *Fleet) substituteMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = f.substitute(v)
	}
	return out
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
