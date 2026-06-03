# Spec: Declarative dynamic credential backends — from inline-Go to a CRD-driven mint surface

> **Status: DESIGN / PARTIALLY BUILT (v0.2.0).** As of HEAD (2026-06-03) the dynamic provider-credential **mint path is a complete, tested library** (`pkg/secrets` — `DynamicBackend`, `CredentialPolicy`, `GitHubAppBackend`, `StaticCredentialPolicy`, the `reqMint` server handler with TraT sender-constraint) **with a zero-config operator/CLI surface**. The only ways to turn it on are (a) hand-rolling a `secrets.Server{Dynamic,TraTVerifier,CredPolicy}` in Go (the runbook's approach, [`docs/runbooks/secretless-egress.md`](../runbooks/secretless-egress.md) §2) or (b) the L0 e2e probe (`cmd/spiffe-probe/secretless.go:79-100`). The shipped `cmd/secret-proxy` binary parses **only** `static`/`vault` backends and constructs the server with `Dynamic`/`TraTVerifier`/`CredPolicy` left **nil** (`cmd/secret-proxy/main.go:86-94`, `:161-180`); a mint against it returns `"dynamic credential minting not configured"` (`pkg/secrets/server.go:243-245`). The operator references none of the dynamic types. This spec specifies the missing declarative layer: a `DynamicCredentialBackend` CRD, a `secret-proxy` config block + flag wiring, an operator builder that renders that config and mounts the root secret into a **SPIRE-backed** broker, and the migration path from inline-Go. Every "exists today" claim cites `file:line`; every proposed change is marked **(proposed)**.

> **Builds on (read first, do not duplicate):** [`docs/design/secrets-broker-credential-backends.md`](../design/secrets-broker-credential-backends.md). That design doc surveys both backend shapes (static / dynamic), the shared attestation model, and frames the scope question (cluster / per-Agent / per-Run). **This spec is the implementation-grade plan for the dynamic half** — it resolves the open scope decision, names the CRD fields, and gives `file:line` wiring targets. It does **not** re-derive the wire protocol, the attestation model, or the static-backend path, which are documented there and verified.

> **Hard dependency:** the credential this spec lets you *mint declaratively* is **consumed only by the egress-proxy injection seam** (`pkg/agentnet/proxy/http.go:122-142`), and that proxy sidecar is **not injected by the operator on any datapath today** (it lives only in `cmd/spiffe-probe` + the runbook). So a fully end-to-end declarative secretless-egress path also requires [`docs/specs/agentnetwork-datapath-enforcement.md`](./agentnetwork-datapath-enforcement.md) to land the proxy + SPIRE broker on the run/serving pod. This spec specifies the **producer** (mint config); that spec specifies the **transport** (proxy injection). See §4.4 and §8.

---

## 1. Summary

A "dynamic credential backend" mints a short-lived, request-scoped provider credential (today: a GitHub App **installation access token** scoped to one repo for ≤15 min) from a **root secret the agent must never see** (the App private key). The mint is gated by three independent controls — SO_PEERCRED+SPIRE peer attestation, a signed sender-constrained TraT, and a deny-by-default `CredentialPolicy` — all of which exist and are tested (`pkg/secrets/server.go:242-296`).

The gap this spec closes is **operability**, not security: there is no declarative way to register a backend. An operator can only turn on dynamic minting by writing Go that constructs `secrets.Server{Dynamic,TraTVerifier,CredPolicy}` by hand. "Full support" here means: a platform-team-owned **`DynamicCredentialBackend` CRD** declares `{provider, root-secret ref, scope→permissions, grants}`; an **operator builder** renders that into a `secret-proxy` config block + flags and mounts the root secret into a **SPIRE-backed** broker (never into the agent); `cmd/secret-proxy` learns a `backend.kind: dynamic` block and wires the three server fields from it; and the **inline-Go library API stays unchanged** so existing hand-rolled brokers and tests keep working (the CRD is purely additive). The outcome: declaring a `DynamicCredentialBackend` + a per-Agent grant is enough to mint secretless provider credentials, with SPIFFE-scoped tenant isolation, lease-TTL rotation, and audit — no Go required.

**Scope decision resolved (was open in the design doc §4.1):** the **backend is cluster- or namespace-scoped platform infrastructure** (one root secret, one rotation surface, one blast radius), and the **authorization-to-mint is per-Agent grant**. There is no per-Run backend — per-Run scoping already lives in the *lease* (≤`MaxLeaseTTL`, `pkg/secrets/server.go:288-290`) and in the per-request TraT `rctx`. See §4.1.

---

## 2. Current state

### 2.1 What exists (verified, tested)

| Thing | Where (`file:line`) | State |
|---|---|---|
| `DynamicBackend` interface | `pkg/secrets/types.go:75-78` | `Mint(ctx, CredentialRequest) (Lease, error)` + `Close()` |
| `CredentialPolicy` interface | `pkg/secrets/types.go:84-93` | `AuthorizeMint(req) (req, error)`; deny-by-default; `CredentialPolicyFunc` adapter |
| `CredentialRequest` (verified ctx) | `pkg/secrets/types.go:63-70` | `{Name, Principal, Subject, Scope, ReqWL, ReqCtx}` |
| `GitHubAppBackend` concrete impl | `pkg/secrets/backend_github.go:18-166` | mints repo-scoped installation tokens; App key in-struct only (`:22-33`); `ScopePermissions` map (`:30-32`); never returns the root key |
| `StaticCredentialPolicy` | `pkg/secrets/credpolicy.go:21-62` | `(principal → scope → grant)` map; per-grant repo allow-list validated against `rctx.repo` (`:55-60`) |
| `reqMint` server handler | `pkg/secrets/server.go:242-296` | (1) nil-check the 3 fields → reject (`:243-245`); (2) verify TraT sig+aud+exp; (3) **sender-constraint** `req_wl == principal` (`:262-265`); (4) `AuthorizeMint`; (5) `Mint`; cap to `MaxLeaseTTL` (`:288-290`) |
| Mint wire kind | `pkg/secrets/wire.go:19-24`, `:32` | `reqMint = "mint"`; `request.TraT` field |
| Proxy consumer (the only prod `Mint` caller) | `pkg/agentnet/proxy/http.go:127-141`, `pkg/secrets/proxyminter.go:17-23` | `CredentialMinterAdapter` → `Client.Mint`; injects into upstream header; value discarded after (`:141`) |
| Inline-Go config (runbook) | `docs/runbooks/secretless-egress.md` §2 | constructs `Server.{Dynamic,TraTVerifier,CredPolicy}` by hand |
| L0 e2e probe wiring | `cmd/spiffe-probe/secretless.go:79-100` | the only in-repo place that builds `GitHubAppBackend` + grants + verifier against fakes |
| TraT verifier + claims | `pkg/trat/verifier.go:35`, `pkg/trat/types.go:33-39` | `Verify` → `Claims{Subject,Scope,ReqWL,ReqCtx,...}`; JWKS-cached (`HTTPKeySource`) |

### 2.2 What is stubbed / missing — the gap this spec closes

| Gap | Evidence (`file:line`) |
|---|---|
| **No CRD** registers a dynamic backend | `operator/api/agentmodel/v1/` has `agentnetwork.go`, `memory.go`, `types.go` only; no `dynamiccredentialbackend.go`. No CRD manifest under `operator/config/crd/`. |
| **`cmd/secret-proxy` cannot mint** | `brokerConfig` parses `socketPath/peerAuth/backend.{static,vault}/policy` only (`cmd/secret-proxy/main.go:26-47`); the server is built with **no** `Dynamic`/`TraTVerifier`/`CredPolicy` (`:86-94`); `buildBackend` rejects everything but `static`/`vault` (`:161-180`). |
| **No operator builder** for dynamic config | `operator/internal/builders/secret_broker.go` renders **only** `static` backend + policy (`BuildBrokerConfigSecret`, `:112-149`); `brokerConfigYAML` has no dynamic fields (`:85-104`). A grep of `operator/` for `DynamicBackend\|GitHubAppBackend\|CredentialPolicy\|Mint` returns nothing. |
| **The injected broker is static + local only** | `AttachSecretBroker` (the only operator broker-injection site, called from `agentrun_controller.go:209` + `agentsession_controller.go:135`) attaches a `peerAuth=local`, `backend.kind=static` sidecar (`secret_broker.go:79`, `:127-129`). It carries **no SPIRE CSI socket**, so it *cannot* run the SPIRE-backed mint path even if configured. |
| **The egress-proxy sidecar is never injected** | the consumer of a minted credential is `pkg/agentnet/proxy.Sidecar` (`pkg/agentnet/proxy/sidecar.go:16-30`); a grep of `operator/` for `agentnet/proxy\|proxy.Sidecar\|BuildProxySidecar` returns nothing — it exists only in `cmd/spiffe-probe`. So **no datapath today both mints and injects**. |
| `AgentNetwork` names the consumer, never the producer | `CredentialInjection{Name,Scope,Header,Scheme}` (`pkg/agentmodel/v1/agentnetwork.go:91-104`) tells the proxy *which* credential to request; **nothing** tells the broker *how to mint* `name=github` (which App, which key, which `ScopePermissions`). |

**Net:** dynamic minting is a first-class library feature reachable only by writing Go. The missing pieces are (1) a declarative registration surface (CRD), (2) a `secret-proxy` config dialect for it, (3) an operator builder that renders that config into a **SPIRE-backed** broker with the root secret mounted, and (4) the controller wiring that attaches that broker on a datapath. (4) is shared with [`agentnetwork-datapath-enforcement.md`](./agentnetwork-datapath-enforcement.md) — see §4.4.

---

## 3. External interface research

Skipped — this spec is internal-only (the broker library + operator wiring already exist; the external GitHub App REST contract is documented in `pkg/secrets/backend_github.go` and the runbook, and is not changing here). New provider backends (GitLab, cloud-IAM) that *would* need external research are explicitly out of scope (§4.3, §10).

---

## 4. Design

### 4.1 Scope: cluster/namespace backend + per-Agent grant (RESOLVED)

The design doc left this open (§4.1). **Resolution:**

```
┌─────────────────────────────────────────────────────────────────────┐
│ DynamicCredentialBackend  (cluster- or namespace-scoped INFRASTRUCTURE)│
│   provider: githubApp                                                  │
│   root secret (App private key)  ── platform-team-owned, agent-blind   │
│   scopePermissions: TraT-scope → installation perms                    │
└─────────────────────────────────────────────────────────────────────┘
                    ▲ referenced by name ("github")
                    │
┌───────────────────┴───────────────────────────────────────────────────┐
│ grants (per-Agent AUTHORIZATION) — who may mint, for which scope/repo   │
│   principal: spiffe://…/ns/tenant-a/sa/agent                            │
│   scope: github:repo:read   repos: [smol-platform/app]                  │
└─────────────────────────────────────────────────────────────────────────┘
                    │  enforced at mint time by StaticCredentialPolicy
                    ▼
        (per-RUN scoping is the LEASE: ≤ MaxLeaseTTL + per-request TraT rctx)
```

- **Backend = infrastructure.** A GitHub App private key is long-lived infra, not run state. One App installed org-wide → one `DynamicCredentialBackend`. Best root-secret hygiene (one blast radius, one rotation). This is exactly how secretless egress was proven (the probe + runbook use one App).
- **Authorization = per-Agent grant.** This maps 1:1 onto `StaticCredentialPolicy.Grant(id, scope, credential, repos...)` (`pkg/secrets/credpolicy.go:31-41`), which is already keyed on the agent SPIFFE ID and already validates `rctx.repo`. No new authorization machinery — just a declarative source for the grants.
- **No per-Run backend.** Per-run isolation is already the lease (`MaxLeaseTTL`, `server.go:288-290`) + the per-request TraT `rctx` (e.g. `{"repo": "..."}`). A per-run App key is nonsensical.

**Why a separate CRD and not an `AgentNetwork` extension (design doc Option A vs B):** the root secret + App registration belong to the **platform team**; tenants own only the *consumer-side* `CredentialInjection` they already have (`agentnetwork.go:91-104`). Putting a root-secret reference on a tenant-namespaced, tenant-editable `AgentNetwork` (Option B) is weaker separation of duties. **This spec picks Option A** (the standalone CRD). The interaction with `AgentNetwork`/`AgentPolicy` ownership is examined in [`docs/design/agentnetwork-agentpolicy-interaction.md`](../design/agentnetwork-agentpolicy-interaction.md); if that doc later argues for a single egress object, the grant list (not the backend) could migrate onto it — see §10.

### 4.2 The `DynamicCredentialBackend` CRD (proposed)

A namespaced CR (recommended deployment: one dedicated `platform-secrets` namespace, RBAC-locked to the platform team). Cluster scope is rejected — see §10 for why namespaced wins.

```yaml
# (proposed) — no CRD exists yet
apiVersion: runtime.agents.smol-agents.ai/v1
kind: DynamicCredentialBackend
metadata:
  name: github
  namespace: platform-secrets        # platform-team-owned, RBAC-locked
spec:
  credentialName: github             # MUST match AgentNetwork resources[].credential.name
  provider: githubApp                # enum: githubApp (only impl today)
  githubApp:
    appID: "123456"
    privateKeyRef:                    # the ROOT secret — broker-only, agent-blind
      secretName: github-app-key
      key: private-key.pem
    baseURL: https://api.github.com   # optional; GHES override
    scopePermissions:                 # TraT scope → installation-token permissions
      "github:repo:read":  { contents: read }
      "github:repo:write": { contents: write, pull_requests: write }
  maxLeaseTTL: 5m                     # optional; capped to pkg/secrets.MaxLeaseTTL (15m)
  grants:                             # per-Agent authorization (→ StaticCredentialPolicy)
    - principal: spiffe://smol-agents.ai/ns/tenant-a/sa/agent
      scope: github:repo:read
      repos: [ smol-platform/app ]    # rctx.repo allow-list; empty = any repo the App can reach
status:
  phase: Ready                        # Ready | Pending(SecretMissing) | Failed(InvalidSpec)
  observedGeneration: 3
  grantCount: 1
  reason: ""
  message: ""
```

**Grant placement decision (open, leaning inline):** grants can live (a) inline on the backend (shown above — platform team curates everything in one object) or (b) as separate per-namespace `DynamicCredentialGrant` CRs that *reference* a backend (tenants self-serve grants, platform owns the root secret). (a) is simpler and matches the "platform owns it all" model; (b) decouples authorization churn from the root-secret object and gives tenants a self-service surface without touching the key. **This spec ships (a) first** (smallest leap, matches `StaticCredentialPolicy` directly) and lists (b) as a follow-up in §8/§10.

### 4.3 `provider` is an enum with one member today

`provider: githubApp` is the only value with a concrete `DynamicBackend` (`GitHubAppBackend`). The CRD validation rejects anything else. Adding `gitlab` / `cloudIAM` is a **future** backend + a CRD enum bump + its own external-research spec — explicitly **not** in this spec. The `githubApp:` sub-block is the only provider config block; future providers add sibling blocks (`gitlab:`, etc.), exactly one of which must be set and must match `provider`.

### 4.4 Where the broker runs: a SPIRE-backed egress broker (proposed; depends on datapath spec)

This is the load-bearing honest constraint. The dynamic mint path is **only reachable from the egress proxy** (`http.go:136`), and:

- the **run/session broker that the operator injects today is `peerAuth=local`, static-only** (`secret_broker.go:79`, `:127-129`) and carries no SPIRE socket — it physically cannot host the SPIRE-attested mint path;
- the **egress proxy sidecar is not injected by the operator at all** (`grep operator/ agentnet/proxy` → empty); it exists only in `cmd/spiffe-probe`.

So the broker that hosts a `DynamicCredentialBackend` is a **new, SPIRE-backed broker sidecar** co-located with the egress proxy, both injected by the path that [`agentnetwork-datapath-enforcement.md`](./agentnetwork-datapath-enforcement.md) is specifying. The split of responsibilities:

```
                 (this spec)                         (agentnetwork-datapath-enforcement.md)
┌──────────────────────────────────┐        ┌─────────────────────────────────────────────┐
│ DynamicCredentialBackend CRD      │        │ AgentNetwork identityProxy.resources[].       │
│   + operator builder              │ mints  │   credential  (consumer side, exists)         │
│   BuildDynamicBrokerConfig        │───────▶│   + proxy.Sidecar injection (PROPOSED there)  │
│   → secret-proxy(SPIRE) config    │        │   + SPIRE-backed broker sidecar (PROPOSED)    │
│   + root-secret Secret mount      │        │   → calls Broker.Mint over the UDS            │
└──────────────────────────────────┘        └─────────────────────────────────────────────┘
```

If `agentnetwork-datapath-enforcement.md` lands the proxy + SPIRE-broker sidecars, **this spec supplies the config those sidecars read**. The two can be developed in parallel against the contract: *the SPIRE broker reads a `secret-proxy` config whose `backend.kind: dynamic` block this spec defines.*

### 4.5 End-state config dialect (`secret-proxy backend.kind: dynamic`)

The shipped `brokerConfig` (`cmd/secret-proxy/main.go:26-47`) gains a `dynamic` arm. This is the *generated* artifact the operator renders; it is never hand-written in the declarative path (it is the analogue of `BuildBrokerConfigSecret`'s static YAML):

```yaml
# (proposed) generated secret-proxy config for a SPIRE-backed broker
socketPath: /run/secret-broker/secret-broker.sock
peerAuth: spire                       # dynamic mint REQUIRES spire (sender-constraint binds to the SVID)
workloadAPI: unix:///run/spire/agent-sockets/api.sock
maxLeaseTTL: 5m
backend:
  kind: dynamic                       # NEW arm (today: static|vault)
  dynamic:
    provider: githubApp
    credentialName: github
    githubApp:
      appID: "123456"
      privateKeyPath: /etc/broker-keys/github/private-key.pem   # mounted from privateKeyRef
      baseURL: https://api.github.com
      scopePermissions:
        "github:repo:read": { contents: read }
tts:                                  # NEW — the broker needs the JWKS to verify TraTs
  jwksUrl: https://tts.security.svc/jwks
  audience: spiffe://smol-agents.ai   # expected TraT aud (the trust domain)
credentialPolicy:                     # NEW — deny-by-default grants (→ StaticCredentialPolicy)
  - principal: spiffe://smol-agents.ai/ns/tenant-a/sa/agent
    scope: github:repo:read
    credential: github
    repos: [ smol-platform/app ]
```

A broker can carry **both** a static `backend` (for env-leased secrets) and a dynamic mint path — they are independent fields on `secrets.Server` (`Backend`/`Policy` vs `Dynamic`/`TraTVerifier`/`CredPolicy`). The probe already does exactly this (`secretless.go:81-96`: static `Backend`+`Policy` *and* `Dynamic`+`TraTVerifier`+`CredPolicy`). The config grammar must therefore allow both arms simultaneously; treat `backend.kind` as the *static* backend selector and add `backend.dynamic` as an **independent optional block** (renaming is avoided to keep backward compat — see §5.2).

---

## 5. Concrete changes

### 5.1 New CRD types (proposed)

**New file `operator/api/agentmodel/v1/dynamiccredentialbackend.go`** — the K8s wrapper, registered in the existing `runtime.agents.smol-agents.ai/v1` group (`groupversion_info.go:23`). Pattern mirrors `agentnetwork.go:9-38` (wrap a pure spec, `SchemeBuilder.Register` in `init()`).

**New pure spec `pkg/agentmodel/v1/dynamiccredentialbackend.go`** (no-K8s-deps package, same split rationale as `groupversion_info.go:5-9`):

```go
// (proposed) pkg/agentmodel/v1/dynamiccredentialbackend.go
type CredentialProvider string
const ProviderGitHubApp CredentialProvider = "githubApp"

type DynamicCredentialBackendSpec struct {
    // CredentialName MUST match AgentNetwork resources[].credential.name.
    CredentialName string `json:"credentialName"`
    // +kubebuilder:validation:Enum=githubApp
    Provider  CredentialProvider `json:"provider"`
    GitHubApp *GitHubAppBackendSpec `json:"githubApp,omitempty"` // required when provider=githubApp
    // +optional  (capped to pkg/secrets.MaxLeaseTTL = 15m)
    MaxLeaseTTL metav1.Duration `json:"maxLeaseTTL,omitempty"`
    Grants []CredentialGrantSpec `json:"grants,omitempty"`
}

type GitHubAppBackendSpec struct {
    AppID         string            `json:"appID"`
    PrivateKeyRef AuthRef           `json:"privateKeyRef"`           // reuse pkg/agentmodel/v1 AuthRef{SecretName,Key}
    BaseURL       string            `json:"baseURL,omitempty"`       // GHES override
    ScopePermissions map[string]map[string]string `json:"scopePermissions,omitempty"` // TraT scope → perms
}

type CredentialGrantSpec struct {
    Principal string   `json:"principal"`            // SPIFFE ID string
    Scope     string   `json:"scope"`                // TraT scope
    Repos     []string `json:"repos,omitempty"`      // rctx.repo allow-list; empty = any
}

type DynamicCredentialBackendStatus struct {
    Phase, Reason, Message string
    ObservedGeneration     int64
    GrantCount             int32
}

func ValidateDynamicCredentialBackend(s DynamicCredentialBackendSpec) error { /* … */ }
```

- **`AuthRef`** already exists in the pure package (used by `WireGuardSpec.PrivateKeyRef`, `agentnetwork.go:195`) — reuse it for `privateKeyRef`.
- **Validation** (`ValidateDynamicCredentialBackend`): `credentialName` non-empty; `provider` ∈ enum; exactly the matching provider block set; each grant has a parseable SPIFFE `principal` + non-empty `scope`; `maxLeaseTTL` (if set) > 0 (the broker caps it down to 15m regardless, `server.go:288-290`). Mirror the `ValidateAgentNetwork` error-join style (`agentnetwork.go:253-279`).
- **Deepcopy + CRD manifest + RBAC**: regenerate via the existing controller-gen path (`make -C operator deepcopy` / `manifests`). **Heed the known CRD-generation drift** (`operator/config/crd` is not blindly reproducible from source — hand-verify the generated manifest, per the project memory note).

### 5.2 `cmd/secret-proxy` config + wiring (proposed)

**`cmd/secret-proxy/main.go`:**

1. Extend `brokerConfig` (`:26-47`) with an **independent** dynamic block and a `tts`/`credentialPolicy` block (do **not** repurpose `backend.kind` — keep it as the static selector so existing static configs are byte-compatible):
   ```go
   Backend struct {
       Kind    string `yaml:"kind"`            // static | vault   (unchanged)
       Static  []…    `yaml:"static"`
       Dynamic *struct {                        // NEW (optional, independent of Kind)
           Provider       string            `yaml:"provider"`
           CredentialName string            `yaml:"credentialName"`
           GitHubApp      *struct {
               AppID            string                       `yaml:"appID"`
               PrivateKeyPath   string                       `yaml:"privateKeyPath"`
               BaseURL          string                       `yaml:"baseURL"`
               ScopePermissions map[string]map[string]string `yaml:"scopePermissions"`
           } `yaml:"githubApp"`
       } `yaml:"dynamic"`
   } `yaml:"backend"`
   TTS *struct {
       JWKSURL  string `yaml:"jwksUrl"`
       Audience string `yaml:"audience"`
   } `yaml:"tts"`
   CredentialPolicy []struct {
       SPIFFEID, Scope, Credential string
       Repos                       []string
   } `yaml:"credentialPolicy"`
   ```
2. New `buildDynamic(cfg) (secrets.DynamicBackend, trat.Verifier, secrets.CredentialPolicy, error)`:
   - load the PEM at `privateKeyPath` → `*rsa.PrivateKey`; construct `&secrets.GitHubAppBackend{AppID, PrivateKey, BaseURL, ScopePermissions}` (`backend_github.go:22-33`);
   - construct `&trat.JWKSVerifier{Keys: &trat.HTTPKeySource{URL: cfg.TTS.JWKSURL}, Audience: cfg.TTS.Audience}` (matches `secretless.go:94`);
   - build `secrets.NewStaticCredentialPolicy()` and `.Grant(id, scope, credential, repos...)` per entry (`credpolicy.go:31`).
3. Set `srv.Dynamic / srv.TraTVerifier / srv.CredPolicy` when the dynamic block is present (`main.go:86-94`). **Refuse to start if `backend.dynamic` is set but `peerAuth != spire`** — the sender-constraint (`server.go:262-265`) binds the TraT to a SPIRE SVID; `peerAuth=local` would let any uid-65532 process mint, defeating per-workload isolation. This guard is new and load-bearing (see §7).

`buildBackend` (`:161-180`) and `buildPolicy` (`:182-192`) stay unchanged (static path untouched).

### 5.3 Operator builder (proposed)

**New file `operator/internal/builders/dynamic_broker.go`** — the dynamic analogue of `secret_broker.go`:

- `BuildDynamicBrokerConfigSecret(b *amv1.DynamicCredentialBackend) (*corev1.Secret, error)`: render the §4.5 YAML (a `Secret`, since it holds the App ID + JWKS URL but **not** the private key — the key is mounted separately by ref so it never round-trips through this object).
- `AttachDynamicBroker(pod, backendRef, privateKeyRef)`: add (a) the SPIRE CSI socket volume + mount (so `peerAuth=spire` works — unlike the static run broker), (b) the config Secret volume, (c) the **root-secret volume from `privateKeyRef`** mounted **only** into the broker container at `/etc/broker-keys/<name>/`, **never** into the agent container (mirror the deliberate "UDS not mounted into other sidecars" comment, `secret_broker.go:48`), (d) the SPIRE-backed `secret-proxy` sidecar. This is **co-injected with the egress proxy** by the datapath spec's controller path — it is *not* the local run broker.

> **Note (proposed):** `AttachDynamicBroker` is a distinct sidecar from `AttachSecretBroker`. A pod could carry **both** (a local static broker for env leases + a SPIRE dynamic broker for egress mint) on different sockets, or the datapath spec may unify them into one SPIRE broker that serves both. Unification is preferable (one broker process) but requires the run broker to gain a SPIRE socket; that trade-off belongs to [`agentnetwork-datapath-enforcement.md`](./agentnetwork-datapath-enforcement.md). This spec assumes the **separate-sidecar** shape and notes unification as an open decision (§10).

### 5.4 Controller (proposed)

**New file `operator/internal/controllers/agentmodel/dynamiccredentialbackend_controller.go`** — a `DynamicCredentialBackendReconciler` modeled on `AgentNetworkReconciler` (`agentnetwork_controller.go:39-153`):

- `Reconcile`: validate spec; resolve `privateKeyRef` — **fail-fast `Pending: SecretMissing`** if the key Secret is absent (mirror the WireGuard fail-fast, `agentnetwork_controller.go:100-112`); set `Status.{Phase,GrantCount,ObservedGeneration}`; `statusUpdateIfChanged` (`:148-153`).
- `SetupWithManager`: `For(&DynamicCredentialBackend{})` + `Watches(&Secret{})` mapping a key-Secret event back to the backends that ref it (so a freshly-created key Secret flips `Pending → Ready` without a spec bump — exactly the `secretToAgentNetworks` pattern, `:54-71`).
- This controller **does not inject pods**; it validates + reports readiness. The injection happens in the datapath controller that consumes the rendered config (§4.4). (Same division as `AgentNetworkReconciler`, which validates but "does NOT inject sidecars or program egress", `agentnetwork_controller.go:26-28`.)

### 5.5 Library API: unchanged (compat guarantee)

`pkg/secrets.Server` fields (`server.go:25-46`), `DynamicBackend`/`CredentialPolicy`/`CredentialRequest` (`types.go:59-93`), `GitHubAppBackend` (`backend_github.go`), `StaticCredentialPolicy` (`credpolicy.go`) are **untouched**. The runbook's inline-Go path and all `pkg/secrets` tests keep compiling and passing. The CRD/config layer is purely additive.

---

## 6. Data / control flow

### 6.1 Config generation (proposed, build/admission time)

```
DynamicCredentialBackend (platform-secrets ns)
   │  DynamicCredentialBackendReconciler: validate + resolve privateKeyRef
   │  → Status=Ready
   ▼
(datapath controller, agentnetwork-datapath-enforcement.md) injects, for an Agent
bound to an AgentNetwork whose resources[].credential.name == backend.credentialName:
   ├─ BuildDynamicBrokerConfigSecret(backend)  → Secret(config.yaml)   [no private key in it]
   ├─ mount privateKeyRef Secret → /etc/broker-keys/<name>/  [broker container ONLY]
   ├─ mount SPIRE CSI socket → /run/spire/...                [so peerAuth=spire works]
   └─ attach secret-proxy(SPIRE) sidecar + agentnet proxy sidecar
```

### 6.2 Runtime mint (exists today; unchanged by this spec)

```
agent HTTP client → http://127.0.0.1:<localPort>          (agentnet proxy listener)
   proxy mints TraT (JWT-SVID → RFC 8693 exchange)         pkg/trat ExchangeMinter
   proxy → Broker.Mint(name, trat) over UDS                http.go:136 → Client.Mint
      broker.handle: SO_PEERCRED attest → SPIFFE SVID       server.go:144-149
      handleMint:                                           server.go:242-296
        Dynamic/TraTVerifier/CredPolicy non-nil?  ───────── now TRUE (was nil → reject)
        TraTVerifier.Verify(trat): sig+aud+exp              verifier.go:35
        req_wl == principal.String()  (sender-constraint)   server.go:262-265
        CredPolicy.AuthorizeMint: (principal,scope,cred)    credpolicy.go:43-62
            + rctx.repo ∈ grant.repos
        Dynamic.Mint: GH App JWT → installation → token     backend_github.go:58-81
        cap lease to MaxLeaseTTL                             server.go:288-290
   proxy injects Authorization: Bearer <token> upstream     http.go:138-141  (agent never sees it)
   eBPF drops egress to anything but the allow-listed host  (agentnetwork-datapath-enforcement.md)
```

The single change at runtime is the **first branch**: the three `Server` fields are now populated (from generated config) instead of nil, so `handleMint` proceeds instead of returning `"dynamic credential minting not configured"` (`server.go:243-245`).

---

## 7. Security model

How the declarative layer composes with the existing controls — and what new surface it adds.

| Layer | Composition | Notes |
|---|---|---|
| **kata-fc sandbox** | unchanged | the SPIRE broker + proxy run in the same microVM as the agent (run/session pods pin kata-fc by default, `operator/cmd/manager/main.go --default-run-runtime-class`); the root key file is inside the guest, never on the agent's reachable filesystem (broker-only mount, §5.3). |
| **egress NetworkPolicy / eBPF** | unchanged + reinforced | a minted token can only leave toward the allow-listed host; a leaked token is useless off-path. eBPF allow-list is the datapath spec's concern. |
| **SO_PEERCRED + SPIRE attestation** | **required** for dynamic | the §5.2 guard refuses `backend.dynamic` unless `peerAuth=spire`. The sender-constraint (`server.go:262-265`) is meaningless under `peerAuth=local` (every in-pod process is uid 65532 → all share one synthetic SVID). Dynamic mint **must** be SPIRE-backed. |
| **TraT sender-constraint** | unchanged | `req_wl == principal` binds the mint to the attested SVID; `AuthorizeMint` + `rctx.repo` allow-list bound the blast radius. Generated grants don't weaken this — they are the same `StaticCredentialPolicy.Grant` calls, just sourced declaratively. |
| **broker SPIFFE identity** | new requirement | the broker reading the root key must itself be a SPIRE workload (a `ClusterSPIFFEID` selecting the broker container) so the SPIRE CSI hands it an SVID. |

### New attack surface + mitigations

1. **Root-secret exposure via the CRD object.** *Mitigation:* the private key is **never inlined** and **never** written into the generated config Secret — only `privateKeyRef` is, and the key Secret is mounted **only** into the broker container (§5.3). RBAC-lock the `platform-secrets` namespace so only the platform team can create/read `DynamicCredentialBackend` and the key Secret.
2. **Tenant privilege escalation via a forged grant.** A tenant who could edit the backend's `grants` could authorize their own SPIFFE ID for a scope/repo they shouldn't reach. *Mitigation:* the backend (incl. grants, in the v1 inline-grant model) lives in the platform-team namespace; tenants have no write access. The separate-`DynamicCredentialGrant` model (§4.2 option b, deferred) would need its own admission gate to prevent a tenant granting *another* tenant's identity — flagged in §10.
3. **`peerAuth` downgrade.** Generating a dynamic config against a `peerAuth=local` broker would silently drop per-workload isolation. *Mitigation:* the §5.2 startup guard (fail-closed) + the operator builder only ever emits `peerAuth: spire` for dynamic brokers.
4. **Audit blind spot.** The broker logs mint *failures* with principal/scope/credential (`server.go:254`, `:263`, `:276`, `:281`) but **not successes**. *Mitigation (proposed):* add a structured success audit event (who minted what credential for which TraT `txn`/`rctx`, no value) emitted to the platform audit sink — see §8 P4. This is the one genuinely missing observability piece.
5. **Stale grant after revocation.** Removing a grant from the CRD blocks *future* mints/refreshes (the policy re-validates each call, `server.go:232-234`), but an already-minted installation token stays valid upstream until its own ~1h expiry. *Mitigation:* short `MaxLeaseTTL` (≤15m, capped) is the de-facto revocation window. No active provider-side revoke today; flagged §10.

### Honest limit (carried from the design doc §6)

Agent-blindness holds for the **opaque HTTP proxy path** only. A CLI harness that reads the *static* env-leased secrets (`pkg/agentruntime/harness/cli.go` mergeEnv) is **not** blind to those — but that is the static path, not this dynamic mint path. The dynamic mint is reached **only** by the proxy (`http.go:136`); no CLI harness calls `Mint`. So this spec's credentials are agent-blind **iff** the consuming proxy is the only `Mint` caller — which it is. Routing a CLI's egress through that same proxy seam is a [`harness-authoring.md`](../design/harness-authoring.md) / [`custom-agent-images.md`](../design/custom-agent-images.md) concern.

---

## 8. Phasing & effort

| Phase | Scope | Effort | Depends on |
|---|---|---|---|
| **P1 — `secret-proxy` dynamic config dialect** | Extend `brokerConfig`; `buildDynamic`; wire `Server.{Dynamic,TraTVerifier,CredPolicy}`; the `peerAuth=spire` startup guard (§5.2). Unit-test the parse + wiring against the same fakes the probe uses. **Makes the runbook's inline-Go unnecessary for an operator who writes the config by hand.** | **M** | — (pure binary change; library unchanged) |
| **P2 — CRD types + validation + controller** | `DynamicCredentialBackend` pure + K8s types (§5.1); `ValidateDynamicCredentialBackend`; deepcopy + CRD manifest + RBAC; `DynamicCredentialBackendReconciler` (validate + readiness + key-Secret watch, §5.4). No injection yet. | **L** | P1 (config shape) |
| **P3 — operator builder + injection** | `BuildDynamicBrokerConfigSecret` + `AttachDynamicBroker` (§5.3); co-inject the SPIRE broker with the egress proxy on the bound-Agent datapath; root-key broker-only mount; SPIRE CSI socket. **This is where it becomes end-to-end declarative.** | **L** | P2 **and** [`agentnetwork-datapath-enforcement.md`](./agentnetwork-datapath-enforcement.md) (the proxy sidecar + SPIRE broker injection it specifies) |
| **P4 — success audit + rotation hook** | Structured mint-success audit event (§7 #4); root-key rotation (reload PEM on Secret change → broker hot-reload or pod-restart-on-key-version). | **M** | P3 |
| **P5 (deferred) — separate `DynamicCredentialGrant` CR + 2nd provider** | tenant self-service grants (§4.2 option b) with its own admission gate; a `gitlab`/`cloudIAM` `DynamicBackend` + external-research spec. | **XL** | P2; new provider needs its own spec |

**Dependency call-outs:** P3 cannot ship a real end-to-end datapath without [`agentnetwork-datapath-enforcement.md`](./agentnetwork-datapath-enforcement.md) (no proxy/SPIRE broker is injected today). P1+P2 are independently shippable and immediately useful (they remove the inline-Go requirement and give a declarative readiness surface). Output-disclosure interaction with [`agentpolicy-enforcement.md`](./agentpolicy-enforcement.md) is orthogonal (that doc redacts the *cluster record*; this never puts the credential there).

---

## 9. Test plan

### Unit (per phase)

- **P1 config:** table-test `loadBrokerConfig` + `buildDynamic` — a `backend.dynamic` block produces a `GitHubAppBackend` with the right `AppID`/`ScopePermissions`, a `JWKSVerifier` with the right audience, and a `StaticCredentialPolicy` with the right grants. **Negative:** `backend.dynamic` set + `peerAuth: local` → startup error (the §5.2 guard). Reuse the in-process broker + fakes pattern from `cmd/spiffe-probe/secretless.go:60-119` (real `Server.handleMint`, fake GitHub + fake TTS).
- **P1 end-to-end-in-process:** start a `Server` built **from generated config** (not hand-constructed), drive a `Client.Mint` with a TraT minted for the attested SVID, assert a token comes back and the static `handleMint` denial path is no longer hit. Mirror `pkg/secrets/mint_test.go` / `backend_github_test.go`.
- **P2 validation:** `ValidateDynamicCredentialBackend` rejects: empty `credentialName`, unknown `provider`, missing `githubApp` block when `provider=githubApp`, unparseable grant `principal`, empty grant `scope`. Property-style if the existing `operator/api/v1/property_test.go` harness fits.
- **P2 controller:** envtest — create a `DynamicCredentialBackend` with a missing key Secret → `Pending: SecretMissing`; create the Secret → reconcile flips to `Ready` (the watch path); `grantCount` reflects spec. Mirror the AgentNetwork reconciler tests.
- **P3 builder:** `BuildDynamicBrokerConfigSecret` emits config with **no private key bytes** in it; `AttachDynamicBroker` mounts the key volume **only** into the broker container and the SPIRE CSI socket; `peerAuth: spire` in the rendered config. Assert the key is *not* mounted into the agent container (the critical agent-blindness invariant).

### E2E (cftest single-node k0s box exists for live verification)

- **L0 (already green, regression guard):** `cmd/spiffe-probe/secretless.go` proves the mint path against fakes. Keep it; add a variant that boots the broker **from a generated config file** instead of inline Go, so the config dialect is exercised on the real `handleMint`.
- **L2 (cftest k0s, P3):** deploy a `DynamicCredentialBackend` (githubApp against a real or `cmd/fake-github` App), an `AgentNetwork` with a `credential` resource, and an Agent; assert an `AgentRun` whose harness/tool reaches `http://127.0.0.1:<port>` gets a working repo-scoped token, the agent container has **no** key file, and a denied scope/repo fails closed. This composes with the datapath spec's e2e (they share the injection path). The live CF-tunnel deploy + 24h-token recipe is in the project memory (`cf_tunnel_deploy`).

---

## 10. Risks & open decisions

**Open decisions (maintainer must choose):**

1. **Inline grants vs. separate `DynamicCredentialGrant` CR.** §4.2: ship inline grants first (simpler, matches `StaticCredentialPolicy`), or invest in a tenant-self-service grant CR up front? Inline keeps everything platform-owned; separate decouples authorization churn but **needs an admission gate** so a tenant can't grant another tenant's SPIFFE ID. **Recommendation:** inline for P2, revisit after [`agentnetwork-agentpolicy-interaction.md`](../design/agentnetwork-agentpolicy-interaction.md) settles ownership.
2. **One broker or two.** §5.3: keep a separate SPIRE dynamic broker alongside the local static run broker, or unify into one SPIRE-backed broker that serves both static leases *and* dynamic mint? Unification is cleaner (one process) but means the run broker gains a SPIRE CSI socket it currently lacks (`secret_broker.go:127`) — a datapath-spec decision.
3. **CRD scope: Namespaced (recommended) vs Cluster.** Namespaced (in a locked `platform-secrets` ns) gives normal RBAC + a clear owner and matches every other CRD in the group (`agentnetwork.go:13` is `Namespaced`). Cluster-scope would centralize but complicates RBAC and cross-namespace secret refs. **Recommendation: Namespaced.**
4. **Config-grammar shape.** §4.5/§5.2: keep `backend.kind` as the static selector and add `backend.dynamic` as an independent block (chosen, backward-compatible), vs. a cleaner `backend.kind: dynamic` that would break the "both arms at once" case the probe needs. Chosen the additive path; confirm before implementing.

**Risks / honest unknowns:**

- **The datapath dependency is real and blocking for end-to-end.** Without [`agentnetwork-datapath-enforcement.md`](./agentnetwork-datapath-enforcement.md) injecting the proxy + SPIRE broker, P3 has nothing to attach to. P1/P2 still deliver value (declarative config + readiness) but the credential isn't *consumed* by any injected datapath until that spec lands. This is the single biggest risk and is called out in the status banner.
- **CRD-generation drift.** The project's `operator/config/crd` is **not** reproducible by blindly running `make manifests` (project memory `crd_generation_drift`); the new manifest must be hand-verified, and the SmolAgent-group mismatch noted there is a reminder that group/manifest skew is a known failure mode.
- **No provider-side revocation.** Removing a grant blocks future mints but not already-issued tokens (≤15m window). Acceptable for short-TTL installation tokens; would be a real gap for longer-lived credentials a future provider might mint.
- **Root-key rotation is manual (until P4).** Rotating the GitHub App key today means re-creating the Secret + restarting the broker pod; P4's reload hook is not built.
- **Success audit is the one missing security-relevant observability piece** (§7 #4) — until P4, only denials are reliably visible in broker logs.

## 11. Cross-links

- [`docs/design/secrets-broker-credential-backends.md`](../design/secrets-broker-credential-backends.md) — the design doc this implements (both backend shapes, attestation model, scope framing).
- [`docs/specs/agentnetwork-datapath-enforcement.md`](./agentnetwork-datapath-enforcement.md) — the **transport** dependency: injects the egress proxy + (proposed) SPIRE broker that *consume* the minted credential.
- [`docs/features/egress-credentials.md`](../features/egress-credentials.md) — the consumer side (proxy injection, the three controls, the `AgentNetwork` `credential` block).
- [`docs/features/runtime-and-identity.md`](../features/runtime-and-identity.md) — the broker in the wider SPIFFE/lease story.
- [`docs/runbooks/secretless-egress.md`](../runbooks/secretless-egress.md) — the **inline-Go** configuration this spec makes declarative.
- [`docs/design/agentnetwork-agentpolicy-interaction.md`](../design/agentnetwork-agentpolicy-interaction.md) — ownership/authorization model that informs the inline-vs-separate-grant decision (§10 #1).
- [`docs/specs/agentpolicy-enforcement.md`](./agentpolicy-enforcement.md) — orthogonal disclosure control on the cluster record.
- [`docs/design/harness-authoring.md`](../design/harness-authoring.md), [`docs/design/custom-agent-images.md`](../design/custom-agent-images.md) — where routing a CLI's egress through the proxy seam (§7 honest limit) is actionable.
- [`docs/research/agent-runtime-fit-analysis-v0.2.0.md`](../research/agent-runtime-fit-analysis-v0.2.0.md) — runtime-isolation context for the agent-blindness boundary.
