package client

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/conformance"
)

// This file is the scoped coverage for the v1.8 contract bump: 21 new
// lifecycle classes riding stock SignalReceived, and the three additive
// run-identity fields declared here. The cross-cutting vocabulary suite
// (signalvocabulary_test.go, schema_guard_test.go, acceptancetest) lives
// elsewhere; this file does not duplicate it.

func TestSchemaVersionIsV110(t *testing.T) {
	if SchemaVersion != "1.10" {
		t.Errorf("SchemaVersion = %q, want %q", SchemaVersion, "1.10")
	}
}

// newLifecycleSignals is the 21 new classes, each
// riding stock SignalReceived with its snake_case signal_name. Order matches
// the constant declaration order.
var newLifecycleSignals = []struct {
	et   EventType
	name string
}{
	{EventSetup, "setup"},
	{EventInstructionsLoaded, "instructions_loaded"},
	{EventUserPromptExpansion, "user_prompt_expansion"},
	{EventMessageDisplay, "message_display"},
	{EventPermissionRequest, "permission_request"},
	{EventPostToolBatch, "post_tool_batch"},
	{EventNotification, "notification"},
	{EventTaskCreated, "task_created"},
	{EventTaskCompleted, "task_completed"},
	{EventTeammateIdle, "teammate_idle"},
	{EventConfigChange, "config_change"},
	{EventCwdChanged, "cwd_changed"},
	{EventDirectoryAdded, "directory_added"},
	{EventFileChanged, "file_changed"},
	{EventWorktreeRemove, "worktree_remove"},
	{EventPreCompact, "pre_compact"},
	{EventPostCompact, "post_compact"},
	{EventPreModelSwitch, "pre_model_switch"},
	{EventPostModelSwitch, "post_model_switch"},
	{EventElicitation, "elicitation"},
	{EventElicitationResult, "elicitation_result"},
}

// TestNewLifecycleClassesRideSignalReceived is the wire-mapping requirement:
// all 21 return (SignalReceived, unique non-empty name, nil error).
func TestNewLifecycleClassesRideSignalReceived(t *testing.T) {
	if len(newLifecycleSignals) != 21 {
		t.Fatalf("test table has %d rows, want 21", len(newLifecycleSignals))
	}
	seen := map[string]EventType{}
	for _, tc := range newLifecycleSignals {
		wire, name, err := wireTypeFor(tc.et)
		if err != nil {
			t.Errorf("wireTypeFor(%s): %v", tc.et, err)
			continue
		}
		if wire != wireSignalReceived {
			t.Errorf("wireTypeFor(%s) = %q, want %q", tc.et, wire, wireSignalReceived)
		}
		if name != tc.name {
			t.Errorf("wireTypeFor(%s) signal_name = %q, want %q", tc.et, name, tc.name)
		}
		if prior, dup := seen[name]; dup {
			t.Errorf("signal_name %q claimed by both %s and %s", name, prior, tc.et)
		}
		seen[name] = tc.et
	}
}

// TestNewClassesAreInAllEventTypes: len == 35 (33 plus the two model-call gate
// halves) and every new constant is present exactly once.
func TestNewClassesAreInAllEventTypes(t *testing.T) {
	if len(AllEventTypes) != 35 {
		t.Fatalf("len(AllEventTypes) = %d, want 35", len(AllEventTypes))
	}
	counts := map[EventType]int{}
	for _, et := range AllEventTypes {
		counts[et]++
	}
	for _, tc := range newLifecycleSignals {
		if counts[tc.et] != 1 {
			t.Errorf("AllEventTypes contains %s %d times, want exactly 1", tc.et, counts[tc.et])
		}
	}
}

// TestNoSessionSuspended: there is no session_suspended class, so neither the constant nor the
// wire string may exist anywhere the client declares its vocabulary.
func TestNoSessionSuspended(t *testing.T) {
	for _, et := range AllEventTypes {
		if strings.Contains(string(et), "SessionSuspended") {
			t.Errorf("AllEventTypes contains %s; SessionSuspended does not exist", et)
		}
		if _, name, err := wireTypeFor(et); err == nil && strings.Contains(name, "session_suspended") {
			t.Errorf("%s maps to signal_name %q; session_suspended does not exist", et, name)
		}
	}
}

