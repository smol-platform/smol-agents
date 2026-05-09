//go:build e2e_l2

package l2

import (
	"bytes"
	"context"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestL2_BottlerocketWorker provisions an AL2023 controller (the
// already-validated full bring-up) + a Bottlerocket aws-k8s-1.31
// worker that joins it.
//
// Status: SKIPPED by default. The Bottlerocket aws-k8s variant's
// settings-generator (`pluto`) blocks boot at "Generate additional
// settings for Kubernetes" with "Timed out retrieving private DNS
// name from EC2" even with `ec2:DescribeInstances` granted to the
// instance role and `[settings.aws.region]` + node-ip set
// explicitly in user-data. The variant is designed for EKS:
// without an EKS cluster's tags + `aws-auth` ConfigMap, pluto
// cannot complete its bootstrap probe within its 5-min internal
// deadline.
//
// To re-enter this work: set L2_RUN_BOTTLEROCKET_WORKER=1 + try
// either:
//  1. Provisioning a real EKS cluster (expensive — $73/mo
//     control-plane fee) and pointing the worker at it.
//  2. Patching Bottlerocket's pluto out via a custom AMI build
//     (variant fork).
//  3. Following bottlerocket-os/bottlerocket#4517 — static pods
//     + standalone mode — which sidesteps pluto entirely but
//     requires shipping pre-rendered kubeadm manifests.
//
// See scripts/aws-l2/bottlerocket-bootstrap/README.md for the
// architectural background.
//
// Cost when enabled: ~$0.04/run (2 × c7g.large for ~6 min on-
// demand). The IAM ec2:DescribeInstances grant is left in place
// in terraform — useful if any future Bottlerocket integration
// needs it.
func TestL2_BottlerocketWorker(t *testing.T) {
	if os.Getenv("L2_RUN_BOTTLEROCKET_WORKER") != "1" {
		t.Skip("Bottlerocket-worker test gated on L2_RUN_BOTTLEROCKET_WORKER=1; pluto blocks bootstrap on non-EKS clusters")
	}
	if os.Getenv("AWS_PROFILE") == "" {
		t.Skip("AWS_PROFILE unset")
	}
	if err := ensureRegion(); err != nil {
		t.Skipf("region check: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)

	// Phase 1: AL2023 controller via the standard smoke path.
	t.Setenv("L2_DISTRO", "al2023")
	controller, ok := provisionAndWaitReady(t)
	if !ok {
		return
	}
	t.Log("controller up; extracting k0s join params via SSM")

	api, ca, token, err := extractK0sJoinParams(ctx, controller, t)
	if err != nil {
		t.Fatalf("extract join params: %v", err)
	}
	t.Logf("controller api=%s ca=%dB token=%dB",
		api, len(ca), len(token))

	// Phase 2: Bottlerocket worker, joining via TOML user-data.
	t.Setenv("L2_DISTRO_OVERRIDE", string(DistroBottlerocketWorker))
	t.Setenv("L2_K8S_API_SERVER", api)
	t.Setenv("L2_K8S_CA_BASE64", base64.StdEncoding.EncodeToString([]byte(ca)))
	t.Setenv("L2_K8S_BOOTSTRAP_TOKEN", token)
	t.Setenv("L2_K8S_CLUSTER_NAME", "knative-agents-l2")

	worker, err := Provision(ctx)
	if err != nil {
		t.Fatalf("worker Provision: %v", err)
	}
	t.Cleanup(func() {
		if os.Getenv("L2_KEEP_INSTANCE") != "" {
			t.Logf("L2_KEEP_INSTANCE: leaving worker %s alive", worker.InstanceID)
			return
		}
		shutdown, c := context.WithTimeout(context.Background(), 5*time.Minute)
		defer c()
		if err := worker.Teardown(shutdown); err != nil {
			t.Logf("worker teardown warning: %v", err)
		}
	})
	t.Logf("worker up: instance=%s public_dns=%s",
		worker.InstanceID, worker.PublicDNS)

	// Phase 3: poll the controller's nodelist for the worker
	// becoming Ready. Bottlerocket aws-k8s nodes register with
	// hostname = ip-<priv>.<region>.compute.internal. We just
	// look for ANY Ready node that isn't the controller itself.
	if err := waitForBottlerocketWorkerReady(ctx, controller, t,
		15*time.Minute); err != nil {
		t.Fatalf("worker never became Ready: %v", err)
	}
	t.Log("Bottlerocket worker joined cluster + reached Ready")
}

// extractK0sJoinParams reads the k0s controller's API endpoint,
// CA cert, and a freshly-issued worker bootstrap token via SSM.
func extractK0sJoinParams(ctx context.Context, env *l2Env, t *testing.T) (api, ca, token string, err error) {
	// Public IP via IMDSv2 (AL2023 enforces token-auth). curl
	// PUT for a 5-min token, then GET the public-ipv4.
	out, e := env.runSSM(ctx,
		`TOKEN=$(curl -fsS -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 300") && \
		 curl -fsS -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/public-ipv4`,
		15*time.Second)
	if e != nil || len(out) == 0 {
		return "", "", "", fmt.Errorf("controller public-ipv4 lookup: %w", e)
	}
	api = "https://" + strings.TrimSpace(string(out)) + ":6443"

	out, e = env.runSSM(ctx, "cat /var/lib/k0s/pki/ca.crt", 30*time.Second)
	if e != nil || !bytes.Contains(out, []byte("BEGIN CERTIFICATE")) {
		return "", "", "", fmt.Errorf("k0s ca.crt: %w (got %dB)", e, len(out))
	}
	ca = string(out)

	// Bottlerocket aws-k8s expects a kubeadm-style bootstrap
	// token (`<id>.<secret>`, 6+16 lowercase alnum). k0s issues
	// kubeconfig-shaped tokens via `k0s token create`, NOT
	// kubeadm-shaped. Create one ourselves: a Secret in
	// kube-system with type bootstrap.kubernetes.io/token. The
	// kubelet on the worker will use this to exchange for a real
	// client cert via the bootstrap auth flow.
	tokenID := randAlpha(6)
	tokenSecret := randAlpha(16)
	expiry := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-%s
  namespace: kube-system
type: bootstrap.kubernetes.io/token
stringData:
  token-id: %s
  token-secret: %s
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: system:bootstrappers:kubeadm:default-node-token
  expiration: %s
`, tokenID, tokenID, tokenSecret, expiry)

	if err := env.Apply(ctx, []byte(manifest)); err != nil {
		return "", "", "", fmt.Errorf("create bootstrap-token: %w", err)
	}

	// Bottlerocket aws-k8s also needs an RBAC binding so a
	// system:bootstrapper can create CSRs and have them
	// auto-approved. K8s ships these as built-in ClusterRoles;
	// just bind:
	rbac := `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: { name: kubeadm:node-autoapprove-bootstrap }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: system:certificates.k8s.io:certificatesigningrequests:nodeclient }
subjects:
  - kind: Group
    name: system:bootstrappers:kubeadm:default-node-token
    apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: { name: kubeadm:node-autoapprove-certificate-rotation }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: system:certificates.k8s.io:certificatesigningrequests:selfnodeclient }
subjects:
  - kind: Group
    name: system:nodes
    apiGroup: rbac.authorization.k8s.io
`
	if err := env.Apply(ctx, []byte(rbac)); err != nil {
		return "", "", "", fmt.Errorf("create bootstrap RBAC: %w", err)
	}

	token = tokenID + "." + tokenSecret
	return api, ca, token, nil
}

// randAlpha returns a random lowercase alphanumeric string of
// length n, suitable for kubeadm bootstrap-token-id (6 chars) and
// token-secret (16 chars). Uses crypto/rand.
func randAlpha(n int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, n)
	for i := range b {
		var buf [1]byte
		_, _ = randRead(buf[:])
		b[i] = alphabet[int(buf[0])%len(alphabet)]
	}
	return string(b)
}

// randRead is a thin wrapper so the test file doesn't need to
// import crypto/rand at the package level conditional on build tag.
func randRead(p []byte) (int, error) {
	return cryptoRand.Read(p)
}

// waitForBottlerocketWorkerReady polls the controller for any
// node tagged with our Bottlerocket node-label (set in the
// worker's user-data) that's Ready.
func waitForBottlerocketWorkerReady(ctx context.Context, controller *l2Env, t *testing.T, deadline time.Duration) error {
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		out, _ := controller.runSSM(dctx,
			`k0s kubectl get nodes -l knative-agents.stigen.ai/distro=bottlerocket -o jsonpath='{range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'`,
			30*time.Second)
		if bytes.Contains(out, []byte("=True")) {
			t.Logf("worker node observed: %s", strings.TrimSpace(string(out)))
			return nil
		}
		select {
		case <-dctx.Done():
			return fmt.Errorf("deadline: last nodelist=%q", string(out))
		case <-tick.C:
		}
	}
}
