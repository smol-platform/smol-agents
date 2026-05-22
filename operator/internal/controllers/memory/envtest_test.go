//go:build envtest

// Package memory — envtest integration tests for MemoryRetrieverReconciler.
//
// Run with:
//
//	KUBEBUILDER_ASSETS=$(setup-envtest use 1.31 -p path) \
//	  go test -tags=envtest -count=1 ./operator/internal/controllers/memory/...
package memory_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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

	amv1 "github.com/stigen/smol-agents/operator/api/agentmodel/v1"
	"github.com/stigen/smol-agents/operator/internal/controllers/memory"
	pure "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

// memoryEnv is a self-contained envtest environment for the memory controller
// suite. It is independent of the parent controllers_test suite so no shared
// manager state bleeds across.
type memoryEnv struct {
	env *envtest.Environment
	cli client.Client
	ctx context.Context
}

func setupMemoryEnv(t *testing.T) *memoryEnv {
	t.Helper()
	log.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(testWriter{t: t})))

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := amv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	// Walk up to the repo root (go.mod).
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

	r := &memory.MemoryRetrieverReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		WorkerImage: "memory-worker:test",
		MCPImage:    "memory-mcp:test",
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager: %v", err)
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

	return &memoryEnv{env: env, cli: cli, ctx: ctx}
}

// projectRoot walks up until it finds go.mod, mirroring the helper in the
// parent suite.
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

// testWriter routes manager log output into testing.T.Log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

// makeNS creates a Namespace, ignoring already-exists.
func makeNS(t *testing.T, e *memoryEnv, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := e.cli.Create(e.ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %q: %v", name, err)
	}
}

// makeStore creates a MemoryStore in the given namespace.
func makeStore(t *testing.T, e *memoryEnv, ns, name string) *amv1.MemoryStore {
	t.Helper()
	store := &amv1.MemoryStore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: pure.MemoryStoreSpec{
			Kind:     pure.MemoryStoreVector,
			Driver:   pure.MemoryDriverQdrant,
			Endpoint: "qdrant.svc:6333",
			Auth:     &pure.AuthRef{SecretName: "qdrant-creds"},
			Tenancy:  pure.TenancySpec{Model: pure.TenancyDedicated},
		},
	}
	if err := e.cli.Create(e.ctx, store); err != nil {
		t.Fatalf("create MemoryStore %q: %v", name, err)
	}
	return store
}

// waitForRetriever polls until the predicate returns true or 30 s elapses.
func waitForRetriever(t *testing.T, e *memoryEnv, key types.NamespacedName, pred func(*amv1.MemoryRetriever) bool) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	last := &amv1.MemoryRetriever{}
	for {
		got := &amv1.MemoryRetriever{}
		if err := e.cli.Get(e.ctx, key, got); err == nil {
			last = got
			if pred(got) {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for MemoryRetriever %s; last phase=%q reason=%q",
				key, last.Status.Phase, last.Status.Reason)
		case <-tick.C:
		}
	}
}

// TestEnvtest_Memory_HappyPath: apply a MemoryStore + MemoryRetriever; the
// reconciler should create worker Deployment, worker Service, MCP Deployment,
// MCP Service, ServiceAccount and set phase=Ready.
func TestEnvtest_Memory_HappyPath(t *testing.T) {
	e := setupMemoryEnv(t)
	makeNS(t, e, "mem-happy")
	makeStore(t, e, "mem-happy", "vec-store")

	ret := &amv1.MemoryRetriever{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ret", Namespace: "mem-happy"},
		Spec: pure.MemoryRetrieverSpec{
			Stores:           []string{"vec-store"},
			ModelProviderRef: "embed-provider",
			TopK:             5,
		},
	}
	if err := e.cli.Create(e.ctx, ret); err != nil {
		t.Fatalf("create MemoryRetriever: %v", err)
	}

	key := types.NamespacedName{Namespace: "mem-happy", Name: "my-ret"}
	waitForRetriever(t, e, key, func(r *amv1.MemoryRetriever) bool {
		return r.Status.Phase == "Ready" && r.Status.BoundWorkers == 1
	})

	// Verify the worker Deployment was created.
	d := &appsv1.Deployment{}
	if err := e.cli.Get(e.ctx, types.NamespacedName{
		Namespace: "mem-happy", Name: "mr-my-ret-worker",
	}, d); err != nil {
		t.Errorf("worker Deployment not found: %v", err)
	}
	// Owner reference points back to the MemoryRetriever.
	if len(d.OwnerReferences) == 0 || d.OwnerReferences[0].Name != "my-ret" {
		t.Errorf("worker Deployment missing OwnerReference to MemoryRetriever")
	}

	// MCP Deployment created.
	mcp := &appsv1.Deployment{}
	if err := e.cli.Get(e.ctx, types.NamespacedName{
		Namespace: "mem-happy", Name: "mr-my-ret-mcp",
	}, mcp); err != nil {
		t.Errorf("mcp Deployment not found: %v", err)
	}

	// MCP Service created.
	mcpSvc := &corev1.Service{}
	if err := e.cli.Get(e.ctx, types.NamespacedName{
		Namespace: "mem-happy", Name: "mr-my-ret-mcp",
	}, mcpSvc); err != nil {
		t.Errorf("mcp Service not found: %v", err)
	}

	// Worker Service created and headless.
	wSvc := &corev1.Service{}
	if err := e.cli.Get(e.ctx, types.NamespacedName{
		Namespace: "mem-happy", Name: "mr-my-ret-worker",
	}, wSvc); err != nil {
		t.Errorf("worker Service not found: %v", err)
	}
	if wSvc.Spec.ClusterIP != "None" {
		t.Errorf("worker Service ClusterIP = %q, want None", wSvc.Spec.ClusterIP)
	}

	// ServiceAccount created.
	sa := &corev1.ServiceAccount{}
	if err := e.cli.Get(e.ctx, types.NamespacedName{
		Namespace: "mem-happy", Name: "mr-my-ret-sa",
	}, sa); err != nil {
		t.Errorf("ServiceAccount not found: %v", err)
	}
}

