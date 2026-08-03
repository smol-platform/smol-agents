package v1

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func mustCompile(t *testing.T, pats ...string) []*regexp.Regexp {
	t.Helper()
	out, errs := CompilePatterns(pats)
	if len(errs) != 0 {
		t.Fatalf("CompilePatterns(%v) errs: %v", pats, errs)
	}
	return out
}

func TestRedactJSON_MasksStringValuesOnly(t *testing.T) {
	pats := mustCompile(t, `sk-[A-Za-z0-9]+`)
	in := json.RawMessage(`{"token":"sk-abc123","count":42,"ok":true,"nested":{"key":"sk-deadbeef"},"arr":["sk-1","plain"]}`)
	out := RedactJSON(in, pats)

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("redacted output must be valid JSON: %v (%s)", err, out)
	}
	if m["token"] != RedactionMask {
		t.Errorf("matching string value not masked: %v", m["token"])
	}
	if m["count"].(float64) != 42 {
		t.Errorf("number must be untouched: %v", m["count"])
	}
	if m["ok"].(bool) != true {
		t.Errorf("bool must be untouched: %v", m["ok"])
	}
	if m["nested"].(map[string]any)["key"] != RedactionMask {
		t.Errorf("nested matching value not masked")
	}
	arr := m["arr"].([]any)
	if arr[0] != RedactionMask || arr[1] != "plain" {
		t.Errorf("array masking wrong: %v", arr)
	}
	// keys are never masked even if they'd match.
	keyPats := mustCompile(t, `token`)
	out2 := RedactJSON(json.RawMessage(`{"token":"v"}`), keyPats)
	if !strings.Contains(string(out2), `"token"`) {
		t.Errorf("object key must never be masked: %s", out2)
	}
}

func TestRedactJSON_NoPatterns_Identity(t *testing.T) {
	in := json.RawMessage(`{"a":"sk-secret"}`)
	out := RedactJSON(in, nil)
	if string(out) != string(in) {
		t.Fatalf("empty patterns must be byte-identity: got %s", out)
	}
}

func TestRedactJSON_OpaqueBlob(t *testing.T) {
	pats := mustCompile(t, `secret`)
	// non-JSON payload that matches → whole blob masked (as a JSON string).
	out := RedactJSON(json.RawMessage(`this is a secret line`), pats)
	if string(out) != `"`+RedactionMask+`"` {
		t.Errorf("opaque matching blob should be masked: %s", out)
	}
	// non-JSON payload that does not match → unchanged.
	out2 := RedactJSON(json.RawMessage(`harmless text`), pats)
	if string(out2) != `harmless text` {
		t.Errorf("opaque non-matching blob should be unchanged: %s", out2)
	}
}

func TestRedactJSON_NoSubstringSurvives(t *testing.T) {
	// Property: after redaction, no string *value* matches a pattern.
	pats := mustCompile(t, `sk-[a-z0-9]+`, `password=\w+`)
	in := json.RawMessage(`{"a":"sk-abc","b":{"c":["password=hunter2","fine"]},"n":7}`)
	out := RedactJSON(in, pats)
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	var walk func(any)
	walk = func(x any) {
		switch t2 := x.(type) {
		case string:
			if anyMatch(t2, pats) {
				t.Errorf("a matching string value survived redaction: %q", t2)
			}
		case []any:
			for _, e := range t2 {
				walk(e)
			}
		case map[string]any:
			for _, e := range t2 {
				walk(e)
			}
		}
	}
	walk(v)
}

