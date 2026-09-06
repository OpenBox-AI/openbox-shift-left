package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func boolPtr(v bool) *bool          { return &v }
func float64Ptr(v float64) *float64 { return &v }

// signalCase is one of the 21 v1.8 observe-only lifecycle signals.
type signalCase struct {
	name     string
	hook     HookName
	ev       *HookEvent
	wantType client.EventType
	wantMeta map[string]any // subset: every key here must match exactly
}

func signalCases() []signalCase {
	return []signalCase{
		{
			name: "Setup", hook: HookSetup,
			ev:       &HookEvent{SessionID: "s", Trigger: "init"},
			wantType: client.EventSetup,
			wantMeta: map[string]any{"trigger": "init"},
		},
		{
			name: "InstructionsLoaded", hook: HookInstructionsLoaded,
			ev: &HookEvent{
				SessionID: "s", FilePath: "CLAUDE.md", MemoryType: "Project",
				LoadReason: "session_start", TriggerFilePath: "a/b", ParentFilePath: "a",
				Globs: []string{"**/*.go", "**/*.md"},
			},
			wantType: client.EventInstructionsLoaded,
			wantMeta: map[string]any{
				"file_path": "CLAUDE.md", "memory_type": "Project", "load_reason": "session_start",
				"trigger_file_path": "a/b", "parent_file_path": "a",
				"globs": []string{"**/*.go", "**/*.md"},
			},
		},
		{
			name: "UserPromptExpansion", hook: HookUserPromptExpansion,
			ev: &HookEvent{
				SessionID: "s", ExpansionType: "slash_command", CommandName: "commit",
				CommandSource: "not-in-any-table",
			},
			wantType: client.EventUserPromptExpansion,
			wantMeta: map[string]any{
				"expansion_type": "slash_command", "command_name": "commit",
				"command_source": "not-in-any-table",
			},
		},
		{
			name: "MessageDisplay", hook: HookMessageDisplay,
			ev: &HookEvent{
				SessionID: "s", TurnID: "t1", MessageID: "m1", Index: intPtr(2), Final: boolPtr(true),
			},
			wantType: client.EventMessageDisplay,
			wantMeta: map[string]any{"turn_id": "t1", "message_id": "m1", "index": 2, "final": true},
		},
		{
			name: "PermissionRequest", hook: HookPermissionRequest,
			ev: &HookEvent{
				SessionID: "s", ToolName: "Bash", PermissionMode: "default",
				ToolInput: json.RawMessage(`{"command":"ls -la"}`),
			},
			wantType: client.EventPermissionRequest,
			wantMeta: map[string]any{"tool_name": "Bash", "permission_mode": "default"},
		},
		{
			name: "PostToolBatch", hook: HookPostToolBatch,
			ev:       &HookEvent{SessionID: "s", ToolCalls: json.RawMessage(`[{"tool_use_id":"tu1"},{"tool_use_id":"tu2"}]`)},
			wantType: client.EventPostToolBatch,
			wantMeta: map[string]any{"batch_size": 2, "batch_tool_use_ids": []string{"tu1", "tu2"}},
		},
		{
			name: "Notification", hook: HookNotification,
			ev:       &HookEvent{SessionID: "s", NotificationType: "idle_prompt", Message: "please respond"},
			wantType: client.EventNotification,
			wantMeta: map[string]any{"notification_type": "idle_prompt"},
		},
		{
			name: "TaskCreated", hook: HookTaskCreated,
			ev: &HookEvent{
				SessionID: "s", TaskID: "tid1", TeammateName: "alice", TeamName: "core",
				TaskSubject: "investigate the flake",
			},
			wantType: client.EventTaskCreated,
			wantMeta: map[string]any{"task_id": "tid1", "teammate_name": "alice", "team_name": "core"},
		},
		{
			name: "TaskCompleted", hook: HookTaskCompleted,
			ev: &HookEvent{
				SessionID: "s", TaskID: "tid1", TeammateName: "alice", TeamName: "core",
				TaskSubject: "investigate the flake",
			},
			wantType: client.EventTaskCompleted,
			wantMeta: map[string]any{"task_id": "tid1", "teammate_name": "alice", "team_name": "core"},
		},
		{
			name: "TeammateIdle", hook: HookTeammateIdle,
			ev:       &HookEvent{SessionID: "s", TeammateName: "bob", TeamName: "core"},
			wantType: client.EventTeammateIdle,
			wantMeta: map[string]any{"teammate_name": "bob", "team_name": "core"},
		},
		{
			name: "ConfigChange", hook: HookConfigChange,
			ev:       &HookEvent{SessionID: "s", Source: "user_settings", FilePath: "settings.json"},
			wantType: client.EventConfigChange,
			wantMeta: map[string]any{"source": "user_settings", "file_path": "settings.json"},
		},
		{
			name: "CwdChanged", hook: HookCwdChanged,
			ev:       &HookEvent{SessionID: "s", OldCwd: "/a", NewCwd: "/b"},
			wantType: client.EventCwdChanged,
			wantMeta: map[string]any{"old_cwd": "/a", "new_cwd": "/b"},
		},
		{
			name: "DirectoryAdded", hook: HookDirectoryAdded,
			ev:       &HookEvent{SessionID: "s", Directory: "/repo2", Source: "slash_command"},
			wantType: client.EventDirectoryAdded,
			wantMeta: map[string]any{"directory": "/repo2", "source": "slash_command"},
		},
		{
			name: "FileChanged", hook: HookFileChanged,
			ev:       &HookEvent{SessionID: "s", FilePath: "CLAUDE.md", FileChangeEvent: "change"},
			wantType: client.EventFileChanged,
			wantMeta: map[string]any{"file_path": "CLAUDE.md", "event": "change"},
		},
		{
			name: "WorktreeRemove", hook: HookWorktreeRemove,
			ev:       &HookEvent{SessionID: "s", WorktreePath: "/wt1"},
			wantType: client.EventWorktreeRemove,
			wantMeta: map[string]any{"worktree_path": "/wt1"},
		},
		{
			name: "PreCompact", hook: HookPreCompact,
			ev:       &HookEvent{SessionID: "s", Trigger: "manual", CustomInstructions: "squash old turns"},
			wantType: client.EventPreCompact,
			wantMeta: map[string]any{"trigger": "manual"},
		},
		{
			name: "PostCompact", hook: HookPostCompact,
			ev:       &HookEvent{SessionID: "s", Trigger: "auto", CompactSummary: "compacted 40 turns"},
			wantType: client.EventPostCompact,
			wantMeta: map[string]any{"trigger": "auto"},
		},
		{
			name: "PreModelSwitch", hook: HookPreModelSwitch,
			ev: &HookEvent{
				SessionID: "s", FromModel: "claude-opus-4-8", ToModel: "claude-sonnet-4-8",
				RequestedModel: "claude-sonnet-4-8", Source: "command", CacheTTL: "5m", Pricing: "configured",
				ContextTokens: intPtr(1000), PromptCacheWarm: boolPtr(true), EstimatedCacheWriteUSD: float64Ptr(0.02),
			},
			wantType: client.EventPreModelSwitch,
			wantMeta: map[string]any{
				"from_model": "claude-opus-4-8", "to_model": "claude-sonnet-4-8",
				"requested_model": "claude-sonnet-4-8", "source": "command", "cache_ttl": "5m",
				"pricing": "configured", "context_tokens": 1000, "prompt_cache_warm": true,
				"estimated_cache_write_usd": 0.02,
			},
		},
		{
			name: "PostModelSwitch", hook: HookPostModelSwitch,
			ev: &HookEvent{
				SessionID: "s", FromModel: "claude-opus-4-8", ToModel: "claude-sonnet-4-8",
				Source: "auto", CacheTTL: "1h", Pricing: "catalog",
			},
			wantType: client.EventPostModelSwitch,
			wantMeta: map[string]any{
				"from_model": "claude-opus-4-8", "to_model": "claude-sonnet-4-8",
				"source": "auto", "cache_ttl": "1h", "pricing": "catalog",
			},
		},
		{
			name: "Elicitation", hook: HookElicitation,
			ev: &HookEvent{
				SessionID: "s", MCPServerName: "srv1", Mode: "form", ElicitationID: "el1",
				Message: "please fill out the form",
			},
			wantType: client.EventElicitation,
			wantMeta: map[string]any{"mcp_server_name": "srv1", "mode": "form", "elicitation_id": "el1"},
		},
		{
			name: "ElicitationResult", hook: HookElicitationResult,
			ev: &HookEvent{
				SessionID: "s", MCPServerName: "srv1", Action: "accept", Mode: "form", ElicitationID: "el1",
				ElicitationContent: json.RawMessage(`{"field":"value"}`),
			},
			wantType: client.EventElicitationResult,
			wantMeta: map[string]any{
				"mcp_server_name": "srv1", "action": "accept", "mode": "form", "elicitation_id": "el1",
			},
		},
	}
}

