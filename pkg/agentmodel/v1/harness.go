package v1

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// AgentMode discriminates how the runtime executes an Agent.
//
//   - ModeLoop    — the executor runs the plan-act-observe loop with the
//     declared Model + Tools.
//   - ModeHarness — the executor delegates a single bounded run to a
//     subprocess- or HTTP-based "harness" (Claude Code,
//     Codex, Pi, …) and reports its output as the final
//     answer.
type AgentMode string

const (
	ModeLoop    AgentMode = "loop"
	ModeHarness AgentMode = "harness"
)

func (m AgentMode) Valid() bool {
	switch m {
	case ModeLoop, ModeHarness, "":
		return true
	}
	return false
}

// HarnessKind enumerates the harnesses we know how to drive. Adding a
// new one is a matter of registering an implementation in
// pkg/agentruntime/harness — the type system keeps callers honest.
type HarnessKind string

const (
	HarnessClaudeCode  HarnessKind = "claude-code"
	HarnessCodex       HarnessKind = "codex"
	HarnessPi          HarnessKind = "pi"
	HarnessAider       HarnessKind = "aider"
	HarnessGoose       HarnessKind = "goose"
	HarnessGenericCLI  HarnessKind = "generic-cli"
	HarnessGenericHTTP HarnessKind = "generic-http"
	// HarnessHermes drives NousResearch's Hermes Agent gateway via its
	// OpenAI-compatible /v1/chat/completions API (HTTP, like pi/generic-http).
	HarnessHermes HarnessKind = "hermes"
)

// Valid returns true iff k is a known kind. Unknown kinds are rejected
// at admission so a typo doesn't silently fall through to a no-op.
func (k HarnessKind) Valid() bool {
	switch k {
	case HarnessClaudeCode, HarnessCodex, HarnessPi, HarnessAider, HarnessGoose,
		HarnessGenericCLI, HarnessGenericHTTP, HarnessHermes:
		return true
	}
	return false
}

// SessionPolicy controls whether the harness reuses state across runs.
type SessionPolicy string

const (
	SessionEphemeral  SessionPolicy = "ephemeral"  // fresh process / context per Run (default)
	SessionPersistent SessionPolicy = "persistent" // share state via the Agent's Storage
)

// HarnessSpec describes a harness invocation. Mutually exclusive with a
// pure LLM Model when AgentSpec.Mode==harness.
type HarnessSpec struct {
	// Kind selects the implementation; required.
	Kind HarnessKind `json:"kind"`

	// Image is the OCI image that bundles the harness binary. The
	// runtime starts a sidecar container from this image when the Pod
	// is scheduled.
	// +optional
	Image string `json:"image,omitempty"`

	// Version is an optional semver pin. When unset the controller uses
	// the harness's default tag.
	// +optional
	Version string `json:"version,omitempty"`

	// Command overrides the harness entrypoint. Use sparingly; defaults
	// for known harnesses are wired in pkg/agentruntime/harness.
	// +optional
	Command []string `json:"command,omitempty"`

	// Env carries non-secret environment variables. Secrets MUST come
	// through the broker — never inline them here.
	// +optional
	Env []HarnessEnvVar `json:"env,omitempty"`

	// SessionPolicy: ephemeral (default) or persistent (requires Storage).
	// +optional
	SessionPolicy SessionPolicy `json:"sessionPolicy,omitempty"`

	// HTTP carries config for HTTP-based harnesses (kind=pi or
	// kind=generic-http). Ignored for other kinds.
	// +optional
	HTTP *HarnessHTTPSpec `json:"http,omitempty"`

	// CLI carries config for subprocess-based harnesses
	// (kind=claude-code|codex|aider|goose|generic-cli).
	// +optional
	CLI *HarnessCLISpec `json:"cli,omitempty"`
}

