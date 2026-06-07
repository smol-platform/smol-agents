package builders

import (
	"encoding/json"
	"strings"
	"testing"
)

// M4.22: the renderer FORCES the D3 posture (sandbox != off, tools.elevated
// false) regardless of input, binds the control plane, and references the key as
// a broker-resolvable ${VAR} (never cleartext).
func TestRenderOpenClawConfig(t *testing.T) {
	raw := RenderOpenClawConfig("0.0.0.0:18789", "claw-1", "platform", "OPENCLAW_API_KEY")

	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v\n%s", err, raw)
	}
	if sb := cfg["sandbox"].(map[string]any); sb["mode"] == "off" || sb["mode"] == "" {
		t.Errorf("sandbox.mode must be forced non-off, got %v", sb["mode"])
	}
	if tools := cfg["tools"].(map[string]any); tools["elevated"] != false {
		t.Errorf("tools.elevated must be forced false, got %v", tools["elevated"])
	}
	if gw := cfg["gateway"].(map[string]any); gw["bind"] != "0.0.0.0:18789" {
		t.Errorf("gateway.bind = %v", gw["bind"])
	}
	// Key is a broker placeholder, not cleartext.
	if !strings.Contains(raw, "${OPENCLAW_API_KEY}") {
		t.Errorf("key must be a ${VAR} placeholder, got:\n%s", raw)
	}

	// Defaults applied when bind/keyVar omitted.
	d := RenderOpenClawConfig("", "m", "p", "")
	if !strings.Contains(d, "0.0.0.0:18789") || !strings.Contains(d, "${OPENCLAW_API_KEY}") {
		t.Errorf("defaults not applied:\n%s", d)
	}
}
