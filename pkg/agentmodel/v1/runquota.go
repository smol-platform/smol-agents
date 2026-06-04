package v1

// AgentRunQuota caps how many AgentRuns may execute concurrently in a namespace
// (and bounds the priority a run may request for the admission queue). It is the
// per-tenant fairness control for mid-scale concurrency (D10). The run
// reconciler enforces it as a soft, eventually-consistent admission gate.
type AgentRunQuota struct {
	Name   string              `json:"name"`
	Spec   AgentRunQuotaSpec   `json:"spec"`
	Status AgentRunQuotaStatus `json:"status,omitempty"`
}

type AgentRunQuotaSpec struct {
	// MaxConcurrentRuns is the namespace-wide cap on simultaneously-Running
	// AgentRuns. 0 = unlimited (falls back to the operator default).
	// +kubebuilder:validation:Minimum=0
	MaxConcurrentRuns int32 `json:"maxConcurrentRuns,omitempty"`
	// MaxPriority bounds spec.priority a run may request (admission queue).
	// 0 = no priority tiers.
	// +kubebuilder:validation:Minimum=0
	MaxPriority int32 `json:"maxPriority,omitempty"`
}

type AgentRunQuotaStatus struct {
	// ActiveRuns is the observed count of Running AgentRuns in the namespace.
	ActiveRuns int32 `json:"activeRuns,omitempty"`
	// QueuedRuns is the count held Pending by the concurrency gate / queue.
	QueuedRuns         int32 `json:"queuedRuns,omitempty"`
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}
