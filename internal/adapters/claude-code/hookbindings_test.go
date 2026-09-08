package claudecode

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// registeredHookNames32 is every hook this adapter registers after phase 05:
// the 11 pre-existing hooks plus the 21 new classes from phase 04's 33-row
// contract table, table-B order. HookWorktreeCreate is deliberately absent --
// registering it would make this hook responsible for printing the created
// worktree path back to Claude Code, breaking `claude --worktree`,
// isolation:"worktree" subagents and background sessions.
var registeredHookNames32 = []string{
	"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
	"PostToolUseFailure", "Stop", "SubagentStop", "SubagentStart",
	"PermissionDenied", "StopFailure", "SessionEnd",
	"Setup", "InstructionsLoaded", "UserPromptExpansion", "MessageDisplay",
	"PermissionRequest", "PostToolBatch", "Notification", "TaskCreated",
	"TaskCompleted", "TeammateIdle", "ConfigChange", "CwdChanged",
	"DirectoryAdded", "FileChanged", "WorktreeRemove", "PreCompact",
	"PostCompact", "PreModelSwitch", "PostModelSwitch", "Elicitation",
	"ElicitationResult",
}

// TestParseHookName_Accepts32AndRejectsWorktreeCreate is the plan-level
// vocabulary count: ParseHookName is the dispatch gate (hookevent.go:54), and
// a constant with no registration would let a future edit register it by
// accident -- so the refusal of WorktreeCreate is pinned here, not left to
// accident.
func TestParseHookName_Accepts32AndRejectsWorktreeCreate(t *testing.T) {
	if got, want := len(hookNames), 32; got != want {
		t.Fatalf("len(hookNames) = %d, want %d", got, want)
	}
	if got, want := len(registeredHookNames32), 32; got != want {
		t.Fatalf("test fixture registeredHookNames32 has %d entries, want %d", got, want)
	}
	for _, name := range registeredHookNames32 {
		if _, err := ParseHookName(name); err != nil {
			t.Errorf("ParseHookName(%q) errored: %v", name, err)
		}
	}
	if _, err := ParseHookName("WorktreeCreate"); err == nil {
		t.Error("ParseHookName(\"WorktreeCreate\") should error: WorktreeCreate is refused, not missed")
	}
	if _, err := ParseHookName("Nope"); err == nil {
		t.Error("ParseHookName(\"Nope\") should error")
	}
}

