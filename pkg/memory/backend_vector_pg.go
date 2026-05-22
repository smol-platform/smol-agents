// Package memory — pgvector Backend adapter.
//
// PgvectorBackend implements memory.Backend over a PostgreSQL database with
// the pgvector extension. It uses github.com/jackc/pgx/v5 for the database
// driver.
//
// Schema (auto-created via EnsureSchema):
//
//	CREATE TABLE IF NOT EXISTS memory_documents (
//	  id          TEXT NOT NULL,
//	  tenant      TEXT NOT NULL,
//	  namespace   TEXT NOT NULL,
//	  content     BYTEA NOT NULL,
//	  path        TEXT NOT NULL DEFAULT '',
//	  metadata    JSONB NOT NULL DEFAULT '{}',
//	  version     TEXT NOT NULL,
//	  created_at  TIMESTAMPTZ NOT NULL,
//	  updated_at  TIMESTAMPTZ NOT NULL,
//	  embedding   VECTOR(<dims>),
//	  PRIMARY KEY (id, tenant)
//	);
//	CREATE INDEX IF NOT EXISTS memory_tenant_ns ON memory_documents (tenant, namespace);
//	CREATE INDEX IF NOT EXISTS memory_embedding_idx ON memory_documents USING ivfflat (embedding vector_cosine_ops);
//
// Tenant + namespace isolation is enforced in every query via WHERE clauses
// (R-MEM-WORK-1, R-MEM-SEC-1). FS-only ops return *ErrNotSupported.
//
// Implements R-MEM-WORK-2.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgvectorConfig holds the configuration for a PgvectorBackend.
type PgvectorConfig struct {
	// DSN is the PostgreSQL connection string, e.g.
	// "postgres://user:pass@host:5432/dbname?sslmode=require"
	// Credentials MUST be broker-resolved before constructing the backend.
	DSN string

	// EmbeddingDims is the dimensionality of embedding vectors stored in the
	// pgvector VECTOR column. Must match the Embedder's Dims().
	EmbeddingDims int

	// TableName is the name of the documents table. Defaults to
	// "memory_documents" when empty.
	TableName string

	// MaxResults is the maximum topK the backend will serve. 0 means no cap
	// (the gateway has already clamped; this is a defence-in-depth backstop).
	MaxResults int
}

func (c *PgvectorConfig) tableName() string {
	if c.TableName == "" {
		return "memory_documents"
	}
	return c.TableName
}

// PgvectorBackend is a memory.Backend backed by PostgreSQL + pgvector.
// Construct with NewPgvectorBackend; call EnsureSchema once at startup.
type PgvectorBackend struct {
	cfg  PgvectorConfig
	pool *pgxpool.Pool
}

// NewPgvectorBackend opens a pgxpool and returns a PgvectorBackend.
// Call EnsureSchema to create the table/indexes before first use.
func NewPgvectorBackend(ctx context.Context, cfg PgvectorConfig) (*PgvectorBackend, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("pgvector: DSN is required")
	}
	if cfg.EmbeddingDims <= 0 {
		return nil, fmt.Errorf("pgvector: EmbeddingDims must be positive")
	}
	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("pgvector: open pool: %w", err)
	}
	return &PgvectorBackend{cfg: cfg, pool: pool}, nil
}

// Close releases the underlying connection pool.
func (b *PgvectorBackend) Close() { b.pool.Close() }

