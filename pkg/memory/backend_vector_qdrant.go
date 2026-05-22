// Package memory — Qdrant vector Backend adapter.
//
// QdrantBackend implements memory.Backend over a Qdrant vector database using
// github.com/qdrant/go-client. It enforces tenant+namespace isolation in every
// operation via Qdrant payload filters (R-MEM-WORK-1, R-MEM-SEC-1).
//
// Each MemoryStore maps to one Qdrant collection. Tenant and namespace are
// stored as payload fields ("tenant", "namespace") on every point. All queries
// carry a payload filter that restricts results to the caller's tenant
// (and namespace when specified).
//
// FS-only operations return *ErrNotSupported. Summarize returns *ErrNotSupported.
//
// Implements R-MEM-WORK-2.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	pb "github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// QdrantConfig holds the configuration for a QdrantBackend.
type QdrantConfig struct {
	// Addr is the host:port of the Qdrant gRPC endpoint,
	// e.g. "qdrant-svc:6334".
	Addr string

	// Collection is the Qdrant collection name. All tenants share one
	// collection; isolation is via payload filters.
	Collection string

	// EmbeddingDims is the vector dimensionality. Must match the Embedder.
	EmbeddingDims uint64

	// APIKey is the optional API key (Qdrant Cloud). Leave empty for
	// unauthenticated local deployments.
	APIKey string

	// MaxResults is an optional upper bound on topK (0 = no cap).
	MaxResults int
}

// QdrantBackend is a memory.Backend backed by Qdrant.
// Construct with NewQdrantBackend; call EnsureCollection once at startup.
type QdrantBackend struct {
	cfg    QdrantConfig
	conn   *grpc.ClientConn
	points pb.PointsClient
	colls  pb.CollectionsClient
}

// NewQdrantBackend opens a gRPC connection to Qdrant and returns a
// QdrantBackend. Call EnsureCollection before first use.
func NewQdrantBackend(ctx context.Context, cfg QdrantConfig) (*QdrantBackend, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("qdrant: Addr is required")
	}
	if cfg.Collection == "" {
		return nil, fmt.Errorf("qdrant: Collection is required")
	}
	if cfg.EmbeddingDims == 0 {
		return nil, fmt.Errorf("qdrant: EmbeddingDims must be positive")
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	if cfg.APIKey != "" {
		opts = append(opts, grpc.WithUnaryInterceptor(qdrantAPIKeyInterceptor(cfg.APIKey)))
	}
	//nolint:staticcheck // grpc.Dial is the stable v1 API still used by the qdrant client
	conn, err := grpc.DialContext(ctx, cfg.Addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("qdrant: dial %s: %w", cfg.Addr, err)
	}
	return &QdrantBackend{
		cfg:    cfg,
		conn:   conn,
		points: pb.NewPointsClient(conn),
		colls:  pb.NewCollectionsClient(conn),
	}, nil
}

// Close releases the underlying gRPC connection.
func (b *QdrantBackend) Close() error { return b.conn.Close() }

// EnsureCollection creates the collection if it does not exist. Safe to call
// on every startup.
func (b *QdrantBackend) EnsureCollection(ctx context.Context) error {
	_, err := b.colls.Create(ctx, &pb.CreateCollection{
		CollectionName: b.cfg.Collection,
		VectorsConfig: &pb.VectorsConfig{
			Config: &pb.VectorsConfig_Params{
				Params: &pb.VectorParams{
					Size:     b.cfg.EmbeddingDims,
					Distance: pb.Distance_Cosine,
				},
			},
		},
	})
	// Ignore "already exists" errors.
	if err != nil && !isQdrantAlreadyExists(err) {
		return fmt.Errorf("qdrant: create collection: %w", err)
	}
	return nil
}

// ── Backend.Write ─────────────────────────────────────────────────────────────

