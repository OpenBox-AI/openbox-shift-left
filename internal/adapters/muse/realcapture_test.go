package muse

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

// These tests pin the payload shapes observed on a real Muse 1.4.1 (see
// testdata/README.md): native tool names, content-block messages, flat dotted
// options and the statuses PostLLMCall actually sends.

func TestNativeToolNamesAreClassified(t *testing.T) {
	for name, want := range map[string]struct {
		kind client.ToolKind
		sem  string
	}{
		"bash":       {client.ToolShell, "internal"},
		"bash_input": {client.ToolShell, "internal"},
		"read_file":  {client.ToolFile, "file_read"},
		"write_file": {client.ToolFile, "file_write"},
		"edit_file":  {client.ToolFile, "file_write"},
		"search":     {client.ToolFile, "internal"},
	} {
		kind, sem, _, _, _ := classifyTool(name)
		if kind != want.kind || sem != want.sem || !isBuiltin(name) {
			t.Errorf("%s = (%s, %s, builtin=%v), want (%s, %s)", name, kind, sem, isBuiltin(name), want.kind, want.sem)
		}
	}
	if !isHighRiskClass("bash_input") || !isHighRiskClass("bash") {
		t.Error("a shell tool is a high-risk class")
	}
}

// A tool this adapter has never heard of maps the same way every time and is
// still gated: it reaches core like any other PreToolUse.
func TestUnknownToolMapsDeterministicallyAndIsGated(t *testing.T) {
	for _, name := range []string{"workflow", "cron_create", "write_memory", "submit_reminder_decision", "brand_new_tool"} {
		e := &HookEvent{SessionID: "s", ToolName: name, ToolUseID: "call_9", ToolInput: json.RawMessage(`{"a":1}`)}
		first, ok := testMapper().Map(HookPreToolUse, e)
		second, _ := testMapper().Map(HookPreToolUse, e)
		if !ok || first.Tool.Kind != client.ToolShell || first.Span.SemanticType != "internal" || first.Tool.Name != name {
			t.Errorf("%s mapped to %+v / %+v", name, first.Tool, first.Span)
		}
		if first.EventID != second.EventID && testMapper().NewID == nil {
			t.Errorf("%s: the mapping is not deterministic", name)
		}
	}

	setHookEnv(t)
	f := serveCore(t, fakecore.Script{Default: allowJSON})
	payload := strings.Replace(fixture(t, "pre-tool-use-bash", "s-unknown"), `"tool_name": "bash"`, `"tool_name": "workflow"`, 1)
	before := f.Hits()
	if out, _ := runHook(t, "PreToolUse", payload); strings.TrimSpace(out) != "" {
		t.Fatalf("an allowed unknown tool wrote %q", out)
	}
	if f.Hits() == before {
		t.Fatal("an unknown tool was never evaluated")
	}
}

// bash's command, write_file's and edit_file's path, and the bodies of both
// writers, are read from the keys Muse really sends.
func TestNativeToolInputKeys(t *testing.T) {
	bash := parseFixture(t, "pre-tool-use-bash")
	if bash.command() != "ls -la" || bash.shellText() != "ls -la" {
		t.Errorf("bash command = %q", bash.command())
	}
	w := parseFixture(t, "pre-tool-use-write")
	if w.filePath() != "a.txt" || w.fileText() != "hello" {
		t.Errorf("write_file path=%q body=%q", w.filePath(), w.fileText())
	}
	ed := parseFixture(t, "pre-tool-use-edit")
	if ed.filePath() != "a.txt" || ed.fileText() != "goodbye" {
		t.Errorf("edit_file path=%q body=%q (replace is the text that lands on disk)", ed.filePath(), ed.fileText())
	}
	if r := parseFixture(t, "pre-tool-use-read"); r.filePath() != "a.txt" {
		t.Errorf("read_file path = %q", r.filePath())
	}
	esc := parseFixture(t, "pre-tool-use-bash-escalated")
	if esc.command() == "" || !strings.Contains(string(esc.ToolInput), "sandbox_permissions") {
		t.Errorf("escalated bash input = %s", esc.ToolInput)
	}
}

