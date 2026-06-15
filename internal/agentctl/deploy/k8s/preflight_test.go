package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func findCheck(checks []preflightCheck, name string) (preflightCheck, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return preflightCheck{}, false
}

// A bare cluster (kindnet, no kata, no cert-manager, no SPIRE) without webhooks:
// every check warns/infos, none fatal.
func TestPreflight_BareKindCluster(t *testing.T) {
	disc := newDiscoveryFake([]string{"v1", "apps/v1"})
	c := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kindnet"}}).
		Build()

	checks, err := preflight(context.Background(), disc, c, false)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	for _, ck := range checks {
		if ck.Severity == sevError {
			t.Errorf("unexpected fatal check on a webhookless bare cluster: %s — %s", ck.Name, ck.Detail)
		}
	}
	if ck, ok := findCheck(checks, "RuntimeClass kata-fc"); !ok || ck.Severity != sevWarn {
		t.Errorf("kata-fc check = %+v, want warn", ck)
	}
	if ck, ok := findCheck(checks, "CNI NetworkPolicy enforcement"); !ok || ck.Severity != sevWarn {
		t.Errorf("kindnet CNI check = %+v, want warn", ck)
	}
}

// cert-manager absent + --with-webhooks is the one fatal case.
func TestPreflight_WebhooksWithoutCertManagerIsFatal(t *testing.T) {
	disc := newDiscoveryFake([]string{"v1", "apps/v1"})
	c := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()

	checks, err := preflight(context.Background(), disc, c, true)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	ck, ok := findCheck(checks, "cert-manager")
	if !ok || ck.Severity != sevError {
		t.Fatalf("cert-manager check = %+v, want fatal error", ck)
	}
}

// A fully-provisioned cluster (kata, cert-manager, SPIRE, non-kindnet) with
// webhooks: cert-manager + kata + SPIRE all OK, nothing fatal.
func TestPreflight_FullyProvisioned(t *testing.T) {
	disc := newDiscoveryFake([]string{"v1", "apps/v1", "cert-manager.io/v1", "spiffeid.spiffe.io/v1alpha1"})
	objs := []client.Object{
		&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata-fc"}, Handler: "kata-fc"},
		&storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "csi.spiffe.io"}},
	}
	c := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()

	checks, err := preflight(context.Background(), disc, c, true)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	for _, name := range []string{"RuntimeClass kata-fc", "cert-manager", "SPIRE (ClusterSPIFFEID + CSI)"} {
		if ck, ok := findCheck(checks, name); !ok || ck.Severity != sevOK {
			t.Errorf("%s = %+v, want OK", name, ck)
		}
	}
}
