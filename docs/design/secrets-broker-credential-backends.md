# Secret Broker: Static & Dynamic Credential Backends

Status: proposed · Owner: platform · Date: 2026-06-02
Scope: the broker's two backend shapes — **static** secret leasing and **dynamic** provider-credential minting — their attestation/authorization model, and the gap that the dynamic path (GitHub-App-style mint) is configurable **only via inline Go in a runbook**, with no CRD or operator hook. Implementation references are to v0.2.0 source. Companion feature docs: [Egress Credentials](../features/egress-credentials.md), [Runtime & Identity](../features/runtime-and-identity.md); runbook: [Secretless egress](../runbooks/secretless-egress.md).

> **This doc does not re-design the wire protocol or the proxy injection seam.** Those exist and are verified (`pkg/secrets/wire.go`, `pkg/agentnet/proxy/http.go`). It documents the backend interfaces as built, then specifies the declarative-config layer that is missing for dynamic backends.

## 1. Problem

An agent needs two distinct kinds of secret material at runtime:

1. **Pre-provisioned secrets** — an API key, a model-provider token — that already exist in a Kubernetes `Secret` and just need to be handed to the right process, short-lived, without baking them into the image or the pod env at admission time.
2. **Dynamically minted, request-scoped credentials** — e.g. a GitHub App *installation access token* scoped to one repo for ~1h — that **do not exist until the moment of use** and are minted from a root secret the agent must never see.

The broker (`pkg/secrets`, shipped as the `secret-proxy` sidecar in `cmd/secret-proxy`) serves both over a Unix-domain socket, behind one attestation model. Case (1) — the **static backend** — is fully wired by the operator: the `AgentRun`/`AgentSession` controller resolves the declared `secretRef`s and generates the broker config (`operator/internal/builders/secret_broker.go`). Case (2) — the **dynamic backend** — exists, is tested, and is proven end-to-end on real SPIRE, **but the only way to turn it on is to construct `secrets.Server{Dynamic, TraTVerifier, CredPolicy}` in Go**, as the runbook shows ([secretless-egress.md §2](../runbooks/secretless-egress.md)). There is no CRD field, no operator builder, and `cmd/secret-proxy` does not parse any dynamic config. **Closing that gap is the subject of this doc.**

### What ships today (verified against source)

- **One broker process, two request kinds.** `Server.dispatch` routes `reqLease` → `handleLease` (static) and `reqMint` → `handleMint` (dynamic) over the same UDS (`pkg/secrets/server.go:170-183`). Wire kinds are `lease|refresh|mint|close` (`pkg/secrets/wire.go:19-24`).
- **Static backend is operator-generated.** `BuildBrokerConfigSecret` renders a `static` backend + an allow-list `Policy`, both keyed to the run pod's local identity `spiffe://local.smol-agents/uid/65532` (`secret_broker.go:112-149`); the controller gathers the values from declared `secretRef`s in `gatherRunSecrets` (`operator/internal/controllers/agentmodel/secrets.go:43-90`).
- **Dynamic backend + credential policy are interfaces with a real impl.** `DynamicBackend.Mint`, `CredentialPolicy.AuthorizeMint`, and the `GitHubAppBackend`/`StaticCredentialPolicy` concrete types all exist and are tested (`pkg/secrets/types.go:75-93`, `backend_github.go`, `credpolicy.go`).
- **`cmd/secret-proxy` does not wire the dynamic path.** Its `brokerConfig` parses only `socketPath`, `peerAuth`, `backend.{static,vault}`, and `policy` (`cmd/secret-proxy/main.go:26-47`); it constructs `secrets.Server` with **no** `Dynamic`/`TraTVerifier`/`CredPolicy` (`main.go:86-94`). `buildBackend` knows only `static` and a stub `vault` (`main.go:161-180`). So a broker started from the shipped binary **cannot mint** — `handleMint` returns `"dynamic credential minting not configured"` (`server.go:243-245`).
- **The operator never references the dynamic types.** A grep of `operator/` for `DynamicBackend|GitHubAppBackend|CredentialPolicy|Mint` returns nothing — the operator builds only the static config.
- **The sole production caller of `Mint` is the egress proxy.** `pkg/agentnet/proxy/http.go:136` calls `p.Broker.Mint(...)` (via the `CredentialMinterAdapter`, `pkg/secrets/proxyminter.go:17-23`) on the agent-blind broker→proxy→upstream path. No CLI harness path mints.

