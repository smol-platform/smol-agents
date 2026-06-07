package builders

import (
	"encoding/json"
	"testing"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// M3.18: RenderClaudeMCPConfig maps stdio → command/args/env and http/sse →
// type/url; a secretRef env renders as ${NAME}, never the value.
func TestRenderClaudeMCPConfig(t *testing.T) {
	raw, err := RenderClaudeMCPConfig([]pure.MCPServerSpec{
		{Name: "fs", Transport: "stdio", Command: []string{"mcp-fs", "--root", "/w"}, Env: []pure.HarnessEnvVar{
			{Name: "PLAIN", Value: "v"},
			{Name: "TOKEN", SecretRef: &pure.AuthRef{SecretName: "tok"}},
		}},
		{Name: "api", Transport: "http", URL: "https://mcp.example.com"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			Type    string            `json:"type"`
			URL     string            `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("rendered config invalid: %v", err)
	}
	fs := cfg.MCPServers["fs"]
	if fs.Command != "mcp-fs" || len(fs.Args) != 2 || fs.Args[0] != "--root" {
		t.Errorf("stdio command/args = %q %v", fs.Command, fs.Args)
	}
	if fs.Env["PLAIN"] != "v" {
		t.Errorf("literal env = %q, want v", fs.Env["PLAIN"])
	}
	if fs.Env["TOKEN"] != "${TOKEN}" {
		t.Errorf("secretRef env = %q, want ${TOKEN} (never the value)", fs.Env["TOKEN"])
	}
	api := cfg.MCPServers["api"]
	if api.Type != "http" || api.URL != "https://mcp.example.com" {
		t.Errorf("http server = %+v", api)
	}
}

func TestRenderClaudeMCPConfig_Empty(t *testing.T) {
	if raw, err := RenderClaudeMCPConfig(nil); err != nil || raw != nil {
		t.Errorf("no servers → (nil,nil), got (%s,%v)", raw, err)
	}
}
