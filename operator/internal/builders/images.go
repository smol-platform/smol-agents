// Package builders — images.go
//
// Central resolution of the platform's own component images (the workloads the
// operator spawns: agent, secret-proxy, ebpf-loader, agentfs-sidecar). They
// default to the published ghcr location and are repointable for forks/private
// registries via env, so an AgentRun pulls what we ship without per-CR overrides.
package builders

import "os"

const (
	defaultImageRegistry = "ghcr.io/smol-platform/smol-agents"
	defaultImageTag      = "0.1.0"

	// EnvImageRegistry / EnvImageTag override the defaults (set on the operator
	// Deployment). E.g. SMOL_AGENTS_IMAGE_REGISTRY=ghcr.io/acme/smol-agents.
	EnvImageRegistry = "SMOL_AGENTS_IMAGE_REGISTRY"
	EnvImageTag      = "SMOL_AGENTS_IMAGE_TAG"
)

// Image returns the full ref for a platform component image, e.g.
// Image("agent") -> "ghcr.io/smol-platform/smol-agents/agent:0.1.0" (default).
func Image(component string) string {
	return imageRegistry() + "/" + component + ":" + imageTag()
}

func imageRegistry() string {
	if v := os.Getenv(EnvImageRegistry); v != "" {
		return v
	}
	return defaultImageRegistry
}

func imageTag() string {
	if v := os.Getenv(EnvImageTag); v != "" {
		return v
	}
	return defaultImageTag
}
