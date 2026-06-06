// Package builders — harness_image.go
//
// Per-kind default harness images. The four known CLI coding agents
// (claude-code, codex, aider, goose) each ship a published image bundling that
// CLI + a shell + git + /agent, so an Agent does NOT need to supply a literal
// harness.image to run them — closing the "every CLI harness needs a custom OCI
// image the operator must build" gap. An explicit harness.image still wins, and
// HarnessSpec.Version pins the tag. HTTP kinds (hermes/pi/generic-http) and
// bring-your-own generic-cli fall back to the base agent image (which already
// carries /agent and makes its calls over HTTP).
package builders

import (
	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// perKindHarnessImage maps a CLI harness kind to its published bundle-image
// component name (resolved via Image/ImageVersioned -> ghcr .../<name>:<tag>).
var perKindHarnessImage = map[pure.HarnessKind]string{
	pure.HarnessClaudeCode: "harness-claude-code",
	pure.HarnessCodex:      "harness-codex",
	pure.HarnessAider:      "harness-aider",
	pure.HarnessGoose:      "harness-goose",
	// pi-mono bundles /agent + /pi-bridge + the pi CLI (M4.17). Unlike the other
	// HTTP kinds it needs the CLI on-image, so it has a per-kind bundle.
	pure.HarnessPiMono: "harness-pi-mono",
}

// HarnessImage resolves the container image for an Agent's harness:
//  1. an explicit harness.image wins;
//  2. a known CLI kind uses its published per-kind bundle (harness-<kind>),
//     tag-pinned by harness.version when set;
//  3. everything else (HTTP kinds, generic-cli without an image, loop mode)
//     uses the base agent image.
func HarnessImage(agent *amv1.Agent) string {
	h := agent.Spec.Harness
	if h == nil {
		return Image("agent")
	}
	if h.Image != "" {
		return h.Image
	}
	if comp, ok := perKindHarnessImage[h.Kind]; ok {
		return ImageVersioned(comp, h.Version)
	}
	return Image("agent")
}
