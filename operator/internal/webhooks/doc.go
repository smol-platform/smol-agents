// Package webhooks holds the operator's admission webhooks.
//
//   - knativeagent_webhook.go    — validating + defaulting for tenant CRs.
//   - knativeagentplatform.go    — validating for platform singleton.
//
// Implementations are pure functions with no client dependency, plus
// thin sigs.k8s.io/controller-runtime adapters that call them. This
// keeps the rule set unit-testable with no envtest.
package webhooks
