package teammailbox

import (
	"context"
	"fmt"
	"sync"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// The team message bus (multi-agent orchestration P5): the pub/sub counterpart to
// the per-member mailbox. Members publish events to topics
// (agentteam.<ns>.<team>.bus.<topic>) and subscribe to topics of interest, so a
// new member joins an emergent workflow by subscribing without rewiring the
// senders. Unlike the mailbox (a member reads only its own inbox), the bus is a
// shared team channel: a member may publish AND subscribe across the team's bus
// subtree — but every subject still carries the <ns>/<team> tokens, so the bus is
// confined to one team in one tenant.

// BusSubject is a team topic subject: agentteam.<ns>.<team>.bus.<topic>.
func BusSubject(namespace, team, topic string) string {
	return subjectPrefix + "." + namespace + "." + team + ".bus." + topic
}

// busStem is agentteam.<ns>.<team>.bus. — the team's whole bus subtree.
func busStem(namespace, team string) string {
	return subjectPrefix + "." + namespace + "." + team + ".bus."
}

// BusEvent is one published event on a topic.
type BusEvent struct {
	Topic string `json:"topic"`
	From  string `json:"from"`
	Body  string `json:"body"`
}

// Bus is a team's topic pub/sub transport.
type Bus interface {
	// Publish broadcasts an event to a topic.
	Publish(ctx context.Context, ev BusEvent) error
	// Subscribe registers interest in a topic (idempotent).
	Subscribe(ctx context.Context, topic string) error
	// Receive drains up to max events across the subscribed topics (promptly).
	Receive(ctx context.Context, max int) ([]BusEvent, error)
	// Close releases transport resources.
	Close() error
}

// MemBus is an in-memory Bus for tests and single-process dev.
type MemBus struct {
	mu     sync.Mutex
	subbed map[string]bool
	buf    []BusEvent
}

// NewMemBus returns an empty in-memory bus.
func NewMemBus() *MemBus { return &MemBus{subbed: map[string]bool{}} }

func (m *MemBus) Publish(_ context.Context, ev BusEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Deliver to this process's subscriptions (a single-process model; the NATS
	// impl fans out across pods).
	if m.subbed[ev.Topic] {
		m.buf = append(m.buf, ev)
	}
	return nil
}

func (m *MemBus) Subscribe(_ context.Context, topic string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subbed[topic] = true
	return nil
}

func (m *MemBus) Receive(_ context.Context, max int) ([]BusEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.buf) == 0 {
		return nil, nil
	}
	if max <= 0 || max > len(m.buf) {
		max = len(m.buf)
	}
	out := make([]BusEvent, max)
	copy(out, m.buf[:max])
	m.buf = m.buf[max:]
	return out, nil
}

func (m *MemBus) Close() error { return nil }

// MemberBusPermissions returns the publish/subscribe allow-lists for a team
// member's bus connection: both scoped to the team's bus subtree
// (agentteam.<ns>.<team>.bus.*). A member can pub + sub any team topic (emergent
// pub/sub) but the <ns>/<team> tokens confine it to this team in this tenant — it
// cannot reach another team's bus or another tenant.
func MemberBusPermissions(namespace, team string) (pub, sub []string) {
	stem := busStem(namespace, team)
	pub = []string{stem + "*"}
	sub = []string{stem + "*", "_INBOX.>"}
	return pub, sub
}

// MintMemberBusCreds mints a NATS user credential scoped to the team's bus
// subtree (MemberBusPermissions), signed by the operator's account key. Mirrors
// MintMemberCreds; a member that uses both the mailbox and the bus is granted the
// union (pub mbox.* + bus.*, sub own-inbox + bus.*) — compose the allow-lists.
func MintMemberBusCreds(accountSeed []byte, namespace, team, member string) ([]byte, error) {
	akp, err := nkeys.FromSeed(accountSeed)
	if err != nil {
		return nil, fmt.Errorf("teammailbox: account seed: %w", err)
	}
	apub, err := akp.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("teammailbox: account pubkey: %w", err)
	}
	ukp, err := nkeys.CreateUser()
	if err != nil {
		return nil, fmt.Errorf("teammailbox: create user key: %w", err)
	}
	upub, err := ukp.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("teammailbox: user pubkey: %w", err)
	}
	useed, err := ukp.Seed()
	if err != nil {
		return nil, fmt.Errorf("teammailbox: user seed: %w", err)
	}
	uc := jwt.NewUserClaims(upub)
	uc.Name = "agentteam-bus-" + namespace + "-" + team + "-" + member
	uc.IssuerAccount = apub
	pub, sub := MemberBusPermissions(namespace, team)
	uc.Permissions.Pub.Allow = pub
	uc.Permissions.Sub.Allow = sub
	ujwt, err := uc.Encode(akp)
	if err != nil {
		return nil, fmt.Errorf("teammailbox: encode bus jwt: %w", err)
	}
	creds, err := jwt.FormatUserConfig(ujwt, useed)
	if err != nil {
		return nil, fmt.Errorf("teammailbox: format bus creds: %w", err)
	}
	return creds, nil
}
