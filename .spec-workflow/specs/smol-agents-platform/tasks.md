# Tasks Document — smol-agents

This file tracks the work that turned the requirements + design into running
code. Each task references the requirement IDs it satisfies and (where
applicable) the file that materialises it.

- [x] 1. Bootstrap module + dev environment
  - File: go.mod, devenv.nix, Makefile, .gitignore
  - Purpose: Reproducible build/test/verify
  - _Requirements: All_

- [x] 2. Encode formal model in Quint
  - Files: spec/quint/{identity,secrets,agent_lifecycle}.qnt
  - Invariants: AlwaysHaveValidSVID, LeaseImpliesAuthorized,
    ReadyImpliesAllSubsystemsReady, StopImpliesDetached
  - _Requirements: R-VRF-1_

- [x] 3. Implement pkg/config (typed YAML, validation, env overrides)
  - Files: pkg/config/{config.go,doc.go,config_test.go}
  - _Requirements: R-IDN-3, R-MTL-2, R-SEC-2_

- [x] 4. Implement pkg/identity (SPIFFE workload identity)
  - Files: pkg/identity/{source.go,authorizer.go,mode.go,authorizer_test.go}
  - _Requirements: R-IDN-1, R-IDN-2, R-IDN-3_

- [x] 5. Implement pkg/transport (private + public mTLS)
  - Files: pkg/transport/{private.go,public.go,peer.go,conn.go,transport_test.go}
  - _Requirements: R-MTL-1, R-MTL-2_

- [x] 6. Implement pkg/secrets (kloak-style broker, client, backends, policy)
  - Files: pkg/secrets/{server.go,client.go,types.go,wire.go,
    peer_attestor*.go,backend_static.go,secrets_test.go,property_test.go}
  - _Requirements: R-SEC-1, R-SEC-2, R-SEC-3, R-VRF-2_

- [x] 7. Implement pkg/sandbox (RuntimeClass abstraction)
  - Files: pkg/sandbox/{sandbox.go,doc.go,sandbox_test.go}
  - _Requirements: R-SBX-1_

- [x] 8. Implement pkg/ebpf (CO-RE loader + EventBus)
  - Files: pkg/ebpf/{event.go,loader.go,loader_linux.go,loader_other.go,
    event_test.go}
  - _Requirements: R-EBP-1, R-EBP-2_

- [x] 9. Implement pkg/runtime, pkg/health, pkg/observability
  - Files: pkg/runtime/{manager.go,manager_test.go},
    pkg/health/{health.go,health_test.go},
    pkg/observability/{observability.go,observability_test.go}
  - _Requirements: R-RUN-1, R-RUN-2_

- [x] 10. Author CO-RE BPF programs (syscalls + network)
  - Files: bpf/programs/{syscalls.bpf.c,network.bpf.c,Makefile}
  - _Requirements: R-EBP-1, R-SBX-2_

- [x] 11. Wire cmd/agent (lifecycle + service registration)
  - File: cmd/agent/main.go
  - _Requirements: R-RUN-1, R-RUN-2, all R-IDN/R-MTL/R-SEC/R-EBP_

- [x] 12. Wire cmd/secret-proxy
  - File: cmd/secret-proxy/main.go
  - _Requirements: R-SEC-1, R-SEC-2, R-SEC-3_

- [x] 13. Implement cmd/agentctl (status + lease debug CLI)
  - File: cmd/agentctl/main.go
  - _Requirements: usability_

- [x] 14. Author Helm chart + Kustomize overlays
  - Files: deploy/helm/**, deploy/kustomize/**, deploy/spire/*,
    deploy/docker/*.Dockerfile
  - _Requirements: R-DEP-1, R-DEP-2, R-SBX-1_

- [x] 15. Verification harness
  - Files: test/integration/**, test/e2e/scripts/up-kind.sh,
    Makefile target `verify-formal`, GitHub Actions in .github/workflows
  - _Requirements: R-VRF-1, R-VRF-2_

## Validation Matrix

| Requirement | Code reference                                       | Test reference                                              |
|-------------|------------------------------------------------------|-------------------------------------------------------------|
| R-IDN-1     | pkg/identity/source.go                               | spec/quint/identity.qnt::AlwaysHaveValidSVID                |
| R-IDN-2     | pkg/identity/source.go                               | pkg/identity/authorizer_test.go                             |
| R-IDN-3     | pkg/identity/mode.go, pkg/config                     | pkg/config/config_test.go::TestValidate_InsecureRequiresEnv |
| R-MTL-1     | pkg/transport/private.go                             | pkg/transport/transport_test.go                             |
| R-MTL-2     | pkg/transport/public.go                              | pkg/transport/transport_test.go::TestPublicListener_*       |
| R-SBX-1     | pkg/sandbox, deploy/helm/templates/*                 | helm template ... runc → fail (R-SBX-1)                     |
| R-SBX-2     | bpf/programs/syscalls.bpf.c                          | e2e/up-kind.sh                                              |
| R-EBP-1     | pkg/ebpf/loader_linux.go                             | pkg/ebpf/event_test.go                                      |
| R-EBP-2     | pkg/ebpf/event.go                                    | pkg/ebpf/event_test.go::TestMemoryBus_*                     |
| R-SEC-1     | pkg/secrets/peer_attestor_linux.go, server.go        | pkg/secrets/secrets_test.go::TestServer_AttestFailure       |
| R-SEC-2     | pkg/secrets/server.go                                | pkg/secrets/property_test.go::TestProperty_LeaseImpliesAuthorized |
| R-SEC-3     | pkg/secrets/types.go (Backend), backend_static.go    | pkg/secrets/secrets_test.go::TestStaticBackend_*            |
| R-RUN-1     | pkg/runtime/manager.go, pkg/health/health.go         | pkg/runtime/manager_test.go::TestManager_HappyPath          |
| R-RUN-2     | pkg/runtime/manager.go (drainThenStop)               | pkg/runtime/manager_test.go                                 |
| R-DEP-1     | deploy/helm/templates/knative-service.yaml           | helm template, kubectl apply --dry-run                      |
| R-DEP-2     | deploy/helm/templates/{deployment,statefulset}.yaml  | helm template ... mode={deployment,statefulset}             |
| R-VRF-1     | spec/quint/*.qnt, Makefile verify-formal             | quint test                                                  |
| R-VRF-2     | pkg/secrets/property_test.go                         | rapid 1000+ generated cases                                 |