// TestMap_21SignalClasses is the single table-driven test over all 21 v1.8
// observe-only lifecycle signals: none becomes an Activity (insight 1), each
// rides signalEvent's fixed shape, and each carries its documented structural
// metadata.
func TestMap_21SignalClasses(t *testing.T) {
	m := testMapper()
	if got := len(signalCases()); got != 21 {
		t.Fatalf("signalCases() has %d entries, want 21", got)
	}
	for _, tc := range signalCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := m.Map(tc.hook, tc.ev)
			if !ok {
				t.Fatalf("Map(%s) ok=false, want an event", tc.hook)
			}
			if got.EventType != tc.wantType {
				t.Errorf("event_type = %q, want %q", got.EventType, tc.wantType)
			}
			wantTool := client.Tool{Name: agentToolName, Kind: client.ToolShell}
			if got.Tool != wantTool {
				t.Errorf("tool = %+v, want %+v", got.Tool, wantTool)
			}
			// Insight 1: nothing new becomes an Activity, no exception.
			if got.Span != nil {
				t.Errorf("span = %+v, want nil", got.Span)
			}
			if got.Status != "" {
				t.Errorf("status = %q, want empty", got.Status)
			}
			if got.TurnIndex != nil {
				t.Errorf("turn_index = %v, want nil", *got.TurnIndex)
			}
			if got.ActivityType != "" {
				t.Errorf("activity_type = %q, want empty", got.ActivityType)
			}
			if got.Tokens != nil {
				t.Errorf("tokens = %+v, want nil", got.Tokens)
			}
			if got.StartedAt != "" || got.EndedAt != "" {
				t.Errorf("started_at/ended_at = %q/%q, want both empty", got.StartedAt, got.EndedAt)
			}
			for k, want := range tc.wantMeta {
				gotV, present := got.Metadata[k]
				if !present {
					t.Errorf("metadata[%q] missing, want %v", k, want)
					continue
				}
				if diff := cmp.Diff(want, gotV); diff != "" {
					t.Errorf("metadata[%q] mismatch (-want +got):\n%s", k, diff)
				}
			}
		})
	}
}

