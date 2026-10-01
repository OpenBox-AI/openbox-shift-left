package muse

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func parseFixture(t *testing.T, name string) *HookEvent {
	t.Helper()
	ev, err := ParseHookEvent(strings.NewReader(fixture(t, name, "")))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return ev
}

func hookOf(ev *HookEvent) HookName { return HookName(ev.HookEventName) }

// The table every fixture is held to: what it maps to, or that it maps to
// nothing on purpose.
var fixtureEvents = map[string]client.EventType{
	"session-start-startup":       client.EventSessionStarted,
	"session-start-clear":         client.EventSessionStarted,
	"user-prompt-submit":          client.EventPromptSubmitted,
	"pre-tool-use-bash":           client.EventToolCall,
	"pre-tool-use-bash-escalated": client.EventToolCall,
	"pre-tool-use-read":           client.EventToolCall,
	"pre-tool-use-write":          client.EventToolCall,
	"pre-tool-use-edit":           client.EventToolCall,
	"pre-tool-use-mcp":            client.EventToolCall,
	"permission-request":          client.EventPermissionRequest,
	"post-tool-use":               client.EventToolResult,
	"post-tool-use-write":         client.EventToolResult,
	"post-tool-use-failure":       client.EventToolResult,
	"subagent-start":              client.EventSessionStarted,
	"stop-failure":                client.EventAPIError,
	"session-end":                 client.EventSessionEnded,
	"pre-llm-call":                client.EventModelCallRequested,
	"pre-llm-call-subagent":       client.EventModelCallRequested,
	"post-llm-call":               client.EventModelCallFinished,
	"post-llm-call-tool-calls":    client.EventModelCallFinished,
	"subagent-stop":               client.EventSessionEnded,
	"stop":                        "",
	"pre-compact":                 client.EventPreCompact,
	"post-compact":                client.EventPostCompact,
	"notification":                client.EventNotification,
}

func TestMapEveryFixture(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "*.json"))
	seen := map[string]bool{}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		seen[name] = true
		want, known := fixtureEvents[name]
		if !known {
			t.Errorf("fixture %s has no expected mapping; add it to fixtureEvents", name)
			continue
		}
		ev := parseFixture(t, name)
		got, ok := testMapper().Map(hookOf(ev), ev)
		if want == "" {
			if ok {
				t.Errorf("%s: mapped to %s, want nothing", name, got.EventType)
			}
			continue
		}
		if !ok || got.EventType != want {
			t.Errorf("%s: mapped to (%s, %v), want %s", name, got.EventType, ok, want)
			continue
		}
		if got.Tokens != nil || got.Cost != nil || got.TurnIndex != nil {
			t.Errorf("%s: carries usage, cost or a turn index", name)
		}
		if got.SessionID != ev.SessionID || !strings.HasPrefix(got.SessionID, "sess-000") || got.DeveloperDID != testDID {
			t.Errorf("%s: identity = %q / %q", name, got.SessionID, got.DeveloperDID)
		}
	}
	for name := range fixtureEvents {
		if !seen[name] {
			t.Errorf("fixtureEvents names %s, which is not in testdata", name)
		}
	}
}

// museInternalTools are Muse's own housekeeping tools, observed on 1.4.1 and
// deliberately unclassified: they map to shell-kinded, semantically opaque, and
// are still gated.
var museInternalTools = map[string]bool{"submit_reminder_decision": true}

// A write whose tool name the table does not know would be shell-kinded and
// opaque, invisible to content gating. Every tool of every fixture must be a
// recorded name, an MCP name, or a known internal tool.
func TestFixturesToolsAreClassified(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "*.json"))
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		ev := parseFixture(t, name)
		if ev.ToolName == "" {
			continue
		}
		if !strings.HasPrefix(ev.ToolName, "mcp__") && !isBuiltin(ev.ToolName) && !museInternalTools[ev.ToolName] {
			t.Errorf("%s: tool %q is not in builtinTools and would be treated as opaque", name, ev.ToolName)
		}
	}
	// The tools listed in PreLLMCall's payload are the model's whole tool set.
	for _, raw := range parseFixture(t, "pre-llm-call").Tools {
		n := nameOf(raw)
		if !strings.HasPrefix(n, "mcp__") && !isBuiltin(n) {
			t.Errorf("model-call tool %q is unclassified", n)
		}
	}
}

