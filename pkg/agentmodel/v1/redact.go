package v1

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sync"
)

// RedactionMask replaces any string value (or opaque blob) that matches a
// redaction pattern. It is a fixed sentinel so downstream consumers can detect
// that redaction occurred.
const RedactionMask = "[REDACTED]"

// DefaultSecretPatterns are always-on redaction patterns for well-known
// credential shapes, applied even when no namespace AgentPolicy declares any
// redaction.Patterns. Kept narrow and shape-based (no generic "secret"/"key"
// keyword matching) to avoid mass false positives.
var DefaultSecretPatterns = []string{
	`sk-[A-Za-z0-9_-]{16,}`,              // OpenAI/Anthropic-style API keys
	`gh[pousr]_[A-Za-z0-9]{16,}`,         // GitHub PAT/OAuth/server/refresh tokens
	`AKIA[0-9A-Z]{16}`,                   // AWS access key id
	`xox[baprs]-[A-Za-z0-9-]{10,}`,       // Slack tokens
	`-----BEGIN [A-Z ]*PRIVATE KEY-----`, // PEM private key block
}

// defaultPats compiles DefaultSecretPatterns once. These are our own constant
// patterns, so a compile failure would be a programming error caught by
// TestDefaultPatterns_MatchesKnownShapes (a dropped pattern fails its positive
// sample); ignoring CompilePatterns' per-pattern errs here is deliberate.
var defaultPats = sync.OnceValue(func() []*regexp.Regexp {
	pats, _ := CompilePatterns(DefaultSecretPatterns)
	return pats
})

// DefaultPatterns returns the compiled DefaultSecretPatterns set. This is a
// *disclosure* control on the cluster-facing record, NOT containment: the
// harness already observed the unredacted data and can exfil it over the
// egress floor. This must never be documented as DLP
// (agentpolicy-enforcement R1).
func DefaultPatterns() []*regexp.Regexp {
	return defaultPats()
}

// CompilePatterns compiles a set of redaction pattern strings, skipping (and
// reporting) any that fail to compile so one bad pattern never disables
// redaction of the rest. Returns the compiled set and the per-pattern errors.
// Go's regexp is RE2 (linear-time, no catastrophic backtracking), so a
// compiled pattern is safe to run on attacker-influenced strings.
func CompilePatterns(patterns []string) ([]*regexp.Regexp, []error) {
	var out []*regexp.Regexp
	var errs []error
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("pattern %q: %w", p, err))
			continue
		}
		out = append(out, re)
	}
	return out, errs
}

// RedactJSON walks a JSON document and replaces every *string value* that
// matches any pattern with RedactionMask. Object keys, numbers, booleans and
// null are never touched, so the document shape and machine-readable scalars
// survive. If raw does not decode as JSON it is treated as an opaque secret:
// the whole blob is masked iff any pattern matches the raw bytes, otherwise it
// is returned unchanged. An empty pattern set is the identity (byte-for-byte).
//
// Redaction is a *disclosure* control on the cluster-facing record, NOT
// containment: the harness already observed the unredacted data and can exfil
// it over the egress floor. This must never be documented as DLP
// (agentpolicy-enforcement R1).
func RedactJSON(raw json.RawMessage, pats []*regexp.Regexp) json.RawMessage {
	if len(pats) == 0 || len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// Opaque (non-JSON) payload: mask the whole blob if any pattern hits.
		if anyMatch(string(raw), pats) {
			b, _ := json.Marshal(RedactionMask)
			return b
		}
		return raw
	}
	b, err := json.Marshal(redactValue(v, pats))
	if err != nil {
		return raw
	}
	return b
}

// redactValue recurses through decoded JSON, masking string leaves only. It
// mutates the maps/slices in place; callers pass a freshly-decoded value, so
// no shared state is affected.
func redactValue(v any, pats []*regexp.Regexp) any {
	switch t := v.(type) {
	case string:
		if anyMatch(t, pats) {
			return RedactionMask
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = redactValue(e, pats)
		}
		return t
	case map[string]any:
		for k, e := range t {
			t[k] = redactValue(e, pats) // value only — keys are never masked
		}
		return t
	default:
		return v // numbers, bools, null
	}
}

// RedactString returns RedactionMask if s matches any pattern, s unchanged
// otherwise. An empty pattern set is the identity.
func RedactString(s string, pats []*regexp.Regexp) string {
	if anyMatch(s, pats) {
		return RedactionMask
	}
	return s
}

func anyMatch(s string, pats []*regexp.Regexp) bool {
	for _, p := range pats {
		if p != nil && p.MatchString(s) {
			return true
		}
	}
	return false
}

// RedactSteps returns a copy of steps with every free-text field masked where
// a pattern matches: Step.Error and each ToolCallRecord's Arguments, Result
// and Error. Structural/typed fields (Index, Kind, timestamps, token counts,
// DurationMs) are left intact. An empty pattern set is the identity.
func RedactSteps(steps []Step, pats []*regexp.Regexp) []Step {
	if len(pats) == 0 || len(steps) == 0 {
		return steps
	}
	out := make([]Step, len(steps))
	for i, s := range steps {
		s.Error = RedactString(s.Error, pats)
		if len(s.ToolCalls) > 0 {
			tcs := make([]ToolCallRecord, len(s.ToolCalls))
			for j, tc := range s.ToolCalls {
				tc.Arguments = RedactJSON(tc.Arguments, pats)
				tc.Result = RedactJSON(tc.Result, pats)
				tc.Error = RedactString(tc.Error, pats)
				tcs[j] = tc
			}
			s.ToolCalls = tcs
		}
		out[i] = s
	}
	return out
}