The result: dynamic minting is a **first-class library feature with a zero-config operator surface**. An operator can deploy secretless egress only by hand-rolling a broker that imports `pkg/secrets` and constructs the server in Go — which is exactly what the runbook documents.

## 2. Attestation model (shared by both backends)

Both backends sit behind the same caller-authentication gate. The broker authenticates the peer **once per connection** in `Server.handle` before any request is dispatched (`server.go:144-149`); a failed attestation closes the connection.

### 2.1 SO_PEERCRED + SPIRE/Local attestor (fail-closed)

The `PeerAttestor` resolves a UDS peer to a SPIFFE ID (`pkg/secrets/peer_attestor.go:16-18`). Two implementations, selected by `peerAuth`:

- **SPIRE** (`peerAuth: spire`, the default): `SPIREPeerAttestor` reads the peer's PID via `SO_PEERCRED` (`peer_attestor_linux.go:34-37`, `peerUcred` at `:108-132`) and resolves an SVID through the SPIRE workload API (`:51-68`). If SPIRE returns no SVID, it fails with `ErrPeerNotSpiffe` (`:61-62`) — **fail-closed**: no SPIRE, no identity, no secret.
- **Local** (`peerAuth: local`): `LocalPeerAttestor` authenticates by the kernel-verified `SO_PEERCRED` **uid** alone and mints a synthetic `spiffe://local.smol-agents/uid/<uid>` (`peer_attestor_local.go:11-19`, `peer_attestor_linux.go:84-96`). This is the no-SPIRE fallback for in-pod brokers (the `EmptyDir` socket bounds access to the pod; the uid bounds it to the workload user). It is explicitly **weaker** than SPIRE — no cryptographic per-workload identity — and the type comment says so (`peer_attestor_local.go:13-19`).
- **`spire+local`**: a `MultiAttestor` that tries SPIRE first and falls back to local (`cmd/secret-proxy/main.go:146-155`, `peer_attestor.go:35-50`).

`peerAuth` is configurable per broker (`cmd/secret-proxy/main.go:30-33`). The operator-generated **run** broker uses `peerAuth: local` because the run pod carries no SPIRE CSI socket (`secret_broker.go:127`, and the comment at `:1-7`). A SPIRE-backed broker (the egress-credential case) uses `spire`.

The attested SPIFFE ID is the **`principal`** threaded into every authorization decision below.

### 2.2 TraT verification + sender-constraint (gates dynamic mint only)

`handleLease` (static) needs only the peer identity + the policy. `handleMint` (dynamic) adds a second, independent credential — a **Transaction Token (TraT)** — and verifies it before minting (`server.go:242-296`):

1. **Configured?** If `Dynamic`, `TraTVerifier`, or `CredPolicy` is nil, reject (`server.go:243-245`). (This is the switch the shipped binary never flips.)
2. **TraT present?** A mint with no TraT is rejected (`server.go:249-251`).
3. **Signature + aud + exp.** `TraTVerifier.Verify` parses the compact JWT, verifies the signature against the TTS JWKS *first* (so forged tokens never reach claim logic), then checks expiry and audience (`pkg/trat/verifier.go:35-104`). The JWKS is fetched + cached from the TTS `jwks_uri` (`HTTPKeySource`, `verifier.go:111-165`).
4. **Sender-constraint.** The TraT's `req_wl` (requesting workload) **must equal** the `SO_PEERCRED`-attested `principal.String()`, else reject with `"trat not bound to caller"` (`server.go:262-265`). This is what makes the TraT *sender-constrained*: a TraT minted for workload A cannot be replayed by workload B even if B's policy overlaps. Absent `req_wl` → fail-closed.
5. **Policy authorize + narrow.** `CredPolicy.AuthorizeMint` receives the verified `CredentialRequest` (principal + TraT `sub`/`scope`/`req_wl`/`rctx`, `types.go:63-70`) and either returns a possibly-narrowed request or denies (`server.go:266-278`).
6. **Mint.** Only now is `Dynamic.Mint` invoked (`server.go:279-283`); the lease's `ExpiresAt` is capped to `MaxLeaseTTL` (≤ 15m, `types.go:11-15`, `server.go:284-293`).

