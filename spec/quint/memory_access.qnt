// SPDX-License-Identifier: Apache-2.0
// Quint specification: memory access control (tenant isolation + deny-by-default
// operation policy). A document is returned to a caller ONLY when the caller's
// identity is granted `read` on the document's namespace within the caller's
// tenant. A mutation (write/delete) happens ONLY when the caller holds an
// explicit grant for the operation (and a valid TraT when the retriever requires
// one). Implements R-MEM-AUTH-1 / R-MEM-AUTH-2 / R-MEM-SEC-1.
module memory_access {

  // ── Types ────────────────────────────────────────────────────────────────

  // Tenants are the top-level isolation boundary.
  type Tenant = TenantA | TenantB | TenantC

  // Identities (SPIFFE SVIDs) are derived from the tenant path.
  // IdentityX belongs to TenantA, IdentityY to TenantB,
  // IdentityZ to TenantC; Outsider has no valid tenant.
  type Identity = IdentityX | IdentityY | IdentityZ | Outsider

  // Namespaces partition documents within a tenant.
  type Namespace = NsAlpha | NsBeta | NsGamma

  // Operations: read (retrieve/get), write, delete.
  type Operation = OpRead | OpWrite | OpDelete

  // A policy grant is a triple (identity, operation, namespace) within a tenant.
  type Grant = { id: Identity, op: Operation, ns: Namespace }

  // An access event records what a caller attempted and the decision factors
  // captured at the moment of the call. Crucially, sameTenant and hadGrant
  // record the state at call time, so invariants can check the decision logic
  // without being confounded by subsequent grant revocations.
  type AccessEvent = {
    caller:     Identity,
    callerTenant: Tenant,
    docTenant:  Tenant,
    ns:         Namespace,
    op:         Operation,
    sameTenant: bool,  // tenantOf(caller) == docTenant at call time
    hadGrant:   bool,  // (id, op, ns) was in grants at call time
    tratOk:     bool,  // TraT valid (always true for reads; flag for mutations)
    granted:    bool,  // final decision = sameTenant AND hadGrant AND tratOk
  }

  // ── Fixed configuration ───────────────────────────────────────────────────

  // Each identity belongs to exactly one tenant.
  pure def tenantOf(id: Identity): Tenant =
    match id {
      | IdentityX => TenantA
      | IdentityY => TenantB
      | IdentityZ => TenantC
      | Outsider  => TenantC   // Outsider is in TenantC but holds no grants
    }

  pure val allIdentities: Set[Identity] = Set(IdentityX, IdentityY, IdentityZ, Outsider)
  pure val allNamespaces: Set[Namespace] = Set(NsAlpha, NsBeta, NsGamma)
  pure val allOperations: Set[Operation] = Set(OpRead, OpWrite, OpDelete)
  pure val allTenants:    Set[Tenant]    = Set(TenantA, TenantB, TenantC)

  // ── State ─────────────────────────────────────────────────────────────────

  // Grants table: deny-by-default — nothing is allowed unless listed here.
  // Mutable: the platform operator can add/revoke grants at runtime.
  var grants: Set[Grant]

  // Access events observed by the system (both allowed and denied), with all
  // decision factors snapshotted at call time.
  var events: List[AccessEvent]

  // ── Init ──────────────────────────────────────────────────────────────────

