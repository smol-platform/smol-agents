package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1 "github.com/stigen/smol-agents/operator/api/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/memory"
)

// WorkerURLAnnotation is the annotation the operator sets on a MemoryRetriever
// to communicate the worker base URL to the gateway without polluting Spec.
//
// Convention: the operator writes this annotation on the MemoryRetriever CR
// during reconciliation, once the worker Deployment + Service are ready.
// Format: "https://worker-svc.namespace.svc.cluster.local:8080" (no trailing slash).
//
// The annotation key is in the stigen.ai namespace to avoid collisions with
// other controllers.
const WorkerURLAnnotation = "runtime.agents.stigen.ai/worker-url"

// K8sStoreConfig parameterises the Kubernetes RetrieverStore.
type K8sStoreConfig struct {
	// Client is a controller-runtime client. Required.
	Client client.Client

	// CacheTTL is how long a successfully resolved RetrieverInfo is cached.
	// Zero defaults to 30 seconds. Use a negative value to disable caching
	// (useful in tests).
	CacheTTL time.Duration

	// WorkerURLFallback is used when the WorkerURLAnnotation is absent.
	// Typically empty in production (the annotation is always set by the operator).
	// Provided so the file-config and k8s paths can be combined in dev mode.
	WorkerURLFallback string
}

// K8sStore is a RetrieverStore backed by a controller-runtime client.
// It reads MemoryRetriever CRs from the cluster and caches successful lookups
// for a configurable duration to amortise Kubernetes API calls on the hot path.
//
// Thread-safe; a single instance can serve all concurrent gateway requests.
type K8sStore struct {
	cfg   K8sStoreConfig
	ttl   time.Duration
	mu    sync.RWMutex
	cache map[string]k8sCacheEntry
}

type k8sCacheEntry struct {
	info      RetrieverInfo
	expiresAt time.Time
}

// NewK8sStore constructs a K8sStore from cfg. A nil Client returns an error.
func NewK8sStore(cfg K8sStoreConfig) (*K8sStore, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("K8sStoreConfig.Client is required")
	}
	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	return &K8sStore{
		cfg:   cfg,
		ttl:   ttl,
		cache: make(map[string]k8sCacheEntry),
	}, nil
}

// Get implements RetrieverStore. The ref must be "namespace/name" format,
// matching the MemoryRetriever CR in that Kubernetes namespace.
//
// On success, the result is cached for K8sStoreConfig.CacheTTL.
// On any error (including NotFound), the result is not cached so transient
// failures do not freeze the gateway.
func (s *K8sStore) Get(ctx context.Context, ref string) (RetrieverInfo, error) {
	ns, name, err := ParseRef(ref)
	if err != nil {
		return RetrieverInfo{}, memory.Invalid(err.Error())
	}

	// Fast path: cached entry still valid.
	if s.ttl > 0 {
		if info, ok := s.cached(ref); ok {
			return info, nil
		}
	}

	// Slow path: fetch from the Kubernetes API.
	var mr operatorv1.MemoryRetriever
	key := client.ObjectKey{Namespace: ns, Name: name}
	if err := s.cfg.Client.Get(ctx, key, &mr); err != nil {
		if isNotFound(err) {
			return RetrieverInfo{}, memory.NotFound(fmt.Sprintf("MemoryRetriever %q not found", ref))
		}
		return RetrieverInfo{}, memory.BackendUnavailable(
			fmt.Sprintf("get MemoryRetriever %q: %v", ref, err))
	}

	workerURL := workerURLFrom(&mr, s.cfg.WorkerURLFallback)
	if workerURL == "" {
		return RetrieverInfo{}, memory.BackendUnavailable(
			fmt.Sprintf("MemoryRetriever %q has no worker URL (annotation %q not set by operator)", ref, WorkerURLAnnotation))
	}

	info := RetrieverInfo{
		Spec:      mr.Spec,
		WorkerURL: workerURL,
	}

	if s.ttl > 0 {
		s.store(ref, info)
	}
	return info, nil
}

// Invalidate evicts the cached entry for ref, if any. Useful after an operator
// reconcile loop updates the MemoryRetriever.
func (s *K8sStore) Invalidate(ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, ref)
}

// ── cache helpers ──────────────────────────────────────────────────────────

func (s *K8sStore) cached(ref string) (RetrieverInfo, bool) {
	s.mu.RLock()
	e, ok := s.cache[ref]
	s.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return RetrieverInfo{}, false
	}
	return e.info, true
}

func (s *K8sStore) store(ref string, info RetrieverInfo) {
	s.mu.Lock()
	s.cache[ref] = k8sCacheEntry{info: info, expiresAt: time.Now().Add(s.ttl)}
	s.mu.Unlock()
}

// ── helpers ────────────────────────────────────────────────────────────────

// workerURLFrom extracts the worker URL from the MemoryRetriever CR.
// The operator writes WorkerURLAnnotation; if absent, fallback is used.
func workerURLFrom(mr *operatorv1.MemoryRetriever, fallback string) string {
	if mr.Annotations != nil {
		if u, ok := mr.Annotations[WorkerURLAnnotation]; ok && u != "" {
			return u
		}
	}
	return fallback
}

// isNotFound returns true for Kubernetes 404 / NotFound errors.
// We avoid importing k8s.io/apimachinery/pkg/api/errors directly here;
// instead we use the controller-runtime client.IgnoreNotFound sentinel.
func isNotFound(err error) bool {
	// controller-runtime re-exports k8s apierrors; the idiomatic check is:
	//   apierrors.IsNotFound(err)
	// We replicate the sentinel-style check here to avoid a new import.
	// The string "not found" is stable across k8s.io/apimachinery versions.
	if err == nil {
		return false
	}
	// Use client.IgnoreNotFound: if it suppresses the error, the err was NotFound.
	return client.IgnoreNotFound(err) == nil
}
