package teammailbox

import (
	"fmt"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// Per-member mailbox isolation (orchestration P2, D1): the operator mints each
// team member a NATS user credential whose permissions are baked into a signed
// JWT, so a JWT-auth NATS server enforces them without per-member server config.
//
// SECURITY BOUNDARY (provable, tested): a member may PUBLISH to any inbox within
// its own team (agentteam.<ns>.<team>.mbox.*) — that is the point, peer-to-peer
// send by name — but may SUBSCRIBE only to its OWN inbox leaf
// (agentteam.<ns>.<team>.mbox.<member>). It therefore cannot read another
// member's mail, and because every subject carries the <ns> and <team> tokens it
// cannot reach another team or another tenant. _INBOX.> is the only unscoped
// allow and is safe (random per-connection reply targets, not a team channel).

// MemberMailboxPermissions returns the publish/subscribe subject allow-lists for
// a team member's mailbox connection. Pure + deterministic.
func MemberMailboxPermissions(namespace, team, member string) (pub, sub []string) {
	stem := teamSubjectStem(namespace, team) // agentteam.<ns>.<team>.mbox.
	pub = []string{
		stem + "*", // send to any teammate's inbox IN THIS TEAM
	}
	sub = []string{
		stem + member, // read ONLY this member's inbox
		"_INBOX.>",    // request/reply inboxes (random per connection)
	}
	return pub, sub
}

// MintMemberCreds mints a NATS user credential file (decorated .creds: JWT + user
// seed) for one team member, signed by the operator's account key (accountSeed,
// an "SA…" account nkey seed). The MemberMailboxPermissions are baked into the
// signed JWT. The returned bytes go into a Secret the member's pod mounts and
// passes to nats.UserCredentials.
func MintMemberCreds(accountSeed []byte, namespace, team, member string) ([]byte, error) {
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
	uc.Name = "agentteam-member-" + namespace + "-" + team + "-" + member
	uc.IssuerAccount = apub
	pub, sub := MemberMailboxPermissions(namespace, team, member)
	uc.Permissions.Pub.Allow = pub
	uc.Permissions.Sub.Allow = sub
	ujwt, err := uc.Encode(akp)
	if err != nil {
		return nil, fmt.Errorf("teammailbox: encode user jwt: %w", err)
	}
	creds, err := jwt.FormatUserConfig(ujwt, useed)
	if err != nil {
		return nil, fmt.Errorf("teammailbox: format creds: %w", err)
	}
	return creds, nil
}