  action init = all {
    // A minimal starting policy:
    //   IdentityX: read on NsAlpha (TenantA)
    //   IdentityY: read+write on NsBeta (TenantB)
    // Everything else is denied by default.
    grants' = Set(
      { id: IdentityX, op: OpRead,  ns: NsAlpha },
      { id: IdentityY, op: OpRead,  ns: NsBeta  },
      { id: IdentityY, op: OpWrite, ns: NsBeta  },
    ),
    events' = List(),
  }

  // ── Actions ───────────────────────────────────────────────────────────────

  // A caller retrieves a document. The access is allowed iff:
  //   1. The caller's derived tenant matches the document's tenant.
  //   2. The caller is granted OpRead on the namespace.
  action retrieve(caller: Identity, docTenant: Tenant, ns: Namespace): bool =
    val same = tenantOf(caller) == docTenant
    val hasG = grants.contains({ id: caller, op: OpRead, ns: ns })
    val ok   = same and hasG
    all {
      events' = events.append({
        caller:      caller,
        callerTenant: tenantOf(caller),
        docTenant:   docTenant,
        ns:          ns,
        op:          OpRead,
        sameTenant:  same,
        hadGrant:    hasG,
        tratOk:      true,
        granted:     ok,
      }),
      grants' = grants,
    }

  // A caller writes to a namespace. Allowed iff:
  //   1. Same-tenant (no cross-tenant writes).
  //   2. Granted OpWrite on the namespace.
  //   3. TraT valid (tratOk). When the retriever does not require a TraT, pass true.
  action writeDoc(caller: Identity, docTenant: Tenant, ns: Namespace, tratOk: bool): bool =
    val same = tenantOf(caller) == docTenant
    val hasG = grants.contains({ id: caller, op: OpWrite, ns: ns })
    val ok   = same and hasG and tratOk
    all {
      events' = events.append({
        caller:      caller,
        callerTenant: tenantOf(caller),
        docTenant:   docTenant,
        ns:          ns,
        op:          OpWrite,
        sameTenant:  same,
        hadGrant:    hasG,
        tratOk:      tratOk,
        granted:     ok,
      }),
      grants' = grants,
    }

  // A caller deletes a document. Allowed iff same-tenant + granted OpDelete + TraT.
  action deleteDoc(caller: Identity, docTenant: Tenant, ns: Namespace, tratOk: bool): bool =
    val same = tenantOf(caller) == docTenant
    val hasG = grants.contains({ id: caller, op: OpDelete, ns: ns })
    val ok   = same and hasG and tratOk
    all {
      events' = events.append({
        caller:      caller,
        callerTenant: tenantOf(caller),
        docTenant:   docTenant,
        ns:          ns,
        op:          OpDelete,
        sameTenant:  same,
        hadGrant:    hasG,
        tratOk:      tratOk,
        granted:     ok,
      }),
      grants' = grants,
    }

  // The operator grants a permission (adding to the policy table).
  action grantAccess(id: Identity, op: Operation, ns: Namespace): bool = all {
    grants' = grants.union(Set({ id: id, op: op, ns: ns })),
    events' = events,
  }

  // The operator revokes a permission.
  action revokeAccess(id: Identity, op: Operation, ns: Namespace): bool = all {
    grants' = grants.exclude(Set({ id: id, op: op, ns: ns })),
    events' = events,
  }

  // ── Step (nondeterministic model checker driver) ──────────────────────────

  action step =
    nondet caller    = oneOf(allIdentities)
    nondet docTenant = oneOf(allTenants)
    nondet ns        = oneOf(allNamespaces)
    nondet op        = oneOf(allOperations)
    nondet tratOk    = oneOf(Set(true, false))
    any {
      retrieve(caller, docTenant, ns),
      writeDoc(caller, docTenant, ns, tratOk),
      deleteDoc(caller, docTenant, ns, tratOk),
      grantAccess(caller, op, ns),
      revokeAccess(caller, op, ns),
    }

  // ── Invariants ────────────────────────────────────────────────────────────
  // All decision factors were snapshotted at call time, so invariants are
  // independent of subsequent grant mutations.

  // No cross-tenant read ever succeeds: a granted read implies sameTenant.
  val NoCrossTenantRead: bool =
    events.indices().forall(i =>
      val e = events[i]
      e.op == OpRead and e.granted implies e.sameTenant
    )

  // No cross-tenant write ever succeeds.
  val NoCrossTenantWrite: bool =
    events.indices().forall(i =>
      val e = events[i]
      e.op == OpWrite and e.granted implies e.sameTenant
    )

  // No cross-tenant delete ever succeeds.
  val NoCrossTenantDelete: bool =
    events.indices().forall(i =>
      val e = events[i]
      e.op == OpDelete and e.granted implies e.sameTenant
    )

  // Deny-by-default (reads): access was only granted when a grant existed at call time.
  val ReadImpliesGrant: bool =
    events.indices().forall(i =>
      val e = events[i]
      e.op == OpRead and e.granted implies e.hadGrant
    )

  // Deny-by-default (writes): write only granted when write grant existed at call time.
  val WriteImpliesGrant: bool =
    events.indices().forall(i =>
      val e = events[i]
      e.op == OpWrite and e.granted implies e.hadGrant
    )

  // Deny-by-default (deletes): delete only granted when delete grant existed at call time.
  val DeleteImpliesGrant: bool =
    events.indices().forall(i =>
      val e = events[i]
      e.op == OpDelete and e.granted implies e.hadGrant
    )

  // Mutations require a valid TraT (when the retriever enforces it).
  // Recorded at call time; regardless of later grant changes, a granted
  // mutation had tratOk=true at the moment of the call.
  val MutationRequiresTraT: bool =
    events.indices().forall(i =>
      val e = events[i]
      (e.op == OpWrite or e.op == OpDelete) and e.granted implies e.tratOk
    )

  // Decision correctness: the `granted` flag is exactly the conjunction of
  // sameTenant, hadGrant, and tratOk (as recorded at call time).
  val DecisionCorrect: bool =
    events.indices().forall(i =>
      val e = events[i]
      e.granted == (e.sameTenant and e.hadGrant and e.tratOk)
    )

  // The whole point: no successful access (any operation) is ever cross-tenant;
  // every successful read/write/delete had an explicit grant; mutations needed
  // TraT; and the decision was computed correctly from those factors.
  val Safety: bool =
    NoCrossTenantRead
      and NoCrossTenantWrite
      and NoCrossTenantDelete
      and ReadImpliesGrant
      and WriteImpliesGrant
      and DeleteImpliesGrant
      and MutationRequiresTraT
      and DecisionCorrect

  // ── Named test runs ────────────────────────────────────────────────────────
  // Each run exercises a concrete scenario; the invariant is verified by:
  //   quint run --invariant=Safety --max-steps=30 spec/quint/memory_access.qnt
  // and named runs are checked by:
  //   quint test spec/quint/memory_access.qnt

  // Happy path: IdentityX reads NsAlpha within TenantA — should be granted.
  run happyReadTest = {
    init
      .then(retrieve(IdentityX, TenantA, NsAlpha))
      .then(all {
        assert(events.length() == 1),
        assert(events[0].granted),
        assert(events[0].sameTenant),
        assert(events[0].hadGrant),
        grants' = grants,
        events' = events,
      })
  }

  // Cross-tenant read must be denied even when the caller holds a read grant
  // on the same namespace name in their own tenant.
  run crossTenantDeniedTest = {
    init
      .then(retrieve(IdentityX, TenantB, NsAlpha))
      .then(all {
        assert(events.length() == 1),
        assert(not(events[0].granted)),
        assert(not(events[0].sameTenant)),
        grants' = grants,
        events' = events,
      })
  }

  // Deny-by-default: Outsider (no grants) is denied even for their own tenant.
  run denyByDefaultTest = {
    init
      .then(retrieve(Outsider, TenantC, NsAlpha))
      .then(all {
        assert(events.length() == 1),
        assert(not(events[0].granted)),
        assert(not(events[0].hadGrant)),
        grants' = grants,
        events' = events,
      })
  }

  // Mutation without grant: IdentityX holds no write grant, so write is denied.
  run mutationWithoutGrantDeniedTest = {
    init
      .then(writeDoc(IdentityX, TenantA, NsAlpha, true))
      .then(all {
        assert(events.length() == 1),
        assert(not(events[0].granted)),
        assert(not(events[0].hadGrant)),
        grants' = grants,
        events' = events,
      })
  }

  // TraT missing (tratOk=false) blocks a write even when the grant exists.
  run mutationMissingTraTDeniedTest = {
    init
      .then(writeDoc(IdentityY, TenantB, NsBeta, false))
      .then(all {
        assert(events.length() == 1),
        assert(not(events[0].granted)),
        assert(events[0].hadGrant),      // grant existed, but TraT missing
        assert(not(events[0].tratOk)),
        grants' = grants,
        events' = events,
      })
  }

  // Allowed mutation: IdentityY holds write on NsBeta, TenantB, TraT present.
  run mutationGrantedTest = {
    init
      .then(writeDoc(IdentityY, TenantB, NsBeta, true))
      .then(all {
        assert(events.length() == 1),
        assert(events[0].granted),
        assert(events[0].sameTenant),
        assert(events[0].hadGrant),
        assert(events[0].tratOk),
        grants' = grants,
        events' = events,
      })
  }

  // Grant → revoke cycle: adding then removing a grant correctly denies.
  // After revokeAccess, the subsequent writeDoc has hadGrant=false (snapshotted
  // at the new call time, after revocation).
  run grantRevokeCycleTest = {
    init
      .then(grantAccess(IdentityX, OpWrite, NsAlpha))
      .then(revokeAccess(IdentityX, OpWrite, NsAlpha))
      .then(writeDoc(IdentityX, TenantA, NsAlpha, true))
      .then(all {
        assert(events.length() == 1),
        assert(not(events[0].granted)),
        assert(not(events[0].hadGrant)),   // grant was revoked before this call
        grants' = grants,
        events' = events,
      })
  }
}
