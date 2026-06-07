// Package teammailbox is the peer-to-peer mailbox for an AgentTeam (multi-agent
// orchestration, P2). Any member can message any other member BY NAME (the
// SendMessage analog); each member has a private inbox subject
// agentteam.<ns>.<team>.mbox.<member>. The operator mints each member a NATS
// credential scoped to PUBLISH any teammate's inbox within its team but
// SUBSCRIBE only its own inbox (MemberMailboxPermissions) — so a member cannot
// read another member's mail, reach another team, or reach another tenant. That
// structural isolation is the governance differentiator over a local agent-team.
//
// Two implementations: NATSMailbox (JetStream, durable until consumed) and
// MemMailbox (in-memory, tests + dev).
package teammailbox

import "context"

// subjectPrefix roots every team subject; the per-member inbox is
// agentteam.<ns>.<team>.mbox.<member>.
const subjectPrefix = "agentteam"

// Message is one note from one member to another.
type Message struct {
	From string `json:"from"`
	To   string `json:"to"`
	Body string `json:"body"`
}

// Mailbox is a team's peer messaging transport.
type Mailbox interface {
	// Send delivers msg to msg.To's inbox.
	Send(ctx context.Context, msg Message) error
	// Receive drains up to max messages from self's inbox (promptly; it does not
	// block for a full batch).
	Receive(ctx context.Context, self string, max int) ([]Message, error)
	// Close releases transport resources.
	Close() error
}

// InboxSubject is a member's private inbox subject within a team.
func InboxSubject(namespace, team, member string) string {
	return subjectPrefix + "." + namespace + "." + team + ".mbox." + member
}

// teamSubjectStem is agentteam.<ns>.<team>.mbox. — the shared prefix a member may
// publish to (any teammate) but only subscribe to its own leaf.
func teamSubjectStem(namespace, team string) string {
	return subjectPrefix + "." + namespace + "." + team + ".mbox."
}
