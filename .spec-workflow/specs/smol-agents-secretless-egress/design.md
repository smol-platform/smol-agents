# Design Document — smol-agents-secretless-egress

## Overview

When an agent egresses to an external provider (GitHub, GitLab) or a backend
that needs a provider-native credential, the eBPF-redirected `agentnet` sidecar
**asks the secret broker to mint a short-lived credential**, authorized by the
agent's **SPIFFE identity** + the **verified TraT** (`sub`/`scope`/`rctx`), and
**injects** it into the outbound request. The **agent never receives the
credential** (it flows broker → proxy → upstream, not broker → agent), and the
eBPF allow-list ensures the credential can only leave toward the authorized host.

This is "token exchange all the way down": `user token → TraT (internal intent)
→ provider credential (external, short-lived, scoped)`. The result is a
secretless agent — no long-lived PAT in its env, no ability to read or
exfiltrate the token it's using.

## Steering Document Alignment

### Technical Standards (`.spec-workflow/steering/tech.md`)
- go-spiffe/v2 for the attested broker channel + TraT verification (TTS JWKS).
- Reuses `pkg/secrets` (kloak-style broker, SPIFFE-attested, leases ≤ 15m) and
  the `agentnet` egress proxy. No new kernel-side code.
- Defense in depth: SPIFFE attestation **and** a signed TraT **and** eBPF
  allow-list all gate a single injected credential.

### Project Structure (`.spec-workflow/steering/structure.md`)
- New broker backends under `pkg/secrets/backend_*.go` (interface already
  pluggable). New proxy injection mode in `pkg/agentnet/proxy`. DAG preserved:
  proxy → secrets(client) + trat; broker → backends.

## Code Reuse Analysis

### Existing Components to Leverage
- **`pkg/secrets` broker** — `SO_PEERCRED → SPIFFE` attestation, `Policy`,
  pluggable `Backend`, short-lived `Lease`. We add a `DynamicBackend` mint path
  + TraT verification; the attestation/lease/policy spine is reused.
- **`pkg/secrets/client`** — the proxy uses it to call the broker over the UDS.
- **`pkg/agentnet/proxy/http.go` `HTTPProxy.Director`** — the injection seam
  (already sets `Authorization` for JWT-SVID). Adds a broker-minted-credential
  injection mode.
- **`smol-agents-trat-egress` (`pkg/trat`)** — supplies the TraT whose
  `sub`/`scope`/`rctx` is the authorization context for the mint.
- **eBPF `egress_redirect` + allow-list** — capture + exfil-prevention, unchanged.

### Integration Points
- **GitHub App API** — `POST /app/installations/{id}/access_tokens` (App-signed
  JWT) → repo/permission-scoped installation token (~1h).
- **Tokenetes TTS JWKS** — to verify the TraT in the broker.
- **SPIRE** — attestation + the JWT-SVID behind the TraT.

## Architecture

```mermaid
sequenceDiagram
    participant A as agent
    participant BPF as eBPF egress
    participant PX as agentnet sidecar (HTTPProxy)
    participant TR as pkg/trat
    participant BK as secret broker
    participant GH as GitHub App backend
    participant UP as api.github.com

    A->>BPF: egress (allow-listed dest)
    BPF->>PX: redirect to sidecar
    PX->>TR: TraT(scope=resource intent)
    TR-->>PX: TraT (sub=agent, scope, rctx)
    PX->>BK: Mint(name="github", TraT) over UDS (SO_PEERCRED)
    BK->>BK: attest caller + verify TraT (TTS JWKS) + policy
    BK->>GH: mint installation token scoped by scope/rctx
    GH-->>BK: token (~1h, repo-scoped)
    BK-->>PX: Lease(value=token, exp)
    PX->>UP: request + Authorization: Bearer <token>
    Note over A,UP: agent never sees the token; eBPF<br/>blocks egress to any non-allow-listed host
```

### Modular Design Principles
- **`DynamicBackend`**: one method, `Mint(req) → Lease`; each provider is a
  focused file (`backend_github.go`, later `backend_vault.go`).
- **Broker mint path**: attest → verify TraT → policy → backend; pure decision
  separated from the GitHub HTTP I/O.
- **Proxy injection**: a single `injectBrokeredCredential(req, lease, cfg)`.

## Components and Interfaces

### 1. Broker dynamic minting (`pkg/secrets`)
```go
type CredentialRequest struct {
    Name           string                 // "github"
    Principal      spiffeid.ID            // SO_PEERCRED-attested caller (the sidecar/pod)
    Subject        string                 // TraT sub (verified) — the agent
    Scope          string                 // TraT scope (transaction intent)
    RequestContext map[string]any         // TraT rctx (on-behalf-of user, repo, ...)
}

type DynamicBackend interface {
    Mint(ctx context.Context, req CredentialRequest) (Lease, error)
    Close() error
}
```
- The broker `Server` gains a `Mint` RPC: read `SO_PEERCRED` → SPIFFE; **verify
  the TraT** (signature via TTS JWKS, `exp`, `aud`); build `CredentialRequest`
  from the verified claims; check `Policy`; call the `DynamicBackend`; return a
  `Lease` capped at `MaxLeaseTTL` (or the provider expiry if shorter).
