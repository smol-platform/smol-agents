package sessionqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
)

const (
	defaultStream = "AGENT_SESSIONS"
	subjectPrefix = "agentsession"
	turnIDHeader  = "Smol-Turn-Id"
)

// NATSQueue is a JetStream-backed Queue. Turns for a session are published to
// agentsession.<key>.turns (a durable, file-backed stream with at-least-once
// delivery + replay); results to agentsession.<key>.result.<turnID>. A durable
// pull consumer per session is created lazily and reused.
type NATSQueue struct {
	nc     *nats.Conn
	js     nats.JetStreamContext
	stream string

	mu   sync.Mutex
	subs map[string]*nats.Subscription // sessionKey -> durable pull subscription
}

// NewNATSQueue connects to url and ensures the session stream exists.
func NewNATSQueue(url string) (*NATSQueue, error) {
	nc, err := nats.Connect(url, nats.Name("smol-agents-sessionqueue"), nats.MaxReconnects(-1))
	if err != nil {
		return nil, fmt.Errorf("sessionqueue: connect %q: %w", url, err)
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("sessionqueue: jetstream: %w", err)
	}
	q := &NATSQueue{nc: nc, js: js, stream: defaultStream, subs: map[string]*nats.Subscription{}}
	_, err = js.AddStream(&nats.StreamConfig{
		Name:      q.stream,
		Subjects:  []string{subjectPrefix + ".>"},
		Storage:   nats.FileStorage,
		Retention: nats.LimitsPolicy,
		MaxAge:    24 * time.Hour,
	})
	if err != nil && !errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
		nc.Close()
		return nil, fmt.Errorf("sessionqueue: ensure stream: %w", err)
	}
	return q, nil
}

func turnsSubject(key string) string      { return subjectPrefix + "." + key + ".turns" }
func resultSubject(key, id string) string { return subjectPrefix + "." + key + ".result." + id }
func durableName(key string) string       { return "w_" + strings.ReplaceAll(key, ".", "_") }

func (q *NATSQueue) Publish(_ context.Context, key string, body []byte) (string, error) {
	id := nuid.Next()
	msg := nats.NewMsg(turnsSubject(key))
	msg.Data = body
	msg.Header.Set(turnIDHeader, id)
	if _, err := q.js.PublishMsg(msg); err != nil {
		return "", fmt.Errorf("sessionqueue: publish turn: %w", err)
	}
	return id, nil
}

// pullSub returns the durable pull subscription for key, creating it once.
func (q *NATSQueue) pullSub(key string) (*nats.Subscription, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if s := q.subs[key]; s != nil {
		return s, nil
	}
	s, err := q.js.PullSubscribe(turnsSubject(key), durableName(key))
	if err != nil {
		return nil, fmt.Errorf("sessionqueue: subscribe: %w", err)
	}
	q.subs[key] = s
	return s, nil
}

func (q *NATSQueue) Consume(_ context.Context, key string, max int) ([]Turn, error) {
	sub, err := q.pullSub(key)
	if err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 16
	}
	msgs, err := sub.Fetch(max, nats.MaxWait(500*time.Millisecond))
	if err != nil && !errors.Is(err, nats.ErrTimeout) {
		return nil, fmt.Errorf("sessionqueue: fetch: %w", err)
	}
	turns := make([]Turn, 0, len(msgs))
	for _, m := range msgs {
		turns = append(turns, Turn{
			ID:   m.Header.Get(turnIDHeader),
			Body: m.Data,
			Ack:  func() error { return m.Ack() }, // m.Ack is variadic; adapt to func() error
		})
	}
	return turns, nil
}

func (q *NATSQueue) PublishResult(_ context.Context, key, turnID string, body []byte) error {
	if _, err := q.js.Publish(resultSubject(key, turnID), body); err != nil {
		return fmt.Errorf("sessionqueue: publish result: %w", err)
	}
	return nil
}

func (q *NATSQueue) FetchResult(ctx context.Context, key, turnID string, timeout time.Duration) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The result is a single retained message on a unique per-turn subject.
	sub, err := q.js.SubscribeSync(resultSubject(key, turnID), nats.DeliverAll())
	if err != nil {
		return nil, fmt.Errorf("sessionqueue: subscribe result: %w", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck
	msg, err := sub.NextMsgWithContext(cctx)
	if err != nil {
		return nil, ErrNoResult
	}
	return msg.Data, nil
}

func (q *NATSQueue) Close() error {
	q.nc.Close()
	return nil
}
