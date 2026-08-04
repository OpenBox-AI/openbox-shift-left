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

// The bytes buildPayload returns are the bytes that get signed AND sent — the
// client never re-marshals. So the wire contract is not "these fields decode to
// these values", it is the exact byte sequence, key order included.
//
// Every other wire test in this package decodes first and asserts field by
// field, which structurally cannot catch a key rename that a test happens not
// to mention, an omitempty gained or lost, a value silently changing type, or
// map key ordering shifting. These fixtures catch all of it, and they exist so
// that the refactors extracting shared machinery out of this module can prove
// they moved no bytes.
//
// Fixtures are stored indented for reviewable diffs and compacted before the
// comparison; json.Compact only strips insignificant whitespace, so the check
// is byte-exact on everything that reaches the wire.
//
// Regenerate deliberately, never reflexively:  go test ./client -run Golden -update
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

// goldenCases pins one event per wire class. Values are fixed literals: every
// id on the wire is derived (sha256 of the session/pair key) rather than
// random, so the payload is a pure function of the event.
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

	// A signal carrying content: prompt text rides signal_args only when
	// content-capture left it in place (INV-2 gate runs before this point).
	promptSubmitted := base(EventPromptSubmitted, "ev-3")
	promptSubmitted.Content = &Content{Prompt: "refactor the spool"}

	// A signal carrying lineage: commit/deploy keys have no first-class core
	// column and ride the pass-through metadata blob.
	deploy := base(EventDeploy, "deploy-production-37ec0a3f")
	deploy.Tool = Tool{Name: "openbox-git-action", Kind: ToolShell}
	deploy.Metadata = map[string]any{
		"deploy_id":  "deploy-production-37ec0a3f",
		"commit_sha": "37ec0a3f1c9b2e0000000000000000000000abcd",
		"repo":       ws,
	}

	fileCall := base(EventToolCall, "ev-4")
	fileCall.Tool = Tool{Name: "Write", Kind: ToolFile}
	fileCall.Span = &Span{
		SemanticType: "file_write",
		Stage:        "started",
		FilePath:     "cli/cmd/openbox/main.go",
		FileOp:       "write",
		BytesWritten: &bytesWritten,
	}

	// The completed half of the same call: it must share the activity_id and
	// span_id of fileCall (that pairing is the whole point) and carry a real
	// end_time/duration_ns where the started half emits nulls.
	fileResult := fileCall
	fileResult.EventID = "ev-5"
	fileResult.EventType = EventToolResult
	fileResult.StartedAt = ts
	fileResult.EndedAt = "2026-07-31T09:00:02.5Z"
	fileResult.Timestamp = "2026-07-31T09:00:02.5Z"
	fileResult.Span = &Span{
		SemanticType: "file_write",
		Stage:        "completed",
		FilePath:     "cli/cmd/openbox/main.go",
		FileOp:       "write",
		BytesWritten: &bytesWritten,
	}

	// Shell: shell_command must stay present-but-null. The command is read for
	// the local enforce decision and never egresses (SL3-SEC-3).
	shellCall := base(EventToolCall, "ev-6")
	shellCall.Tool = Tool{Name: "Bash", Kind: ToolShell}
	shellCall.Span = &Span{SemanticType: "shell_command", Stage: "started"}

	mcpCall := base(EventToolCall, "ev-7")
	mcpCall.Tool = Tool{Name: "search_issues", Kind: ToolMCP, MCPServer: "github"}
	mcpCall.Span = &Span{
		SemanticType: "mcp_tool_call",
		Stage:        "started",
		MCPServer:    "github",
		Function:     "search_issues",
	}

	return []goldenCase{
		{"lifecycle_session_started", sessionStarted},
		{"lifecycle_session_ended", sessionEnded},
		{"signal_prompt_submitted", promptSubmitted},
		{"signal_deploy_lineage", deploy},
		{"hook_file_started", fileCall},
		{"hook_file_completed", fileResult},
		{"hook_shell_started", shellCall},
		{"hook_mcp_started", mcpCall},
	}
}
