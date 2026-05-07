// Package config loads and validates the agent's typed YAML configuration.
//
// The configuration schema is documented in
// .spec-workflow/specs/knative-agents-platform/design.md. Environment
// variables prefixed with KNATIVE_AGENTS_ override matching YAML fields
// (e.g. KNATIVE_AGENTS_MODE=strict overrides .mode).
package config
