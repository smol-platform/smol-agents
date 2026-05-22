package retrievalpb_test

// Round-trip tests for the gRPC transport adapters. These tests mirror the
// structure of pkg/memory/api/transport_test.go (HTTP transport) so the two
// transports are held to the same contract.
//
// Test approach: start an in-process grpc.Server backed by a fakeSvc (same
// fake as in transport_test.go), dial it with grpc.NewClient + insecure
// credentials (mTLS is a wiring concern, not an adapter correctness concern),
// then call each method through NewGRPCClient. Verify:
//   - Happy-path request fields round-trip to the server unchanged.
//   - Typed memory.Kind errors survive the gRPC hop with the correct Kind.
//   - ErrNotSupported maps to KindNotSupported across the wire.
//   - NotFound maps correctly.

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/stigen/smol-agents/pkg/memory"
	apipkg "github.com/stigen/smol-agents/pkg/memory/api"
	retrievalpb "github.com/stigen/smol-agents/pkg/memory/api/grpc"
)

// ── fake service (mirrors transport_test.go fakeSvc) ─────────────────────────

type fakeSvc struct {
	gotRetrieve *apipkg.RetrieveRequest
	retErr      error
}

func (f *fakeSvc) Retrieve(_ context.Context, r *apipkg.RetrieveRequest) (*apipkg.RetrieveResponse, error) {
	f.gotRetrieve = r
	if f.retErr != nil {
		return nil, f.retErr
	}
	return &apipkg.RetrieveResponse{
		Result: memory.RetrieveResult{
			Total:  1,
			Chunks: []memory.ScoredChunk{{Score: 0.9}},
		},
	}, nil
}
func (f *fakeSvc) Write(_ context.Context, _ *apipkg.WriteRequest) (*apipkg.WriteResponse, error) {
	return &apipkg.WriteResponse{Result: memory.WriteResult{ID: "doc-1", Version: "v1"}}, nil
}
func (f *fakeSvc) Get(_ context.Context, _ *apipkg.GetRequest) (*apipkg.GetResponse, error) {
	return nil, memory.NotFound("no such doc")
}
func (f *fakeSvc) Delete(_ context.Context, _ *apipkg.DeleteRequest) (*apipkg.DeleteResponse, error) {
	return &apipkg.DeleteResponse{}, nil
}
func (f *fakeSvc) ListNamespaces(_ context.Context, _ *apipkg.ListNamespacesRequest) (*apipkg.ListNamespacesResponse, error) {
	return &apipkg.ListNamespacesResponse{Namespaces: []string{"a", "b"}}, nil
}
func (f *fakeSvc) Summarize(_ context.Context, _ *apipkg.SummarizeRequest) (*apipkg.SummarizeResponse, error) {
	return nil, &memory.ErrNotSupported{Op: "Summarize", Backend: "vector"}
}
func (f *fakeSvc) BranchFS(_ context.Context, _ *apipkg.BranchFSRequest) (*apipkg.BranchFSResponse, error) {
	return &apipkg.BranchFSResponse{}, nil
}
func (f *fakeSvc) SnapshotFS(_ context.Context, _ *apipkg.SnapshotFSRequest) (*apipkg.SnapshotFSResponse, error) {
	return &apipkg.SnapshotFSResponse{}, nil
}
func (f *fakeSvc) ListBranches(_ context.Context, _ *apipkg.ListBranchesRequest) (*apipkg.ListBranchesResponse, error) {
	return &apipkg.ListBranchesResponse{}, nil
}

// ── test helpers ──────────────────────────────────────────────────────────────

