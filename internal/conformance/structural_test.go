package conformance

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemaCompilesWithoutFetching the compiler must resolve the schema
// entirely from memory.
func TestSchemaCompilesWithoutFetching(t *testing.T) {
	doc, err := LoadSchema()
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	sch, err := compileSchema(doc)
	if err != nil {
		t.Fatalf("compiling with a refusing loader failed, so something tried to fetch: %v", err)
	}
	if sch == nil {
		t.Fatal("compiled schema is nil")
	}

	// So prove the loader is actually wired: a schema with an external $ref must
	// fail to compile.
	external := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$ref":    "https://example.invalid/some-remote-schema.json",
	}
	_, err = compileSchema(external)
	if err == nil {
		t.Error("a schema with an external $ref compiled — the refusing loader is not " +
			"installed, so a conformance run can be influenced from off-host")
	} else if !strings.Contains(err.Error(), "refused to fetch") {
		t.Errorf("external $ref failed, but not through the refusing loader: %v", err)
	}
}

// TestDateTimeFormatIsAsserted format assertions must stay ON.
func TestDateTimeFormatIsAsserted(t *testing.T) {
	doc, err := LoadSchema()
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	sch, err := compileSchema(doc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join("testdata", "valid", "session_started.json"))
	if err != nil {
		entries, derr := os.ReadDir(filepath.Join("testdata", "valid"))
		if derr != nil || len(entries) == 0 {
			t.Fatalf("no valid fixture to mutate: %v / %v", err, derr)
		}
		raw, err = os.ReadFile(filepath.Join("testdata", "valid", entries[0].Name()))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
	}

	var inst map[string]any
	if err := json.Unmarshal(raw, &inst); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if _, ok := inst["timestamp"]; !ok {
		t.Fatal("fixture carries no timestamp, so this test would prove nothing")
	}
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("unmutated fixture must be valid: %v", err)
	}

	inst["timestamp"] = "not-a-date-time"
	if err := sch.Validate(inst); err == nil {
		t.Error("a malformed date-time validated — format assertions are OFF. " +
			"compileSchema must call Compiler.AssertFormat().")
	}
}

// TestContentGateIsItsOwnPass the content gate and structural validation stay
// two passes, in that order. So the assertion is not "both are reported" but
// "the gate is reached on its own terms once the structure is sound, and never
// reports through a structural error".
func TestContentGateIsItsOwnPass(t *testing.T) {
	dir := filepath.Join("testdata", "content")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) == 0 {
		t.Fatal("no content fixtures — this test would assert nothing")
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}

		err = ValidateDevEvent(raw, false)
		if !errors.Is(err, ErrContentDisabled) {
			t.Errorf("%s: want ErrContentDisabled on its own, got %v", e.Name(), err)
			continue
		}
		if strings.Contains(err.Error(), "oneOf") || strings.Contains(err.Error(), "not conformant") {
			t.Errorf("%s: the content violation was reported through a structural error (%v) — "+
				"the two passes have been folded together", e.Name(), err)
		}

		// The structural error takes precedence, and crucially it must NOT be an
		// ErrContentDisabled wearing a structural message: the two findings stay
		// distinguishable.
		var inst map[string]any
		if err := json.Unmarshal(raw, &inst); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		inst["event_type"] = "NotAnEventType"
		broken, err := json.Marshal(inst)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		err = ValidateDevEvent(broken, false)
		if err == nil {
			t.Errorf("%s: structurally broken instance validated", e.Name())
			continue
		}
		if errors.Is(err, ErrContentDisabled) {
			t.Errorf("%s: a structural failure was reported as a content violation", e.Name())
		}
	}
}

// TestGatedFieldsAreReachableWithoutOneOf every x-content-gated node must be
// reachable through `properties` alone.
func TestGatedFieldsAreReachableWithoutOneOf(t *testing.T) {
	doc, err := LoadSchema()
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}

	var walk func(node map[string]any, path string, underOneOf bool)
	walk = func(node map[string]any, path string, underOneOf bool) {
		if gated, _ := node["x-content-gated"].(bool); gated && underOneOf {
			t.Errorf("%s is x-content-gated but sits under a oneOf branch. hasGatedContent "+
				"does not descend oneOf, so this field would NOT be gated and its content "+
				"would egress with content-capture off. Move it under `properties`, or teach "+
				"hasGatedContent to descend oneOf.", path)
		}
		for _, key := range []string{"properties", "$defs"} {
			if m, ok := node[key].(map[string]any); ok {
				for name, sub := range m {
					if s, ok := sub.(map[string]any); ok {
						walk(s, path+"."+name, underOneOf)
					}
				}
			}
		}
		if list, ok := node["oneOf"].([]any); ok {
			for _, sub := range list {
				if s, ok := sub.(map[string]any); ok {
					walk(s, path+".oneOf[]", true)
				}
			}
		}
	}
	walk(doc, "#", false)
}

// TestOneOfDiscriminatorSemantics the contract's next version adds oneOf
// discriminator branches, and the retired walk had never been stressed on
// them: its trial counted a branch valid whenever its own subset of keywords
// found no fault, and it discarded siblings of $ref.
func TestOneOfDiscriminatorSemantics(t *testing.T) {
	doc := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"oneOf": []any{
			map[string]any{"required": []any{"gateway_request_id"}},
			map[string]any{"required": []any{"otel_request_id"}},
			map[string]any{"required": []any{"proxy_request_id"}},
		},
		"properties": map[string]any{
			"gateway_request_id": map[string]any{"type": "string"},
			"otel_request_id":    map[string]any{"type": "string"},
			"proxy_request_id":   map[string]any{"type": "string"},
		},
	}
	sch, err := compileSchema(doc)
	if err != nil {
		t.Fatalf("compile throwaway schema: %v", err)
	}

	cases := []struct {
		name  string
		inst  map[string]any
		valid bool
	}{
		{"exactly one discriminator", map[string]any{"otel_request_id": "x"}, true},
		{"another single discriminator", map[string]any{"proxy_request_id": "x"}, true},
		{"two discriminators", map[string]any{"otel_request_id": "x", "proxy_request_id": "y"}, false},
		{"all three", map[string]any{"gateway_request_id": "a", "otel_request_id": "b", "proxy_request_id": "c"}, false},
		{"none", map[string]any{}, false},
	}
	for _, c := range cases {
		err := sch.Validate(c.inst)
		if c.valid && err != nil {
			t.Errorf("%s: want valid, got %v", c.name, err)
		}
		if !c.valid && err == nil {
			t.Errorf("%s: want rejected, got valid — oneOf is not enforcing exactly-one", c.name)
		}
	}
}
