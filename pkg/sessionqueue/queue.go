// Package sessionqueue is the durable turn transport between the gateway
// (producers of turns / incoming requests) and a long-running AgentSession
// worker (the consumer). The gateway Publishes a turn and optionally waits for
// its Result; the worker Consumes turns and PublishResults. Two implementations:
// NATSQueue (NATS JetStream — at-least-once delivery + replay, the Phase 4
// transport) and MemQueue (in-memory — tests + single-process dev).
package sessionqueue

import (
	"context"
	"errors"
	"time"
)

// ErrNoResult is returned by FetchResult when no result lands within the wait.
var ErrNoResult = errors.New("sessionqueue: result not available")

// Turn is one pending turn delivered to a session worker. Ack marks it durably
// processed; an unacked turn is redelivered (at-least-once) by NATS.
type Turn struct {
	ID   string
	Body []byte
	Ack  func() error
}

// Queue is the durable turn transport. sessionKey scopes turns to one session
// (build it with SessionKey).
type Queue interface {
	// Publish enqueues a turn for sessionKey and returns its id.
	Publish(ctx context.Context, sessionKey string, body []byte) (id string, err error)

	// Consume returns up to max ready turns for sessionKey, promptly (it does not
	// block for a full batch). The caller Acks each once processed.
	Consume(ctx context.Context, sessionKey string, max int) ([]Turn, error)

	// PublishResult records a processed turn's result.
	PublishResult(ctx context.Context, sessionKey, turnID string, body []byte) error

	// FetchResult returns a turn's result, waiting up to timeout for it to land
	// (synchronous gateway calls). Returns ErrNoResult on timeout.
	FetchResult(ctx context.Context, sessionKey, turnID string, timeout time.Duration) ([]byte, error)

	// Close releases transport resources.
	Close() error
}

// SessionKey builds the transport key for a namespaced session. k8s namespaces
// and names are DNS labels (no dots), so a dot-joined key is a valid two-token
// NATS subject fragment.
func SessionKey(namespace, name string) string { return namespace + "." + name }
