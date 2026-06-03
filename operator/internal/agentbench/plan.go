package agentbench

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// APIVersionBench is the apiVersion stamped on every BenchPlan / BenchCase.
const APIVersionBench = "agentbench/v1"

// Tier classifies a case. Cases in TierFuture are blocked and only run with
// --allow-blocked.
type Tier string

const (
	TierCorrectness Tier = "correctness"
	TierPerf        Tier = "perf"
	TierScale       Tier = "scale"
	TierIsolation   Tier = "isolation"
	TierFuture      Tier = "future"
)

func (t Tier) valid() bool {
	switch t {
	case TierCorrectness, TierPerf, TierScale, TierIsolation, TierFuture:
		return true
	}
	return false
}

// DriverKind selects how a case is submitted.
type DriverKind string

const (
	DriverRun     DriverKind = "run"     // create an AgentRun CR, watch to terminal
	DriverGateway DriverKind = "gateway" // POST a turn to the agentgateway
)

func (d DriverKind) valid() bool {
	switch d {
	case DriverRun, DriverGateway, "":
		return true
	}
	return false
}

// GateOp is a metric comparison operator.
type GateOp string

const (
	GateLTE GateOp = "lte"
	GateGTE GateOp = "gte"
	GateEQ  GateOp = "eq"
)

func (o GateOp) valid() bool {
	switch o {
	case GateLTE, GateGTE, GateEQ:
		return true
	}
	return false
}

// Meta is the trimmed ObjectMeta carried by plan/case docs.
type Meta struct {
	Name string `json:"name"`
}

// FleetSecret is an imperatively-created Secret materialized into the run
// namespace before the fleet applies. stringData values are written verbatim.
type FleetSecret struct {
	Name       string            `json:"name"`
	StringData map[string]string `json:"stringData,omitempty"`
}

// FleetSpec declares the CR fleet + fixtures a plan deploys into its run
// namespace (agentbench-<runID>).
type FleetSpec struct {
	// Secrets are created first (the broker/harness env references them).
	Secrets []FleetSecret `json:"secrets,omitempty"`
	// Manifests are repo-relative paths to YAML applied into the run namespace
	// (ModelProvider / Agent / Tool / AgentSession / fixture Deployments…).
	Manifests []string `json:"manifests,omitempty"`
	// AwaitReady lists object refs (kind/name) to block on before submitting.
	AwaitReady []string `json:"awaitReady,omitempty"`
	// SecretSourceNamespace is where CopySecrets are read from (e.g. a deployed
	// stack like hermes-e2e). Lets the plan reference real provider secrets
	// without committing their values.
	SecretSourceNamespace string `json:"secretSourceNamespace,omitempty"`
	// CopySecrets are secret names copied verbatim from SecretSourceNamespace into
	// the run namespace before manifests apply (the operator bakes their values
	// into the per-run broker config, so they must exist in-namespace).
	CopySecrets []string `json:"copySecrets,omitempty"`
}

// CaseInput is the prompt + nonce knob for a case.
type CaseInput struct {
	Prompt string `json:"prompt"`
	// Nonce, when true, substitutes a fresh per-run 128-bit hex nonce for the
	// literal token {{NONCE}} in the prompt so a stub/replay cannot satisfy a
	// value it never saw.
	Nonce bool `json:"nonce,omitempty"`
}

// Oracle is the discriminated-union oracle config: Kind selects the impl, the
// remaining (kind-specific) keys are carried raw and decoded by the impl.
type Oracle struct {
	Kind string `json:"kind"`
	// Want is the comparison value for output_match / output_jsonpath / secret_*.
	Want string `json:"want,omitempty"`
	// Path is the JSONPath expression for output_jsonpath.
	Path string `json:"path,omitempty"`
	// Equals selects exact (true) vs contains (false) for output_match.
	Equals bool `json:"equals,omitempty"`
	// Raw retains any extra kind-specific keys for future oracles.
	Raw map[string]json.RawMessage `json:"-"`
}

