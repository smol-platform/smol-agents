# Runbook — Secretless egress (TraT-authorized credential injection)

When an agent calls `https://api.github.com`, the identity-aware sidecar mints
a short-lived provider credential *for that one request* and injects it. The
agent never holds a long-lived token, and eBPF drops any egress that isn't on
the allow-list, so a leaked credential can't be exfiltrated.

The chain is **token exchange all the way down**:

```
agent JWT-SVID ──(RFC 8693)──▶ Txn-Token (internal intent)
                                   │  verified by the broker:
                                   │    sig + aud + exp + req_wl == caller
                                   ▼
                          GitHub App installation token (external, ~1h, repo-scoped)
                                   │  injected as Authorization: Bearer …
                                   ▼
                              api.github.com   (eBPF allow-lists this host only)
```

Components: `pkg/trat` (mint/verify), `pkg/secrets` (broker + `GitHubAppBackend`
+ `StaticCredentialPolicy`), `pkg/agentnet/proxy` (injection), `pkg/agentmodel/v1`
(`AgentNetwork` `tts`/`credential` fields). Spec:
[`.spec-workflow/specs/smol-agents-secretless-egress`](../../.spec-workflow/specs/smol-agents-secretless-egress).
Formal model: [`spec/quint/secretless_egress.qnt`](../../spec/quint/secretless_egress.qnt).

## Prerequisites

1. **SPIRE** issuing JWT-SVIDs to agents (all rings already run this).
2. **A TTS** (Tokenetes Transaction Token Service, or any RFC 8693 endpoint that
   returns `txn_token`s) reachable in-cluster, plus its JWKS URL. The sidecar
   exchanges the agent's JWT-SVID for a Txn-Token; the broker verifies the
   Txn-Token against the JWKS.
3. **The secret broker** running with the dynamic-mint path configured
   (`Dynamic`, `TraTVerifier`, `CredPolicy` all non-nil) — see below.
4. **eBPF egress enforcement** (`CapEBPF`) so the credential can only travel to
   allow-listed hosts.

## 1. Register a GitHub App

The broker mints **installation access tokens**, not personal tokens.

1. Create a GitHub App (org → Settings → Developer settings → GitHub Apps).
   - **Permissions**: grant only what agents need, e.g. *Contents: Read-only*.
   - Note the **App ID**.
   - Generate a **private key** (PEM) and download it.
2. **Install** the App on the org/repos the agents may touch. The broker
   resolves the installation per repo at mint time
   (`GET /repos/{owner}/{repo}/installation`).
3. Store the App private key where the broker can read it **in memory only** —
   a mounted Secret or a static backend entry. It is the root secret: never log
   it, never write it to disk in the agent's sandbox.

## 2. Configure the broker dynamic backend + policy

The broker needs three things set (`pkg/secrets.Server`):

```go
attestor, _ := secrets.NewSPIREPeerAttestor("unix:///run/spire/agent.sock")
srv := &secrets.Server{
    SocketPath:  "/run/smol/broker.sock",
    MaxLeaseTTL: 5 * time.Minute,        // minted tokens are capped to this
    Attestor:    attestor,               // SO_PEERCRED → SPIFFE id
    Backend:     secrets.NewStaticBackend(),      // static leases (unchanged)
    Policy:      secrets.NewStaticPolicy(),       // static leases (unchanged)

    // --- secretless egress (dynamic) ---
    Dynamic: &secrets.GitHubAppBackend{
        AppID:      "123456",
        PrivateKey: appKey, // *rsa.PrivateKey, loaded in memory
        ScopePermissions: map[string]map[string]string{
            "github:repo:read": {"contents": "read"},
        },
    },
    TraTVerifier: &trat.JWKSVerifier{
        Keys:     &trat.HTTPKeySource{URL: ttsJWKSURL},
        Audience: "spiffe://stigen.ai", // expected TraT audience (trust domain)
    },
    CredPolicy: credPolicy, // built below
}
```

The **credential policy is deny-by-default**. Grant each agent SPIFFE id the
right to mint a named credential under a TraT scope, constrained to a repo
allow-list:

```go
credPolicy := secrets.NewStaticCredentialPolicy()
credPolicy.Grant(
    spiffeid.RequireFromString("spiffe://stigen.ai/ns/tenant-a/sa/agent"),
    "github:repo:read", // TraT scope that authorizes this
    "github",           // credential name (matches AgentNetwork credential.name)
    "stigen/app",       // repo allow-list; the TraT's rctx.repo must be in here
)
```

Two independent gates must both pass before the backend is ever called:

