// Package retrievalpb provides the gRPC transport adapters for the memory
// retrieval API (pkg/memory/api).
//
// Two adapters are exported:
//
//   - NewGRPCServer(svc api.RetrievalService, opts ...) *grpc.Server
//     Wraps an api.RetrievalService as a gRPC server that the retrieval worker
//     registers. Accepts grpc.ServerOption varargs so callers can inject mTLS
//     credentials (tls.Config → grpc.Creds) without this package knowing the
//     certificate management strategy.
//
//   - NewGRPCClient(conn grpc.ClientConnInterface) api.RetrievalService
//     Wraps the generated gRPC client as an api.RetrievalService so the
//     memory-mcp gateway can call workers over gRPC with the same interface it
//     uses for the HTTP transport.
//
// mTLS wiring (caller responsibility):
//
//	serverCreds := credentials.NewTLS(serverTLSConfig)
//	srv := NewGRPCServer(svc, grpc.Creds(serverCreds))
//
//	clientCreds := credentials.NewTLS(clientTLSConfig)
//	conn, _ := grpc.NewClient(addr, grpc.WithTransportCredentials(clientCreds))
//	client := NewGRPCClient(conn)
//
// Error mapping:
//   - api→gRPC: memory.Kind → gRPC status code (kindToCode)
//   - gRPC→api: gRPC status code → memory.Kind (codeToKind)
//
// The mapping is fail-closed: an unknown gRPC code becomes KindInternal.
// An unknown/zero Kind becomes codes.Internal.
//
// Implements R-MEM-WORK-1 (gRPC transport), steering doc (gRPC+mTLS),
// R-MEM-SEC-1 (fail-closed typed errors).
package retrievalpb

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/smol-platform/smol-agents/pkg/memory"
	apipkg "github.com/smol-platform/smol-agents/pkg/memory/api"
)

// ── Error mapping ─────────────────────────────────────────────────────────────

// kindToCode maps a memory.Kind to the most appropriate gRPC status code.
// Fail-closed: unknown Kind → codes.Internal.
func kindToCode(k memory.Kind) codes.Code {
	switch k {
	case memory.KindUnauthenticated:
		return codes.Unauthenticated
	case memory.KindPermissionDenied:
		return codes.PermissionDenied
	case memory.KindQuotaExceeded:
		return codes.ResourceExhausted
	case memory.KindNotFound:
		return codes.NotFound
	case memory.KindNotSupported:
		return codes.Unimplemented
	case memory.KindInvalid:
		return codes.InvalidArgument
	case memory.KindBackendUnavailable:
		return codes.Unavailable
	default:
		return codes.Internal
	}
}

// codeToKind maps a gRPC status code back to a memory.Kind.
// Fail-closed: unknown code → KindInternal.
func codeToKind(c codes.Code) memory.Kind {
	switch c {
	case codes.Unauthenticated:
		return memory.KindUnauthenticated
	case codes.PermissionDenied:
		return memory.KindPermissionDenied
	case codes.ResourceExhausted:
		return memory.KindQuotaExceeded
	case codes.NotFound:
		return memory.KindNotFound
	case codes.Unimplemented:
		return memory.KindNotSupported
	case codes.InvalidArgument:
		return memory.KindInvalid
	case codes.Unavailable:
		return memory.KindBackendUnavailable
	default:
		return memory.KindInternal
	}
}

// toGRPCErr wraps a Go error into a gRPC status error preserving the
// memory.Kind classification. nil → nil.
func toGRPCErr(err error) error {
	if err == nil {
		return nil
	}
	k := memory.KindOf(err)
	return status.Error(kindToCode(k), err.Error())
}

// fromGRPCErr converts a gRPC status error back to a *memory.Error.
// Non-status errors (including nil) are returned unchanged.
func fromGRPCErr(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	k := codeToKind(st.Code())
	return &memory.Error{Kind: k, Msg: st.Message()}
}

// ── Proto ↔ domain conversion helpers ────────────────────────────────────────

func identityToProto(id apipkg.RequestIdentity) *RequestIdentity {
	return &RequestIdentity{
		Tenant:         id.Tenant,
		Namespace:      id.Namespace,
		CallerSpiffeId: id.CallerSPIFFEID,
		RetrieverRef:   id.RetrieverRef,
	}
}

func identityFromProto(p *RequestIdentity) apipkg.RequestIdentity {
	if p == nil {
		return apipkg.RequestIdentity{}
	}
	return apipkg.RequestIdentity{
		Tenant:         p.Tenant,
		Namespace:      p.Namespace,
		CallerSPIFFEID: p.CallerSpiffeId,
		RetrieverRef:   p.RetrieverRef,
	}
}

