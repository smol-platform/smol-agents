# Tasks — smol-agents-trat-egress

- [ ] 1. Author spec (requirements/design/tasks)
  - Files: this directory
  - _Requirements: all_

- [ ] 2. `pkg/trat` TraT client (token-exchange + cache)
  - Files: pkg/trat/{client,exchange,cache}.go (+ client_test.go with a fake TTS)
  - RFC 8693 token-exchange POST; JWT-SVID subject_token; mTLS via X509-SVID;
    cache by (sub, scope, audience) with exp-skew refresh; never persist/log.
  - _Requirements: R-TRAT-MINT-1, R-TRAT-MINT-2_

- [ ] 3. AgentNetwork CRD additions + validation
  - Files: pkg/agentmodel/v1/agentnetwork.go (TraTInjection, TTSRef,
    validateIdentityProxy), pkg/agentmodel/v1/agentnetwork_test.go,
    operator/api/agentmodel/v1 wrapper + zz_generated, the runtime
    AgentNetwork CRD yaml, a sample CR
  - _Requirements: R-TRAT-API-1, R-TRAT-API-2_

- [ ] 4. HTTPProxy TraT injection
  - Files: pkg/agentnet/proxy/http.go (Director sets Txn-Token; jwtTransport
    fail-closed on trat error), pkg/agentnet/proxy/sidecar.go (pass TraT client),
    proxy_test.go
  - _Requirements: R-TRAT-INJECT-1, R-TRAT-SEC-1_

- [ ] 5. Wire TTS client into the sidecar runtime
  - Files: cmd/agent or the sidecar bootstrap that builds the Sidecar — construct
    the trat.Client from IdentityProxySpec.TTS + identity.Source
  - _Requirements: R-TRAT-MINT-1, R-TRAT-API-2_

- [ ] 6. eBPF coupling validation (no exfiltration)
  - Files: docs note in design + a webhook check that trat-enabled resources are
    covered by the egress allow-list/redirect; no eBPF code change
  - _Requirements: R-TRAT-EBPF-1_

- [ ] 7. e2e: fake-tts fixture + R-E2E-SCN-TRAT scenario
  - Files: cmd/fake-tts (+ deploy/docker/fake-tts.Dockerfile, multiarch),
    test/e2e/manifests/fake-tts.yaml, test/e2e/fullstack/shared/scenarios.go
    (runTraTEgress), coverage.go entry
  - Assert upstream sees Txn-Token; assert eBPF drops egress to a non-allow-listed
    host even with trat set.
  - _Requirements: R-TRAT-INJECT-1, R-TRAT-EBPF-1, R-TRAT-SEC-1_

- [ ] 8. Quint invariant
  - File: spec/quint/trat.qnt (or extend agentnet.qnt) — "TraT attached ⇒ egress
    permitted by policy"; Safety invariant
  - _Requirements: R-TRAT-EBPF-1_

- [ ] 9. Docs
  - Files: docs/runbooks/trat-egress.md (configure TTS + a trat resource; verify
    Txn-Token; fail-closed behaviour), INSTALL.md prereq (Tokenetes Service)
  - _Requirements: all_

## Out of P1 (tracked, not scheduled)
- [ ] Request-derived scope/rctx (per outbound method/path)
- [ ] Transparent-TLS-intercept egress (operator-managed CA) for non-resource egress
- [ ] Inbound TraT verification (our sidecar or the Tokenetes Agent sidecar) — R-TRAT-VERIFY-1
