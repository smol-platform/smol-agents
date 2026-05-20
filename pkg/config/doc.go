// Package config loads and validates the agent's typed YAML configuration.
//
// The configuration schema is documented in
// .spec-workflow/specs/smol-agents-platform/design.md. Environment
// variables prefixed with SMOL_AGENTS_ override matching YAML fields
// (e.g. SMOL_AGENTS_MODE=strict overrides .mode).
package config
