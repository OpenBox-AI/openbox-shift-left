package client

import (
	"bytes"
	"testing"
)

const (
	canaryPrompt       = "CANARY-PROMPT-8f21"
	canaryOutput       = "CANARY-OUTPUT-8f22"
	canaryFileText     = "CANARY-FILETEXT-8f23"
	canaryReqBody      = "CANARY-REQBODY-8f24"
	canaryRespBody     = "CANARY-RESPBODY-8f25"
	canaryCommand      = "CANARY-COMMAND-8f26"
	canaryToolOutput   = "CANARY-TOOLOUTPUT-8f27"
	canarySignalDetail = "CANARY-SIGNALDETAIL-8f28"
	canaryThinking     = "CANARY-THINKING-8f29"
)

// TestNoGatedContentEgressesWhenCaptureIsOff iNV-2 asserted against the whole
// payload rather than against the one field a test happens to look at.
func TestNoGatedContentEgressesWhenCaptureIsOff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		event    DevEvent
		canaries []string
	}{
		{
			name: "prompt signal",
			event: DevEvent{
				SchemaVersion: SchemaVersion, EventID: "ev-1", EventType: EventPromptSubmitted,
				SessionID: "sess-leak", DeveloperDID: "did:aip:x", Timestamp: "2026-07-31T09:00:00Z",
				Content: &Content{Prompt: canaryPrompt, Output: canaryOutput},
			},
			canaries: []string{canaryPrompt, canaryOutput},
		},
		{
			name: "file write call",
			event: DevEvent{
				SchemaVersion: SchemaVersion, EventID: "ev-2", EventType: EventToolCall,
				SessionID: "sess-leak", DeveloperDID: "did:aip:x", Timestamp: "2026-07-31T09:00:00Z",
				Tool:    Tool{Name: "Write", Kind: ToolFile},
				Content: &Content{FileText: canaryFileText},
				Span: &Span{
					SemanticType: "file_write", Stage: "started", FilePath: "a.go",
					RequestBody: canaryReqBody, ResponseBody: canaryRespBody,
				},
			},
			canaries: []string{canaryFileText, canaryReqBody, canaryRespBody},
		},
		{
			name: "tool result",
			event: DevEvent{
				SchemaVersion: SchemaVersion, EventID: "ev-3", EventType: EventToolResult,
				SessionID: "sess-leak", DeveloperDID: "did:aip:x", Timestamp: "2026-07-31T09:00:00Z",
				Tool:    Tool{Name: "Bash", Kind: ToolShell},
				Content: &Content{Output: canaryOutput},
				Span:    &Span{SemanticType: "shell_command", Stage: "completed", ResponseBody: canaryRespBody},
			},
			canaries: []string{canaryOutput, canaryRespBody},
		},
		{
			name: "turn carrying the assistant message",
			event: func() DevEvent {
				idx := 0
				return DevEvent{
					SchemaVersion: SchemaVersion, EventID: "ev-4", EventType: EventTurnCompleted,
					SessionID: "sess-leak", DeveloperDID: "did:aip:x", Timestamp: "2026-07-31T09:00:00Z",
					Tool:      Tool{Name: "claude-code", Kind: ToolShell},
					TurnIndex: &idx,
					Content:   &Content{Output: canaryOutput},
				}
			}(),
			canaries: []string{canaryOutput},
		},
		{
			name: "tool call carrying input, output and a signal detail",
			event: DevEvent{
				SchemaVersion: SchemaVersion, EventID: "ev-30", EventType: EventToolResult,
				SessionID: "sess-leak", DeveloperDID: "did:aip:x", Timestamp: "2026-07-31T09:00:00Z",
				Tool: Tool{Name: "Bash", Kind: ToolShell},
				Content: &Content{
					ToolInput:    canaryCommand,
					ToolOutput:   canaryToolOutput,
					SignalDetail: canarySignalDetail,
				},
			},
			canaries: []string{canaryCommand, canaryToolOutput, canarySignalDetail},
		},
		{
			name: "turn carrying thinking",
			event: func() DevEvent {
				idx := 0
				return DevEvent{
					SchemaVersion: SchemaVersion, EventID: "ev-31", EventType: EventTurnCompleted,
					SessionID: "sess-leak", DeveloperDID: "did:aip:x", Timestamp: "2026-07-31T09:00:00Z",
					Tool:      Tool{Name: "claude-code", Kind: ToolShell},
					TurnIndex: &idx,
					Content:   &Content{Thinking: canaryThinking},
				}
			}(),
			canaries: []string{canaryThinking},
		},
		{
			name: "content smuggled through metadata",
			event: DevEvent{
				SchemaVersion: SchemaVersion, EventID: "ev-32", EventType: EventToolCall,
				SessionID: "sess-leak", DeveloperDID: "did:aip:x", Timestamp: "2026-07-31T09:00:00Z",
				Tool: Tool{Name: "mcp__github__create_issue", Kind: ToolMCP},
				Metadata: map[string]any{
					"arguments": canaryCommand,
					"thinking":  canaryThinking,
					"command":   canaryCommand,
				},
			},
			canaries: []string{canaryCommand, canaryThinking},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildPayload(stripContent(tc.event))
			if err != nil {
				t.Fatalf("buildPayload: %v", err)
			}
			assertAbsent(t, got, tc.canaries...)
		})
	}
}

