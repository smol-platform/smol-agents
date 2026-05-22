# Product Overview — smol-agents-agentnet

## Product Purpose

`agentnet` is the networking layer that lets agent workloads reach
restricted resources without exposing the host to a compromised
agent. It offers two interoperable modes under a single
`AgentNetwork` CR:

1. **Identity proxy** — a SPIFFE-aware sidecar that wraps the agent's
   egress in mTLS / JWT and routes it to per-resource gateways.
   Reuses the platform's existing identity stack (R-IDN-1, R-MTL-1)
   and is the recommended path for "agent talks to known services"
   (databases, internal HTTP APIs, MCP servers).
2. **WireGuard mesh** — a userspace WireGuard adapter (running on
   gVisor's netstack TUN, no kernel module required) that joins an
   existing WireGuard network OR runs a small WireGuard server. Right
   choice when agents need raw network reach into a private network
   the operator doesn't already control with a proxy.

A third leg is **eBPF-driven egress policy** on the host: a cgroup
`connect4` program redirects matching destinations to the local
sidecar transparently, and an egress allow-list drops everything
else. This sits *outside* the Kata-FC sandbox so a compromised agent
cannot disable it.

## Target Users

- **Application teams** whose agents need to talk to a private
  Postgres / Redis / internal HTTP service without burning long-lived
  credentials in environment variables.
- **Platform engineers** who want a uniform identity-aware path to
  every restricted resource, with audit trails per call.
- **Network engineers** who already run a WireGuard hub and want
  agents to participate in it as ordinary peers — without giving
  agents kernel privileges.
- **Security engineers** who need defense-in-depth: even if the
  in-Pod proxy is bypassed, an eBPF egress rule on the host drops
  the connection.

## Key Features

1. **`AgentNetwork` CR** — discriminated by `kind`
   (`identityProxy | wireguardMesh`). Lives in
   `runtime.agents.smol-agents.ai/v1`.
2. **TCP + HTTP identity proxies** as sidecars. TCP is a byte
   forwarder over SPIFFE mTLS; HTTP is a reverse proxy that mints
   JWT-SVIDs per upstream audience.
3. **Userspace WireGuard** via `golang.zx2c4.com/wireguard` +
   `tun/netstack`. No kernel module, runs cleanly inside Kata-FC.
   Two modes: `client` (joins peers) and `server` (listens for peers).
4. **eBPF egress redirect + allow-list** — `cgroup/connect4` rewrites
   matching destinations to the sidecar; `cgroup_skb/egress` drops
   anything outside the policy. Maps managed by the operator from
   the CR.
5. **Per-call audit** — the host eBPF probes already in place log
   `(SPIFFE ID, dst_ip, dst_port, ts)` for every connect, regardless
   of which transport the agent thinks it's using.
6. **Verifiable** — Quint invariants
   `EgressOnlyToAllowedCIDRs`, `ProxyAuthRequired`,
   `WireGuardPeerKnown` checked in CI.

## Business Objectives

- Cut "wire up agent → internal Postgres" from days (rolling Vault
  policies + transit encryption + custom proxy) to one CR.
- Make every agent-to-resource call independently auditable.
- Eliminate long-lived API tokens from agent Pods entirely.

## Success Metrics

- **TCP proxy P99 added latency** ≤ 5 ms over direct dial.
- **HTTP proxy P99 added latency** ≤ 8 ms.
- **WireGuard handshake P95** ≤ 250 ms (userspace netstack).
- **Egress drop rate**: 100% of connects outside the allow-list
  blocked by eBPF in chaos tests.
- **CRD round-trip** (apply → sidecar configured → traffic flows)
  ≤ 30 s on a fresh kind cluster.

## Product Principles

1. **One CR, two transports** — identity proxy and WireGuard share
   the `AgentNetwork` shape so a tenant can mix them.
2. **No kernel modules in the sandbox** — both transports run in
   userspace inside Kata-FC.
3. **Privileged operations stay outside** — eBPF policy enforcement
   runs on the host via the existing ebpf-loader DaemonSet.
4. **Identity is the unit of policy** — every ACL keys on SPIFFE ID,
   not IP or namespace.
5. **Defense in depth** — proxy enforces *what* the agent can reach;
   eBPF enforces *that* the agent can only reach what's allowed.
6. **Verifiable** — the formal model proves both legs and their
   composition.

## Monitoring & Visibility

- `kubectl get agentnetworks -A -o wide` — printer columns: `KIND`,
  `RESOURCES`, `WG-PEERS`, `EGRESS`, `READY`, `AGE`.
- Prometheus metrics: `agentnet_proxy_dial_total{resource}`,
  `agentnet_proxy_dial_errors_total{resource,reason}`,
  `agentnet_egress_dropped_total{cidr}`,
  `agentnet_wg_peers{name,state}`.
- OTel spans per proxy call with `gen_ai.tool.name` linkage when the
  call originated from a tool invocation.

## Future Vision

- **Service-mesh interop** — emit `Cilium` `CiliumNetworkPolicy`
  from the same CR for clusters that want kernel-level enforcement.
- **Multi-cluster** — mesh agents across clusters via WireGuard
  hub-and-spoke or via SPIRE federation + identity proxy.
- **Per-tool networking** — scope an `AgentNetwork` to a specific
  tool reference rather than the whole agent.
- **Cost-aware routing** — when the same resource is reachable via
  proxy or WireGuard, pick the lower-latency path per call.
