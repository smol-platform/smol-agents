// SPDX-License-Identifier: Apache-2.0
// Quint specification: AgentFS 3-way branch merge classifier.
//
// Models the file-level 3-way classification table from Backend.Merge.
// Content is represented as integers: 0 = absent, positive = hash value.
//
// Key invariants:
//   Safety_FailNeverLosesDstChange
//     When policy=fail and the merge is committed (no conflicts), no dst change
//     was silently overwritten — if a conflict would arise, fail commits nothing.
//   Safety_OursAlwaysKeepsDst
//     When policy=ours and dst changed, the dst content is preserved after merge.
//   Safety_TheirsAlwaysReportsConflict
//     When policy=theirs overwrites a dst change, the event is tagged as conflict.
module memory_merge {

  // ── Types ────────────────────────────────────────────────────────────────

  // ConflictPolicy mirrors memory.ConflictPolicy.
  type ConflictPolicy = Fail | Ours | Theirs | Markers | Union

  // A MergeEvent records one classify() call and its observable outcomes.
  type MergeEvent = {
    // base content hash: 0 = absent, positive = distinct hash value.
    base:  int,
    // ours (dst) content hash at merge time.
    ours:  int,
    // theirs (src) content hash at merge time.
    theirs: int,
    // policy applied.
    policy: ConflictPolicy,
    // resulted in a conflict being surfaced.
    hadConflict: bool,
    // the merge overall was committed (whole-merge semantics: fail aborts all).
    committed: bool,
    // the final dst content after merge (0 = deleted).
    finalDst: int,
  }

  // ── State ────────────────────────────────────────────────────────────────

  var events: List[MergeEvent]

  // ── Classification helper ────────────────────────────────────────────────

  // classify returns (hadConflict, committed, finalDst) as a record.
  // base/ours/theirs = 0 means absent; non-zero = content hash.
  // Returned as a record for clarity.
  pure def classify(base: int, ours: int, theirs: int, policy: ConflictPolicy):
      { hadConflict: bool, committed: bool, finalDst: int } = {
    // Both dst and src absent: nothing to do.
    if (ours == 0 and theirs == 0)
      { hadConflict: false, committed: true, finalDst: 0 }
    // Src absent, dst present.
    else if (theirs == 0 and ours != 0) {
      if (base != 0 and ours == base)
        // O unchanged since base; T deleted → take deletion.
        { hadConflict: false, committed: true, finalDst: 0 }
      else if (base != 0 and ours != base)
        // O changed but T deleted → edit/delete conflict.
        match policy {
        | Fail    => { hadConflict: true,  committed: false, finalDst: ours }
        | Ours    => { hadConflict: true,  committed: true,  finalDst: ours }
        | Theirs  => { hadConflict: true,  committed: true,  finalDst: 0 }
        | Markers => { hadConflict: true,  committed: true,  finalDst: ours }
        | Union   => { hadConflict: true,  committed: true,  finalDst: ours }
        }
      else
        // base=0: dst added it independently, src never had it; keep dst.
        { hadConflict: false, committed: true, finalDst: ours }
    }
    // Src present, dst absent.
    else if (ours == 0 and theirs != 0) {
      if (base != 0 and theirs == base)
        // T unchanged; O deleted → keep deleted (preserve O's deletion).
        { hadConflict: false, committed: true, finalDst: 0 }
      else if (base != 0 and theirs != base)
        // T changed but O deleted → edit/delete conflict.
        match policy {
        | Fail    => { hadConflict: true,  committed: false, finalDst: 0 }
        | Ours    => { hadConflict: true,  committed: true,  finalDst: 0 }
        | Theirs  => { hadConflict: true,  committed: true,  finalDst: theirs }
        | Markers => { hadConflict: true,  committed: true,  finalDst: theirs }
        | Union   => { hadConflict: true,  committed: true,  finalDst: theirs }
        }
      else
        // base=0: src added a new file that dst doesn't have → add it.
        { hadConflict: false, committed: true, finalDst: theirs }
    }
    // Both present.
    else {
      if (ours == theirs)
        // Both identical (regardless of base): keep dst.
        { hadConflict: false, committed: true, finalDst: ours }
      else if (base == 0)
        // No base: add/add with different content.
        match policy {
        | Fail    => { hadConflict: true,  committed: false, finalDst: ours }
        | Ours    => { hadConflict: true,  committed: true,  finalDst: ours }
        | Theirs  => { hadConflict: true,  committed: true,  finalDst: theirs }
        | Markers => { hadConflict: true,  committed: true,  finalDst: ours }
        | Union   => { hadConflict: true,  committed: true,  finalDst: ours }
        }
      else if (ours == base and theirs != base)
        // Only T changed: fast-forward take T.
        { hadConflict: false, committed: true, finalDst: theirs }
      else if (ours != base and theirs == base)
        // Only O changed: keep O.
        { hadConflict: false, committed: true, finalDst: ours }
      else
        // Both O and T differ from base and each other: edit/edit conflict.
        match policy {
        | Fail    => { hadConflict: true,  committed: false, finalDst: ours }
        | Ours    => { hadConflict: true,  committed: true,  finalDst: ours }
        | Theirs  => { hadConflict: true,  committed: true,  finalDst: theirs }
        | Markers => { hadConflict: true,  committed: true,  finalDst: ours }
        | Union   => { hadConflict: true,  committed: true,  finalDst: ours }
        }
    }
  }