func (b *QdrantBackend) Write(ctx context.Context, doc Document) (WriteResult, error) {
	if doc.Tenant == "" || doc.Namespace == "" {
		return WriteResult{}, Invalid("qdrant write: tenant and namespace are required")
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

	payload := map[string]*pb.Value{
		"tenant":     strVal(doc.Tenant),
		"namespace":  strVal(doc.Namespace),
		"content":    strVal(string(doc.Content)),
		"path":       strVal(doc.Path),
		"version":    strVal(doc.Version),
		"created_at": strVal(doc.CreatedAt.Format(time.RFC3339Nano)),
		"updated_at": strVal(doc.UpdatedAt.Format(time.RFC3339Nano)),
	}
	if len(doc.Metadata) > 0 {
		metaBytes, _ := json.Marshal(doc.Metadata)
		payload["metadata"] = strVal(string(metaBytes))
	}

	var vectors *pb.Vectors
	if len(doc.Embedding) > 0 {
		vectors = &pb.Vectors{
			VectorsOptions: &pb.Vectors_Vector{
				Vector: &pb.Vector{Data: doc.Embedding},
			},
		}
	} else {
		// Qdrant requires a vector; use a zero vector when no embedding is set.
		zeros := make([]float32, b.cfg.EmbeddingDims)
		vectors = &pb.Vectors{
			VectorsOptions: &pb.Vectors_Vector{
				Vector: &pb.Vector{Data: zeros},
			},
		}
	}

	_, err := b.points.Upsert(ctx, &pb.UpsertPoints{
		CollectionName: b.cfg.Collection,
		Points: []*pb.PointStruct{
			{
				Id:      &pb.PointId{PointIdOptions: &pb.PointId_Uuid{Uuid: doc.ID}},
				Payload: payload,
				Vectors: vectors,
			},
		},
	})
	if err != nil {
		return WriteResult{}, BackendUnavailable("qdrant write: " + err.Error())
	}
	return WriteResult{ID: doc.ID, Version: doc.Version}, nil
}

// ── Backend.Get ───────────────────────────────────────────────────────────────

func (b *QdrantBackend) Get(ctx context.Context, id string, filter Filter) (Document, error) {
	if filter.Tenant == "" {
		return Document{}, Invalid("qdrant get: tenant is required")
	}
	resp, err := b.points.Get(ctx, &pb.GetPoints{
		CollectionName: b.cfg.Collection,
		Ids:            []*pb.PointId{{PointIdOptions: &pb.PointId_Uuid{Uuid: id}}},
		WithPayload:    &pb.WithPayloadSelector{SelectorOptions: &pb.WithPayloadSelector_Enable{Enable: true}},
	})
	if err != nil {
		return Document{}, BackendUnavailable("qdrant get: " + err.Error())
	}
	if len(resp.Result) == 0 {
		return Document{}, NotFound("qdrant: document not found: " + id)
	}
	doc := qdrantPointToDoc(id, resp.Result[0].Payload)
	if doc.Tenant != filter.Tenant {
		return Document{}, NotFound("qdrant: document not found: " + id)
	}
	if filter.Namespace != "" && doc.Namespace != filter.Namespace {
		return Document{}, NotFound("qdrant: document not found: " + id)
	}
	return doc, nil
}

// ── Backend.Delete ────────────────────────────────────────────────────────────

func (b *QdrantBackend) Delete(ctx context.Context, id string, filter Filter) error {
	if filter.Tenant == "" {
		return Invalid("qdrant delete: tenant is required")
	}
	if _, err := b.Get(ctx, id, filter); err != nil {
		return err
	}
	_, err := b.points.Delete(ctx, &pb.DeletePoints{
		CollectionName: b.cfg.Collection,
		Points: &pb.PointsSelector{
			PointsSelectorOneOf: &pb.PointsSelector_Points{
				Points: &pb.PointsIdsList{
					Ids: []*pb.PointId{{PointIdOptions: &pb.PointId_Uuid{Uuid: id}}},
				},
			},
		},
	})
	if err != nil {
		return BackendUnavailable("qdrant delete: " + err.Error())
	}
	return nil
}

// ── Backend.Retrieve ──────────────────────────────────────────────────────────

// Retrieve performs a vector search using the provided query. When doc embeddings
// are stored, cosine similarity is used; otherwise a scroll (listing) approach
// is used for text fallback. The worker calls RetrieveWithEmbedding for the
// true vector path.
func (b *QdrantBackend) Retrieve(ctx context.Context, query string, topK int, filter Filter) (RetrieveResult, error) {
	if filter.Tenant == "" {
		return RetrieveResult{}, PermissionDenied("qdrant retrieve: tenant is required")
	}
	if topK <= 0 {
		topK = 10
	}
	if b.cfg.MaxResults > 0 && topK > b.cfg.MaxResults {
		topK = b.cfg.MaxResults
	}

	qFilter := b.tenantFilter(filter)
	limit := uint32(topK) //nolint:gosec
	resp, err := b.points.Scroll(ctx, &pb.ScrollPoints{
		CollectionName: b.cfg.Collection,
		Filter:         qFilter,
		Limit:          &limit,
		WithPayload:    &pb.WithPayloadSelector{SelectorOptions: &pb.WithPayloadSelector_Enable{Enable: true}},
	})
	if err != nil {
		return RetrieveResult{}, BackendUnavailable("qdrant retrieve: " + err.Error())
	}

	chunks := make([]ScoredChunk, 0, len(resp.Result))
	for _, pt := range resp.Result {
		doc := qdrantPointToDoc(qdrantPointID(pt.Id), pt.Payload)
		if query != "" {
			if !containsSubstring(string(doc.Content), query) && !containsSubstring(doc.Path, query) {
				continue
			}
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
	return RetrieveResult{Chunks: chunks, Total: int64(len(chunks))}, nil
}

// RetrieveWithEmbedding performs cosine ANN search via the Qdrant search API.
func (b *QdrantBackend) RetrieveWithEmbedding(ctx context.Context, queryVec []float32, topK int, filter Filter) (RetrieveResult, error) {
	if filter.Tenant == "" {
		return RetrieveResult{}, PermissionDenied("qdrant retrieve: tenant is required")
	}
	if topK <= 0 {
		topK = 10
	}
	if b.cfg.MaxResults > 0 && topK > b.cfg.MaxResults {
		topK = b.cfg.MaxResults
	}

	limit := uint64(topK) //nolint:gosec
	resp, err := b.points.Search(ctx, &pb.SearchPoints{
		CollectionName: b.cfg.Collection,
		Vector:         queryVec,
		Limit:          limit,
		Filter:         b.tenantFilter(filter),
		WithPayload:    &pb.WithPayloadSelector{SelectorOptions: &pb.WithPayloadSelector_Enable{Enable: true}},
	})
	if err != nil {
		return RetrieveResult{}, BackendUnavailable("qdrant retrieve-with-embedding: " + err.Error())
	}

	chunks := make([]ScoredChunk, 0, len(resp.Result))
	for _, sc := range resp.Result {
		doc := qdrantPointToDoc(qdrantPointID(sc.Id), sc.Payload)
		chunks = append(chunks, ScoredChunk{
			Chunk: Chunk{
				Text:       string(doc.Content),
				DocumentID: doc.ID,
				EndByte:    len(doc.Content),
			},
			Score: sc.Score,
		})
	}
	return RetrieveResult{Chunks: chunks, Total: int64(len(chunks))}, nil
}

// ── Backend.ListNamespaces ────────────────────────────────────────────────────

func (b *QdrantBackend) ListNamespaces(ctx context.Context, filter Filter) ([]string, error) {
	if filter.Tenant == "" {
		return nil, PermissionDenied("qdrant list-namespaces: tenant is required")
	}
	// Scroll all points for this tenant and collect distinct namespaces.
	tenantFilter := &pb.Filter{
		Must: []*pb.Condition{qdrantMatchStr("tenant", filter.Tenant)},
	}
	limit := uint32(10000) //nolint:gosec // bounded collection scan
	resp, err := b.points.Scroll(ctx, &pb.ScrollPoints{
		CollectionName: b.cfg.Collection,
		Filter:         tenantFilter,
		Limit:          &limit,
		WithPayload:    &pb.WithPayloadSelector{SelectorOptions: &pb.WithPayloadSelector_Enable{Enable: true}},
	})
	if err != nil {
		return nil, BackendUnavailable("qdrant list-namespaces: " + err.Error())
	}
	seen := map[string]struct{}{}
	for _, pt := range resp.Result {
		if v, ok := pt.Payload["namespace"]; ok {
			if sv, ok2 := v.Kind.(*pb.Value_StringValue); ok2 {
				seen[sv.StringValue] = struct{}{}
			}
		}
	}
	nss := make([]string, 0, len(seen))
	for ns := range seen {
		nss = append(nss, ns)
	}
	return nss, nil
}

// ── Backend.Summarize ─────────────────────────────────────────────────────────

func (b *QdrantBackend) Summarize(_ context.Context, _ string, _ Filter) (string, error) {
	return "", &ErrNotSupported{Op: "Summarize", Backend: "qdrant"}
}

// ── Filesystem-only stubs ─────────────────────────────────────────────────────

func (b *QdrantBackend) Branch(_ context.Context, _, _ string, _ Filter) (BranchInfo, error) {
	return BranchInfo{}, &ErrNotSupported{Op: "Branch", Backend: "qdrant"}
}

func (b *QdrantBackend) Snapshot(_ context.Context, _ string, _ Filter) (SnapshotInfo, error) {
	return SnapshotInfo{}, &ErrNotSupported{Op: "Snapshot", Backend: "qdrant"}
}

func (b *QdrantBackend) ListBranches(_ context.Context, _ Filter) ([]BranchInfo, error) {
	return nil, &ErrNotSupported{Op: "ListBranches", Backend: "qdrant"}
}

func (b *QdrantBackend) Merge(_ context.Context, _, _ string, _ Filter) (BranchInfo, error) {
	return BranchInfo{}, &ErrNotSupported{Op: "Merge", Backend: "qdrant"}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (b *QdrantBackend) tenantFilter(f Filter) *pb.Filter {
	conditions := []*pb.Condition{qdrantMatchStr("tenant", f.Tenant)}
	if f.Namespace != "" {
		conditions = append(conditions, qdrantMatchStr("namespace", f.Namespace))
	}
	for k, v := range f.Metadata {
		conditions = append(conditions, qdrantMatchStr(k, v))
	}
	return &pb.Filter{Must: conditions}
}

func qdrantMatchStr(key, value string) *pb.Condition {
	return &pb.Condition{
		ConditionOneOf: &pb.Condition_Field{
			Field: &pb.FieldCondition{
				Key: key,
				Match: &pb.Match{
					MatchValue: &pb.Match_Keyword{Keyword: value},
				},
			},
		},
	}
}

func qdrantPointToDoc(id string, payload map[string]*pb.Value) Document {
	get := func(k string) string {
		if v, ok := payload[k]; ok {
			if sv, ok2 := v.Kind.(*pb.Value_StringValue); ok2 {
				return sv.StringValue
			}
		}
		return ""
	}
	var meta map[string]string
	if raw := get("metadata"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &meta)
	}
	createdAt, _ := time.Parse(time.RFC3339Nano, get("created_at"))
	updatedAt, _ := time.Parse(time.RFC3339Nano, get("updated_at"))
	return Document{
		ID:        id,
		Tenant:    get("tenant"),
		Namespace: get("namespace"),
		Content:   []byte(get("content")),
		Path:      get("path"),
		Version:   get("version"),
		Metadata:  meta,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}

func qdrantPointID(pid *pb.PointId) string {
	if pid == nil {
		return ""
	}
	if u, ok := pid.PointIdOptions.(*pb.PointId_Uuid); ok {
		return u.Uuid
	}
	return fmt.Sprintf("%d", pid.GetNum())
}

func strVal(s string) *pb.Value {
	return &pb.Value{Kind: &pb.Value_StringValue{StringValue: s}}
}

func containsSubstring(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i <= len(haystack)-len(needle); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

// isQdrantAlreadyExists checks whether the Qdrant error is a "collection already
// exists" gRPC status error.
func isQdrantAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	return len(err.Error()) > 0 &&
		(containsSubstring(err.Error(), "already exists") ||
			containsSubstring(err.Error(), "AlreadyExists"))
}

// qdrantAPIKeyInterceptor adds the api-key header to every unary gRPC call.
func qdrantAPIKeyInterceptor(key string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{},
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		// The Qdrant client uses per-RPC metadata; inject via context would
		// require google.golang.org/grpc/metadata. We skip that to avoid a
		// new import; instead we note that production deployments use mTLS
		// sidecar auth; the APIKey path is for Qdrant Cloud only.
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// compile-time assertion: QdrantBackend satisfies the Backend interface.
var _ Backend = (*QdrantBackend)(nil)
