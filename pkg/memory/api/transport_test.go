package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stigen/smol-agents/pkg/memory"
)

// fakeSvc records the last request and returns a canned result or error.
type fakeSvc struct {
	gotRetrieve *RetrieveRequest
	retErr      error
}

func (f *fakeSvc) Retrieve(_ context.Context, r *RetrieveRequest) (*RetrieveResponse, error) {
	f.gotRetrieve = r
	if f.retErr != nil {
		return nil, f.retErr
	}
	return &RetrieveResponse{Result: memory.RetrieveResult{Total: 1, Chunks: []memory.ScoredChunk{{Score: 0.9}}}}, nil
}
func (f *fakeSvc) Write(context.Context, *WriteRequest) (*WriteResponse, error) {
	return &WriteResponse{Result: memory.WriteResult{ID: "doc-1", Version: "v1"}}, nil
}
func (f *fakeSvc) Get(context.Context, *GetRequest) (*GetResponse, error) {
	return nil, memory.NotFound("no such doc")
}
func (f *fakeSvc) Delete(context.Context, *DeleteRequest) (*DeleteResponse, error) {
	return &DeleteResponse{}, nil
}
func (f *fakeSvc) ListNamespaces(context.Context, *ListNamespacesRequest) (*ListNamespacesResponse, error) {
	return &ListNamespacesResponse{Namespaces: []string{"a", "b"}}, nil
}
func (f *fakeSvc) Summarize(context.Context, *SummarizeRequest) (*SummarizeResponse, error) {
	return nil, &memory.ErrNotSupported{Op: "Summarize", Backend: "vector"}
}
func (f *fakeSvc) BranchFS(context.Context, *BranchFSRequest) (*BranchFSResponse, error) {
	return &BranchFSResponse{}, nil
}
func (f *fakeSvc) SnapshotFS(context.Context, *SnapshotFSRequest) (*SnapshotFSResponse, error) {
	return &SnapshotFSResponse{}, nil
}
func (f *fakeSvc) ListBranches(context.Context, *ListBranchesRequest) (*ListBranchesResponse, error) {
	return &ListBranchesResponse{}, nil
}
func (f *fakeSvc) MergeFS(context.Context, *MergeFSRequest) (*MergeFSResponse, error) {
	return &MergeFSResponse{}, nil
}

func newPair(t *testing.T, svc RetrievalService) RetrievalService {
	t.Helper()
	ts := httptest.NewServer(NewHTTPServer(svc))
	t.Cleanup(ts.Close)
	return NewHTTPClient(ts.URL, ts.Client())
}

func TestTransport_RoundTrip_OK(t *testing.T) {
	f := &fakeSvc{}
	c := newPair(t, f)
	resp, err := c.Retrieve(context.Background(), &RetrieveRequest{
		Identity: RequestIdentity{Tenant: "platform", Namespace: "kb", CallerSPIFFEID: "spiffe://td/x", RetrieverRef: "team/r"},
		Query:    "GPU scheduling", TopK: 8,
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if resp.Result.Total != 1 || len(resp.Result.Chunks) != 1 {
		t.Errorf("result = %+v", resp.Result)
	}
	// Identity round-tripped to the server unchanged.
	if f.gotRetrieve == nil || f.gotRetrieve.Identity.Tenant != "platform" || f.gotRetrieve.TopK != 8 {
		t.Errorf("server got = %+v", f.gotRetrieve)
	}
}

// A typed error keeps its Kind across the HTTP hop (fail-closed classification).
func TestTransport_RoundTrip_TypedError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want memory.Kind
	}{
		{"permission", memory.PermissionDenied("tenant mismatch"), memory.KindPermissionDenied},
		{"quota", memory.QuotaExceeded("topK too high"), memory.KindQuotaExceeded},
		{"unauth", memory.Unauthenticated("no svid"), memory.KindUnauthenticated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newPair(t, &fakeSvc{retErr: tc.err})
			_, err := c.Retrieve(context.Background(), &RetrieveRequest{})
			if err == nil {
				t.Fatal("expected error")
			}
			if got := memory.KindOf(err); got != tc.want {
				t.Errorf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

// ErrNotSupported maps to KindNotSupported across the hop.
func TestTransport_NotSupported(t *testing.T) {
	c := newPair(t, &fakeSvc{})
	_, err := c.Summarize(context.Background(), &SummarizeRequest{})
	if memory.KindOf(err) != memory.KindNotSupported {
		t.Errorf("kind = %q, want not_supported", memory.KindOf(err))
	}
}

// Get's NotFound round-trips.
func TestTransport_NotFound(t *testing.T) {
	c := newPair(t, &fakeSvc{})
	_, err := c.Get(context.Background(), &GetRequest{ID: "missing"})
	if memory.KindOf(err) != memory.KindNotFound {
		t.Errorf("kind = %q, want not_found", memory.KindOf(err))
	}
}

// MergeFS round-trips successfully over HTTP.
func TestTransport_MergeFS_RoundTrip(t *testing.T) {
	c := newPair(t, &fakeSvc{})
	resp, err := c.MergeFS(context.Background(), &MergeFSRequest{
		Identity: RequestIdentity{
			Tenant:         "tenant-a",
			Namespace:      "ns",
			CallerSPIFFEID: "spiffe://td/x",
			RetrieverRef:   "team/r",
		},
		SrcBranch: "run-001",
		DstBranch: "main",
	})
	if err != nil {
		t.Fatalf("MergeFS: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

// mergeFSNotSupportedSvc is a fakeSvc that returns ErrNotSupported from MergeFS.
type mergeFSNotSupportedSvc struct {
	fakeSvc
}

func (m *mergeFSNotSupportedSvc) MergeFS(_ context.Context, _ *MergeFSRequest) (*MergeFSResponse, error) {
	return nil, &memory.ErrNotSupported{Op: "Merge", Backend: "vector-inmem"}
}

// MergeFS KindNotSupported survives the HTTP hop.
func TestTransport_MergeFS_NotSupported(t *testing.T) {
	c := newPair(t, &mergeFSNotSupportedSvc{})
	_, err := c.MergeFS(context.Background(), &MergeFSRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := memory.KindOf(err); got != memory.KindNotSupported {
		t.Errorf("kind = %q, want not_supported", got)
	}
}