// TestEnvtest_Memory_PendingWhenStoreMissing: a MemoryRetriever that
// references a non-existent MemoryStore must stay Pending with
// condition StoresBound=False and reason StoreMissing.
func TestEnvtest_Memory_PendingWhenStoreMissing(t *testing.T) {
	e := setupMemoryEnv(t)
	makeNS(t, e, "mem-pending")

	ret := &amv1.MemoryRetriever{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-ret", Namespace: "mem-pending"},
		Spec: pure.MemoryRetrieverSpec{
			Stores: []string{"does-not-exist"},
			TopK:   5,
		},
	}
	if err := e.cli.Create(e.ctx, ret); err != nil {
		t.Fatalf("create MemoryRetriever: %v", err)
	}

	key := types.NamespacedName{Namespace: "mem-pending", Name: "orphan-ret"}
	waitForRetriever(t, e, key, func(r *amv1.MemoryRetriever) bool {
		return r.Status.Phase == "Pending"
	})

	// Check the StoresBound condition.
	got := &amv1.MemoryRetriever{}
	if err := e.cli.Get(e.ctx, key, got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range got.Status.Conditions {
		if c.Type == "StoresBound" && c.Status == "False" && c.Reason == "StoreMissing" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected StoresBound=False/StoreMissing condition; got: %v", got.Status.Conditions)
	}

	// Now create the missing store — the reconciler requeues and transitions to Ready.
	makeStore(t, e, "mem-pending", "does-not-exist")
	waitForRetriever(t, e, key, func(r *amv1.MemoryRetriever) bool {
		return r.Status.Phase == "Ready"
	})
}

// TestEnvtest_Memory_Teardown: deleting a MemoryRetriever must cascade-delete
// only its owned resources. The MemoryStore must remain untouched.
func TestEnvtest_Memory_Teardown(t *testing.T) {
	e := setupMemoryEnv(t)
	makeNS(t, e, "mem-teardown")
	makeStore(t, e, "mem-teardown", "shared-store")

	ret := &amv1.MemoryRetriever{
		ObjectMeta: metav1.ObjectMeta{Name: "to-delete", Namespace: "mem-teardown"},
		Spec: pure.MemoryRetrieverSpec{
			Stores: []string{"shared-store"},
			TopK:   5,
		},
	}
	if err := e.cli.Create(e.ctx, ret); err != nil {
		t.Fatalf("create MemoryRetriever: %v", err)
	}

	key := types.NamespacedName{Namespace: "mem-teardown", Name: "to-delete"}
	waitForRetriever(t, e, key, func(r *amv1.MemoryRetriever) bool {
		return r.Status.Phase == "Ready"
	})

	// Delete the MemoryRetriever.
	got := &amv1.MemoryRetriever{}
	if err := e.cli.Get(e.ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if err := e.cli.Delete(e.ctx, got); err != nil {
		t.Fatalf("delete MemoryRetriever: %v", err)
	}

	// Wait for the worker Deployment to disappear (owner ref cascade).
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		d := &appsv1.Deployment{}
		err := e.cli.Get(e.ctx, types.NamespacedName{
			Namespace: "mem-teardown", Name: "mr-to-delete-worker",
		}, d)
		if apierrors.IsNotFound(err) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout: worker Deployment not deleted after MemoryRetriever deletion")
		case <-tick.C:
		}
	}

	// The MemoryStore must still exist.
	store := &amv1.MemoryStore{}
	if err := e.cli.Get(e.ctx, types.NamespacedName{
		Namespace: "mem-teardown", Name: "shared-store",
	}, store); err != nil {
		t.Errorf("MemoryStore was unexpectedly deleted: %v", err)
	}
}
