package conformance

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// contractEventTypes is this package's declaration of the developer-runtime
// vocabulary, used for coverage bookkeeping in conformance_test.go. This module
// must not depend on `client` — the adapters import both, and the contract is
// meant to be the thing they are checked AGAINST rather than a mirror of one of
// them — which is why the list is declared here rather than read from
// client.EventType.
//
// TestSchemaEnumMatchesContract below binds it to the schema, and the acceptance
// module binds the schema to the client constants — so all three declarations
// are pinned to each other transitively without this module importing client.
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

// schemaKeywords is the keyword set this contract deliberately confines itself to.
//
// It used to be the list the hand-rolled validator implemented, and anything
// outside it was silently ignored at validation time. The library implements the
// whole draft, so that failure mode is gone — but the list is kept, narrowed to
// its remaining purpose: the contract stays small and every keyword in it has
// been reviewed for the semantics we actually want. `contentEncoding`, for one,
// needs Compiler.AssertContent to do anything at all.
var schemaKeywords = map[string]bool{
	"$ref": true, "const": true, "enum": true, "type": true,
	"minLength": true, "pattern": true, "minimum": true, "oneOf": true,
	// maxLength bounds all three producer ids — gateway_request_id was retrofitted
	// to the same bound in v1.6. Reviewed, per this list's purpose: it is a plain
	// assertion needing no Compiler setting, and it counts CODE POINTS where the
	// imperative precedent it mirrors (gatewayemit.printableASCII) counts bytes.
	// Those agree here only because the same properties also carry a
	// printable-ASCII `pattern`, which leaves no rune wider than one byte. A
	// future field taking maxLength without that pattern does NOT inherit the
	// equivalence.
	"maxLength": true,
	"required":  true, "properties": true, "additionalProperties": true,
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

// The contract confines itself to a reviewed subset of JSON Schema. The library
// would honour more, so this is a scope guard rather than a capability guard:
// a keyword arriving here should be a decision, because some need a compiler
// setting to take effect at all (see TestDateTimeFormatIsAsserted) and some
// interact in ways the contract has not considered.
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
				t.Errorf("%s: keyword %q is outside the contract's reviewed keyword set — "+
					"add it to schemaKeywords deliberately (checking whether it needs a "+
					"Compiler setting to take effect) or express the constraint differently", path, k)
			}
			// additionalProperties is kept to its boolean form. The library handles
			// the schema form correctly, but the contract has never used it and the
			// two forms read very differently to someone auditing the schema.
			if k == "additionalProperties" {
				if _, isBool := v.(bool); !isBool {
					t.Errorf("%s.additionalProperties: the contract uses only the boolean form, got %T", path, v)
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

// TestSchemaVersionMarkersAgree pins the three places the schema states its own
// version to each other.
//
// It exists because they disagreed: the file declared `x-schema-version: 1.5`
// and `schema_version.const: 1.5` while `$id` still named 1.4, and the 1.5
// changelog entry described seven fields that were never added to `properties`.
// With both objects `additionalProperties: false`, that made every event the
// version claimed to support fail its own contract — and nothing noticed,
// because no fixture carried one.
//
// The changelog check is the load-bearing half: a bump whose entry names a field
// the schema does not declare is exactly the state this file shipped in, and it
// is not catchable by validating the fixtures that already exist.
func TestSchemaVersionMarkersAgree(t *testing.T) {
	schema, err := LoadSchema()
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}

	version, _ := schema["x-schema-version"].(string)
	if version == "" {
		t.Fatal("schema declares no x-schema-version")
	}

	id, _ := schema["$id"].(string)
	if !strings.Contains(id, "/"+version+"/") {
		t.Errorf("$id %q does not name the declared version %q — a consumer resolving the id gets a different contract than the one it validated against", id, version)
	}

	props, _ := schema["properties"].(map[string]any)
	sv, _ := props["schema_version"].(map[string]any)
	if got, _ := sv["const"].(string); got != version {
		t.Errorf("schema_version const is %q but x-schema-version is %q", got, version)
	}

	changelog, _ := schema["x-changelog"].(map[string]any)
	if _, ok := changelog[version]; !ok {
		t.Errorf("no x-changelog entry for the declared version %q", version)
	}

	// Every backtick-quoted `span.<field>` / bare `<field>` the current entry
	// claims must actually be declared. Scoped to the current version's entry:
	// older entries describe fields that may since have been removed.
	entry, _ := changelog[version].(string)
	declared := declaredNames(schema)
	for _, claimed := range backquoted(entry) {
		name := strings.TrimPrefix(claimed, "span.")
		// Field names in this contract are lower_snake_case, so anything else in
		// backticks is prose: a glob (`http_*`), a Go symbol (`isLLMCall`), an id
		// template (`<session>:turn:<n>`), a nested path, or a value literal.
		// Checking those would make the assertion unmaintainable rather than
		// stricter.
		if !fieldNamePattern.MatchString(name) {
			continue
		}
		if !declared[name] {
			t.Errorf("the %s changelog claims field %q, which is not declared in properties — an event carrying it is rejected by additionalProperties:false", version, claimed)
		}
	}
}

// fieldNamePattern is the shape a property name has in this contract. Anything
// else inside backticks in the changelog is prose.
var fieldNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// declaredNames collects every property name the schema declares, at the top
// level and inside $defs, so a claim naming either is checkable.
func declaredNames(schema map[string]any) map[string]bool {
	out := map[string]bool{}
	var walk func(any)
	walk = func(n any) {
		m, ok := n.(map[string]any)
		if !ok {
			return
		}
		if props, ok := m["properties"].(map[string]any); ok {
			for name, sub := range props {
				out[name] = true
				walk(sub)
			}
		}
		for _, key := range []string{"$defs", "items"} {
			if sub, ok := m[key]; ok {
				if defs, ok := sub.(map[string]any); ok {
					for _, d := range defs {
						walk(d)
					}
				}
				walk(sub)
			}
		}
	}
	walk(schema)
	return out
}

// backquoted returns the `…` spans in s — how the changelog names fields.
func backquoted(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "`")
		if i < 0 {
			return out
		}
		s = s[i+1:]
		j := strings.Index(s, "`")
		if j < 0 {
			return out
		}
		out = append(out, s[:j])
		s = s[j+1:]
	}
}
