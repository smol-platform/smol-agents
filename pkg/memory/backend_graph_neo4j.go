// Package memory — Neo4j graph Backend adapter.
//
// Neo4jBackend implements memory.Backend over a Neo4j graph database using
// github.com/neo4j/neo4j-go-driver/v5. Documents are stored as
// (:MemoryDoc) nodes with properties matching the Document type. Tenant
// and namespace are mandatory node properties used in every Cypher query
// for isolation (R-MEM-WORK-1, R-MEM-SEC-1).
//
// Node label: MemoryDoc
// Mandatory properties: id, tenant, namespace, version
// Optional: content (bytes as base64-encoded string), path, metadata (JSON),
//
//	embedding (float array), created_at (int64 unix nano), updated_at.
//
// Indexes (created in EnsureSchema):
//   - RANGE INDEX ON MemoryDoc(id, tenant)    — fast lookup + cross-tenant check
//   - RANGE INDEX ON MemoryDoc(tenant, namespace) — namespace listing
//
// FS-only operations return *ErrNotSupported. Summarize returns *ErrNotSupported.
//
// Implements R-MEM-WORK-2.
package memory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// Neo4jConfig holds the configuration for a Neo4jBackend.
type Neo4jConfig struct {
	// URI is the Neo4j Bolt connection URI, e.g.
	// "neo4j+s://host:7687" or "bolt://host:7687".
	URI string

	// Username and Password are the auth credentials.
	// Credentials MUST be broker-resolved before constructing the backend.
	Username string
	Password string

	// Database is the Neo4j database name. Defaults to "neo4j".
	Database string

	// MaxResults is an optional upper bound on topK (0 = no cap).
	MaxResults int
}

func (c *Neo4jConfig) database() string {
	if c.Database != "" {
		return c.Database
	}
	return "neo4j"
}

// Neo4jBackend is a memory.Backend backed by Neo4j.
// Construct with NewNeo4jBackend; call EnsureSchema once at startup.
type Neo4jBackend struct {
	cfg    Neo4jConfig
	driver neo4j.DriverWithContext
}

// NewNeo4jBackend opens a Neo4j driver and verifies connectivity.
// Call EnsureSchema to create indexes before first use.
func NewNeo4jBackend(ctx context.Context, cfg Neo4jConfig) (*Neo4jBackend, error) {
	if cfg.URI == "" {
		return nil, fmt.Errorf("neo4j: URI is required")
	}
	driver, err := neo4j.NewDriverWithContext(
		cfg.URI,
		neo4j.BasicAuth(cfg.Username, cfg.Password, ""),
	)
	if err != nil {
		return nil, fmt.Errorf("neo4j: create driver: %w", err)
	}
	if err := driver.VerifyConnectivity(ctx); err != nil {
		_ = driver.Close(ctx)
		return nil, fmt.Errorf("neo4j: verify connectivity: %w", err)
	}
	return &Neo4jBackend{cfg: cfg, driver: driver}, nil
}

// Close releases the Neo4j driver.
func (b *Neo4jBackend) Close(ctx context.Context) error { return b.driver.Close(ctx) }

// EnsureSchema creates the required indexes idempotently.
func (b *Neo4jBackend) EnsureSchema(ctx context.Context) error {
	session := b.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: b.cfg.database()})
	defer func() { _ = session.Close(ctx) }()

	stmts := []string{
		`CREATE INDEX memory_doc_id IF NOT EXISTS FOR (n:MemoryDoc) ON (n.id, n.tenant)`,
		`CREATE INDEX memory_doc_ns IF NOT EXISTS FOR (n:MemoryDoc) ON (n.tenant, n.namespace)`,
	}
	for _, stmt := range stmts {
		if _, err := session.Run(ctx, stmt, nil); err != nil {
			return fmt.Errorf("neo4j: ensure schema: %w", err)
		}
	}
	return nil
}

// ── Backend.Write ─────────────────────────────────────────────────────────────

