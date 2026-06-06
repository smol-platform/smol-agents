// Package builders — mcp_config.go
//
// Renders claude-code's MCP server config (M3.18) from HarnessCLISpec.MCPServers
// into the JSON claude expects for --mcp-config. The operator writes it as a key
// in the run-spec ConfigMap (mounted at RunSpecMountPath), and the claude harness
// passes --mcp-config <RunSpecMountPath>/claude-mcp.json + auto-allows the
// mcp__<server>__* tools. MCP secrets are NOT inlined: a secretRef env renders as
// ${NAME} so claude expands it from the broker-populated container env.
package builders

import (
	"encoding/json"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// RenderClaudeMCPConfig produces the claude mcp-config JSON for the given servers.
// stdio → {command, args, env}; http/sse → {type, url}. Returns nil for no servers.
func RenderClaudeMCPConfig(servers []pure.MCPServerSpec) ([]byte, error) {
	if len(servers) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(servers))
	for _, s := range servers {
		switch s.Transport {
		case "stdio":
			cfg := map[string]any{}
			if len(s.Command) > 0 {
				cfg["command"] = s.Command[0]
				if len(s.Command) > 1 {
					cfg["args"] = s.Command[1:]
				}
			}
			if env := mcpEnv(s.Env); len(env) > 0 {
				cfg["env"] = env
			}
			out[s.Name] = cfg
		case "http", "sse":
			out[s.Name] = map[string]any{"type": s.Transport, "url": s.URL}
		}
	}
	return json.MarshalIndent(map[string]any{"mcpServers": out}, "", "  ")
}

// mcpEnv maps an MCP server's env: a literal Value passes through; a secretRef
// renders as ${NAME} (a placeholder claude expands from the container env the
// broker populates) — the secret value is never written into the config.
func mcpEnv(vars []pure.HarnessEnvVar) map[string]string {
	if len(vars) == 0 {
		return nil
	}
	m := make(map[string]string, len(vars))
	for _, e := range vars {
		if e.SecretRef != nil {
			m[e.Name] = "${" + e.Name + "}"
		} else {
			m[e.Name] = e.Value
		}
	}
	return m
}
