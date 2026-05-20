//go:build e2e_l2

package l2

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestL2_AgentNodePoolProvision verifies the operator compiles an
// AgentNodePool into its owned Karpenter objects on a live k0s cluster and
// the pool reaches Ready (the operator's contract — programming Karpenter).
//
// SKIPPED by default: the L2 bootstrap does not install Karpenter (node→k0s
// join is owned by an external Karpenter deployment — see
// docs/design/agent-platform.md and docs/runbooks/agent-node-pools.md).
// Enable with L2_RUN_PROVISION=1 against a cluster that already has
// Karpenter (non-EKS k0s config). With L2_PROVISION_DEEP=1 it additionally
// applies a kata pod and waits for Karpenter to launch a *.metal node that
// joins and runs the microVM (burns on-demand metal; ~minutes).
func TestL2_AgentNodePoolProvision(t *testing.T) {
	if os.Getenv("L2_RUN_PROVISION") != "1" {
		t.Skip("provision test gated on L2_RUN_PROVISION=1; needs Karpenter on the k0s cluster — see docs/runbooks/agent-node-pools.md")
	}
	if os.Getenv("AWS_PROFILE") == "" {
		t.Skip("AWS_PROFILE unset")
	}
	if err := ensureRegion(); err != nil {
		t.Skipf("region check: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)

	t.Setenv("L2_DISTRO", "al2023")
	env, ok := provisionAndWaitReady(t)
	if !ok {
		return
	}

	// Prereq: Karpenter installed on the cluster (the L2 bootstrap doesn't
	// install it; this is where you point at your Karpenter deployment).
	if out, err := env.runSSM(ctx, "k0s kubectl get crd nodepools.karpenter.sh -o name", 30*time.Second); err != nil ||
		!bytes.Contains(out, []byte("nodepools.karpenter.sh")) {
		t.Skipf("Karpenter not installed on the L2 cluster (out=%q err=%v); install Karpenter to run this test", out, err)
	}

	name := fmt.Sprintf("e2e-kata-%d", time.Now().Unix())
	child := "anp-" + name
	manifest := fmt.Sprintf(`apiVersion: agents.stigen.ai/v1
kind: AgentNodePool
metadata: { name: %s }
spec:
  isolation: kata-fc
  arch: arm64
  instanceFamilies: [c7gd, c6gd, m7gd]
  capacityType: [on-demand, spot]
  bootstrap: { mode: UserData, distro: al2023 }
`, name)
	if err := env.Apply(ctx, []byte(manifest)); err != nil {
		t.Fatalf("apply AgentNodePool: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = env.runSSM(c, "k0s kubectl delete agentnodepool "+name+" --ignore-not-found", 30*time.Second)
	})

	// The operator must mark the pool Ready (KarpenterSynced).
	if err := pollSSMUntil(ctx, env, 90*time.Second,
		"k0s kubectl get agentnodepool "+name+" -o jsonpath='{.status.phase}'",
		func(out []byte) bool { return bytes.Contains(out, []byte("Ready")) }); err != nil {
		st, _ := env.runSSM(ctx, "k0s kubectl get agentnodepool "+name+" -o jsonpath='{.status}'", 30*time.Second)
		t.Fatalf("AgentNodePool %q never reached Ready: %v\nstatus=%s", name, err, st)
	}

	// …and have created both owned Karpenter objects.
	for _, kind := range []string{"nodepool.karpenter.sh", "ec2nodeclass.karpenter.k8s.aws"} {
		out, err := env.runSSM(ctx, "k0s kubectl get "+kind+" "+child+" -o name", 30*time.Second)
		if err != nil || !bytes.Contains(out, []byte(child)) {
			t.Fatalf("operator did not create %s/%s (out=%q err=%v)", kind, child, out, err)
		}
	}
	t.Logf("AgentNodePool %q programmed Karpenter (%s + ec2nodeclass), phase=Ready", name, child)

	if os.Getenv("L2_PROVISION_DEEP") != "1" {
		t.Log("set L2_PROVISION_DEEP=1 to also wait for a metal node + kata pod (burns on-demand metal)")
		return
	}

	// Deep check: a kata pod targeting the pool triggers Karpenter to launch
	// a metal node that joins (via the existing mechanism) and runs the VM.
	pod := fmt.Sprintf("provision-kata-%d", time.Now().Unix())
	podManifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: { name: %s, namespace: tenant-a }
spec:
  restartPolicy: Never
  runtimeClassName: kata-fc
  tolerations:
    - { key: agents.stigen.ai/isolation, operator: Equal, value: kata-fc, effect: NoSchedule }
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - { key: agents.stigen.ai/pool, operator: In, values: [%s] }
  containers:
    - name: uname
      image: docker.io/library/busybox:1.36
      command: ["sh", "-c", "uname -r"]
`, pod, name)
	if err := env.Apply(ctx, []byte(podManifest)); err != nil {
		t.Fatalf("apply kata pod: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = env.runSSM(c, "k0s kubectl -n tenant-a delete pod "+pod+" --ignore-not-found", 30*time.Second)
	})

	if err := pollSSMUntil(ctx, env, 12*time.Minute,
		"k0s kubectl -n tenant-a get pod "+pod+" -o jsonpath='{.status.phase}'",
		func(out []byte) bool { return bytes.Contains(out, []byte("Succeeded")) }); err != nil {
		desc, _ := env.runSSM(ctx, "k0s kubectl -n tenant-a describe pod "+pod, 30*time.Second)
		nodes, _ := env.runSSM(ctx, "k0s kubectl get nodes -l agents.stigen.ai/pool="+name, 20*time.Second)
		t.Fatalf("kata pod never Succeeded on a provisioned node: %v\nnodes=%s\ndescribe=%s", err, nodes, desc)
	}
	t.Logf("kata pod ran on a Karpenter-provisioned metal node (pool %q)", name)
}

// pollSSMUntil runs cmd via SSM on a 10s ticker until pred(out) holds or the
// deadline fires.
func pollSSMUntil(ctx context.Context, env *l2Env, deadline time.Duration, cmd string, pred func([]byte) bool) error {
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		out, _ := env.runSSM(dctx, cmd, 30*time.Second)
		if pred(out) {
			return nil
		}
		select {
		case <-dctx.Done():
			return fmt.Errorf("deadline: last out=%q", string(out))
		case <-tick.C:
		}
	}
}
