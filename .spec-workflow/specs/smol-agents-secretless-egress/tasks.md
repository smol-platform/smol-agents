# Tasks — smol-agents-secretless-egress

Depends on **smol-agents-trat-egress** (TraT minting + the egress injection seam).

- [x] 1. Author spec (requirements/design/tasks)
  - Files: this directory
  - _Requirements: all_

- [x] 2. Broker dynamic-mint path
  - Files: pkg/secrets/{types.go (CredentialRequest, DynamicBackend), server.go
    (Mint RPC: attest + verify TraT via TTS JWKS + policy + backend), client.go
    (Mint call)}, secrets_test.go
  - Deny-by-default; lease cap MaxLeaseTTL; never log values.
  - _Requirements: R-SEGR-MINT-1, R-SEGR-AUTH-1, R-SEGR-SEC-1_

- [x] 3. GitHub App backend
  - Files: pkg/secrets/backend_github.go (+ test against a fake GitHub API)
  - App-JWT → installation access token; scope repositories/permissions from
    CredentialRequest.Scope/RequestContext per policy; App key from broker/mount.
  - _Requirements: R-SEGR-MINT-2_

- [x] 4. Broker credential policy
  - Files: pkg/secrets policy types (principal+scope → credential+backend+scoping
    + repo allow-list), policy_test.go
  - _Requirements: R-SEGR-API-2, R-SEGR-AUTH-1_

- [x] 5. Proxy credential-injection mode
  - Files: pkg/agentnet/proxy/http.go (Director: TraT → broker.Mint → inject
    header; cache+refresh; fail-closed; value never returned to agent),
    sidecar.go (wire broker client + trat client), proxy_test.go
  - _Requirements: R-SEGR-INJECT-1, R-SEGR-SEC-1_

- [x] 6. AgentNetwork CRD additions + validation
  - Files: pkg/agentmodel/v1/agentnetwork.go (CredentialInjection + validate:
    http-only, dest covered by allow-list), agentnetwork_test.go, runtime CRD
    yaml, sample CR
  - _Requirements: R-SEGR-API-1, R-SEGR-EBPF-1_

- [x] 7. Security tests (the whole point)
  - Files: pkg/secrets + proxy tests
  - Forged/unsigned TraT → deny; rctx repo outside allow-list → deny;
    agent-facing UDS never returns a dynamic credential; minted value absent
    from logs.
  - _Requirements: R-SEGR-AUTH-1, R-SEGR-SEC-1_

- [x] 8. e2e: fake-github + R-E2E-SCN-SECRETLESS (code+unit done; full-stack ring run deferred — needs TTS + broker dynamic backend deployed)
  - Files: cmd/fake-github (+ multiarch Dockerfile), test/e2e/manifests/
    fake-github.yaml, shared/scenarios.go (runSecretlessEgress), coverage.go
  - Assert upstream sees injected token; agent can't read it; eBPF drops egress
    to a disallowed host with credential set.
  - _Requirements: R-SEGR-INJECT-1, R-SEGR-EBPF-1, R-SEGR-SEC-1_

- [x] 9. Quint invariant
  - File: spec/quint/secretless_egress.qnt — "minted credential injected only on
    policy-permitted egress AND only with a valid authorizing TraT"
  - _Requirements: R-SEGR-AUTH-1, R-SEGR-EBPF-1_

- [x] 10. Docs
  - Files: docs/runbooks/secretless-egress.md (register a GitHub App, broker
    policy, an AgentNetwork credential resource, verify the agent never holds the
    token), INSTALL.md prereqs
  - _Requirements: all_

## Out of P1 (tracked)
- [ ] Vault dynamic-secret + cloud STS backends; GitLab backend
- [ ] Request-derived rctx (per outbound method/path/repo)
- [ ] Tokenetes accessEvaluation/azdMapping driving the per-mint scoping
