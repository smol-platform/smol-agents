package builders

import "encoding/json"

// OpenClawConfigMountPath is where the rendered openclaw.json is mounted; the
// image's start shim links it to ~/.openclaw/openclaw.json (M4.22).
const OpenClawConfigMountPath = "/etc/openclaw"

// OpenClawConfigFile is the rendered config filename.
const OpenClawConfigFile = "openclaw.json"

// RenderOpenClawConfig renders ~/.openclaw/openclaw.json for an OpenClaw daemon
// (M4.22). It FORCES the D3 security posture regardless of caller input —
// sandbox.mode is never "off" and tools.elevated is always false (no nested
// Docker / host escape) — and binds the control plane off-loopback so the
// in-pod gateway can reach :18789 (kept in-pod by the terminal ingress
// NetworkPolicy). Provider credentials are referenced as ${VAR} placeholders the
// broker resolves at start — never written here in cleartext.
//
// bind is the control-plane bind address (e.g. "0.0.0.0:18789"); model/provider
// configure the loop. keyVar is the ${VAR} name the broker fills (e.g.
// "OPENCLAW_API_KEY").
func RenderOpenClawConfig(bind, model, provider, keyVar string) string {
	if bind == "" {
		bind = "0.0.0.0:18789"
	}
	if keyVar == "" {
		keyVar = "OPENCLAW_API_KEY"
	}
	cfg := map[string]any{
		"gateway": map[string]any{
			"bind": bind,
		},
		"model": map[string]any{
			"name":     model,
			"provider": provider,
			// Broker-resolved at start; never cleartext in the rendered config.
			"apiKey": "${" + keyVar + "}",
		},
		// D3 (forced, not caller-overridable): a real sandbox + no elevated tools.
		"sandbox": map[string]any{
			"mode": "workspace",
		},
		"tools": map[string]any{
			"elevated": false,
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return string(b)
}
