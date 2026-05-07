// Package fullstack hosts the cross-cutting end-to-end tests for the
// knative-agents project. The tests run at three concentric rings:
//
//	L0  docker-compose on the dev host (no kernel features)
//	L1  kind cluster (eBPF + Pod sandbox; OrbStack on macOS)
//	L2  single-node k0s on AWS Spot bare-metal (Kata-FC microVM)
//
// See `.spec-workflow/specs/knative-agents-fullstack-e2e/` for the
// full design. Each ring's setup lives in its own subpackage with a
// build tag so unit-test runs don't accidentally pull in heavy deps:
//
//	test/e2e/fullstack/l0  // build tag: e2e_l0
//	test/e2e/fullstack/l1  // build tag: e2e_l1
//	test/e2e/fullstack/l2  // build tag: e2e_l2
//
// Scenario logic lives in `shared/scenarios.go` and runs unmodified
// at every ring whose `Env.Capabilities()` advertises the scenario's
// required capabilities. Gating is intentional: a scenario that
// needs Kata is only attempted at L2.
package fullstack
