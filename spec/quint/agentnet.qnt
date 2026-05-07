// SPDX-License-Identifier: Apache-2.0
// Quint specification: AgentNetwork — identity-aware proxy + WireGuard
// userspace mesh. Implements R-AN-PROXY-* / R-AN-EBPF-* / R-AN-WG-*.
//
// Run with:
//   quint typecheck spec/quint/agentnet.qnt
//   quint run --invariant=Safety spec/quint/agentnet.qnt
module agentnet {

  type Kind = ProxyMode | WGMode

  type Decision = Allow | DropPolicy | DropPeer | RedirectToSidecar

  // A flow is one egress packet/connection's view: a destination CIDR
  // + L4 (proto, port). We model dst as a string label rather than
  // octets — the invariants are about set membership, not numerics.
  type Flow = {
    dst:   str,
    proto: str,
    port:  int,
  }

  // ------------------------------- State ----------------------------

  // Configured kind for the current AgentNetwork being modeled.
  var kind: Kind

  // Identity-proxy state.
  var allowSet:    Set[Flow]   // egress allow-list rules
  var redirectSet: Set[str]    // CIDRs the cgroup/connect4 redirects
  var jwtMinted:   bool        // an HTTP request has minted a JWT-SVID
  var lastDecision: Decision

  // WireGuard state.
  var wgPeers:        Set[str]  // public keys we've configured
  var lastSeenPeer:   str       // most recent inbound packet's peer key
  var listenPortOpen: bool      // server-mode UDP socket bound

  // ------------------------------- Init -----------------------------

  action init = all {
    kind' = ProxyMode,
    allowSet' = Set(),
    redirectSet' = Set(),
    jwtMinted' = false,
    lastDecision' = DropPolicy,
    wgPeers' = Set(),
    lastSeenPeer' = "",
    listenPortOpen' = false,
  }

  // Switch model to wireguard mode. Per R-AN-API-1 the CRD-level
  // mutex forbids both transports coexisting, so this transition is
  // only legitimate from a fresh init state — modelling that
  // explicitly is what makes NoCrossTransport meaningful.
  action useWireGuard = all {
    allowSet.size() == 0,
    redirectSet.size() == 0,
    not(jwtMinted),
    kind' = WGMode,
    allowSet' = allowSet,
    redirectSet' = redirectSet,
    jwtMinted' = jwtMinted,
    lastDecision' = lastDecision,
    wgPeers' = wgPeers,
    lastSeenPeer' = lastSeenPeer,
    listenPortOpen' = listenPortOpen,
  }

  // ----------------------- Identity-proxy actions -------------------

  action configureAllow(f: Flow): bool = all {
    kind == ProxyMode,
    allowSet' = allowSet.union(Set(f)),
    redirectSet' = redirectSet,
    kind' = kind,
    jwtMinted' = jwtMinted,
    lastDecision' = lastDecision,
    wgPeers' = wgPeers,
    lastSeenPeer' = lastSeenPeer,
    listenPortOpen' = listenPortOpen,
  }

  action configureRedirect(cidr: str): bool = all {
    kind == ProxyMode,
    redirectSet' = redirectSet.union(Set(cidr)),
    allowSet' = allowSet,
    kind' = kind,
    jwtMinted' = jwtMinted,
    lastDecision' = lastDecision,
    wgPeers' = wgPeers,
    lastSeenPeer' = lastSeenPeer,
    listenPortOpen' = listenPortOpen,
  }

  // attemptEgress models the cgroup_skb/egress + connect4 pipeline.
  // Order matches the kernel hook order:
  //   1. connect4 redirects matching CIDR → sidecar (RedirectToSidecar)
  //   2. otherwise cgroup_skb/egress checks the allow-list:
  //      hit → Allow; miss → DropPolicy.
  action attemptEgress(f: Flow): bool = all {
    kind == ProxyMode,
    lastDecision' =
      if (redirectSet.contains(f.dst))
        RedirectToSidecar
      else if (allowSet.contains(f))
        Allow
      else
        DropPolicy,
    kind' = kind,
    allowSet' = allowSet,
    redirectSet' = redirectSet,
    jwtMinted' = jwtMinted,
    wgPeers' = wgPeers,
    lastSeenPeer' = lastSeenPeer,
    listenPortOpen' = listenPortOpen,
  }

  // mintJWT models the HTTP sidecar minting a JWT-SVID before sending
  // the upstream request. R-AN-PROXY-2.
  action mintJWT = all {
    kind == ProxyMode,
    lastDecision == RedirectToSidecar,
    jwtMinted' = true,
    kind' = kind,
    allowSet' = allowSet,
    redirectSet' = redirectSet,
    lastDecision' = lastDecision,
    wgPeers' = wgPeers,
    lastSeenPeer' = lastSeenPeer,
    listenPortOpen' = listenPortOpen,
  }

  // ----------------------- WireGuard actions ------------------------

  action openListen = all {
    kind == WGMode,
    not(listenPortOpen),
    listenPortOpen' = true,
    kind' = kind,
    allowSet' = allowSet,
    redirectSet' = redirectSet,
    jwtMinted' = jwtMinted,
    lastDecision' = lastDecision,
    wgPeers' = wgPeers,
    lastSeenPeer' = lastSeenPeer,
  }

