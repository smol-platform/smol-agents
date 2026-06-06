package v1

import (
	"errors"
	"fmt"
	"net"
	"net/url"
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
	// HarnessClaudeCode runs Anthropic's `claude --print` CLI as a subprocess (CLI kind).
	HarnessClaudeCode HarnessKind = "claude-code"
	// HarnessCodex runs OpenAI's `codex exec` CLI as a subprocess (CLI kind).
	HarnessCodex HarnessKind = "codex"
	// HarnessInflectionPi drives Inflection AI's hosted Pi inference HTTP API
	// (default https://api.inflection.ai/external/api/inference). This is the
	// canonical name; the old "pi" kind was a FALSE FRIEND for Mario Zechner's
	// pi-mono coding-agent CLI and is now the deprecated alias HarnessPi (M4.14).
	HarnessInflectionPi HarnessKind = "inflection-pi"
	// HarnessPi is the DEPRECATED alias for HarnessInflectionPi — still accepted
	// (CanonicalHarnessKind maps it) but emits a deprecation path; new specs
	// should use "inflection-pi".
	HarnessPi HarnessKind = "pi"
	// HarnessPiMono drives Mario Zechner's `pi-mono` coding-agent CLI via an
	// in-pod pi-bridge HTTP server (M4.14/M4.15) — distinct from the hosted
	// Inflection Pi above. Kind registration only here; the HTTP harness impl +
	// bridge land in M4.15/M4.16.
	HarnessPiMono HarnessKind = "pi-mono"
	// HarnessAider runs the `aider` CLI as a subprocess (CLI kind).
	HarnessAider HarnessKind = "aider"
	// HarnessGoose runs Block's `goose run` CLI as a subprocess (CLI kind).
	HarnessGoose HarnessKind = "goose"
	// HarnessGenericCLI runs an arbitrary subprocess; spec.command is required (CLI kind).
	HarnessGenericCLI HarnessKind = "generic-cli"
	// HarnessGenericHTTP POSTs to an arbitrary HTTP+JSON endpoint (HTTP kind).
	HarnessGenericHTTP HarnessKind = "generic-http"
	// HarnessHermes drives NousResearch's Hermes Agent gateway via its
	// OpenAI-compatible /v1/chat/completions API (HTTP kind). Only HTTP kinds
	// (hermes/pi/generic-http) unpack multimodal input.images; CLI kinds drop them.
	HarnessHermes HarnessKind = "hermes"
)

// Valid returns true iff k is a known kind. Unknown kinds are rejected
// at admission so a typo doesn't silently fall through to a no-op.
func (k HarnessKind) Valid() bool {
	switch k {
	case HarnessClaudeCode, HarnessCodex, HarnessInflectionPi, HarnessPi, HarnessPiMono, HarnessAider, HarnessGoose,
		HarnessGenericCLI, HarnessGenericHTTP, HarnessHermes:
		return true
	}
	return false
}

// CanonicalHarnessKind resolves a deprecated kind alias to its canonical kind
// ("pi" → "inflection-pi"); all other kinds pass through unchanged. The harness
// registry resolves through this so a deprecated alias still finds its impl.
func CanonicalHarnessKind(k HarnessKind) HarnessKind {
	if k == HarnessPi {
		return HarnessInflectionPi
	}
	return k
}

// SessionPolicy controls whether a SINGLE harness AgentRun reuses state across
// runs. It is NOT the AgentSession CRD: AgentSession is a separate long-lived
// worker pod for multi-turn conversations over a NATS turn queue (see
// docs/design/durable-session-architecture.md). For CLI kinds, persistent means
// "reuse the Agent's durable AgentFS workspace"; for HTTP kinds (Hermes),
// persistent additionally forwards a stable provider session id while ephemeral
// mints a fresh one (ephemeral = a fresh server-side session, not stateless).
type SessionPolicy string