func filterToProto(f memory.Filter) *Filter {
	return &Filter{
		Namespace: f.Namespace,
		Tenant:    f.Tenant,
		Metadata:  f.Metadata,
	}
}

func filterFromProto(p *Filter) memory.Filter {
	if p == nil {
		return memory.Filter{}
	}
	return memory.Filter{
		Namespace: p.Namespace,
		Tenant:    p.Tenant,
		Metadata:  p.Metadata,
	}
}

func documentToProto(d memory.Document) *Document {
	return &Document{
		Id:            d.ID,
		Namespace:     d.Namespace,
		Tenant:        d.Tenant,
		Content:       d.Content,
		Path:          d.Path,
		Metadata:      d.Metadata,
		Embedding:     d.Embedding,
		Version:       d.Version,
		CreatedAtUnix: d.CreatedAt.Unix(),
		UpdatedAtUnix: d.UpdatedAt.Unix(),
	}
}

func documentFromProto(p *Document) memory.Document {
	if p == nil {
		return memory.Document{}
	}
	return memory.Document{
		ID:        p.Id,
		Namespace: p.Namespace,
		Tenant:    p.Tenant,
		Content:   p.Content,
		Path:      p.Path,
		Metadata:  p.Metadata,
		Embedding: p.Embedding,
		Version:   p.Version,
		CreatedAt: time.Unix(p.CreatedAtUnix, 0).UTC(),
		UpdatedAt: time.Unix(p.UpdatedAtUnix, 0).UTC(),
	}
}

func chunksFromProto(ps []*ScoredChunk) []memory.ScoredChunk {
	out := make([]memory.ScoredChunk, 0, len(ps))
	for _, p := range ps {
		sc := memory.ScoredChunk{Score: p.Score}
		if p.Chunk != nil {
			sc.Chunk = memory.Chunk{
				Index:      int(p.Chunk.Index),
				Text:       p.Chunk.Text,
				Embedding:  p.Chunk.Embedding,
				DocumentID: p.Chunk.DocumentId,
				StartByte:  int(p.Chunk.StartByte),
				EndByte:    int(p.Chunk.EndByte),
			}
		}
		out = append(out, sc)
	}
	return out
}

func chunksToProto(cs []memory.ScoredChunk) []*ScoredChunk {
	out := make([]*ScoredChunk, 0, len(cs))
	for _, c := range cs {
		out = append(out, &ScoredChunk{
			Score: c.Score,
			Chunk: &Chunk{
				Index:      int32(c.Chunk.Index),
				Text:       c.Chunk.Text,
				Embedding:  c.Chunk.Embedding,
				DocumentId: c.Chunk.DocumentID,
				StartByte:  int32(c.Chunk.StartByte),
				EndByte:    int32(c.Chunk.EndByte),
			},
		})
	}
	return out
}

func branchInfoFromProto(p *BranchInfo) memory.BranchInfo {
	if p == nil {
		return memory.BranchInfo{}
	}
	b := memory.BranchInfo{
		Name:      p.Name,
		Base:      p.Base,
		CreatedAt: time.Unix(p.CreatedAtUnix, 0).UTC(),
	}
	if p.CommittedAtUnix > 0 {
		b.CommittedAt = time.Unix(p.CommittedAtUnix, 0).UTC()
	}
	if p.DiscardedAtUnix > 0 {
		b.DiscardedAt = time.Unix(p.DiscardedAtUnix, 0).UTC()
	}
	return b
}

func branchInfoToProto(b memory.BranchInfo) *BranchInfo {
	return &BranchInfo{
		Name:            b.Name,
		Base:            b.Base,
		CreatedAtUnix:   b.CreatedAt.Unix(),
		CommittedAtUnix: b.CommittedAt.Unix(),
		DiscardedAtUnix: b.DiscardedAt.Unix(),
	}
}

func branchesFromProto(ps []*BranchInfo) []memory.BranchInfo {
	out := make([]memory.BranchInfo, 0, len(ps))
	for _, p := range ps {
		out = append(out, branchInfoFromProto(p))
	}
	return out
}

func snapshotInfoFromProto(p *SnapshotInfo) memory.SnapshotInfo {
	if p == nil {
		return memory.SnapshotInfo{}
	}
	return memory.SnapshotInfo{
		ID:        p.Id,
		Branch:    p.Branch,
		CreatedAt: time.Unix(p.CreatedAtUnix, 0).UTC(),
		SizeBytes: p.SizeBytes,
	}
}

