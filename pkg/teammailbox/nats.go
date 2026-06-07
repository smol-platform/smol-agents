package teammailbox

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/nats-io/nats.go"
)

// NATSMailbox is a core-NATS Mailbox: the member subscribes to its own inbox
// subject (buffering arrivals) and publishes to a teammate's inbox by name. The
// per-member credential (MemberMailboxPermissions) makes that subscribe scope —
// only its own leaf — the enforced isolation boundary. Core NATS is
// fire-and-forget: a message published while the recipient is not subscribed is
// not retained (members are resident session workers; a restart-window loss is
// the documented tradeoff for clean subject-ACL isolation — a durable variant
// would need per-member JetStream streams, whose filter subjects NATS does not
// ACL).
type NATSMailbox struct {
	nc             *nats.Conn
	sub            *nats.Subscription
	ns, team, self string

	mu  sync.Mutex
	buf []Message
}

// NATSMailboxOption tunes the connection (e.g. per-member credentials).
type NATSMailboxOption func(*[]nats.Option)

// WithCredentials authenticates with a NATS .creds file (the operator-minted,
// per-member credential — MintMemberCreds).
func WithCredentials(path string) NATSMailboxOption {
	return func(o *[]nats.Option) {
		if path != "" {
			*o = append(*o, nats.UserCredentials(path))
		}
	}
}

// NewNATSMailbox connects to url and subscribes self's inbox.
func NewNATSMailbox(url, namespace, team, self string, opts ...NATSMailboxOption) (*NATSMailbox, error) {
	connOpts := []nats.Option{nats.Name("smol-agents-teammailbox"), nats.MaxReconnects(-1)}
	for _, f := range opts {
		f(&connOpts)
	}
	nc, err := nats.Connect(url, connOpts...)
	if err != nil {
		return nil, fmt.Errorf("teammailbox: connect %q: %w", url, err)
	}
	mb := &NATSMailbox{nc: nc, ns: namespace, team: team, self: self}
	sub, err := nc.Subscribe(InboxSubject(namespace, team, self), func(m *nats.Msg) {
		var msg Message
		if err := json.Unmarshal(m.Data, &msg); err != nil {
			return // drop a malformed frame rather than wedging the inbox
		}
		mb.mu.Lock()
		mb.buf = append(mb.buf, msg)
		mb.mu.Unlock()
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("teammailbox: subscribe inbox: %w", err)
	}
	mb.sub = sub
	return mb, nil
}

func (mb *NATSMailbox) Send(_ context.Context, msg Message) error {
	if msg.From == "" {
		msg.From = mb.self
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := mb.nc.Publish(InboxSubject(mb.ns, mb.team, msg.To), data); err != nil {
		return fmt.Errorf("teammailbox: send to %q: %w", msg.To, err)
	}
	return mb.nc.Flush()
}

func (mb *NATSMailbox) Receive(_ context.Context, _ string, max int) ([]Message, error) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if len(mb.buf) == 0 {
		return nil, nil
	}
	if max <= 0 || max > len(mb.buf) {
		max = len(mb.buf)
	}
	out := make([]Message, max)
	copy(out, mb.buf[:max])
	mb.buf = mb.buf[max:]
	return out, nil
}

func (mb *NATSMailbox) Close() error {
	if mb.sub != nil {
		_ = mb.sub.Unsubscribe()
	}
	if mb.nc != nil {
		mb.nc.Close()
	}
	return nil
}
