package agentmodel

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/smol-platform/smol-agents/pkg/sessionqueue"
)

const (
	natsCredsVolumeName = "nats-worker-creds"
	natsCredsMountPath  = "/etc/nats-creds"
	natsCredsFileKey    = "worker.creds"
)

// workerCredsSecretName is the per-namespace NATS worker-creds Secret — shared by
// every session worker in the namespace (it carries the namespace's scope, not a
// per-session identity), so it is NOT owned by any one AgentSession.
func workerCredsSecretName(ns string) string { return ns + "-nats-worker-creds" }

// ensureWorkerCreds mints (once per namespace) the namespace-scoped NATS worker
// credential (M2.20) and stores it in a per-namespace Secret, returning the name.
// Idempotent: an existing Secret is reused — re-minting would churn the user key
// and needlessly roll every worker in the namespace.
func (r *AgentSessionReconciler) ensureWorkerCreds(ctx context.Context, ns string) (string, error) {
	name := workerCredsSecretName(ns)
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &existing)
	if err == nil {
		return name, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}
	creds, err := sessionqueue.MintWorkerCreds(r.NATSAccountSeed, ns)
	if err != nil {
		return "", fmt.Errorf("mint worker creds: %w", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"app.kubernetes.io/name": "smol-agents", "app.kubernetes.io/component": "agentsession"},
		},
		Data: map[string][]byte{natsCredsFileKey: creds},
	}
	if err := r.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", err
	}
	return name, nil
}

// attachNATSCreds mounts the per-namespace worker-creds Secret into the worker
// container (Containers[0]) and points serve-session at it via
// AGENTSESSION_NATS_CREDS, so the worker authenticates with its namespace-scoped
// NATS credential (M2.20).
func attachNATSCreds(pod *corev1.Pod, secretName string) {
	if len(pod.Spec.Containers) == 0 {
		return
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: natsCredsVolumeName,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: secretName,
			Items:      []corev1.KeyToPath{{Key: natsCredsFileKey, Path: natsCredsFileKey}},
		}},
	})
	c := &pod.Spec.Containers[0]
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: natsCredsVolumeName, MountPath: natsCredsMountPath, ReadOnly: true})
	c.Env = append(c.Env, corev1.EnvVar{Name: "AGENTSESSION_NATS_CREDS", Value: natsCredsMountPath + "/" + natsCredsFileKey})
}
