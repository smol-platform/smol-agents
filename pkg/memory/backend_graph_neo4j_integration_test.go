//go:build integration

// Integration tests for Neo4jBackend. Require a live Neo4j instance.
// Skipped when NEO4J_URI is unset.
//
// Usage:
//
//	NEO4J_URI="bolt://localhost:7687" NEO4J_USER="neo4j" NEO4J_PASS="password" \
//	  go test -tags integration ./pkg/memory/... -run TestNeo4jIntegration
package memory_test

import (
	"context"
	"os"
	"testing"

	"github.com/stigen/smol-agents/pkg/memory"
)

func TestNeo4jIntegration_WriteGetDelete(t *testing.T) {
	uri := os.Getenv("NEO4J_URI")
	if uri == "" {
		t.Skip("NEO4J_URI not set; skipping Neo4j integration tests")
	}
	user := os.Getenv("NEO4J_USER")
	pass := os.Getenv("NEO4J_PASS")

	ctx := context.Background()
	b, err := memory.NewNeo4jBackend(ctx, memory.Neo4jConfig{
		URI:      uri,
		Username: user,
		Password: pass,
	})
	if err != nil {
		t.Fatalf("NewNeo4jBackend: %v", err)
	}
	defer func() { _ = b.Close(ctx) }()

	if err := b.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	wr, err := b.Write(ctx, memory.Document{
		Tenant:    "neo4j-tenant",
		Namespace: "integ-ns",
		Content:   []byte("neo4j integration test"),
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := b.Get(ctx, wr.ID, memory.Filter{Tenant: "neo4j-tenant", Namespace: "integ-ns"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Content) != "neo4j integration test" {
		t.Errorf("content = %q", got.Content)
	}

	// Cross-tenant isolation.
	_, err = b.Get(ctx, wr.ID, memory.Filter{Tenant: "other-tenant", Namespace: "integ-ns"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Errorf("cross-tenant Get must return NotFound, got %v", err)
	}

	if err := b.Delete(ctx, wr.ID, memory.Filter{Tenant: "neo4j-tenant", Namespace: "integ-ns"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestNeo4jIntegration_ListNamespaces(t *testing.T) {
	uri := os.Getenv("NEO4J_URI")
	if uri == "" {
		t.Skip("NEO4J_URI not set")
	}
	user, pass := os.Getenv("NEO4J_USER"), os.Getenv("NEO4J_PASS")
	ctx := context.Background()
	b, err := memory.NewNeo4jBackend(ctx, memory.Neo4jConfig{
		URI:      uri,
		Username: user,
		Password: pass,
	})
	if err != nil {
		t.Fatalf("NewNeo4jBackend: %v", err)
	}
	defer func() { _ = b.Close(ctx) }()
	_ = b.EnsureSchema(ctx)

	tenant := "neo4j-ns-integ-tenant"
	for _, ns := range []string{"alpha", "beta"} {
		_, _ = b.Write(ctx, memory.Document{
			Tenant:    tenant,
			Namespace: ns,
			Content:   []byte("x"),
		})
	}

	nss, err := b.ListNamespaces(ctx, memory.Filter{Tenant: tenant})
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	got := map[string]bool{}
	for _, ns := range nss {
		got[ns] = true
	}
	if !got["alpha"] || !got["beta"] {
		t.Errorf("expected alpha+beta, got %v", nss)
	}
}
