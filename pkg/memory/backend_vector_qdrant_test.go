package memory

import (
	"errors"
	"testing"
)

// x9i.5: each tenant maps to its OWN Qdrant collection, so two tenants' ops can
// never target the same collection — hard isolation above the payload filter +
// the read-guard (D1).
func TestQdrantCollectionFor_PerTenant(t *testing.T) {
	b := &QdrantBackend{cfg: QdrantConfig{Collection: "memory"}}
	a := b.collectionFor("tenant-a")
	c := b.collectionFor("tenant-b")
	if a != "memory-tenant-a" {
		t.Errorf("collectionFor(tenant-a) = %q, want memory-tenant-a", a)
	}
	if c != "memory-tenant-b" {
		t.Errorf("collectionFor(tenant-b) = %q, want memory-tenant-b", c)
	}
	if a == c {
		t.Fatal("two tenants must map to DIFFERENT collections (D1 hard isolation)")
	}
}

// isQdrantCollectionNotFound recognizes Qdrant's missing-collection message so a
// read for a tenant that never wrote returns empty, not a backend outage.
func TestIsQdrantCollectionNotFound(t *testing.T) {
	if !isQdrantCollectionNotFound(errors.New("Collection `memory-tenant-x` doesn't exist!")) {
		t.Error("should match Qdrant's missing-collection message")
	}
	if isQdrantCollectionNotFound(errors.New("rpc error: code = Unavailable")) {
		t.Error("must not match unrelated errors")
	}
	if isQdrantCollectionNotFound(nil) {
		t.Error("nil is not a not-found error")
	}
}
