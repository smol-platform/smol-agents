package secrets

import (
	"context"
	"sync"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// StaticBackend is an in-memory secret store useful for tests and demos.
//
// Keys are namespaced by SPIFFE ID, so the same secret name can yield
// different material for different principals.
type StaticBackend struct {
	mu      sync.RWMutex
	store   map[string]map[string][]byte // principal → name → bytes
	failure error                        // if non-nil, all Fetch calls return this
}

func NewStaticBackend() *StaticBackend {
	return &StaticBackend{store: make(map[string]map[string][]byte)}
}

// Set adds a secret for principal/name.
func (b *StaticBackend) Set(principal spiffeid.ID, name string, value []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := principal.String()
	if b.store[key] == nil {
		b.store[key] = make(map[string][]byte)
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	b.store[key][name] = cp
}

// SetGlobal adds a secret available to any principal (subject to Policy).
func (b *StaticBackend) SetGlobal(name string, value []byte) {
	b.Set(spiffeid.ID{}, name, value)
}

// SetFailure causes all Fetch calls to return err. Pass nil to recover.
func (b *StaticBackend) SetFailure(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failure = err
}

func (b *StaticBackend) Fetch(_ context.Context, principal spiffeid.ID, name string) ([]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.failure != nil {
		return nil, b.failure
	}
	if m, ok := b.store[principal.String()]; ok {
		if v, ok := m[name]; ok {
			return append([]byte(nil), v...), nil
		}
	}
	if m, ok := b.store[""]; ok {
		if v, ok := m[name]; ok {
			return append([]byte(nil), v...), nil
		}
	}
	return nil, ErrNotFound
}

func (b *StaticBackend) Close() error { return nil }
