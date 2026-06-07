package builders

import (
	"strings"
	"testing"
)

// M3.21: RenderCodexConfigTOML points codex at the platform Responses provider;
// the key is via env_key (broker-leased), never written into the TOML.
func TestRenderCodexConfigTOML(t *testing.T) {
	toml := RenderCodexConfigTOML("o4-mini", "https://gw.example.com/v1")
	for _, want := range []string{
		`model_provider = "platform"`,
		`model = "o4-mini"`,
		`[model_providers.platform]`,
		`base_url = "https://gw.example.com/v1"`,
		`wire_api = "responses"`,
		`env_key = "CODEX_API_KEY"`,
	} {
		if !strings.Contains(toml, want) {
			t.Errorf("config.toml missing %q\n---\n%s", want, toml)
		}
	}
	// Empty baseURL → no config (codex keeps its defaults).
	if RenderCodexConfigTOML("m", "") != "" {
		t.Error("empty baseURL must render no config")
	}
	// Model omitted when empty.
	if strings.Contains(RenderCodexConfigTOML("", "https://x"), "model =") {
		t.Error("empty model must not emit a model line")
	}
}
