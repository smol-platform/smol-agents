package v1

import (
	"strings"
	"testing"
	"time"
)

// M4.14: pi is a deprecated alias for the canonical inflection-pi; both
// validate, and the alias canonicalizes (the registry resolves through it).
func TestHarnessKind_InflectionPiAlias(t *testing.T) {
	if !HarnessPi.Valid() || !HarnessInflectionPi.Valid() {
		t.Fatalf("both pi (deprecated) and inflection-pi must be valid kinds")
	}
	if CanonicalHarnessKind(HarnessPi) != HarnessInflectionPi {
		t.Errorf("pi must canonicalize to inflection-pi")
	}
	if CanonicalHarnessKind(HarnessHermes) != HarnessHermes {
		t.Errorf("non-alias kinds must pass through unchanged")
	}
	// pi-mono is a DISTINCT kind (Mario Zechner's CLI), NOT an alias of the
	// hosted Inflection pi — it must be valid and canonicalize to itself.
	if !HarnessPiMono.Valid() {
		t.Errorf("pi-mono must be a valid kind")
	}
	if CanonicalHarnessKind(HarnessPiMono) != HarnessPiMono {
		t.Errorf("pi-mono must NOT canonicalize to inflection-pi (it is a separate harness)")
	}
}

// knative-agents-l3x: IsCLI gates TerminationReason redaction — CLI kinds run
// the harness as a subprocess holding the provider credential (not
// agent-blind); HTTP kinds and loop-mode keep the credential in the runtime.
// This must mirror operator/internal/agentbench/oracles.go's isCLIHarness.
func TestHarnessKind_IsCLI(t *testing.T) {
	cli := []HarnessKind{HarnessClaudeCode, HarnessCodex, HarnessAider, HarnessGoose, HarnessGenericCLI, HarnessPiMono, HarnessOpenClaw, HarnessInflectionPi, HarnessPi}
	for _, k := range cli {
		if !k.IsCLI() {
			t.Errorf("%s.IsCLI() = false, want true", k)
		}
	}
	blind := []HarnessKind{HarnessHermes, HarnessGenericHTTP, HarnessKind("loop"), ""}
	for _, k := range blind {
		if k.IsCLI() {
			t.Errorf("%s.IsCLI() = true, want false", k)
		}
	}
}

func TestHarnessKind_Valid(t *testing.T) {
	for _, k := range []HarnessKind{
		HarnessClaudeCode, HarnessCodex, HarnessPi, HarnessPiMono, HarnessInflectionPi, HarnessAider, HarnessGoose,
		HarnessGenericCLI, HarnessGenericHTTP, HarnessHermes,
	} {
		if !k.Valid() {
			t.Errorf("%s should be valid", k)
		}
	}
	if HarnessKind("nope").Valid() {
		t.Error("unknown kind accepted")
	}
}

func TestAgentMode_Valid(t *testing.T) {
	for _, m := range []AgentMode{ModeLoop, ModeHarness, ""} {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
	}
	if AgentMode("garbage").Valid() {
		t.Error("garbage mode accepted")
	}
}

func TestValidateHarness_HTTPRequiresURL(t *testing.T) {
	err := ValidateHarness(HarnessSpec{Kind: HarnessPi})
	if err == nil || !strings.Contains(err.Error(), "http.url") {
		t.Errorf("expected http.url error: %v", err)
	}
	err = ValidateHarness(HarnessSpec{Kind: HarnessPi, HTTP: &HarnessHTTPSpec{URL: "https://api.pi.ai/chat"}})
	if err != nil {
		t.Errorf("with URL: %v", err)
	}
}

// yxh.4: harness.http.auth has no consumer (HTTP harnesses auth via a
// broker-leased HEADER_Authorization env var); setting it would silently yield
// unauthenticated calls, so it must be rejected at admission, not ignored.
func TestValidateHarness_HTTPAuthRejected(t *testing.T) {
	err := ValidateHarness(HarnessSpec{Kind: HarnessHermes, HTTP: &HarnessHTTPSpec{URL: "https://h", Auth: &AuthRef{SecretName: "tok"}}})
	if err == nil || !strings.Contains(err.Error(), "harness.http.auth is not honored") {
		t.Errorf("harness.http.auth must be rejected, got %v", err)
	}
	// Without auth the same harness validates.
	if err := ValidateHarness(HarnessSpec{Kind: HarnessHermes, HTTP: &HarnessHTTPSpec{URL: "https://h"}}); err != nil {
		t.Errorf("hermes harness without http.auth rejected: %v", err)
	}
}