- Existing static `Backend.Fetch` is unchanged.

### 2. GitHub App backend (`pkg/secrets/backend_github.go`)
- **Holds:** App ID + private key (PEM) — sourced from the broker/a mounted
  secret; **never** handed to the agent.
- **Mint:** sign a short JWT (RS256, `iss`=App ID, `exp`≈10m) → call
  `POST /app/installations/{installationID}/access_tokens` with a body scoping
  `repositories` + `permissions` derived from `req.Scope`/`req.RequestContext`
  per policy → return the installation token + `expires_at` as a `Lease`.

### 3. Proxy injection mode (`pkg/agentnet/proxy`)
- For a resource with `credential`, the `Director`:
  1. `tratClient.Token(scope)` → TraT.
  2. `brokerClient.Mint(name, trat)` over the UDS → `Lease`.
  3. `req.Header.Set(cfg.Header, cfg.Scheme+" "+string(lease.Value))`.
  - Cache the lease in-proc until near `exp`; refresh transparently.
  - On any failure: stamp an error header → `jwtTransport` returns 503
    (fail-closed, R-SEGR-SEC-1). The value is never logged.

### 4. AgentNetwork CRD (`pkg/agentmodel/v1`)
- `ResourceTarget.Credential *CredentialInjection`; broker policy entries.

## Data Models

### CRD additions
```go
type CredentialInjection struct {
    Name   string `json:"name"`             // broker credential/policy key, e.g. "github"
    Header string `json:"header,omitempty"` // default "Authorization"
    Scheme string `json:"scheme,omitempty"` // default "Bearer"
}
```

### Broker policy (illustrative)
```yaml
# principal + TraT scope -> credential + backend + scoping
- principal: spiffe://smol-agents.ai/ns/tenant-a/sa/alice-agent
  scope: github:repo:read
  credential: github
  backend: github-app
  scope:
    repositories: ["{rctx.repo}"]   # from TraT rctx, validated against an allow-list
    permissions: { contents: read }
```

### Lease (reused, `pkg/secrets`)
`{ Name, Value (the provider token), Issued, ExpiresAt, Audience, TTL≤15m }`.

## Error Handling
- TraT missing/invalid → broker denies → proxy 503 (fail closed).
- Policy deny / unknown credential → broker `ErrUnauthorized` → 503.
- GitHub API error / rate-limit → `ErrBackendDown` → 503 + metric
  `mint_error{backend="github",reason=…}`.
- Lease near expiry → refresh; if refresh fails mid-flight, fail closed.
- eBPF drops egress to a non-allow-listed host before any mint runs.

## Testing Strategy
- **Unit:** broker `Mint` (attest + TraT-verify + policy + backend) with a fake
  `DynamicBackend`; GitHub backend against a fake GitHub API (golden
  installation-token request + scoping from rctx); proxy injection (header set,
  value never returned to agent, fail-closed).
- **Security tests:** forged/unsigned TraT → deny; scope/rctx outside the repo
  allow-list → deny; agent-facing UDS path never returns a dynamic credential.
- **e2e (L1/L2):** `fake-github` + `fake-tts` fixtures; an AgentNetwork resource
  with `credential: github`; assert the upstream sees the injected token, the
  agent process can't read it, and eBPF drops egress to a disallowed host with
  `credential` set. Scenario `R-E2E-SCN-SECRETLESS`.
- **Formal:** Quint invariant — "a minted credential is injected only on
  policy-permitted egress, and only when a valid TraT authorizes it."

## Open Questions
1. **rctx → scope mapping trust** — the `rctx.repo` must be validated against a
   policy allow-list (don't let rctx request arbitrary repos). P1: explicit
   per-policy allow-list; the TraT's `azd`/Tokenetes `accessEvaluation` can
   tighten this.
2. **Who verifies the TraT** — the broker (this design, strongest) vs the proxy.
   Broker verification means a compromised proxy still can't forge intent.
3. **Backends after GitHub** — Vault dynamic + cloud STS next; GitLab via
   project/group tokens or OAuth.
4. **Caching granularity** — per `(principal, name, scope, rctx-hash)`.

## Phasing
- **P1:** `DynamicBackend` + GitHub App backend + broker `Mint` (TraT-verified)
  + proxy injection mode + CRD/policy + unit/security tests + fake-github e2e.
- **P2:** Vault + cloud STS backends; GitLab; request-derived rctx; tighter
  Tokenetes `accessEvaluation` integration.
