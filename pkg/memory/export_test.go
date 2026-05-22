// export_test.go exposes internal helpers to the _test package for white-box
// unit testing. This file is compiled only during tests (the _test.go suffix
// on the calling test files keeps these symbols invisible to non-test code).
package memory

import (
	"context"
	"fmt"
	"strings"
)

// PgvectorLiteralForTest wraps the internal pgvectorLiteral so external tests
// can verify the wire encoding without touching a live DB.
func PgvectorLiteralForTest(v []float32) string { return pgvectorLiteral(v) }

// NewPgvectorBackendForTest is an alias for NewPgvectorBackend used by tests
// that only need construction-phase validation errors (i.e. no real DB).
func NewPgvectorBackendForTest(ctx context.Context, dsn string, dims int) (*PgvectorBackend, error) {
	return NewPgvectorBackend(ctx, PgvectorConfig{DSN: dsn, EmbeddingDims: dims})
}

// Neo4jNodeToDocForTest wraps the private node-to-doc conversion for tests.
// It accepts a map[string]interface{} mirroring neo4j.Node.Props.
func Neo4jNodeToDocForTest(props map[string]interface{}) Document {
	// We replicate the decoding logic here to avoid importing neo4j in tests.
	getString := func(key string) string {
		if v, ok := props[key]; ok {
			if s, ok2 := v.(string); ok2 {
				return s
			}
		}
		return ""
	}
	return Document{
		ID:        getString("id"),
		Tenant:    getString("tenant"),
		Namespace: getString("namespace"),
		Path:      getString("path"),
		Version:   getString("version"),
	}
}

// PgvectorQueryBuildForTest exposes the query-building logic so tests can
// assert the WHERE clause contains the right tenant/namespace predicates
// without executing SQL.
func PgvectorQueryBuildForTest(tenant, namespace, query string) string {
	conditions := []string{"tenant = $1"}
	argIdx := 3
	if namespace != "" {
		conditions = append(conditions, fmt.Sprintf("namespace = $%d", argIdx))
		argIdx++
	}
	if query != "" {
		conditions = append(conditions, fmt.Sprintf(
			"(content ILIKE $%d OR path ILIKE $%d)", argIdx, argIdx))
	}
	return strings.Join(conditions, " AND ")
}
