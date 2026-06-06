# Per-namespace NATS tenant ACLs (M2.20, D1)

> Status: **built (operator-rendered worker creds), not yet live-verified.** The
> permission model + creds minting are unit-tested; end-to-end enforcement needs a
> NATS server in JWT/account mode (a deployment change), so it is validated at
> bring-up, not in the kindnet dev ring. Resolves open question #1 (the ACL
> *shape*) with the approach chosen 2026-06-06: **operator-rendered worker creds.**

## Threat

The session turn bus is one shared JetStream stream (`AGENT_SESSIONS`). Without
per-tenant auth, a compromised session worker in namespace A could subscribe to
namespace B's turns (`agentsession.B.*.turns`) or forge B's results — a D1
(multi-tenant, untrusted) violation.

## Model: operator-rendered worker credentials

- The **gateway** (the multi-tenant front door) keeps a broad credential — it
  publishes turns for every namespace and reads results.
- Each **session worker** connects with a **namespace-scoped** credential the
  operator mints: publish/subscribe limited to its own `agentsession.<ns>.>`
  subjects + this namespace's durable consumers (`w_<ns>_*`). A worker physically
  cannot name another tenant's subjects.

Pieces (all in this repo):

| Piece | Where |
|---|---|
| `WorkerPermissions(ns)` — the ns-scoped pub/sub allow-lists | `pkg/sessionqueue/acl.go` |
| `MintWorkerCreds(accountSeed, ns)` — signs a user JWT with those perms → `.creds` | `pkg/sessionqueue/acl.go` |
| Worker uses the creds + connects stream-management-off | `cmd/agent/serve_session.go` (`--nats-creds`) + `sessionqueue.With{UserCredentials,WithoutStreamManagement}` |
| Operator mints a per-namespace creds Secret + mounts it on the worker | `agentsession_controller.go` (`ensureWorkerCreds` + `attachNATSCreds`), gated on `--nats-account-seed-file` |

## Deployment prerequisites (the live-validation gap)

1. **NATS in decentralized-auth mode** trusting the operator's account public key
   (account JWT in the server's resolver, or the account listed in the server
   config). Without this the signed user JWTs are not honored.
2. **Operator account seed**: mount the account signing seed (an `SA…` nkey seed)
   into the operator and pass `--nats-account-seed-file=<path>`. Absent → workers
   connect unauthenticated (today's behavior; no per-tenant ACL).
3. The **gateway** keeps its broad credential (it is not per-namespace-scoped).

When `--nats-account-seed-file` is unset (the kindnet dev ring), the feature is a
no-op and sessions behave exactly as before — so this is safe to ship dark and
enable per environment.

## What is proven vs. pending

- **Proven (unit tests):** the permission allow-lists are namespace-scoped (no
  subject grants cross-tenant access; the only unscoped allow is `_INBOX.>`, a
  random per-connection reply target), and `MintWorkerCreds` produces a parseable,
  account-signed user JWT carrying exactly those permissions.
- **Pending (deployment-gated):** the exact JetStream control-subject set
  (`$JS.API.CONSUMER.*`) must be validated against a JWT-auth NATS server — a
  too-strict entry breaks the worker (caught at bring-up) but **cannot leak across
  tenants**, because every entry is `w_<ns>_`-scoped. Validate on the L2 ring (or
  any JWT-mode NATS) before enabling in prod.