func TestRedactSteps_MasksFourFieldsOnly(t *testing.T) {
	pats := mustCompile(t, `sk-[a-z0-9]+`)
	steps := []Step{{
		Index:     3,
		Kind:      StepToolCall,
		TokensIn:  100,
		TokensOut: 50,
		Error:     "boom sk-deadbeef",
		ToolCalls: []ToolCallRecord{{
			Tool:       "search",
			Arguments:  json.RawMessage(`{"q":"sk-secretarg"}`),
			Result:     json.RawMessage(`{"r":"sk-secretres"}`),
			Error:      "fail sk-err1",
			DurationMs: 12,
		}},
	}}
	out := RedactSteps(steps, pats)
	s := out[0]
	if s.Index != 3 || s.Kind != StepToolCall || s.TokensIn != 100 || s.TokensOut != 50 {
		t.Errorf("structural fields must be untouched: %+v", s)
	}
	if s.Error != RedactionMask {
		t.Errorf("Step.Error not masked: %q", s.Error)
	}
	tc := s.ToolCalls[0]
	if tc.Tool != "search" || tc.DurationMs != 12 {
		t.Errorf("tool-call structural fields must be untouched: %+v", tc)
	}
	if strings.Contains(string(tc.Arguments), "sk-secretarg") {
		t.Errorf("ToolCall.Arguments not masked: %s", tc.Arguments)
	}
	if strings.Contains(string(tc.Result), "sk-secretres") {
		t.Errorf("ToolCall.Result not masked: %s", tc.Result)
	}
	if tc.Error != RedactionMask {
		t.Errorf("ToolCall.Error not masked: %q", tc.Error)
	}
	// original input must not be mutated.
	if steps[0].Error != "boom sk-deadbeef" {
		t.Errorf("input steps were mutated")
	}
}

func TestDefaultPatterns_MatchesKnownShapes(t *testing.T) {
	cases := []struct {
		name     string
		positive string // realistic fake sample that must match
		negative string // must NOT match
	}{
		{
			name:     "openai/anthropic-style key",
			positive: "sk-abcdEFGH12345678ijklMNOP",
			negative: "sk-tooshort",
		},
		{
			name:     "github token",
			positive: "ghp_ABCDEFGHIJ0123456789klmn",
			negative: "gh_no_infix_1234567890abcd",
		},
		{
			name:     "aws access key id",
			positive: "AKIAABCDEFGHIJ123456",
			negative: "AKIA-not-a-key-shape",
		},
		{
			name:     "slack token",
			positive: "xoxb-1234567890-abcdefghij",
			negative: "xoxq-1234567890-abcdefghij",
		},
		{
			name:     "pem private key block",
			positive: "-----BEGIN RSA PRIVATE KEY-----",
			negative: "-----BEGIN CERTIFICATE-----",
		},
	}
	pats := DefaultPatterns()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !anyMatch(tc.positive, pats) {
				t.Errorf("positive sample %q did not match any default pattern", tc.positive)
			}
			if anyMatch(tc.negative, pats) {
				t.Errorf("negative sample %q unexpectedly matched a default pattern", tc.negative)
			}
		})
	}
	if anyMatch("just some ordinary prose about keys and tokens", pats) {
		t.Errorf("plain prose must not match any default pattern")
	}
}

func TestRedactJSON_DefaultPatterns_MasksFakeKey(t *testing.T) {
	pats := DefaultPatterns()
	in := json.RawMessage(`{"apiKey":"sk-FAKEabcdEFGH12345678","note":"contact ops","retries":3}`)
	out := RedactJSON(in, pats)

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("redacted output must be valid JSON: %v (%s)", err, out)
	}
	if m["apiKey"] != RedactionMask {
		t.Errorf("fake key value not masked: %v", m["apiKey"])
	}
	if m["note"] != "contact ops" {
		t.Errorf("non-secret string must survive untouched: %v", m["note"])
	}
	if m["retries"].(float64) != 3 {
		t.Errorf("number must survive untouched: %v", m["retries"])
	}
}

func TestValidateAgentPolicy_RejectsBadRegex(t *testing.T) {
	err := ValidateAgentPolicy(AgentPolicy{Name: "p", Spec: AgentPolicySpec{Redaction: &RedactionPolicy{Patterns: []string{"("}}}})
	if err == nil {
		t.Fatalf("a non-compiling pattern must be rejected at admission")
	}
	if ok := ValidateAgentPolicy(AgentPolicy{Name: "p", Spec: AgentPolicySpec{Redaction: &RedactionPolicy{Patterns: []string{`sk-\w+`}}}}); ok != nil {
		t.Fatalf("a valid pattern must pass: %v", ok)
	}
}