// TestNewClassesProjectSignalArgs is the enforcement-surface requirement, and
// it is the exact inverse of what v1.8 asserted here. metadata has no reader in
// any governance engine: OPA matches signal_name + signal_args, Guardrails read
// signal_args and nothing else. So every one of the 21 projects its structural
// keys, and its one gated content key, into signal_args.
//
// What made the old assertion necessary was core reading ANY non-empty
// signal_args as a new user goal. Core's source-and-name gate now suppresses
// that for a developer-runtime signal whose name is not prompt_submitted, which
// is why this inverts. A build carrying this projection must not reach a
// developer before that gate is running.
func TestNewClassesProjectSignalArgs(t *testing.T) {
	for _, tc := range newLifecycleSignals {
		ev := DevEvent{
			EventID: "ev-1", EventType: tc.et, SessionID: "s", DeveloperDID: "did:aip:x",
			Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
			Content:  &Content{SignalDetail: "some free text"},
			Metadata: map[string]any{"commit_sha": "abc", "repo": "r", "deploy_id": "d", "environment": "prod"},
		}
		args := signalArgs(t, ev)
		if args == nil {
			t.Errorf("%s: signal_args absent; the class is invisible to every policy engine", tc.et)
			continue
		}
		for k, want := range map[string]string{
			"commit_sha": "abc", "repo": "r", "deploy_id": "d", "environment": "prod",
		} {
			if args[k] != want {
				t.Errorf("%s: signal_args[%q] = %v, want %q", tc.et, k, args[k], want)
			}
		}
		// The content key only exists for the classes signalDetailKeyFor names.
		if k := signalDetailKeyFor(tc.et); k != "" && args[k] != "some free text" {
			t.Errorf("%s: signal_args[%q] = %v, want the free text", tc.et, k, args[k])
		}
	}
}

// newContentKeyCases is the 8 signalDetailKeyFor cases / 7 distinct keys.
var newContentKeyCases = []struct {
	et  EventType
	key string
}{
	{EventPermissionRequest, "requested_tool_input"},
	{EventNotification, "notification_message"},
	{EventTaskCreated, "task_subject"},
	{EventTaskCompleted, "task_subject"},
	{EventPreCompact, "compact_instructions"},
	{EventPostCompact, "compact_summary"},
	{EventElicitation, "elicitation_message"},
	{EventElicitationResult, "elicitation_response"},
}

func TestNewSignalDetailKeyForCases(t *testing.T) {
	if len(newContentKeyCases) != 8 {
		t.Fatalf("test table has %d rows, want 8", len(newContentKeyCases))
	}
	distinct := map[string]bool{}
	for _, tc := range newContentKeyCases {
		if got := signalDetailKeyFor(tc.et); got != tc.key {
			t.Errorf("signalDetailKeyFor(%s) = %q, want %q", tc.et, got, tc.key)
		}
		distinct[tc.key] = true
	}
	if len(distinct) != 7 {
		t.Errorf("newContentKeyCases has %d distinct keys, want 7", len(distinct))
	}
	// EventUserPromptExpansion is structural-only: no signalDetailKeyFor case.
	if got := signalDetailKeyFor(EventUserPromptExpansion); got != "" {
		t.Errorf("signalDetailKeyFor(EventUserPromptExpansion) = %q, want empty (structural-only)", got)
	}
}

// TestNewContentKeysAreGated is the INV-2 backstop: every key
// signalDetailKeyFor can return for a new class must be in
// contentMetadataKeys, and must actually be dropped when capture is off.
func TestNewContentKeysAreGated(t *testing.T) {
	for _, tc := range newContentKeyCases {
		if !contentMetadataKeys[tc.key] {
			t.Errorf("contentMetadataKeys is missing %q (used by %s)", tc.key, tc.et)
		}
		ev := DevEvent{
			EventID: "ev-1", EventType: tc.et, SessionID: "s", DeveloperDID: "did:aip:x",
			Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
			Content: &Content{SignalDetail: "the free text"},
		}
		on := rawMeta(t, decodePayload(t, ev))
		if on[tc.key] != "the free text" {
			t.Errorf("%s: metadata[%q] = %v with capture on, want %q", tc.et, tc.key, on[tc.key], "the free text")
		}
		off := rawMeta(t, decodePayload(t, stripContent(ev)))
		if _, present := off[tc.key]; present {
			t.Errorf("%s: metadata[%q] present with capture off: %v", tc.et, tc.key, off)
		}
	}
}

