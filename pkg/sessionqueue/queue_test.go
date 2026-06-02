package sessionqueue

import (
	"context"
	"testing"
	"time"
)

func TestMemQueue_RoundTrip(t *testing.T) {
	q := NewMemQueue()
	ctx := context.Background()
	key := SessionKey("tenant-a", "s1")

	id1, _ := q.Publish(ctx, key, []byte(`{"prompt":"one"}`))
	id2, _ := q.Publish(ctx, key, []byte(`{"prompt":"two"}`))
	if id1 == id2 {
		t.Fatal("turn ids must be unique")
	}

	turns, err := q.Consume(ctx, key, 10)
	if err != nil || len(turns) != 2 {
		t.Fatalf("Consume = (%d turns, %v), want 2", len(turns), err)
	}
	if string(turns[0].Body) != `{"prompt":"one"}` || turns[0].ID != id1 {
		t.Errorf("FIFO/id mismatch: %+v", turns[0])
	}
	// Drained.
	if more, _ := q.Consume(ctx, key, 10); len(more) != 0 {
		t.Errorf("Consume after drain returned %d", len(more))
	}

	// Result round-trip + isolation by turn id.
	if _, err := q.FetchResult(ctx, key, id1, 10*time.Millisecond); err != ErrNoResult {
		t.Errorf("FetchResult before publish: want ErrNoResult, got %v", err)
	}
	_ = q.PublishResult(ctx, key, id1, []byte(`"done-one"`))
	got, err := q.FetchResult(ctx, key, id1, time.Second)
	if err != nil || string(got) != `"done-one"` {
		t.Fatalf("FetchResult = (%q, %v), want done-one", got, err)
	}
}

func TestSessionKey(t *testing.T) {
	if got := SessionKey("tenant-a", "s1"); got != "tenant-a.s1" {
		t.Errorf("SessionKey = %q, want tenant-a.s1", got)
	}
}
