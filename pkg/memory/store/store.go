// Package store provides the RetrieverStore interface used by the MCP gateway
// to resolve a retrieverRef to its MemoryRetriever configuration, and the
// worker base URL.
//
// The production implementation uses a controller-runtime client to look up
// MemoryRetriever resources from Kubernetes. A fake implementation is provided
// for tests.
package store

import (
	"context"
	"fmt"
	"strings"
	"sync"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
	"github.com/stigen/smol-agents/pkg/memory"
)

// RetrieverInfo bundles the resolved configuration for one MemoryRetriever.
type RetrieverInfo struct {
	// Spec is the full retriever configuration (policy, quota, etc.).
	Spec v1.MemoryRetrieverSpec

	// WorkerURL is the base URL of the retrieval worker that serves this retriever.
	// The gateway calls api.NewHTTPClient(WorkerURL, ...) to reach it.
	WorkerURL string
}

// RetrieverStore resolves a retrieverRef (namespace-qualified name such as
// "team-alpha/prod-knowledge") to its RetrieverInfo. Implementations must be
// safe for concurrent use.
//
// The interface is deliberately narrow so it can be satisfied by a Kubernetes
// client, a config-file reader, or a test fake without pulling in controller-
// runtime as a test dependency.
type RetrieverStore interface {
	// Get returns the RetrieverInfo for the given ref. Returns a typed
	// memory.NotFound error when the retriever does not exist or the caller
	// is not permitted to see it.
	Get(ctx context.Context, ref string) (RetrieverInfo, error)
}

// ── Fake implementation for tests ─────────────────────────────────────────

// FakeStore is a thread-safe in-memory RetrieverStore for tests.
type FakeStore struct {
	mu    sync.RWMutex
	items map[string]RetrieverInfo
}

// NewFakeStore returns an empty FakeStore.
func NewFakeStore() *FakeStore {
	return &FakeStore{items: make(map[string]RetrieverInfo)}
}

// Add registers a retriever under the given ref.
func (f *FakeStore) Add(ref string, info RetrieverInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[ref] = info
}

// Get implements RetrieverStore.
func (f *FakeStore) Get(_ context.Context, ref string) (RetrieverInfo, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	info, ok := f.items[ref]
	if !ok {
		return RetrieverInfo{}, memory.NotFound("retriever not found: " + ref)
	}
	return info, nil
}

// ── Parsing helper ─────────────────────────────────────────────────────────

// ParseRef splits a namespace-qualified retriever ref "ns/name" into its
// components. Returns an error if the format is invalid.
func ParseRef(ref string) (ns, name string, err error) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("retrieverRef %q must be namespace/name", ref)
	}
	return parts[0], parts[1], nil
}