func conflictInfoToProto(ci memory.ConflictInfo) *ConflictInfo {
	return &ConflictInfo{Path: ci.Path, Kind: ci.Kind}
}

func conflictInfoFromProto(p *ConflictInfo) memory.ConflictInfo {
	if p == nil {
		return memory.ConflictInfo{}
	}
	return memory.ConflictInfo{Path: p.Path, Kind: p.Kind}
}

func conflictInfosToProto(cis []memory.ConflictInfo) []*ConflictInfo {
	out := make([]*ConflictInfo, 0, len(cis))
	for _, ci := range cis {
		out = append(out, conflictInfoToProto(ci))
	}
	return out
}

func conflictInfosFromProto(ps []*ConflictInfo) []memory.ConflictInfo {
	out := make([]memory.ConflictInfo, 0, len(ps))
	for _, p := range ps {
		out = append(out, conflictInfoFromProto(p))
	}
	return out
}

func snapshotInfoToProto(s memory.SnapshotInfo) *SnapshotInfo {
	return &SnapshotInfo{
		Id:            s.ID,
		Branch:        s.Branch,
		CreatedAtUnix: s.CreatedAt.Unix(),
		SizeBytes:     s.SizeBytes,
	}
}

// ── Server adapter ────────────────────────────────────────────────────────────

// grpcServer wraps api.RetrievalService as a gRPC RetrievalServiceServer.
type grpcServer struct {
	UnimplementedRetrievalServiceServer
	svc apipkg.RetrievalService
}

// NewGRPCServer registers svc as a gRPC service and returns the *grpc.Server.
// Pass grpc.ServerOption values (e.g. grpc.Creds) for mTLS configuration.
// The returned server has not been started; callers call srv.Serve(lis).
func NewGRPCServer(svc apipkg.RetrievalService, opts ...grpc.ServerOption) *grpc.Server {
	srv := grpc.NewServer(opts...)
	RegisterRetrievalServiceServer(srv, &grpcServer{svc: svc})
	return srv
}

func (s *grpcServer) Retrieve(ctx context.Context, req *RetrieveRequest) (*RetrieveResponse, error) {
	resp, err := s.svc.Retrieve(ctx, &apipkg.RetrieveRequest{
		Identity: identityFromProto(req.Identity),
		Query:    req.Query,
		TopK:     req.TopK,
		Filters:  filterFromProto(req.Filters),
		StoreRef: req.StoreRef,
	})
	if err != nil {
		return nil, toGRPCErr(err)
	}
	return &RetrieveResponse{
		Chunks: chunksToProto(resp.Result.Chunks),
		Total:  resp.Result.Total,
	}, nil
}

func (s *grpcServer) Write(ctx context.Context, req *WriteRequest) (*WriteResponse, error) {
	resp, err := s.svc.Write(ctx, &apipkg.WriteRequest{
		Identity: identityFromProto(req.Identity),
		Document: documentFromProto(req.Document),
		TraT:     req.Trat,
	})
	if err != nil {
		return nil, toGRPCErr(err)
	}
	return &WriteResponse{Id: resp.Result.ID, Version: resp.Result.Version}, nil
}

func (s *grpcServer) Get(ctx context.Context, req *GetRequest) (*GetResponse, error) {
	resp, err := s.svc.Get(ctx, &apipkg.GetRequest{
		Identity: identityFromProto(req.Identity),
		ID:       req.Id,
	})
	if err != nil {
		return nil, toGRPCErr(err)
	}
	return &GetResponse{Document: documentToProto(resp.Document)}, nil
}

func (s *grpcServer) Delete(ctx context.Context, req *DeleteRequest) (*DeleteResponse, error) {
	_, err := s.svc.Delete(ctx, &apipkg.DeleteRequest{
		Identity: identityFromProto(req.Identity),
		ID:       req.Id,
		TraT:     req.Trat,
	})
	if err != nil {
		return nil, toGRPCErr(err)
	}
	return &DeleteResponse{}, nil
}

func (s *grpcServer) ListNamespaces(ctx context.Context, req *ListNamespacesRequest) (*ListNamespacesResponse, error) {
	resp, err := s.svc.ListNamespaces(ctx, &apipkg.ListNamespacesRequest{
		Identity: identityFromProto(req.Identity),
	})
	if err != nil {
		return nil, toGRPCErr(err)
	}
	return &ListNamespacesResponse{Namespaces: resp.Namespaces}, nil
}