const (
	SessionEphemeral  SessionPolicy = "ephemeral"  // fresh workspace / server-side session per Run (default)
	SessionPersistent SessionPolicy = "persistent" // reuse AgentFS workspace (CLI) / stable provider session id (HTTP)
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

	// ExtraFlags are appended to the harness CLI invocation (after the kind's
	// fixed flags, before the prompt for prompt-flag kinds). Use for permission/
	// sandbox flags a CLI harness needs in headless mode — e.g. claude-code's
	// "--dangerously-skip-permissions" or "--permission-mode acceptEdits" (the
	// platform's kata + default-deny egress sandbox is the real boundary), or
	// codex's "--ask-for-approval never". Tenant-controlled; keep narrow.
	// +optional
	ExtraFlags []string `json:"extraFlags,omitempty"`

	// OutputFormat selects the CLI's machine-readable output (when it supports
	// one) so the harness can parse tokens/cost/tool-calls. Empty = the kind's
	// default (claude-code/codex parse "json"; generic-cli stays "text").
	// +kubebuilder:validation:Enum=text;json;stream-json
	// +optional
	OutputFormat string `json:"outputFormat,omitempty"`

	// ApprovalMode maps to the CLI's permission posture (claude-code
	// --permission-mode, codex --ask-for-approval). "" / "safe" keep the safe
	// headless default; "never" enables the opt-in danger flag and is
	// admission-refused unless the resolved RuntimeClass is a microVM (D3).
	// +kubebuilder:validation:Enum=safe;acceptEdits;never
	// +optional
	ApprovalMode string `json:"approvalMode,omitempty"`

	// AllowedTools / DisallowedTools map to the CLI's permission allow/deny
	// lists (e.g. claude-code --allowedTools / --disallowedTools).
	// +optional
	AllowedTools []string `json:"allowedTools,omitempty"`
	// +optional
	DisallowedTools []string `json:"disallowedTools,omitempty"`

	// APIKeyHelperSecret opts into short-lived provider credentials for claude-code
	// (M3.20): instead of a static ANTHROPIC_API_KEY, claude is configured with an
	// apiKeyHelper that runs `/agent lease <secret>` to fetch a fresh broker-leased
	// key, and re-invokes it on TTL/401. Empty = the static key (default). The
	// named secret must be one the broker is configured to serve.
	// +optional
	APIKeyHelperSecret string `json:"apiKeyHelperSecret,omitempty"`

	// CodexBaseURL / CodexModel configure codex's provider (M3.21): when BaseURL is
	// set the operator renders ~/.codex/config.toml with a [model_providers.platform]
	// pointing at it (wire_api="responses") and the harness sets CODEX_HOME so codex
	// routes through the platform's Responses gateway instead of the public OpenAI
	// API. Empty = codex's built-in defaults.
	// +optional
	CodexBaseURL string `json:"codexBaseURL,omitempty"`
	// +optional
	CodexModel string `json:"codexModel,omitempty"`

	// MCPServers declares MCP servers claude-code connects to (M3.18). The operator
	// renders them to a claude mcp-config file mounted with the run spec; the
	// harness passes --mcp-config and auto-allows mcp__<name>__* tools. stdio
	// servers must resolve to an operator-approved image (cluster allow-list,
	// D7/D11); http/sse URLs to internal/private hosts are rejected at admission.
	// +optional
	MCPServers []MCPServerSpec `json:"mcpServers,omitempty"`
}

// ClaudeMCPConfigFile is the run-spec ConfigMap key (and filename) holding the
// rendered claude mcp-config; ClaudeMCPConfigPath is where it mounts (must equal
// builders.RunSpecMountPath + "/" + ClaudeMCPConfigFile — the harness reads it
// without importing the operator builders). M3.18.
const (
	ClaudeMCPConfigFile = "claude-mcp.json"
	ClaudeMCPConfigPath = "/etc/smol-agents/run/" + ClaudeMCPConfigFile

	// CodexConfigFile is the run-spec ConfigMap key (and filename) holding the
	// rendered codex config.toml; CodexConfigMountPath is where it mounts. The
	// codex harness copies it to $CODEX_HOME/config.toml at startup (M3.21).
	CodexConfigFile      = "codex-config.toml"
	CodexConfigMountPath = "/etc/smol-agents/run/" + CodexConfigFile
)

// MCPServerSpec declares one MCP server for claude-code (M3.18). Exactly one
// transport: stdio (a Command, restricted to the operator's cluster allow-list)
// or http/sse (a URL — internal/private hosts rejected at admission). Env carries
// non-secret values; MCP secrets come through the broker, never inline.
type MCPServerSpec struct {
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=stdio;http;sse
	Transport string `json:"transport"`
	// +optional
	Command []string `json:"command,omitempty"`
	// +optional
	URL string `json:"url,omitempty"`
	// +optional
	Env []HarnessEnvVar `json:"env,omitempty"`
}

// HarnessHTTPSpec configures HTTP-based harnesses (pi, generic-http).
type HarnessHTTPSpec struct {
	URL string `json:"url"`

	// API selects the Hermes endpoint family: "" / "chat" → /v1/chat/completions
	// (default, back-compat), "responses" → /v1/responses, "runs" → async
	// /v1/runs. Only valid for the Hermes kind (M3.9).
	// +kubebuilder:validation:Enum=chat;responses;runs
	// +optional
	API string `json:"api,omitempty"`

	// Stream prefers a streaming/SSE transport where the API supports it (M3.9).
	// Currently a hint for the async /v1/runs poller. +optional
	Stream bool `json:"stream,omitempty"`

	// PollIntervalMs is the async /v1/runs poll cadence in ms (0 → 1000). M3.11.
	// +kubebuilder:validation:Minimum=0
	// +optional
	PollIntervalMs int32 `json:"pollIntervalMs,omitempty"`

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

	// ImagePolicy governs http(s) image URLs in multimodal Run input. Unset =
	// the secure default (data: URIs only; http(s) URLs rejected).
	// +optional
	ImagePolicy *ImagePolicy `json:"imagePolicy,omitempty"`
}

