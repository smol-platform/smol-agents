package agentmodel

import (
	"context"
	"testing"

	"github.com/nats-io/nkeys"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

// attachNATSCreds mounts the creds Secret into the worker container ONLY and points
// serve-session at it; no other container is touched (M2.20).
func TestAttachNATSCreds(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}}}
	attachNATSCreds(pod, "tenant-a-nats-worker-creds")

	c := pod.Spec.Containers[0]
	var env string
	for _, e := range c.Env {
		if e.Name == "AGENTSESSION_NATS_CREDS" {
			env = e.Value
		}
	}
	if env != natsCredsMountPath+"/"+natsCredsFileKey {
		t.Errorf("AGENTSESSION_NATS_CREDS = %q, want the mounted creds path", env)
	}
	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.Name == natsCredsVolumeName {
			mounted = true
		}
	}
	if !mounted {
		t.Error("worker container must mount the creds volume")
	}
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == natsCredsVolumeName {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.Secret == nil || vol.Secret.SecretName != "tenant-a-nats-worker-creds" {
		t.Errorf("creds volume = %+v, want the per-ns secret", vol)
	}
}

// ensureWorkerCreds mints the per-namespace creds Secret once and reuses it on the
// next reconcile (idempotent — no key churn).
func TestEnsureWorkerCreds_MintsOnceAndReuses(t *testing.T) {
	akp, _ := nkeys.CreateAccount()
	seed, _ := akp.Seed()

	sch := runtime.NewScheme()
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	_ = corev1.AddToScheme(sch)
	c := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &AgentSessionReconciler{Client: c, Scheme: sch, NATSAccountSeed: seed}

	name, err := r.ensureWorkerCreds(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("ensureWorkerCreds: %v", err)
	}
	if name != "tenant-a-nats-worker-creds" {
		t.Errorf("secret name = %q", name)
	}
	var sec corev1.Secret
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: name}, &sec); err != nil {
		t.Fatalf("creds secret not created: %v", err)
	}
	creds := sec.Data[natsCredsFileKey]
	if len(creds) == 0 {
		t.Fatal("creds secret has no worker.creds")
	}
	// Second call reuses the same minted creds (no churn).
	if _, err := r.ensureWorkerCreds(context.Background(), "tenant-a"); err != nil {
		t.Fatalf("second ensureWorkerCreds: %v", err)
	}
	var again corev1.Secret
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: name}, &again)
	if string(again.Data[natsCredsFileKey]) != string(creds) {
		t.Error("ensureWorkerCreds must reuse the existing creds, not re-mint")
	}
}
