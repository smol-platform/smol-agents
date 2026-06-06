package agentmodel

import (
	"context"
	"sort"

	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/smol-platform/smol-agents/operator/internal/builders"
)

// apiserverEgressRule discovers the kube-apiserver endpoints (the `kubernetes`
// Service's EndpointSlices in the default namespace) and renders an egress allow
// rule for them (M1.18). The default-deny floor only permits public IPs on
// 80/443, so on a single-node / public-IP cluster (apiserver at <node-ip>:6443)
// a run pod could not reach the apiserver — breaking A2A child creation.
//
// Best-effort: a discovery failure (or no EndpointSlices) returns nil, leaving
// the floor unchanged. That preserves the prior behaviour, where the apiserver
// is reachable only if it already sits in an allowed range (e.g. an in-cluster
// ClusterIP in RFC1918).
func apiserverEgressRule(ctx context.Context, c client.Client) *networkingv1.NetworkPolicyEgressRule {
	var slices discoveryv1.EndpointSliceList
	if err := c.List(ctx, &slices,
		client.InNamespace("default"),
		client.MatchingLabels{"kubernetes.io/service-name": "kubernetes"}); err != nil {
		return nil
	}
	ipSet := map[string]struct{}{}
	var port int32 = 443
	for i := range slices.Items {
		for _, p := range slices.Items[i].Ports {
			if p.Port != nil {
				port = *p.Port
			}
		}
		for _, ep := range slices.Items[i].Endpoints {
			for _, a := range ep.Addresses {
				ipSet[a] = struct{}{}
			}
		}
	}
	ips := make([]string, 0, len(ipSet))
	for ip := range ipSet {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	return builders.APIServerEgressRule(ips, port)
}
