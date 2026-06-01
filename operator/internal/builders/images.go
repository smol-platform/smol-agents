// Package builders — images.go
//
// Central resolution of the platform's own component images (the workloads the
// operator spawns: agent, secret-proxy, ebpf-loader, agentfs-sidecar). They
// default to the published ghcr location and are repointable for forks/private
// registries via env, so an AgentRun pulls what we ship without per-CR overrides.
//
// Per-component overrides exist so a fix to one binary (e.g. the agent) can be
// rolled out without forcing every other component to be rebuilt and tagged
// at the same version — set SMOL_AGENTS_IMAGE_AGENT_TAG=0.1.2 to bump just
// that one, or SMOL_AGENTS_IMAGE_AGENT=<full ref> to swap the whole reference.
package builders

import (
	"os"
	"strings"
)

const (
	defaultImageRegistry = "ghcr.io/smol-platform/smol-agents"
	defaultImageTag      = "0.1.0"

	// EnvImageRegistry / EnvImageTag are the GLOBAL fallbacks: a single
	// registry + tag applied to every spawned component unless a per-component
	// override (below) takes precedence.
	EnvImageRegistry = "SMOL_AGENTS_IMAGE_REGISTRY"
	EnvImageTag      = "SMOL_AGENTS_IMAGE_TAG"

	// envImagePrefix is the common prefix for per-component overrides:
	//   SMOL_AGENTS_IMAGE_<COMPONENT>      — full image ref; wins outright.
	//   SMOL_AGENTS_IMAGE_<COMPONENT>_TAG  — tag-only; uses the global registry.
	// <COMPONENT> is the component name uppercased with '-' replaced by '_'
	// (e.g. "secret-proxy" -> "SECRET_PROXY", "agentfs-sidecar" -> "AGENTFS_SIDECAR").
	// "REGISTRY" and "TAG" are reserved by the globals above; components named
	// "registry" or "tag" would collide.
	envImagePrefix = "SMOL_AGENTS_IMAGE_"
)

// Image returns the full ref for a platform component image, e.g.
// Image("agent") -> "ghcr.io/smol-platform/smol-agents/agent:0.1.0" (default).
//
// Resolution (most specific first):
//  1. SMOL_AGENTS_IMAGE_<COMPONENT>           — full ref for this component only.
//  2. <registry>/<component>:<tag>, where
//     registry = SMOL_AGENTS_IMAGE_REGISTRY        else default
//     tag      = SMOL_AGENTS_IMAGE_<COMPONENT>_TAG else SMOL_AGENTS_IMAGE_TAG else default.
func Image(component string) string {
	suffix := componentEnvSuffix(component)
	if v := os.Getenv(envImagePrefix + suffix); v != "" {
		return v
	}
	return imageRegistry() + "/" + component + ":" + imageTag(suffix)
}

// ImageVersioned is Image but pins the tag to version when non-empty (used for
// HarnessSpec.Version). A full-ref per-component override still wins outright so
// forks/private registries can repoint; otherwise the registry resolves
// normally and version replaces the tag.
func ImageVersioned(component, version string) string {
	if version == "" {
		return Image(component)
	}
	suffix := componentEnvSuffix(component)
	if v := os.Getenv(envImagePrefix + suffix); v != "" {
		return v
	}
	return imageRegistry() + "/" + component + ":" + version
}

// componentEnvSuffix maps a kebab-case component name to an UPPER_SNAKE_CASE
// env-var suffix. "secret-proxy" -> "SECRET_PROXY".
func componentEnvSuffix(component string) string {
	return strings.ToUpper(strings.ReplaceAll(component, "-", "_"))
}

func imageRegistry() string {
	if v := os.Getenv(EnvImageRegistry); v != "" {
		return v
	}
	return defaultImageRegistry
}

func imageTag(componentSuffix string) string {
	if v := os.Getenv(envImagePrefix + componentSuffix + "_TAG"); v != "" {
		return v
	}
	if v := os.Getenv(EnvImageTag); v != "" {
		return v
	}
	return defaultImageTag
}