// Gate is a numeric/boolean threshold on an aggregated metric. Want is a raw
// JSON scalar so it accepts a number (latency.p95.ms: 5000), a boolean
// (oracle.pass: true), or a string uniformly.
type Gate struct {
	Metric string          `json:"metric"`
	Op     GateOp          `json:"op"`
	Want   json.RawMessage `json:"want"`
}

// wantFloat parses the gate's want as a float; ok=false for non-numeric wants.
func (g Gate) wantFloat() (float64, bool) {
	var f float64
	if err := json.Unmarshal(g.Want, &f); err != nil {
		return 0, false
	}
	return f, true
}

// wantBool parses the gate's want as a boolean (true / "true" / 1).
func (g Gate) wantBool() bool {
	var b bool
	if json.Unmarshal(g.Want, &b) == nil {
		return b
	}
	var s string
	if json.Unmarshal(g.Want, &s) == nil {
		return s == "true" || s == "1"
	}
	var f float64
	if json.Unmarshal(g.Want, &f) == nil {
		return f != 0
	}
	return false
}

// BlockedSpec parks a case behind --allow-blocked and names the spec that
// unblocks it. A blocked case may only carry a negative oracle.
type BlockedSpec struct {
	Reason      string `json:"reason"`
	UnblockSpec string `json:"unblockSpec,omitempty"`
}

// BenchCase is one benchmark/verification case.
type BenchCase struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Metadata   Meta   `json:"metadata"`

	Tier         Tier         `json:"tier"`
	AgentRef     string       `json:"agentRef"`
	Driver       DriverKind   `json:"driver,omitempty"`
	Samples      int          `json:"samples,omitempty"`
	Seed         int64        `json:"seed,omitempty"`
	RequiredCaps []string     `json:"requiredCaps,omitempty"`
	Input        CaseInput    `json:"input"`
	Oracle       Oracle       `json:"oracle"`
	Gates        []Gate       `json:"gates,omitempty"`
	Blocked      *BlockedSpec `json:"blocked,omitempty"`
}

// BenchPlan is the top-level plan manifest (plan.yaml).
type BenchPlan struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Metadata   Meta      `json:"metadata"`
	Fleet      FleetSpec `json:"fleet,omitempty"`
	// CaseFiles are globs (relative to the plan dir) of *.bench.yaml files.
	CaseFiles []string `json:"caseFiles,omitempty"`
	// Cases are inline cases declared directly in plan.yaml.
	Cases []BenchCase `json:"cases,omitempty"`

	// dir is the directory plan.yaml was loaded from; used to resolve globs +
	// fleet manifest paths. Not serialized.
	dir string `json:"-"`
}

// UnmarshalJSON keeps the discriminated-union extra keys in Oracle.Raw while
// decoding the well-known fields.
func (o *Oracle) UnmarshalJSON(b []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	type plain struct {
		Kind   string `json:"kind"`
		Want   string `json:"want,omitempty"`
		Path   string `json:"path,omitempty"`
		Equals bool   `json:"equals,omitempty"`
	}
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	o.Kind, o.Want, o.Path, o.Equals = p.Kind, p.Want, p.Path, p.Equals
	for _, known := range []string{"kind", "want", "path", "equals"} {
		delete(all, known)
	}
	if len(all) > 0 {
		o.Raw = all
	}
	return nil
}