// TestMap_ContentGatingForSignalClasses: with capture ON, the 8 classes that
// have a content line get it (gated through gatedSignalDetail, never a raw
// Content literal); MessageDisplay, PostToolBatch and UserPromptExpansion
// never get Content even with capture on (D1 / A-04 A1). With capture OFF,
// none of the 21 ever get Content.
func TestMap_ContentGatingForSignalClasses(t *testing.T) {
	off := testMapper()
	for _, tc := range signalCases() {
		got, ok := off.Map(tc.hook, tc.ev)
		if !ok {
			t.Fatalf("%s: Map ok=false", tc.name)
		}
		if got.Content != nil {
			t.Errorf("%s: capture off, content = %+v, want nil", tc.name, got.Content)
		}
	}

	on := testMapper()
	on.CaptureContent = true

	structuralOnly := map[HookName]bool{
		HookMessageDisplay:      true,
		HookPostToolBatch:       true,
		HookUserPromptExpansion: true,
	}
	contentBearing := map[HookName]string{ // hook -> expected SignalDetail text
		// toolInputExtract reuses the PreToolUse observe path's extractor: for
		// a shell tool that is the command string, not the raw tool_input JSON.
		HookPermissionRequest: "ls -la",
		HookNotification:      "please respond",
		HookTaskCreated:       "investigate the flake",
		HookTaskCompleted:     "investigate the flake",
		HookPreCompact:        "squash old turns",
		HookPostCompact:       "compacted 40 turns",
		HookElicitation:       "please fill out the form",
		HookElicitationResult: `{"field":"value"}`,
	}

	for _, tc := range signalCases() {
		got, ok := on.Map(tc.hook, tc.ev)
		if !ok {
			t.Fatalf("%s: Map ok=false", tc.name)
		}
		switch {
		case structuralOnly[tc.hook]:
			if got.Content != nil {
				t.Errorf("%s: structural-only class carries content with capture on: %+v", tc.name, got.Content)
			}
		case contentBearing[tc.hook] != "":
			want := contentBearing[tc.hook]
			if got.Content == nil || got.Content.SignalDetail != want {
				t.Errorf("%s: content = %+v, want SignalDetail %q", tc.name, got.Content, want)
			}
		default:
			if got.Content != nil {
				t.Errorf("%s: no content line specified, but content = %+v", tc.name, got.Content)
			}
		}
	}
}

