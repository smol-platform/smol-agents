package agentmodel

import (
	"context"
	"fmt"

	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pkgsandbox "github.com/smol-platform/smol-agents/pkg/sandbox"
)

// resolveSandbox picks a run/session pod's RuntimeClass, fail-closed. At most
// one of (class, pending, failed) is non-empty: failed is a hard R-SBX-1
// violation (runc without operator opt-in); pending means the chosen hardened
// RuntimeClass is not registered yet, so the caller refuses to schedule an
// unisolated pod and waits. An empty requested class falls back to defaultClass,
// then kata-fc. Shared by the AgentRun and AgentSession reconcilers.
func resolveSandbox(ctx context.Context, r client.Reader, requested, defaultClass string, allowHostRuntime bool) (class, pending, failed string) {
	class = requested
	if class == "" {
		class = defaultClass
	}
	if class == "" {
		class = string(pkgsandbox.KindKataFC)
	}
	if pkgsandbox.ParseKind(class) == pkgsandbox.KindRunc {
		if allowHostRuntime {
			return class, "", "" // deliberately permitted (dev/CI cluster)
		}
		return "", "", "runc-requires-allow-host-runtime"
	}
	var rc nodev1.RuntimeClass
	if err := r.Get(ctx, types.NamespacedName{Name: class}, &rc); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Sprintf("RuntimeClass %q not registered; refusing to run unisolated", class), ""
		}
		return "", fmt.Sprintf("checking RuntimeClass %q: %v", class, err), ""
	}
	return class, "", ""
}