// TestRunIdentityFieldsOmitEmptyAtGeneration0 is the byte-identity guarantee
// run continuation depends on: a DevEvent that never continued marshals with none of
// the three run-identity keys present at all.
func TestRunIdentityFieldsOmitEmptyAtGeneration0(t *testing.T) {
	ev := DevEvent{
		EventID: "ev-1", EventType: EventSessionStarted, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"run_id", "run_generation", "continued_from_run_id"} {
		if v, present := m[k]; present {
			t.Errorf("DevEvent JSON carries %q = %v at generation 0, want omitted", k, v)
		}
	}
}

// TestGovernancePayloadCarriesRunGenerationAndContinuedFromRunID: both fields
// reach the wire payload, both omitempty, and RunID falls back to ev.SessionID
// because this event sets no RunID of its own.
func TestGovernancePayloadCarriesRunGenerationAndContinuedFromRunID(t *testing.T) {
	ev := DevEvent{
		EventID: "ev-1", EventType: EventSessionStarted, SessionID: "sess-1", DeveloperDID: "did:aip:x",
		Timestamp: "2026-07-08T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
		RunGeneration:      2,
		ContinuedFromRunID: "sess-0-prior-run-id",
	}
	p := decodePayload(t, ev)
	if p.RunID != "sess-1" {
		t.Errorf("RunID = %q, want ev.SessionID %q (no RunID set)", p.RunID, "sess-1")
	}
	if p.RunGeneration != 2 {
		t.Errorf("run_generation = %d, want 2", p.RunGeneration)
	}
	if p.ContinuedFromRunID != "sess-0-prior-run-id" {
		t.Errorf("continued_from_run_id = %q, want %q", p.ContinuedFromRunID, "sess-0-prior-run-id")
	}

	// Generation 0 with no lineage: both keys absent from the wire, not zero
	// or empty-string valued.
	gen0 := ev
	gen0.RunGeneration = 0
	gen0.ContinuedFromRunID = ""
	raw := decodeRaw(t, gen0)
	if _, present := raw["run_generation"]; present {
		t.Errorf("run_generation present at generation 0: %v", raw["run_generation"])
	}
	if _, present := raw["continued_from_run_id"]; present {
		t.Errorf("continued_from_run_id present at generation 0: %v", raw["continued_from_run_id"])
	}
}

// --- Schema-level assertions ---

func devEventJSON(t *testing.T, ev DevEvent) []byte {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal DevEvent: %v", err)
	}
	return raw
}

func baseValidEvent() DevEvent {
	return DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "ev-1",
		EventType:     EventSessionStarted,
		SessionID:     "sess-1",
		DeveloperDID:  "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		Timestamp:     "2026-07-08T00:00:00Z",
		Tool:          Tool{Name: "claude-code", Kind: ToolShell},
	}
}

func TestSchemaAcceptsRunIdentityFields(t *testing.T) {
	ev := baseValidEvent()
	ev.RunID = "018f1a2b-0000-7000-8000-000000000001"
	ev.RunGeneration = 2
	ev.ContinuedFromRunID = "sess-0-prior-run-id"
	if err := conformance.ValidateDevEvent(devEventJSON(t, ev), false); err != nil {
		t.Errorf("a valid run-identity fixture failed schema validation: %v", err)
	}
}

func TestSchemaRejectsInvalidRunIdentityFields(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*DevEvent)
	}{
		{"negative run_generation", func(ev *DevEvent) { ev.RunGeneration = -1 }},
		{"empty run_id", func(ev *DevEvent) {
			// json.Marshal drops an empty RunID under omitempty, so this case
			// must inject the key directly to exercise minLength:1.
		}},
		{"129-char run_id", func(ev *DevEvent) { ev.RunID = strings.Repeat("a", 129) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := baseValidEvent()
			tc.break_(&ev)
			raw := devEventJSON(t, ev)
			if tc.name == "empty run_id" {
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				m["run_id"] = ""
				var err error
				raw, err = json.Marshal(m)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
			}
			if err := conformance.ValidateDevEvent(raw, false); err == nil {
				t.Errorf("%s: expected schema validation to reject the fixture", tc.name)
			}
		})
	}
}