// c5r.2: a kind-specific config block set on a kind that does not read it is a
// mis-author the runtime would silently ignore — reject it.
func TestValidateHarness_ForeignBlockRejected(t *testing.T) {
	// CLI kind carrying an http / piMono block.
	if err := ValidateHarness(HarnessSpec{Kind: HarnessClaudeCode, HTTP: &HarnessHTTPSpec{URL: "https://x"}}); err == nil || !strings.Contains(err.Error(), "harness.http is set but ignored") {
		t.Errorf("CLI kind with http block must be rejected, got %v", err)
	}
	if err := ValidateHarness(HarnessSpec{Kind: HarnessCodex, PiMono: &HarnessPiMonoSpec{}}); err == nil || !strings.Contains(err.Error(), "harness.piMono is set but ignored") {
		t.Errorf("CLI kind with piMono block must be rejected, got %v", err)
	}
	// HTTP kind carrying a cli block (http.url set so only the foreign-block error fires).
	if err := ValidateHarness(HarnessSpec{Kind: HarnessHermes, HTTP: &HarnessHTTPSpec{URL: "https://h"}, CLI: &HarnessCLISpec{}}); err == nil || !strings.Contains(err.Error(), "harness.cli is set but ignored") {
		t.Errorf("HTTP kind with cli block must be rejected, got %v", err)
	}
	// pi-mono carrying a foreign cli block.
	if err := ValidateHarness(HarnessSpec{Kind: HarnessPiMono, CLI: &HarnessCLISpec{}}); err == nil || !strings.Contains(err.Error(), "harness.cli is set but ignored") {
		t.Errorf("pi-mono with cli block must be rejected, got %v", err)
	}
	// Native blocks accepted.
	for _, h := range []HarnessSpec{
		{Kind: HarnessClaudeCode, CLI: &HarnessCLISpec{}},
		{Kind: HarnessHermes, HTTP: &HarnessHTTPSpec{URL: "https://h"}},
		{Kind: HarnessPiMono, PiMono: &HarnessPiMonoSpec{}},
	} {
		if err := ValidateHarness(h); err != nil {
			t.Errorf("native block for kind=%s rejected: %v", h.Kind, err)
		}
	}
}

// M3.18: MCP server validation — transport enum, stdio⇒command, http/sse⇒url,
// internal-host URLs rejected, unique names.
func TestValidateHarness_MCPServers(t *testing.T) {
	mk := func(s MCPServerSpec) error {
		return ValidateHarness(HarnessSpec{Kind: HarnessClaudeCode, CLI: &HarnessCLISpec{MCPServers: []MCPServerSpec{s}}})
	}
	if err := mk(MCPServerSpec{Name: "fs", Transport: "stdio", Command: []string{"mcp-fs"}}); err != nil {
		t.Errorf("valid stdio rejected: %v", err)
	}
	if err := mk(MCPServerSpec{Name: "api", Transport: "http", URL: "https://mcp.example.com"}); err != nil {
		t.Errorf("valid http rejected: %v", err)
	}
	if err := mk(MCPServerSpec{Name: "x", Transport: "stdio"}); err == nil {
		t.Error("stdio without command must be rejected")
	}
	if err := mk(MCPServerSpec{Name: "x", Transport: "http"}); err == nil {
		t.Error("http without url must be rejected")
	}
	if err := mk(MCPServerSpec{Name: "x", Transport: "carrier-pigeon", Command: []string{"c"}}); err == nil {
		t.Error("invalid transport must be rejected")
	}
	for _, bad := range []string{"http://169.254.169.254/x", "http://localhost/x", "http://10.0.0.5/x", "http://svc.internal/x"} {
		if err := mk(MCPServerSpec{Name: "x", Transport: "http", URL: bad}); err == nil {
			t.Errorf("internal-host MCP URL %q must be rejected", bad)
		}
	}
	if err := ValidateHarness(HarnessSpec{Kind: HarnessClaudeCode, CLI: &HarnessCLISpec{MCPServers: []MCPServerSpec{
		{Name: "dup", Transport: "stdio", Command: []string{"a"}},
		{Name: "dup", Transport: "stdio", Command: []string{"b"}},
	}}}); err == nil {
		t.Error("duplicate MCP server names must be rejected")
	}
}

