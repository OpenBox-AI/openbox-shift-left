package client

import (
	"encoding/json"
	"strings"
	"testing"
)

func activityInput(t *testing.T, ev DevEvent) map[string]any {
	t.Helper()
	p := decodeRaw(t, ev)
	ai, ok := p["activity_input"]
	if !ok {
		return nil
	}
	m, ok := ai.(map[string]any)
	if !ok {
		t.Fatalf("activity_input is not an object: %v", ai)
	}
	return m
}

func signalArgs(t *testing.T, ev DevEvent) map[string]any {
	t.Helper()
	b, err := buildPayload(ev)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var raw struct {
		SignalArgs json.RawMessage `json:"signal_args"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(raw.SignalArgs) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw.SignalArgs, &m); err != nil {
		t.Fatalf("unmarshal signal_args: %v", err)
	}
	return m
}

func TestActivityInput_FileTool_StructuralOnly(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventToolCall, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-15T00:00:00Z", Tool: Tool{Name: "Edit", Kind: ToolFile},
		Span: &Span{SemanticType: "file_write", Stage: "started", FilePath: "/repo/a.go", FileOp: "edit"},
	}
	in := activityInput(t, ev)
	if in["tool_name"] != "Edit" || in["kind"] != "file" || in["file_path"] != "/repo/a.go" || in["file_operation"] != "edit" {
		t.Fatalf("file activity_input missing structural fields: %v", in)
	}
}

func TestActivityInput_MCPTool(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventToolCall, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-15T00:00:00Z", Tool: Tool{Name: "mcp__github__create_issue", Kind: ToolMCP, MCPServer: "github"},
		Span: &Span{SemanticType: "mcp_tool_call", Stage: "started", MCPServer: "github", Function: "create_issue"},
	}
	in := activityInput(t, ev)
	if in["mcp_server"] != "github" || in["mcp_tool"] != "create_issue" || in["tool_name"] != "mcp__github__create_issue" {
		t.Fatalf("mcp activity_input missing structural fields: %v", in)
	}
}

// TestActivityInput_ShellCarriesNoCommand iNV-2: a shell tool call must carry
// only the identifier; never the command text (which lives in the gated
// request_body, stripped by default).
func TestActivityInput_ShellCarriesNoCommand(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventToolCall, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-15T00:00:00Z", Tool: Tool{Name: "Bash", Kind: ToolShell},
		Span: &Span{SemanticType: "internal", Stage: "started", RequestBody: "rm -rf /tmp/danger"},
	}
	in := activityInput(t, stripContent(ev))
	if in["tool_name"] != "Bash" || in["kind"] != "shell" {
		t.Fatalf("shell activity_input should carry the identifier: %v", in)
	}
	b, _ := buildPayload(stripContent(ev))
	if strings.Contains(string(b), "rm -rf") {
		t.Fatalf("INV-2: shell command text leaked into the payload: %s", b)
	}
}

// TestActivityInput_OnlyOnStarted activity_input rides only the started event
// (which creates the Core event row); a ToolResult (completed) must not carry
// it (Core would ignore it, and it keeps the two paired events distinct on the
// wire).
func TestActivityInput_OnlyOnStarted(t *testing.T) {
	base := DevEvent{
		EventID: "e1", SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-15T00:00:00Z", Tool: Tool{Name: "Edit", Kind: ToolFile},
		Span: &Span{SemanticType: "file_write", FilePath: "/repo/a.go", FileOp: "edit"},
	}
	started := base
	started.EventType = EventToolCall
	started.Span.Stage = "started"
	if activityInput(t, started) == nil {
		t.Errorf("started ToolCall must carry activity_input")
	}
	completed := base
	completed.EventType = EventToolResult
	completed.Span.Stage = "completed"
	if _, ok := decodeRaw(t, completed)["activity_input"]; ok {
		t.Errorf("completed ToolResult must NOT carry activity_input")
	}
}

// TestActivityInput_EscalationContextIsTruncated pins the size cap on the one
// content field that still reaches the wire from a tool call: the Tier-2
// escalation context an approver decides on (Content.ToolInput).
func TestActivityInput_EscalationContextIsTruncated(t *testing.T) {
	huge := strings.Repeat("x", maxBodySize+5000)
	ev := DevEvent{
		EventID: "e1", EventType: EventToolCall, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-15T00:00:00Z", Tool: Tool{Name: "Bash", Kind: ToolShell},
		Span:    &Span{SemanticType: "shell_command", Stage: "started"},
		Content: &Content{ToolInput: huge},
	}
	cmd, _ := activityInput(t, ev)["command"].(string)
	if len([]rune(cmd)) != maxBodySize {
		t.Errorf("activity_input.command = %d runes, want capped to %d", len([]rune(cmd)), maxBodySize)
	}
}

// TestSignalArgs_PromptIsTruncated pins the same cap on the other surviving
// content field; the prompt carried under content capture.
func TestSignalArgs_PromptIsTruncated(t *testing.T) {
	huge := strings.Repeat("y", maxBodySize+5000)
	ev := DevEvent{
		EventID: "e1", EventType: EventPromptSubmitted, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-15T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
		Content: &Content{Prompt: huge},
	}
	prompt, _ := signalArgs(t, ev)["prompt"].(string)
	if len([]rune(prompt)) != maxBodySize {
		t.Errorf("signal_args.prompt = %d runes, want capped to %d", len([]rune(prompt)), maxBodySize)
	}
}

// TestSignalArgs_Prompt_EmptyByDefault a prompt's input IS the prompt text =
// gated content. Under the default metadata-only posture (no ev.Content),
// signal_args must be empty; permission_mode is session context (it stays in
// metadata/Overview), NOT the prompt's input.
func TestSignalArgs_Prompt_EmptyByDefault(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventPromptSubmitted, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-15T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
		Metadata: map[string]any{"permission_mode": "default"},
	}
	if args := signalArgs(t, ev); args != nil {
		t.Fatalf("metadata-only prompt must have empty signal_args (prompt is gated content); got %v", args)
	}
	b, _ := buildPayload(ev)
	if !strings.Contains(string(b), "permission_mode") {
		t.Errorf("permission_mode should still ride metadata: %s", b)
	}
}

// TestSignalArgs_Prompt_ContentGated when content-capture is enabled
// (ev.Content survives), the prompt is carried in signal_args as its input;
// gated + capped exactly like a tool request_body.
func TestSignalArgs_Prompt_ContentGated(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventPromptSubmitted, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-15T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
		Content: &Content{Prompt: "refactor the auth module"},
	}
	args := signalArgs(t, ev)
	if args["prompt"] != "refactor the auth module" {
		t.Fatalf("content-capture prompt should carry the prompt in signal_args: %v", args)
	}
	if a := signalArgs(t, stripContent(ev)); a != nil {
		t.Fatalf("stripped prompt must have empty signal_args; got %v", a)
	}
}

// TestSignalArgs_Commit_RidesTheUniformProjection replaces v1.8's
// lineage-allowlist case. commit_created and deploy used to name their keys in
// a hand-written list; they now ride the same projection as the other 25,
// because a deploy's lineage is exactly what a deploy policy matches on and an
// allowlist drifts on the first key a producer adds.
//
// The commit message is content, so it rides only when capture is on — that is
// the gate doing its job, not a leak. With capture off it must be gone from
// BOTH destinations.
func TestSignalArgs_Commit_RidesTheUniformProjection(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventCommitCreated, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-15T00:00:00Z", Tool: Tool{Name: "git", Kind: ToolShell},
		Metadata: map[string]any{"commit_sha": "abc123", "repo": "acme/app", "branch": "main", "message": "SECRET commit message body"},
	}
	args := signalArgs(t, ev)
	if args["commit_sha"] != "abc123" || args["repo"] != "acme/app" || args["branch"] != "main" {
		t.Fatalf("commit signal_args should carry lineage: %v", args)
	}
	if args["message"] != "SECRET commit message body" {
		t.Fatalf("with capture on the projection is uniform; message should ride: %v", args)
	}

	off := signalArgs(t, stripContent(ev))
	if _, leaked := off["message"]; leaked {
		t.Fatalf("INV-2: commit message content survived the gate in signal_args: %v", off)
	}
	if off["commit_sha"] != "abc123" {
		t.Fatalf("the gate took the structural lineage with it: %v", off)
	}
}

// TestSignalArgs_DeployLineageProjects is the one live producer of a lineage
// signal (internal/actions/openbox-git-action/deploy.go). commit_created has no
// producer at all, so this is the case that actually ships.
func TestSignalArgs_DeployLineageProjects(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventDeploy, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-15T00:00:00Z", Tool: Tool{Name: "git", Kind: ToolShell},
		Metadata: map[string]any{
			"deploy_id": "dpl-1", "commit_sha": "abc123", "repo": "acme/app",
			"environment": "prod", "deploy_did": "did:aip:y",
		},
	}
	args := signalArgs(t, ev)
	for k, want := range map[string]string{
		"deploy_id": "dpl-1", "commit_sha": "abc123", "repo": "acme/app",
		"environment": "prod", "deploy_did": "did:aip:y",
	} {
		if args[k] != want {
			t.Errorf("deploy signal_args[%q] = %v, want %q", k, args[k], want)
		}
	}
}

// TestSignalArgs_Prompt_Verbatim is R2: prompt_submitted's signal_args is a
// shipped goal-creating contract and the projection must not touch it. Core's
// stringifySignalArgs tries ["prompt","message","input","text","content"] in
// order, so a second key added here could silently become the goal text.
func TestSignalArgs_Prompt_Verbatim(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventPromptSubmitted, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-15T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
		Content: &Content{Prompt: "refactor the spool"},
		Metadata: map[string]any{
			"prompt_id": "p-1", "commit_sha": "abc123", "message": "SECRET",
		},
	}
	args := signalArgs(t, ev)
	if len(args) != 1 {
		t.Fatalf("prompt_submitted signal_args must carry exactly the prompt key, got %v", args)
	}
	if args["prompt"] != "refactor the spool" {
		t.Fatalf("prompt_submitted signal_args = %v, want the prompt verbatim", args)
	}
}

// TestSignalArgs_AbsentOnWorkflowEvents a WorkflowStarted/Completed (non-
// signal) lifecycle event carries no signal_args.
func TestSignalArgs_AbsentOnWorkflowEvents(t *testing.T) {
	ev := DevEvent{
		EventID: "e1", EventType: EventSessionStarted, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-15T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
		Metadata: map[string]any{"provider": "claude-code"},
	}
	if args := signalArgs(t, ev); args != nil {
		t.Fatalf("WorkflowStarted must not carry signal_args, got %v", args)
	}
}