// TestSchemaEnumAndOneOfHave35Entries binds the schema's own declared
// vocabulary size, independent of AllEventTypes (the vocabulary suite asserts
// the two are equal).
func TestSchemaEnumAndOneOfHave33Entries(t *testing.T) {
	schema, err := conformance.LoadSchema()
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	if v := schema["x-schema-version"]; v != "1.10" {
		t.Errorf("x-schema-version = %v, want 1.10", v)
	}
	props, _ := schema["properties"].(map[string]any)
	sv, _ := props["schema_version"].(map[string]any)
	if sv["const"] != "1.10" {
		t.Errorf("properties.schema_version.const = %v, want 1.10", sv["const"])
	}
	et, _ := props["event_type"].(map[string]any)
	enum, _ := et["enum"].([]any)
	if len(enum) != 35 {
		t.Errorf("event_type.enum has %d entries, want 35", len(enum))
	}
	oneOf, _ := schema["oneOf"].([]any)
	if len(oneOf) != 35 {
		t.Errorf("oneOf has %d branches, want 35", len(oneOf))
	}
	if _, present := props["run_id"]; !present {
		t.Error("properties.run_id is not declared")
	}
	if _, present := props["run_generation"]; !present {
		t.Error("properties.run_generation is not declared")
	}
	if _, present := props["continued_from_run_id"]; !present {
		t.Error("properties.continued_from_run_id is not declared")
	}
	required, _ := schema["required"].([]any)
	for _, r := range required {
		if r == "run_id" || r == "run_generation" || r == "continued_from_run_id" {
			t.Errorf("%v must not be in top-level required; all three are additive/optional", r)
		}
	}
}

// TestOpenboxSessionIDDescriptionNoLongerClaimsRunIDEquality: since v1.8 a
// continued run mints its own run_id, so the description must not equate them.
func TestOpenboxSessionIDDescriptionNoLongerClaimsRunIDEquality(t *testing.T) {
	schema, err := conformance.LoadSchema()
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	sid, _ := props["openbox_session_id"].(map[string]any)
	desc, _ := sid["description"].(string)
	if strings.Contains(desc, "run_id = openbox_session_id") {
		t.Errorf("openbox_session_id description still asserts run_id = openbox_session_id unconditionally: %q", desc)
	}
}

// TestExistingClassesUnchangedByV18Bump is the "no byte change at generation
// 0" guarantee, scoped to what this file can assert independent of
// the full golden suite: the 12 pre-1.8 classes' wireTypeFor/signal names are
// untouched.
func TestExistingClassesUnchangedByV18Bump(t *testing.T) {
	cases := []struct {
		et         EventType
		wire, name string
	}{
		{EventSessionStarted, wireWorkflowStarted, ""},
		{EventSessionEnded, wireWorkflowCompleted, ""},
		{EventPromptSubmitted, wireSignalReceived, "prompt_submitted"},
		{EventCommitCreated, wireSignalReceived, "commit_created"},
		{EventDeploy, wireSignalReceived, "deploy"},
		{EventSubagentStarted, wireSignalReceived, "subagent_started"},
		{EventPermissionDenied, wireSignalReceived, "permission_denied"},
		{EventAPIError, wireSignalReceived, "api_error"},
		{EventToolCall, wireActivityStarted, ""},
		{EventTurnStarted, wireActivityStarted, ""},
		{EventToolResult, wireActivityCompleted, ""},
		{EventTurnCompleted, wireActivityCompleted, ""},
	}
	if len(cases) != 12 {
		t.Fatalf("test table has %d rows, want the 12 pre-1.8 classes", len(cases))
	}
	for _, tc := range cases {
		wire, name, err := wireTypeFor(tc.et)
		if err != nil {
			t.Errorf("wireTypeFor(%s): %v", tc.et, err)
			continue
		}
		if wire != tc.wire || name != tc.name {
			t.Errorf("wireTypeFor(%s) = (%q,%q), want (%q,%q)", tc.et, wire, name, tc.wire, tc.name)
		}
	}
}
