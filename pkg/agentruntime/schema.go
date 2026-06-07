package agentruntime

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// ValidateAgainstSchema validates a JSON value against a JSON Schema document
// (2020-12 et al., via santhosh-tekuri/jsonschema). This is the real runtime
// check the loop-mode executor uses for tool-call arguments and tool results —
// the heavy dependency lives here, not in the pure pkg/agentmodel/v1 package
// (whose v1.MatchesSchema stays the lightweight admission-time shape check).
//
// An empty schema is permissive (any well-formed JSON passes). A schema that
// fails to compile is returned as an error rather than silently accepted, so a
// malformed tool schema surfaces instead of disabling validation.
func ValidateAgainstSchema(schema, value json.RawMessage) error {
	if !json.Valid(value) {
		return fmt.Errorf("value is not valid JSON")
	}
	if len(bytes.TrimSpace(schema)) == 0 {
		return nil // no schema declared → accept any well-formed JSON
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("tool.json", bytes.NewReader(schema)); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	sch, err := c.Compile("tool.json")
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	var v any
	if err := json.Unmarshal(value, &v); err != nil {
		return fmt.Errorf("value is not valid JSON: %w", err)
	}
	if err := sch.Validate(v); err != nil {
		return fmt.Errorf("schema validation: %w", err)
	}
	return nil
}
