# Requirements — smol-agents-trat-egress

## Alignment with Product Vision

Integrate [Tokenetes](https://tokenetes.io/) Transaction Tokens (TraTs) so a
smol-agent's **egress** carries a short-lived, narrowly-scoped, identity-bound
token instead of (or alongside) a static credential. This extends the existing
identity-proxy + eBPF egress story: the eBPF layer *captures and constrains*
egress; the sidecar *injects* a per-request TraT minted from the agent's SPIFFE
identity. Aligns with the product principles: identity-keyed policy, defense in
depth (a leaked TraT expires in seconds and only works for its scope), verifiable.

Background (verified against the IETF Transaction Tokens draft + tokenetes.io):
- A **TraT** is a JWT (`typ: txntoken+jwt`) with claims `iat`, `aud` (trust
  domain), `exp`, `txn` (transaction id), `sub` (principal), `scope`
  (transaction intent), `req_wl` (requesting workload), optional `tctx`/`rctx`.
- It is minted by the **Tokenetes Service (TTS)** via OAuth Token Exchange
  (RFC 8693): `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`,
  `requested_token_type=urn:ietf:params:oauth:token-type:txn_token`,
  `subject_token=<inbound identity>`, `audience`, `scope`, optional
  `request_context`/`request_details`. Response carries `access_token` (the TraT).
- It is conveyed between workloads in the **`Txn-Token`** HTTP header (exactly one).

## Requirements

### R-TRAT-API: Configuration surface

#### R-TRAT-API-1 — Per-resource TraT injection
**User Story:** As a developer, I want to mark an egress resource so the sidecar
injects a TraT on every request to it.

**Acceptance Criteria:**
1. `AgentNetwork.spec.identityProxy.resources[].trat` (optional) SHALL enable
   TraT injection for that resource; absent ⇒ today's behaviour (JWT-SVID only).
2. `trat.scope` (required when `trat` is set) SHALL be the RFC 8693 `scope`
   (the transaction intent) for that resource.
3. `trat.audience` (optional) SHALL override the trust-domain audience; default
   is the platform trust domain.
4. `trat.header` (optional) SHALL override the conveyance header; default
   `Txn-Token`.
5. THE validating webhook SHALL reject `trat` on a resource whose `kind != http`
   in P1 (TraT injection requires HTTP-layer header insertion).

#### R-TRAT-API-2 — TTS connection
**User Story:** As an admin, I want the TTS endpoint + trust configured once,
not hardcoded.

**Acceptance Criteria:**
1. The TTS endpoint, audience, and subject-token type SHALL come from
   `AgentNetwork.spec.identityProxy.tts` with defaults inheritable from the
   `SmolAgentPlatform`.
2. The TTS endpoint SHALL be required whenever any resource sets `trat`.

### R-TRAT-MINT: Token acquisition

#### R-TRAT-MINT-1 — Token Exchange from SPIFFE identity
**User Story:** As an agent, I want my SPIFFE identity to become the TraT
subject without managing any extra credential.

**Acceptance Criteria:**
1. THE sidecar SHALL mint a TraT by calling the TTS token-exchange endpoint
   with `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`,
   `requested_token_type=urn:ietf:params:oauth:token-type:txn_token`,
   `subject_token=<agent JWT-SVID>`, the resource's `scope`, and `audience`.
2. THE `subject_token` SHALL be a JWT-SVID fetched from the SPIRE workload API
   for the TTS's expected audience.
3. THE sidecar SHALL authenticate the TTS connection with the agent's SPIFFE
   identity (mTLS via X509-SVID, or the JWT-SVID), never a static secret.

#### R-TRAT-MINT-2 — Caching + lifetime
**Acceptance Criteria:**
1. TraTs SHALL be cached in memory keyed by `(subject, scope, audience)` and
   reused until shortly before `exp` (skew margin), then re-exchanged.
2. TraTs SHALL never be written to disk or logged.

### R-TRAT-INJECT: Egress injection

#### R-TRAT-INJECT-1 — Header injection
**User Story:** As an agent, I want the TraT added to my outbound request
transparently.

**Acceptance Criteria:**
1. FOR a TraT-enabled HTTP resource, THE sidecar SHALL set the configured header
   (`Txn-Token` by default) to exactly one TraT on every upstream request.
2. THE existing `Authorization: Bearer <JWT-SVID>` injection SHALL be unchanged
   unless the resource opts out.

### R-TRAT-EBPF: eBPF coupling (no exfiltration)

#### R-TRAT-EBPF-1 — Capture + enforce, not inject
**User Story:** As a security owner, I want it impossible to send a TraT to an
unauthorized host.

**Acceptance Criteria:**
1. THE eBPF egress layer SHALL remain the mechanism that (a) transparently
   redirects egress to the injecting sidecar (`ebpfRedirect`) and (b) drops
   egress outside the allow-list (`ebpfAllowList`).
2. A TraT SHALL only be injectable on egress that the AgentNetwork egress policy
   permits; egress to a non-allow-listed destination SHALL be dropped by eBPF
   *before* any TraT is attached.
3. THE design SHALL document that eBPF does NOT rewrite (TLS) payloads; header
   injection is performed by the sidecar the eBPF layer redirects to.

### R-TRAT-SEC: Safety

#### R-TRAT-SEC-1 — Fail closed
**Acceptance Criteria:**
1. IF the TTS is unreachable or returns an error, THE sidecar SHALL reject the
   egress request (no token ⇒ no request); it SHALL NOT forward without the TraT.
2. Failures SHALL be surfaced via proxy metrics and an AgentNetwork condition.

### R-TRAT-VERIFY: Inbound verification (future / out of P1 scope)

#### R-TRAT-VERIFY-1 — Verify received TraTs
**User Story:** As an agent receiving traffic, I want inbound `Txn-Token`s
verified before my code sees them.

**Acceptance Criteria:**
1. (Future) THE ingress side of the sidecar MAY verify an inbound `Txn-Token`
   (signature against the TTS JWKS, `exp`, `aud`, `txn`) — or delegate to the
   Tokenetes Agent sidecar — before forwarding to the agent. Egress injection is
   P1; inbound verification is a follow-on.
