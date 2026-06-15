package k8s

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// preflightSeverity ranks a finding. error is the only fatal level.
type preflightSeverity int

const (
	sevOK preflightSeverity = iota
	sevInfo
	sevWarn
	sevError
)

func (s preflightSeverity) symbol() string {
	switch s {
	case sevOK:
		return "✓"
	case sevError:
		return "✗"
	case sevWarn:
		return "⚠"
	default:
		return "·"
	}
}

// preflightCheck is one validated prerequisite with actionable detail.
type preflightCheck struct {
	Name     string
	Severity preflightSeverity
	Detail   string
}

// preflight validates cluster prerequisites before a k8s deploy so the user
// gets actionable guidance up front instead of discovering them later as
// Pending/NoKVMCapacity, missing-SVID, or webhook-cert failures. Every check is
// best-effort and non-fatal except cert-manager being absent while admission
// webhooks are requested (that deploy cannot succeed). The kata-fc RuntimeClass,
// SPIRE, and CNI checks warn — a runc/dev posture or host-network is legitimate.
func preflight(ctx context.Context, disc discovery.DiscoveryInterface, c client.Client, withWebhooks bool) ([]preflightCheck, error) {
	var out []preflightCheck

	// 1. kata-fc RuntimeClass — the default sandbox class for AgentRun pods.
	var rcs nodev1.RuntimeClassList
	if err := c.List(ctx, &rcs); err != nil {
		return nil, fmt.Errorf("list RuntimeClasses: %w", err)
	}
	hasKata := false
	for i := range rcs.Items {
		if rcs.Items[i].Name == "kata-fc" {
			hasKata = true
			break
		}
	}
	if hasKata {
		out = append(out, preflightCheck{"RuntimeClass kata-fc", sevOK, "present"})
	} else {
		out = append(out, preflightCheck{
			"RuntimeClass kata-fc", sevWarn,
			"absent — kata-fc runs stay Pending/NoKVMCapacity. Use --default-run-runtime-class=runc --allow-host-runtime for a dev/CI cluster, or install a kata RuntimeClass.",
		})
	}

	// 2. cert-manager — required only when installing the admission webhooks,
	// which need an injected serving cert. This is the one fatal check.
	hasCertMgr, cmVer := hasAPIGroup(disc, "cert-manager.io")
	switch {
	case !withWebhooks:
		out = append(out, preflightCheck{"cert-manager", sevInfo, "not required (deploying without admission webhooks)"})
	case hasCertMgr:
		out = append(out, preflightCheck{"cert-manager", sevOK, "present (" + cmVer + ")"})
	default:
		out = append(out, preflightCheck{
			"cert-manager", sevError,
			"absent but --with-webhooks is set — the webhook serving cert will never be injected (failurePolicy=Fail blocks all CR writes). Install cert-manager, or deploy without --with-webhooks.",
		})
	}

	// 3. SPIRE — the secretless/SVID attestation rail. Absent is fine (the broker
	// falls back to local SO_PEERCRED peer attestation), so warn, don't fail.
	hasSpiffeID, _ := hasAPIGroup(disc, "spiffeid.spiffe.io")
	hasCSI := false
	var csi storagev1.CSIDriver
	if err := c.Get(ctx, types.NamespacedName{Name: "csi.spiffe.io"}, &csi); err == nil {
		hasCSI = true
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get csi.spiffe.io CSIDriver: %w", err)
	}
	switch {
	case hasSpiffeID && hasCSI:
		out = append(out, preflightCheck{"SPIRE (ClusterSPIFFEID + CSI)", sevOK, "present"})
	case hasSpiffeID || hasCSI:
		out = append(out, preflightCheck{
			"SPIRE (ClusterSPIFFEID + CSI)", sevWarn,
			"partial install — need both the spiffeid.spiffe.io CRD and the csi.spiffe.io CSIDriver for SVID mounting; the broker falls back to local peer attestation.",
		})
	default:
		out = append(out, preflightCheck{
			"SPIRE (ClusterSPIFFEID + CSI)", sevWarn,
			"absent — secretless SVID attestation is unavailable; the broker uses local SO_PEERCRED peer attestation (peerAuth: local). Install SPIRE for the spire/spire+local posture.",
		})
	}

	// 4. CNI NetworkPolicy enforcement (ties to rv1.2). kindnet does not enforce
	// NetworkPolicy, so the egress floor is a silent no-op there.
	var kindnet appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: "kindnet"}, &kindnet); err == nil {
		out = append(out, preflightCheck{
			"CNI NetworkPolicy enforcement", sevWarn,
			"kindnet detected — it does NOT enforce NetworkPolicy, so the egress floor is a no-op (status reports egressEnforcement=unenforced, rv1.2). Use a policy-enforcing CNI (Cilium/Calico) and set the operator --cni-enforces-networkpolicy in production.",
		})
	} else if apierrors.IsNotFound(err) {
		out = append(out, preflightCheck{
			"CNI NetworkPolicy enforcement", sevInfo,
			"non-kindnet CNI — if it enforces NetworkPolicy (Cilium/Calico/eBPF), set the operator --cni-enforces-networkpolicy so status reports honest egress enforcement (rv1.2).",
		})
	} else {
		return nil, fmt.Errorf("get kindnet DaemonSet: %w", err)
	}

	return out, nil
}

// hasAPIGroup reports whether the cluster serves an API group, with its
// preferred (or first) group-version for the detail line. Mirrors the
// discovery-based detection used by detectAutoscalers.
func hasAPIGroup(disc discovery.DiscoveryInterface, name string) (bool, string) {
	groups, err := disc.ServerGroups()
	if err != nil {
		return false, ""
	}
	for _, g := range groups.Groups {
		if g.Name != name {
			continue
		}
		ver := g.PreferredVersion.GroupVersion
		if ver == "" && len(g.Versions) > 0 {
			ver = g.Versions[0].GroupVersion
		}
		return true, ver
	}
	return false, ""
}