// TestMap_PerHookEnumAllowlistsAreDistinct proves enumOr is applied per hook
// (insight 2): reusing another hook's allowlist would silently drop a value
// that hook's own table accepts, and would accept a value another hook's
// table rejects.
func TestMap_PerHookEnumAllowlistsAreDistinct(t *testing.T) {
	m := testMapper()

	// Setup accepts "maintenance"; PreCompact/PostCompact do not (their table
	// is {manual,auto}), proving setupTriggers != compactTriggers.
	setup, _ := m.Map(HookSetup, &HookEvent{SessionID: "s", Trigger: "maintenance"})
	if setup.Metadata["trigger"] != "maintenance" {
		t.Errorf("Setup trigger=maintenance dropped: %v", setup.Metadata)
	}
	pre, _ := m.Map(HookPreCompact, &HookEvent{SessionID: "s", Trigger: "maintenance"})
	if _, present := pre.Metadata["trigger"]; present {
		t.Errorf("PreCompact accepted Setup's trigger value: %v", pre.Metadata)
	}
	// PreCompact accepts "auto"; Setup does not.
	preAuto, _ := m.Map(HookPreCompact, &HookEvent{SessionID: "s", Trigger: "auto"})
	if preAuto.Metadata["trigger"] != "auto" {
		t.Errorf("PreCompact trigger=auto dropped: %v", preAuto.Metadata)
	}
	setupAuto, _ := m.Map(HookSetup, &HookEvent{SessionID: "s", Trigger: "auto"})
	if _, present := setupAuto.Metadata["trigger"]; present {
		t.Errorf("Setup accepted PreCompact's trigger value: %v", setupAuto.Metadata)
	}

	// DirectoryAdded's source table accepts register_repo_root; SessionStart's
	// sourceValues does not, proving directoryAddedSources is its own table.
	dir, _ := m.Map(HookDirectoryAdded, &HookEvent{SessionID: "s", Directory: "/x", Source: "register_repo_root"})
	if dir.Metadata["source"] != "register_repo_root" {
		t.Errorf("DirectoryAdded source dropped: %v", dir.Metadata)
	}
	ss, _ := m.Map(HookSessionStart, &HookEvent{SessionID: "s", Source: "register_repo_root"})
	if _, present := ss.Metadata["source"]; present {
		t.Errorf("SessionStart accepted DirectoryAdded's source value: %v", ss.Metadata)
	}

	// ConfigChange's source table accepts "skills"; SessionStart's does not.
	cc, _ := m.Map(HookConfigChange, &HookEvent{SessionID: "s", Source: "skills"})
	if cc.Metadata["source"] != "skills" {
		t.Errorf("ConfigChange source=skills dropped: %v", cc.Metadata)
	}

	// PostModelSwitch's source table accepts "auto"/"resume"; PreModelSwitch's
	// does not.
	post, _ := m.Map(HookPostModelSwitch, &HookEvent{SessionID: "s", Source: "resume"})
	if post.Metadata["source"] != "resume" {
		t.Errorf("PostModelSwitch source=resume dropped: %v", post.Metadata)
	}
	preMS, _ := m.Map(HookPreModelSwitch, &HookEvent{SessionID: "s", Source: "resume"})
	if _, present := preMS.Metadata["source"]; present {
		t.Errorf("PreModelSwitch accepted PostModelSwitch's source value: %v", preMS.Metadata)
	}
}