  // ── Actions ──────────────────────────────────────────────────────────────

  action init = {
    events' = []
  }

  // step: non-deterministically pick a merge scenario and record its outcome.
  action step = {
    nondet base   = oneOf(Set(0, 1, 2))
    nondet ours   = oneOf(Set(0, 1, 2))
    nondet theirs = oneOf(Set(0, 1, 2))
    nondet policy = oneOf(Set(Fail, Ours, Theirs, Markers, Union))
    val r = classify(base, ours, theirs, policy)
    events' = events.append({
      base:        base,
      ours:        ours,
      theirs:      theirs,
      policy:      policy,
      hadConflict: r.hadConflict,
      committed:   r.committed,
      finalDst:    r.finalDst,
    })
  }

  // ── Invariants ────────────────────────────────────────────────────────────

  // Safety_FailNeverLosesDstChange:
  //   If policy=fail and the merge committed (no conflict found),
  //   then no dst *change* was silently overwritten.
  //   A dst change is ours != base (and base != 0 so dst independently changed,
  //   or base == 0 so dst added the file independently).
  //   Clean fast-forwards (ours == base, theirs diverged) are not dst changes
  //   and are permitted even under Fail with committed=true.
  val Safety_FailNeverLosesDstChange: bool =
    events.indices().forall(i => {
      val e = events.nth(i)
      val dstChanged = (e.ours != e.base) or (e.base == 0 and e.ours != 0)
      (e.policy == Fail and e.committed and dstChanged) implies (e.finalDst == e.ours)
    })

  // Safety_OursAlwaysKeepsDst:
  //   If policy=ours, ours was present (non-zero), the merge committed, AND
  //   there was an actual conflict (hadConflict=true — i.e. the two sides diverged),
  //   then the final dst equals ours (our change wins in ours-policy conflicts).
  //   Clean fast-forwards (only theirs changed, no conflict) are exempt: in that
  //   case ours == base so dst didn't actively change, and taking theirs is correct.
  val Safety_OursAlwaysKeepsDst: bool =
    events.indices().forall(i => {
      val e = events.nth(i)
      (e.policy == Ours and e.ours != 0 and e.committed and e.hadConflict)
        implies (e.finalDst == e.ours)
    })

  // Safety_TheirsAlwaysReportsConflict:
  //   If policy=theirs overwrote a dst *change* (ours changed from base,
  //   i.e. dstChanged=true) and the merge committed, then hadConflict must be true.
  //   Clean fast-forwards (ours == base, so dst didn't actively change) are exempt:
  //   those are taken without conflict correctly.
  val Safety_TheirsAlwaysReportsConflict: bool =
    events.indices().forall(i => {
      val e = events.nth(i)
      val dstChanged = (e.ours != e.base) or (e.base == 0 and e.ours != 0)
      (e.policy == Theirs and dstChanged and e.ours != 0 and e.committed
        and e.finalDst != e.ours) implies e.hadConflict
    })

  // Safety: conjunction of all three properties.
  val Safety: bool =
    Safety_FailNeverLosesDstChange
      and Safety_OursAlwaysKeepsDst
      and Safety_TheirsAlwaysReportsConflict
}
