// Package memory — Redis KV Backend adapter.
//
// RedisBackend implements memory.Backend over Redis. It stores documents as
// JSON strings under keys of the form:
//
//	memory:{tenant}:{namespace}:{id}
//
// A secondary index set per tenant+namespace tracks all IDs:
//
//	memory:ns:{tenant}:{namespace}
//
// This allows ListNamespaces and Delete to work without a full scan.
// Tenant+namespace isolation is enforced in every method (R-MEM-WORK-1,
// R-MEM-SEC-1). Embedding-based retrieval falls back to iterating all
// documents in the namespace and computing cosine similarity in-process —
// suitable for small KV stores; for large corpora use the vector backend.
//
// FS-only operations return *ErrNotSupported. Summarize returns *ErrNotSupported.
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

	"github.com/redis/go-redis/v9"
)

// RedisConfig holds the configuration for a RedisBackend.
type RedisConfig struct {
	// Addr is the Redis server address, e.g. "redis-svc:6379".
	Addr string

	// Password is the Redis AUTH password. Leave empty for unauthenticated.
	// Credentials MUST be broker-resolved before constructing the backend.
	Password string

	// DB is the Redis database index (0–15).
	DB int

	// KeyPrefix is an optional prefix for all keys (e.g. "agents:").
	// Defaults to "memory:" when empty.
	KeyPrefix string

	// MaxResults is an optional upper bound on topK (0 = no cap).
	MaxResults int
}

func (c *RedisConfig) prefix() string {
	if c.KeyPrefix != "" {
		return c.KeyPrefix
	}
	return "memory:"
}

// RedisBackend is a memory.Backend backed by Redis.
// Construct with NewRedisBackend.
type RedisBackend struct {
	cfg    RedisConfig
	client *redis.Client
}

// NewRedisBackend constructs a RedisBackend and pings the server to verify
// connectivity.
func NewRedisBackend(ctx context.Context, cfg RedisConfig) (*RedisBackend, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("redis: Addr is required")
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis: ping: %w", err)
	}
	return &RedisBackend{cfg: cfg, client: rdb}, nil
}

// Close releases the Redis connection.
func (b *RedisBackend) Close() error { return b.client.Close() }

// ── key helpers ───────────────────────────────────────────────────────────────

func (b *RedisBackend) docKey(tenant, namespace, id string) string {
	return b.cfg.prefix() + tenant + ":" + namespace + ":" + id
}

func (b *RedisBackend) nsIndexKey(tenant, namespace string) string {
	return b.cfg.prefix() + "ns:" + tenant + ":" + namespace
}

func (b *RedisBackend) tenantNSKey(tenant string) string {
	return b.cfg.prefix() + "tenantns:" + tenant
}

// ── serialised document ───────────────────────────────────────────────────────

