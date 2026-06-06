// Package builders — codex_config.go
//
// Renders codex's ~/.codex/config.toml (M3.21) so codex routes through the
// platform's OpenAI-Responses gateway (HarnessCLISpec.CodexBaseURL) instead of
// the public OpenAI API. The operator writes it as a key in the run-spec
// ConfigMap; the codex harness copies it into a writable $CODEX_HOME at startup
// (codex writes thread state there, so it can't be a read-only mount). The
// provider key is supplied via env (env_key), broker-leased — never in the TOML.
package builders

import (
	"fmt"
	"strings"
)

// codexProviderEnvKey is the env var codex reads the provider key from; the
// broker populates it (never written into config.toml).
const codexProviderEnvKey = "CODEX_API_KEY"

// RenderCodexConfigTOML produces a config.toml pointing codex at the platform
// provider (wire_api="responses") at baseURL. model is optional. Returns "" when
// baseURL is empty (codex keeps its built-in defaults).
func RenderCodexConfigTOML(model, baseURL string) string {
	if strings.TrimSpace(baseURL) == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Rendered by smol-agents (M3.21) — routes codex through the platform Responses gateway.\n")
	b.WriteString("model_provider = \"platform\"\n")
	if strings.TrimSpace(model) != "" {
		fmt.Fprintf(&b, "model = %q\n", model)
	}
	b.WriteString("\n[model_providers.platform]\n")
	b.WriteString("name = \"platform\"\n")
	fmt.Fprintf(&b, "base_url = %q\n", baseURL)
	b.WriteString("wire_api = \"responses\"\n")
	fmt.Fprintf(&b, "env_key = %q\n", codexProviderEnvKey)
	return b.String()
}
