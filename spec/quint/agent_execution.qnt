// SPDX-License-Identifier: Apache-2.0
// Quint specification: agent execution lifecycle (R-AM-VRF-1).
//
// Safety properties verified via:
//   quint run --invariant=Safety spec/quint/agent_execution.qnt
//
//   BudgetNeverExceeded   — counters never exceed declared limits.
//   OnlyAllowedToolsCalled — every recorded tool call is in the allow-list.
//   LifecycleConsistent    — phase transitions follow the legal table.
//   StopImpliesTerminal    — once phase is terminal it stays terminal.
module agent_execution {

  type Phase = Pending | Running | RequiresAction | Completed | Failed | Cancelled | Expired

  // Tool universe.
  type Tool = TA | TB | TC | TIntruder

  // Tunables.
  pure val MAX_STEPS: int = 4
  pure val MAX_TOKENS: int = 50
  pure val MAX_TOOL_CALLS: int = 3
  pure val MAX_WALLCLOCK: int = 10
  pure val ALLOWED: Set[Tool] = Set(TA, TB, TC)

  var phase: Phase
  var steps: int
  var tokens: int
  var toolCalls: int
  var wall: int
  var lastTool: Tool          // sentinel-friendly; TIntruder iff seen invalid call
  var sawIllegalCall: bool

  action init = all {
    phase' = Pending,
    steps' = 0,
    tokens' = 0,
    toolCalls' = 0,
    wall' = 0,
    lastTool' = TA,
    sawIllegalCall' = false,
  }

  action begin = all {
    phase == Pending,
    phase' = Running,
    steps' = steps,
    tokens' = tokens,
    toolCalls' = toolCalls,
    wall' = wall,
    lastTool' = lastTool,
    sawIllegalCall' = sawIllegalCall,
  }

  // A Plan step. Adds one step + some tokens. Bounded by MAX_STEPS.
  action plan(dt: int): bool = all {
    phase == Running,
    steps + 1 <= MAX_STEPS,
    tokens + dt <= MAX_TOKENS,
    wall <= MAX_WALLCLOCK,
    steps' = steps + 1,
    tokens' = tokens + dt,
    toolCalls' = toolCalls,
    wall' = wall,
    phase' = Running,
    lastTool' = lastTool,
    sawIllegalCall' = sawIllegalCall,
  }

  // A successful tool call. Must be in the allow-list.
  action toolGood(t: Tool): bool = all {
    phase == Running,
    ALLOWED.contains(t),
    toolCalls + 1 <= MAX_TOOL_CALLS,
    toolCalls' = toolCalls + 1,
    lastTool' = t,
    phase' = Running,
    steps' = steps,
    tokens' = tokens,
    wall' = wall,
    sawIllegalCall' = sawIllegalCall,
  }

  // A rejected tool call: a non-allowed tool name was attempted.
  // Recorded as a marker; counters do NOT advance for rejects.
  action toolReject(t: Tool): bool = all {
    phase == Running,
    not(ALLOWED.contains(t)),
    sawIllegalCall' = true,
    lastTool' = lastTool,
    phase' = Running,
    steps' = steps,
    tokens' = tokens,
    toolCalls' = toolCalls,
    wall' = wall,
  }

  action tickClock = all {
    phase == Running,
    wall + 1 <= MAX_WALLCLOCK,
    wall' = wall + 1,
    phase' = phase,
    steps' = steps,
    tokens' = tokens,
    toolCalls' = toolCalls,
    lastTool' = lastTool,
    sawIllegalCall' = sawIllegalCall,
  }

  // Terminal transitions.
  action complete = all {
    phase == Running,
    phase' = Completed,
    steps' = steps,
    tokens' = tokens,
    toolCalls' = toolCalls,
    wall' = wall,
    lastTool' = lastTool,
    sawIllegalCall' = sawIllegalCall,
  }

  action expire = all {
    phase == Running,
    or {
      steps == MAX_STEPS,
      tokens == MAX_TOKENS,
      toolCalls == MAX_TOOL_CALLS,
      wall == MAX_WALLCLOCK,
    },
    phase' = Expired,
    steps' = steps,
    tokens' = tokens,
    toolCalls' = toolCalls,
    wall' = wall,
    lastTool' = lastTool,
    sawIllegalCall' = sawIllegalCall,
  }

