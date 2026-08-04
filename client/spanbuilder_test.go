package client

import (
	"encoding/json"
	"sync"
	"testing"
)

// roundTrip marshals a built payload and re-decodes it as Core would receive it
// (int→float64, []map→[]any) — the realistic path AssertHookWireShape runs on.
func roundTrip(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return decoded
}

// The headline conformance test: a span built by BuildHookSpan for every hook
// family and stage, wrapped by BuildHookEvent, satisfies the base SDK wire
// contract (AssertHookWireShape / fake_core.assert_hook_wire_shape) — both
// in-memory and after a JSON round-trip. This is the gate E7-S3 targets.
func TestBuildHookSpan_ConformsToWireShape(t *testing.T) {
	tc := NewTraceContext()
	for _, ht := range []HookType{HookFileOperation, HookShell, HookMCP, HookTool} {
		for _, stage := range []string{"started", "completed"} {
			in := HookSpan{HookType: ht, Stage: stage, StartTime: 1000, EndTime: 5000}
			span := BuildHookSpan(tc, in)
			ev := BuildHookEvent("act-1", "Bash", span)

			// In-memory: BuildHookEvent emits spans as []any so the assertion runs
			// without a round-trip (client/hookspan_test.go L3).
			if err := AssertHookWireShape(ev); err != nil {
				t.Errorf("%s/%s in-memory: %v", ht, stage, err)
			}
			// And after the real JSON decode path.
			if err := AssertHookWireShape(roundTrip(t, ev)); err != nil {
				t.Errorf("%s/%s round-trip: %v", ht, stage, err)
			}
		}
	}
}

func TestBuildHookSpan_StartedStageEmitsNulls(t *testing.T) {
	tc := NewTraceContext()
	span := BuildHookSpan(tc, HookSpan{HookType: HookShell, Stage: "started", StartTime: 42, EndTime: 99})
	// Both keys must be PRESENT (common root fields) and NULL (started invariant).
	for _, k := range []string{"end_time", "duration_ns"} {
		v, present := span[k]
		if !present {
			t.Errorf("started span missing common root field %q", k)
		}
		if v != nil {
			t.Errorf("started span %q = %v, want nil (EndTime must be ignored)", k, v)
		}
	}
	if span["start_time"] != int64(42) {
		t.Errorf("start_time = %v, want 42", span["start_time"])
	}
}

func TestBuildHookSpan_CompletedStageDuration(t *testing.T) {
	tc := NewTraceContext()
	span := BuildHookSpan(tc, HookSpan{HookType: HookShell, Stage: "completed", StartTime: 1000, EndTime: 4200})
	if span["end_time"] != int64(4200) {
		t.Errorf("end_time = %v, want 4200", span["end_time"])
	}
	if span["duration_ns"] != int64(3200) {
		t.Errorf("duration_ns = %v, want 3200 (end-start)", span["duration_ns"])
	}
}

// A completed span with an out-of-order/zero EndTime never emits a negative
// duration — it clamps end up to start (duration 0), staying shape-valid.
func TestBuildHookSpan_CompletedStageClampsNegativeDuration(t *testing.T) {
	tc := NewTraceContext()
	span := BuildHookSpan(tc, HookSpan{HookType: HookShell, Stage: "completed", StartTime: 5000, EndTime: 0})
	if span["duration_ns"] != int64(0) {
		t.Errorf("duration_ns = %v, want 0 (clamped)", span["duration_ns"])
	}
	if span["end_time"] != int64(5000) {
		t.Errorf("end_time = %v, want 5000 (clamped to start)", span["end_time"])
	}
}

func TestBuildHookSpan_IDFormatsAndTraceStability(t *testing.T) {
	tc := NewTraceContext()
	a := BuildHookSpan(tc, HookSpan{HookType: HookTool, Stage: "started", StartTime: 1})
	b := BuildHookSpan(tc, HookSpan{HookType: HookTool, Stage: "started", StartTime: 2})

	if id, _ := a["span_id"].(string); !spanIDRe.MatchString(id) {
		t.Errorf("span_id %q not 16-hex", a["span_id"])
	}
	if id, _ := a["trace_id"].(string); !traceIDRe.MatchString(id) {
		t.Errorf("trace_id %q not 32-hex", a["trace_id"])
	}
	// trace_id is stable across every span of the session; span_ids differ.
	if a["trace_id"] != tc.TraceID() || b["trace_id"] != tc.TraceID() {
		t.Errorf("trace_id not stable: %v / %v vs %v", a["trace_id"], b["trace_id"], tc.TraceID())
	}
	if a["span_id"] == b["span_id"] {
		t.Errorf("distinct spans must get distinct span_ids, both %v", a["span_id"])
	}
	// A different session gets a different trace_id.
	if NewTraceContext().TraceID() == tc.TraceID() {
		t.Error("two sessions must not share a trace_id")
	}
	// Root span → parent_span_id present-but-null.
	if v, present := a["parent_span_id"]; !present || v != nil {
		t.Errorf("root span parent_span_id = %v (present=%v), want present nil", v, present)
	}
}

