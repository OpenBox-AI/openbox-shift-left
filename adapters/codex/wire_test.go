package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/client/memhttptest"
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
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"verdict":"allow"}`))
	}))
	t.Cleanup(srv.Close)

	seed := base64.StdEncoding.EncodeToString(make([]byte, 32))
	cl, err := client.New(client.Config{
		BaseURL:       srv.URL, // loopback http is allowed by the INV-1 TLS guard
		APIKey:        "obx_test_key",
		DID:           testDID,
		PrivateKeyB64: seed,
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

// assertActivityWireShape is the envelope contract every tool event must satisfy
// on the wire. It replaces client.AssertHookWireShape, which checked the flat
// hook-span shape this repo no longer emits.
//
// Its twin lives in adapters/claude-code/conformance_parity_test.go and asserts
// the identical contract. The two are deliberate copies rather than a shared
// helper: the adapters are separate Go modules, and the property under test is
// that both produce the SAME shape independently — a shared helper they both
// called could drift with them and still pass.
func assertActivityWireShape(t *testing.T, payload map[string]any, wantType string) {
	t.Helper()
	if payload["event_type"] != wantType {
		t.Errorf("event_type = %v, want %s", payload["event_type"], wantType)
	}
	// The retired hook envelope. A key here means the span layer grew a caller.
	for _, k := range []string{"spans", "span_count", "hook_trigger"} {
		if v, present := payload[k]; present {
			t.Errorf("payload carries retired key %q = %v", k, v)
		}
	}
	for _, k := range []string{"source", "event_type", "workflow_id", "run_id", "workflow_type", "activity_id", "activity_type", "timestamp"} {
		if v, _ := payload[k].(string); v == "" {
			t.Errorf("missing required envelope field %q", k)
		}
	}
	if payload["workflow_type"] != "developer-session" {
		t.Errorf("workflow_type = %v, want developer-session", payload["workflow_type"])
	}
	// semantic_type was computed by core from the span. With no span there is no
	// classification, and the client must not invent one — an unowned field would
	// be a claim nothing verifies.
	if _, present := payload["semantic_type"]; present {
		t.Error("client must not set semantic_type")
	}
}

// TestWire_ToolEventsAreActivityPairs proves the activity lifecycle end-to-end
// through the REAL client: a ToolCall is an ActivityStarted and its ToolResult
// an ActivityCompleted, the pair shares one activity_id, and a second invocation
// of the same tool gets its own — the exact-pairing property Codex's tool_use_id
// buys, now carried by activity_id alone since there is no span_id.
func TestWire_ToolEventsAreActivityPairs(t *testing.T) {
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

	activityID := func(raw []byte, wantType string) string {
		payload := decodeBody(t, raw)
		assertActivityWireShape(t, payload, wantType)
		id, _ := payload["activity_id"].(string)
		return id
	}

	started := activityID((*bodies)[0], "ActivityStarted")
	completed := activityID((*bodies)[1], "ActivityCompleted")
	second := activityID((*bodies)[2], "ActivityStarted")

	// Both halves of one call address one row — and one approval.
	if started != completed {
		t.Errorf("pre/post of one tool_use_id must share activity_id: %s vs %s", started, completed)
	}
	// A distinct invocation is a distinct operation here (a bare Bash call has no
	// structural discriminator, so the operation id falls back to the tool_use_id).
	if started == second {
		t.Error("two Bash invocations with distinct tool_use_ids must not share activity_id")
	}
}

// TestWire_ToolUseIDNeverRidesTheWire pins the pairing-channel invariant the
// mapper's mapTool doc comment claims: the tool_use_id shapes the DERIVED
// activity_id but never appears as a wire FIELD outside metadata, which is the
// one deliberate carrier (a structural identifier, and the audit channel).
//
// The old span root fields it used to check are gone, so this now scans the
// whole payload with metadata removed — a stricter test than the field list it
// replaces, since it catches a leak into any field, including one added later.
func TestWire_ToolUseIDNeverRidesTheWire(t *testing.T) {
	cl, bodies := newWireCapture(t)
	m := testMapper()
	m.NewID = nil

	pre, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-sentinel-xyz"})
	emit(t, cl, pre)
	payload := decodeBody(t, (*bodies)[0])

	meta, _ := payload["metadata"].(map[string]any)
	if meta == nil || meta["tool_use_id"] != "call-sentinel-xyz" {
		t.Errorf("metadata should carry tool_use_id (the deliberate audit channel): %v", meta)
	}
	delete(payload, "metadata")
	rest, _ := json.Marshal(payload)
	if strings.Contains(string(rest), "call-sentinel-xyz") {
		t.Errorf("tool_use_id leaked outside metadata: %s", rest)
	}
}

// TestWire_MCPIdentifiersRideActivityInput: the mcp server/tool identifiers used
// to ride the span's mcp family fields. With the span retired, activity_input is
// their only home — so an mcp call must still be distinguishable from a shell
// one on the wire, and the mapper must not overwrite the function with the
// tool_use_id.
func TestWire_MCPIdentifiersRideActivityInput(t *testing.T) {
	cl, bodies := newWireCapture(t)
	m := testMapper()
	m.NewID = nil

	pre, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "mcp__github__create_issue", ToolUseID: "call-9"})
	emit(t, cl, pre)
	payload := decodeBody(t, (*bodies)[0])
	assertActivityWireShape(t, payload, "ActivityStarted")

	in, _ := payload["activity_input"].(map[string]any)
	if in == nil {
		t.Fatalf("mcp ActivityStarted must carry activity_input: %v", payload)
	}
	if in["kind"] != "mcp" || in["mcp_server"] != "github" || in["mcp_tool"] != "create_issue" {
		t.Errorf("mcp identifiers wrong: kind=%v mcp_server=%v mcp_tool=%v",
			in["kind"], in["mcp_server"], in["mcp_tool"])
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
