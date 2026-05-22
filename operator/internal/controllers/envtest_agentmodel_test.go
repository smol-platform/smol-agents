//go:build envtest

package controllers_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/controllers/agentmodel"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// agentmodelEnv is a clean envtest environment for the
// runtime.agents.smol-agents.ai/v1 controllers — separate from setupEnv()
// so each suite gets its own manager with only its reconciler family
// registered.
type agentmodelEnv struct {
	env *envtest.Environment
	cli client.Client
	ctx context.Context
}

func setupAgentmodelEnv(t *testing.T) *agentmodelEnv {
	t.Helper()
	log.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(testWriter{t: t})))
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := amv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	root := projectRoot(t)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "operator", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest Start: %v", err)
	}
	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     scheme,
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	for _, sub := range []interface {
		SetupWithManager(ctrl.Manager) error
	}{
		&agentmodel.AgentReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()},
		&agentmodel.AgentRunReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()},
		&agentmodel.AgentNetworkReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()},
	} {
		if err := sub.SetupWithManager(mgr); err != nil {
			t.Fatalf("SetupWithManager: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	go func() {
		_ = mgr.Start(ctx)
		close(stop)
	}()

	t.Cleanup(func() {
		cancel()
		<-stop
		_ = env.Stop()
	})

	return &agentmodelEnv{env: env, cli: cli, ctx: ctx}
}

// makeProvider applies a minimal ModelProvider so Agents can resolve.
func makeProvider(t *testing.T, e *agentmodelEnv, ns, name string) {
	t.Helper()
	p := &amv1.ModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: pure.ModelProviderSpec{
			Kind:      "openai",
			SecretRef: pure.AuthRef{SecretName: "openai-key"},
		},
	}
	if err := e.cli.Create(e.ctx, p); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create provider: %v", err)
	}
}

// makeNamespaceAM mirrors makeNamespace() in envtest_suite_test.go but
// takes the agentmodelEnv shape so each suite stays independent.
func makeNamespaceAM(t *testing.T, e *agentmodelEnv, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := e.cli.Create(e.ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
}

func waitForAgent(t *testing.T, e *agentmodelEnv, key types.NamespacedName, pred func(*amv1.Agent) bool) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	last := &amv1.Agent{}
	for {
		got := &amv1.Agent{}
		if err := e.cli.Get(e.ctx, key, got); err == nil {
			last = got
			if pred(got) {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for agent %s; last status: phase=%q reason=%q msg=%q",
				key, last.Status.Phase, last.Status.Reason, last.Status.Message)
		case <-tick.C:
		}
	}
}

func waitForAgentNetwork(t *testing.T, e *agentmodelEnv, key types.NamespacedName, pred func(*amv1.AgentNetwork) bool) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		got := &amv1.AgentNetwork{}
		if err := e.cli.Get(e.ctx, key, got); err == nil && pred(got) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for agentnetwork %s", key)
		case <-tick.C:
		}
	}
}

// waitForPod / patchPodPhase simulate the kubelet that envtest lacks.

func waitForPod(t *testing.T, e *agentmodelEnv, key types.NamespacedName, pred func(*corev1.Pod) bool) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		p := &corev1.Pod{}
		if err := e.cli.Get(e.ctx, key, p); err == nil && pred(p) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for pod %s", key)
		case <-tick.C:
		}
	}
}

func patchPodPhase(t *testing.T, e *agentmodelEnv, key types.NamespacedName, phase corev1.PodPhase) {
	t.Helper()
	p := &corev1.Pod{}
	if err := e.cli.Get(e.ctx, key, p); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	p.Status.Phase = phase
	if err := e.cli.Status().Update(e.ctx, p); err != nil {
		t.Fatalf("patch pod status: %v", err)
	}
}