// HarnessEnvVar is a typed env var with optional secret broker reference.
type HarnessEnvVar struct {
	Name string `json:"name"`

	// Value is a literal value — visible to the harness process.
	// +optional
	Value string `json:"value,omitempty"`

	// SecretRef points at a broker-managed secret. The runtime fetches
	// the lease and exposes it via env when the harness starts. The raw
	// value never leaves the broker→harness boundary.
	// +optional
	SecretRef *AuthRef `json:"secretRef,omitempty"`
}

// HarnessCLISpec configures subprocess-based harnesses.
type HarnessCLISpec struct {
	// PromptFlag is the CLI flag that takes the input prompt
	// (e.g. `--print`, `-p`, `--prompt`).
	// +optional
	PromptFlag string `json:"promptFlag,omitempty"`

	// WorkingDir is where the harness runs. Set to the AgentFS mount
	// point when Storage.AgentFS is configured.
	// +optional
	WorkingDir string `json:"workingDir,omitempty"`

	// MaxOutputBytes caps stdout capture. Defaults to 1 MiB.
	// +optional
	MaxOutputBytes int32 `json:"maxOutputBytes,omitempty"`

	// PassthroughEnv allows specific host env vars to flow into the
	// harness (e.g. ANTHROPIC_API_KEY when set via the broker).
	// +optional
	PassthroughEnv []string `json:"passthroughEnv,omitempty"`
}

// HarnessHTTPSpec configures HTTP-based harnesses (pi, generic-http).
type HarnessHTTPSpec struct {
	URL string `json:"url"`

	// +optional
	Method string `json:"method,omitempty"` // POST default

	// +optional
	Headers map[string]string `json:"headers,omitempty"`

	// +optional
	Auth *AuthRef `json:"auth,omitempty"`

	// PromptField is the JSON key under which the input prompt is sent
	// (e.g. "prompt" for Pi, "messages" for chat-completions).
	// +optional
	PromptField string `json:"promptField,omitempty"`

	// ResponseField is the dotted JSON path to extract the answer from
	// the response body (e.g. "choices.0.message.content").
	// +optional
	ResponseField string `json:"responseField,omitempty"`

	// Retry configures transient-failure retries (network errors, HTTP 429,
	// 5xx). Unset means a single attempt — no retry — preserving prior behavior.
	// +optional
	Retry *RetrySpec `json:"retry,omitempty"`
}

// RetrySpec configures transient-failure retries for HTTP-based harnesses
// (kind=hermes|pi|generic-http). Retryable failures are network errors, HTTP
// 429, and 5xx; 4xx client errors (auth, bad request) are never retried — a
// retry can't fix them. The zero value (MaxAttempts 0 or 1) is a single
// attempt, so existing agents are unaffected.
//
// Retries always run inside the run's wallclock budget: an admitted Budget has
// MaxWallClockSeconds > 0, so the ctx deadline bounds the whole retry loop.
type RetrySpec struct {
	// MaxAttempts is the TOTAL attempt count (not extra retries): 0 or 1 means
	// one attempt. Clamped to [1,5] at runtime.
	// +optional
	MaxAttempts int32 `json:"maxAttempts,omitempty"`

	// BackoffBaseMs is the base for exponential backoff between attempts
	// (base, 2*base, 4*base, ...). Default 250.
	// +optional
	BackoffBaseMs int32 `json:"backoffBaseMs,omitempty"`

	// MaxBackoffMs caps any single backoff wait, including a server Retry-After.
	// Default 5000.
	// +optional
	MaxBackoffMs int32 `json:"maxBackoffMs,omitempty"`
}

// Attempts returns the total attempt count, clamped to [1,5]. A nil RetrySpec
// (and the zero value) yields 1 — a single attempt, no retry.
func (r *RetrySpec) Attempts() int {
	if r == nil || r.MaxAttempts <= 1 {
		return 1
	}
	if r.MaxAttempts > 5 {
		return 5
	}
	return int(r.MaxAttempts)
}