// LoadPlan reads plan.yaml from dir, expands caseFiles globs, and returns a
// validated BenchPlan with all cases resolved. The plan digest is computed from
// the resolved bytes.
func LoadPlan(planPath string) (*BenchPlan, error) {
	info, err := os.Stat(planPath)
	if err != nil {
		return nil, fmt.Errorf("plan: stat %s: %w", planPath, err)
	}
	dir := planPath
	file := planPath
	if info.IsDir() {
		file = filepath.Join(planPath, "plan.yaml")
	} else {
		dir = filepath.Dir(planPath)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("plan: read %s: %w", file, err)
	}
	var plan BenchPlan
	if err := yaml.Unmarshal(raw, &plan); err != nil {
		return nil, fmt.Errorf("plan: decode %s: %w", file, err)
	}
	plan.dir = dir

	// Expand caseFile globs, appending their decoded cases.
	for _, glob := range plan.CaseFiles {
		matches, gerr := filepath.Glob(filepath.Join(dir, glob))
		if gerr != nil {
			return nil, fmt.Errorf("plan: glob %q: %w", glob, gerr)
		}
		sort.Strings(matches)
		for _, m := range matches {
			cases, cerr := decodeCaseFile(m)
			if cerr != nil {
				return nil, cerr
			}
			plan.Cases = append(plan.Cases, cases...)
		}
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	return &plan, nil
}

// decodeCaseFile decodes a single *.bench.yaml, which may be one BenchCase or a
// list of them (a "cases:" wrapper).
func decodeCaseFile(path string) ([]BenchCase, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("case: read %s: %w", path, err)
	}
	// Try the list-wrapper shape first.
	var wrapper struct {
		Cases []BenchCase `json:"cases"`
	}
	if err := yaml.Unmarshal(raw, &wrapper); err == nil && len(wrapper.Cases) > 0 {
		return wrapper.Cases, nil
	}
	var single BenchCase
	if err := yaml.Unmarshal(raw, &single); err != nil {
		return nil, fmt.Errorf("case: decode %s: %w", path, err)
	}
	if single.Metadata.Name == "" && single.Oracle.Kind == "" {
		return nil, fmt.Errorf("case: %s has no name/oracle (empty doc?)", path)
	}
	return []BenchCase{single}, nil
}

// Digest returns sha256:<hex> over the plan's apiVersion+name+resolved cases —
// recorded in results.json so a run is auditable + re-describable.
func (p *BenchPlan) Digest() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n", p.APIVersion, p.Kind, p.Metadata.Name)
	// Cases are already in deterministic order (glob sorted + inline order).
	for _, c := range p.Cases {
		b, _ := json.Marshal(c)
		h.Write(b)
		h.Write([]byte{'\n'})
	}
	return "sha256:" + fmt.Sprintf("%x", h.Sum(nil))
}

// Dir returns the directory the plan was loaded from.
func (p *BenchPlan) Dir() string { return p.dir }

