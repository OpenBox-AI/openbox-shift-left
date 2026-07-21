package decision

import (
	"encoding/json"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// TestParseBundle_AcceptsBuilderFields proves the new PIN + PolicyBuilder fields
// round-trip through the DisallowUnknownFields decoder (STORY-E6-S8 §3).
func TestParseBundle_AcceptsBuilderFields(t *testing.T) {
	raw := []byte(`{
		"version": "pol-1@2026-07-15",
		"policy_id": "pol-1",
		"updated_at": "2026-07-15T00:00:00Z",
		"policy_builder": {"version":1,"rules":[
			{"id":"r1","decision":"BLOCK","reason":"no rm","matchMode":"all","conditions":[
				{"field":"spans[_].attributes.command","operator":"contains","transform":"value","value":"rm -rf","valueType":"string"}
			]}
		]}
	}`)
	b, err := ParseBundle(raw)
	if err != nil {
		t.Fatalf("ParseBundle builder bundle: %v", err)
	}
	if b.PolicyID != "pol-1" || b.UpdatedAt != "2026-07-15T00:00:00Z" {
		t.Errorf("pin not parsed: %+v", b)
	}
	if b.PolicyBuilder == nil || len(b.PolicyBuilder.Rules) != 1 {
		t.Fatalf("policy_builder not parsed: %+v", b.PolicyBuilder)
	}
}

// TestParseBundle_RejectsBadBuilderDecision pins G_SEC-INFO-3: a typo'd builder
// decision (e.g. "BLOKC") would silently map to ALLOW via decisionToVerdict —
// dropping a BLOCK. validate() must reject it at parse time (keeping the last-good
// bundle), exactly as it does for the legacy Rules[] path.
func TestParseBundle_RejectsBadBuilderDecision(t *testing.T) {
	bad := []byte(`{"version":"v","policy_id":"p","policy_builder":{"version":1,"rules":[
		{"id":"r1","decision":"BLOKC","reason":"typo","matchMode":"all","conditions":[
			{"field":"spans[_].attributes.command","operator":"contains","transform":"value","value":"rm","valueType":"string"}
		]}
	]}}`)
	if _, err := ParseBundle(bad); err == nil {
		t.Fatalf("ParseBundle must reject an unrecognized builder decision (BLOKC) so a BLOCK is never silently dropped")
	}
	// A valid decision still parses.
	good := []byte(`{"version":"v","policy_id":"p","policy_builder":{"version":1,"rules":[
		{"id":"r1","decision":"BLOCK","reason":"ok","matchMode":"all","conditions":[
			{"field":"spans[_].attributes.command","operator":"contains","transform":"value","value":"rm","valueType":"string"}
		]}
	]}}`)
	if _, err := ParseBundle(good); err != nil {
		t.Fatalf("ParseBundle rejected a valid builder decision: %v", err)
	}
}

// TestSetBundle_SelectsBuilderEvaluator: a builder bundle is evaluated FIRST-MATCH
// through the sidecar decide path (Source=local-bundle, a real verdict).
func TestSetBundle_SelectsBuilderEvaluator(t *testing.T) {
	raw := []byte(`{"version":"v","policy_id":"pol-x","policy_builder":{"version":1,"rules":[
		{"decision":"BLOCK","reason":"destructive","matchMode":"all","conditions":[
			{"field":"spans[_].attributes.command","operator":"contains","transform":"value","value":"rm -rf","valueType":"string"}
		]}
	]}}`)
	b, err := ParseBundle(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s := NewServer(ServerConfig{})
	s.SetBundle(b)

	resp := s.decide(DecisionRequest{
		SessionID: "s", EventType: client.EventToolCall,
		Tool:       client.Tool{Name: "Bash", Kind: client.ToolShell},
		Attributes: map[string]any{"command": "rm -rf /tmp/x"},
	})
	if resp.Source != sourceLocalBundle {
		t.Errorf("source = %q, want %q (a real verdict)", resp.Source, sourceLocalBundle)
	}
	if resp.Evaluation.Verdict != client.VerdictBlock {
		t.Errorf("verdict = %v, want BLOCK", resp.Evaluation.Verdict)
	}
	if resp.Evaluation.PolicyID != "pol-x" {
		t.Errorf("policy id = %q, want pol-x", resp.Evaluation.PolicyID)
	}
	// A benign command → default ALLOW (real verdict, not a fail-open).
	benign := s.decide(DecisionRequest{
		SessionID: "s", EventType: client.EventToolCall,
		Tool:       client.Tool{Name: "Bash", Kind: client.ToolShell},
		Attributes: map[string]any{"command": "echo hi"},
	})
	if benign.Source != sourceLocalBundle || benign.Evaluation.Verdict != client.VerdictAllow {
		t.Errorf("benign builder decision = %v/%q, want ALLOW/local-bundle", benign.Evaluation.Verdict, benign.Source)
	}
}

// TestRawRegoBundle_ServesRealAllow: a raw-rego-unlocalized bundle serves a REAL
// local ALLOW (sourceLocalBundle) so it proceeds under BOTH fail-open and
// fail-closed — honest under-blocking, never over-blocking (ADR-0005 §Decision-2).
func TestRawRegoBundle_ServesRealAllow(t *testing.T) {
	raw, _ := json.Marshal(Bundle{Version: "v", PolicyID: "p", RawRegoUnlocalized: true})
	b, err := ParseBundle(raw)
	if err != nil {
		t.Fatalf("parse raw-rego bundle: %v", err)
	}
	s := NewServer(ServerConfig{})
	s.SetBundle(b)
	resp := s.decide(DecisionRequest{
		SessionID: "s", EventType: client.EventToolCall,
		Tool:       client.Tool{Name: "Bash", Kind: client.ToolShell},
		Attributes: map[string]any{"command": "rm -rf /"},
	})
	if resp.Source != sourceLocalBundle || resp.Evaluation.Verdict != client.VerdictAllow {
		t.Errorf("raw-rego bundle = %v/%q, want ALLOW/local-bundle (real allow, proceeds under fail-closed)", resp.Evaluation.Verdict, resp.Source)
	}
}
