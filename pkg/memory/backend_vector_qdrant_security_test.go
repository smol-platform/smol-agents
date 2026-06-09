package memory

import (
	"context"
	"testing"

	pb "github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// TestQdrantAPIKeyInterceptor verifies the interceptor actually injects the
// api-key header (x9i.4 — it was previously a documented no-op, so a configured
// key was silently ignored).
func TestQdrantAPIKeyInterceptor(t *testing.T) {
	const key = "test-api-key-123"
	var gotKey string
	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		if md, ok := metadata.FromOutgoingContext(ctx); ok {
			if v := md.Get("api-key"); len(v) > 0 {
				gotKey = v[0]
			}
		}
		return nil
	}
	ic := qdrantAPIKeyInterceptor(key)
	if err := ic(context.Background(), "/qdrant.Points/Search", nil, nil, nil, invoker); err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	if gotKey != key {
		t.Errorf("api-key header = %q, want %q", gotKey, key)
	}
}

// TestQdrantScoredFromPoint_TenantGuard verifies the defense-in-depth tenant
// guard (x9i.5): a point whose payload tenant != the caller's tenant is dropped
// and never surfaced, even though the server-side filter is the primary control.
func TestQdrantScoredFromPoint_TenantGuard(t *testing.T) {
	payloadA := map[string]*pb.Value{"tenant": strVal("tenant-a"), "namespace": strVal("ns"), "content": strVal("hello A")}
	payloadB := map[string]*pb.Value{"tenant": strVal("tenant-b"), "namespace": strVal("ns"), "content": strVal("secret B")}

	// Same tenant: kept, content surfaced.
	if sc, _, ok := qdrantScoredFromPoint("id-a", payloadA, "tenant-a", 1.0); !ok {
		t.Errorf("same-tenant point was dropped")
	} else if sc.Chunk.Text != "hello A" {
		t.Errorf("text = %q, want %q", sc.Chunk.Text, "hello A")
	}
	// Cross-tenant: dropped — a server-side filter regression must not leak it.
	if _, _, ok := qdrantScoredFromPoint("id-b", payloadB, "tenant-a", 1.0); ok {
		t.Error("cross-tenant point NOT dropped — D1 isolation breach")
	}
}
