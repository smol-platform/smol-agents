package fullstack

// Coverage maps each requirement ID from
// .spec-workflow/specs/knative-agents-fullstack-e2e/requirements.md
// to the test that exercises it. The CI coverage gate parses this
// registry and fails if any requirement is unreferenced (R-E2E-VRF-1).
//
// Add a new entry whenever a requirement gets its first test, OR
// whenever a test newly covers a previously-uncovered requirement.
//
// Format: "R-E2E-* ID" → "<package>.<TestName>". The package path is
// relative to test/e2e/fullstack/.
var Coverage = map[string]string{
	// Scenarios — registered via shared.All(), bodies land in Phase 1+.
	"R-E2E-SCN-IDENT-1":    "shared.identityRotation",
	"R-E2E-SCN-PROXY-TCP":  "shared.proxyTCP",
	"R-E2E-SCN-PROXY-HTTP": "shared.proxyHTTP",
	"R-E2E-SCN-EBPF-DROP":  "shared.ebpfDrop",
	"R-E2E-SCN-EBPF-REDIR": "shared.ebpfRedirect",
	"R-E2E-SCN-WG-CLIENT":  "shared.wgClient",
	"R-E2E-SCN-AGENTRUN":   "shared.agentRun",
	"R-E2E-SCN-CANCEL":     "shared.cancel",
	"R-E2E-SCN-WEBHOOK":    "shared.webhook",
	"R-E2E-SCN-KATA":       "shared.kataIsolation",
	// Driver / ring requirements wire up in T-1.* and T-2.*.
}

// CoverageOf returns the test for a requirement, or "" if uncovered.
func CoverageOf(reqID string) string { return Coverage[reqID] }
