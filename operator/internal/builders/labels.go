package builders

import (
	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
)

// Labels returns the canonical label set for resources owned by cr.
// Stable across renders so SSA + drift watchers behave deterministically.
func Labels(cr *v1.SmolAgent) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "smol-agents",
		"app.kubernetes.io/instance":   cr.Name,
		"app.kubernetes.io/managed-by": "smol-agents-operator",
		"agents.smol-agents.ai/agent":       cr.Name,
	}
}

// Selector returns the matchLabels subset (no managed-by). Used by
// Service / Deployment selectors.
func Selector(cr *v1.SmolAgent) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "smol-agents",
		"app.kubernetes.io/instance": cr.Name,
	}
}