// TestShellCommandNeverEgressesEvenWithCaptureOn the narrow property this
// still guards: Span.Function is NOT an egress channel for a command.
func TestShellCommandNeverEgressesEvenWithCaptureOn(t *testing.T) {
	ev := DevEvent{
		SchemaVersion: SchemaVersion, EventID: "ev-4", EventType: EventToolCall,
		SessionID: "sess-leak", DeveloperDID: "did:aip:x", Timestamp: "2026-07-31T09:00:00Z",
		Tool: Tool{Name: "Bash", Kind: ToolShell},
		Span: &Span{SemanticType: "shell_command", Stage: "started", Function: canaryCommand},
	}
	got, err := buildPayload(ev)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	assertAbsent(t, got, canaryCommand)
}

// TestContentBearingMetadataIsGated metadata is content-gated too.
func TestContentBearingMetadataIsGated(t *testing.T) {
	for _, key := range []string{"message", "prompt", "output", "diff", "command", "stdout"} {
		t.Run(key, func(t *testing.T) {
			ev := DevEvent{
				SchemaVersion: SchemaVersion, EventID: "ev-5", EventType: EventCommitCreated,
				SessionID: "sess-leak", DeveloperDID: "did:aip:x", Timestamp: "2026-07-31T09:00:00Z",
				Metadata: map[string]any{key: canaryPrompt, "commit_sha": "abc123"},
			}
			got, err := buildPayload(stripContent(ev))
			if err != nil {
				t.Fatalf("buildPayload: %v", err)
			}
			assertAbsent(t, got, canaryPrompt)
			if !bytes.Contains(got, []byte("abc123")) {
				t.Errorf("structural metadata was dropped along with the content: %s", got)
			}
		})
	}
}

// TestContentBearingMetadataRidesWhenCaptureIsOn with content capture ON the
// org has opted in, so metadata passes through: the gate is a content-capture
// gate, not a blanket filter.
func TestContentBearingMetadataRidesWhenCaptureIsOn(t *testing.T) {
	ev := DevEvent{
		SchemaVersion: SchemaVersion, EventID: "ev-6", EventType: EventCommitCreated,
		SessionID: "sess-leak", DeveloperDID: "did:aip:x", Timestamp: "2026-07-31T09:00:00Z",
		Metadata: map[string]any{"message": canaryPrompt},
	}
	got, err := buildPayload(ev) // no stripContent: capture is on
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	if !bytes.Contains(got, []byte(canaryPrompt)) {
		t.Error("with content capture on, metadata must still pass through")
	}
}

func assertAbsent(t *testing.T, payload []byte, canaries ...string) {
	t.Helper()
	for _, c := range canaries {
		if bytes.Contains(payload, []byte(c)) {
			t.Errorf("gated content %q reached the wire.\npayload: %s", c, payload)
		}
	}
}
