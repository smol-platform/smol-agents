package teammailbox

import (
	"context"
	"sync"
)

// MemMailbox is an in-memory Mailbox for tests and single-process dev: one FIFO
// queue per member inbox.
type MemMailbox struct {
	mu     sync.Mutex
	inbox  map[string][]Message
	closed bool
}

// NewMemMailbox returns an empty in-memory mailbox.
func NewMemMailbox() *MemMailbox {
	return &MemMailbox{inbox: map[string][]Message{}}
}

func (m *MemMailbox) Send(_ context.Context, msg Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inbox[msg.To] = append(m.inbox[msg.To], msg)
	return nil
}

func (m *MemMailbox) Receive(_ context.Context, self string, max int) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.inbox[self]
	if len(q) == 0 {
		return nil, nil
	}
	if max <= 0 || max > len(q) {
		max = len(q)
	}
	out := q[:max]
	m.inbox[self] = q[max:]
	return out, nil
}

func (m *MemMailbox) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}