func TestClassifyTool(t *testing.T) {
	tests := []struct {
		name   string
		kind   client.ToolKind
		sem    string
		server string
		fn     string
	}{
		{"Bash", client.ToolShell, "internal", "", ""},
		{"Write", client.ToolFile, "file_write", "", ""},
		{"write_file", client.ToolFile, "file_write", "", ""},
		{"Edit", client.ToolFile, "file_write", "", ""},
		{"edit_file", client.ToolFile, "file_write", "", ""},
		{"MultiEdit", client.ToolFile, "file_write", "", ""},
		{"apply_patch", client.ToolFile, "file_write", "", ""},
		{"Read", client.ToolFile, "file_read", "", ""},
		{"Glob", client.ToolFile, "internal", "", ""},
		{"Grep", client.ToolFile, "internal", "", ""},
		{"WebFetch", client.ToolShell, "internal", "", ""},
		{"mcp__docs__search", client.ToolMCP, "mcp_tool_call", "docs", "search"},
		{"mcp__github__create__issue", client.ToolMCP, "mcp_tool_call", "github", "create__issue"},
		{"mcp__", client.ToolShell, "internal", "", ""},
		{"never_heard_of_it", client.ToolShell, "internal", "", ""},
	}
	for _, tt := range tests {
		kind, sem, _, server, fn := classifyTool(tt.name)
		if kind != tt.kind || sem != tt.sem || server != tt.server || fn != tt.fn {
			t.Errorf("classifyTool(%q) = (%s,%s,%s,%s)", tt.name, kind, sem, server, fn)
		}
	}
}

func TestToolCallIdentityAndGateRecordCarryToolUseID(t *testing.T) {
	m := testMapper()
	for _, name := range []string{"pre-tool-use-bash", "pre-tool-use-read", "pre-tool-use-write", "pre-tool-use-edit", "pre-tool-use-mcp"} {
		ev := parseFixture(t, name)
		got, _ := m.Map(HookPreToolUse, ev)
		if got.Span == nil || got.Span.InvocationID != ev.ToolUseID || ev.ToolUseID == "" {
			t.Errorf("%s: span = %+v, want invocation id %q", name, got.Span, ev.ToolUseID)
		}
		if got.Metadata["tool_use_id"] != ev.ToolUseID {
			t.Errorf("%s: metadata tool_use_id = %v", name, got.Metadata["tool_use_id"])
		}
		if got.StartedAt == "" {
			t.Errorf("%s: no started_at", name)
		}
	}
	w, _ := m.Map(HookPreToolUse, parseFixture(t, "pre-tool-use-write"))
	if w.Span.SemanticType != "file_write" || w.Span.FilePath != "a.txt" || w.Span.FileOp != "write" {
		t.Errorf("write span = %+v", w.Span)
	}
	mc, _ := m.Map(HookPreToolUse, parseFixture(t, "pre-tool-use-mcp"))
	if mc.Tool.Kind != client.ToolMCP || mc.Tool.MCPServer != "docs" || mc.Span.Function != "search" {
		t.Errorf("mcp tool = %+v span = %+v", mc.Tool, mc.Span)
	}
}

// The started and completed halves of one call must name one activity.
func TestToolHalvesShareAnOperation(t *testing.T) {
	m := testMapper()
	pre, _ := m.Map(HookPreToolUse, parseFixture(t, "pre-tool-use-bash"))
	post, _ := m.Map(HookPostToolUse, parseFixture(t, "post-tool-use"))
	if pre.Span.OperationID == "" || pre.Span.OperationID != post.Span.OperationID || pre.Span.InvocationID != post.Span.InvocationID {
		t.Errorf("pre %+v vs post %+v", pre.Span, post.Span)
	}
	if post.Status != client.StatusCompleted {
		t.Errorf("PostToolUse status = %q", post.Status)
	}
	failed, _ := m.Map(HookPostToolUseFailure, parseFixture(t, "post-tool-use-failure"))
	if failed.Status != client.StatusFailed || failed.EventType != client.EventToolResult {
		t.Errorf("PostToolUseFailure = %s / %q", failed.EventType, failed.Status)
	}
}