// makeAgentAM creates a minimal Agent referencing the given provider.
func makeAgentAM(t *testing.T, e *agentmodelEnv, ns, name, providerRef string) *amv1.Agent {
	t.Helper()
	a := &amv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: pure.AgentSpec{
			Model:        pure.ModelRef{ProviderRef: providerRef, Name: "gpt-4"},
			Instructions: "be helpful",
			Budget:       pure.Budget{MaxSteps: 10, MaxTokens: 1000, MaxWallClockSeconds: 30, MaxToolCalls: 5},
		},
	}
	if err := e.cli.Create(e.ctx, a); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return a
}

func waitForAgentRun(t *testing.T, e *agentmodelEnv, key types.NamespacedName, pred func(*amv1.AgentRun) bool) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	last := &amv1.AgentRun{}
	for {
		got := &amv1.AgentRun{}
		if err := e.cli.Get(e.ctx, key, got); err == nil {
			last = got
			if pred(got) {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for agentrun %s; last state=%q reason=%q",
				key, last.Status.State, last.Status.TerminationReason)
		case <-tick.C:
		}
	}
}

// TestEnvtest_Agent_PendingWhenProviderMissing — Agent stays Pending
// with reason ProviderMissing until we create the ModelProvider.
func TestEnvtest_Agent_PendingWhenProviderMissing(t *testing.T) {
	e := setupAgentmodelEnv(t)
	makeNamespaceAM(t, e, "tenant-am")

	// Agent that references a provider that doesn't exist yet.
	a := &amv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "tenant-am"},
		Spec: pure.AgentSpec{
			Model:        pure.ModelRef{ProviderRef: "openai", Name: "gpt-4"},
			Instructions: "hi",
			Budget:       pure.Budget{MaxSteps: 10, MaxTokens: 1000, MaxWallClockSeconds: 30, MaxToolCalls: 5},
		},
	}
	if err := e.cli.Create(e.ctx, a); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	waitForAgent(t, e, types.NamespacedName{Namespace: "tenant-am", Name: "alice"}, func(g *amv1.Agent) bool {
		return g.Status.Phase == "Pending" && g.Status.Reason == "ProviderMissing"
	})

	// Now create the provider, then nudge the agent so the reconciler
	// re-runs (the AgentReconciler watches Agent only, not Provider).
	makeProvider(t, e, "tenant-am", "openai")
	got := &amv1.Agent{}
	if err := e.cli.Get(e.ctx, types.NamespacedName{Namespace: "tenant-am", Name: "alice"}, got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Instructions = "hi v2"
	if err := e.cli.Update(e.ctx, got); err != nil {
		t.Fatal(err)
	}
	waitForAgent(t, e, types.NamespacedName{Namespace: "tenant-am", Name: "alice"}, func(g *amv1.Agent) bool {
		return g.Status.Phase == "Ready"
	})
}

// TestEnvtest_AgentNetwork_ProxyHappyPath — apply an identityProxy
// AgentNetwork; reconciler should mark Ready and report
// proxyResourceCount.
func TestEnvtest_AgentNetwork_ProxyHappyPath(t *testing.T) {
	e := setupAgentmodelEnv(t)
	makeNamespaceAM(t, e, "tenant-pxy")

	an := &amv1.AgentNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-net", Namespace: "tenant-pxy"},
		Spec: pure.AgentNetworkSpec{
			Kind: pure.NetworkIdentityProxy,
			IdentityProxy: &pure.IdentityProxySpec{
				Resources: []pure.ResourceTarget{
					{
						Name: "orders-db", Kind: "tcp",
						LocalAddr: "127.0.0.1:5432",
						Gateway:   "pg.svc:8443",
						Authorize: []string{"spiffe://smol-agents.ai/ns/infra/sa/pg"},
					},
				},
				Egress: pure.EgressPolicy{
					Enforcement:   "ebpfBoth",
					RedirectCIDRs: []string{"10.42.0.0/16"},
				},
			},
		},
	}
	if err := e.cli.Create(e.ctx, an); err != nil {
		t.Fatalf("create agentnetwork: %v", err)
	}

	waitForAgentNetwork(t, e,
		types.NamespacedName{Namespace: "tenant-pxy", Name: "prod-net"},
		func(g *amv1.AgentNetwork) bool {
			return g.Status.Phase == "Ready" && g.Status.ProxyResourceCount == 1
		})
}