// TestMap_ConfigChangeInvalidSourceOmitsKey: an out-of-enum source must not
// egress the raw value; enumOr drops it and compact() deletes the key.
func TestMap_ConfigChangeInvalidSourceOmitsKey(t *testing.T) {
	m := testMapper()
	got, ok := m.Map(HookConfigChange, &HookEvent{SessionID: "s", Source: "evil-injected-source"})
	if !ok {
		t.Fatal("Map ok=false")
	}
	if v, present := got.Metadata["source"]; present {
		t.Errorf("out-of-enum source egressed: %v", v)
	}
}

// TestMap_CommandSourceIsFreeform: command_source binds through capStr, not
// enumOr (R1) — there is only one documented value and no confirmed table, so
// an unconfirmed allowlist must not silently discard real data.
func TestMap_CommandSourceIsFreeform(t *testing.T) {
	m := testMapper()
	got, ok := m.Map(HookUserPromptExpansion, &HookEvent{
		SessionID: "s", ExpansionType: "mcp_prompt", CommandSource: "some-brand-new-source",
	})
	if !ok {
		t.Fatal("Map ok=false")
	}
	if got.Metadata["command_source"] != "some-brand-new-source" {
		t.Errorf("command_source = %v, want verbatim pass-through (capStr, not enumOr)", got.Metadata["command_source"])
	}
}

// TestMap_PointerFieldsAbsentStayUnset: a nil pointer must never bind as a
// false "0" claim; the case must guard on nil, not rely on compact (compact
// only drops empty strings).
func TestMap_PointerFieldsAbsentStayUnset(t *testing.T) {
	m := testMapper()

	md, _ := m.Map(HookMessageDisplay, &HookEvent{SessionID: "s", TurnID: "t1"})
	if _, present := md.Metadata["index"]; present {
		t.Errorf("index present with nil pointer: %v", md.Metadata)
	}
	if _, present := md.Metadata["final"]; present {
		t.Errorf("final present with nil pointer: %v", md.Metadata)
	}

	sw, _ := m.Map(HookPreModelSwitch, &HookEvent{SessionID: "s", Source: "command"})
	for _, k := range []string{"context_tokens", "prompt_cache_warm", "estimated_cache_write_usd"} {
		if v, present := sw.Metadata[k]; present {
			t.Errorf("%s present with nil pointer: %v", k, v)
		}
	}
}

// TestMap_ModelSwitchNeverSetsEvModel: buildMetadata copies ev.Model into
// metadata.model, the key core aggregates token rollups under. A switch
// spends no tokens, so ev.Model must stay "" even if the shared Model field
// happens to be populated on the payload.
func TestMap_ModelSwitchNeverSetsEvModel(t *testing.T) {
	m := testMapper()
	ev := &HookEvent{SessionID: "s", Model: "claude-should-not-bind", FromModel: "a", ToModel: "b", Source: "command"}

	pre, ok := m.Map(HookPreModelSwitch, ev)
	if !ok {
		t.Fatal("Map ok=false")
	}
	if pre.Model != "" {
		t.Errorf("PreModelSwitch set ev.Model = %q, want empty", pre.Model)
	}

	post, ok := m.Map(HookPostModelSwitch, ev)
	if !ok {
		t.Fatal("Map ok=false")
	}
	if post.Model != "" {
		t.Errorf("PostModelSwitch set ev.Model = %q, want empty", post.Model)
	}
}

