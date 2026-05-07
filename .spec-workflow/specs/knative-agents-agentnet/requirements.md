# Requirements — knative-agents-agentnet

## Alignment with Product Vision

These requirements implement the principles in `product.md`: one CR
for two transports, no kernel modules in the sandbox, privileged
work on the host, identity-keyed policy, defense in depth,
verifiable.

## Requirements

### R-AN-API: API Surface

#### R-AN-API-1 — `AgentNetwork` CR
**User Story:** As a developer, I want one CR that describes how an
agent reaches restricted resources.

**Acceptance Criteria:**
1. THE CR SHALL be served at
   `runtime.agents.stigen.ai/v1` (kind `AgentNetwork`).
2. `spec.kind` SHALL be one of `identityProxy` or `wireguardMesh`.
3. THE validating webhook SHALL reject CRs whose nested config
   does not match `kind`.
4. THE CR SHALL include printer columns `KIND`, `RESOURCES`,
   `WG-PEERS`, `EGRESS`, `READY`, `AGE`.

#### R-AN-API-2 — Per-agent association
**User Story:** As an admin, I want `AgentNetwork` to bind to one
or more `Agent` CRs by selector.

**Acceptance Criteria:**
1. `spec.agentSelector` (Kubernetes label selector) SHALL determine
   which Agents in the same namespace receive the network sidecar.

### R-AN-PROXY: Identity proxy

#### R-AN-PROXY-1 — TCP byte-forwarder
**User Story:** As an agent, I want to dial `localhost:5432` and
have the bytes flow over mTLS to a private Postgres.

**Acceptance Criteria:**
1. WHEN `kind=identityProxy` AND a resource has `kind=tcp` THEN the
   sidecar SHALL listen on `localAddr` and forward each accepted
   connection over a SPIFFE mTLS dial to `gateway`.
2. THE peer SVID presented by the gateway MUST match
   `resource.authorize` (SPIFFE-ID match list).
3. WHEN the SVID rotates THEN existing connections SHALL stay open
   and new connections SHALL use the new bundle.

#### R-AN-PROXY-2 — HTTP reverse proxy with JWT-SVID
**User Story:** As an agent, I want to call `127.0.0.1:9100/orders`
and have the request reach the internal billing API with my SPIFFE
identity attached.

**Acceptance Criteria:**
1. WHEN `kind=identityProxy` AND a resource has `kind=http` THEN the
   sidecar SHALL run an HTTP reverse proxy that, on every request,
   mints a JWT-SVID for `resource.jwtAudience` and attaches it as
   `Authorization: Bearer …`.
2. WHEN the JWT-SVID is within 50 % of its remaining lifetime THEN
   the sidecar SHALL refresh it before the next request (R-IDN-1).
3. UPSTREAM 4xx/5xx responses SHALL be forwarded unchanged; the
   sidecar SHALL emit a metric `agentnet_proxy_dial_errors_total`
   tagged with the upstream status.

#### R-AN-PROXY-3 — Audit + metrics
**Acceptance Criteria:**
1. EACH proxy call SHALL emit an OTel span with attributes
   `gen_ai.tool.name` (when invoked from a tool path),
   `agentnet.resource`, `agentnet.kind`, `agentnet.upstream`,
   `agentnet.spiffe_id`, `agentnet.duration_ms`.
2. THE Prometheus counters listed in `product.md` SHALL be
   registered with controller-runtime's registry on startup.

### R-AN-EBPF: Host-side eBPF policy

#### R-AN-EBPF-1 — Egress redirect program
**User Story:** As a platform engineer, I want a tenant agent's
egress to known internal IPs to be transparently rewritten to the
local sidecar without modifying the agent.

**Acceptance Criteria:**
1. THE program SHALL be a `cgroup/connect4` BPF object that consults
   an `BPF_MAP_TYPE_LPM_TRIE` of redirect CIDRs.
2. WHEN `dst` is in the trie THEN `user_ip4` SHALL be rewritten to
   `127.0.0.1` and `user_port` to the configured sidecar port.
