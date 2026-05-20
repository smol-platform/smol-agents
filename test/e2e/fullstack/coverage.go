package fullstack

// Coverage maps each requirement ID from
// .spec-workflow/specs/smol-agents-fullstack-e2e/requirements.md
// to the test that exercises it. The CI coverage gate parses this
// registry and fails if any requirement is unreferenced (R-E2E-VRF-1).
//
// Add a new entry whenever a requirement gets its first test, OR
// whenever a test newly covers a previously-uncovered requirement.
//
// Format: "R-E2E-* ID" → "<package>.<TestName>". The package path is
// relative to test/e2e/fullstack/.
var Coverage = map[string]string{
	// Scenarios — registered via shared.All(); body land per task.
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
	"R-E2E-SCN-KA-PHASE":   "shared.smolAgentPhase",

	// Driver / ring orchestration: validated by the existence + green
	// pass of each ring's TestLn root test. Each entry below names
	// the test that exercises the requirement structurally.
	"R-E2E-DRV-1": "l0.TestL0",
	"R-E2E-DRV-2": "l1.TestL1",
	"R-E2E-DRV-3": "l2.TestL2",
	"R-E2E-DRV-4": "shared.RunAll",
	"R-E2E-DRV-5": "(structural; t.Cleanup ensures cleanup runs)",

	// L0 ring requirements are exercised by TestL0 + the compose stack.
	"R-E2E-L0-1": "l0.TestL0:spire-server+agent",
	"R-E2E-L0-2": "l0.TestL0:real-binaries",
	"R-E2E-L0-3": "fake-llm.TestServer_PlanFileSequence + l0.TestL0",
	"R-E2E-L0-4": "fake-gateway.TestObserved + l0.TestL0",
	"R-E2E-L0-5": "shared.Caps.Has (capability gating)",

	// L1 ring requirements.
	"R-E2E-L1-1": "l1.TestL1:detect-orbstack-or-native",
	"R-E2E-L1-2": "l1.TestL1:apply-pod-security-label-first",
	"R-E2E-L1-3": "l1.TestL1:agent-sa-precreated",
	"R-E2E-L1-4": "(pending) l1.TestL1:bpf-loader-DS",
	"R-E2E-L1-5": "l1.TestL1:kind-overlay-disables-webhook",
	"R-E2E-L1-6": "l1.TestL1:linux-arm64-kind-load",

	// L2 ring requirements (test bodies pending Phase 5).
	"R-E2E-L2-1": "(pending) l2.TestL2:assume-stigen-us-east-2",
	"R-E2E-L2-2": "(pending) l2.TestL2:spot-c6gd-metal-tagged",
	"R-E2E-L2-3": "(pending) l2.TestL2:wait-ssm-ready",
	"R-E2E-L2-4": "(pending) l2.TestL2:bootstrap-sentinel",
	"R-E2E-L2-5": "(pending) l2.TestL2:terminate-on-cleanup",
	"R-E2E-L2-6": "(pending) l2.TestL2:webhooks-enabled",
	"R-E2E-L2-7": "(pending) l2.TestL2:kata-kernel-distinct",
	"R-E2E-L2-8": "(pending) l2.TestL2:preflight-instance-cap",
	"R-E2E-L2-9": "(pending) l2.TestL2:ecr-image-pull",

	// Cleanup invariants: the structural design + the L2 sweeper
	// Lambda cover them. Each entry references where they're enforced.
	"R-E2E-CLEAN-1": "scripts/aws-l2/sweep.sh + sweeper Lambda",
	"R-E2E-CLEAN-2": "(pending) infra/terraform/aws-e2e/sweeper",
	"R-E2E-CLEAN-3": "(pending) infra/terraform/aws-e2e/budget",
	"R-E2E-CLEAN-4": "(pending) cloud-init: spot interruption hook",
	"R-E2E-CLEAN-5": "(pending) terraform-no-leak property",

	// Cost guardrails — enforced by Terraform input validation +
	// pre-flight checks in the L2 driver.
	"R-E2E-COST-1": "(pending) l2.cluster.go:Spot-only",
	"R-E2E-COST-2": "(pending) l2.cluster.go:c6gd.metal-only",
	"R-E2E-COST-3": "(pending) infra/terraform: no-NAT,no-EBS,single-AZ",
	"R-E2E-COST-4": "(pending) infra/terraform/aws-e2e/budget",

	// Verification meta-requirements.
	"R-E2E-VRF-1": "fullstack.TestCoverageGate (this file)",
	"R-E2E-VRF-2": "Makefile:e2e-l0,e2e-l1,e2e-l2",
	"R-E2E-VRF-3": "Makefile:e2e-clean-aws + scripts/aws-l2/sweep.sh",
	"R-E2E-VRF-4": "(pending) .github/workflows/e2e.yml",
}

// CoverageOf returns the test for a requirement, or "" if uncovered.
func CoverageOf(reqID string) string { return Coverage[reqID] }
