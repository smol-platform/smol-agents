package v1

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ValidateAgent runs synchronous admission-time checks on an Agent
// without consulting the cluster (no cross-CR lookups). Cluster
// integrity (Tool refs exist, Provider refs exist) is the controller's
// job at reconcile time.
//
// Implements R-AM-API-1, R-AM-BUD-1.
//
// Mode-aware: when Mode==harness, ModelRef is optional and HarnessSpec
// is required. When Mode==loop (default), ModelRef is required.
func ValidateAgent(a Agent) error {
	var errs []error

	if !a.Spec.Mode.Valid() {
		errs = append(errs, fmt.Errorf("spec.mode=%q is invalid", a.Spec.Mode))
	}

	mode := a.Spec.Mode
	if mode == "" {
		mode = ModeLoop
	}

	switch mode {
	case ModeLoop:
		if a.Spec.Model.ProviderRef == "" {
			errs = append(errs, errors.New("spec.model.providerRef is required (mode=loop)"))
		}
		if a.Spec.Model.Name == "" {
			errs = append(errs, errors.New("spec.model.name is required (mode=loop)"))
		}
		if a.Spec.Harness != nil {
			errs = append(errs, errors.New("spec.harness must be nil when mode=loop"))
		}
	case ModeHarness:
		if a.Spec.Harness == nil {
			errs = append(errs, errors.New("spec.harness is required (mode=harness)"))
		} else if err := ValidateHarness(*a.Spec.Harness); err != nil {
			errs = append(errs, fmt.Errorf("spec.harness: %w", err))
		}
		// Persistent sessions need persistent storage — EXCEPT Hermes, whose
		// cross-turn memory lives gateway-side (keyed by a session id), not in a
		// workspace volume (M3.13 / D6). CLI kinds still require storage.
		if a.Spec.Harness != nil &&
			a.Spec.Harness.SessionPolicy == SessionPersistent &&
			a.Spec.Harness.Kind != HarnessHermes &&
			(a.Spec.Storage == nil || a.Spec.Storage.Kind == StorageNone) {
			errs = append(errs, errors.New("harness.sessionPolicy=persistent requires spec.storage"))
		}
	}

	if strings.TrimSpace(a.Spec.Instructions) == "" {
		errs = append(errs, errors.New("spec.instructions is required"))
	}
	if err := a.Spec.Budget.Validate(); err != nil {
		errs = append(errs, err)
	}
	for i, t := range a.Spec.Tools {
		if t.Name == "" {
			errs = append(errs, fmt.Errorf("spec.tools[%d].name is required", i))
		}
	}
	if err := ValidateStorage(a.Spec.Storage); err != nil {
		errs = append(errs, fmt.Errorf("spec.storage: %w", err))
	}
	if a.Spec.Approval != nil && a.Spec.Approval.ApprovalTimeoutSeconds < 0 {
		errs = append(errs, errors.New("spec.approval.approvalTimeoutSeconds must be >= 0"))
	}
	if a.Spec.Session != nil && a.Spec.Session.Interactive && !a.Spec.Session.Required {
		errs = append(errs, errors.New("spec.session.interactive requires session.required=true (an attach plane needs a resident pod)"))
	}
	if a.Spec.Artifacts != nil {
		errs = append(errs, validateArtifacts(a.Spec)...)
	}

	return errors.Join(errs...)
}

// validateArtifacts checks the ArtifactSpec: it needs AgentFS storage (the
// workspace volume the sidecar reads), unique rule names, and workspace-relative
// globs (no absolute paths or ".." traversal).
func validateArtifacts(spec AgentSpec) []error {
	var errs []error
	if spec.Storage == nil || spec.Storage.Kind != StorageAgentFS {
		errs = append(errs, errors.New("spec.artifacts requires spec.storage.kind=agentfs"))
	}
	seen := map[string]bool{}
	for i, r := range spec.Artifacts.Outputs {
		if r.Name == "" {
			errs = append(errs, fmt.Errorf("spec.artifacts.outputs[%d].name is required", i))
		} else if seen[r.Name] {
			errs = append(errs, fmt.Errorf("spec.artifacts.outputs[%d].name %q is duplicated", i, r.Name))
		}
		seen[r.Name] = true
		if r.Glob == "" {
			errs = append(errs, fmt.Errorf("spec.artifacts.outputs[%d].glob is required", i))
		} else if strings.HasPrefix(r.Glob, "/") || strings.Contains(r.Glob, "..") {
			errs = append(errs, fmt.Errorf("spec.artifacts.outputs[%d].glob must be workspace-relative (no leading / or ..)", i))
		}
	}
	return errs
}

