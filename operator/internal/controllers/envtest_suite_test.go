//go:build envtest

package controllers_test

import (
	"context"
	"os"
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

	v1 "github.com/stigen/smol-agents/operator/api/v1"
	"github.com/stigen/smol-agents/operator/internal/controllers"
)

// envTest is the shared test environment. It boots a real api-server
// + etcd via setup-envtest binaries, applies the operator's CRDs, and
// runs the controller against it. Run with:
//
//	make envtest
//
// or
//
//	KUBEBUILDER_ASSETS=$(setup-envtest use 1.31 -p path) \
//	  go test -tags=envtest -count=1 ./operator/internal/controllers/...
type envContext struct {
	env     *envtest.Environment
	cfg     *runtime.Scheme
	cli     client.Client
	cancel  context.CancelFunc
	ctx     context.Context
	stopMgr chan struct{}
}

func setupEnv(t *testing.T) *envContext {
	t.Helper()
	log.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(testWriter{t: t})))
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
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
		Scheme: scheme,
		// envtest reuses the package-global controller name registry
		// across tests; skip the validation so each subtest can spin up
		// its own manager + controller cleanly.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	r := &controllers.SmolAgentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: %v", err)
	}
	if err := (&controllers.AgentNodePoolReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("AgentNodePool SetupWithManager: %v", err)
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

	return &envContext{env: env, cfg: scheme, cli: cli, cancel: cancel, ctx: ctx, stopMgr: stop}
}

// projectRoot walks up from the test working dir until it finds go.mod.
// Robust against `go test` invocation paths.
func projectRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod from %s", dir)
		}
		dir = parent
	}
}

// applyPlatform creates a default singleton Platform CR.
func applyPlatform(t *testing.T, e *envContext) {
	t.Helper()
	p := &v1.SmolAgentPlatform{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       v1.SmolAgentPlatformSpec{DefaultTrustDomain: "stigen.ai"},
	}
	if err := e.cli.Create(e.ctx, p); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create platform: %v", err)
	}
}

func makeNamespace(t *testing.T, e *envContext, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := e.cli.Create(e.ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
}

// makeAgent applies a minimal SmolAgent and waits for it to exist.
func makeAgent(t *testing.T, e *envContext, ns, name string) *v1.SmolAgent {
	t.Helper()
	cr := &v1.SmolAgent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1.SmolAgentSpec{
			TrustDomain: "stigen.ai",
			Mode:        "strict",
			Features: v1.Features{
				Identity: v1.IdentityFeature{FeatureBase: v1.FeatureBase{Enabled: true}, Mode: "strict"},
				Sandbox:  v1.SandboxFeature{FeatureBase: v1.FeatureBase{Enabled: true}, RuntimeClass: "kata-fc"},
				Secrets:  v1.SecretsFeature{FeatureBase: v1.FeatureBase{Enabled: true}, MaxLeaseTTLSeconds: 60},
				Transport: v1.TransportFeature{
					Private: v1.TransportPrivateFeature{FeatureBase: v1.FeatureBase{Enabled: true}, Addr: "0.0.0.0:8443"},
				},
				EBPF:          v1.EBPFFeature{FeatureBase: v1.FeatureBase{Enabled: false}},
				Knative:       v1.KnativeFeature{FeatureBase: v1.FeatureBase{Enabled: true}},
				Observability: v1.ObservabilityFeature{FeatureBase: v1.FeatureBase{Enabled: true}, ServiceName: "test"},
			},
		},
	}
	if err := e.cli.Create(e.ctx, cr); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return cr
}

// waitFor polls the predicate until it returns true or the test
// timeout fires.
func waitFor(t *testing.T, e *envContext, key types.NamespacedName, pred func(*v1.SmolAgent) bool) {
	t.Helper()
	deadline := 30
	for i := 0; i < deadline; i++ {
		got := &v1.SmolAgent{}
		if err := e.cli.Get(e.ctx, key, got); err == nil && pred(got) {
			return
		}
		<-roundtrip()
	}
	t.Fatalf("timeout waiting for predicate on %s", key)
}

// roundtrip yields after the api-server's reconcile cycle (1s tick).
func roundtrip() <-chan struct{} {
	c := make(chan struct{})
	go func() {
		// 1 second is more than enough for envtest's apiserver to settle.
		<-time.After(time.Second)
		close(c)
	}()
	return c
}

// testWriter routes manager logs into testing.T.Log so failures are
// debuggable without a separate file.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