func TestValidateHarness_CLIDefaultsAccepted(t *testing.T) {
	for _, k := range []HarnessKind{HarnessClaudeCode, HarnessCodex, HarnessAider, HarnessGoose, HarnessGenericCLI} {
		if err := ValidateHarness(HarnessSpec{Kind: k}); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
}

// M3.14: the typed CLI seam validates OutputFormat/ApprovalMode enums.
func TestValidateHarness_CLIEnums(t *testing.T) {
	mk := func(of, am string) HarnessSpec {
		return HarnessSpec{Kind: HarnessClaudeCode, CLI: &HarnessCLISpec{OutputFormat: of, ApprovalMode: am}}
	}
	if err := ValidateHarness(mk("json", "never")); err != nil {
		t.Errorf("valid enums rejected: %v", err)
	}
	if err := ValidateHarness(mk("", "")); err != nil {
		t.Errorf("empty (default) enums must pass: %v", err)
	}
	if err := ValidateHarness(mk("yaml", "")); err == nil {
		t.Errorf("bad outputFormat must be rejected")
	}
	if err := ValidateHarness(mk("", "yolo")); err == nil {
		t.Errorf("bad approvalMode must be rejected")
	}
}

// M3.9: the Hermes API discriminator validates its enum and is rejected
// (responses/runs) on non-Hermes HTTP kinds; default ("") is back-compat.
func TestValidateHarness_HTTPAPIDiscriminator(t *testing.T) {
	hermes := func(api string) HarnessSpec {
		return HarnessSpec{Kind: HarnessHermes, HTTP: &HarnessHTTPSpec{URL: "http://gw", API: api}}
	}
	for _, api := range []string{"", "chat", "responses", "runs"} {
		if err := ValidateHarness(hermes(api)); err != nil {
			t.Errorf("hermes api=%q must be valid: %v", api, err)
		}
	}
	if err := ValidateHarness(hermes("bogus")); err == nil {
		t.Errorf("bad api must be rejected")
	}
	// responses/runs on a non-Hermes HTTP kind is rejected; chat/"" is fine.
	gen := func(api string) HarnessSpec {
		return HarnessSpec{Kind: HarnessGenericHTTP, HTTP: &HarnessHTTPSpec{URL: "http://x", API: api}}
	}
	if err := ValidateHarness(gen("responses")); err == nil {
		t.Errorf("api=responses on generic-http must be rejected")
	}
	if err := ValidateHarness(gen("")); err != nil {
		t.Errorf("api=\"\" on generic-http must pass: %v", err)
	}
}

// M3.13: Hermes persistent sessions don't require storage (gateway-side memory,
// D6); CLI kinds still do.
func TestValidateAgent_HermesPersistentNoStorage(t *testing.T) {
	base := func(kind HarnessKind, http *HarnessHTTPSpec) Agent {
		return Agent{Spec: AgentSpec{
			Mode:         ModeHarness,
			Instructions: "x",
			Budget:       Budget{MaxSteps: 1, MaxTokens: 100, MaxWallClockSeconds: 10},
			Harness:      &HarnessSpec{Kind: kind, HTTP: http, SessionPolicy: SessionPersistent},
		}}
	}
	if err := ValidateAgent(base(HarnessHermes, &HarnessHTTPSpec{URL: "http://gw"})); err != nil {
		t.Errorf("Hermes persistent without storage must pass: %v", err)
	}
	if err := ValidateAgent(base(HarnessClaudeCode, nil)); err == nil {
		t.Errorf("claude-code persistent without storage must still be rejected")
	}
}

func TestValidateHarness_EnvSecretMutex(t *testing.T) {
	h := HarnessSpec{Kind: HarnessClaudeCode, Env: []HarnessEnvVar{{
		Name: "KEY", Value: "literal", SecretRef: &AuthRef{SecretName: "k"},
	}}}
	if err := ValidateHarness(h); err == nil {
		t.Error("expected mutex error")
	}
}

func TestValidateAgent_HarnessMode(t *testing.T) {
	a := Agent{Spec: AgentSpec{
		Mode:         ModeHarness,
		Instructions: "code review",
		Budget:       Budget{MaxSteps: 1, MaxTokens: 1000, MaxWallClockSeconds: 60, MaxToolCalls: 0},
		Harness: &HarnessSpec{
			Kind: HarnessClaudeCode,
			CLI:  &HarnessCLISpec{PromptFlag: "--print"},
		},
	}}
	if err := ValidateAgent(a); err != nil {
		t.Errorf("happy harness: %v", err)
	}
	// Harness mode must reject Model presence — actually current validator
	// only checks Harness presence; Model is silently ignored. The rule
	// for "Harness mutually exclusive" is exercised differently:
	a.Spec.Harness = nil
	if err := ValidateAgent(a); err == nil {
		t.Error("harness mode without harness should fail")
	}
}

func TestValidateAgent_LoopModeRejectsHarness(t *testing.T) {
	a := Agent{Spec: AgentSpec{
		Mode:         ModeLoop,
		Model:        ModelRef{ProviderRef: "openai", Name: "gpt-4"},
		Instructions: "be helpful",
		Budget:       Budget{MaxSteps: 1, MaxTokens: 1000, MaxWallClockSeconds: 60, MaxToolCalls: 0},
		Harness:      &HarnessSpec{Kind: HarnessClaudeCode},
	}}
	err := ValidateAgent(a)
	if err == nil || !strings.Contains(err.Error(), "spec.harness must be nil") {
		t.Errorf("expected mutex error: %v", err)
	}
}

func TestValidateAgent_PersistentRequiresStorage(t *testing.T) {
	a := Agent{Spec: AgentSpec{
		Mode:         ModeHarness,
		Instructions: "code",
		Budget:       Budget{MaxSteps: 1, MaxTokens: 1000, MaxWallClockSeconds: 60, MaxToolCalls: 0},
		Harness: &HarnessSpec{
			Kind:          HarnessClaudeCode,
			SessionPolicy: SessionPersistent,
		},
	}}
	err := ValidateAgent(a)
	if err == nil || !strings.Contains(err.Error(), "spec.storage") {
		t.Errorf("expected storage error: %v", err)
	}
	a.Spec.Storage = &StorageSpec{
		Kind:    StorageAgentFS,
		AgentFS: &AgentFSSpec{SizeGiB: 5},
	}
	if err := ValidateAgent(a); err != nil {
		t.Errorf("with storage: %v", err)
	}
}

func TestEffectiveWorkingDir(t *testing.T) {
	agentFS := func(mount string) *StorageSpec {
		return &StorageSpec{Kind: StorageAgentFS, AgentFS: &AgentFSSpec{SizeGiB: 1, MountPath: mount}}
	}
	cases := []struct {
		name string
		spec AgentSpec
		want string
	}{
		{
			name: "explicit CLI working dir wins over storage",
			spec: AgentSpec{
				Harness: &HarnessSpec{Kind: HarnessClaudeCode, CLI: &HarnessCLISpec{WorkingDir: "/work"}},
				Storage: agentFS("/var/agentfs"),
			},
			want: "/work",
		},
		{
			name: "AgentFS mount used when no CLI dir (default mount)",
			spec: AgentSpec{Harness: &HarnessSpec{Kind: HarnessClaudeCode}, Storage: agentFS("")},
			want: DefaultAgentFSMountPath,
		},
		{
			name: "AgentFS custom mount honored",
			spec: AgentSpec{Harness: &HarnessSpec{Kind: HarnessHermes}, Storage: agentFS("/data")},
			want: "/data",
		},
		{
			name: "no storage, no CLI dir → empty (runtime default)",
			spec: AgentSpec{Harness: &HarnessSpec{Kind: HarnessHermes}},
			want: "",
		},
		{
			name: "storage kind none → empty",
			spec: AgentSpec{Harness: &HarnessSpec{Kind: HarnessClaudeCode}, Storage: &StorageSpec{Kind: StorageNone}},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.EffectiveWorkingDir(); got != tc.want {
				t.Errorf("EffectiveWorkingDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRetrySpec_Defaults(t *testing.T) {
	var nilSpec *RetrySpec
	if nilSpec.Attempts() != 1 {
		t.Errorf("nil Attempts = %d, want 1", nilSpec.Attempts())
	}
	if nilSpec.BackoffBase() != 250*time.Millisecond {
		t.Errorf("nil BackoffBase = %v, want 250ms", nilSpec.BackoffBase())
	}
	if nilSpec.MaxBackoff() != 5*time.Second {
		t.Errorf("nil MaxBackoff = %v, want 5s", nilSpec.MaxBackoff())
	}
	// MaxAttempts clamps to [1,5]; 0/1 = single attempt (no retry).
	if (&RetrySpec{MaxAttempts: 0}).Attempts() != 1 {
		t.Error("0 should be a single attempt")
	}
	if (&RetrySpec{MaxAttempts: 99}).Attempts() != 5 {
		t.Error("99 should clamp to 5")
	}
	if (&RetrySpec{MaxAttempts: 3}).Attempts() != 3 {
		t.Error("3 should be 3")
	}
	if (&RetrySpec{BackoffBaseMs: 100}).BackoffBase() != 100*time.Millisecond {
		t.Error("custom BackoffBaseMs not honored")
	}
}

func TestValidateHarness_Retry(t *testing.T) {
	bad := HarnessSpec{Kind: HarnessHermes, HTTP: &HarnessHTTPSpec{URL: "http://x", Retry: &RetrySpec{MaxAttempts: -1}}}
	if err := ValidateHarness(bad); err == nil || !strings.Contains(err.Error(), "retry") {
		t.Errorf("expected retry validation error, got %v", err)
	}
	ok := HarnessSpec{Kind: HarnessHermes, HTTP: &HarnessHTTPSpec{URL: "http://x", Retry: &RetrySpec{MaxAttempts: 3}}}
	if err := ValidateHarness(ok); err != nil {
		t.Errorf("valid retry should pass: %v", err)
	}
}