// A shell tool whose input is not {command} (bash_input) is not read as empty:
// the decider and the content gate see the whole input.
func TestShellToolWithoutACommandKeyIsNotEmpty(t *testing.T) {
	e := &HookEvent{SessionID: "s", ToolName: "bash_input", ToolInput: json.RawMessage(`{"session":"exec-1","input":"rm -rf x\n"}`)}
	if got := e.shellText(); !strings.Contains(got, "rm -rf x") {
		t.Fatalf("shellText = %q", got)
	}
	req := buildDecisionRequest(Identity{DeveloperDID: testDID}, e, true)
	if cmd, _ := req.Attributes["command"].(string); !strings.Contains(cmd, "rm -rf x") {
		t.Errorf("decision attributes = %v", req.Attributes)
	}
	if toolInputExtract(e) == "" {
		t.Error("the observe extraction of a shell call with no command is empty")
	}
	if (&HookEvent{ToolName: "bash_input", ToolInput: json.RawMessage(`null`)}).shellText() != "" {
		t.Error("a null tool_input reads as text")
	}
}

// Neither half of an edit can carry a secret past the gate: find and replace
// are both scanned, whichever one the rewrite lever would swap.
func TestEditFileFindAndReplaceAreBothRedactedOnTheGateCopy(t *testing.T) {
	secret := "AKIA" + "IOSFODNN7" + "EXAMPLE"
	for _, in := range []string{
		`{"path":"a.txt","find":"key = ` + secret + `","replace":"key = x"}`,
		`{"path":"a.txt","find":"key = x","replace":"key = ` + secret + `"}`,
	} {
		m := testMapper()
		m.RedactContent = func(s string) string { return strings.ReplaceAll(s, secret, "[scrubbed]") }
		e := &HookEvent{SessionID: "s", ToolName: "edit_file", ToolUseID: "call_9", ToolInput: json.RawMessage(in)}
		ev, ok := enforceTarget{id: Identity{DeveloperDID: testDID}, mapper: m, ev: e}.DevEvent(nil)
		if !ok || ev.Content == nil || strings.Contains(ev.Content.ToolInput, secret) || !strings.Contains(ev.Content.ToolInput, "[scrubbed]") {
			t.Errorf("gate copy of %s = %+v", in, ev.Content)
		}
	}
}

func TestPostToolUseUnwrapsTheBashResultString(t *testing.T) {
	m := testMapper()
	m.CaptureContent = true
	post, _ := m.Map(HookPostToolUse, parseFixture(t, "post-tool-use"))
	if post.Content == nil || post.Content.ToolOutput != "total 0\n" {
		t.Errorf("bash output = %+v, want the inner output text", post.Content)
	}
	// An object response without an output key stays raw JSON, and a plain
	// string is taken as is.
	w, _ := m.Map(HookPostToolUse, parseFixture(t, "post-tool-use-write"))
	if w.Content == nil || !strings.Contains(w.Content.ToolOutput, "structuredPatch") {
		t.Errorf("write_file response = %+v", w.Content)
	}
	if got := outputText(json.RawMessage(`"Read text file ` + "`a.txt`" + `.\n1|goodbye"`)); !strings.HasPrefix(got, "Read text file") {
		t.Errorf("plain string response = %q", got)
	}
	failed, _ := m.Map(HookPostToolUseFailure, parseFixture(t, "post-tool-use-failure"))
	if failed.Status != client.StatusFailed || failed.Content == nil || !strings.Contains(failed.Content.ToolOutput, "Operation not permitted") || strings.Contains(failed.Content.ToolOutput, "chunk_id") {
		t.Errorf("failure = %s %+v", failed.Status, failed.Content)
	}
}