// Every key a write or edit tool can carry a body under is a content key, and
// the same precedence reads it and rewrites it.
func TestContentFieldKeysCoverEveryBodyKey(t *testing.T) {
	for _, key := range contentFieldKeys {
		e := &HookEvent{ToolName: "Write", ToolInput: json.RawMessage(`{"file_path":"/a","` + key + `":"the body"}`)}
		if got := e.fileText(); got != "the body" {
			t.Errorf("fileText under %q = %q", key, got)
		}
		rebuilt := hookflow.RedactToolInput(e.ToolInput, "scrubbed", contentFieldKeys)
		if !strings.Contains(string(rebuilt), `"`+key+`":"scrubbed"`) || !strings.Contains(string(rebuilt), `"file_path":"/a"`) {
			t.Errorf("rewrite under %q = %s", key, rebuilt)
		}
	}
	for _, fx := range []string{"pre-tool-use-write", "pre-tool-use-edit"} {
		if parseFixture(t, fx).fileText() == "" {
			t.Errorf("%s: the body was not found under contentFieldKeys", fx)
		}
	}
}

func TestPermissionModeIsClosedEnum(t *testing.T) {
	m := testMapper()
	ev := parseFixture(t, "pre-tool-use-bash")
	if got, _ := m.Map(HookPreToolUse, ev); got.Metadata["permission_mode"] != "default" {
		t.Errorf("permission_mode = %v", got.Metadata["permission_mode"])
	}
	ev.PermissionMode = "some-mode-muse-invented"
	if got, _ := m.Map(HookPreToolUse, ev); got.Metadata["permission_mode"] != nil {
		t.Errorf("an undeclared mode egressed: %v", got.Metadata["permission_mode"])
	}
}

func TestSessionStartCarriesRunLineageOnlyWhenContinued(t *testing.T) {
	ev := parseFixture(t, "session-start-clear")
	m := testMapper()
	plain, _ := m.Map(HookSessionStart, ev)
	if plain.RunID != "" || plain.ContinuedFromRunID != "" {
		t.Errorf("generation 0 carried a run: %+v", plain)
	}
	m.Run = &RunIdentity{Generation: 2, RunID: "run-2", ContinuedFrom: "run-1"}
	got, _ := m.Map(HookSessionStart, ev)
	if got.RunID != "run-2" || got.RunGeneration != 2 || got.ContinuedFromRunID != "run-1" {
		t.Errorf("continued start = %+v", got)
	}
	tool, _ := m.Map(HookPreToolUse, parseFixture(t, "pre-tool-use-bash"))
	if tool.RunID != "run-2" || tool.ContinuedFromRunID != "" {
		t.Errorf("a non-start event must carry the run but not the lineage: %+v", tool)
	}
	for src, want := range map[string]bool{"startup": false, "compact": false, "": false, "resume": true, "clear": false, "bogus": false} {
		if isBumpSource(src) != want {
			t.Errorf("isBumpSource(%q) = %v", src, !want)
		}
	}
}

func TestContentIsGatedAndRedacted(t *testing.T) {
	secret := "AKIA" + "IOSFODNN7" + "EXAMPLE"
	redact := func(s string) string { return strings.ReplaceAll(s, secret, "[scrubbed]") }

	prompt := &HookEvent{SessionID: "s", Prompt: "deploy with " + secret}
	m := testMapper()
	if got, _ := m.Map(HookUserPromptSubmit, prompt); got.Content != nil {
		t.Errorf("capture off still carried content: %+v", got.Content)
	}
	m.CaptureContent, m.RedactContent = true, redact
	got, _ := m.Map(HookUserPromptSubmit, prompt)
	if got.Content == nil || strings.Contains(got.Content.Prompt, secret) || !strings.Contains(got.Content.Prompt, "deploy with") {
		t.Errorf("prompt content = %+v", got.Content)
	}

	post := &HookEvent{SessionID: "s", ToolName: "Bash", ToolUseID: "t1", ToolResponse: json.RawMessage(`{"output":"key ` + secret + `"}`)}
	out, _ := m.Map(HookPostToolUse, post)
	if out.Content == nil || strings.Contains(out.Content.ToolOutput, secret) || !strings.Contains(out.Content.ToolOutput, "key") {
		t.Errorf("tool output content = %+v", out.Content)
	}
}

