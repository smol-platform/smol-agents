package agentepisode

import (
	"context"
	"strconv"
	"testing"

	"github.com/smol-platform/smol-agents/pkg/memory"
	"github.com/smol-platform/smol-agents/pkg/memory/api"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// fakeSvc records written documents and returns them (newest-first) on Retrieve,
// echoing the D1 scoping it was given so the test can assert tenant/ns flow.
type fakeSvc struct {
	api.RetrievalService // embedded: only Write + Retrieve are exercised
	docs                 []memory.Document
	gotTenant            string
	gotNS                string
	gotTopK              int32
}

func (f *fakeSvc) Write(_ context.Context, req *api.WriteRequest) (*api.WriteResponse, error) {
	d := req.Document
	d.ID = "ep-" + strconv.Itoa(len(f.docs)+1)
	f.docs = append(f.docs, d)
	return &api.WriteResponse{Result: memory.WriteResult{ID: d.ID}}, nil
}

func (f *fakeSvc) Retrieve(_ context.Context, req *api.RetrieveRequest) (*api.RetrieveResponse, error) {
	f.gotTenant = req.Filters.Tenant
	f.gotNS = req.Filters.Namespace
	f.gotTopK = req.TopK
	var chunks []memory.ScoredChunk
	for i := len(f.docs) - 1; i >= 0; i-- { // newest-first
		chunks = append(chunks, memory.ScoredChunk{Chunk: memory.Chunk{Text: string(f.docs[i].Content)}, Score: 1})
	}
	return &api.RetrieveResponse{Result: memory.RetrieveResult{Chunks: chunks, Total: int64(len(chunks))}}, nil
}

func TestEpisode_RecordAndRecall(t *testing.T) {
	ctx := context.Background()
	svc := &fakeSvc{}
	id := api.RequestIdentity{Tenant: "tenant-a", Namespace: "episodes/researcher"}
	rec := &Recorder{Svc: svc, Identity: id}
	rcl := &Recaller{Svc: svc, Identity: id}

	for i, ep := range []Episode{
		{RunName: "r1", AgentName: "researcher", InputSummary: "find X", Outcome: v1.PhaseCompleted, ToolSequence: []string{"search"}},
		{RunName: "r2", AgentName: "researcher", InputSummary: "find Y", Outcome: v1.PhaseFailed, TerminationReason: "budget:tokens"},
	} {
		gotID, err := rec.Record(ctx, ep)
		if err != nil || gotID == "" {
			t.Fatalf("record %d: %v id=%q", i, err, gotID)
		}
	}
	// Each record carries the episode metadata (kind=episode, agent, outcome).
	if svc.docs[0].Metadata["kind"] != "episode" || svc.docs[1].Metadata["outcome"] != "Failed" {
		t.Fatalf("episode metadata wrong: %+v", svc.docs)
	}

	eps, err := rcl.Recall(ctx, "find something", 5)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(eps) != 2 || eps[0].RunName != "r2" { // newest-first
		t.Fatalf("recall: want 2 newest-first, got %+v", eps)
	}
	// D1: tenant + namespace were forced from Identity onto the query filter.
	if svc.gotTenant != "tenant-a" || svc.gotNS != "episodes/researcher" {
		t.Fatalf("recall must scope by identity tenant/ns: %q/%q", svc.gotTenant, svc.gotNS)
	}
	if svc.gotTopK != 5 {
		t.Fatalf("topK not passed: %d", svc.gotTopK)
	}

	// FewShotContext renders recalled episodes (data, not instructions).
	if FewShotContext(eps) == "" {
		t.Fatal("few-shot context should be non-empty for recalled episodes")
	}
	if FewShotContext(nil) != "" {
		t.Fatal("few-shot context must be empty when there are no episodes")
	}
}

func TestEpisode_RecallDefaultsTopKAndSkipsGarbage(t *testing.T) {
	ctx := context.Background()
	svc := &fakeSvc{docs: []memory.Document{{Content: []byte("not json")}}}
	rcl := &Recaller{Svc: svc, Identity: api.RequestIdentity{Tenant: "t", Namespace: "n"}}
	eps, err := rcl.Recall(ctx, "q", 0) // 0 → default 3
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(eps) != 0 {
		t.Fatalf("garbage chunk must be skipped, got %+v", eps)
	}
	if svc.gotTopK != 3 {
		t.Fatalf("topK default: want 3, got %d", svc.gotTopK)
	}
}
