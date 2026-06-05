package features

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
)

// M1.17: the SmolAgent reconcile installs a default-ON egress floor selecting
// the served pods — no enable flag — with the metadata IP blocked.
func TestEgressFloorReconciler(t *testing.T) {
	cr := &v1.SmolAgent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "team-a"}}

	res, objs, err := EgressFloorReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Enabled || !res.Ready {
		t.Errorf("egress floor must be default-ON + Ready, got %+v", res)
	}
	if len(objs) != 1 {
		t.Fatalf("want exactly one owned object (the NetworkPolicy), got %d", len(objs))
	}
	np, ok := objs[0].(*networkingv1.NetworkPolicy)
	if !ok {
		t.Fatalf("owned object is %T, want *NetworkPolicy", objs[0])
	}
	if np.Name != "alice-serving-egress" || np.Namespace != "team-a" {
		t.Errorf("policy name/ns = %s/%s", np.Name, np.Namespace)
	}
	// Selects exactly the served pods (the Knative revision template labels).
	if !reflect.DeepEqual(np.Spec.PodSelector.MatchLabels, builders.Selector(cr)) {
		t.Errorf("podSelector = %v, want Selector(cr) %v", np.Spec.PodSelector.MatchLabels, builders.Selector(cr))
	}
	// It is an egress policy with at least one allow rule (a blanket deny-all
	// would break serving; the floor allows DNS/in-cluster/80-443).
	egressType := false
	for _, pt := range np.Spec.PolicyTypes {
		if pt == networkingv1.PolicyTypeEgress {
			egressType = true
		}
	}
	if !egressType || len(np.Spec.Egress) == 0 {
		t.Errorf("must be an egress policy with allow rules, got types=%v rules=%d", np.Spec.PolicyTypes, len(np.Spec.Egress))
	}
	// The metadata IP is excluded somewhere in the floor (defense-in-depth).
	b, _ := json.Marshal(np.Spec.Egress)
	if !strings.Contains(string(b), "169.254") {
		t.Errorf("floor must block the 169.254 metadata range: %s", b)
	}
}
