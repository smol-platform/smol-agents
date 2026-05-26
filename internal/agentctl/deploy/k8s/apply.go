package k8s

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	yamldec "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// renderOverlay returns the resources rendered from a kustomize overlay path.
// Uses the on-disk filesystem; the overlay must exist under the manifestsDir
// the caller resolved.
func renderOverlay(overlayPath string) ([]*unstructured.Unstructured, error) {
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rm, err := k.Run(filesys.MakeFsOnDisk(), overlayPath)
	if err != nil {
		return nil, fmt.Errorf("kustomize render %s: %w", overlayPath, err)
	}
	y, err := rm.AsYaml()
	if err != nil {
		return nil, fmt.Errorf("kustomize emit yaml: %w", err)
	}
	return splitYAML(y)
}

// splitYAML decodes a multi-doc YAML stream into unstructured objects, skipping
// empty docs.
func splitYAML(in []byte) ([]*unstructured.Unstructured, error) {
	dec := yamldec.NewYAMLOrJSONDecoder(bytes.NewReader(in), 4096)
	var out []*unstructured.Unstructured
	for {
		var u unstructured.Unstructured
		if err := dec.Decode(&u.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode yaml doc: %w", err)
		}
		if len(u.Object) == 0 {
			continue
		}
		out = append(out, &u)
	}
	return out, nil
}

// defaultOperatorImagePrefix is the repo/name the bundled manifests pin the
// operator to (manager.yaml: "smol-agents/operator:<tag>"). The override
// matches on this prefix so it touches only the operator container, never a
// sidecar (e.g. kube-rbac-proxy) or an image-typed field in a CRD schema.
const defaultOperatorImagePrefix = "smol-agents/operator"

// overrideOperatorImage rewrites the operator container image in any rendered
// Deployment whose container image starts with defaultOperatorImagePrefix.
// This is what makes --operator-image effective: the bundled manifests pin a
// local-only ref, so installing against a remote cluster needs a pullable
// registry ref. Returns the number of containers rewritten.
func overrideOperatorImage(objs []*unstructured.Unstructured, img string) (int, error) {
	if img == "" {
		return 0, nil
	}
	rewritten := 0
	for _, o := range objs {
		if o.GetKind() != "Deployment" {
			continue
		}
		containers, found, err := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
		if err != nil || !found {
			continue
		}
		changed := false
		for i := range containers {
			c, ok := containers[i].(map[string]interface{})
			if !ok {
				continue
			}
			if cur, _ := c["image"].(string); strings.HasPrefix(cur, defaultOperatorImagePrefix) {
				c["image"] = img
				containers[i] = c
				changed = true
				rewritten++
			}
		}
		if changed {
			if err := unstructured.SetNestedSlice(o.Object, containers, "spec", "template", "spec", "containers"); err != nil {
				return rewritten, fmt.Errorf("set operator image on %s: %w", nameForLog(o), err)
			}
		}
	}
	return rewritten, nil
}

// splitCRDs partitions a manifest set so CRDs can be applied (and become
// Established) before any CR that depends on them.
func splitCRDs(objs []*unstructured.Unstructured) (crds, others []*unstructured.Unstructured) {
	for _, o := range objs {
		if isCRD(o) {
			crds = append(crds, o)
		} else {
			others = append(others, o)
		}
	}
	return
}

func isCRD(u *unstructured.Unstructured) bool {
	gk := u.GroupVersionKind().GroupKind()
	return gk.Group == apiextv1.GroupName && gk.Kind == "CustomResourceDefinition"
}

// applyAll does a server-side-apply of each object, retrying briefly on
// "no matches for kind" (the CRD may still be propagating to the discovery
// cache the controller-runtime client uses).
func applyAll(ctx context.Context, c client.Client, out io.Writer, objs []*unstructured.Unstructured) error {
	for i, o := range objs {
		if err := applySSA(ctx, c, o); err != nil {
			return fmt.Errorf("apply %s/%s: %w", o.GetKind(), nameForLog(o), err)
		}
		fmt.Fprintf(out, "    [%d/%d] %-32s %s  ✓\n", i+1, len(objs), o.GetKind(), nameForLog(o))
	}
	return nil
}

func applySSA(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	// CRDs often lag the discovery cache for a moment; retry-on-no-match
	// absorbs that without inflating callers with backoff loops.
	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		err := c.Patch(ctx, obj.DeepCopy(), client.Apply, client.ForceOwnership, client.FieldOwner("agentctl"))
		if err == nil {
			return true, nil
		}
		if meta.IsNoMatchError(err) {
			return false, nil // retry
		}
		return false, err
	})
}

// waitCRDsEstablished blocks until every CRD has Established=True or deadline.
func waitCRDsEstablished(ctx context.Context, c client.Client, crds []*unstructured.Unstructured, deadline time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, deadline, true, func(ctx context.Context) (bool, error) {
		for _, u := range crds {
			var crd apiextv1.CustomResourceDefinition
			if err := c.Get(ctx, types.NamespacedName{Name: u.GetName()}, &crd); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			ok := false
			for _, cond := range crd.Status.Conditions {
				if cond.Type == apiextv1.Established && cond.Status == apiextv1.ConditionTrue {
					ok = true
					break
				}
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	})
}

// waitDeployment blocks until the named Deployment reports AvailableReplicas >= 1.
func waitDeployment(ctx context.Context, c client.Client, ns, name string, deadline time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 1*time.Second, deadline, true, func(ctx context.Context) (bool, error) {
		var d appsv1.Deployment
		err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &d)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return d.Status.AvailableReplicas >= 1, nil
	})
}

// nameForLog formats namespace/name for unstructured (some kinds are
// cluster-scoped, no namespace).
func nameForLog(u *unstructured.Unstructured) string {
	if u.GetNamespace() == "" {
		return u.GetName()
	}
	return u.GetNamespace() + "/" + u.GetName()
}
