package client

import (
	"encoding/json"
	"testing"
)

func lifecycleEvent(et EventType) DevEvent {
	return DevEvent{
		EventID: "e-" + string(et), EventType: et, SessionID: "sess-1", DeveloperDID: "did:aip:abc",
		Timestamp: "2026-07-15T00:00:00Z",
		Tool:      Tool{Name: agentToolNameForType(et), Kind: ToolShell},
	}
}

func agentToolNameForType(et EventType) string {
	switch et {
	case EventCommitCreated, EventDeploy:
		return "openbox-git-action"
	default:
		return "claude-code"
	}
}

func assertWorkflowFields(t *testing.T, p governanceEventPayload) {
	t.Helper()
	if p.WorkflowID == "" || p.RunID == "" || p.WorkflowType == "" {
		t.Errorf("base contract requires workflow_id/run_id/workflow_type; got (%q,%q,%q)",
			p.WorkflowID, p.RunID, p.WorkflowType)
	}
}

// TestLifecycle_WireTypes maps each non-tool event onto its base wire
// event_type (+ signal_name for signals), and proves every one carries the
// required workflow triple.
func TestLifecycle_WireTypes(t *testing.T) {
	cases := []struct {
		et         EventType
		wantType   string
		wantSignal string
	}{
		{EventSessionStarted, "WorkflowStarted", ""},
		{EventSessionEnded, "WorkflowCompleted", ""},
		{EventPromptSubmitted, "SignalReceived", "prompt_submitted"},
		{EventCommitCreated, "SignalReceived", "commit_created"},
		{EventDeploy, "SignalReceived", "deploy"},
	}
	for _, c := range cases {
		t.Run(string(c.et), func(t *testing.T) {
			p := decodePayload(t, lifecycleEvent(c.et))
			if p.EventType != c.wantType {
				t.Errorf("event_type = %q, want %q", p.EventType, c.wantType)
			}
			if p.SignalName != c.wantSignal {
				t.Errorf("signal_name = %q, want %q", p.SignalName, c.wantSignal)
			}
			if (c.wantType == "SignalReceived") != (p.SignalName != "") {
				t.Errorf("signal_name presence (%q) must match SignalReceived (%v)", p.SignalName, c.wantType == "SignalReceived")
			}
			assertWorkflowFields(t, p)
			if p.WorkflowType != workflowType {
				t.Errorf("workflow_type = %q, want constant %q", p.WorkflowType, workflowType)
			}
		})
	}
}

// TestLifecycle_RequiredFieldsOnWire asserts the base contract's required
// fields are physically present on the raw wire (not merely non-empty after
// decode, which omitempty makes indistinguishable from absent): every
// lifecycle event ships workflow_id/run_id/workflow_type, and a signal
// additionally ships signal_name; the exact keys event_rules.go's
// _REQUIRED_WORKFLOW_FIELDS + signal rule demand.
func TestLifecycle_RequiredFieldsOnWire(t *testing.T) {
	signals := map[EventType]bool{EventPromptSubmitted: true, EventCommitCreated: true, EventDeploy: true}
	for _, et := range []EventType{EventSessionStarted, EventSessionEnded, EventPromptSubmitted, EventCommitCreated, EventDeploy} {
		b, err := buildPayload(lifecycleEvent(et))
		if err != nil {
			t.Fatalf("%s buildPayload: %v", et, err)
		}
		var raw map[string]any
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range []string{"workflow_id", "run_id", "workflow_type"} {
			if v, present := raw[k]; !present || v == "" {
				t.Errorf("%s: required field %q missing on the wire (got present=%v val=%v)", et, k, present, v)
			}
		}
		_, hasSignal := raw["signal_name"]
		if signals[et] != hasSignal {
			t.Errorf("%s: signal_name on wire = %v, want %v", et, hasSignal, signals[et])
		}
	}
}

// TestLifecycle_OneWorkflowIdentity is the session=workflow contract: a
// session's WorkflowStarted, a SignalReceived it carries, and its
// WorkflowCompleted share one (workflow_id, run_id, workflow_type) triple, so
// Core resolves them to a single workflow/session row (storage_session.go
// create → lookup → terminal).
func TestLifecycle_OneWorkflowIdentity(t *testing.T) {
	start := decodePayload(t, lifecycleEvent(EventSessionStarted))
	sig := decodePayload(t, lifecycleEvent(EventPromptSubmitted))
	end := decodePayload(t, lifecycleEvent(EventSessionEnded))

	for _, p := range []governanceEventPayload{sig, end} {
		if p.WorkflowID != start.WorkflowID || p.RunID != start.RunID || p.WorkflowType != start.WorkflowType {
			t.Errorf("workflow identity drift: start=(%q,%q,%q) other=(%q,%q,%q)",
				start.WorkflowID, start.RunID, start.WorkflowType,
				p.WorkflowID, p.RunID, p.WorkflowType)
		}
	}
	if start.RunID != "sess-1" || start.WorkflowID != "did:aip:abc" {
		t.Errorf("(workflow_id, run_id) = (%q,%q), want DID fallback + session id", start.WorkflowID, start.RunID)
	}
}

