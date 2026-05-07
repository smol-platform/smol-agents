package v1

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ValidateJSONSchemaShape performs lightweight checks that a raw schema
// at least *looks* like a JSON Schema document: parsable JSON, top-level
// "type" or "$ref", and an object root if "type":"object".
//
// We deliberately keep this simple — full JSON Schema 2020-12 validation
// is delegated to santhosh-tekuri/jsonschema in the runtime path; this
// helper is purely for fast admission-time rejection.
//
// Implements R-AM-TOOL-1 acceptance #1.
func ValidateJSONSchemaShape(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("agentmodel: empty schema")
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("agentmodel: schema not valid JSON: %w", err)
	}
	if _, hasType := m["type"]; hasType {
		return nil
	}
	if _, hasRef := m["$ref"]; hasRef {
		return nil
	}
	if _, hasOneOf := m["oneOf"]; hasOneOf {
		return nil
	}
	if _, hasAnyOf := m["anyOf"]; hasAnyOf {
		return nil
	}
	return errors.New("agentmodel: schema lacks 'type', '$ref', 'oneOf', or 'anyOf'")
}

// MatchesSchema is the runtime-side check. It is implemented as a stub
// here that round-trips through `json.RawMessage` to ensure the input
// is well-formed JSON; the full JSON Schema validation lives in
// pkg/agentruntime/schema.go where the dependency is acceptable.
func MatchesSchema(schema, value json.RawMessage) error {
	if err := ValidateJSONSchemaShape(schema); err != nil {
		return err
	}
	if !json.Valid(value) {
		return errors.New("agentmodel: value is not valid JSON")
	}
	return nil
}