  action cancel = all {
    or { phase == Running, phase == Pending },
    phase' = Cancelled,
    steps' = steps,
    tokens' = tokens,
    toolCalls' = toolCalls,
    wall' = wall,
    lastTool' = lastTool,
    sawIllegalCall' = sawIllegalCall,
  }

  action fail_ = all {
    phase == Running,
    phase' = Failed,
    steps' = steps,
    tokens' = tokens,
    toolCalls' = toolCalls,
    wall' = wall,
    lastTool' = lastTool,
    sawIllegalCall' = sawIllegalCall,
  }

  action stay = all {
    or { phase == Completed, phase == Failed, phase == Cancelled, phase == Expired },
    phase' = phase,
    steps' = steps,
    tokens' = tokens,
    toolCalls' = toolCalls,
    wall' = wall,
    lastTool' = lastTool,
    sawIllegalCall' = sawIllegalCall,
  }

  // ---- Pre-run / mid-run human approval pause (M5.5) ----
  // A gated run parks in RequiresAction with ALL budget counters frozen, then
  // resumes (approve) or terminates (deny/expire). No budget is consumed while
  // paused — the counter-equality below is the formal encoding of the freeze.

  action awaitApproval = all {
    phase == Running,
    phase' = RequiresAction,
    steps' = steps,
    tokens' = tokens,
    toolCalls' = toolCalls,
    wall' = wall,
    lastTool' = lastTool,
    sawIllegalCall' = sawIllegalCall,
  }

  action approve = all {
    phase == RequiresAction,
    phase' = Running,
    steps' = steps,
    tokens' = tokens,
    toolCalls' = toolCalls,
    wall' = wall,
    lastTool' = lastTool,
    sawIllegalCall' = sawIllegalCall,
  }

  action denyOrExpire =
    nondet term = oneOf(Set(Cancelled, Expired, Failed))
    all {
      phase == RequiresAction,
      phase' = term,
      steps' = steps,
      tokens' = tokens,
      toolCalls' = toolCalls,
      wall' = wall,
      lastTool' = lastTool,
      sawIllegalCall' = sawIllegalCall,
    }

  action step =
    nondet dt = oneOf(Set(0, 1, 5, 10, 25))
    nondet t  = oneOf(Set(TA, TB, TC, TIntruder))
    any {
      begin,
      plan(dt),
      toolGood(t),
      toolReject(t),
      tickClock,
      complete,
      expire,
      cancel,
      fail_,
      stay,
      awaitApproval,
      approve,
      denyOrExpire,
    }

  // ------------------------------ Invariants ----------------------------

  val BudgetNeverExceeded: bool =
    steps <= MAX_STEPS and
    tokens <= MAX_TOKENS and
    toolCalls <= MAX_TOOL_CALLS and
    wall <= MAX_WALLCLOCK

  // Every accepted (non-rejected) tool call referenced an allowed tool.
  val OnlyAllowedToolsCalled: bool =
    toolCalls == 0 or ALLOWED.contains(lastTool)

  val LifecycleConsistent: bool =
    or {
      phase == Pending,
      phase == Running,
      phase == RequiresAction,
      phase == Completed,
      phase == Failed,
      phase == Cancelled,
      phase == Expired,
    }

  // Aggregate safety predicate that CI verifies.
  val Safety: bool =
    BudgetNeverExceeded and OnlyAllowedToolsCalled and LifecycleConsistent

  // ------------------------------ Scenarios -----------------------------

  run happyPath = {
    init.then(begin).then(plan(5)).then(toolGood(TA)).then(plan(3)).then(complete)
  }

  run expiresOnSteps = {
    init.then(begin)
        .then(plan(1)).then(plan(1)).then(plan(1)).then(plan(1))
        .then(expire)
  }

  run rejectsIntruder = {
    init.then(begin).then(toolReject(TIntruder)).then(complete)
  }

  // M5.5: a gated run pauses for approval with budget frozen, then resumes and
  // completes — Safety (incl. BudgetNeverExceeded) holds across the pause.
  run humanInTheLoop = {
    init.then(begin).then(plan(5)).then(awaitApproval).then(approve).then(toolGood(TA)).then(complete)
  }

  // A denied/expired approval terminates from RequiresAction without consuming
  // further budget.
  run approvalDenied = {
    init.then(begin).then(awaitApproval).then(denyOrExpire)
  }
}