func TestMapDropsWhatItCannotPlace(t *testing.T) {
	m := testMapper()
	if _, ok := m.Map(HookPreToolUse, &HookEvent{ToolName: "Bash"}); ok {
		t.Error("an event with no session id mapped")
	}
	if _, ok := m.Map(HookPreToolUse, nil); ok {
		t.Error("a nil event mapped")
	}
	bad := NewMapper(Identity{DeveloperDID: "not-a-did"})
	if _, ok := bad.Map(HookPreToolUse, &HookEvent{SessionID: "s"}); ok {
		t.Error("an event under a bad DID mapped")
	}
	for _, h := range []HookName{HookStop, HookInterrupt, HookPostToolBatch} {
		if _, ok := m.Map(h, &HookEvent{SessionID: "s"}); ok {
			t.Errorf("%s mapped; it has no contract type", h)
		}
	}
}

// A subagent is a session of its own under its own id, with no parent link in
// any payload: its start and stop are that session's SessionStarted and
// SessionEnded.
func TestSubagentIsASessionOfItsOwn(t *testing.T) {
	m := testMapper()
	start, ok := m.Map(HookSubagentStart, parseFixture(t, "subagent-start"))
	if !ok || start.EventType != client.EventSessionStarted || start.SessionID != "sess-0002" {
		t.Fatalf("SubagentStart = %+v %v", start, ok)
	}
	if start.Metadata["agent_id"] != "skill-reminder" || start.Metadata["agent_type"] != subagentAgentType {
		t.Errorf("subagent start metadata = %v", start.Metadata)
	}
	for k := range start.Metadata {
		if strings.Contains(k, "parent") {
			t.Errorf("metadata names a parent (%s), which no payload carries", k)
		}
	}
	stop, ok := m.Map(HookSubagentStop, parseFixture(t, "subagent-stop"))
	if !ok || stop.EventType != client.EventSessionEnded || stop.SessionID != "sess-0002" || stop.EndedAt == "" {
		t.Fatalf("SubagentStop = %+v %v", stop, ok)
	}
	if stop.Metadata["agent_id"] != "skill-reminder" || stop.Content != nil {
		t.Errorf("subagent stop = %+v", stop)
	}
	sp := parseFixture(t, "subagent-start")
	if sp.SessionID != sp.ChildSessionID || sp.SessionID != sp.TurnID {
		t.Errorf("a subagent's session, child session and turn ids are one id: %q %q %q", sp.SessionID, sp.ChildSessionID, sp.TurnID)
	}
}

func TestStopFailureMapping(t *testing.T) {
	sf, _ := testMapper().Map(HookStopFailure, parseFixture(t, "stop-failure"))
	if sf.EventType != client.EventAPIError || sf.Content != nil {
		t.Errorf("StopFailure = %+v", sf)
	}
}

// The live-captured shapes: trigger is Muse's own soft|hard, passed through as
// said; the notification message carries the project name, so title and message
// are gated content.
func TestLifecycleSignalsMapAsCapturedAndGateContent(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		want    client.EventType
	}{{"pre-compact", client.EventPreCompact}, {"post-compact", client.EventPostCompact}} {
		got, ok := testMapper().Map(hookOf(parseFixture(t, tc.fixture)), parseFixture(t, tc.fixture))
		if !ok || got.EventType != tc.want || got.Metadata["trigger"] != "soft" || got.Content != nil {
			t.Errorf("%s = %+v %v", tc.fixture, got, ok)
		}
	}
	ev := parseFixture(t, "pre-compact")
	ev.Trigger = "hard"
	if got, _ := testMapper().Map(HookPreCompact, ev); got.Metadata["trigger"] != "hard" {
		t.Errorf("hard trigger = %v", got.Metadata["trigger"])
	}
	ev.Trigger = "something else"
	if got, _ := testMapper().Map(HookPreCompact, ev); got.Metadata["trigger"] != nil {
		t.Errorf("an unknown trigger egressed: %v", got.Metadata["trigger"])
	}

	n := parseFixture(t, "notification")
	off, _ := testMapper().Map(HookNotification, n)
	if off.Metadata["notification_type"] != "permission_prompt" || off.Content != nil || off.Metadata["notification_title"] != nil {
		t.Errorf("capture off: %+v", off)
	}
	on := testMapper()
	on.CaptureContent = true
	on.RedactContent = strings.ToUpper
	got, _ := on.Map(HookNotification, n)
	if got.Metadata["notification_title"] != strings.ToUpper(n.Title) || got.Content == nil || got.Content.SignalDetail != strings.ToUpper(n.Message) {
		t.Errorf("capture on: %+v %+v", got.Metadata, got.Content)
	}
}