- the **TraT** is signature-valid, unexpired, audienced at the trust domain, and
  its `req_wl` equals the SO_PEERCRED-attested caller (anti-replay); and
- the **policy** authorizes `(caller, scope, credential)` and the TraT's
  `rctx.repo` is on the grant's allow-list.

Any failure → `ErrUnauthorized`, the backend is **not** invoked, and nothing is
minted (fail-closed).

## 3. Declare the credential resource on the AgentNetwork

Sample: [`operator/config/samples/agentnetwork_secretless_github.yaml`](../../operator/config/samples/agentnetwork_secretless_github.yaml).

```yaml
spec:
  kind: identityProxy
  identityProxy:
    tts:
      url:     https://tts.security.svc/token   # RFC 8693 token-exchange
      jwksUrl: https://tts.security.svc/jwks     # required for credential resources
      subjectTokenType: urn:ietf:params:oauth:token-type:jwt
      subjectAudience:  spiffe://stigen.ai/ns/security/sa/tts
    resources:
      - name: github
        kind: http                                # credentials are http-only
        localPort: 9200
        gateway: https://api.github.com/
        jwtAudience: spiffe://stigen.ai/ns/tenant-a/sa/agent
        credential:
          name:  github                           # → broker credential/policy key
          scope: github:repo:read                 # → authorizing TraT's intent
          # header/scheme default to Authorization / Bearer
    egress:
      enforcement: ebpfBoth
      redirectCIDRs: [ 0.0.0.0/0 ]
      allow:
        - { cidr: 140.82.112.0/20, protocol: tcp, ports: [443] }  # api.github.com only
```

The agent points its GitHub client at `http://127.0.0.1:9200` (the sidecar's
local listener) — **not** at `api.github.com` directly. The sidecar mints,
injects, and forwards.

## 4. Verify the agent never holds the token

```bash
# 1. From inside the agent container, the call works WITHOUT any token in env.
kubectl exec -n tenant-a deploy/agent -c agent -- \
  sh -c 'env | grep -i -E "github|token" || echo "no token in env ✔"'
kubectl exec -n tenant-a deploy/agent -c agent -- \
  curl -s http://127.0.0.1:9200/repos/stigen/app | head

# 2. The agent's own request carries no Authorization — the SIDECAR adds it.
#    Confirm by pointing curl straight at GitHub (bypassing the sidecar):
kubectl exec -n tenant-a deploy/agent -c agent -- \
  curl -s -o /dev/null -w '%{http_code}\n' https://api.github.com/repos/stigen/app
#    → 401/403 (no creds) AND/OR dropped by eBPF (host not on the agent's path).

# 3. Exfiltration attempt to a non-allow-listed host is dropped by eBPF.
kubectl exec -n tenant-a deploy/agent -c agent -- \
  curl -s --max-time 5 https://evil.example/  ; echo "exit=$?"   # non-zero (dropped)
```

What you should observe: the agent has **no GitHub token** in its environment,
filesystem, or request headers; the upstream (GitHub) sees a valid short-lived
installation token; and any egress off the allow-list never leaves the node.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Sidecar returns **503** on the resource | TraT or mint failed (fail-closed) | Check broker logs for `trat invalid` / `trat not bound to caller` / `mint policy denied`. The control headers `X-Agentnet-{TraT,Cred}-Error` carry the reason internally (never forwarded upstream). |
| `trat not bound to caller` | TTS `req_wl` ≠ the agent's SPIFFE id | Ensure the TTS sets `req_wl` from the JWT-SVID subject; the broker enforces `req_wl == attested caller`. |
| `repo … not allow-listed` | TraT `rctx.repo` outside the grant | Add the repo to the matching `credPolicy.Grant(...)`. |
| GitHub `404` on installation | App not installed on that repo | Install the App on the owner/repo. |
| Token TTL longer than expected | provider token > `MaxLeaseTTL` | The broker caps `ExpiresAt` to `MaxLeaseTTL`; lower it if needed. |

## Security invariants (enforced + proven)

- A credential is injected **only** with a valid authorizing TraT bound to the
  caller **and** only onto eBPF-permitted egress
  (`spec/quint/secretless_egress.qnt`, `make verify-formal`).
- The minted value is never returned over the agent-facing path, never logged,
  and the authorizing TraT is internal-only (never sent upstream)
  (`pkg/agentnet/proxy` `TestHTTPProxy_InjectsCredential_AgentBlind`,
  `pkg/secrets` `TestServer_Mint_*`).
- Mint is fail-closed end to end: any TraT/policy/backend failure yields 503 and
  no upstream request (`R-SEGR-SEC-1`).