func (b *Neo4jBackend) Write(ctx context.Context, doc Document) (WriteResult, error) {
	if doc.Tenant == "" || doc.Namespace == "" {
		return WriteResult{}, Invalid("neo4j write: tenant and namespace are required")
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

	metaJSON, _ := json.Marshal(doc.Metadata)
	contentB64 := base64.StdEncoding.EncodeToString(doc.Content)

	params := map[string]interface{}{
		"id":         doc.ID,
		"tenant":     doc.Tenant,
		"namespace":  doc.Namespace,
		"content":    contentB64,
		"path":       doc.Path,
		"metadata":   string(metaJSON),
		"version":    doc.Version,
		"created_at": doc.CreatedAt.UnixNano(),
		"updated_at": doc.UpdatedAt.UnixNano(),
	}
	if len(doc.Embedding) > 0 {
		embSlice := make([]interface{}, len(doc.Embedding))
		for i, f := range doc.Embedding {
			embSlice[i] = float64(f)
		}
		params["embedding"] = embSlice
	}

	cypher := `
MERGE (n:MemoryDoc {id: $id, tenant: $tenant})
SET n.namespace  = $namespace,
    n.content    = $content,
    n.path       = $path,
    n.metadata   = $metadata,
    n.version    = $version,
    n.created_at = CASE WHEN n.created_at IS NULL THEN $created_at ELSE n.created_at END,
    n.updated_at = $updated_at
` + func() string {
		if len(doc.Embedding) > 0 {
			return ", n.embedding = $embedding"
		}
		return ""
	}()

	session := b.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: b.cfg.database()})
	defer func() { _ = session.Close(ctx) }()

	_, err := session.Run(ctx, cypher, params)
	if err != nil {
		return WriteResult{}, BackendUnavailable("neo4j write: " + err.Error())
	}
	return WriteResult{ID: doc.ID, Version: doc.Version}, nil
}

// ── Backend.Get ───────────────────────────────────────────────────────────────

func (b *Neo4jBackend) Get(ctx context.Context, id string, filter Filter) (Document, error) {
	if filter.Tenant == "" {
		return Document{}, Invalid("neo4j get: tenant is required")
	}

	session := b.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: b.cfg.database()})
	defer func() { _ = session.Close(ctx) }()

	result, err := session.Run(ctx,
		`MATCH (n:MemoryDoc {id: $id, tenant: $tenant}) RETURN n`,
		map[string]interface{}{"id": id, "tenant": filter.Tenant})
	if err != nil {
		return Document{}, BackendUnavailable("neo4j get: " + err.Error())
	}
	if !result.Next(ctx) {
		return Document{}, NotFound("neo4j: document not found: " + id)
	}
	record := result.Record()
	node, ok := record.Values[0].(neo4j.Node)
	if !ok {
		return Document{}, BackendUnavailable("neo4j get: unexpected result type")
	}
	doc := neo4jNodeToDoc(node)
	if filter.Namespace != "" && doc.Namespace != filter.Namespace {
		return Document{}, NotFound("neo4j: document not found: " + id)
	}
	return doc, nil
}

// ── Backend.Delete ────────────────────────────────────────────────────────────

func (b *Neo4jBackend) Delete(ctx context.Context, id string, filter Filter) error {
	if filter.Tenant == "" {
		return Invalid("neo4j delete: tenant is required")
	}
	if _, err := b.Get(ctx, id, filter); err != nil {
		return err
	}

	session := b.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: b.cfg.database()})
	defer func() { _ = session.Close(ctx) }()

	_, err := session.Run(ctx,
		`MATCH (n:MemoryDoc {id: $id, tenant: $tenant}) DELETE n`,
		map[string]interface{}{"id": id, "tenant": filter.Tenant})
	if err != nil {
		return BackendUnavailable("neo4j delete: " + err.Error())
	}
	return nil
}

// ── Backend.Retrieve ──────────────────────────────────────────────────────────

func (b *Neo4jBackend) Retrieve(ctx context.Context, query string, topK int, filter Filter) (RetrieveResult, error) {
	if filter.Tenant == "" {
		return RetrieveResult{}, PermissionDenied("neo4j retrieve: tenant is required")
	}
	if topK <= 0 {
		topK = 10
	}
	if b.cfg.MaxResults > 0 && topK > b.cfg.MaxResults {
		topK = b.cfg.MaxResults
	}

	conditions := []string{"n.tenant = $tenant"}
	params := map[string]interface{}{"tenant": filter.Tenant}
	if filter.Namespace != "" {
		conditions = append(conditions, "n.namespace = $namespace")
		params["namespace"] = filter.Namespace
	}
	where := strings.Join(conditions, " AND ")
	cypher := fmt.Sprintf(`MATCH (n:MemoryDoc) WHERE %s RETURN n LIMIT $limit`, where)
	params["limit"] = topK * 3 // over-fetch to allow client-side filtering

	session := b.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: b.cfg.database()})
	defer func() { _ = session.Close(ctx) }()

	result, err := session.Run(ctx, cypher, params)
	if err != nil {
		return RetrieveResult{}, BackendUnavailable("neo4j retrieve: " + err.Error())
	}

	type scored struct {
		sc    ScoredChunk
		score float32
	}
	var candidates []scored
	for result.Next(ctx) {
		record := result.Record()
		node, ok := record.Values[0].(neo4j.Node)
		if !ok {
			continue
		}
		doc := neo4jNodeToDoc(node)
		if !matchMetadata(doc.Metadata, filter.Metadata) {
			continue
		}
		var score float32
		if query != "" {
			lower := strings.ToLower(string(doc.Content))
			q := strings.ToLower(query)
			terms := strings.Fields(q)
			var hits int
			for _, t := range terms {
				if strings.Contains(lower, t) {
					hits++
				}
			}
			if hits == 0 {
				continue
			}
			if len(terms) > 0 {
				score = float32(hits) / float32(len(terms))
			}
		} else {
			score = 0.5
		}
		candidates = append(candidates, scored{
			sc: ScoredChunk{
				Chunk: Chunk{
					Text:       string(doc.Content),
					DocumentID: doc.ID,
					EndByte:    len(doc.Content),
				},
				Score: score,
			},
			score: score,
		})
	}
	if err := result.Err(); err != nil {
		return RetrieveResult{}, BackendUnavailable("neo4j retrieve: " + err.Error())
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	total := int64(len(candidates))
	if len(candidates) > topK {
		candidates = candidates[:topK]
	}
	chunks := make([]ScoredChunk, len(candidates))
	for i, c := range candidates {
		chunks[i] = c.sc
	}
	return RetrieveResult{Chunks: chunks, Total: total}, nil
}

