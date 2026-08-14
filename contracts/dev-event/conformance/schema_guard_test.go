package conformance

import (
	"fmt"
	"sort"
	"testing"
)

// contractEventTypes is this package's declaration of the developer-runtime
// vocabulary, used for coverage bookkeeping in conformance_test.go. The module
// is deliberately dependency-free so adapters can import it without pulling in
// the client, which is why the list exists here at all rather than being read
// from client.EventType.
//
// TestSchemaEnumMatchesContract below binds it to the schema, and the acceptance
// module binds the schema to the client constants — so all three declarations
// are pinned to each other transitively without this module gaining a dependency.
var contractEventTypes = []string{
	"SessionStarted", "PromptSubmitted", "ToolCall", "ToolResult",
	"SessionEnded", "CommitCreated", "Deploy",
	"TurnStarted", "TurnCompleted",
	"SubagentStarted", "PermissionDenied", "APIError",
}

// The previous version of this test asserted len(enum) == 7. That passes
// unchanged when a type is renamed on one side of the contract, which is the
// failure it most needed to catch.
func TestSchemaEnumMatchesContract(t *testing.T) {
	schema, err := LoadSchema()
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	et, _ := props["event_type"].(map[string]any)
	enum, _ := et["enum"].([]any)

	got := make([]string, 0, len(enum))
	for _, e := range enum {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("schema enum holds a non-string value %v", e)
		}
		got = append(got, s)
	}
	assertSameSet(t, "schema event_type enum", got, "conformance contractEventTypes", contractEventTypes)
}

// schemaKeywords the hand-rolled validator actually implements. Anything else in
// the schema is silently ignored at validation time, so a constraint written with
// an unimplemented keyword would look enforced while enforcing nothing.
var schemaKeywords = map[string]bool{
	"$ref": true, "const": true, "enum": true, "type": true,
	"minLength": true, "pattern": true, "minimum": true, "oneOf": true,
	"required": true, "properties": true, "additionalProperties": true,
	"$defs": true, "x-content-gated": true, "format": true,
}

// annotationKeywords carry no constraint, so ignoring them is correct. The x-*
// entries are this contract's own documentation extensions; x-content-gated is
// deliberately absent because it does drive behaviour and is implemented.
var annotationKeywords = map[string]bool{
	"$schema": true, "$id": true, "title": true, "description": true,
	"examples": true, "default": true, "deprecated": true, "comment": true, "$comment": true,
	"x-schema-version": true, "x-legacy-action": true, "x-wire-mapping": true,
	// x-changelog records what each contract version changed and why. Prose, so
	// it constrains nothing — but it is where a reader learns that v1.1
	// re-defined tokens.input, which no keyword can express.
	"x-changelog": true,
	// x-local-only marks a field that exists on the normalized event and the
	// spool but is never a field on the core wire payload (invocation_id,
	// operation_id). Like the others here it is documentation: what actually
	// keeps those fields off the wire is buildPayload plus the golden wire
	// fixtures, not a schema keyword.
	"x-local-only": true,
}

// The validator covers a deliberate subset of JSON Schema. That is a fine
// trade-off for a dependency-free contract check, but only while the schema
// stays inside the subset: `"format": "date-time"` or `"maxLength": 64` added to
// the schema would read as a tightened contract and change nothing at all.
func TestSchemaUsesOnlySupportedKeywords(t *testing.T) {
	schema, err := LoadSchema()
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	var walk func(node map[string]any, path string)
	walk = func(node map[string]any, path string) {
		for k, v := range node {
			switch {
			case schemaKeywords[k], annotationKeywords[k]:
			default:
				t.Errorf("%s: keyword %q is not implemented by this validator — "+
					"either implement it in validator.go or express the constraint differently; "+
					"as written it validates nothing", path, k)
			}
			// additionalProperties is honoured only in its boolean form; as a
			// schema object it is silently skipped.
			if k == "additionalProperties" {
				if _, isBool := v.(bool); !isBool {
					t.Errorf("%s.additionalProperties: only the boolean form is implemented, got %T", path, v)
				}
			}
		}
		// Recurse only into positions that hold subschemas. properties/$defs hold
		// name→schema maps; enum/required/const hold data and must not be walked.
		for _, key := range []string{"properties", "$defs"} {
			if m, ok := node[key].(map[string]any); ok {
				for name, sub := range m {
					if s, ok := sub.(map[string]any); ok {
						walk(s, path+"."+key+"."+name)
					}
				}
			}
		}
		if list, ok := node["oneOf"].([]any); ok {
			for i, sub := range list {
				if s, ok := sub.(map[string]any); ok {
					walk(s, fmt.Sprintf("%s.oneOf[%d]", path, i))
				}
			}
		}
	}
	walk(schema, "#")
}

// assertSameSet compares two vocabularies by set, reporting each side's extras
// so a rename shows up as one added and one removed name rather than "differs".
func assertSameSet(t *testing.T, aName string, a []string, bName string, b []string) {
	t.Helper()
	inB := make(map[string]bool, len(b))
	for _, s := range b {
		inB[s] = true
	}
	inA := make(map[string]bool, len(a))
	for _, s := range a {
		inA[s] = true
	}
	var onlyA, onlyB []string
	for _, s := range a {
		if !inB[s] {
			onlyA = append(onlyA, s)
		}
	}
	for _, s := range b {
		if !inA[s] {
			onlyB = append(onlyB, s)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	if len(onlyA) > 0 {
		t.Errorf("in %s but not %s: %v", aName, bName, onlyA)
	}
	if len(onlyB) > 0 {
		t.Errorf("in %s but not %s: %v", bName, aName, onlyB)
	}
}