// ImagePolicy governs http(s) image URLs found in multimodal Run input. An
// http(s) image URL is fetched by the GATEWAY (a separate Service), not the
// sandboxed agent pod — so it is an SSRF/exfil surface AgentNet cannot see. The
// secure default forwards only self-contained data: URIs and rejects http(s)
// URLs; set AllowURLs to opt in, optionally narrowing to AllowedURLHosts.
// Internal/loopback/link-local/metadata targets are always blocked.
type ImagePolicy struct {
	// AllowURLs permits http(s) image URLs. Default false: only inline data:
	// URIs are forwarded; an http(s) URL fails the run with a clear error.
	// +optional
	AllowURLs bool `json:"allowURLs,omitempty"`

	// AllowedURLHosts, when set (and AllowURLs is true), restricts http(s) image
	// URLs to these hostnames (exact, case-insensitive). Empty = any public host.
	// +optional
	AllowedURLHosts []string `json:"allowedURLHosts,omitempty"`
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
	case HarnessGenericHTTP, HarnessPi, HarnessInflectionPi, HarnessHermes:
		if h.HTTP == nil || strings.TrimSpace(h.HTTP.URL) == "" {
			errs = append(errs, errors.New("harness.http.url is required for kind="+string(h.Kind)))
		}
		if h.HTTP != nil {
			switch h.HTTP.API {
			case "", "chat", "responses", "runs":
			default:
				errs = append(errs, fmt.Errorf("harness.http.api=%q is invalid", h.HTTP.API))
			}
			if h.HTTP.API != "" && h.HTTP.API != "chat" && h.Kind != HarnessHermes {
				errs = append(errs, errors.New("harness.http.api (responses/runs) is only valid for kind=hermes"))
			}
		}
		if h.HTTP != nil && h.HTTP.Retry != nil {
			r := h.HTTP.Retry
			if r.MaxAttempts < 0 || r.BackoffBaseMs < 0 || r.MaxBackoffMs < 0 {
				errs = append(errs, errors.New("harness.http.retry values must be ≥ 0"))
			}
		}
	case HarnessClaudeCode, HarnessCodex, HarnessAider, HarnessGoose, HarnessGenericCLI:
		// CLI block is optional — defaults wired by the runtime — but if present
		// its MaxOutputBytes must be sane and its enums valid.
		if h.CLI != nil {
			if h.CLI.MaxOutputBytes < 0 {
				errs = append(errs, errors.New("harness.cli.maxOutputBytes must be ≥ 0"))
			}
			switch h.CLI.OutputFormat {
			case "", "text", "json", "stream-json":
			default:
				errs = append(errs, fmt.Errorf("harness.cli.outputFormat=%q is invalid", h.CLI.OutputFormat))
			}
			switch h.CLI.ApprovalMode {
			case "", "safe", "acceptEdits", "never":
			default:
				errs = append(errs, fmt.Errorf("harness.cli.approvalMode=%q is invalid", h.CLI.ApprovalMode))
			}
			errs = append(errs, validateMCPServers(h.CLI.MCPServers)...)
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

// validateMCPServers checks claude MCP server declarations (M3.18): unique names,
// stdio ⇒ command, http/sse ⇒ an http(s) URL to a non-internal host. The stdio
// IMAGE allow-list (D7/D11) is enforced operator-side (it knows the cluster list).
func validateMCPServers(servers []MCPServerSpec) []error {
	var errs []error
	seen := map[string]bool{}
	for i, s := range servers {
		if strings.TrimSpace(s.Name) == "" {
			errs = append(errs, fmt.Errorf("harness.cli.mcpServers[%d].name is required", i))
		} else if seen[s.Name] {
			errs = append(errs, fmt.Errorf("harness.cli.mcpServers[%d]: duplicate name %q", i, s.Name))
		}
		seen[s.Name] = true
		switch s.Transport {
		case "stdio":
			if len(s.Command) == 0 {
				errs = append(errs, fmt.Errorf("harness.cli.mcpServers[%d] (stdio) requires command", i))
			}
		case "http", "sse":
			if strings.TrimSpace(s.URL) == "" {
				errs = append(errs, fmt.Errorf("harness.cli.mcpServers[%d] (%s) requires url", i, s.Transport))
				continue
			}
			u, err := url.Parse(s.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				errs = append(errs, fmt.Errorf("harness.cli.mcpServers[%d].url %q must be http(s)", i, s.URL))
				continue
			}
			if isInternalMCPHost(u.Hostname()) {
				errs = append(errs, fmt.Errorf("harness.cli.mcpServers[%d].url %q targets an internal/private host (rejected)", i, s.URL))
			}
		default:
			errs = append(errs, fmt.Errorf("harness.cli.mcpServers[%d].transport=%q is invalid (stdio|http|sse)", i, s.Transport))
		}
	}
	return errs
}

// isInternalMCPHost reports whether host is loopback/private/link-local or an
// internal name — a remote MCP URL pointing there is an SSRF/exfil surface the
// agent's egress cage can't see (the gateway, not the pod, dials it).
func isInternalMCPHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || host == "localhost" ||
		strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	return false
}