// ValidateTool — R-AM-API-2 + R-AM-TOOL-1.
func ValidateTool(t Tool) error {
	var errs []error
	if t.Name == "" {
		errs = append(errs, errors.New("name is required"))
	}
	if !t.Spec.Kind.Valid() {
		errs = append(errs, fmt.Errorf("spec.kind=%q is invalid", t.Spec.Kind))
	}
	if err := ValidateJSONSchemaShape(t.Spec.InputSchema); err != nil {
		errs = append(errs, fmt.Errorf("spec.inputSchema: %w", err))
	}
	if err := ValidateJSONSchemaShape(t.Spec.OutputSchema); err != nil {
		errs = append(errs, fmt.Errorf("spec.outputSchema: %w", err))
	}
	switch t.Spec.Kind {
	case ToolMCP:
		if t.Spec.MCP == nil || t.Spec.MCP.URL == "" {
			errs = append(errs, errors.New("spec.mcp.url is required for kind=mcp"))
		}
	case ToolHTTP:
		if t.Spec.HTTP == nil || t.Spec.HTTP.URL == "" {
			errs = append(errs, errors.New("spec.http.url is required for kind=http"))
		}
	case ToolAgent:
		if t.Spec.Agent == nil || t.Spec.Agent.Ref.Name == "" {
			errs = append(errs, errors.New("spec.agent.ref.name is required for kind=agent"))
		}
		if t.Spec.Agent != nil && (t.Spec.Agent.MaxTokens < 0 || t.Spec.Agent.TimeoutSeconds < 0) {
			errs = append(errs, errors.New("spec.agent.maxTokens/timeoutSeconds must be >= 0"))
		}
	case ToolFunction:
		if t.Spec.Function == nil || t.Spec.Function.Name == "" {
			errs = append(errs, errors.New("spec.function.name is required for kind=function"))
		}
	case ToolTask:
		// The team shared-task-list tool (P1) needs no extra spec — the op
		// (list|claim|complete|create) + args are supplied at call time. The
		// TaskInvoker is wired only inside a team context (WireTaskInvoker).
	case ToolTeammate:
		// The peer mailbox tool (P2) needs no extra spec — the op (send|receive)
		// + args are supplied at call time. The TeammateInvoker is wired only
		// inside a team context (WireTeammateInvoker).
	case ToolTeamBus:
		// The team bus tool (P5) needs no extra spec — the op
		// (publish|subscribe|receive) + args are supplied at call time. The
		// TeamBusInvoker is wired only inside a team context (WireTeamBusInvoker).
	}
	return errors.Join(errs...)
}

// ValidateModelProvider — R-AM-API-3.
func ValidateModelProvider(p ModelProvider) error {
	var errs []error
	if p.Name == "" {
		errs = append(errs, errors.New("name is required"))
	}
	if p.Spec.Kind == "" {
		errs = append(errs, errors.New("spec.kind is required"))
	}
	if p.Spec.SecretRef.SecretName == "" {
		errs = append(errs, errors.New("spec.secretRef.secretName is required"))
	}
	return errors.Join(errs...)
}

// ValidateAgentRun — R-AM-API-4.
func ValidateAgentRun(r AgentRun) error {
	var errs []error
	if r.Spec.AgentRef == "" {
		errs = append(errs, errors.New("spec.agentRef is required"))
	}
	if len(r.Spec.Input) == 0 {
		errs = append(errs, errors.New("spec.input is required"))
	}
	if r.Spec.BudgetOverride != nil {
		if err := r.Spec.BudgetOverride.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("spec.budgetOverride: %w", err))
		}
	}
	for i, in := range r.Spec.Inputs {
		errs = append(errs, validateRunInput(i, in)...)
	}
	if r.Spec.Decision != nil && r.Spec.Decision.Token == "" {
		errs = append(errs, errors.New("spec.decision.token is required"))
	}
	return errors.Join(errs...)
}

// validateRunInput checks one run input file: a relative, traversal-free path
// and exactly one content source.
func validateRunInput(i int, in RunInputFile) []error {
	var errs []error
	switch {
	case in.Path == "":
		errs = append(errs, fmt.Errorf("spec.inputs[%d].path is required", i))
	case strings.HasPrefix(in.Path, "/"):
		errs = append(errs, fmt.Errorf("spec.inputs[%d].path must be relative", i))
	default:
		for _, seg := range strings.Split(in.Path, "/") {
			if seg == ".." {
				errs = append(errs, fmt.Errorf("spec.inputs[%d].path must not contain a %q segment", i, ".."))
				break
			}
		}
	}
	sources := 0
	if in.Inline != "" {
		sources++
	}
	if in.InlineBase64 != "" {
		sources++
	}
	if in.SecretRef != nil && in.SecretRef.SecretName != "" {
		sources++
	}
	if sources != 1 {
		errs = append(errs, fmt.Errorf("spec.inputs[%d]: exactly one of inline, inlineBase64, secretRef is required", i))
	}
	return errs
}

// ValidateAgentPolicy — R-AM-API-6.
func ValidateAgentPolicy(p AgentPolicy) error {
	if p.Name == "" {
		return errors.New("name is required")
	}
	if p.Spec.MaxBudget != nil {
		if err := p.Spec.MaxBudget.Validate(); err != nil {
			return err
		}
	}
	// Reject a redaction pattern that does not compile, so a bad pattern is
	// caught at admission rather than silently skipped (or panicking) on the
	// fold path. Go's regexp is RE2, so compilation is the only failure mode.
	if p.Spec.Redaction != nil {
		for i, pat := range p.Spec.Redaction.Patterns {
			if _, err := regexp.Compile(pat); err != nil {
				return fmt.Errorf("spec.redaction.patterns[%d]: %w", i, err)
			}
		}
	}
	return nil
}
