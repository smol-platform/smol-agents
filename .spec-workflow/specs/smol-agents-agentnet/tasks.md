# Tasks — smol-agents-agentnet

- [x] 1. Author spec (product/req/design/tasks)
  - Files: this directory
  - _Requirements: all_

- [x] 2. Add AgentNetwork* types to pkg/agentmodel/v1
  - Files: pkg/agentmodel/v1/agentnetwork.go (+ test)
  - _Requirements: R-AN-API-1..2, R-AN-PROXY-*, R-AN-WG-*_

- [x] 3. Build pkg/agentnet/proxy
  - Files: pkg/agentnet/proxy/{tcp,http,sidecar}.go (+ tests)
  - _Requirements: R-AN-PROXY-1..3_

- [x] 4. Build pkg/agentnet/wireguard
  - Files: pkg/agentnet/wireguard/{adapter,userspace,config}.go (+ test)
  - _Requirements: R-AN-WG-1..4_

- [x] 5. Egress redirect + allow-list eBPF program
  - Files: bpf/programs/egress_redirect.bpf.c, pkg/agentnet/cgroup/maps.go
  - _Requirements: R-AN-EBPF-1..3_

- [x] 6. AgentNetwork CRD wrapper + sample CRs
  - Files: operator/api/agentmodel/v1/agentnetwork.go,
    operator/config/crd/runtime.agents.stigen.ai_agentnetworks.yaml,
    operator/config/samples/agentnetwork_*.yaml
  - _Requirements: R-AN-API-1_

- [x] 7. Quint invariants
  - File: spec/quint/agentnet.qnt
  - _Requirements: R-AN-VRF-1_

- [ ] 8. AgentNetworkReconciler (envtest covered)
  - File: operator/internal/controllers/agentmodel/agentnetwork_controller.go
  - _Requirements: R-AN-API-2_

- [ ] 9. Sidecar injection wired into AgentRun reconciler
  - When an Agent has an AgentNetworkRef, the AgentRun's Pod gets
    the proxy / WG sidecar container added.
  - _Requirements: R-AN-API-2_