// TestMap_PromptIDThreadedOnEveryEvent: prompt_id is merged once in Map,
// regardless of hook, and does not change bytes for a payload that never
// carries one (compact() drops the empty string).
func TestMap_PromptIDThreadedOnEveryEvent(t *testing.T) {
	m := testMapper()

	withID, _ := m.Map(HookSetup, &HookEvent{SessionID: "s", Trigger: "init", PromptID: "req-1"})
	if withID.Metadata["prompt_id"] != "req-1" {
		t.Errorf("prompt_id not threaded onto Setup: %v", withID.Metadata)
	}

	withoutID, _ := m.Map(HookSetup, &HookEvent{SessionID: "s", Trigger: "init"})
	if _, present := withoutID.Metadata["prompt_id"]; present {
		t.Errorf("empty prompt_id must be dropped (byte-identity), got %v", withoutID.Metadata)
	}

	// Existing (pre-v1.8) class: SessionStart never carried prompt_id, and
	// must not start doing so unless the payload actually has one.
	ss, _ := m.Map(HookSessionStart, &HookEvent{SessionID: "s", Source: "startup"})
	if _, present := ss.Metadata["prompt_id"]; present {
		t.Errorf("SessionStart with no prompt_id must not carry the key: %v", ss.Metadata)
	}
	ssWithID, _ := m.Map(HookSessionStart, &HookEvent{SessionID: "s", Source: "startup", PromptID: "req-2"})
	if ssWithID.Metadata["prompt_id"] != "req-2" {
		t.Errorf("SessionStart with prompt_id must carry it: %v", ssWithID.Metadata)
	}
}

// TestMapTurn_PromptIDThreadedOnBothHalves: both MapTurn halves merge
// commonMetadata too.
func TestMapTurn_PromptIDThreadedOnBothHalves(t *testing.T) {
	m := testMapper()
	started, completed, ok := m.MapTurn(&HookEvent{SessionID: "s", PromptID: "req-3"}, turnWindow{HasUsage: true}, 0)
	if !ok {
		t.Fatal("MapTurn ok=false")
	}
	if started.Metadata["prompt_id"] != "req-3" {
		t.Errorf("started half missing prompt_id: %v", started.Metadata)
	}
	if completed.Metadata["prompt_id"] != "req-3" {
		t.Errorf("completed half missing prompt_id: %v", completed.Metadata)
	}
}

// TestMap_HookSessionEndAllReasonsSealTheRun: V3 — every SessionEnd,
// including reason=clear, terminates the run; there is no branch to test
// separately from this loop.
func TestMap_HookSessionEndAllReasonsSealTheRun(t *testing.T) {
	m := testMapper()
	for _, reason := range []string{"clear", "resume", "logout", "prompt_input_exit", "other", "", "totally-unrecognised"} {
		got, ok := m.Map(HookSessionEnd, &HookEvent{SessionID: "s", Reason: reason})
		if !ok {
			t.Fatalf("reason=%q: Map ok=false", reason)
		}
		if got.EventType != client.EventSessionEnded {
			t.Errorf("reason=%q: event_type = %q, want %q", reason, got.EventType, client.EventSessionEnded)
		}
		if got.EndedAt == "" {
			t.Errorf("reason=%q: ended_at empty, run not sealed", reason)
		}
	}
}

// TestSourceValues_GainsFork: phase 08 reads sourceValues to decide when a run
// continues (clear/resume bump; startup/compact/fork do not) — a missing
// value there is a wrong run identity, not a cosmetic metadata gap.
func TestSourceValues_GainsFork(t *testing.T) {
	if got := enumOr("fork", sourceValues); got != "fork" {
		t.Fatalf("enumOr(fork, sourceValues) = %q, want %q", got, "fork")
	}
	m := testMapper()
	got, ok := m.Map(HookSessionStart, &HookEvent{SessionID: "s", Source: "fork"})
	if !ok {
		t.Fatal("Map ok=false")
	}
	if got.Metadata["source"] != "fork" {
		t.Errorf("SessionStart(source=fork) metadata[source] = %v, want %q", got.Metadata["source"], "fork")
	}
}