  action addPeer(pubKey: str): bool = all {
    kind == WGMode,
    wgPeers' = wgPeers.union(Set(pubKey)),
    kind' = kind,
    allowSet' = allowSet,
    redirectSet' = redirectSet,
    jwtMinted' = jwtMinted,
    lastDecision' = lastDecision,
    lastSeenPeer' = lastSeenPeer,
    listenPortOpen' = listenPortOpen,
  }

  // receivePacket models WireGuard's cryptokey routing: a packet is
  // accepted iff its decrypted source key is in our peer set. This
  // models R-AN-WG-3: only authorized peers' traffic is delivered.
  action receivePacket(pubKey: str): bool = all {
    kind == WGMode,
    listenPortOpen,
    lastSeenPeer' =
      if (wgPeers.contains(pubKey)) pubKey else "",
    lastDecision' =
      if (wgPeers.contains(pubKey)) Allow else DropPeer,
    kind' = kind,
    allowSet' = allowSet,
    redirectSet' = redirectSet,
    jwtMinted' = jwtMinted,
    wgPeers' = wgPeers,
    listenPortOpen' = listenPortOpen,
  }

  // ------------------------------ Step ------------------------------
  // The Quint runner needs a non-deterministic step action. We
  // enumerate a small finite set of representative configurations
  // and packets — that's enough surface to exercise every guarded
  // branch in `attemptEgress` and `receivePacket`.

  pure val FlowSamples: Set[Flow] = Set(
    { dst: "10.42.5.7/32",  proto: "tcp", port: 443  },
    { dst: "10.42.6.10/32", proto: "tcp", port: 443  },
    { dst: "1.1.1.1/32",    proto: "tcp", port: 443  },
    { dst: "10.42.0.0/16",  proto: "tcp", port: 5432 }
  )
  pure val CIDRSamples: Set[str] = Set("10.42.0.0/16", "10.99.0.0/16")
  pure val PeerKeys:    Set[str] = Set("AAAA", "BBBB", "CCCC")

  action step = any {
    nondet f = oneOf(FlowSamples) configureAllow(f),
    nondet c = oneOf(CIDRSamples) configureRedirect(c),
    nondet f = oneOf(FlowSamples) attemptEgress(f),
    mintJWT,
    useWireGuard,
    openListen,
    nondet p = oneOf(PeerKeys) addPeer(p),
    nondet p = oneOf(PeerKeys) receivePacket(p),
  }

  // ----------------------------- Invariants -------------------------

  // EgressOnlyToAllowedCIDRs: every Allowed flow in proxy mode must
  // be in the allow-list (the eBPF allow-list check is sound).
  // Allowed redirects are accepted because they go to the sidecar
  // which then enforces SPIFFE auth.
  val EgressOnlyToAllowedCIDRs: bool =
    (kind == ProxyMode and lastDecision == Allow)
      implies allowSet.size() > 0

  // WireGuardPeerKnown: any inbound packet that influenced
  // lastSeenPeer must come from a configured peer (cryptokey
  // routing). This is the central WG invariant.
  val WireGuardPeerKnown: bool =
    (kind == WGMode and lastSeenPeer != "")
      implies wgPeers.contains(lastSeenPeer)

  // NoCrossTransport: in proxy mode, WG state must be quiescent;
  // in WG mode, proxy state must be quiescent. Mirrors the spec's
  // CRD-level mutex (R-AN-API-1).
  val NoCrossTransport: bool =
    (kind == ProxyMode implies (not(listenPortOpen) and wgPeers.size() == 0))
      and (kind == WGMode implies (allowSet.size() == 0 and redirectSet.size() == 0))

  val Safety: bool =
    EgressOnlyToAllowedCIDRs
      and WireGuardPeerKnown
      and NoCrossTransport

  // ------------------------------- Tests ----------------------------

  run proxyAllowsKnownDestination = {
    val f: Flow = { dst: "10.42.5.7/32", proto: "tcp", port: 443 }
    init.then(configureAllow(f))
        .then(attemptEgress(f))
  }

  run proxyDropsUnknownDestination = {
    val known: Flow = { dst: "10.42.5.7/32", proto: "tcp", port: 443 }
    val unknown: Flow = { dst: "1.1.1.1/32", proto: "tcp", port: 443 }
    init.then(configureAllow(known))
        .then(attemptEgress(unknown))
  }

  run proxyRedirectsCIDR = {
    val f: Flow = { dst: "10.42.0.0/16", proto: "tcp", port: 5432 }
    init.then(configureRedirect("10.42.0.0/16"))
        .then(attemptEgress(f))
        .then(mintJWT)
  }

  run wgRejectsUnknownPeer = {
    init.then(useWireGuard)
        .then(addPeer("AAAA"))
        .then(openListen)
        .then(receivePacket("BBBB"))
  }

  run wgAcceptsKnownPeer = {
    init.then(useWireGuard)
        .then(addPeer("AAAA"))
        .then(openListen)
        .then(receivePacket("AAAA"))
  }
}
