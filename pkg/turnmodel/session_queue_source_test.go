package turnmodel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/smol-platform/smol-agents/pkg/sessionqueue"
)

// TestSessionWorker_QueueTransport drives the worker over a Queue (the NATS
// path, exercised here with the in-memory queue): a published turn is consumed,
// run, checkpointed, and its result published back — fetchable by turn id.
func TestSessionWorker_QueueTransport(t *testing.T) {
	q := sessionqueue.NewMemQueue()
	key := sessionqueue.SessionKey("t", "s1")
	w := newTestWorker(t.TempDir())
	w.Source = &QueueSource{Queue: q, SessionKey: key, Max: 10}
	w.Sink = &QueueSink{Queue: q, SessionKey: key}

	ctx := context.Background()
	id, err := q.Publish(ctx, key, []byte(`{"input":{"prompt":"hi"}}`))
	if err != nil {
		t.Fatal(err)
	}

	state := &SessionState{}
	n, err := w.processTurns(ctx, state)
	if err != nil || n != 1 || len(state.Turns) != 1 {
		t.Fatalf("processTurns over queue = (%d turns handled, %v), state turns=%d", n, err, len(state.Turns))
	}

	res, err := q.FetchResult(ctx, key, id, time.Second)
	if err != nil {
		t.Fatalf("FetchResult: %v", err)
	}
	if !strings.Contains(string(res), `"prompt":"hi"`) {
		t.Errorf("result should echo the input, got: %s", res)
	}
	// Queue drained (turn acked).
	if more, _ := q.Consume(ctx, key, 10); len(more) != 0 {
		t.Errorf("turn not acked: %d still pending", len(more))
	}
}