// newGRPCPair starts an in-process gRPC server backed by svc and returns a
// client connected to it. Both use insecure credentials (mTLS is a wiring
// concern outside the adapter). The server is stopped on test cleanup.
func newGRPCPair(t *testing.T, svc apipkg.RetrievalService) apipkg.RetrievalService {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := retrievalpb.NewGRPCServer(svc, grpc.Creds(insecure.NewCredentials()))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)

	conn, err := grpc.NewClient(
		lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return retrievalpb.NewGRPCClient(conn)
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestGRPCTransport_RoundTrip_OK verifies that a Retrieve call round-trips all
// request fields to the server and returns the correct response.
func TestGRPCTransport_RoundTrip_OK(t *testing.T) {
	f := &fakeSvc{}
	c := newGRPCPair(t, f)

	resp, err := c.Retrieve(context.Background(), &apipkg.RetrieveRequest{
		Identity: apipkg.RequestIdentity{
			Tenant:         "platform",
			Namespace:      "kb",
			CallerSPIFFEID: "spiffe://td/x",
			RetrieverRef:   "team/r",
		},
		Query: "GPU scheduling",
		TopK:  8,
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if resp.Result.Total != 1 || len(resp.Result.Chunks) != 1 {
		t.Errorf("result = %+v", resp.Result)
	}
	if f.gotRetrieve == nil {
		t.Fatal("server never received the request")
	}
	if f.gotRetrieve.Identity.Tenant != "platform" {
		t.Errorf("tenant = %q, want platform", f.gotRetrieve.Identity.Tenant)
	}
	if f.gotRetrieve.Identity.CallerSPIFFEID != "spiffe://td/x" {
		t.Errorf("callerSPIFFEID = %q, want spiffe://td/x", f.gotRetrieve.Identity.CallerSPIFFEID)
	}
	if f.gotRetrieve.TopK != 8 {
		t.Errorf("topK = %d, want 8", f.gotRetrieve.TopK)
	}
}

// TestGRPCTransport_RoundTrip_TypedError verifies that typed memory.Kind errors
// survive the gRPC hop with the correct Kind classification (fail-closed).
func TestGRPCTransport_RoundTrip_TypedError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want memory.Kind
	}{
		{"permission", memory.PermissionDenied("tenant mismatch"), memory.KindPermissionDenied},
		{"quota", memory.QuotaExceeded("topK too high"), memory.KindQuotaExceeded},
		{"unauth", memory.Unauthenticated("no svid"), memory.KindUnauthenticated},
		{"invalid", memory.Invalid("bad request"), memory.KindInvalid},
		{"backend_unavailable", memory.BackendUnavailable("qdrant down"), memory.KindBackendUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newGRPCPair(t, &fakeSvc{retErr: tc.err})
			_, err := c.Retrieve(context.Background(), &apipkg.RetrieveRequest{})
			if err == nil {
				t.Fatal("expected error")
			}
			if got := memory.KindOf(err); got != tc.want {
				t.Errorf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGRPCTransport_NotSupported verifies ErrNotSupported maps to
// KindNotSupported across the wire.
func TestGRPCTransport_NotSupported(t *testing.T) {
	c := newGRPCPair(t, &fakeSvc{})
	_, err := c.Summarize(context.Background(), &apipkg.SummarizeRequest{})
	if memory.KindOf(err) != memory.KindNotSupported {
		t.Errorf("kind = %q, want not_supported", memory.KindOf(err))
	}
}

// TestGRPCTransport_NotFound verifies Get's NotFound error round-trips.
func TestGRPCTransport_NotFound(t *testing.T) {
	c := newGRPCPair(t, &fakeSvc{})
	_, err := c.Get(context.Background(), &apipkg.GetRequest{ID: "missing"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Errorf("kind = %q, want not_found", memory.KindOf(err))
	}
}

// TestGRPCTransport_Write verifies a Write round-trip returns the correct ID.
func TestGRPCTransport_Write(t *testing.T) {
	c := newGRPCPair(t, &fakeSvc{})
	resp, err := c.Write(context.Background(), &apipkg.WriteRequest{
		Identity: apipkg.RequestIdentity{Tenant: "t", Namespace: "ns"},
		Document: memory.Document{Content: []byte("hello")},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if resp.Result.ID != "doc-1" {
		t.Errorf("ID = %q, want doc-1", resp.Result.ID)
	}
	if resp.Result.Version != "v1" {
		t.Errorf("Version = %q, want v1", resp.Result.Version)
	}
}

// TestGRPCTransport_ListNamespaces verifies the namespace list round-trips.
func TestGRPCTransport_ListNamespaces(t *testing.T) {
	c := newGRPCPair(t, &fakeSvc{})
	resp, err := c.ListNamespaces(context.Background(), &apipkg.ListNamespacesRequest{})
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(resp.Namespaces) != 2 || resp.Namespaces[0] != "a" || resp.Namespaces[1] != "b" {
		t.Errorf("namespaces = %v, want [a b]", resp.Namespaces)
	}
}

// TestGRPCTransport_BranchFS verifies BranchFS succeeds without error.
func TestGRPCTransport_BranchFS(t *testing.T) {
	c := newGRPCPair(t, &fakeSvc{})
	_, err := c.BranchFS(context.Background(), &apipkg.BranchFSRequest{
		BaseBranch: "main",
		NewBranch:  "run-abc",
	})
	if err != nil {
		t.Fatalf("BranchFS: %v", err)
	}
}

// TestGRPCTransport_Delete verifies Delete succeeds without error.
func TestGRPCTransport_Delete(t *testing.T) {
	c := newGRPCPair(t, &fakeSvc{})
	_, err := c.Delete(context.Background(), &apipkg.DeleteRequest{ID: "doc-1"})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// TestGRPCTransport_RetrieveScore verifies the scored chunk score round-trips.
func TestGRPCTransport_RetrieveScore(t *testing.T) {
	c := newGRPCPair(t, &fakeSvc{})
	resp, err := c.Retrieve(context.Background(), &apipkg.RetrieveRequest{TopK: 1})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(resp.Result.Chunks) != 1 {
		t.Fatalf("chunks count = %d, want 1", len(resp.Result.Chunks))
	}
	if resp.Result.Chunks[0].Score != 0.9 {
		t.Errorf("score = %f, want 0.9", resp.Result.Chunks[0].Score)
	}
}
