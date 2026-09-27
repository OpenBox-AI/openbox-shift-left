package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
)

// TestEmittedEventsAreConformant is the cross-contract acceptance check: every
// event the Claude Code adapter produces must validate against the dev-event
// schema with content-capture disabled (the default).
func TestEmittedEventsAreConformant(t *testing.T) {
	m := testMapper()

	cases := []struct {
		name string
		hook HookName
		ev   *HookEvent
	}{
		{"SessionStart", HookSessionStart, &HookEvent{SessionID: "s1", Cwd: "/repo", Source: "startup", Model: "claude-opus-4-8"}},
		{"UserPromptSubmit", HookUserPromptSubmit, &HookEvent{SessionID: "s1", PermissionMode: "default"}},
		{"PreToolUse/file", HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "Edit", ToolInput: json.RawMessage(`{"file_path":"a.go"}`)}},
		{"PreToolUse/bash", HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`)}},
		{"PreToolUse/mcp", HookPreToolUse, &HookEvent{SessionID: "s1", ToolName: "mcp__github__create_issue"}},
		{"PostToolUse/file", HookPostToolUse, &HookEvent{SessionID: "s1", ToolName: "Read", ToolInput: json.RawMessage(`{"file_path":"a.go"}`)}},
		{"SessionEnd", HookSessionEnd, &HookEvent{SessionID: "s1", Reason: "logout"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := m.Map(tc.hook, tc.ev)
			if !ok {
				t.Fatalf("Map ok=false")
			}
			raw := mustMarshalContractShape(t, ev)
			if err := conformance.ValidateDevEvent(raw, false); err != nil {
				t.Fatalf("emitted event is not dev-event schema conformant:\n%s\nerror: %v", raw, err)
			}
		})
	}
}

func mustMarshalContractShape(t *testing.T, ev client.DevEvent) []byte {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// TestNewSignalClassesAreConformant is the extension of
// TestEmittedEventsAreConformant to the 21 v1.8 observe-only lifecycle
// signals (conformance_parity_test.go's anchor drifted; this is the file
// that actually validates against the schema): all 21 classes must validate
// with content capture off AND on, at generation 0 AND at generation >= 1 --
// four dimensions, none of which the pre-1.8 cases above exercised, since
// run-identity and the content-on path did not exist when they were written.
func TestNewSignalClassesAreConformant(t *testing.T) {
	off := testMapper()
	on := testMapper()
	on.CaptureContent = true

	cases := signalCases()
	if len(cases) != 21 {
		t.Fatalf("signalCases() has %d entries, want 21", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("capture off, generation 0", func(t *testing.T) {
				ev, ok := off.Map(tc.hook, tc.ev)
				if !ok {
					t.Fatal("Map ok=false")
				}
				raw := mustMarshalContractShape(t, ev)
				if err := conformance.ValidateDevEvent(raw, false); err != nil {
					t.Fatalf("not dev-event schema conformant:\n%s\nerror: %v", raw, err)
				}
			})
			t.Run("capture on, generation 0", func(t *testing.T) {
				ev, ok := on.Map(tc.hook, tc.ev)
				if !ok {
					t.Fatal("Map ok=false")
				}
				raw := mustMarshalContractShape(t, ev)
				if err := conformance.ValidateDevEvent(raw, true); err != nil {
					t.Fatalf("not dev-event schema conformant:\n%s\nerror: %v", raw, err)
				}
			})
			t.Run("capture off, generation >= 1", func(t *testing.T) {
				ev, ok := off.Map(tc.hook, tc.ev)
				if !ok {
					t.Fatal("Map ok=false")
				}
				ev.RunID = "018f1a2b-0000-7000-8000-000000000001"
				ev.RunGeneration = 2
				ev.ContinuedFromRunID = "sess-0-prior-run-id"
				raw := mustMarshalContractShape(t, ev)
				if err := conformance.ValidateDevEvent(raw, false); err != nil {
					t.Fatalf("not dev-event schema conformant at generation >= 1:\n%s\nerror: %v", raw, err)
				}
			})
			t.Run("capture on, generation >= 1", func(t *testing.T) {
				ev, ok := on.Map(tc.hook, tc.ev)
				if !ok {
					t.Fatal("Map ok=false")
				}
				ev.RunID = "018f1a2b-0000-7000-8000-000000000001"
				ev.RunGeneration = 2
				ev.ContinuedFromRunID = "sess-0-prior-run-id"
				raw := mustMarshalContractShape(t, ev)
				if err := conformance.ValidateDevEvent(raw, true); err != nil {
					t.Fatalf("not dev-event schema conformant at generation >= 1:\n%s\nerror: %v", raw, err)
				}
			})
		})
	}
}
