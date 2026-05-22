package store_test

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1 "github.com/stigen/smol-agents/operator/api/agentmodel/v1"
	purev1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/memory"
	"github.com/stigen/smol-agents/pkg/memory/store"
)

// scheme registers the operator CRDs so the fake client recognises them.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := operatorv1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	// metav1 types are always needed.
	metav1.AddToGroupVersion(s, operatorv1.GroupVersion)
	return s
}

// makeRetriever creates a minimal MemoryRetriever with the given namespace/name
// and, optionally, the worker-URL annotation.
func makeRetriever(ns, name, workerURL string, spec purev1.MemoryRetrieverSpec) *operatorv1.MemoryRetriever {
	mr := &operatorv1.MemoryRetriever{
		TypeMeta: metav1.TypeMeta{
			APIVersion: operatorv1.GroupVersion.String(),
			Kind:       "MemoryRetriever",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
		Spec: spec,
	}
	if workerURL != "" {
		mr.Annotations = map[string]string{
			store.WorkerURLAnnotation: workerURL,
		}
	}
	return mr
}

// TestK8sStore_GetHappy tests the normal read-from-cluster path.
func TestK8sStore_GetHappy(t *testing.T) {
	scheme := newScheme(t)
	spec := purev1.MemoryRetrieverSpec{
		Stores: []string{"my-store"},
		TopK:   5,
		Tenant: "team-alpha",
	}
	mr := makeRetriever("team-alpha", "prod-knowledge", "http://worker:8080", spec)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mr).
		Build()

	s, err := store.NewK8sStore(store.K8sStoreConfig{
		Client:   fakeClient,
		CacheTTL: -1, // disable cache so we can test the live path
	})
	if err != nil {
		t.Fatalf("NewK8sStore: %v", err)
	}

	info, err := s.Get(context.Background(), "team-alpha/prod-knowledge")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.WorkerURL != "http://worker:8080" {
		t.Errorf("WorkerURL = %q, want http://worker:8080", info.WorkerURL)
	}
	if info.Spec.Tenant != "team-alpha" {
		t.Errorf("Spec.Tenant = %q, want team-alpha", info.Spec.Tenant)
	}
	if info.Spec.TopK != 5 {
		t.Errorf("Spec.TopK = %d, want 5", info.Spec.TopK)
	}
}

// TestK8sStore_GetNotFound tests that a missing CR returns a typed NotFound error.
func TestK8sStore_GetNotFound(t *testing.T) {
	s, err := store.NewK8sStore(store.K8sStoreConfig{
		Client: fake.NewClientBuilder().
			WithScheme(newScheme(t)).
			Build(),
		CacheTTL: -1,
	})
	if err != nil {
		t.Fatalf("NewK8sStore: %v", err)
	}

	_, err = s.Get(context.Background(), "ns/does-not-exist")
	if err == nil {
		t.Fatal("expected error for missing MemoryRetriever, got nil")
	}
	if memory.KindOf(err) != memory.KindNotFound {
		t.Errorf("error kind = %q, want %q", memory.KindOf(err), memory.KindNotFound)
	}
}

// TestK8sStore_GetInvalidRef tests that a malformed ref returns a typed Invalid error.
func TestK8sStore_GetInvalidRef(t *testing.T) {
	s, err := store.NewK8sStore(store.K8sStoreConfig{
		Client: fake.NewClientBuilder().
			WithScheme(newScheme(t)).
			Build(),
		CacheTTL: -1,
	})
	if err != nil {
		t.Fatalf("NewK8sStore: %v", err)
	}

	for _, bad := range []string{"no-slash", "", "/", "ns/", "/name"} {
		_, gotErr := s.Get(context.Background(), bad)
		if gotErr == nil {
			t.Errorf("ref %q: expected error, got nil", bad)
			continue
		}
		if memory.KindOf(gotErr) != memory.KindInvalid {
			t.Errorf("ref %q: kind = %q, want KindInvalid", bad, memory.KindOf(gotErr))
		}
	}
}