// EnsureSchema creates the documents table and indexes idempotently. Safe to
// call on every startup.
func (b *PgvectorBackend) EnsureSchema(ctx context.Context) error {
	tbl := b.cfg.tableName()
	dims := b.cfg.EmbeddingDims
	_, err := b.pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	if err != nil {
		return fmt.Errorf("pgvector: enable vector extension: %w", err)
	}
	_, err = b.pool.Exec(ctx, fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
  id          TEXT NOT NULL,
  tenant      TEXT NOT NULL,
  namespace   TEXT NOT NULL,
  content     BYTEA NOT NULL,
  path        TEXT NOT NULL DEFAULT '',
  metadata    JSONB NOT NULL DEFAULT '{}',
  version     TEXT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL,
  updated_at  TIMESTAMPTZ NOT NULL,
  embedding   VECTOR(%d),
  PRIMARY KEY (id, tenant)
)`, tbl, dims))
	if err != nil {
		return fmt.Errorf("pgvector: create table: %w", err)
	}
	_, err = b.pool.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS memory_tenant_ns ON %s (tenant, namespace)`, tbl))
	if err != nil {
		return fmt.Errorf("pgvector: create tenant_ns index: %w", err)
	}
	// The ivfflat index requires at least one row; use CONCURRENTLY to be safe.
	_, err = b.pool.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS memory_embedding_idx ON %s USING ivfflat (embedding vector_cosine_ops) WHERE embedding IS NOT NULL`, tbl))
	if err != nil {
		return fmt.Errorf("pgvector: create embedding index: %w", err)
	}
	return nil
}

// ── Backend.Write ─────────────────────────────────────────────────────────────

func (b *PgvectorBackend) Write(ctx context.Context, doc Document) (WriteResult, error) {
	if doc.Tenant == "" || doc.Namespace == "" {
		return WriteResult{}, Invalid("pgvector write: tenant and namespace are required")
	}
	if doc.ID == "" {
		doc.ID = newDocID()
	}
	now := time.Now().UTC()
	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = now
	}
	doc.UpdatedAt = now
	if doc.Version == "" {
		doc.Version = now.Format(time.RFC3339Nano)
	}

	metaJSON, err := marshalMetadata(doc.Metadata)
	if err != nil {
		return WriteResult{}, Invalid("pgvector write: marshal metadata: " + err.Error())
	}

	tbl := b.cfg.tableName()
	var embeddingParam interface{}
	if len(doc.Embedding) > 0 {
		embeddingParam = pgvectorLiteral(doc.Embedding)
	}

	_, err = b.pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s (id, tenant, namespace, content, path, metadata, version, created_at, updated_at, embedding)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id, tenant) DO UPDATE SET
  namespace   = EXCLUDED.namespace,
  content     = EXCLUDED.content,
  path        = EXCLUDED.path,
  metadata    = EXCLUDED.metadata,
  version     = EXCLUDED.version,
  updated_at  = EXCLUDED.updated_at,
  embedding   = EXCLUDED.embedding
`, tbl),
		doc.ID, doc.Tenant, doc.Namespace, doc.Content, doc.Path,
		metaJSON, doc.Version, doc.CreatedAt, doc.UpdatedAt, embeddingParam,
	)
	if err != nil {
		return WriteResult{}, BackendUnavailable("pgvector write: " + err.Error())
	}
	return WriteResult{ID: doc.ID, Version: doc.Version}, nil
}

// ── Backend.Get ───────────────────────────────────────────────────────────────

func (b *PgvectorBackend) Get(ctx context.Context, id string, filter Filter) (Document, error) {
	if filter.Tenant == "" {
		return Document{}, Invalid("pgvector get: tenant is required")
	}
	tbl := b.cfg.tableName()
	row := b.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT id, tenant, namespace, content, path, metadata, version, created_at, updated_at
FROM %s
WHERE id = $1 AND tenant = $2
`, tbl), id, filter.Tenant)

	doc, err := scanDocument(row)
	if err != nil {
		if isNoRows(err) {
			return Document{}, NotFound("pgvector: document not found: " + id)
		}
		return Document{}, BackendUnavailable("pgvector get: " + err.Error())
	}
	// Namespace filter (defense-in-depth).
	if filter.Namespace != "" && doc.Namespace != filter.Namespace {
		return Document{}, NotFound("pgvector: document not found: " + id)
	}
	return doc, nil
}

// ── Backend.Delete ────────────────────────────────────────────────────────────

func (b *PgvectorBackend) Delete(ctx context.Context, id string, filter Filter) error {
	if filter.Tenant == "" {
		return Invalid("pgvector delete: tenant is required")
	}
	// First verify ownership (cross-tenant: NotFound, not PermissionDenied).
	existing, err := b.Get(ctx, id, filter)
	if err != nil {
		return err
	}
	if filter.Namespace != "" && existing.Namespace != filter.Namespace {
		return NotFound("pgvector: document not found: " + id)
	}

	tbl := b.cfg.tableName()
	_, err = b.pool.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE id = $1 AND tenant = $2`, tbl),
		id, filter.Tenant)
	if err != nil {
		return BackendUnavailable("pgvector delete: " + err.Error())
	}
	return nil
}

// ── Backend.Retrieve ──────────────────────────────────────────────────────────

