// SPDX-License-Identifier: Apache-2.0
// Quint specification: secret broker (kloak-style).
// Implements R-SEC-1 / R-SEC-2 / R-SEC-3.
module secrets {

  type Principal = AgentA | AgentB | Intruder
  type Secret    = DBCred | APIKey | Other
  type Lease = {
    name:      Secret,
    principal: Principal,
    expires:   int,
  }

  pure val MAX_TTL: int = 900
  pure val DEFAULT_TTL: int = 600

  var now: int
  var policy: Set[(Principal, Secret)]
  var issued: Set[Lease]
  var backendUp: bool

  action init = all {
    now' = 0,
    policy' = Set((AgentA, DBCred), (AgentB, APIKey)),
    issued' = Set(),
    backendUp' = true,
  }

  action tick = all {
    now' = now + 1,
    policy' = policy,
    backendUp' = backendUp,
    issued' = issued.filter(l => l.expires > now + 1),
  }

  action issueAuthorized(p: Principal, s: Secret, ttl: int): bool = all {
    backendUp,
    policy.contains((p, s)),
    ttl > 0,
    ttl <= MAX_TTL,
    issued' = issued.union(Set({ name: s, principal: p, expires: now + ttl })),
    now' = now,
    policy' = policy,
    backendUp' = backendUp,
  }

  action denyUnauthorized(p: Principal, s: Secret): bool = all {
    not(policy.contains((p, s))),
    issued' = issued,
    now' = now,
    policy' = policy,
    backendUp' = backendUp,
  }

  action backendDown = all {
    backendUp,
    backendUp' = false,
    issued' = issued,
    now' = now,
    policy' = policy,
  }

  action backendRecover = all {
    not(backendUp),
    backendUp' = true,
    issued' = issued,
    now' = now,
    policy' = policy,
  }

  action step =
    nondet p = oneOf(Set(AgentA, AgentB, Intruder))
    nondet s = oneOf(Set(DBCred, APIKey, Other))
    nondet ttl = oneOf(Set(60, 300, DEFAULT_TTL, MAX_TTL))
    any {
      issueAuthorized(p, s, ttl),
      denyUnauthorized(p, s),
      tick,
      backendDown,
      backendRecover,
    }

  // ------------------------------ Invariants ----------------------------
  val LeaseImpliesAuthorized: bool =
    issued.forall(l => policy.contains((l.principal, l.name)))

  val LeaseTTLBounded: bool =
    issued.forall(l => l.expires - now <= MAX_TTL)

  val Safety: bool = LeaseImpliesAuthorized and LeaseTTLBounded

  // ------------------------------- Tests --------------------------------
  // Property checking happens via:
  //   quint run --invariant=Safety secrets.qnt
  run authorizedIssueTest = {
    init.then(issueAuthorized(AgentA, DBCred, DEFAULT_TTL))
  }

  run unauthorizedDenyTest = {
    init.then(denyUnauthorized(Intruder, DBCred))
  }

  run ttlBoundedTest = {
    init.then(issueAuthorized(AgentA, DBCred, MAX_TTL))
  }

  run mixedScenario = {
    init
      .then(issueAuthorized(AgentA, DBCred, DEFAULT_TTL))
      .then(denyUnauthorized(Intruder, DBCred))
      .then(issueAuthorized(AgentB, APIKey, 60))
      .then(tick)
      .then(tick)
  }
}
