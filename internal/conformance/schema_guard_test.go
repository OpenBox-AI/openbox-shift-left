package conformance

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// contractEventTypes is this package's declaration of the developer-runtime
// vocabulary, used for coverage bookkeeping in conformance_test.go.
var contractEventTypes = []string{
	"SessionStarted", "PromptSubmitted", "ToolCall", "ToolResult",
	"SessionEnded", "CommitCreated", "Deploy",
	"TurnStarted", "TurnCompleted",
	"SubagentStarted", "PermissionDenied", "APIError",
}

// TestSchemaEnumMatchesContract the previous version of this test asserted
// len(enum) == 7.
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

var schemaKeywords = map[string]bool{
	"$ref": true, "const": true, "enum": true, "type": true,
	"minLength": true, "pattern": true, "minimum": true, "oneOf": true,
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
	"x-changelog":  true,
	"x-local-only": true,
}

// TestSchemaUsesOnlySupportedKeywords the contract confines itself to a
// reviewed subset of JSON Schema.
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
			// The library handles the schema form correctly, but the contract has never
			// used it and the two forms read very differently to someone auditing the
			// schema.
			if k == "additionalProperties" {
				if _, isBool := v.(bool); !isBool {
					t.Errorf("%s.additionalProperties: the contract uses only the boolean form, got %T", path, v)
				}
			}
		}
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

// TestSchemaVersionMarkersAgree pins the three places the schema states its
// own version to each other.
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

	entry, _ := changelog[version].(string)
	declared := declaredNames(schema)
	for _, claimed := range backquoted(entry) {
		name := strings.TrimPrefix(claimed, "span.")
		if !fieldNamePattern.MatchString(name) {
			continue
		}
		if !declared[name] {
			t.Errorf("the %s changelog claims field %q, which is not declared in properties — an event carrying it is rejected by additionalProperties:false", version, claimed)
		}
	}
}

var fieldNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

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
