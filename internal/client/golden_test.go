package client

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden wire fixtures from current output")

// TestGoldenWirePayloads the bytes buildPayload returns are the bytes that get
// signed AND sent; the client never re-marshals.
func TestGoldenWirePayloads(t *testing.T) {
	for _, tc := range goldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildPayload(tc.event)
			if err != nil {
				t.Fatalf("buildPayload: %v", err)
			}
			path := filepath.Join("testdata", "golden", tc.name+".json")

			if *updateGolden {
				var pretty bytes.Buffer
				if err := json.Indent(&pretty, got, "", "  "); err != nil {
					t.Fatalf("indent: %v", err)
				}
				pretty.WriteByte('\n')
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, want); err != nil {
				t.Fatalf("golden file is not valid JSON: %v", err)
			}
			if !bytes.Equal(compact.Bytes(), got) {
				t.Errorf("wire bytes changed.\n golden: %s\n actual: %s\n\n"+
					"If the change is intended, re-run with -update and review the diff as a wire-contract change.",
					compact.String(), got)
			}
		})
	}
}

type goldenCase struct {
	name  string
	event DevEvent
}

func goldenCases() []goldenCase {
	const (
		session = "sess-golden-0001"
		did     = "did:aip:7f3c9b2e-0000-5000-a000-000000000001"
		ws      = "openbox-ai/openbox-shift-left"
		ts      = "2026-07-31T09:00:00Z"
	)
	base := func(t EventType, id string) DevEvent {
		return DevEvent{
			SchemaVersion: SchemaVersion,
			EventID:       id,
			EventType:     t,
			SessionID:     session,
			DeveloperDID:  did,
			WorkspaceID:   ws,
			Timestamp:     ts,
			Metadata:      map[string]any{"provider": "claude-code"},
		}
	}
	bytesWritten := 128

	sessionStarted := base(EventSessionStarted, "ev-1")

	sessionEnded := base(EventSessionEnded, "ev-2")
	sessionEnded.Timestamp = "2026-07-31T09:30:00Z"

	promptSubmitted := base(EventPromptSubmitted, "ev-3")
	promptSubmitted.Content = &Content{Prompt: "refactor the spool"}

	deploy := base(EventDeploy, "deploy-production-37ec0a3f")
	deploy.Tool = Tool{Name: "openbox-git-action", Kind: ToolShell}
	deploy.Metadata = map[string]any{
		"deploy_id":  "deploy-production-37ec0a3f",
		"commit_sha": "37ec0a3f1c9b2e0000000000000000000000abcd",
		"repo":       ws,
	}

	completed := func(call DevEvent, eventID, endedAt string) DevEvent {
		res := call
		res.EventID = eventID
		res.EventType = EventToolResult
		res.StartedAt = call.Timestamp
		res.EndedAt = endedAt
		res.Timestamp = endedAt
		span := *call.Span
		span.Stage = "completed"
		res.Span = &span
		res.Status = StatusCompleted
		return res
	}

	fileCall := base(EventToolCall, "ev-4")
	fileCall.Tool = Tool{Name: "Write", Kind: ToolFile}
	fileCall.Span = &Span{
		SemanticType: "file_write",
		Stage:        "started",
		// It does not track the real tree and must not be "corrected" when a
		// directory moves; a rename sweep already did that once and broke the byte
		// pin, which is core's dedupe key.
		FilePath:     "cli/cmd/openbox/main.go",
		FileOp:       "write",
		BytesWritten: &bytesWritten,
	}
	fileResult := completed(fileCall, "ev-5", "2026-07-31T09:00:02.5Z")

	shellCall := base(EventToolCall, "ev-6")
	shellCall.Tool = Tool{Name: "Bash", Kind: ToolShell}
	shellCall.Span = &Span{SemanticType: "shell_command", Stage: "started"}
	shellResult := completed(shellCall, "ev-8", "2026-07-31T09:00:01Z")

	// Everything but `status` is identical to the successful one, which is the
	// point; nothing else on the wire distinguishes a failed call, so the enum is
	// load-bearing rather than decorative.
	shellFailedCall := base(EventToolCall, "ev-15")
	shellFailedCall.Tool = Tool{Name: "Bash", Kind: ToolShell}
	shellFailedCall.Span = &Span{SemanticType: "shell_command", Stage: "started", InvocationID: "toolu_fail01"}
	shellFailed := completed(shellFailedCall, "ev-16", "2026-07-31T09:00:03.25Z")
	shellFailed.Status = StatusFailed
	shellFailed.Metadata = map[string]any{"provider": "claude-code", "is_interrupt": false}

	mcpCall := base(EventToolCall, "ev-7")
	mcpCall.Tool = Tool{Name: "search_issues", Kind: ToolMCP, MCPServer: "github"}
	mcpCall.Span = &Span{
		SemanticType: "mcp_tool_call",
		Stage:        "started",
		MCPServer:    "github",
		Function:     "search_issues",
	}
	mcpResult := completed(mcpCall, "ev-9", "2026-07-31T09:00:00.75Z")

	// Core reads a SignalReceived carrying signal_args as a new user goal and
	// overwrites the alignment session's goal with it (age.go:112-137), so a
	// well-meaning "let's show the denied tool in the Verify tab's Input" would
	// silently destroy goal alignment.
	subagentStarted := base(EventSubagentStarted, "ev-17")
	subagentStarted.Tool = Tool{Name: "claude-code", Kind: ToolShell}
	subagentStarted.AgentID = "agt-code-reviewer-01"
	subagentStarted.Metadata = map[string]any{
		"provider":   "claude-code",
		"agent_id":   "agt-code-reviewer-01",
		"agent_type": "code-reviewer",
	}

	permissionDenied := base(EventPermissionDenied, "ev-18")
	permissionDenied.Tool = Tool{Name: "Bash", Kind: ToolShell}
	permissionDenied.Span = &Span{SemanticType: "internal", Stage: "completed", InvocationID: "toolu_denied01"}
	permissionDenied.Metadata = map[string]any{
		"provider":    "claude-code",
		"tool_use_id": "toolu_denied01",
	}

	apiError := base(EventAPIError, "ev-19")
	apiError.Tool = Tool{Name: "claude-code", Kind: ToolShell}
	apiError.Metadata = map[string]any{"provider": "claude-code", "error_type": "rate_limit"}

	turnIndex := 0
	turnStarted := base(EventTurnStarted, "ev-10")
	turnStarted.Tool = Tool{Name: "claude-code", Kind: ToolShell}
	turnStarted.TurnIndex = &turnIndex
	turnStarted.Metadata = map[string]any{"provider": "claude-code", "turn_index": turnIndex}

	turnCompleted := base(EventTurnCompleted, "ev-11")
	turnCompleted.Tool = Tool{Name: "claude-code", Kind: ToolShell}
	turnCompleted.TurnIndex = &turnIndex
	turnCompleted.Timestamp = "2026-07-31T09:00:12Z"
	turnCompleted.StartedAt = ts
	turnCompleted.EndedAt = "2026-07-31T09:00:12Z"
	turnCompleted.Model = "claude-opus-4-8"
	turnCompleted.Tokens = &Tokens{
		Input:              intPtrGolden(1204),
		Output:             intPtrGolden(318),
		CacheCreationInput: intPtrGolden(4096),
		CacheRead:          intPtrGolden(58210),
		Total:              intPtrGolden(63828),
	}
	turnCompleted.Metadata = map[string]any{"provider": "claude-code", "turn_index": turnIndex}

	// Every one of those fails silently if it drifts; core logs and returns "";
	// so the fixture is the alarm. It carries both content fields a turn can
	// hold, deliberately: the reply on the span and the thinking in
	// activity_output.
	turnWithContent := turnCompleted
	turnWithContent.EventID = "ev-20"
	turnWithContent.Content = &Content{
		Output:   "I refactored the spool; all 11 modules are green.",
		Thinking: "The spool writer is called from two hooks, so the lock has to be held across the rename.",
	}

	subIndex := 1
	subagentTurn := turnCompleted
	subagentTurn.EventID = "ev-12"
	subagentTurn.TurnIndex = &subIndex
	subagentTurn.AgentID = "agt-code-reviewer-01"
	subagentTurn.Model = "claude-sonnet-4-8"
	subagentTurn.Tokens = &Tokens{
		Input:     intPtrGolden(88),
		Output:    intPtrGolden(1902),
		CacheRead: intPtrGolden(12000),
		Total:     intPtrGolden(13990),
	}
	subagentTurn.Metadata = map[string]any{
		"provider":   "claude-code",
		"turn_index": subIndex,
		"agent_type": "code-reviewer",
	}

	// The two adapters are separate modules and cannot import each other, so the
	// client's fixtures are the only place the two shapes can be compared byte
	// for byte.
	rollupStarted := base(EventTurnStarted, "ev-13")
	rollupStarted.Tool = Tool{Name: "codex", Kind: ToolShell}
	rollupStarted.SessionRollup = true
	rollupStarted.Metadata = map[string]any{"provider": "codex", "usage_scope": "session"}

	rollupCompleted := base(EventTurnCompleted, "ev-14")
	rollupCompleted.Tool = Tool{Name: "codex", Kind: ToolShell}
	rollupCompleted.SessionRollup = true
	rollupCompleted.Timestamp = "2026-07-31T09:30:00Z"
	rollupCompleted.EndedAt = "2026-07-31T09:30:00Z"
	rollupCompleted.Model = "gpt-5.6-sol"
	rollupCompleted.Tokens = &Tokens{
		Input:     intPtrGolden(4734),
		Output:    intPtrGolden(232),
		CacheRead: intPtrGolden(9984),
		Total:     intPtrGolden(14950),
	}
	rollupCompleted.Metadata = map[string]any{"provider": "codex", "usage_scope": "session"}

	return []goldenCase{
		{"lifecycle_session_started", sessionStarted},
		{"lifecycle_session_ended", sessionEnded},
		{"signal_prompt_submitted", promptSubmitted},
		{"signal_deploy_lineage", deploy},
		{"activity_file_started", fileCall},
		{"activity_file_completed", fileResult},
		{"activity_shell_started", shellCall},
		{"activity_shell_completed", shellResult},
		{"activity_tool_failed", shellFailed},
		{"signal_subagent_started", subagentStarted},
		{"signal_permission_denied", permissionDenied},
		{"signal_api_error", apiError},
		{"activity_mcp_started", mcpCall},
		{"activity_mcp_completed", mcpResult},
		{"activity_turn_started", turnStarted},
		{"activity_turn_completed", turnCompleted},
		{"activity_turn_completed_content", turnWithContent},
		{"activity_turn_subagent_completed", subagentTurn},
		{"activity_usage_rollup_started", rollupStarted},
		{"activity_usage_rollup_completed", rollupCompleted},
	}
}

func intPtrGolden(v int) *int { return &v }
