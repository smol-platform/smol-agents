package features

import (
	"context"
	"testing"

	v1 "github.com/stigen/knative-agents/operator/api/v1"
)

func TestTransportPrivate_HappyAndPrereq(t *testing.T) {
	cr := sample()
	cr.Spec.Features.Transport.Private.Enabled = true
	res, _, _ := TransportPrivateReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if !res.Ready {
		t.Errorf("happy: %+v", res)
	}
	cr.Spec.Features.Identity.Enabled = false
	res, _, _ = TransportPrivateReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if res.Ready || res.Reason != "PrerequisitesUnmet" {
		t.Errorf("prereq missing: %+v", res)
	}
}

func TestTransportPublic_RequiresCertKey(t *testing.T) {
	cr := sample()
	cr.Spec.Features.Transport.Public.Enabled = true
	res, _, _ := TransportPublicReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if res.Ready || res.Reason != "PrerequisitesUnmet" {
		t.Errorf("missing cert/key: %+v", res)
	}
	cr.Spec.Features.Transport.Public.CertPath = "/tls/c"
	cr.Spec.Features.Transport.Public.KeyPath = "/tls/k"
	res, _, _ = TransportPublicReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if !res.Ready {
		t.Errorf("with paths: %+v", res)
	}
}

func TestTransportPublic_DisabledByDefault(t *testing.T) {
	cr := sample()
	res, _, _ := TransportPublicReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if res.Reason != "Disabled" {
		t.Errorf("default disabled: %+v", res)
	}
}

func TestKnative_RendersWithCorrectKind(t *testing.T) {
	cr := sample()
	cr.Spec.Features.Knative.Enabled = true
	cr.Spec.DeploymentKind = "knative"
	res, owned, err := KnativeReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ready || len(owned) != 1 {
		t.Errorf("expected 1 owned + Ready: %+v owned=%d", res, len(owned))
	}
	if owned[0].GetObjectKind().GroupVersionKind().Group != "serving.knative.dev" {
		t.Errorf("wrong GVK: %s", owned[0].GetObjectKind().GroupVersionKind())
	}
}

func TestKnative_DeploymentKindMismatch(t *testing.T) {
	cr := sample()
	cr.Spec.Features.Knative.Enabled = true
	cr.Spec.DeploymentKind = "deployment"
	res, _, _ := KnativeReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if res.Reason != "DeploymentKindMismatch" {
		t.Errorf("got %+v", res)
	}
}

func TestObservability_HappyAndNoEndpoint(t *testing.T) {
	cr := sample()
	cr.Spec.Features.Observability.Enabled = true
	cr.Spec.Features.Observability.OTLPEndpoint = "otel:4317"
	res, _, _ := ObservabilityReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if !res.Ready || res.Message != "" {
		t.Errorf("with endpoint: %+v", res)
	}
	cr.Spec.Features.Observability.OTLPEndpoint = ""
	res, _, _ = ObservabilityReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if !res.Ready || res.Message == "" {
		t.Errorf("without endpoint should still be Ready with note: %+v", res)
	}
}

func TestRegistry_AllReconcilers(t *testing.T) {
	all := []FeatureReconciler{
		IdentityReconciler{},
		SandboxReconciler{},
		SecretsReconciler{},
		EBPFReconciler{},
		TransportPrivateReconciler{},
		TransportPublicReconciler{},
		KnativeReconciler{},
		ObservabilityReconciler{},
	}
	if len(all) != 8 {
		t.Errorf("expected 8 reconcilers, got %d", len(all))
	}
	_ = v1.Features{} // keep import
}
