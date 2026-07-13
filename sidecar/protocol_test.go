package sidecar

import (
	"encoding/json"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
)

func TestProtocol_RequestResponseRoundTrip(t *testing.T) {
	req := DecisionRequest{
		Protocol:     ProtocolVersion,
		SessionID:    "sess-9",
		DeveloperDID: "did:aip:x",
		EventType:    client.EventToolCall,
		Tool:         client.Tool{Name: "Bash", Kind: client.ToolShell},
		Attributes:   map[string]any{"command": "echo hi"},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var got DecisionRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != req.SessionID || got.Tool.Name != "Bash" || got.EventType != client.EventToolCall {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	resp := DecisionResponse{
		Protocol:   ProtocolVersion,
		Evaluation: client.Evaluation{Verdict: client.VerdictBlock, Reason: "no"},
		Source:     sourceLocalBundle,
	}
	raw, err = json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var gotResp DecisionResponse
	if err := json.Unmarshal(raw, &gotResp); err != nil {
		t.Fatal(err)
	}
	if gotResp.Evaluation.Verdict != client.VerdictBlock || gotResp.Source != sourceLocalBundle {
		t.Fatalf("response round-trip mismatch: %+v", gotResp)
	}
}

// INV-2: Content is omitted from the wire when absent (metadata-only default).
func TestProtocol_ContentOmittedByDefault(t *testing.T) {
	raw, _ := json.Marshal(DecisionRequest{SessionID: "s"})
	if containsSubstring(string(raw), "content") {
		t.Errorf("absent content should be omitted from the wire, got %s", raw)
	}
}

func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestBuildOPAInput_ShapeMatchesCore(t *testing.T) {
	// The rego input must carry the axes core's buildOPAInput builds
	// (event_type/source/run_id/agent_id + a span for the tool call).
	in := BuildOPAInput(DecisionRequest{
		SessionID:    "run-1",
		DeveloperDID: "did:aip:a",
		WorkspaceID:  "wf-1",
		Org:          "org_x",
		EventType:    client.EventToolCall,
		Tool:         client.Tool{Name: "Bash", Kind: client.ToolShell},
		Attributes:   map[string]any{"command": "ls"},
	})
	if in["event_type"] != "ToolCall" || in["run_id"] != "run-1" || in["workflow_id"] != "wf-1" ||
		in["agent_id"] != "did:aip:a" || in["org"] != "org_x" {
		t.Fatalf("top-level input shape wrong: %+v", in)
	}
	spans, ok := in["spans"].([]any)
	if !ok || len(spans) != 1 {
		t.Fatalf("spans = %v, want 1", in["spans"])
	}
	span := spans[0].(map[string]any)
	if span["name"] != "Bash" || span["tool_kind"] != "shell" || span["semantic_type"] != "command_execution" {
		t.Fatalf("span shape wrong: %+v", span)
	}
	if span["command"] != "ls" {
		t.Errorf("attribute not merged into span: %+v", span)
	}
}