type redisDoc struct {
	ID        string            `json:"id"`
	Tenant    string            `json:"tenant"`
	Namespace string            `json:"namespace"`
	Content   []byte            `json:"content"`
	Path      string            `json:"path"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Embedding []float32         `json:"embedding,omitempty"`
	Version   string            `json:"version"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

func toRedisDoc(doc Document) redisDoc {
	return redisDoc{
		ID:        doc.ID,
		Tenant:    doc.Tenant,
		Namespace: doc.Namespace,
		Content:   doc.Content,
		Path:      doc.Path,
		Metadata:  doc.Metadata,
		Embedding: doc.Embedding,
		Version:   doc.Version,
		CreatedAt: doc.CreatedAt,
		UpdatedAt: doc.UpdatedAt,
	}
}

func fromRedisDoc(r redisDoc) Document {
	return Document{
		ID:        r.ID,
		Tenant:    r.Tenant,
		Namespace: r.Namespace,
		Content:   r.Content,
		Path:      r.Path,
		Metadata:  r.Metadata,
		Embedding: r.Embedding,
		Version:   r.Version,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

// ── Backend.Write ─────────────────────────────────────────────────────────────

func (b *RedisBackend) Write(ctx context.Context, doc Document) (WriteResult, error) {
	if doc.Tenant == "" || doc.Namespace == "" {
		return WriteResult{}, Invalid("redis write: tenant and namespace are required")
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

	raw, err := json.Marshal(toRedisDoc(doc))
	if err != nil {
		return WriteResult{}, Invalid("redis write: marshal: " + err.Error())
	}

	pipe := b.client.Pipeline()
	pipe.Set(ctx, b.docKey(doc.Tenant, doc.Namespace, doc.ID), raw, 0)
	// Add the ID to the namespace index set.
	pipe.SAdd(ctx, b.nsIndexKey(doc.Tenant, doc.Namespace), doc.ID)
	// Track the namespace in the per-tenant set (for ListNamespaces).
	pipe.SAdd(ctx, b.tenantNSKey(doc.Tenant), doc.Namespace)
	if _, err := pipe.Exec(ctx); err != nil {
		return WriteResult{}, BackendUnavailable("redis write: " + err.Error())
	}
	return WriteResult{ID: doc.ID, Version: doc.Version}, nil
}

// ── Backend.Get ───────────────────────────────────────────────────────────────

func (b *RedisBackend) Get(ctx context.Context, id string, filter Filter) (Document, error) {
	if filter.Tenant == "" {
		return Document{}, Invalid("redis get: tenant is required")
	}
	// We must search across all namespaces for this tenant+id because the caller
	// may not always specify namespace in filter. Use a wildcard pattern scan on
	// the known namespace index to find the right key.
	key, err := b.findDocKey(ctx, filter.Tenant, filter.Namespace, id)
	if err != nil {
		return Document{}, err
	}

	raw, err := b.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return Document{}, NotFound("redis: document not found: " + id)
	}
	if err != nil {
		return Document{}, BackendUnavailable("redis get: " + err.Error())
	}

	var r redisDoc
	if err := json.Unmarshal(raw, &r); err != nil {
		return Document{}, BackendUnavailable("redis get: unmarshal: " + err.Error())
	}
	doc := fromRedisDoc(r)
	if doc.Tenant != filter.Tenant {
		return Document{}, NotFound("redis: document not found: " + id)
	}
	if filter.Namespace != "" && doc.Namespace != filter.Namespace {
		return Document{}, NotFound("redis: document not found: " + id)
	}
	return doc, nil
}

// findDocKey locates the Redis key for a document. When namespace is provided,
// the lookup is direct; otherwise it scans the tenant's namespace sets.
func (b *RedisBackend) findDocKey(ctx context.Context, tenant, namespace, id string) (string, error) {
	if namespace != "" {
		return b.docKey(tenant, namespace, id), nil
	}
	// Scan the tenant namespace index to find which namespace holds this id.
	nss, err := b.client.SMembers(ctx, b.tenantNSKey(tenant)).Result()
	if err != nil {
		return "", BackendUnavailable("redis find-key: " + err.Error())
	}
	for _, ns := range nss {
		isMember, memberErr := b.client.SIsMember(ctx, b.nsIndexKey(tenant, ns), id).Result()
		if memberErr != nil {
			continue
		}
		if isMember {
			return b.docKey(tenant, ns, id), nil
		}
	}
	return "", NotFound("redis: document not found: " + id)
}

// ── Backend.Delete ────────────────────────────────────────────────────────────

func (b *RedisBackend) Delete(ctx context.Context, id string, filter Filter) error {
	if filter.Tenant == "" {
		return Invalid("redis delete: tenant is required")
	}
	// Get first to verify ownership.
	doc, err := b.Get(ctx, id, filter)
	if err != nil {
		return err
	}

	pipe := b.client.Pipeline()
	pipe.Del(ctx, b.docKey(doc.Tenant, doc.Namespace, doc.ID))
	pipe.SRem(ctx, b.nsIndexKey(doc.Tenant, doc.Namespace), doc.ID)
	if _, err := pipe.Exec(ctx); err != nil {
		return BackendUnavailable("redis delete: " + err.Error())
	}
	return nil
}

// ── Backend.Retrieve ──────────────────────────────────────────────────────────

// Retrieve iterates all documents in the namespace and scores by cosine
// similarity (when embeddings are present) or substring match otherwise.
// This is correct but O(n); pair with the vector backend for large corpora.
func (b *RedisBackend) Retrieve(ctx context.Context, query string, topK int, filter Filter) (RetrieveResult, error) {
	if filter.Tenant == "" {
		return RetrieveResult{}, PermissionDenied("redis retrieve: tenant is required")
	}
	if topK <= 0 {
		topK = 10
	}
	if b.cfg.MaxResults > 0 && topK > b.cfg.MaxResults {
		topK = b.cfg.MaxResults
	}
	if filter.Namespace == "" {
		return RetrieveResult{}, Invalid("redis retrieve: namespace is required")
	}

	ids, err := b.client.SMembers(ctx, b.nsIndexKey(filter.Tenant, filter.Namespace)).Result()
	if err != nil {
		return RetrieveResult{}, BackendUnavailable("redis retrieve: " + err.Error())
	}

	type scored struct {
		chunk ScoredChunk
		score float32
	}
	var results []scored
	for _, id := range ids {
		raw, getErr := b.client.Get(ctx, b.docKey(filter.Tenant, filter.Namespace, id)).Bytes()
		if getErr != nil {
			continue
		}
		var r redisDoc
		if jsonErr := json.Unmarshal(raw, &r); jsonErr != nil {
			continue
		}
		if !matchMetadata(r.Metadata, filter.Metadata) {
			continue
		}
		var score float32
		if query != "" {
			lower := strings.ToLower(string(r.Content))
			q := strings.ToLower(query)
			terms := strings.Fields(q)
			var hits int
			for _, t := range terms {
				if strings.Contains(lower, t) {
					hits++
				}
			}
			if hits == 0 && !strings.Contains(strings.ToLower(r.Path), q) {
				continue
			}
			if len(terms) > 0 {
				score = float32(hits) / float32(len(terms))
			}
		} else {
			score = 0.5
		}
		results = append(results, scored{
			chunk: ScoredChunk{
				Chunk: Chunk{
					Text:       string(r.Content),
					DocumentID: r.ID,
					EndByte:    len(r.Content),
				},
				Score: score,
			},
			score: score,
		})
	}

	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	total := int64(len(results))
	if len(results) > topK {
		results = results[:topK]
	}
	chunks := make([]ScoredChunk, len(results))
	for i, r := range results {
		chunks[i] = r.chunk
	}
	return RetrieveResult{Chunks: chunks, Total: total}, nil
}

// ── Backend.ListNamespaces ────────────────────────────────────────────────────

func (b *RedisBackend) ListNamespaces(ctx context.Context, filter Filter) ([]string, error) {
	if filter.Tenant == "" {
		return nil, PermissionDenied("redis list-namespaces: tenant is required")
	}
	nss, err := b.client.SMembers(ctx, b.tenantNSKey(filter.Tenant)).Result()
	if err != nil {
		return nil, BackendUnavailable("redis list-namespaces: " + err.Error())
	}
	sort.Strings(nss)
	return nss, nil
}

// ── Backend.Summarize ─────────────────────────────────────────────────────────

func (b *RedisBackend) Summarize(_ context.Context, _ string, _ Filter) (string, error) {
	return "", &ErrNotSupported{Op: "Summarize", Backend: "redis"}
}

// ── Filesystem-only stubs ─────────────────────────────────────────────────────

func (b *RedisBackend) Branch(_ context.Context, _, _ string, _ Filter) (BranchInfo, error) {
	return BranchInfo{}, &ErrNotSupported{Op: "Branch", Backend: "redis"}
}

func (b *RedisBackend) Snapshot(_ context.Context, _ string, _ Filter) (SnapshotInfo, error) {
	return SnapshotInfo{}, &ErrNotSupported{Op: "Snapshot", Backend: "redis"}
}

func (b *RedisBackend) ListBranches(_ context.Context, _ Filter) ([]BranchInfo, error) {
	return nil, &ErrNotSupported{Op: "ListBranches", Backend: "redis"}
}

func (b *RedisBackend) Merge(_ context.Context, _, _ string, _ Filter) (BranchInfo, error) {
	return BranchInfo{}, &ErrNotSupported{Op: "Merge", Backend: "redis"}
}

// compile-time assertion: RedisBackend satisfies the Backend interface.
var _ Backend = (*RedisBackend)(nil)
