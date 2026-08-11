package client

import (
	"bytes"
	"testing"
)

// Canaries are distinctive enough that a substring hit in the payload can only
// mean the field reached the wire.
const (
	canaryPrompt   = "CANARY-PROMPT-8f21"
	canaryOutput   = "CANARY-OUTPUT-8f22"
	canaryFileText = "CANARY-FILETEXT-8f23"
	canaryReqBody  = "CANARY-REQBODY-8f24"
	canaryRespBody = "CANARY-RESPBODY-8f25"
	canaryCommand  = "CANARY-COMMAND-8f26"
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

// SL3-SEC-3 is stronger than the content gate: a shell command is read for the
// local enforce decision and must never egress, content-capture on or off. It
// holds structurally today because the serializer has no field that carries a
// command on the observe path — activity_input takes structural locators only,
// and the shell fixtures pin that. So this guards the property from the input
// side: an adapter stuffing the command into a Span field must not get it onto
// the wire by turning capture on.
//
// The one deliberate exception is the Tier-2 escalation's Content.ToolInput,
// which is not the observe path and is content-gated — see
// structuralActivityInput.
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