// Base-SDK shared-span pairing: reusing a started span's id as the completed
// span's SpanID yields the same span_id/trace_id, differing only in stage —
// exactly how openbox_core pairs a ToolResult to its ToolCall.
func TestBuildHookSpan_SharedSpanPairing(t *testing.T) {
	tc := NewTraceContext()
	started := BuildHookSpan(tc, HookSpan{HookType: HookShell, Stage: "started", StartTime: 1000})
	sid := started["span_id"].(string)
	completed := BuildHookSpan(tc, HookSpan{
		HookType: HookShell, Stage: "completed", SpanID: sid, StartTime: 1000, EndTime: 2000,
	})
	if completed["span_id"] != sid {
		t.Errorf("completed span_id = %v, want reused %v", completed["span_id"], sid)
	}
	if completed["trace_id"] != started["trace_id"] {
		t.Error("paired spans must share the trace")
	}
	if completed["stage"] != "completed" || started["stage"] != "started" {
		t.Error("only the stage should differ between the paired spans")
	}
}

// Explicit parent nesting (the distinct case): a child span's parent_span_id is
// the supplied 16-hex parent id, and the payload still conforms.
func TestBuildHookSpan_ParentLinkage(t *testing.T) {
	tc := NewTraceContext()
	parentID := tc.NewSpanID()
	child := BuildHookSpan(tc, HookSpan{
		HookType: HookMCP, Stage: "started", ParentSpanID: parentID, StartTime: 1,
	})
	if child["parent_span_id"] != parentID {
		t.Errorf("parent_span_id = %v, want %v", child["parent_span_id"], parentID)
	}
	if err := AssertHookWireShape(BuildHookEvent("a", "t", child)); err != nil {
		t.Errorf("parented span rejected: %v", err)
	}
}

func TestBuildHookSpan_FamilyFieldsPresentNullOrCarried(t *testing.T) {
	tc := NewTraceContext()
	// No Fields supplied → every family field of the type is present-but-null.
	span := BuildHookSpan(tc, HookSpan{HookType: HookFileOperation, Stage: "started", StartTime: 1})
	for _, f := range FamilyRootFields[HookFileOperation] {
		v, present := span[f]
		if !present {
			t.Errorf("family field %q missing", f)
		}
		if v != nil {
			t.Errorf("unset family field %q = %v, want nil", f, v)
		}
	}
	// Supplied Fields are carried; the rest stay present-but-null.
	fp := "/a.go"
	span2 := BuildHookSpan(tc, HookSpan{
		HookType:  HookFileOperation,
		Stage:     "started",
		StartTime: 1,
		Fields:    map[string]any{"file_path": fp, "file_operation": "write"},
	})
	if span2["file_path"] != fp || span2["file_operation"] != "write" {
		t.Errorf("supplied family fields not carried: %v / %v", span2["file_path"], span2["file_operation"])
	}
	if span2["bytes_read"] != nil {
		t.Errorf("unsupplied family field bytes_read = %v, want nil", span2["bytes_read"])
	}
}

func TestBuildHookSpan_Defaults(t *testing.T) {
	tc := NewTraceContext()
	// Name defaults to the hook_type; kind to KindFor; attributes non-nil; status
	// to the UNSET dict; events to [].
	span := BuildHookSpan(tc, HookSpan{HookType: HookMCP, Stage: "started", StartTime: 1})
	if span["name"] != "mcp" {
		t.Errorf("name default = %v, want mcp", span["name"])
	}
	if span["kind"] != "CLIENT" {
		t.Errorf("kind default = %v, want CLIENT (KindFor mcp)", span["kind"])
	}
	if span["attributes"] == nil {
		t.Error("attributes must default to a non-nil map (common root field present)")
	}
	st, ok := span["status"].(map[string]any)
	if !ok || st["code"] != "UNSET" {
		t.Errorf("status default = %v, want {code:UNSET,...}", span["status"])
	}
	if _, ok := span["events"].([]any); !ok {
		t.Errorf("events default = %v, want []any", span["events"])
	}
	// Explicit overrides win.
	span2 := BuildHookSpan(tc, HookSpan{
		HookType: HookMCP, Stage: "started", Name: "mcp__x__y", Kind: "SERVER",
		Attributes: map[string]any{"mcp.method": "callTool"}, StartTime: 1,
	})
	if span2["name"] != "mcp__x__y" || span2["kind"] != "SERVER" {
		t.Errorf("overrides ignored: name=%v kind=%v", span2["name"], span2["kind"])
	}
	if a := span2["attributes"].(map[string]any); a["mcp.method"] != "callTool" {
		t.Errorf(`attributes["mcp.method"] = %v, want callTool`, a["mcp.method"])
	}
}

