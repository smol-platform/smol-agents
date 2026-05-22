package worker_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stigen/smol-agents/pkg/memory/worker"
)

// ── FakeSummarizer ────────────────────────────────────────────────────────────

func TestFakeSummarizer_NoDocs(t *testing.T) {
	s := &worker.FakeSummarizer{}
	got, err := s.Summarize(context.Background(), "topic", nil)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !strings.Contains(got, "No documents") {
		t.Errorf("expected 'No documents' message, got %q", got)
	}
}

func TestFakeSummarizer_WithDocs(t *testing.T) {
	s := &worker.FakeSummarizer{}
	docs := []string{"doc about golang", "doc about channels", "doc about goroutines"}
	got, err := s.Summarize(context.Background(), "concurrency", docs)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !strings.Contains(got, "concurrency") {
		t.Errorf("expected query in summary, got %q", got)
	}
	if !strings.Contains(got, "3") {
		t.Errorf("expected doc count in summary, got %q", got)
	}
}

func TestFakeSummarizer_EmptyQuery(t *testing.T) {
	s := &worker.FakeSummarizer{}
	docs := []string{"some content"}
	got, err := s.Summarize(context.Background(), "", docs)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if got == "" {
		t.Error("expected non-empty summary for empty query")
	}
}

func TestFakeSummarizer_MoreThanThreeDocs(t *testing.T) {
	s := &worker.FakeSummarizer{}
	// Provide 7 docs; the fake takes up to 3 in the preview.
	docs := make([]string, 7)
	for i := range docs {
		docs[i] = "document"
	}
	got, err := s.Summarize(context.Background(), "test", docs)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	// Must not panic and must contain the count.
	if !strings.Contains(got, "7") {
		t.Errorf("expected doc count 7 in summary, got %q", got)
	}
}

// ── Determinism ───────────────────────────────────────────────────────────────

func TestFakeSummarizer_Deterministic(t *testing.T) {
	s := &worker.FakeSummarizer{}
	docs := []string{"first", "second"}
	a, _ := s.Summarize(context.Background(), "q", docs)
	b, _ := s.Summarize(context.Background(), "q", docs)
	if a != b {
		t.Errorf("FakeSummarizer must be deterministic: %q vs %q", a, b)
	}
}
