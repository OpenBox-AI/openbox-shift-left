package client

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The family-field tuples and kinds are a MIRROR of the base SDK
// (openbox_core/contracts/otel_spans.py + conformance/fake_core.py). This guard
// fails loudly if the mirror drifts from the values recorded at mirror time.
func TestFamilyRootFieldsMirror(t *testing.T) {
	want := map[HookType][]string{
		HookFileOperation: {"file_path", "file_mode", "file_operation", "bytes_read", "bytes_written"},
		HookShell:         {"shell_command", "shell_exit_code"},
		HookMCP:           {"mcp_server", "mcp_tool", "mcp_method"},
		HookTool:          {"tool_name"},
	}
	if !reflect.DeepEqual(FamilyRootFields, want) {
		t.Fatalf("FamilyRootFields drifted from the base SDK mirror:\n got %v\nwant %v", FamilyRootFields, want)
	}
	for h, wantKind := range map[HookType]string{
		HookFileOperation: "INTERNAL",
		HookShell:         "INTERNAL",
		HookMCP:           "CLIENT",
		HookTool:          "INTERNAL",
	} {
		if got := KindFor(h); got != wantKind {
			t.Errorf("KindFor(%s)=%s, want %s", h, got, wantKind)
		}
	}
	if got := KindFor(HookType("unknown")); got != "INTERNAL" {
		t.Errorf("KindFor(unknown)=%s, want INTERNAL fallback", got)
	}
}

// conformantPayload builds a minimal, spec-conformant ActivityStarted+hook
// payload for a hook type and stage (family fields present-but-null).
func conformantPayload(ht HookType, stage string) map[string]any {
	span := map[string]any{
		"span_id":        "00f067aa0ba902b7",
		"trace_id":       "4bf92f3577b34da6a3ce929d0e0e4736",
		"parent_span_id": nil,
		"name":           "op",
		"kind":           KindFor(ht),
		"stage":          stage,
		"start_time":     int64(1),
		"end_time":       nil,
		"duration_ns":    nil,
		"attributes":     map[string]any{},
		"status":         map[string]any{"code": "UNSET", "description": nil},
		"events":         []any{},
		"hook_type":      string(ht),
		"error":          nil,
	}
	if stage == "completed" {
		span["end_time"] = int64(2)
		span["duration_ns"] = int64(1)
	}
	for _, f := range FamilyRootFields[ht] {
		span[f] = nil // present-but-null
	}
	return map[string]any{
		"event_type":   "ActivityStarted",
		"hook_trigger": true,
		"span_count":   1,
		"spans":        []any{span},
	}
}

func TestAssertHookWireShape_ConformantFamilies(t *testing.T) {
	for _, ht := range []HookType{HookFileOperation, HookShell, HookMCP, HookTool} {
		for _, stage := range []string{"started", "completed"} {
			if err := AssertHookWireShape(conformantPayload(ht, stage)); err != nil {
				t.Errorf("%s/%s: conformant payload rejected: %v", ht, stage, err)
			}
		}
	}
}

// A payload that survives a JSON round-trip (span_count/start_time become
// float64) must still conform — the realistic decode path the emitter produces.
func TestAssertHookWireShape_JSONRoundTrip(t *testing.T) {
	raw, err := json.Marshal(conformantPayload(HookShell, "started"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := AssertHookWireShape(decoded); err != nil {
		t.Fatalf("JSON-decoded conformant payload rejected: %v", err)
	}
}

func TestAssertHookWireShape_Rejects(t *testing.T) {
	span := func(p map[string]any) map[string]any { return p["spans"].([]any)[0].(map[string]any) }
	cases := []struct {
		name   string
		mutate func(p map[string]any)
	}{
		{"wrong event_type", func(p map[string]any) { p["event_type"] = "WorkflowStarted" }},
		{"hook_trigger false", func(p map[string]any) { p["hook_trigger"] = false }},
		{"empty spans", func(p map[string]any) { p["spans"] = []any{} }},
		{"span_count mismatch", func(p map[string]any) { p["span_count"] = 2 }},
		{"nested otel envelope", func(p map[string]any) { span(p)["otel"] = map[string]any{} }},
		{"data blob", func(p map[string]any) { span(p)["data"] = "x" }},
		{"sdk semantic_type", func(p map[string]any) { span(p)["semantic_type"] = "shell_command" }},
		{"missing common field", func(p map[string]any) { delete(span(p), "error") }},
		{"bad span_id (uppercase)", func(p map[string]any) { span(p)["span_id"] = "00F067AA0BA902B7" }},
		{"bad span_id (length)", func(p map[string]any) { span(p)["span_id"] = "abc" }},
		{"bad trace_id", func(p map[string]any) { span(p)["trace_id"] = "tooshort" }},
		{"missing family field", func(p map[string]any) { delete(span(p), "shell_command") }},
		{"missing hook_type", func(p map[string]any) { span(p)["hook_type"] = "" }},
		{"started with end_time", func(p map[string]any) { span(p)["end_time"] = int64(9) }},
		{"started with duration_ns", func(p map[string]any) { span(p)["duration_ns"] = int64(9) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := conformantPayload(HookShell, "started")
			c.mutate(p)
			if err := AssertHookWireShape(p); err == nil {
				t.Fatalf("%s: expected rejection, got nil", c.name)
			}
		})
	}
}
