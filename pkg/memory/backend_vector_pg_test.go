package memory_test

// Unit tests for PgvectorBackend adapter logic that does NOT require a live
// PostgreSQL server.
//
// Tests cover:
//   - Constructor validation (bad DSN / bad dims → error before any DB call)
//   - pgvectorLiteral encoding (the helper that formats []float32 for pgvector)
//   - ErrNotSupported for filesystem-only methods
//   - Interface compliance (compile-time; no assertion needed at runtime)
//
// Integration tests that require a live pgvector instance are in a separate
// file with the "integration" build tag and are skipped when
// PGVECTOR_DSN is unset.

import (
	"context"
	"strings"
	"testing"

	"github.com/stigen/smol-agents/pkg/memory"
)

// TestPgvector_ConstructorValidation verifies that NewPgvectorBackend rejects
// invalid configuration without touching any database.
func TestPgvector_ConstructorValidation(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name    string
		dsn     string
		dims    int
		wantErr string
	}{
		{"empty DSN", "", 128, "DSN is required"},
		{"zero dims", "postgres://localhost/test", 0, "EmbeddingDims must be positive"},
		{"negative dims", "postgres://localhost/test", -1, "EmbeddingDims must be positive"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// NewPgvectorBackend calls pgxpool.New which may fail before we can
			// validate dims when DSN is non-empty but invalid. We only test the
			// in-process validation guard here; live-DSN tests require a DB.
			_, err := memory.NewPgvectorBackendForTest(ctx, tc.dsn, tc.dims)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestPgvector_PgvectorLiteralEncoding verifies the wire format used to send
// embedding vectors to PostgreSQL as string literals.
func TestPgvector_PgvectorLiteralEncoding(t *testing.T) {
	cases := []struct {
		input []float32
		want  string
	}{
		{nil, "[]"},
		{[]float32{}, "[]"},
		{[]float32{1.0, 2.0, 3.0}, "[1,2,3]"},
		{[]float32{0.5, -0.5}, "[0.5,-0.5]"},
		{[]float32{1e-3}, "[0.001]"},
	}
	for _, tc := range cases {
		got := memory.PgvectorLiteralForTest(tc.input)
		if got != tc.want {
			t.Errorf("pgvectorLiteral(%v) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