// Retrieve performs cosine-similarity ANN search via pgvector when the document
// has an embedding, otherwise falls back to full-text ILIKE search.
func (b *PgvectorBackend) Retrieve(ctx context.Context, query string, topK int, filter Filter) (RetrieveResult, error) {
	if filter.Tenant == "" {
		return RetrieveResult{}, PermissionDenied("pgvector retrieve: tenant is required")
	}
	if topK <= 0 {
		topK = 10
	}
	if b.cfg.MaxResults > 0 && topK > b.cfg.MaxResults {
		topK = b.cfg.MaxResults
	}

	tbl := b.cfg.tableName()
	// Use ILIKE text search — the worker will call RetrieveWithEmbedding for
	// vector search when an embedding is available.
	args := []interface{}{filter.Tenant, topK}
	conditions := []string{"tenant = $1"}
	argIdx := 3

	if filter.Namespace != "" {
		conditions = append(conditions, fmt.Sprintf("namespace = $%d", argIdx))
		args = append(args, filter.Namespace)
		argIdx++
	}
	if query != "" {
		conditions = append(conditions, fmt.Sprintf(
			"(content ILIKE $%d OR path ILIKE $%d)", argIdx, argIdx))
		args = append(args, "%"+query+"%")
		argIdx++
	}
	if len(filter.Metadata) > 0 {
		metaJSON, merr := marshalMetadata(filter.Metadata)
		if merr == nil {
			conditions = append(conditions, fmt.Sprintf("metadata @> $%d", argIdx))
			args = append(args, string(metaJSON))
			argIdx++
		}
	}

	where := strings.Join(conditions, " AND ")
	sql := fmt.Sprintf(`
SELECT id, tenant, namespace, content, path, metadata, version, created_at, updated_at
FROM %s
WHERE %s
LIMIT $2
`, tbl, where)

	rows, err := b.pool.Query(ctx, sql, args...)
	if err != nil {
		return RetrieveResult{}, BackendUnavailable("pgvector retrieve: " + err.Error())
	}
	defer rows.Close()

	var chunks []ScoredChunk
	for rows.Next() {
		doc, scanErr := scanDocumentFromRows(rows)
		if scanErr != nil {
			continue
		}
		chunks = append(chunks, ScoredChunk{
			Chunk: Chunk{
				Text:       string(doc.Content),
				DocumentID: doc.ID,
				EndByte:    len(doc.Content),
			},
			Score: 1.0,
		})
	}
	if err := rows.Err(); err != nil {
		return RetrieveResult{}, BackendUnavailable("pgvector retrieve rows: " + err.Error())
	}

	return RetrieveResult{Chunks: chunks, Total: int64(len(chunks))}, nil
}

// RetrieveWithEmbedding performs cosine ANN search using the pgvector <=> operator.
// Call this from the worker when a query embedding is available.
func (b *PgvectorBackend) RetrieveWithEmbedding(ctx context.Context, queryVec []float32, topK int, filter Filter) (RetrieveResult, error) {
	if filter.Tenant == "" {
		return RetrieveResult{}, PermissionDenied("pgvector retrieve: tenant is required")
	}
	if topK <= 0 {
		topK = 10
	}
	if b.cfg.MaxResults > 0 && topK > b.cfg.MaxResults {
		topK = b.cfg.MaxResults
	}

	tbl := b.cfg.tableName()
	args := []interface{}{pgvectorLiteral(queryVec), filter.Tenant, topK}
	conditions := []string{"tenant = $2", "embedding IS NOT NULL"}
	argIdx := 4

	if filter.Namespace != "" {
		conditions = append(conditions, fmt.Sprintf("namespace = $%d", argIdx))
		args = append(args, filter.Namespace)
		argIdx++
	}
	if len(filter.Metadata) > 0 {
		metaJSON, merr := marshalMetadata(filter.Metadata)
		if merr == nil {
			conditions = append(conditions, fmt.Sprintf("metadata @> $%d", argIdx))
			args = append(args, string(metaJSON))
			argIdx++
		}
	}

	where := strings.Join(conditions, " AND ")
	sql := fmt.Sprintf(`
SELECT id, tenant, namespace, content, path, metadata, version, created_at, updated_at,
       1 - (embedding <=> $1) AS score
FROM %s
WHERE %s
ORDER BY embedding <=> $1
LIMIT $3
`, tbl, where)

	rows, err := b.pool.Query(ctx, sql, args...)
	if err != nil {
		return RetrieveResult{}, BackendUnavailable("pgvector retrieve-with-embedding: " + err.Error())
	}
	defer rows.Close()

	var chunks []ScoredChunk
	for rows.Next() {
		var (
			id, tenant, ns, path, version string
			content                       []byte
			metaJSON                      []byte
			createdAt, updatedAt          time.Time
			score                         float32
		)
		if scanErr := rows.Scan(&id, &tenant, &ns, &content, &path,
			&metaJSON, &version, &createdAt, &updatedAt, &score); scanErr != nil {
			continue
		}
		chunks = append(chunks, ScoredChunk{
			Chunk: Chunk{
				Text:       string(content),
				DocumentID: id,
				EndByte:    len(content),
			},
			Score: score,
		})
	}
	if err := rows.Err(); err != nil {
		return RetrieveResult{}, BackendUnavailable("pgvector retrieve-embedding rows: " + err.Error())
	}

	sort.Slice(chunks, func(i, j int) bool { return chunks[i].Score > chunks[j].Score })
	return RetrieveResult{Chunks: chunks, Total: int64(len(chunks))}, nil
}