// CasesForTier returns the subset of cases whose tier matches, or all cases
// when tier is empty. A future-tier case is included only when allowBlocked.
func (p *BenchPlan) CasesForTier(tier Tier, allowBlocked bool) []BenchCase {
	var out []BenchCase
	for _, c := range p.Cases {
		if tier != "" && c.Tier != tier {
			continue
		}
		if (c.Tier == TierFuture || c.Blocked != nil) && !allowBlocked {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Validate enforces the plan/case rules from the design doc.
func (p *BenchPlan) Validate() error {
	var errs []string
	if p.APIVersion != APIVersionBench {
		errs = append(errs, fmt.Sprintf("plan apiVersion=%q, want %q", p.APIVersion, APIVersionBench))
	}
	if p.Kind != "BenchPlan" {
		errs = append(errs, fmt.Sprintf("plan kind=%q, want BenchPlan", p.Kind))
	}
	if p.Metadata.Name == "" {
		errs = append(errs, "plan metadata.name is required")
	}
	if len(p.Cases) == 0 {
		errs = append(errs, "plan has no cases (caseFiles matched nothing and cases is empty)")
	}
	seen := map[string]bool{}
	for i := range p.Cases {
		c := &p.Cases[i]
		ref := c.Metadata.Name
		if ref == "" {
			ref = fmt.Sprintf("cases[%d]", i)
		}
		if c.Metadata.Name != "" {
			if seen[c.Metadata.Name] {
				errs = append(errs, fmt.Sprintf("%s: duplicate case name", ref))
			}
			seen[c.Metadata.Name] = true
		}
		errs = append(errs, validateCase(c, ref)...)
	}
	if len(errs) > 0 {
		return fmt.Errorf("plan validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func validateCase(c *BenchCase, ref string) []string {
	var errs []string
	if c.APIVersion != "" && c.APIVersion != APIVersionBench {
		errs = append(errs, fmt.Sprintf("%s: apiVersion=%q, want %q", ref, c.APIVersion, APIVersionBench))
	}
	if c.Kind != "" && c.Kind != "BenchCase" {
		errs = append(errs, fmt.Sprintf("%s: kind=%q, want BenchCase", ref, c.Kind))
	}
	if c.Metadata.Name == "" {
		errs = append(errs, fmt.Sprintf("%s: metadata.name is required", ref))
	}
	if !c.Tier.valid() {
		errs = append(errs, fmt.Sprintf("%s: tier=%q is not in the enum", ref, c.Tier))
	}
	if c.AgentRef == "" {
		errs = append(errs, fmt.Sprintf("%s: agentRef is required", ref))
	}
	if !c.Driver.valid() {
		errs = append(errs, fmt.Sprintf("%s: driver=%q is invalid (run|gateway)", ref, c.Driver))
	}
	// Oracle kind must be registered.
	if c.Oracle.Kind == "" {
		errs = append(errs, fmt.Sprintf("%s: oracle.kind is required", ref))
	} else if !IsRegistered(c.Oracle.Kind) {
		errs = append(errs, fmt.Sprintf("%s: oracle.kind=%q is not registered", ref, c.Oracle.Kind))
	}
	// Gates: validate ops + the >=3 samples rule for numeric-metric gates.
	hasNumericGate := false
	for gi, g := range c.Gates {
		if g.Metric == "" {
			errs = append(errs, fmt.Sprintf("%s: gates[%d].metric is required", ref, gi))
		}
		if !g.Op.valid() {
			errs = append(errs, fmt.Sprintf("%s: gates[%d].op=%q is invalid (lte|gte|eq)", ref, gi, g.Op))
		}
		if g.Metric != "" && !validGateMetric(g.Metric) {
			errs = append(errs, fmt.Sprintf("%s: gates[%d].metric=%q is unknown (see metricValue in metrics.go)", ref, gi, g.Metric))
		}
		if isNumericMetric(g.Metric) {
			hasNumericGate = true
		}
	}
	if hasNumericGate {
		samples := c.EffectiveSamples()
		if samples < 3 {
			errs = append(errs, fmt.Sprintf("%s: samples=%d but a numeric metric gate requires samples>=3", ref, samples))
		}
	}
	// A blocked case may only carry a NEGATIVE oracle.
	if c.Blocked != nil && !isNegativeOracle(c.Oracle.Kind) {
		errs = append(errs, fmt.Sprintf(
			"%s: blocked case must carry a negative oracle (got %q; want tool_rejected / *_rejected)",
			ref, c.Oracle.Kind))
	}
	return errs
}

// EffectiveSamples returns Samples or 1 when unset.
func (c *BenchCase) EffectiveSamples() int {
	if c.Samples <= 0 {
		return 1
	}
	return c.Samples
}

// EffectiveDriver returns Driver or DriverRun when unset.
func (c *BenchCase) EffectiveDriver() DriverKind {
	if c.Driver == "" {
		return DriverRun
	}
	return c.Driver
}

// isNegativeOracle reports whether an oracle kind asserts a NEGATIVE (a thing
// must NOT work) — only these may back a blocked case.
func isNegativeOracle(kind string) bool {
	return kind == "tool_rejected" || strings.HasSuffix(kind, "_rejected")
}

// isNumericMetric reports whether a gate metric is a number (vs the boolean
// oracle.pass gate), which triggers the samples>=3 rule.
func isNumericMetric(metric string) bool {
	switch metric {
	case "oracle.pass", "":
		return false
	}
	return true
}

// validGateMetric reports whether a gate metric name is one the runner can
// resolve at run time. Keep in sync with metricValue() in metrics.go — an
// unknown metric must fail lint, not silently no-op a gate at run time.
func validGateMetric(m string) bool {
	switch m {
	case "oracle.pass",
		"latency.p50.ms", "latency.p95.ms", "latency.p99.ms", "latency.max.ms",
		"tokens.total", "tokens.max", "tokens.mean",
		"errorRate.pct", "throughput.runsPerMin", "coldStart.ms", "cost.usd":
		return true
	}
	return false
}
