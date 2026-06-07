package teammailbox

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/nats-io/nats.go"
)

// NATSBus is a core-NATS Bus: the member subscribes to the team topics it cares
// about (buffering arrivals) and publishes events to a topic. The per-member bus
// credential (MemberBusPermissions) confines pub/sub to the team's bus subtree.
// Core NATS is fire-and-forget (a missed event is not retained) — adequate for an
// emergent-workflow signal channel; a durable variant would need JetStream.
type NATSBus struct {
	nc       *nats.Conn
	ns, team string

	mu   sync.Mutex
	subs map[string]*nats.Subscription
	buf  []BusEvent
}

// NewNATSBus connects to url for a team's bus.
func NewNATSBus(url, namespace, team string, opts ...NATSMailboxOption) (*NATSBus, error) {
	connOpts := []nats.Option{nats.Name("smol-agents-teambus"), nats.MaxReconnects(-1)}
	for _, f := range opts {
		f(&connOpts)
	}
	nc, err := nats.Connect(url, connOpts...)
	if err != nil {
		return nil, fmt.Errorf("teambus: connect %q: %w", url, err)
	}
	return &NATSBus{nc: nc, ns: namespace, team: team, subs: map[string]*nats.Subscription{}}, nil
}

func (b *NATSBus) Publish(_ context.Context, ev BusEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if err := b.nc.Publish(BusSubject(b.ns, b.team, ev.Topic), data); err != nil {
		return fmt.Errorf("teambus: publish %q: %w", ev.Topic, err)
	}
	return b.nc.Flush()
}

func (b *NATSBus) Subscribe(_ context.Context, topic string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[topic] != nil {
		return nil
	}
	sub, err := b.nc.Subscribe(BusSubject(b.ns, b.team, topic), func(m *nats.Msg) {
		var ev BusEvent
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			return
		}
		b.mu.Lock()
		b.buf = append(b.buf, ev)
		b.mu.Unlock()
	})
	if err != nil {
		return fmt.Errorf("teambus: subscribe %q: %w", topic, err)
	}
	b.subs[topic] = sub
	return nil
}

func (b *NATSBus) Receive(_ context.Context, max int) ([]BusEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) == 0 {
		return nil, nil
	}
	if max <= 0 || max > len(b.buf) {
		max = len(b.buf)
	}
	out := make([]BusEvent, max)
	copy(out, b.buf[:max])
	b.buf = b.buf[max:]
	return out, nil
}

func (b *NATSBus) Close() error {
	b.mu.Lock()
	for _, s := range b.subs {
		_ = s.Unsubscribe()
	}
	b.mu.Unlock()
	if b.nc != nil {
		b.nc.Close()
	}
	return nil
}
