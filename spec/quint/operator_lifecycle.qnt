// SPDX-License-Identifier: Apache-2.0
// Quint specification: operator feature-flag lifecycle.
// Implements R-OP-VRF-3.
//
// Models a single SmolAgent CR whose features can be toggled, with
// per-feature prereqs that come and go. Verified invariants:
//
//   FeatureProgresses          — on ∧ prereqs_met → eventually applied & ready.
//   OwnerSafety                — disabled feature is never `applied`.
//   ReadyImpliesAllEnabledReady — Phase=Ready iff every on feature ready.
//   NoForbiddenFeatures        — no on feature whose policy disallows it.
module operator_lifecycle {

  type Feature = Identity | Sandbox | Secrets | EBPF
  type Phase = Pending | Reconciling | Ready | Failed

  pure val ALL_FEATURES: Set[Feature] = Set(Identity, Sandbox, Secrets, EBPF)

  // Per-feature state, expressed as parallel maps so we can iterate.
  var on: Feature -> bool
  var ok: Feature -> bool      // platform policy
  var prereqs: Feature -> bool       // prereqs satisfied
  var applied: Feature -> bool       // operator has applied resources
  var ready: Feature -> bool         // feature reports Ready
  var phase: Phase

  pure def initialEnabled: Feature -> bool =
    ALL_FEATURES.mapBy(_ => true)
  pure def initialAllowed: Feature -> bool =
    ALL_FEATURES.mapBy(_ => true)
  pure def initialFalse: Feature -> bool =
    ALL_FEATURES.mapBy(_ => false)

  action init = all {
    on' = initialEnabled,
    ok' = initialAllowed,
    prereqs' = initialFalse,
    applied' = initialFalse,
    ready' = initialFalse,
    phase' = Pending,
  }

  action toggleEnabled(f: Feature): bool = all {
    // Always allowed to disable; only allowed to enable when policy says so.
    or { on.get(f), ok.get(f) },
    on' = on.set(f, not(on.get(f))),
    ok' = ok,
    prereqs' = prereqs,
    // Disabling clears applied/ready immediately (R-OP-FF-1 #2).
    applied' = if (on.get(f)) applied.set(f, false) else applied,
    ready' = if (on.get(f)) ready.set(f, false) else ready,
    phase' = Pending,
  }

  action prereqArrives(f: Feature): bool = all {
    not(prereqs.get(f)),
    prereqs' = prereqs.set(f, true),
    on' = on,
    ok' = ok,
    applied' = applied,
    ready' = ready,
    phase' = Pending,
  }

  action prereqGoesAway(f: Feature): bool = all {
    prereqs.get(f),
    prereqs' = prereqs.set(f, false),
    // Loss of prereq does NOT auto-unapply; operator must reconcile.
    on' = on,
    ok' = ok,
    applied' = applied,
    ready' = ready.set(f, false),
    phase' = Pending,
  }

  action policyForbids(f: Feature): bool = all {
    ok.get(f),
    ok' = ok.set(f, false),
    // Forbidding an already-on feature triggers disable.
    on' = on.set(f, false),
    prereqs' = prereqs,
    applied' = applied.set(f, false),
    ready' = ready.set(f, false),
    phase' = Pending,
  }

  // The operator reconcile picks up the slack:
  //   if on ∧ prereqs ∧ ok → applied=true, ready=true
  //   if not on                 → applied=false, ready=false
  action reconcile: bool = all {
    on' = on,
    ok' = ok,
    prereqs' = prereqs,
    applied' = ALL_FEATURES.mapBy(f =>
      if (on.get(f) and ok.get(f) and prereqs.get(f)) true else false),
    ready' = ALL_FEATURES.mapBy(f =>
      if (on.get(f) and ok.get(f) and prereqs.get(f)) true else false),
    phase' = if (ALL_FEATURES.forall(f => not(on.get(f)) or (ok.get(f) and prereqs.get(f))))
                Ready
             else
                Pending,
  }

  action step =
    nondet f = oneOf(ALL_FEATURES)
    any {
      toggleEnabled(f),
      prereqArrives(f),
      prereqGoesAway(f),
      policyForbids(f),
      reconcile,
    }

  // ------------------------------ Invariants ----------------------------

  // No disabled feature is "applied".
  val OwnerSafety: bool =
    ALL_FEATURES.forall(f => not(applied.get(f)) or on.get(f))

  // No forbidden feature is on.
  val NoForbiddenFeatures: bool =
    ALL_FEATURES.forall(f => not(on.get(f)) or ok.get(f))

  // Phase=Ready iff every on feature is ready.
  val ReadyImpliesAllEnabledReady: bool =
    phase != Ready or ALL_FEATURES.forall(f => not(on.get(f)) or ready.get(f))

  // Aggregate safety predicate.
  val Safety: bool =
    OwnerSafety and NoForbiddenFeatures and ReadyImpliesAllEnabledReady

  // -------------------------------- Tests --------------------------------
  run happyPath = {
    init
      .then(prereqArrives(Identity))
      .then(prereqArrives(Sandbox))
      .then(prereqArrives(Secrets))
      .then(prereqArrives(EBPF))
      .then(reconcile)
  }

  run disableThenReEnable = {
    init
      .then(toggleEnabled(EBPF))
      .then(reconcile)
      .then(toggleEnabled(EBPF))
      .then(reconcile)
  }
}