// A PermissionRequest has no tool_use_id to pair with, and is a signal.
func TestPermissionRequestCarriesNoToolUseID(t *testing.T) {
	e := parseFixture(t, "permission-request")
	if e.ToolUseID != "" {
		t.Fatalf("tool_use_id = %q", e.ToolUseID)
	}
	ev, ok := testMapper().Map(HookPermissionRequest, e)
	if !ok || ev.EventType != client.EventPermissionRequest || ev.Span != nil || ev.Tool.Name != "submit_reminder_decision" {
		t.Errorf("event = %+v", ev)
	}
	if pre := parseFixture(t, "pre-tool-use-bash"); !strings.HasPrefix(pre.ToolUseID, "call_") || pre.ModelProvider != "meta" {
		t.Errorf("PreToolUse tool_use_id=%q model_provider=%q", pre.ToolUseID, pre.ModelProvider)
	}
}

func TestPreLLMCallRequestIsBuiltFromTheCapturedMessages(t *testing.T) {
	m := testMapper()
	m.CaptureContent = true
	ev, _ := m.Map(HookPreLLMCall, parseFixture(t, "pre-llm-call"))
	if ev.Span == nil || !strings.Contains(ev.Span.RequestBody, "list the workspace files") ||
		!strings.Contains(ev.Span.RequestBody, "You are a coding agent working in /tmp/proj.") {
		t.Fatalf("request body = %+v", ev.Span)
	}
}

// Muse sends a success with a null finish_reason and a tool_calls status with a
// finish_reason; both completed, neither carries text or usage.
func TestPostLLMCallStatusesObservedOnARealMuse(t *testing.T) {
	m := testMapper()
	for name, want := range map[string]struct{ finish string }{
		"post-llm-call":            {""},
		"post-llm-call-tool-calls": {"tool_calls"},
	} {
		ev, _ := m.Map(HookPostLLMCall, parseFixture(t, name))
		if ev.Status != client.StatusCompleted {
			t.Errorf("%s: status = %q", name, ev.Status)
		}
		got, _ := ev.Metadata["finish_reason"].(string)
		if got != want.finish {
			t.Errorf("%s: finish_reason = %q, want %q", name, got, want.finish)
		}
		raw, _ := json.Marshal(ev)
		for _, leaked := range []string{"output_text_preview", "Neutral reply text", "cached_tokens", "traceparent"} {
			if strings.Contains(string(raw), leaked) {
				t.Errorf("%s: %s reached the event", name, leaked)
			}
		}
	}
}

// An empty or unreadable payload on a gated event is answered with that event's
// own closed answer shape, and never latches the run.
func TestUnreadableGatedPayloadIsASchemaValidRefusalThatCannotLatch(t *testing.T) {
	setHookEnv(t)
	serveCore(t, fakecore.Script{Default: allowJSON})
	for _, tc := range contractCases {
		for _, payload := range []string{"", "   ", "{trunc"} {
			out, _ := runHook(t, string(tc.event), payload)
			m := decodeAnswer(t, []byte(strings.TrimSpace(out)))
			if extra := subset(keysOf(m), tc.topKeys); len(extra) != 0 {
				t.Errorf("%s %q: keys outside the contract: %v", tc.name, payload, extra)
			}
			if hso, ok := m["hookSpecificOutput"].(map[string]any); ok {
				if extra := subset(keysOf(hso), tc.hsoKeys); len(extra) != 0 {
					t.Errorf("%s %q: hookSpecificOutput keys outside the contract: %v", tc.name, payload, extra)
				}
			}
			if !strings.Contains(out, "could not read this hook payload") {
				t.Errorf("%s %q: refusal = %q", tc.name, payload, out)
			}
		}
	}
	if l := haltLatches(t); len(l) != 0 {
		t.Fatalf("an unreadable payload latched the run: %v", l)
	}
	// The next, readable call is judged on its own.
	if out, _ := runHook(t, "PreLLMCall", fixture(t, "pre-llm-call", "s-after-empty")); strings.TrimSpace(out) != "" {
		t.Fatalf("a readable call after an empty one was refused: %q", out)
	}
}