// TestLifecycle_WorkspaceIDIsWorkflowID asserts an explicit WorkspaceID
// overrides the DID fallback as the workflow_id (per-workspace grouping,
// mapping.md §1).
func TestLifecycle_WorkspaceIDIsWorkflowID(t *testing.T) {
	ev := lifecycleEvent(EventSessionStarted)
	ev.WorkspaceID = "repo-x"
	if p := decodePayload(t, ev); p.WorkflowID != "repo-x" {
		t.Errorf("workflow_id = %q, want explicit WorkspaceID repo-x", p.WorkflowID)
	}
}

// TestLifecycle_SpanLess asserts lifecycle events carry NO span; the base
// contract rejects a span-bearing non-hook lifecycle event (event_rules
// HOOK_TRIGGER_FALSE / ACTIVITY_COMPLETED_WITH_SPANS), and only the tool hook
// path emits spans.
func TestLifecycle_SpanLess(t *testing.T) {
	for _, et := range []EventType{EventSessionStarted, EventSessionEnded, EventPromptSubmitted, EventCommitCreated, EventDeploy} {
		b, err := buildPayload(lifecycleEvent(et))
		if err != nil {
			t.Fatalf("%s buildPayload: %v", et, err)
		}
		var raw map[string]any
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, present := raw["spans"]; present {
			t.Errorf("%s must be span-less; got spans=%v", et, raw["spans"])
		}
		if _, present := raw["hook_trigger"]; present {
			t.Errorf("%s must not set hook_trigger", et)
		}
	}
}

// TestLifecycle_CommitLineageSurvivesSignal asserts a CommitCreated's lineage
// keys (no first-class Core column) survive the Signal mapping in the pass-
// through metadata blob (FR-5).
func TestLifecycle_CommitLineageSurvivesSignal(t *testing.T) {
	ev := lifecycleEvent(EventCommitCreated)
	ev.Metadata = map[string]any{"commit_sha": "abc123", "repo": "openbox-shift-left", "branch": "main"}

	p := decodePayload(t, ev)
	if p.EventType != "SignalReceived" || p.SignalName != "commit_created" {
		t.Fatalf("commit mapping = (%q,%q), want SignalReceived/commit_created", p.EventType, p.SignalName)
	}
	m := rawMeta(t, p)
	for k, want := range map[string]string{"commit_sha": "abc123", "repo": "openbox-shift-left", "branch": "main"} {
		if m[k] != want {
			t.Errorf("metadata[%q] = %v, want %q (lineage must survive the Signal mapping)", k, m[k], want)
		}
	}
}

// TestLifecycle_DeployLineageSurvivesSignal asserts a Deploy's lineage keys
// (deploy_id, commit_sha, deploy_did, environment) survive into the Signal
// metadata (FR-6/7).
func TestLifecycle_DeployLineageSurvivesSignal(t *testing.T) {
	ev := lifecycleEvent(EventDeploy)
	ev.Metadata = map[string]any{
		"deploy_id":   "dep-9",
		"commit_sha":  "abc123",
		"deploy_did":  "did:aip:deploy-x",
		"repo":        "openbox-shift-left",
		"environment": "production",
	}

	p := decodePayload(t, ev)
	if p.EventType != "SignalReceived" || p.SignalName != "deploy" {
		t.Fatalf("deploy mapping = (%q,%q), want SignalReceived/deploy", p.EventType, p.SignalName)
	}
	m := rawMeta(t, p)
	for _, k := range []string{"deploy_id", "commit_sha", "deploy_did", "repo", "environment"} {
		if _, ok := m[k]; !ok {
			t.Errorf("deploy lineage key %q missing from Signal metadata (FR-6/7)", k)
		}
	}
}

// TestLifecycle_ActivityLabelPreserved asserts the DevEvent type is preserved
// as the dashboard activity_type label (additive pass-through column) even
// though the wire event_type is rewritten; so the shared dashboard shows
// "SessionStarted"/ "Deploy", never "Unknown".
func TestLifecycle_ActivityLabelPreserved(t *testing.T) {
	for _, et := range []EventType{EventSessionStarted, EventSessionEnded, EventPromptSubmitted, EventCommitCreated, EventDeploy} {
		if p := decodePayload(t, lifecycleEvent(et)); p.ActivityType != string(et) {
			t.Errorf("%s activity_type = %q, want the DevEvent type as the dashboard label", et, p.ActivityType)
		}
	}
}