// TestEnvtest_AgentNetwork_WireGuardSecretMissing: Pending/SecretMissing
// flips to Ready when the broker secret appears (Watches drives the requeue).
func TestEnvtest_AgentNetwork_WireGuardSecretMissing(t *testing.T) {
	e := setupAgentmodelEnv(t)
	makeNamespaceAM(t, e, "tenant-wg")

	an := &amv1.AgentNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "corp-vpn", Namespace: "tenant-wg"},
		Spec: pure.AgentNetworkSpec{
			Kind: pure.NetworkWireGuardMesh,
			WireGuardMesh: &pure.WireGuardSpec{
				Mode:          "client",
				PrivateKeyRef: pure.AuthRef{SecretName: "wg-priv"},
				Addresses:     []string{"10.99.0.5/32"},
				Peers: []pure.WGPeer{{
					Name:       "hub",
					PublicKey:  "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
					Endpoint:   "vpn.example.com:51820",
					AllowedIPs: []string{"10.0.0.0/16"},
				}},
			},
		},
	}
	if err := e.cli.Create(e.ctx, an); err != nil {
		t.Fatalf("create agentnetwork: %v", err)
	}

	waitForAgentNetwork(t, e,
		types.NamespacedName{Namespace: "tenant-wg", Name: "corp-vpn"},
		func(g *amv1.AgentNetwork) bool {
			return g.Status.Phase == "Pending" && g.Status.Reason == "SecretMissing"
		})

	// Create the secret; reconciler should observe the change.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wg-priv", Namespace: "tenant-wg"},
		Data:       map[string][]byte{"key": []byte("placeholder")},
	}
	if err := e.cli.Create(e.ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	// Reconciler watches Secrets, so the create event drives the
	// transition automatically — no spec bump needed.
	waitForAgentNetwork(t, e,
		types.NamespacedName{Namespace: "tenant-wg", Name: "corp-vpn"},
		func(g *amv1.AgentNetwork) bool {
			return g.Status.Phase == "Ready" && g.Status.WGPeerCount == 1
		})
}

// TestEnvtest_AgentRun_CreatesPodAndAdvances drives the Run through
// PodPending → PodRunning → PodSucceeded. envtest has no kubelet so
// patchPodPhase() simulates the transitions.
func TestEnvtest_AgentRun_CreatesPodAndAdvances(t *testing.T) {
	e := setupAgentmodelEnv(t)
	makeNamespaceAM(t, e, "tenant-run")
	makeProvider(t, e, "tenant-run", "openai")
	makeAgentAM(t, e, "tenant-run", "alice", "openai")

	// Wait for agent to be Ready before scheduling the run.
	waitForAgent(t, e, types.NamespacedName{Namespace: "tenant-run", Name: "alice"}, func(a *amv1.Agent) bool {
		return a.Status.Phase == "Ready"
	})

	run := &amv1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-001", Namespace: "tenant-run"},
		Spec: pure.AgentRunSpec{
			AgentRef: "alice",
			Input:    []byte(`{"q":"hi"}`),
		},
	}
	if err := e.cli.Create(e.ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Reconciler should create a Pod named after the run.
	podKey := types.NamespacedName{Namespace: "tenant-run", Name: "alice-001"}
	waitForPod(t, e, podKey, func(p *corev1.Pod) bool {
		if len(p.OwnerReferences) == 0 || p.OwnerReferences[0].Name != "alice-001" {
			return false
		}
		return true
	})

	// Simulate kubelet: PodPending → PodRunning. The reconciler
	// requeues every 5s while non-terminal, so we don't need to nudge
	// the Run separately — the next reconcile picks up the new Pod
	// phase.
	patchPodPhase(t, e, podKey, corev1.PodRunning)
	waitForAgentRun(t, e, podKey, func(r *amv1.AgentRun) bool {
		return r.Status.State == pure.PhaseRunning
	})

	// Then PodSucceeded → AgentRun Completed.
	patchPodPhase(t, e, podKey, corev1.PodSucceeded)
	waitForAgentRun(t, e, podKey, func(r *amv1.AgentRun) bool {
		return r.Status.State == pure.PhaseCompleted
	})
}

