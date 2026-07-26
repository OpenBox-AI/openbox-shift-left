package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// newWireCapture builds a real AIP-signed client pointed at a loopback core
// that records every /evaluate body, so tests can assert the EXACT bytes the
// Codex adapter's events put on the wire (story AC-5: E7 flat-hook parity).
func newWireCapture(t *testing.T) (*client.Client, *[][]byte) {
	t.Helper()
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"verdict":"allow"}`))
	}))
	t.Cleanup(srv.Close)

	seed := base64.StdEncoding.EncodeToString(make([]byte, 32))
	cl, err := client.New(client.Config{
		BaseURL: srv.URL, // loopback http is allowed by the INV-1 TLS guard
		APIKey:  "obx_test_key",
		DID:     testDID,
		SeedB64: seed,
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return cl, &bodies
}

func emit(t *testing.T, cl *client.Client, ev client.DevEvent) {
	t.Helper()
	if _, err := cl.Emit(context.Background(), ev); err != nil {
		t.Fatalf("emit: %v", err)
	}
}

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode wire body: %v\n%s", err, raw)
	}
	return m
}

// TestWire_ToolEventsPassHookShapeAndPairByToolUseID proves the E7 parity core
// of AC-5 end-to-end through the REAL client: ToolCall AND ToolResult are both
// event_type=ActivityStarted hook payloads that pass client.AssertHookWireShape,
// and the started+completed pair derived from ONE tool_use_id share span_id +
// activity_id, while a second invocation of the SAME tool gets DIFFERENT ids —
// the exact-pairing improvement Codex's tool_use_id buys over the CC adapter.
func TestWire_ToolEventsPassHookShapeAndPairByToolUseID(t *testing.T) {
	cl, bodies := newWireCapture(t)
	m := testMapper()
	m.NewID = nil

	pre1, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-1"})
	post1, _ := m.Map(HookPostToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-1"})
	pre2, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-2"})
	for _, ev := range []client.DevEvent{pre1, post1, pre2} {
		emit(t, cl, ev)
	}
	if len(*bodies) != 3 {
		t.Fatalf("expected 3 wire bodies, got %d", len(*bodies))
	}

	type ids struct{ span, trace, activity, stage string }
	extract := func(raw []byte) ids {
		payload := decodeBody(t, raw)
		if err := client.AssertHookWireShape(payload); err != nil {
			t.Fatalf("hook wire shape (E7 parity): %v\n%s", err, raw)
		}
		span := payload["spans"].([]any)[0].(map[string]any)
		act, _ := payload["activity_id"].(string)
		return ids{
			span:     span["span_id"].(string),
			trace:    span["trace_id"].(string),
			activity: act,
			stage:    span["stage"].(string),
		}
	}

	i1, i2, i3 := extract((*bodies)[0]), extract((*bodies)[1]), extract((*bodies)[2])
	if i1.stage != "started" || i2.stage != "completed" {
		t.Errorf("stages = %q/%q, want started/completed", i1.stage, i2.stage)
	}
	// Same tool_use_id ⇒ shared span/activity ids (base-SDK shared-span pairing).
	if i1.span != i2.span || i1.activity != i2.activity {
		t.Errorf("pre/post of one tool_use_id must share ids: span %s/%s activity %s/%s",
			i1.span, i2.span, i1.activity, i2.activity)
	}
	// Different tool_use_id, same tool ⇒ DISTINCT ids (exact per-invocation pairing).
	if i1.span == i3.span || i1.activity == i3.activity {
		t.Errorf("two Bash invocations with distinct tool_use_ids must not share ids")
	}
	// One session ⇒ one trace.
	if i1.trace != i2.trace || i1.trace != i3.trace {
		t.Errorf("session events must share the trace: %s/%s/%s", i1.trace, i2.trace, i3.trace)
	}
}

// TestWire_ToolUseIDNeverRidesTheWire pins the pairing-channel invariant the
// mapper's mapTool doc comment claims: the tool_use_id shapes the DERIVED ids
// but never appears as a wire FIELD on a shell span (span.function is not a
// shell/tool family field, and activity_input carries file/mcp locators only).
// The metadata blob is the one deliberate carrier (structural identifier).
func TestWire_ToolUseIDNeverRidesTheWire(t *testing.T) {
	cl, bodies := newWireCapture(t)
	m := testMapper()
	m.NewID = nil

	pre, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-sentinel-xyz"})
	emit(t, cl, pre)
	payload := decodeBody(t, (*bodies)[0])

	span := payload["spans"].([]any)[0].(map[string]any)
	for _, k := range []string{"function", "file_path", "shell_command"} {
		if v, present := span[k]; present && v != nil {
			if s, _ := v.(string); strings.Contains(s, "call-sentinel-xyz") {
				t.Errorf("tool_use_id leaked onto wire span field %q: %v", k, v)
			}
		}
	}
	if in, present := payload["activity_input"]; present {
		if raw, _ := json.Marshal(in); strings.Contains(string(raw), "call-sentinel-xyz") {
			t.Errorf("tool_use_id leaked into activity_input: %s", raw)
		}
	}
	meta, _ := payload["metadata"].(map[string]any)
	if meta == nil || meta["tool_use_id"] != "call-sentinel-xyz" {
		t.Errorf("metadata should carry tool_use_id (the deliberate audit channel): %v", meta)
	}
}

// TestWire_MCPKeepsFunctionAsWireData: for MCP tools span.function IS wire data
// (mcp_tool), so the mapper must NOT overwrite it with tool_use_id.
func TestWire_MCPKeepsFunctionAsWireData(t *testing.T) {
	cl, bodies := newWireCapture(t)
	m := testMapper()
	m.NewID = nil

	pre, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "mcp__github__create_issue", ToolUseID: "call-9"})
	emit(t, cl, pre)
	payload := decodeBody(t, (*bodies)[0])
	if err := client.AssertHookWireShape(payload); err != nil {
		t.Fatalf("hook wire shape: %v", err)
	}
	span := payload["spans"].([]any)[0].(map[string]any)
	if span["hook_type"] != "mcp" || span["mcp_server"] != "github" || span["mcp_tool"] != "create_issue" {
		t.Errorf("mcp family fields wrong: hook_type=%v mcp_server=%v mcp_tool=%v",
			span["hook_type"], span["mcp_server"], span["mcp_tool"])
	}
}

// TestWire_LifecycleEventsUseBaseWireTypes (AC-5, E7-S5 rule): lifecycle events
// ride the base Workflow*/SignalReceived wire types with workflow_type set on
// signals too, and are span-less.
func TestWire_LifecycleEventsUseBaseWireTypes(t *testing.T) {
	cl, bodies := newWireCapture(t)
	m := testMapper()
	m.NewID = nil

	ss, _ := m.Map(HookSessionStart, &HookEvent{SessionID: "th-1", Cwd: "/repo", Source: "startup", Model: "gpt-5.3-codex", PermissionMode: "default"})
	ps, _ := m.Map(HookUserPromptSubmit, &HookEvent{SessionID: "th-1", PermissionMode: "default"})
	se, _ := m.Map(HookSessionEnd, &HookEvent{SessionID: "th-1", Reason: "other"})
	for _, ev := range []client.DevEvent{ss, ps, se} {
		emit(t, cl, ev)
	}

	want := []struct{ wireType, signalName string }{
		{"WorkflowStarted", ""},
		{"SignalReceived", "prompt_submitted"},
		{"WorkflowCompleted", ""},
	}
	for i, w := range want {
		payload := decodeBody(t, (*bodies)[i])
		if payload["event_type"] != w.wireType {
			t.Errorf("body[%d] event_type = %v, want %s", i, payload["event_type"], w.wireType)
		}
		if payload["workflow_type"] != "developer-session" {
			t.Errorf("body[%d] workflow_type = %v, want developer-session (required on signals too)", i, payload["workflow_type"])
		}
		if w.signalName != "" && payload["signal_name"] != w.signalName {
			t.Errorf("body[%d] signal_name = %v, want %s", i, payload["signal_name"], w.signalName)
		}
		if payload["run_id"] != "th-1" {
			t.Errorf("body[%d] run_id = %v, want the codex thread id", i, payload["run_id"])
		}
		if spans, present := payload["spans"]; present && spans != nil {
			t.Errorf("body[%d] lifecycle events must be span-less, got %v", i, spans)
		}
	}
}

// TestWire_NoContentLeakEndToEnd (SL3-SEC-3, AC-7/AC-10): sentinel content in
// tool_input and the prompt (with content-capture OFF) never reaches the wire
// bytes.
func TestWire_NoContentLeakEndToEnd(t *testing.T) {
	cl, bodies := newWireCapture(t)
	m := testMapper()
	m.NewID = nil
	m.CaptureContent = false

	cmdSecret := "WIRE-SECRET-COMMAND"
	promptSecret := "WIRE-SECRET-PROMPT"
	pre, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "c1",
		ToolInput: json.RawMessage(`{"command":"` + cmdSecret + `"}`)})
	ps, _ := m.Map(HookUserPromptSubmit, &HookEvent{SessionID: "th-1", Prompt: promptSecret})
	emit(t, cl, pre)
	emit(t, cl, ps)

	for i, raw := range *bodies {
		for _, secret := range []string{cmdSecret, promptSecret} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("content leaked to wire body[%d]: %s", i, raw)
			}
		}
	}
}
