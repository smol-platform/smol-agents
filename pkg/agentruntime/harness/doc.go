// Package harness implements concrete agent-harness backends used when
// AgentSpec.Mode==harness. Each Kind in pkg/agentmodel/v1.HarnessKind
// has exactly one Harness implementation here.
//
// Harnesses are deliberately simple: they take an input prompt and a
// budget, run the underlying agent (subprocess or HTTP service), and
// return a structured Result. The plan-act-observe loop is the
// harness's responsibility — we just bound it.
//
// Implementations:
//   - ClaudeCodeHarness  — `claude` CLI (Anthropic Claude Code)
//   - CodexHarness       — OpenAI Codex CLI
//   - PiHarness          — Inflection Pi HTTP API
//   - GenericCLIHarness  — any subprocess that takes a prompt argument
//   - GenericHTTPHarness — any HTTP+JSON endpoint
package harness
