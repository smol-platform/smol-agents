// SPDX-License-Identifier: Apache-2.0
// Quint specification: secretless egress (TraT-authorized credential minting +
// eBPF-gated injection). A broker-minted provider credential may be injected
// onto an egress request ONLY when (1) a valid authorizing TraT, bound to the
// requesting workload, permitted the mint under policy, AND (2) the destination
// is on the eBPF egress allow-list. Implements R-SEGR-AUTH-1 / R-SEGR-EBPF-1 /
// R-SEGR-SEC-1.
module secretless_egress {

  type Principal = AgentA | AgentB | Intruder
  type Scope     = RepoRead | RepoWrite
  type Repo      = RepoApp | RepoSecret
  type Host      = GitHubAPI | EvilHost

  type Mint     = { p: Principal, scope: Scope, repo: Repo }
  type Injection = { p: Principal, host: Host, scope: Scope, repo: Repo }

  // Fixed configuration: who may mint what for which repo, and which egress
  // destinations eBPF permits. Everything else is denied / dropped.
  pure val credPolicy: Set[(Principal, Scope, Repo)] = Set(
    (AgentA, RepoRead, RepoApp),
    (AgentB, RepoRead, RepoApp),
  )
  pure val allowList: Set[Host] = Set(GitHubAPI)

  pure val principals: Set[Principal] = Set(AgentA, AgentB, Intruder)
  pure val scopes: Set[Scope]         = Set(RepoRead, RepoWrite)
  pure val repos: Set[Repo]           = Set(RepoApp, RepoSecret)
  pure val hosts: Set[Host]           = Set(GitHubAPI, EvilHost)

  // minted: credentials the broker has produced.
  // injected: credentials the proxy has placed on an egress request.
  var minted: Set[Mint]
  var injected: Set[Injection]

  action init = all {
    minted' = Set(),
    injected' = Set(),
  }

  // The broker mints iff the authorizing TraT is valid, bound to the calling
  // workload (req_wl == p), and policy permits (p, scope, repo).
  action brokerMint(p: Principal, scope: Scope, repo: Repo, tratValid: bool, boundTo: Principal): bool = all {
    tratValid,
    boundTo == p,
    credPolicy.contains((p, scope, repo)),
    minted' = minted.union(Set({ p: p, scope: scope, repo: repo })),
    injected' = injected,
  }

  // A mint attempt that fails any precondition leaks nothing (fail-closed).
  action mintDenied(p: Principal, scope: Scope, repo: Repo, tratValid: bool, boundTo: Principal): bool = all {
    not(tratValid and boundTo == p and credPolicy.contains((p, scope, repo))),
    minted' = minted,
    injected' = injected,
  }

  // The proxy injects a minted credential onto egress iff eBPF permits the
  // destination. A disallowed host is dropped — never injected.
  action egressInject(p: Principal, scope: Scope, repo: Repo, host: Host): bool = all {
    minted.contains({ p: p, scope: scope, repo: repo }),
    allowList.contains(host),
    injected' = injected.union(Set({ p: p, host: host, scope: scope, repo: repo })),
    minted' = minted,
  }

  // Egress to a non-allow-listed host (or with no minted credential) is dropped.
  action egressDropped(p: Principal, scope: Scope, repo: Repo, host: Host): bool = all {
    not(minted.contains({ p: p, scope: scope, repo: repo }) and allowList.contains(host)),
    minted' = minted,
    injected' = injected,
  }

  action step =
    nondet p = oneOf(principals)
    nondet scope = oneOf(scopes)
    nondet repo = oneOf(repos)
    nondet host = oneOf(hosts)
    nondet tratValid = oneOf(Set(true, false))
    nondet boundTo = oneOf(principals)
    any {
      brokerMint(p, scope, repo, tratValid, boundTo),
      mintDenied(p, scope, repo, tratValid, boundTo),
      egressInject(p, scope, repo, host),
      egressDropped(p, scope, repo, host),
    }

  // ------------------------------ Invariants ----------------------------

  // Every minted credential was authorized by policy (the valid, bound TraT is
  // a transient precondition enforced at mint time).
  val MintImpliesAuthorized: bool =
    minted.forall(m => credPolicy.contains((m.p, m.scope, m.repo)))

  // Nothing is injected that wasn't first minted.
  val InjectImpliesMinted: bool =
    injected.forall(x => minted.contains({ p: x.p, scope: x.scope, repo: x.repo }))

  // Credentials are only ever injected onto eBPF-permitted egress.
  val InjectOnAllowedHost: bool =
    injected.forall(x => allowList.contains(x.host))

  // The whole point: an injected credential implies BOTH a policy-authorized
  // mint AND a policy-permitted egress destination.
  val InjectImpliesAuthorizedEgress: bool =
    injected.forall(x =>
      credPolicy.contains((x.p, x.scope, x.repo)) and allowList.contains(x.host))

  val Safety: bool =
    MintImpliesAuthorized and InjectImpliesMinted and InjectOnAllowedHost
      and InjectImpliesAuthorizedEgress

  // ------------------------------- Tests --------------------------------
  // Property checking: quint run --invariant=Safety secretless_egress.qnt

  run happyPathTest = {
    init
      .then(brokerMint(AgentA, RepoRead, RepoApp, true, AgentA))
      .then(egressInject(AgentA, RepoRead, RepoApp, GitHubAPI))
  }

  // A valid TraT minted for AgentA, presented by the Intruder, is denied
  // (req_wl binding) — nothing is minted.
  run replayedTraTDeniedTest = {
    init.then(mintDenied(Intruder, RepoRead, RepoApp, true, AgentA))
  }

  // An unsigned/invalid TraT is denied.
  run forgedTraTDeniedTest = {
    init.then(mintDenied(AgentA, RepoRead, RepoApp, false, AgentA))
  }

  // Mint succeeds, but egress to a disallowed host is dropped by eBPF — the
  // credential is never injected.
  run disallowedHostDroppedTest = {
    init
      .then(brokerMint(AgentA, RepoRead, RepoApp, true, AgentA))
      .then(egressDropped(AgentA, RepoRead, RepoApp, EvilHost))
  }

  run mixedScenario = {
    init
      .then(brokerMint(AgentA, RepoRead, RepoApp, true, AgentA))
      .then(mintDenied(Intruder, RepoRead, RepoApp, true, AgentA))
      .then(egressInject(AgentA, RepoRead, RepoApp, GitHubAPI))
      .then(egressDropped(AgentA, RepoRead, RepoApp, EvilHost))
  }
}