// BackoffBase is the exponential-backoff base (default 250ms).
func (r *RetrySpec) BackoffBase() time.Duration {
	if r == nil || r.BackoffBaseMs <= 0 {
		return 250 * time.Millisecond
	}
	return time.Duration(r.BackoffBaseMs) * time.Millisecond
}

// MaxBackoff caps a single backoff wait (default 5s).
func (r *RetrySpec) MaxBackoff() time.Duration {
	if r == nil || r.MaxBackoffMs <= 0 {
		return 5 * time.Second
	}
	return time.Duration(r.MaxBackoffMs) * time.Millisecond
}

// WorkingDirOrEmpty returns the CLI working directory if specified,
// otherwise empty (caller chooses the default).
func (h HarnessSpec) WorkingDirOrEmpty() string {
	if h.CLI != nil {
		return h.CLI.WorkingDir
	}
	return ""
}

// EffectiveWorkingDir is the directory the harness actually runs in. An explicit
// HarnessCLISpec.WorkingDir wins; otherwise, when the Agent has durable AgentFS
// storage, the harness runs in the AgentFS mount so its file writes land on the
// backed-up volume (the operator mounts the volume there — see
// builders.AttachStorageFS). Empty means the runtime picks a default (/tmp).
//
// This is the single seam that keeps the harness CWD aligned with the operator's
// mount path. Before it, an AgentFS-backed harness ran in /tmp regardless of the
// mount, so its output never reached the durable volume.
func (a AgentSpec) EffectiveWorkingDir() string {
	if a.Harness != nil {
		if wd := a.Harness.WorkingDirOrEmpty(); wd != "" {
			return wd
		}
	}
	if a.Storage != nil && a.Storage.Kind == StorageAgentFS {
		if a.Storage.AgentFS != nil && a.Storage.AgentFS.MountPath != "" {
			return a.Storage.AgentFS.MountPath
		}
		return DefaultAgentFSMountPath
	}
	return ""
}

// ValidateHarness enforces structural rules. Implements R-AM-API-1
// extension: harness mode requires a kind-appropriate config block.
func ValidateHarness(h HarnessSpec) error {
	var errs []error
	if !h.Kind.Valid() {
		errs = append(errs, fmt.Errorf("harness.kind=%q is invalid", h.Kind))
	}
	if h.SessionPolicy != "" && h.SessionPolicy != SessionEphemeral && h.SessionPolicy != SessionPersistent {
		errs = append(errs, fmt.Errorf("harness.sessionPolicy=%q is invalid", h.SessionPolicy))
	}
	switch h.Kind {
	case HarnessGenericHTTP, HarnessPi, HarnessHermes:
		if h.HTTP == nil || strings.TrimSpace(h.HTTP.URL) == "" {
			errs = append(errs, errors.New("harness.http.url is required for kind="+string(h.Kind)))
		}
		if h.HTTP != nil && h.HTTP.Retry != nil {
			r := h.HTTP.Retry
			if r.MaxAttempts < 0 || r.BackoffBaseMs < 0 || r.MaxBackoffMs < 0 {
				errs = append(errs, errors.New("harness.http.retry values must be ≥ 0"))
			}
		}
	case HarnessClaudeCode, HarnessCodex, HarnessAider, HarnessGoose, HarnessGenericCLI:
		// CLI block is optional — defaults wired by the runtime — but if
		// present its MaxOutputBytes must be sane.
		if h.CLI != nil && h.CLI.MaxOutputBytes < 0 {
			errs = append(errs, errors.New("harness.cli.maxOutputBytes must be ≥ 0"))
		}
	}
	for i, e := range h.Env {
		if e.Name == "" {
			errs = append(errs, fmt.Errorf("harness.env[%d].name is required", i))
		}
		if e.Value != "" && e.SecretRef != nil {
			errs = append(errs, fmt.Errorf("harness.env[%d]: value and secretRef are mutually exclusive", i))
		}
	}
	return errors.Join(errs...)
}