// TestK8sStore_GetNoWorkerURL tests that a CR without the worker-URL annotation
// returns BackendUnavailable (not a panic or nil).
func TestK8sStore_GetNoWorkerURL(t *testing.T) {
	spec := purev1.MemoryRetrieverSpec{Stores: []string{"s"}, TopK: 10}
	mr := makeRetriever("ns", "r", "" /* no annotation */, spec)

	s, err := store.NewK8sStore(store.K8sStoreConfig{
		Client: fake.NewClientBuilder().
			WithScheme(newScheme(t)).
			WithObjects(mr).
			Build(),
		CacheTTL: -1,
	})
	if err != nil {
		t.Fatalf("NewK8sStore: %v", err)
	}

	_, gotErr := s.Get(context.Background(), "ns/r")
	if gotErr == nil {
		t.Fatal("expected BackendUnavailable error for missing annotation")
	}
	if memory.KindOf(gotErr) != memory.KindBackendUnavailable {
		t.Errorf("error kind = %q, want KindBackendUnavailable", memory.KindOf(gotErr))
	}
}

// TestK8sStore_FallbackWorkerURL tests that WorkerURLFallback is used when
// the annotation is absent.
func TestK8sStore_FallbackWorkerURL(t *testing.T) {
	spec := purev1.MemoryRetrieverSpec{Stores: []string{"s"}, TopK: 10}
	mr := makeRetriever("ns", "r", "" /* no annotation */, spec)

	s, err := store.NewK8sStore(store.K8sStoreConfig{
		Client: fake.NewClientBuilder().
			WithScheme(newScheme(t)).
			WithObjects(mr).
			Build(),
		CacheTTL:          -1,
		WorkerURLFallback: "http://fallback:9090",
	})
	if err != nil {
		t.Fatalf("NewK8sStore: %v", err)
	}

	info, err := s.Get(context.Background(), "ns/r")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.WorkerURL != "http://fallback:9090" {
		t.Errorf("WorkerURL = %q, want fallback", info.WorkerURL)
	}
}

// TestK8sStore_CacheHit tests that a second call within TTL does not hit the
// fake client (by removing the object and verifying the result still comes back
// from cache).
func TestK8sStore_CacheHit(t *testing.T) {
	spec := purev1.MemoryRetrieverSpec{Stores: []string{"s"}, TopK: 10}
	mr := makeRetriever("ns", "cached", "http://worker:8080", spec)

	fakeClient := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(mr).
		Build()

	s, err := store.NewK8sStore(store.K8sStoreConfig{
		Client:   fakeClient,
		CacheTTL: 30 * time.Second, // enabled
	})
	if err != nil {
		t.Fatalf("NewK8sStore: %v", err)
	}

	// First call: populates cache.
	info, err := s.Get(context.Background(), "ns/cached")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if info.WorkerURL != "http://worker:8080" {
		t.Errorf("first Get WorkerURL = %q", info.WorkerURL)
	}

	// Delete the object from the fake cluster — a live call would return NotFound.
	if delErr := fakeClient.Delete(context.Background(), mr); delErr != nil {
		t.Fatalf("Delete: %v", delErr)
	}

	// Second call: should return cached result, not NotFound.
	info2, err := s.Get(context.Background(), "ns/cached")
	if err != nil {
		t.Fatalf("second Get (should be cached): %v", err)
	}
	if info2.WorkerURL != "http://worker:8080" {
		t.Errorf("cached WorkerURL = %q", info2.WorkerURL)
	}
}

// TestK8sStore_InvalidateForcesRefresh tests that Invalidate evicts the cache
// so the next Get fetches from the cluster.
func TestK8sStore_InvalidateForcesRefresh(t *testing.T) {
	spec := purev1.MemoryRetrieverSpec{Stores: []string{"s"}, TopK: 10}
	mr := makeRetriever("ns", "inv", "http://worker:8080", spec)

	fakeClient := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(mr).
		Build()

	s, err := store.NewK8sStore(store.K8sStoreConfig{
		Client:   fakeClient,
		CacheTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewK8sStore: %v", err)
	}

	// Populate cache.
	if _, err := s.Get(context.Background(), "ns/inv"); err != nil {
		t.Fatalf("first Get: %v", err)
	}

	// Delete from cluster.
	if err := fakeClient.Delete(context.Background(), mr); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Invalidate cache entry.
	s.Invalidate("ns/inv")

	// Next Get must see NotFound (live cluster read).
	_, err = s.Get(context.Background(), "ns/inv")
	if err == nil {
		t.Fatal("expected NotFound after Invalidate, got nil")
	}
	if memory.KindOf(err) != memory.KindNotFound {
		t.Errorf("error kind = %q, want KindNotFound", memory.KindOf(err))
	}
}

// TestK8sStore_NilClientError tests that NewK8sStore rejects a nil Client.
func TestK8sStore_NilClientError(t *testing.T) {
	_, err := store.NewK8sStore(store.K8sStoreConfig{Client: nil})
	if err == nil {
		t.Fatal("expected error for nil Client")
	}
}
