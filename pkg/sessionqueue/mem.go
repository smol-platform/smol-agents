package sessionqueue

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemQueue is an in-process Queue for tests + single-binary dev. Turns are FIFO
// per session; Consume pops them (Ack is a no-op). Results are retained until
// fetched. Safe for concurrent use.
type MemQueue struct {
	mu      sync.Mutex
	pending map[string][]Turn // sessionKey -> FIFO pending turns
	results map[string][]byte // sessionKey + "/" + turnID -> result
	seq     int
}

// NewMemQueue returns an empty in-memory queue.
func NewMemQueue() *MemQueue {
	return &MemQueue{pending: map[string][]Turn{}, results: map[string][]byte{}}
}

func (q *MemQueue) Publish(_ context.Context, key string, body []byte) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	id := fmt.Sprintf("t-%d", q.seq)
	q.pending[key] = append(q.pending[key], Turn{
		ID:   id,
		Body: append([]byte(nil), body...),
		Ack:  func() error { return nil },
	})
	return id, nil
}

func (q *MemQueue) Consume(_ context.Context, key string, max int) ([]Turn, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	p := q.pending[key]
	if len(p) == 0 {
		return nil, nil
	}
	if max <= 0 || max > len(p) {
		max = len(p)
	}
	out := append([]Turn(nil), p[:max]...)
	q.pending[key] = append([]Turn(nil), p[max:]...)
	return out, nil
}

func (q *MemQueue) PublishResult(_ context.Context, key, turnID string, body []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.results[key+"/"+turnID] = append([]byte(nil), body...)
	return nil
}

func (q *MemQueue) FetchResult(ctx context.Context, key, turnID string, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		q.mu.Lock()
		r, ok := q.results[key+"/"+turnID]
		q.mu.Unlock()
		if ok {
			return r, nil
		}
		if time.Now().After(deadline) {
			return nil, ErrNoResult
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (q *MemQueue) Close() error { return nil }