// ── Backend.ListNamespaces ────────────────────────────────────────────────────

func (b *Neo4jBackend) ListNamespaces(ctx context.Context, filter Filter) ([]string, error) {
	if filter.Tenant == "" {
		return nil, PermissionDenied("neo4j list-namespaces: tenant is required")
	}

	session := b.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: b.cfg.database()})
	defer func() { _ = session.Close(ctx) }()

	result, err := session.Run(ctx,
		`MATCH (n:MemoryDoc {tenant: $tenant}) RETURN DISTINCT n.namespace AS ns ORDER BY ns`,
		map[string]interface{}{"tenant": filter.Tenant})
	if err != nil {
		return nil, BackendUnavailable("neo4j list-namespaces: " + err.Error())
	}

	var nss []string
	for result.Next(ctx) {
		rec := result.Record()
		if v, ok := rec.Values[0].(string); ok {
			nss = append(nss, v)
		}
	}
	if err := result.Err(); err != nil {
		return nil, BackendUnavailable("neo4j list-namespaces: " + err.Error())
	}
	return nss, nil
}

// ── Backend.Summarize ─────────────────────────────────────────────────────────

func (b *Neo4jBackend) Summarize(_ context.Context, _ string, _ Filter) (string, error) {
	return "", &ErrNotSupported{Op: "Summarize", Backend: "neo4j"}
}

// ── Filesystem-only stubs ─────────────────────────────────────────────────────

func (b *Neo4jBackend) Branch(_ context.Context, _, _ string, _ Filter) (BranchInfo, error) {
	return BranchInfo{}, &ErrNotSupported{Op: "Branch", Backend: "neo4j"}
}

func (b *Neo4jBackend) Snapshot(_ context.Context, _ string, _ Filter) (SnapshotInfo, error) {
	return SnapshotInfo{}, &ErrNotSupported{Op: "Snapshot", Backend: "neo4j"}
}

func (b *Neo4jBackend) ListBranches(_ context.Context, _ Filter) ([]BranchInfo, error) {
	return nil, &ErrNotSupported{Op: "ListBranches", Backend: "neo4j"}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func neo4jNodeToDoc(node neo4j.Node) Document {
	getString := func(key string) string {
		if v, ok := node.Props[key]; ok {
			if s, ok2 := v.(string); ok2 {
				return s
			}
		}
		return ""
	}
	getInt64 := func(key string) int64 {
		if v, ok := node.Props[key]; ok {
			switch n := v.(type) {
			case int64:
				return n
			case float64:
				return int64(n)
			}
		}
		return 0
	}

	contentRaw := getString("content")
	content, _ := base64.StdEncoding.DecodeString(contentRaw)

	var meta map[string]string
	if raw := getString("metadata"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &meta)
	}

	createdAt := time.Unix(0, getInt64("created_at")).UTC()
	updatedAt := time.Unix(0, getInt64("updated_at")).UTC()

	return Document{
		ID:        getString("id"),
		Tenant:    getString("tenant"),
		Namespace: getString("namespace"),
		Content:   content,
		Path:      getString("path"),
		Version:   getString("version"),
		Metadata:  meta,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}

// compile-time assertion: Neo4jBackend satisfies the Backend interface.
var _ Backend = (*Neo4jBackend)(nil)