func (s *grpcServer) Summarize(ctx context.Context, req *SummarizeRequest) (*SummarizeResponse, error) {
	resp, err := s.svc.Summarize(ctx, &apipkg.SummarizeRequest{
		Identity: identityFromProto(req.Identity),
		Query:    req.Query,
	})
	if err != nil {
		return nil, toGRPCErr(err)
	}
	return &SummarizeResponse{Summary: resp.Summary}, nil
}

func (s *grpcServer) BranchFS(ctx context.Context, req *BranchFSRequest) (*BranchFSResponse, error) {
	resp, err := s.svc.BranchFS(ctx, &apipkg.BranchFSRequest{
		Identity:   identityFromProto(req.Identity),
		BaseBranch: req.BaseBranch,
		NewBranch:  req.NewBranch,
	})
	if err != nil {
		return nil, toGRPCErr(err)
	}
	return &BranchFSResponse{Branch: branchInfoToProto(resp.Branch)}, nil
}

func (s *grpcServer) SnapshotFS(ctx context.Context, req *SnapshotFSRequest) (*SnapshotFSResponse, error) {
	resp, err := s.svc.SnapshotFS(ctx, &apipkg.SnapshotFSRequest{
		Identity: identityFromProto(req.Identity),
		Branch:   req.Branch,
	})
	if err != nil {
		return nil, toGRPCErr(err)
	}
	return &SnapshotFSResponse{Snapshot: snapshotInfoToProto(resp.Snapshot)}, nil
}

func (s *grpcServer) ListBranches(ctx context.Context, req *ListBranchesRequest) (*ListBranchesResponse, error) {
	resp, err := s.svc.ListBranches(ctx, &apipkg.ListBranchesRequest{
		Identity: identityFromProto(req.Identity),
	})
	if err != nil {
		return nil, toGRPCErr(err)
	}
	return &ListBranchesResponse{Branches: func() []*BranchInfo {
		out := make([]*BranchInfo, 0, len(resp.Branches))
		for _, b := range resp.Branches {
			out = append(out, branchInfoToProto(b))
		}
		return out
	}()}, nil
}

func (s *grpcServer) MergeFS(ctx context.Context, req *MergeFSRequest) (*MergeFSResponse, error) {
	resp, err := s.svc.MergeFS(ctx, &apipkg.MergeFSRequest{
		Identity:   identityFromProto(req.Identity),
		SrcBranch:  req.SrcBranch,
		DstBranch:  req.DstBranch,
		OnConflict: req.OnConflict,
		DryRun:     req.DryRun,
	})
	if err != nil {
		return nil, toGRPCErr(err)
	}
	return &MergeFSResponse{
		Branch:    branchInfoToProto(resp.Branch),
		Conflicts: conflictInfosToProto(resp.Conflicts),
		Committed: resp.Committed,
		Merged:    int32(resp.Merged),
		Added:     int32(resp.Added),
		Deleted:   int32(resp.Deleted),
	}, nil
}

// compile-time check: grpcServer satisfies the generated server interface.
var _ RetrievalServiceServer = (*grpcServer)(nil)

// ── Client adapter ────────────────────────────────────────────────────────────

// grpcClient wraps the generated gRPC client as api.RetrievalService.
type grpcClient struct {
	pb RetrievalServiceClient
}

// NewGRPCClient returns an api.RetrievalService backed by a gRPC connection.
// Pass a grpc.ClientConnInterface (e.g. *grpc.ClientConn) obtained with
// grpc.NewClient and transport credentials for mTLS.
func NewGRPCClient(conn grpc.ClientConnInterface) apipkg.RetrievalService {
	return &grpcClient{pb: NewRetrievalServiceClient(conn)}
}

func (c *grpcClient) Retrieve(ctx context.Context, req *apipkg.RetrieveRequest) (*apipkg.RetrieveResponse, error) {
	resp, err := c.pb.Retrieve(ctx, &RetrieveRequest{
		Identity: identityToProto(req.Identity),
		Query:    req.Query,
		TopK:     req.TopK,
		Filters:  filterToProto(req.Filters),
		StoreRef: req.StoreRef,
	})
	if err != nil {
		return nil, fromGRPCErr(err)
	}
	return &apipkg.RetrieveResponse{
		Result: memory.RetrieveResult{
			Chunks: chunksFromProto(resp.Chunks),
			Total:  resp.Total,
		},
	}, nil
}

func (c *grpcClient) Write(ctx context.Context, req *apipkg.WriteRequest) (*apipkg.WriteResponse, error) {
	resp, err := c.pb.Write(ctx, &WriteRequest{
		Identity: identityToProto(req.Identity),
		Document: documentToProto(req.Document),
		Trat:     req.TraT,
	})
	if err != nil {
		return nil, fromGRPCErr(err)
	}
	return &apipkg.WriteResponse{
		Result: memory.WriteResult{ID: resp.Id, Version: resp.Version},
	}, nil
}

