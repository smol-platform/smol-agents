// Package k8s implements `agentctl deploy --target=k8s`: install the operator
// stack into an existing cluster reached via the user's kubeconfig.
package k8s

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Autoscalers records which node-autoscaling providers we found in the target
// cluster. The deploy command warns (non-fatal) when *both* are absent because
// sandboxed agents (kata-fc) need a provisioner that can request *.metal hosts.
type Autoscalers struct {
	Karpenter         bool
	KarpenterDetail   string // e.g. "API group karpenter.sh/v1"
	ClusterAutoscaler bool
	CADetail          string // e.g. "Deployment kube-system/cluster-autoscaler (Ready=2/2)"
}

// detectAutoscalers checks for Karpenter (via the discovery API — its presence
// installs the karpenter.sh group) and Cluster Autoscaler (a Deployment named
// cluster-autoscaler in a few well-known namespaces).
//
// The check is best-effort: Karpenter installed without the standard group
// version, or a CAS Deployment named differently, will read as "not found".
// That conservatism is intentional — false-positive detection would suppress
// the warning a user needs to see.
func detectAutoscalers(ctx context.Context, disc discovery.DiscoveryInterface, c client.Client) (Autoscalers, error) {
	var out Autoscalers

	groups, err := disc.ServerGroups()
	if err != nil {
		return out, fmt.Errorf("discover server groups: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name != "karpenter.sh" {
			continue
		}
		ver := g.PreferredVersion.GroupVersion
		if ver == "" && len(g.Versions) > 0 {
			ver = g.Versions[0].GroupVersion
		}
		out.Karpenter = true
		out.KarpenterDetail = "API group " + ver
		break
	}

	for _, ns := range []string{"kube-system", "cluster-autoscaler", "autoscaler"} {
		var dep appsv1.Deployment
		err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "cluster-autoscaler"}, &dep)
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return out, fmt.Errorf("get cluster-autoscaler in %s: %w", ns, err)
		}
		out.ClusterAutoscaler = true
		out.CADetail = fmt.Sprintf("Deployment %s/%s (replicas=%d/%d)",
			ns, dep.Name, dep.Status.ReadyReplicas, dep.Status.Replicas)
		break
	}

	return out, nil
}
