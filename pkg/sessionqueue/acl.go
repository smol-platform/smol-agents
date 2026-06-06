package sessionqueue

import (
	"fmt"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// Per-namespace NATS tenant isolation (M2.20, D1): a session worker connects with
// an operator-minted user credential whose publish/subscribe permissions are
// scoped to its OWN namespace's subjects, so a compromised worker cannot read
// another tenant's turns or forge another tenant's results. The gateway (the
// multi-tenant front door) keeps a broad credential; only the per-namespace
// WORKERS are constrained.
//
// SECURITY BOUNDARY (provable, tested): every data subject below is scoped to the
// namespace — turns it may subscribe to and results it may publish all carry the
// <ns> token, and the JetStream consumer/ack subjects are scoped to this
// namespace's durable consumers (w_<ns>_*). No permission grants cross-namespace
// access; _INBOX.> is the only unscoped allow and is safe (inboxes are random,
// per-connection reply targets, not a tenant channel).
//
// The exact JetStream control-subject set ($JS.API.CONSUMER.*) is best-effort and
// must be validated against a JWT-auth NATS server in deployment — a too-strict
// entry breaks the worker (caught at bring-up), but cannot leak across tenants
// because every entry is w_<ns>_-scoped. The worker connects stream-management-off
// (it never creates the shared stream; the gateway/operator owns AGENT_SESSIONS).

// WorkerPermissions returns the publish/subscribe subject allow-lists for a
// session worker in namespace ns. Pure + deterministic.
func WorkerPermissions(ns string) (pub, sub []string) {
	data := subjectPrefix + "." + ns + "."  // agentsession.<ns>.
	cons := "AGENT_SESSIONS.w_" + ns + "_*" // this namespace's durable consumers
	pub = []string{
		data + "*.result.>",                       // publish turn results
		"$JS.API.CONSUMER.CREATE." + cons,         // create/bind its pull consumers
		"$JS.API.CONSUMER.CREATE." + cons + ".>",  // (subject variants with extra tokens)
		"$JS.API.CONSUMER.DURABLE.CREATE." + cons, // durable consumer create (older NATS)
		"$JS.API.CONSUMER.INFO." + cons,           // consumer info
		"$JS.API.CONSUMER.MSG.NEXT." + cons,       // pull fetch
		"$JS.API.CONSUMER.DELETE." + cons,         // teardown
		"$JS.ACK.AGENT_SESSIONS.w_" + ns + "_*.>", // ack delivered turns
	}
	sub = []string{
		data + "*.turns", // its own turn subjects
		"_INBOX.>",       // request/reply + pull-fetch delivery inboxes (random per connection)
	}
	return pub, sub
}

// MintWorkerCreds mints a NATS user credential file (the JWT + the user seed, in
// the standard decorated .creds format) for a session worker in namespace ns,
// signed by the operator's account key (accountSeed, an "SA…" account nkey seed).
// The permissions from WorkerPermissions(ns) are baked into the signed JWT, so a
// JWT-auth NATS server enforces them without per-namespace server config. The
// returned bytes are written to a Secret the worker mounts and passes to
// nats.UserCredentials.
func MintWorkerCreds(accountSeed []byte, ns string) ([]byte, error) {
	akp, err := nkeys.FromSeed(accountSeed)
	if err != nil {
		return nil, fmt.Errorf("sessionqueue: account seed: %w", err)
	}
	apub, err := akp.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("sessionqueue: account pubkey: %w", err)
	}

	ukp, err := nkeys.CreateUser()
	if err != nil {
		return nil, fmt.Errorf("sessionqueue: create user key: %w", err)
	}
	upub, err := ukp.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("sessionqueue: user pubkey: %w", err)
	}
	useed, err := ukp.Seed()
	if err != nil {
		return nil, fmt.Errorf("sessionqueue: user seed: %w", err)
	}

	uc := jwt.NewUserClaims(upub)
	uc.Name = "agentsession-worker-" + ns
	uc.IssuerAccount = apub
	pub, sub := WorkerPermissions(ns)
	uc.Permissions.Pub.Allow = pub
	uc.Permissions.Sub.Allow = sub
	ujwt, err := uc.Encode(akp)
	if err != nil {
		return nil, fmt.Errorf("sessionqueue: encode user jwt: %w", err)
	}

	creds, err := jwt.FormatUserConfig(ujwt, useed)
	if err != nil {
		return nil, fmt.Errorf("sessionqueue: format creds: %w", err)
	}
	return creds, nil
}