// ── Backend.ListNamespaces ────────────────────────────────────────────────────

func (b *PgvectorBackend) ListNamespaces(ctx context.Context, filter Filter) ([]string, error) {
	if filter.Tenant == "" {
		return nil, PermissionDenied("pgvector list-namespaces: tenant is required")
	}
	tbl := b.cfg.tableName()
	rows, err := b.pool.Query(ctx, fmt.Sprintf(
		`SELECT DISTINCT namespace FROM %s WHERE tenant = $1 ORDER BY namespace`, tbl),
		filter.Tenant)
	if err != nil {
		return nil, BackendUnavailable("pgvector list-namespaces: " + err.Error())
	}
	defer rows.Close()

	var nss []string
	for rows.Next() {
		var ns string
		if scanErr := rows.Scan(&ns); scanErr == nil {
			nss = append(nss, ns)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, BackendUnavailable("pgvector list-namespaces rows: " + err.Error())
	}
	return nss, nil
}

// ── Backend.Summarize ─────────────────────────────────────────────────────────

func (b *PgvectorBackend) Summarize(_ context.Context, _ string, _ Filter) (string, error) {
	return "", &ErrNotSupported{Op: "Summarize", Backend: "pgvector"}
}

// ── Filesystem-only stubs ─────────────────────────────────────────────────────

func (b *PgvectorBackend) Branch(_ context.Context, _, _ string, _ Filter) (BranchInfo, error) {
	return BranchInfo{}, &ErrNotSupported{Op: "Branch", Backend: "pgvector"}
}

func (b *PgvectorBackend) Snapshot(_ context.Context, _ string, _ Filter) (SnapshotInfo, error) {
	return SnapshotInfo{}, &ErrNotSupported{Op: "Snapshot", Backend: "pgvector"}
}

func (b *PgvectorBackend) ListBranches(_ context.Context, _ Filter) ([]BranchInfo, error) {
	return nil, &ErrNotSupported{Op: "ListBranches", Backend: "pgvector"}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// pgvectorLiteral formats a float32 slice as a pgvector literal string,
// e.g. "[0.1,0.2,0.3]". pgx sends it as a TEXT parameter; pgvector casts it.
func pgvectorLiteral(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func marshalMetadata(m map[string]string) (json.RawMessage, error) {
	if len(m) == 0 {
		return json.RawMessage("{}"), nil
	}
	b, err := json.Marshal(m)
	return b, err
}

func unmarshalMetadata(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func scanDocument(row pgx.Row) (Document, error) {
	var (
		id, tenant, ns, path, version string
		content                       []byte
		metaJSON                      []byte
		createdAt, updatedAt          time.Time
	)
	err := row.Scan(&id, &tenant, &ns, &content, &path,
		&metaJSON, &version, &createdAt, &updatedAt)
	if err != nil {
		return Document{}, err
	}
	meta, _ := unmarshalMetadata(metaJSON)
	return Document{
		ID:        id,
		Tenant:    tenant,
		Namespace: ns,
		Content:   content,
		Path:      path,
		Metadata:  meta,
		Version:   version,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}, nil
}

func scanDocumentFromRows(rows pgx.Rows) (Document, error) {
	var (
		id, tenant, ns, path, version string
		content                       []byte
		metaJSON                      []byte
		createdAt, updatedAt          time.Time
	)
	err := rows.Scan(&id, &tenant, &ns, &content, &path,
		&metaJSON, &version, &createdAt, &updatedAt)
	if err != nil {
		return Document{}, err
	}
	meta, _ := unmarshalMetadata(metaJSON)
	return Document{
		ID:        id,
		Tenant:    tenant,
		Namespace: ns,
		Content:   content,
		Path:      path,
		Metadata:  meta,
		Version:   version,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}, nil
}

func isNoRows(err error) bool {
	return err == pgx.ErrNoRows
}

// compile-time assertion: PgvectorBackend satisfies the Backend interface.
var _ Backend = (*PgvectorBackend)(nil)