3. THE map SHALL be populated by the operator from
   `spec.identityProxy.egress.redirect.cidrs`.

#### R-AN-EBPF-2 — Egress allow-list
**Acceptance Criteria:**
1. THE program SHALL be a `cgroup_skb/egress` BPF object that
   permits packets whose `(cgroup_id, dst_ip, dst_port, proto)` is
   in the allow-list map; otherwise drop.
2. WHEN the agent's cgroup ID is unknown to the policy THEN the
   default action SHALL be `drop` (deny by default).
3. THE map SHALL be populated by the operator from
   `spec.identityProxy.egress.allow`.

#### R-AN-EBPF-3 — Audit
**Acceptance Criteria:**
1. EACH outbound `tcp_v4_connect` from a managed cgroup SHALL emit
   a ringbuf event tagged with the calling cgroup ID; the userspace
   collector SHALL resolve the cgroup ID to the SPIFFE ID and emit
   an OTel `gen_ai.tool_call` child span.

### R-AN-WG: WireGuard

#### R-AN-WG-1 — Userspace device
**User Story:** As a security engineer, I want WireGuard to run
inside Kata-FC without granting the agent kernel access.

**Acceptance Criteria:**
1. THE adapter SHALL use `golang.zx2c4.com/wireguard` with
   `tun/netstack` so the agent process owns the WG device entirely
   in user space.
2. THE adapter SHALL accept a private key via the broker
   (`privateKeyRef`) — never a literal value in the CR.

#### R-AN-WG-2 — Client mode (join existing tunnel)
**User Story:** As an admin, I want my agent to be a peer of an
existing WireGuard hub.

**Acceptance Criteria:**
1. WHEN `mode=client` THEN the adapter SHALL register every entry
   in `spec.wireguardMesh.peers[]` with the userspace device.
2. EACH peer SHALL declare `publicKey`, `endpoint`, `allowedIPs`,
   and optional `persistentKeepalive`.

#### R-AN-WG-3 — Server mode (listen for peers)
**User Story:** As an admin, I want the agent to act as a small
WireGuard hub for downstream peers.

**Acceptance Criteria:**
1. WHEN `mode=server` THEN the adapter SHALL bind a UDP listener at
   `spec.wireguardMesh.listenPort` (default 51820) and accept the
   declared peers.
2. The validating webhook SHALL reject `mode=server` when no
   `addresses` are declared (server needs an interface address).

#### R-AN-WG-4 — DNS + addresses
**Acceptance Criteria:**
1. THE adapter SHALL bring up the netstack interface with the
   declared `addresses` (CIDR list) and SHALL configure the agent
   process's resolver to use `dns` if provided.

### R-AN-SEC: Security

#### R-AN-SEC-1 — No long-lived credentials in env
**Acceptance Criteria:**
1. THE proxy + WG adapter SHALL fetch all credentials (SVIDs,
   pre-shared keys, peer keys) through the existing secret-broker;
   none SHALL be inlined in the CR.

#### R-AN-SEC-2 — Sandbox respect
**Acceptance Criteria:**
1. THE in-Pod components SHALL run with the same restricted
   `securityContext` as the agent (R-SBX-1).
2. THE host eBPF policy SHALL be enforced from the privileged
   ebpf-loader DaemonSet, not from any agent Pod.

### R-AN-VRF: Verification

#### R-AN-VRF-1 — Quint invariants
**Acceptance Criteria:**
1. `spec/quint/agentnet.qnt` SHALL declare and verify
   `EgressOnlyToAllowedCIDRs`, `ProxyAuthRequired`,
   `WireGuardPeerKnown` via `quint run --invariant=Safety`.

#### R-AN-VRF-2 — Property tests
**Acceptance Criteria:**
1. The proxy package's tests SHALL include rapid-driven property
   tests asserting that the upstream peer SVID always matches the
   declared `authorize` set on every accepted dial.
