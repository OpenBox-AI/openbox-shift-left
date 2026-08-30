package client

import (
	"bytes"
	"testing"
)

// Canaries are distinctive enough that a substring hit in the payload can only
// mean the field reached the wire.
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

// INV-2 asserted against the whole payload rather than against the one field a
// test happens to look at. A field-scoped assertion proves only that content is
// absent from where the test expected it; scanning the emitted bytes proves it
// is absent from the wire, which is the actual invariant.
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
			// ADR-0018's new content class. The turn span is the one place the
			// client puts assistant text on the wire, so it is the one place a
			// gate regression would put it there unconditionally — and unlike the
			// cases above, this one is NEW, so no prior test covers it.
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
			// The classes added after this table was last extended: ToolInput and
			// ToolOutput (v1.3), SignalDetail (v1.3) and Thinking (v1.4). Their
			// capture-off behaviour was asserted only by the conformance cases;
			// nothing pinned it at the ONE choke point every event crosses, which
			// is where a gate regression would land.
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
			// The METADATA backstop, which is a separate mechanism from Content:
			// stripContent nils Content and the span, while buildMetadata drops
			// only the keys contentMetadataKeys names. `arguments` (the MCP class's
			// own key, contentKeyFor(ToolMCP)) and `thinking` were missing from
			// that list, so an adapter setting either directly routed around the
			// gate entirely.
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
			// stripContent is what Emit applies when content-capture is off.
			got, err := buildPayload(stripContent(tc.event))
			if err != nil {
				t.Fatalf("buildPayload: %v", err)
			}
			assertAbsent(t, got, tc.canaries...)
		})
	}
}

// The narrow property this still guards: Span.Function is NOT an egress channel
// for a command. The serializer reads Function only for an MCP tool's mcp_tool
// name (structuralActivityInput), so an adapter stuffing a shell command in there
// must not get it onto the wire — capture on or off. That is an input-side guard
// and it is unaffected by the content gate.
//
// It used to be framed as SL3-SEC-3: "a shell command must never egress,
// content-capture on or off", holding structurally because no field carried a
// command on the observe path. ADR-0019 P1 retired that — Content.ToolInput now
// rides the observe ToolCall under the gate, and the command DOES egress with
// capture on. This test still passes because its canary sits on Span.Function
// rather than on Content, so re-read the name as being about the FIELD, not
// about commands in general.
func TestShellCommandNeverEgressesEvenWithCaptureOn(t *testing.T) {
	ev := DevEvent{
		SchemaVersion: SchemaVersion, EventID: "ev-4", EventType: EventToolCall,
		SessionID: "sess-leak", DeveloperDID: "did:aip:x", Timestamp: "2026-07-31T09:00:00Z",
		Tool: Tool{Name: "Bash", Kind: ToolShell},
		Span: &Span{SemanticType: "shell_command", Stage: "started", Function: canaryCommand},
	}
	// No stripContent: this is the content-capture-ON path.
	got, err := buildPayload(ev)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	assertAbsent(t, got, canaryCommand)
}

// Metadata is content-gated too. It was not: stripContent nulled Content and
// the span bodies while buildMetadata copied every adapter-supplied key through
// untouched, so INV-2 rested on every adapter never putting content there
// rather than on the one choke point every event passes through.
//
// This replaces the characterization test that documented the gap.
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
			// The structural sibling must survive — the gate drops content, not
			// the lineage identifiers a governance record needs.
			if !bytes.Contains(got, []byte("abc123")) {
				t.Errorf("structural metadata was dropped along with the content: %s", got)
			}
		})
	}
}

// With content capture ON the org has opted in, so metadata passes through: the
// gate is a content-capture gate, not a blanket filter.
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