// TestPromptIDBinding_CommonField. prompt_id is the best cross-event
// correlator, bound as a common field on every payload once one exists.
func TestPromptIDBinding_CommonField(t *testing.T) {
	body := `{"hook_event_name":"MessageDisplay","session_id":"s1","cwd":"/r","prompt_id":"pr_123"}`
	ev, err := ParseHookEvent(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.PromptID != "pr_123" {
		t.Errorf("PromptID = %q, want pr_123", ev.PromptID)
	}

	// Absent pre-first-prompt (e.g. SessionStart): decodes to "", and compact()
	// drops empty strings, so it never egresses as a false claim.
	absent := `{"hook_event_name":"SessionStart","session_id":"s1","cwd":"/r"}`
	ev2, err := ParseHookEvent(strings.NewReader(absent))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev2.PromptID != "" {
		t.Errorf("PromptID absent should decode to empty string, got %q", ev2.PromptID)
	}
}

// TestNumericAndBoolFieldsAbsentStayNil. compact() drops empty strings only; a
// plain int/bool binds to 0/false when the key is absent and egresses as a
// claim. Every new numeric or boolean field must be a pointer so "absent"
// stays absent.
func TestNumericAndBoolFieldsAbsentStayNil(t *testing.T) {
	body := `{"hook_event_name":"MessageDisplay","session_id":"s1","cwd":"/r"}`
	ev, err := ParseHookEvent(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Index != nil {
		t.Errorf("Index should be nil when absent, got %v", *ev.Index)
	}
	if ev.Final != nil {
		t.Errorf("Final should be nil when absent, got %v", *ev.Final)
	}
	if ev.ContextTokens != nil {
		t.Errorf("ContextTokens should be nil when absent, got %v", *ev.ContextTokens)
	}
	if ev.PromptCacheWarm != nil {
		t.Errorf("PromptCacheWarm should be nil when absent, got %v", *ev.PromptCacheWarm)
	}
	if ev.EstimatedCacheWriteUSD != nil {
		t.Errorf("EstimatedCacheWriteUSD should be nil when absent, got %v", *ev.EstimatedCacheWriteUSD)
	}

	present := `{"hook_event_name":"MessageDisplay","session_id":"s1","cwd":"/r",` +
		`"index":3,"final":true}`
	ev2, err := ParseHookEvent(strings.NewReader(present))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev2.Index == nil || *ev2.Index != 3 {
		t.Errorf("Index = %v, want 3", ev2.Index)
	}
	if ev2.Final == nil || !*ev2.Final {
		t.Errorf("Final = %v, want true", ev2.Final)
	}
}

// TestFieldBindings_OneEventPerNewGroup spot-checks one payload per new field
// group against phase 04's field binding table, exact JSON keys.
func TestFieldBindings_OneEventPerNewGroup(t *testing.T) {
	cases := []struct {
		name string
		body string
		want func(*HookEvent) (got, want any)
	}{
		{"Setup trigger", `{"hook_event_name":"Setup","session_id":"s","cwd":"/r","trigger":"init"}`,
			func(e *HookEvent) (any, any) { return e.Trigger, "init" }},
		{"InstructionsLoaded", `{"hook_event_name":"InstructionsLoaded","session_id":"s","cwd":"/r",` +
			`"memory_type":"Project","load_reason":"session_start","file_path":"CLAUDE.md",` +
			`"trigger_file_path":"a.go","parent_file_path":"b.go","globs":["*.go","*.md"]}`,
			func(e *HookEvent) (any, any) {
				if e.FilePath != "CLAUDE.md" || e.LoadReason != "session_start" ||
					e.TriggerFilePath != "a.go" || e.ParentFilePath != "b.go" ||
					len(e.Globs) != 2 {
					return e.MemoryType, "unused"
				}
				return e.MemoryType, "Project"
			}},
		{"UserPromptExpansion", `{"hook_event_name":"UserPromptExpansion","session_id":"s","cwd":"/r",` +
			`"expansion_type":"slash_command","command_name":"/plan","command_args":"x","command_source":"user"}`,
			func(e *HookEvent) (any, any) {
				return []string{e.ExpansionType, e.CommandName, e.CommandArgs, e.CommandSource},
					[]string{"slash_command", "/plan", "x", "user"}
			}},
		{"PostToolBatch tool_calls", `{"hook_event_name":"PostToolBatch","session_id":"s","cwd":"/r",` +
			`"tool_calls":[{"tool_use_id":"tu_1"}]}`,
			func(e *HookEvent) (any, any) { return string(e.ToolCalls), `[{"tool_use_id":"tu_1"}]` }},
		{"Notification", `{"hook_event_name":"Notification","session_id":"s","cwd":"/r",` +
			`"message":"hi","title":"t","notification_type":"idle"}`,
			func(e *HookEvent) (any, any) {
				return []string{e.Message, e.Title, e.NotificationType}, []string{"hi", "t", "idle"}
			}},
		{"TaskCreated", `{"hook_event_name":"TaskCreated","session_id":"s","cwd":"/r",` +
			`"task_id":"tk1","task_subject":"subj","task_description":"desc",` +
			`"teammate_name":"alice","team_name":"core"}`,
			func(e *HookEvent) (any, any) {
				return []string{e.TaskID, e.TaskSubject, e.TaskDescription, e.TeammateName, e.TeamName},
					[]string{"tk1", "subj", "desc", "alice", "core"}
			}},
		{"CwdChanged", `{"hook_event_name":"CwdChanged","session_id":"s","cwd":"/r",` +
			`"old_cwd":"/a","new_cwd":"/b"}`,
			func(e *HookEvent) (any, any) { return []string{e.OldCwd, e.NewCwd}, []string{"/a", "/b"} }},
		{"DirectoryAdded", `{"hook_event_name":"DirectoryAdded","session_id":"s","cwd":"/r",` +
			`"directory":"/d","source":"slash_command"}`,
			func(e *HookEvent) (any, any) { return []string{e.Directory, e.Source}, []string{"/d", "slash_command"} }},
		{"FileChanged", `{"hook_event_name":"FileChanged","session_id":"s","cwd":"/r",` +
			`"file_path":".env","event":"change"}`,
			func(e *HookEvent) (any, any) {
				return []string{e.FilePath, e.FileChangeEvent}, []string{".env", "change"}
			}},
		{"WorktreeRemove", `{"hook_event_name":"WorktreeRemove","session_id":"s","cwd":"/r",` +
			`"worktree_path":"/wt"}`,
			func(e *HookEvent) (any, any) { return e.WorktreePath, "/wt" }},
		{"PreCompact", `{"hook_event_name":"PreCompact","session_id":"s","cwd":"/r",` +
			`"trigger":"manual","custom_instructions":"keep tests"}`,
			func(e *HookEvent) (any, any) {
				return []string{e.Trigger, e.CustomInstructions}, []string{"manual", "keep tests"}
			}},
		{"PreCompact custom_instructions null", `{"hook_event_name":"PreCompact","session_id":"s","cwd":"/r",` +
			`"trigger":"auto","custom_instructions":null}`,
			func(e *HookEvent) (any, any) { return e.CustomInstructions, "" }},
		{"PostCompact", `{"hook_event_name":"PostCompact","session_id":"s","cwd":"/r",` +
			`"trigger":"manual","compact_summary":"summary text"}`,
			func(e *HookEvent) (any, any) { return e.CompactSummary, "summary text" }},
		{"PreModelSwitch", `{"hook_event_name":"PreModelSwitch","session_id":"s","cwd":"/r",` +
			`"from_model":"a","to_model":"b","requested_model":"b","source":"command",` +
			`"context_tokens":100,"prompt_cache_warm":false,"cache_ttl":"5m",` +
			`"estimated_cache_write_usd":0.5,"pricing":"configured"}`,
			func(e *HookEvent) (any, any) {
				if e.ContextTokens == nil || *e.ContextTokens != 100 {
					return "context_tokens", "mismatch"
				}
				if e.PromptCacheWarm == nil || *e.PromptCacheWarm != false {
					return "prompt_cache_warm", "mismatch"
				}
				if e.EstimatedCacheWriteUSD == nil || *e.EstimatedCacheWriteUSD != 0.5 {
					return "estimated_cache_write_usd", "mismatch"
				}
				return []string{e.FromModel, e.ToModel, e.RequestedModel, e.CacheTTL, e.Pricing, e.Source},
					[]string{"a", "b", "b", "5m", "configured", "command"}
			}},
		{"PreModelSwitch requested_model null", `{"hook_event_name":"PreModelSwitch","session_id":"s","cwd":"/r",` +
			`"requested_model":null}`,
			func(e *HookEvent) (any, any) { return e.RequestedModel, "" }},
		{"Elicitation", `{"hook_event_name":"Elicitation","session_id":"s","cwd":"/r",` +
			`"mcp_server_name":"srv","mode":"form","url":"https://x","elicitation_id":"el1",` +
			`"message":"please confirm"}`,
			func(e *HookEvent) (any, any) {
				return []string{e.MCPServerName, e.Mode, e.URL, e.ElicitationID, e.Message},
					[]string{"srv", "form", "https://x", "el1", "please confirm"}
			}},
		{"ElicitationResult", `{"hook_event_name":"ElicitationResult","session_id":"s","cwd":"/r",` +
			`"mcp_server_name":"srv","action":"accept","mode":"form","elicitation_id":"el1",` +
			`"content":{"answer":"yes"}}`,
			func(e *HookEvent) (any, any) {
				return []string{e.MCPServerName, e.Action, e.Mode, e.ElicitationID, e.elicitationContentText()},
					[]string{"srv", "accept", "form", "el1", `{"answer":"yes"}`}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev, err := ParseHookEvent(strings.NewReader(c.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, want := c.want(ev)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("got %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestBatchToolUseIDs covers the reader method's contract: only tool_use_id,
// non-empty, capped at 64, nil on malformed input.
func TestBatchToolUseIDs(t *testing.T) {
	t.Run("nil ToolCalls", func(t *testing.T) {
		ev := &HookEvent{}
		if got := ev.batchToolUseIDs(); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("malformed array", func(t *testing.T) {
		ev := &HookEvent{ToolCalls: []byte(`not an array`)}
		if got := ev.batchToolUseIDs(); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("collects non-empty ids only", func(t *testing.T) {
		ev := &HookEvent{ToolCalls: []byte(`[{"tool_use_id":"a"},{"tool_use_id":""},{"tool_use_id":"b"}]`)}
		got := ev.batchToolUseIDs()
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Errorf("got %v, want [a b]", got)
		}
	})
	t.Run("does not decode tool_input or tool_response", func(t *testing.T) {
		// A malformed nested tool_input/tool_response must not break decoding of
		// tool_use_id: the struct this unmarshals into never names those keys.
		ev := &HookEvent{ToolCalls: []byte(`[{"tool_use_id":"a","tool_input":{"bad":` + "1" + `},"tool_response":123}]`)}
		got := ev.batchToolUseIDs()
		if len(got) != 1 || got[0] != "a" {
			t.Errorf("got %v, want [a]", got)
		}
	})
	t.Run("caps at 64", func(t *testing.T) {
		var sb strings.Builder
		sb.WriteByte('[')
		for i := 0; i < 100; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`{"tool_use_id":"id` + strconv.Itoa(i) + `"}`)
		}
		sb.WriteByte(']')
		ev := &HookEvent{ToolCalls: []byte(sb.String())}
		got := ev.batchToolUseIDs()
		if len(got) != 64 {
			t.Errorf("len = %d, want 64", len(got))
		}
	})
}

// TestElicitationContentText covers "" for empty/null, and compact JSON text
// otherwise; this method only serializes, redaction/capping/gating happen in
// the mapper.
func TestElicitationContentText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"nil", "", ""},
		{"null", "null", ""},
		{"whitespace null", "  null  ", ""},
		{"object", `{"answer":"yes","n":1}`, `{"answer":"yes","n":1}`},
		{"object with insignificant space", "{\n  \"a\": 1\n}", `{"a":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := &HookEvent{ElicitationContent: []byte(c.raw)}
			if got := ev.elicitationContentText(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
