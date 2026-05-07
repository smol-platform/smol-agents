// SPDX-License-Identifier: Apache-2.0
// Quint specification: identity (SPIFFE SVID rotation).
// Implements R-IDN-1 / R-IDN-3.
//
// Run with:
//   quint typecheck spec/quint/identity.qnt
//   quint test      spec/quint/identity.qnt
module identity {

  type Mode = Insecure | Permissive | Strict | Degraded

  type SVID = {
    issued:  int,
    expires: int,
  }

  pure val LIFETIME: int = 60
  pure val ROTATE_AT: int = 30
  pure val BOOT_TIMEOUT: int = 5

  var now: int
  var booted: bool
  var mode: Mode
  var current: SVID
  var historyLen: int
  var lastIssued: int
  var workloadAPIUp: bool

  action init = all {
    now' = 0,
    booted' = false,
    mode' = Strict,
    current' = { issued: -1, expires: -1 },
    historyLen' = 0,
    lastIssued' = -1,
    workloadAPIUp' = true,
  }

  action tick = all {
    now' = now + 1,
    booted' = booted,
    mode' = mode,
    current' = current,
    historyLen' = historyLen,
    lastIssued' = lastIssued,
    workloadAPIUp' = workloadAPIUp,
  }

  action issueInitial = all {
    not(booted),
    workloadAPIUp,
    booted' = true,
    current' = { issued: now, expires: now + LIFETIME },
    historyLen' = historyLen + 1,
    lastIssued' = now,
    now' = now,
    mode' = mode,
    workloadAPIUp' = workloadAPIUp,
  }

  action rotate = all {
    booted,
    workloadAPIUp,
    current.expires - now <= ROTATE_AT,
    current' = { issued: now, expires: now + LIFETIME },
    historyLen' = historyLen + 1,
    lastIssued' = now,
    now' = now,
    booted' = booted,
    mode' = mode,
    workloadAPIUp' = workloadAPIUp,
  }

  action workloadAPIDown = all {
    workloadAPIUp,
    workloadAPIUp' = false,
    mode' = Degraded,
    now' = now,
    booted' = booted,
    current' = current,
    historyLen' = historyLen,
    lastIssued' = lastIssued,
  }

  action workloadAPIRecover = all {
    not(workloadAPIUp),
    workloadAPIUp' = true,
    mode' = Strict,
    now' = now,
    booted' = booted,
    current' = current,
    historyLen' = historyLen,
    lastIssued' = lastIssued,
  }

  action step = any {
    issueInitial,
    rotate,
    tick,
    workloadAPIDown,
    workloadAPIRecover,
  }

  // Invariants
  val AlwaysHaveValidSVID: bool =
    booted implies current.expires > now

  val RotationMonotonic: bool =
    booted implies current.issued >= lastIssued or current.issued == lastIssued

  val DegradedWhenAPIDown: bool =
    not(workloadAPIUp) implies mode == Degraded

  val NoStrictWhenDown: bool =
    mode == Strict implies workloadAPIUp

  val Safety: bool =
    AlwaysHaveValidSVID and RotationMonotonic and DegradedWhenAPIDown and NoStrictWhenDown

  // ------------------------------- Tests --------------------------------
  // Concrete scenarios. Property checking happens via:
  //   quint run --invariant=Safety identity.qnt
  run rotateTest = {
    init.then(issueInitial)
        .then(tick)
        .then(tick)
        .then(tick)
        .then(tick)
        .then(tick)
        .then(rotate)
  }

  run degradeTest = {
    init.then(issueInitial)
        .then(workloadAPIDown)
        .then(workloadAPIRecover)
  }

  run safetyHoldsAfterRotate = {
    init.then(issueInitial)
        .then(rotate)
  }
}