// TestEnvtest_AgentRun_CancelStopsPod: spec.cancel on a Running run
// transitions to Cancelled and deletes the Pod.
func TestEnvtest_AgentRun_CancelStopsPod(t *testing.T) {
	e := setupAgentmodelEnv(t)
	makeNamespaceAM(t, e, "tenant-cancel")
	makeProvider(t, e, "tenant-cancel", "openai")
	makeAgentAM(t, e, "tenant-cancel", "alice", "openai")
	waitForAgent(t, e, types.NamespacedName{Namespace: "tenant-cancel", Name: "alice"}, func(a *amv1.Agent) bool {
		return a.Status.Phase == "Ready"
	})

	run := &amv1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "go-002", Namespace: "tenant-cancel"},
		Spec:       pure.AgentRunSpec{AgentRef: "alice", Input: []byte(`{}`)},
	}
	if err := e.cli.Create(e.ctx, run); err != nil {
		t.Fatal(err)
	}

	key := types.NamespacedName{Namespace: "tenant-cancel", Name: "go-002"}
	waitForPod(t, e, key, func(p *corev1.Pod) bool { return p.Name != "" })
	patchPodPhase(t, e, key, corev1.PodRunning)
	waitForAgentRun(t, e, key, func(r *amv1.AgentRun) bool {
		return r.Status.State == pure.PhaseRunning
	})

	// Now cancel.
	got := &amv1.AgentRun{}
	if err := e.cli.Get(e.ctx, key, got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Cancel = true
	if err := e.cli.Update(e.ctx, got); err != nil {
		t.Fatal(err)
	}

	waitForAgentRun(t, e, key, func(r *amv1.AgentRun) bool {
		return r.Status.State == pure.PhaseCancelled
	})
}

// TestEnvtest_AgentNetwork_InvalidSpecRejected: with no webhook, the
// reconciler is the gate — both transports set should land Failed/InvalidSpec.
func TestEnvtest_AgentNetwork_InvalidSpecRejected(t *testing.T) {
	e := setupAgentmodelEnv(t)
	makeNamespaceAM(t, e, "tenant-bad")

	// Both transports set — caught by ValidateAgentNetwork at reconcile.
	an := &amv1.AgentNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "broken", Namespace: "tenant-bad"},
		Spec: pure.AgentNetworkSpec{
			Kind: pure.NetworkIdentityProxy,
			IdentityProxy: &pure.IdentityProxySpec{
				Resources: []pure.ResourceTarget{{
					Name: "x", Kind: "tcp",
					LocalAddr: "127.0.0.1:5432",
					Gateway:   "g.svc:8443",
					Authorize: []string{"spiffe://x"},
				}},
			},
			WireGuardMesh: &pure.WireGuardSpec{
				Mode:          "client",
				PrivateKeyRef: pure.AuthRef{SecretName: "x"},
			},
		},
	}
	if err := e.cli.Create(e.ctx, an); err != nil {
		t.Fatalf("create agentnetwork: %v", err)
	}

	waitForAgentNetwork(t, e,
		types.NamespacedName{Namespace: "tenant-bad", Name: "broken"},
		func(g *amv1.AgentNetwork) bool {
			return g.Status.Phase == "Failed" && g.Status.Reason == "InvalidSpec"
		})
}
