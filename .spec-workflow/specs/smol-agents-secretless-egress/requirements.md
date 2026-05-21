# Requirements — smol-agents-secretless-egress

## Alignment with Product Vision

Make smol-agents **secretless** toward external providers (GitHub, GitLab) and
backends. When an agent egresses to e.g. `api.github.com`, the eBPF-redirected
sidecar proxy asks the **secret broker** (extended with a *dynamic* backend) to
**mint a short-lived provider credential**, authorized by the agent's **SPIFFE
identity** and the **TraT's `sub`/`scope`/`rctx`**, then **injects** it into the
outbound request — and **the agent never sees it**. The eBPF egress allow-list
guarantees the minted credential can only leave toward the authorized host.

This composes three existing pieces: the eBPF egress capture/allow-list
(`agentnet`), the SPIFFE-attested secret broker (`pkg/secrets`), and the egress
injection seam + TraT authorization context (`smol-agents-trat-egress`). It turns
"a long-lived PAT in the agent's env" into "a 1-hour, repo-scoped, per-transaction
token the agent can't read and can't exfiltrate."

Depends on: **smol-agents-trat-egress** (TraT minting/injection) for the
authorization context. Builds on: **R-SEC-*** (the broker).

## Requirements

### R-SEGR-API: Configuration surface

#### R-SEGR-API-1 — Per-resource credential injection
**User Story:** As a developer, I want to mark an egress resource so the proxy
injects a broker-minted provider credential my agent never sees.

**Acceptance Criteria:**
1. `AgentNetwork.spec.identityProxy.resources[].credential` (optional) SHALL
   enable secretless injection for that resource.
2. `credential.name` (required) SHALL identify the broker credential/policy
   entry (e.g. `github`).
3. `credential.header` (default `Authorization`) and `credential.scheme`
   (default `Bearer`) SHALL control how the minted value is placed on the request.
4. THE validating webhook SHALL require `kind == http` and that the resource's
   destination is covered by the egress allow-list/redirect (R-SEGR-EBPF-1).

#### R-SEGR-API-2 — Broker credential policy
**User Story:** As an admin, I want to declare which identity+scope may mint
which provider credential, and how it's scoped.

**Acceptance Criteria:**
1. THE broker policy SHALL map `(SPIFFE principal, TraT scope)` → an allowed
   credential name + a dynamic backend + per-mint scoping inputs.
2. Policy SHALL be deny-by-default.

### R-SEGR-MINT: Dynamic credential minting

#### R-SEGR-MINT-1 — Dynamic backend interface
**User Story:** As a platform, I want pluggable backends that mint short-lived
provider credentials.

**Acceptance Criteria:**
1. THE broker SHALL support a `DynamicBackend.Mint(ctx, CredentialRequest)
   → Lease` alongside the existing static `Backend.Fetch` (R-SEC-3).
2. `CredentialRequest` SHALL carry the attested principal and the verified TraT
   `sub`/`scope`/`rctx` so the backend can scope the credential.
3. Minted leases SHALL obey `MaxLeaseTTL` (≤ 15m) and SHALL carry the provider
   credential's own expiry when shorter.

#### R-SEGR-MINT-2 — GitHub App backend (first backend)
**User Story:** As an agent, I want a repo-scoped GitHub token without holding
the App key.

**Acceptance Criteria:**
1. THE GitHub backend SHALL hold the GitHub **App private key** (itself sourced
   from the broker / a mounted secret, never exposed to the agent) and mint an
   **installation access token** via the GitHub API.
2. THE installation token SHALL be scoped (repositories/permissions) from the
   TraT `scope`/`rctx` per policy — narrowest viable, not the App's full reach.
3. Vault dynamic secrets + cloud STS SHALL be addable behind the same interface
   (designed for, not required in P1).

### R-SEGR-AUTH: Authorization model

#### R-SEGR-AUTH-1 — SPIFFE + verified TraT
**User Story:** As a security owner, I want minting gated by *both* a real in-pod
caller and a signed statement of intent.

**Acceptance Criteria:**
1. THE broker SHALL attest the calling sidecar via `SO_PEERCRED` → SPIFFE
   (existing R-SEC-1).
2. THE broker SHALL **verify the TraT signature** (against the TTS JWKS) and use
   its `sub`/`scope`/`rctx` as the authoritative authorization context — a
   compromised sidecar cannot forge scope/rctx.
3. THE mint decision SHALL require both the attested principal AND a valid TraT
   satisfying policy.

### R-SEGR-INJECT: Injection (agent-blind)

#### R-SEGR-INJECT-1 — Proxy injects, agent never sees
**Acceptance Criteria:**
1. THE minted credential SHALL flow broker → proxy → upstream request only; it
   SHALL NOT be returned to the agent over the agent-facing UDS.
2. THE proxy SHALL set `credential.header` to `scheme + " " + value` on each
   upstream request, refreshing before lease expiry.

### R-SEGR-EBPF: No exfiltration

#### R-SEGR-EBPF-1 — Allow-list gates the credential
**Acceptance Criteria:**
1. A credential SHALL only be injectable on egress permitted by the AgentNetwork
   egress allow-list/redirect; egress to a non-allow-listed host SHALL be dropped
   by eBPF before any mint/inject occurs.
2. THE design SHALL document that injection is at the sidecar (eBPF captures +
   enforces; it does not rewrite TLS payloads).

### R-SEGR-SEC: Safety

#### R-SEGR-SEC-1 — Fail closed + no leakage
**Acceptance Criteria:**
1. IF minting fails (policy deny, backend error, TraT invalid), THE proxy SHALL
   reject the egress request (no credential ⇒ no request).
2. Root provider secrets (e.g. the GitHub App key) and minted values SHALL never
   be logged or written to disk; leases live in memory only.
3. Failures SHALL be surfaced via proxy/broker metrics and an AgentNetwork
   condition.