### 2.3 The three independent controls

Defense-in-depth for a single dynamically-minted credential — remove any one and the other two still hold (this mirrors [egress-credentials.md "Defense in depth"](../features/egress-credentials.md)):

| Control | Where enforced | Guarantees |
|---|---|---|
| SPIFFE identity (`SO_PEERCRED` + SPIRE/Local) | `server.go:144-149`, `peer_attestor_linux.go` | only the real in-pod caller can reach the broker |
| Signed, sender-constrained TraT | `server.go:252-265`, `pkg/trat/verifier.go` | the request matches an authorized *intent*, bound to that caller |
| Network allow-list (eBPF egress) | `pkg/agentnet` (out of this doc's scope) | the credential can only leave toward the authorized host |

A TraT is **not** an external credential — it authorizes the mint; the provider token (e.g. the GitHub installation token) is what reaches the upstream.

## 3. Backend interfaces

### 3.1 Static backend — `Backend`

`Backend.Fetch(ctx, principal, name) ([]byte, error)` is the pluggable static store; implementations "SHOULD scope by principal so a misconfigured policy cannot leak" (`pkg/secrets/types.go:46-57`). `handleLease` gates every fetch on `Policy.Allowed(principal, name)` (deny-by-default via `StaticPolicy`, `server.go:189-192`, `types.go:95-134`), then wraps the bytes in a short-lived `Lease` (`server.go:201-216`). `refresh` re-validates policy + re-fetches so a newly-revoked grant blocks future refreshes (`server.go:219-235`).

**Operator wiring (complete).** For an `AgentRun`/`AgentSession`, `gatherRunSecrets` resolves each input/harness-env `secretRef` plus the model-provider key into a `name → bytes` map (`agentmodel/secrets.go:43-90`), and `BuildBrokerConfigSecret` emits a `static` backend + matching allow-list keyed to `spiffe://local.smol-agents/uid/65532` (`secret_broker.go:112-149`). The `secret-proxy` is attached as a **native sidecar** (init container, `RestartPolicy=Always`) so the pod still reaches a terminal phase when the agent exits (`secret_broker.go:52-81`, `secretProxyRunContainer` at `:151-183`). This is the declarative path that *works today*.

```
Agent.spec.harness.env[].secretRef  ─┐
RunInputFile.secretRef               ─┼─▶ gatherRunSecrets ─▶ BuildBrokerConfigSecret ─▶ Secret(config.yaml)
ModelProvider.spec.secretRef         ─┘        (controller)         (static backend + policy)        │
                                                                                                     ▼
                                                                              secret-proxy sidecar (peerAuth=local)
```

### 3.2 Dynamic backend — `DynamicBackend` + `CredentialPolicy`

The dynamic path has three collaborating types (`pkg/secrets/types.go:59-93`):

- **`CredentialRequest`** — the *verified* authorization context: the attested `Principal` plus the TraT's `Subject`/`Scope`/`ReqWL`/`ReqCtx` (`types.go:63-70`).
- **`DynamicBackend.Mint(ctx, CredentialRequest) (Lease, error)`** — mints a short-lived, request-scoped provider credential; "MUST NOT log the minted value or any root secret" (`types.go:72-78`).
- **`CredentialPolicy.AuthorizeMint(CredentialRequest) (CredentialRequest, error)`** — deny-by-default authorize-and-narrow (`types.go:80-93`).

**The shipped concrete impl** is `GitHubAppBackend` (`pkg/secrets/backend_github.go`): from the TraT's `rctx.repo` it resolves the App installation (`GET /repos/{owner}/{repo}/installation`, `:105-117`) and mints a repo-scoped installation access token (`POST /app/installations/{id}/access_tokens`, `:119-136`), with permissions derived from the TraT scope via `ScopePermissions` (`:30-33`, `:76`). The App private key lives only in the backend struct (`:22-33`) and is never returned to the agent — the lease value is the *installation token*. `StaticCredentialPolicy` (`pkg/secrets/credpolicy.go`) is a `(principal → scope → grant)` deny-by-default map that also validates `rctx.repo` against a per-grant allow-list (`credpolicy.go:43-62`).

**Where it is wired:** only in tests (`pkg/secrets/mint_test.go`, `backend_github_test.go`) and in the runbook's inline Go (`secretless-egress.md §2`, which sets `Server.Dynamic`, `Server.TraTVerifier`, `Server.CredPolicy` by hand). **The `secret-proxy` binary and the operator know nothing about these types** (§1). The credential is consumed by the egress proxy's agent-blind injection seam (`pkg/agentnet/proxy/http.go:122-142`), which keeps the value off the agent-facing path.

### 3.3 The shape of the gap

| Aspect | Static backend | Dynamic backend |
|---|---|---|
| Interface | `Backend` (`types.go:50`) | `DynamicBackend` + `CredentialPolicy` (`types.go:75-86`) |
| Concrete impl | `StaticBackend` (`backend_static.go`) | `GitHubAppBackend` (`backend_github.go`) |
| `cmd/secret-proxy` config | ✅ `backend.static`, `policy` (`main.go:36-47`) | ❌ none parsed |
| Operator builder | ✅ `BuildBrokerConfigSecret` (`secret_broker.go`) | ❌ none |
| CRD surface | ✅ `secretRef` on Agent/inputs/ModelProvider | ❌ none — only `AgentNetwork` `credential.{name,scope}` *names* a credential the broker must already know how to mint |
| How you turn it on | declare a `secretRef`; controller does the rest | **write Go**, construct `secrets.Server{Dynamic,…}` by hand |

The `AgentNetwork` `identityProxy.resources[].credential` block (`name`, `scope`, `header`, `scheme` — see [egress-credentials.md "The CR"](../features/egress-credentials.md)) is the *consumer* side: it tells the proxy *which* credential to request and *how* to inject it. But nothing declaratively tells the **broker** *how to mint* `name=github` — which App ID, which private key, which `ScopePermissions`, which repo allow-list. That binding lives only in hand-written Go. **The gap is a declarative backend-registration surface for dynamic credentials.**

## 4. The gap and the design options

### 4.1 Question 1 — scope: cluster-wide, per-Agent, or per-AgentRun?

A dynamic backend holds a **root secret** (the GitHub App private key) and mints on behalf of many agents. Three placements:

- **Cluster-wide singleton.** One platform-team-owned backend per provider (one GitHub App), referenced by many tenants. Matches the runbook's mental model (one App installed org-wide) and how secretless egress was proven. Best root-secret hygiene (one blast radius, one rotation). Requires the per-tenant `CredentialPolicy` to do the isolation (it already does — `StaticCredentialPolicy` is keyed on the agent SPIFFE ID, `credpolicy.go:43-48`).
- **Per-Agent.** Each Agent (or namespace) brings its own App. Strong tenant isolation, but multiplies root secrets and rotation surface; usually overkill.
- **Per-AgentRun.** A backend minted per run is nonsensical for an App private key (it is long-lived infrastructure, not run state); the *leases* are already per-run/short-lived (`MaxLeaseTTL`, `server.go:284-293`).

**Recommendation (DESIGN):** model the **backend as cluster- or namespace-scoped infrastructure** (the root secret), and the **authorization-to-mint as per-Agent policy** (which already maps cleanly onto `StaticCredentialPolicy` and the existing per-resource `AgentNetwork` `credential` block). This keeps root-secret count low while preserving per-agent, per-scope, per-repo authorization.

### 4.2 Option A — a `DynamicCredentialBackend` CRD (DESIGN)

A cluster- (or namespace-) scoped CR registering one provider backend; the operator generates the broker's dynamic config from it (mirroring how `BuildBrokerConfigSecret` generates the static config today). Sketch — **not yet built**:

```yaml
# (future) — proposed, no CRD exists yet
apiVersion: runtime.agents.smol-agents.ai/v1
kind: DynamicCredentialBackend
metadata: { name: github, namespace: platform-secrets }
spec:
  credentialName: github            # matches AgentNetwork resources[].credential.name
  provider: githubApp               # → secrets.GitHubAppBackend
  githubApp:
    appID: "123456"
    privateKeyRef: { secretName: github-app-key, key: private-key.pem }  # broker-only
    scopePermissions:               # TraT scope → installation-token permissions
      "github:repo:read": { contents: read }
  policy:                           # → secrets.StaticCredentialPolicy grants
    - principal: spiffe://smol-agents.ai/ns/tenant-a/sa/agent
      scope: github:repo:read
      repos: [ smol-platform/app ]  # rctx.repo allow-list
```

The operator would: mount `privateKeyRef` into the SPIRE-backed egress broker (never into the agent), populate `Server.Dynamic`/`CredPolicy`/`TraTVerifier` from `spec`, and reconcile changes. This is the **direct analogue of the static path** and the smallest conceptual leap. Trade-off: a new top-level CRD and a broker that loads a private-key Secret.

### 4.3 Option B — extend `AgentNetwork` with backend definitions (DESIGN)

The `AgentNetwork` `identityProxy` block already declares the TTS, JWKS, and the *consumer-side* `credential` (name/scope/header/scheme). Option B adds the **producer side** in the same place — a `credentialBackends:` list co-located with the `resources[].credential` that references it:

```yaml
# (future) — proposed extension to the existing AgentNetwork identityProxy
spec:
  identityProxy:
    tts: { url: …, jwksUrl: … }
    credentialBackends:                         # NEW (producer side)
      - name: github
        provider: githubApp
        githubApp: { appID: "123456", privateKeyRef: {…}, scopePermissions: {…} }
        grants:
          - { principal: …, scope: github:repo:read, repos: [smol-platform/app] }
    resources:
      - { name: github, kind: http, credential: { name: github, scope: github:repo:read } }  # consumer side (exists)
```

This keeps egress producer + consumer in one CR (good locality, one RBAC surface) at the cost of putting a root-secret reference on a tenant-namespaced, tenant-editable object — weaker separation of duties than a platform-team-owned CRD (Option A). The interaction between `AgentNetwork` (egress) and `AgentPolicy` (admission/authorization) is examined in [agentnetwork-agentpolicy-interaction.md](agentnetwork-agentpolicy-interaction.md); a backend-on-AgentNetwork choice should be reconciled with whatever ownership model that doc lands on.

### 4.4 Recommendation and migration

**Lean Option A** (a platform-owned `DynamicCredentialBackend` CRD) for separation of duties: the root secret and the App registration belong to the platform team, while tenants keep only the *consumer-side* `credential` reference they already have. Reassess if [agentnetwork-agentpolicy-interaction.md](agentnetwork-agentpolicy-interaction.md) argues for a single egress object.

Migration from inline-Go to declarative config:

1. **Teach `cmd/secret-proxy` a `backend.kind: dynamic` block** (provider, key-ref path, `scopePermissions`, grants) and wire `Server.{Dynamic,TraTVerifier,CredPolicy}` from it — the binary change that makes the runbook's Go unnecessary. *(Today `buildBackend` rejects everything but `static`/`vault`, `main.go:161-180`.)*
2. **Add the operator builder** (`BuildDynamicBrokerConfig`, analogous to `BuildBrokerConfigSecret`) that renders that YAML from the CRD and mounts the key Secret into the SPIRE-backed broker only.
3. **Keep the inline-Go path working** (the library API is unchanged — `secrets.Server` fields stay) so existing hand-rolled brokers and tests are unaffected; the CRD is purely additive.
4. **Document** the new CRD with a `(future) docs/features/secrets-api.md`-style reference once it lands (no such doc exists yet).

## 5. Multi-tenancy

What exists vs. what's needed for safe multi-tenant dynamic credentials:

| Concern | Exists today | Needed (DESIGN) |
|---|---|---|
| **SPIFFE-scoped policy isolation** | ✅ `StaticCredentialPolicy` is keyed on the agent SPIFFE ID; deny-by-default; per-grant `scope` + `repos` allow-list (`credpolicy.go:43-62`). Sender-constraint binds the TraT to that identity (`server.go:262-265`). | A CRD/operator surface so each tenant's grants are generated from declarative policy rather than appended in Go; per-namespace RBAC on who may register grants. |
| **Lease TTL / rotation** | ✅ Hard cap `MaxLeaseTTL` ≤ 15m enforced regardless of per-call TTL (`types.go:11-15`, `server.go:284-293`); minted tokens are inherently short-lived (GitHub installation tokens ~1h, capped down). `refresh` re-validates policy each time (`server.go:219-235`). | **Root-secret rotation** (the App private key) — no rotation hook today; would be a backend/operator concern (re-mount the Secret, reload the key). |
| **Revocation** | ⚠️ Partial. Policy revocation blocks *future* leases/mints/refreshes (`server.go:189-192`, `:232-234`); minted provider tokens remain valid upstream until their own short expiry. There is no active revoke-at-provider call. | An explicit revoke path (or rely on short TTL as the de-facto revocation window). |
| **Audit logging** | ⚠️ The broker logs *failures* with principal/scope/credential (`server.go:254`, `:263`, `:276`, `:281`) and never logs the minted value or root secret (`types.go:74-77`). The TraT carries `txn` (transaction id) for correlation (`pkg/trat/types.go:37`). | Structured **success** audit events (who minted what, for which `txn`/`rctx`), emitted to the platform's audit sink — today only denials are reliably visible. |

The crucial multi-tenant property — **one tenant cannot mint another's credential** — already holds via the SPIFFE-keyed `CredentialPolicy` + the `req_wl` sender-constraint. The gaps are operational (declarative grants, rotation, success-audit), not a hole in the authorization model.

## 6. CLI-harness caveat (honest limit on agent-blindness)

Agent-blindness — "the agent never holds the credential" — is a property of the **opaque HTTP proxy path**, not a universal guarantee. Be precise about which harness shape gets it:

- **Opaque HTTP harnesses (e.g. Hermes): blind.** The proxy mints the credential and injects it into the *upstream request header* without ever returning it to the agent (`pkg/agentnet/proxy/http.go:126-142`; the lease value is consumed at `:141` and discarded). The agent's HTTP client points at `http://127.0.0.1:<port>` and never sees the token. The root secret stays in the broker. This is the [egress-credentials.md](../features/egress-credentials.md) story and it holds.

- **CLI harnesses (claude-code, codex, aider, goose, generic-cli): NOT blind for static leases.** For the *static* lease path, broker-leased secret values are merged into the harness **subprocess environment** by `mergeEnv` (`pkg/agentruntime/harness/cli.go:108-119`) and the subprocess runs as the **same uid 65532** as the broker client (`secret_broker.go:39`). Any `env`-reading code in the CLI's own bash/tool loop — which runs at that uid — can read the value. Short-lived leasing reduces the *window* and avoids baking secrets into the image, but it does **not** make a CLI harness blind to its own env. (The dynamic *mint* path is reached only by the proxy, `http.go:136` — a CLI harness does not call `Mint`, so this caveat is specifically about static env injection.)

**Recommendation (DESIGN):** for daemons and CLI harnesses that genuinely must not read a credential, prefer either (a) **SPIRE-issued mTLS** so the credential is an X.509-SVID the process authenticates *with* but cannot exfiltrate as a bearer token, or (b) routing the CLI's traffic through the **same opaque proxy seam** so the mint/inject happens outside the agent process. Choosing the harness shape is a security decision, documented alongside harness authoring in [harness-authoring.md](harness-authoring.md); packaging a CLI that routes egress through the proxy is a [custom-agent-images.md](custom-agent-images.md) concern. The env-injection mechanism itself is also noted in the runtime-fit analysis ([agent-runtime-fit-analysis-v0.2.0.md](../research/agent-runtime-fit-analysis-v0.2.0.md)).

## 7. Cross-links

- [Egress Credentials — TraT & Secretless](../features/egress-credentials.md) — the consumer side: proxy injection, the three controls, the `AgentNetwork` `credential` block.
- [Runtime & Identity](../features/runtime-and-identity.md) — the broker in the wider identity/SPIFFE/lease story (§4 "Secrets — kloak-style broker").
- [Runbook: Secretless egress](../runbooks/secretless-egress.md) — the **inline-Go** dynamic-backend configuration this doc proposes to make declarative.
- [AgentNetwork ⇄ AgentPolicy interaction](agentnetwork-agentpolicy-interaction.md) — ownership/authorization model that informs Option B.
- [Harness authoring](harness-authoring.md) and [Custom agent images](custom-agent-images.md) — where the CLI-harness env-readability caveat (§6) is actionable.
- [Agent runtime fit analysis (v0.2.0)](../research/agent-runtime-fit-analysis-v0.2.0.md) — runtime-isolation context for the agent-blindness boundary.