func (c *grpcClient) Get(ctx context.Context, req *apipkg.GetRequest) (*apipkg.GetResponse, error) {
	resp, err := c.pb.Get(ctx, &GetRequest{
		Identity: identityToProto(req.Identity),
		Id:       req.ID,
	})
	if err != nil {
		return nil, fromGRPCErr(err)
	}
	return &apipkg.GetResponse{Document: documentFromProto(resp.Document)}, nil
}

func (c *grpcClient) Delete(ctx context.Context, req *apipkg.DeleteRequest) (*apipkg.DeleteResponse, error) {
	_, err := c.pb.Delete(ctx, &DeleteRequest{
		Identity: identityToProto(req.Identity),
		Id:       req.ID,
		Trat:     req.TraT,
	})
	if err != nil {
		return nil, fromGRPCErr(err)
	}
	return &apipkg.DeleteResponse{}, nil
}

func (c *grpcClient) ListNamespaces(ctx context.Context, req *apipkg.ListNamespacesRequest) (*apipkg.ListNamespacesResponse, error) {
	resp, err := c.pb.ListNamespaces(ctx, &ListNamespacesRequest{
		Identity: identityToProto(req.Identity),
	})
	if err != nil {
		return nil, fromGRPCErr(err)
	}
	return &apipkg.ListNamespacesResponse{Namespaces: resp.Namespaces}, nil
}

func (c *grpcClient) Summarize(ctx context.Context, req *apipkg.SummarizeRequest) (*apipkg.SummarizeResponse, error) {
	resp, err := c.pb.Summarize(ctx, &SummarizeRequest{
		Identity: identityToProto(req.Identity),
		Query:    req.Query,
	})
	if err != nil {
		return nil, fromGRPCErr(err)
	}
	return &apipkg.SummarizeResponse{Summary: resp.Summary}, nil
}

func (c *grpcClient) BranchFS(ctx context.Context, req *apipkg.BranchFSRequest) (*apipkg.BranchFSResponse, error) {
	resp, err := c.pb.BranchFS(ctx, &BranchFSRequest{
		Identity:   identityToProto(req.Identity),
		BaseBranch: req.BaseBranch,
		NewBranch:  req.NewBranch,
	})
	if err != nil {
		return nil, fromGRPCErr(err)
	}
	return &apipkg.BranchFSResponse{Branch: branchInfoFromProto(resp.Branch)}, nil
}

func (c *grpcClient) SnapshotFS(ctx context.Context, req *apipkg.SnapshotFSRequest) (*apipkg.SnapshotFSResponse, error) {
	resp, err := c.pb.SnapshotFS(ctx, &SnapshotFSRequest{
		Identity: identityToProto(req.Identity),
		Branch:   req.Branch,
	})
	if err != nil {
		return nil, fromGRPCErr(err)
	}
	return &apipkg.SnapshotFSResponse{Snapshot: snapshotInfoFromProto(resp.Snapshot)}, nil
}

func (c *grpcClient) ListBranches(ctx context.Context, req *apipkg.ListBranchesRequest) (*apipkg.ListBranchesResponse, error) {
	resp, err := c.pb.ListBranches(ctx, &ListBranchesRequest{
		Identity: identityToProto(req.Identity),
	})
	if err != nil {
		return nil, fromGRPCErr(err)
	}
	return &apipkg.ListBranchesResponse{
		Branches: branchesFromProto(resp.Branches),
	}, nil
}

func (c *grpcClient) MergeFS(ctx context.Context, req *apipkg.MergeFSRequest) (*apipkg.MergeFSResponse, error) {
	resp, err := c.pb.MergeFS(ctx, &MergeFSRequest{
		Identity:   identityToProto(req.Identity),
		SrcBranch:  req.SrcBranch,
		DstBranch:  req.DstBranch,
		OnConflict: req.OnConflict,
		DryRun:     req.DryRun,
	})
	if err != nil {
		return nil, fromGRPCErr(err)
	}
	return &apipkg.MergeFSResponse{
		Branch:    branchInfoFromProto(resp.Branch),
		Conflicts: conflictInfosFromProto(resp.Conflicts),
		Committed: resp.Committed,
		Merged:    int(resp.Merged),
		Added:     int(resp.Added),
		Deleted:   int(resp.Deleted),
	}, nil
}

// compile-time check: grpcClient satisfies the api.RetrievalService interface.
var _ apipkg.RetrievalService = (*grpcClient)(nil)
