// worker_helpers_test.go — shared test helpers for the worker package tests.
package worker_test

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/smol-platform/smol-agents/pkg/memory"
	"github.com/smol-platform/smol-agents/pkg/memory/api"
	"github.com/smol-platform/smol-agents/pkg/memory/worker"
)

// newWorkerBackend returns a fresh in-memory VectorBackend.
func newWorkerBackend(t *testing.T) memory.Backend {
	t.Helper()
	return memory.NewVectorBackend()
}

// newWorkerWithSummarizer constructs a Worker with a FakeEmbedder and an
// attached Summarizer.
func newWorkerWithSummarizer(t *testing.T, b memory.Backend, s worker.Summarizer) *worker.Worker {
	t.Helper()
	emb, err := worker.NewFakeEmbedder(16)
	if err != nil {
		t.Fatal(err)
	}
	w, err := worker.New(
		worker.Config{Chunk: worker.ChunkSpec{MaxBytes: 512, OverlapBytes: 64}},
		worker.StaticSelector(b),
		emb,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	)
	if err != nil {
		t.Fatal(err)
	}
	w.WithSummarizer(s)
	return w
}

// writeWorkerDoc writes a document via the Worker using a valid identity.
func writeWorkerDoc(t *testing.T, w *worker.Worker, tenant, ns, content string) string {
	t.Helper()
	resp, err := w.Write(context.Background(), &api.WriteRequest{
		Identity: api.RequestIdentity{
			Tenant:         tenant,
			Namespace:      ns,
			CallerSPIFFEID: "spiffe://td/" + tenant,
			RetrieverRef:   tenant + "/default",
		},
		Document: memory.Document{Content: []byte(content)},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	return resp.Result.ID
}

// workerSummarizeReq builds a SummarizeRequest for the given identity + query.
func workerSummarizeReq(tenant, ns, query string) api.SummarizeRequest {
	return api.SummarizeRequest{
		Identity: api.RequestIdentity{
			Tenant:         tenant,
			Namespace:      ns,
			CallerSPIFFEID: "spiffe://td/" + tenant,
			RetrieverRef:   tenant + "/default",
		},
		Query: query,
	}
}