// INV-2: the builder places content ONLY at the span root (a gated body field),
// never in attributes and never anywhere the builder synthesizes. And it never
// emits the keys Core forbids on the wire (semantic_type/otel/openbox/data).
func TestBuildHookSpan_INV2_ContentPlacementAndForbiddenKeys(t *testing.T) {
	tc := NewTraceContext()
	const secret = "rm -rf / # sensitive command"
	span := BuildHookSpan(tc, HookSpan{
		HookType:  HookShell,
		Stage:     "started",
		StartTime: 1,
		// A post-gate emitter puts the (authorized) command in the family body
		// field; the builder must leave it exactly there.
		Fields: map[string]any{"shell_command": secret},
	})
	if span["shell_command"] != secret {
		t.Errorf("shell_command not carried at root: %v", span["shell_command"])
	}
	// Content must not have leaked into attributes.
	if a, _ := span["attributes"].(map[string]any); a["shell_command"] != nil {
		t.Error("INV-2: content leaked into attributes")
	}
	// Forbidden wire keys must be absent.
	for _, k := range []string{"semantic_type", "otel", "openbox", "data"} {
		if _, present := span[k]; present {
			t.Errorf("forbidden key %q present on the span", k)
		}
	}
}

func TestTraceContextFrom_ReusesValidRejectsInvalid(t *testing.T) {
	valid := "4bf92f3577b34da6a3ce929d0e0e4736"
	if got := TraceContextFrom(valid).TraceID(); got != valid {
		t.Errorf("valid trace_id not reused: %v", got)
	}
	// An invalid persisted id must not propagate; a fresh valid one is minted.
	for _, bad := range []string{"", "tooshort", "XYZ", "4BF92F3577B34DA6A3CE929D0E0E4736"} {
		got := TraceContextFrom(bad).TraceID()
		if !traceIDRe.MatchString(got) {
			t.Errorf("TraceContextFrom(%q) → %q, not 32-hex", bad, got)
		}
		if got == bad {
			t.Errorf("invalid id %q was propagated", bad)
		}
	}
}

func TestNewHexID_Widths(t *testing.T) {
	if id := newHexID(spanIDBytes); !spanIDRe.MatchString(id) {
		t.Errorf("newHexID(8) = %q, not 16-hex", id)
	}
	if id := newHexID(traceIDBytes); !traceIDRe.MatchString(id) {
		t.Errorf("newHexID(16) = %q, not 32-hex", id)
	}
}

// TraceContext is documented as safe to reuse across a session's events; drive
// concurrent span builds under -race to back that claim. Every span must share
// the session trace, carry a distinct valid span_id, and conform.
func TestTraceContext_ConcurrentReuse(t *testing.T) {
	tc := NewTraceContext()
	const n = 64
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			span := BuildHookSpan(tc, HookSpan{HookType: HookTool, Stage: "started", StartTime: 1})
			if span["trace_id"] != tc.TraceID() {
				t.Errorf("trace_id drifted under concurrency: %v", span["trace_id"])
			}
			if err := AssertHookWireShape(BuildHookEvent("a", "t", span)); err != nil {
				t.Errorf("concurrent span non-conformant: %v", err)
			}
			ids <- span["span_id"].(string)
		}()
	}
	wg.Wait()
	close(ids)
	seen := make(map[string]bool, n)
	for id := range ids {
		if !spanIDRe.MatchString(id) {
			t.Errorf("bad span_id under concurrency: %q", id)
		}
		if seen[id] {
			t.Errorf("duplicate span_id minted concurrently: %q", id)
		}
		seen[id] = true
	}
}

func TestBuildHookEvent_EnvelopeShape(t *testing.T) {
	tc := NewTraceContext()
	s1 := BuildHookSpan(tc, HookSpan{HookType: HookTool, Stage: "started", StartTime: 1})
	s2 := BuildHookSpan(tc, HookSpan{HookType: HookShell, Stage: "started", StartTime: 1})
	ev := BuildHookEvent("act-9", "Bash", s1, s2)
	if ev["event_type"] != "ActivityStarted" || ev["hook_trigger"] != true {
		t.Errorf("envelope type/trigger = %v/%v", ev["event_type"], ev["hook_trigger"])
	}
	if ev["span_count"] != 2 {
		t.Errorf("span_count = %v, want 2", ev["span_count"])
	}
	if ev["activity_id"] != "act-9" || ev["activity_type"] != "Bash" {
		t.Errorf("activity fields = %v/%v", ev["activity_id"], ev["activity_type"])
	}
	if _, ok := ev["spans"].([]any); !ok {
		t.Errorf("spans must be []any for the in-memory assertion, got %T", ev["spans"])
	}
	// Multi-span envelope conforms.
	if err := AssertHookWireShape(ev); err != nil {
		t.Errorf("multi-span envelope rejected: %v", err)
	}
	// Empty activity id/type are simply omitted (event→wire mapping fills them, E7-S4).
	bare := BuildHookEvent("", "", s1)
	if _, present := bare["activity_id"]; present {
		t.Error("empty activity_id should be omitted")
	}
}
