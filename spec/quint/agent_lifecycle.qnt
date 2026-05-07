// SPDX-License-Identifier: Apache-2.0
// Quint specification: agent lifecycle.
// Implements R-RUN-1 / R-RUN-2.
module agent_lifecycle {

  type Phase = Booting | Ready | Draining | Stopped

  type Subsystems = {
    identityReady:  bool,
    transportReady: bool,
    ebpfReady:      bool,
    secretsReady:   bool,
  }

  var phase: Phase
  var inFlight: int
  var subs: Subsystems
  var bpfDetached: bool

  action init = all {
    phase' = Booting,
    inFlight' = 0,
    subs' = { identityReady: false, transportReady: false, ebpfReady: false, secretsReady: false },
    bpfDetached' = false,
  }

  action subsystemsBecomeReady = all {
    phase == Booting,
    subs' = { identityReady: true, transportReady: true, ebpfReady: true, secretsReady: true },
    phase' = Ready,
    inFlight' = inFlight,
    bpfDetached' = bpfDetached,
  }

  action acceptRequest = all {
    phase == Ready,
    inFlight' = inFlight + 1,
    phase' = phase,
    subs' = subs,
    bpfDetached' = bpfDetached,
  }

  action completeRequest = all {
    inFlight > 0,
    inFlight' = inFlight - 1,
    phase' = phase,
    subs' = subs,
    bpfDetached' = bpfDetached,
  }

  action receiveSIGTERM = all {
    phase == Ready,
    phase' = Draining,
    inFlight' = inFlight,
    subs' = subs,
    bpfDetached' = bpfDetached,
  }

  action drainComplete = all {
    phase == Draining,
    inFlight == 0,
    phase' = Stopped,
    bpfDetached' = true,
    subs' = subs,
    inFlight' = inFlight,
  }

  action step = any {
    subsystemsBecomeReady,
    acceptRequest,
    completeRequest,
    receiveSIGTERM,
    drainComplete,
  }

  val ReadyImpliesAllSubsystemsReady: bool =
    phase == Ready implies (subs.identityReady and subs.transportReady and subs.ebpfReady and subs.secretsReady)

  val StopImpliesDetached: bool =
    phase == Stopped implies bpfDetached

  val Safety: bool = ReadyImpliesAllSubsystemsReady and StopImpliesDetached

  // Property checking happens via:
  //   quint run --invariant=Safety agent_lifecycle.qnt
  run happyPath = {
    init
      .then(subsystemsBecomeReady)
      .then(acceptRequest)
      .then(receiveSIGTERM)
      .then(completeRequest)
      .then(drainComplete)
  }

  run drainWaitsForInFlight = {
    init
      .then(subsystemsBecomeReady)
      .then(acceptRequest)
      .then(acceptRequest)
      .then(receiveSIGTERM)
      .then(completeRequest)
      .then(completeRequest)
      .then(drainComplete)
  }
}
