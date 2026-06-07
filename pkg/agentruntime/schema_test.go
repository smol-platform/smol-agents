package agentruntime

import (
	"encoding/json"
	"testing"
)

func TestValidateAgainstSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["q"],"properties":{"q":{"type":"string"}}}`)

	if err := ValidateAgainstSchema(schema, json.RawMessage(`{"q":"hi"}`)); err != nil {
		t.Errorf("valid value rejected: %v", err)
	}
	if err := ValidateAgainstSchema(schema, json.RawMessage(`{}`)); err == nil {
		t.Errorf("missing required field must be rejected")
	}
	if err := ValidateAgainstSchema(schema, json.RawMessage(`{"q":123}`)); err == nil {
		t.Errorf("wrong type must be rejected")
	}
	// empty schema is permissive for well-formed JSON.
	if err := ValidateAgainstSchema(nil, json.RawMessage(`{"anything":true}`)); err != nil {
		t.Errorf("empty schema must accept valid JSON: %v", err)
	}
	// an invalid JSON value is rejected regardless of schema.
	if err := ValidateAgainstSchema(nil, json.RawMessage(`not json`)); err == nil {
		t.Errorf("invalid JSON value must be rejected")
	}
	// a malformed schema surfaces as an error, never silently passes.
	if err := ValidateAgainstSchema(json.RawMessage(`{"type":`), json.RawMessage(`{}`)); err == nil {
		t.Errorf("malformed schema must error")
	}
}
