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
	// STORY-E6-S8 AC-2 (load-bearing): the input document must match core's
	// buildOPAInput + buildSpanMap field NAMES so a builder policy authored against
	// core fires identically. Top-level keys align to buildOPAInput; NO top-level
	// `org` (core carries org only in the OPA query path).
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
		in["agent_id"] != "did:aip:a" || in["span_count"] != 1 {
		t.Fatalf("top-level input shape wrong: %+v", in)
	}
	if _, present := in["org"]; present {
		t.Errorf("top-level `org` must NOT be present (core carries org in the query path): %+v", in)
	}
	span := in["spans"].([]any)[0].(map[string]any)

	// The always-present buildSpanMap keys.
	for _, k := range []string{"span_id", "trace_id", "name", "semantic_type", "start_time", "end_time", "attributes"} {
		if _, ok := span[k]; !ok {
			t.Errorf("span missing always-present buildSpanMap key %q: %+v", k, span)
		}
	}
	if span["name"] != "Bash" {
		t.Errorf("span name = %v, want Bash", span["name"])
	}
	// Shell has NO core semantic type → "internal" (a shell command matches on
	// attributes.command, a local-only axis, never on semantic_type).
	if span["semantic_type"] != "internal" {
		t.Errorf("shell semantic_type = %v, want internal", span["semantic_type"])
	}
	// The load-bearing negatives: NO top-level `tool_kind`, NO top-level `command`.
	if _, ok := span["tool_kind"]; ok {
		t.Errorf("span must NOT carry a top-level tool_kind key: %+v", span)
	}
	if _, ok := span["command"]; ok {
		t.Errorf("command must live under attributes, NOT as a top-level span key: %+v", span)
	}
	attrs := span["attributes"].(map[string]any)
	if attrs["command"] != "ls" {
		t.Errorf("command must be under span.attributes: %+v", attrs)
	}
}

// TestBuildOPAInput_SpanShapePerToolKind asserts the emitted field names match
// core's buildSpanMap for each tool kind a CC hook produces (STORY-E6-S8 AC-2).
func TestBuildOPAInput_SpanShapePerToolKind(t *testing.T) {
	cases := []struct {
		name         string
		req          DecisionRequest
		wantSemantic string
		wantTop      map[string]any // top-level span keys that MUST be present w/ value
		wantAttrs    map[string]any // keys that MUST be under attributes
		notTop       []string       // keys that must NOT be top-level span keys
	}{
		{
			name: "file write",
			req: DecisionRequest{
				SessionID: "s", EventType: client.EventToolCall,
				Tool:       client.Tool{Name: "Write", Kind: client.ToolFile},
				Attributes: map[string]any{"file_path": "/etc/passwd", "file_operation": "write", "permission_mode": "default"},
			},
			wantSemantic: "file_write",
			wantTop:      map[string]any{"file_path": "/etc/passwd", "file_operation": "write"},
			wantAttrs:    map[string]any{"permission_mode": "default"},
			notTop:       []string{"tool_kind", "command"},
		},
		{
			name: "file read",
			req: DecisionRequest{
				SessionID: "s", EventType: client.EventToolCall,
				Tool:       client.Tool{Name: "Read", Kind: client.ToolFile},
				Attributes: map[string]any{"file_path": "/tmp/x", "file_operation": "read"},
			},
			wantSemantic: "file_read",
			wantTop:      map[string]any{"file_path": "/tmp/x", "file_operation": "read"},
		},
		{
			name: "mcp tool call",
			req: DecisionRequest{
				SessionID: "s", EventType: client.EventToolCall,
				Tool:       client.Tool{Name: "mcp__github__create_issue", Kind: client.ToolMCP, MCPServer: "github"},
				Attributes: map[string]any{"mcp_function": "create_issue"},
			},
			wantSemantic: "mcp_tool_call",
			wantAttrs:    map[string]any{"mcp_server": "github", "mcp_function": "create_issue"},
			notTop:       []string{"tool_kind", "mcp_server"},
		},
		{
			name: "shell command",
			req: DecisionRequest{
				SessionID: "s", EventType: client.EventToolCall,
				Tool:       client.Tool{Name: "Bash", Kind: client.ToolShell},
				Attributes: map[string]any{"command": "rm -rf /"},
			},
			wantSemantic: "internal",
			wantAttrs:    map[string]any{"command": "rm -rf /"},
			notTop:       []string{"tool_kind", "command"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			span := BuildOPAInput(tc.req)["spans"].([]any)[0].(map[string]any)
			if span["semantic_type"] != tc.wantSemantic {
				t.Errorf("semantic_type = %v, want %v", span["semantic_type"], tc.wantSemantic)
			}
			for k, v := range tc.wantTop {
				if span[k] != v {
					t.Errorf("top-level span[%q] = %v, want %v", k, span[k], v)
				}
			}
			attrs := span["attributes"].(map[string]any)
			for k, v := range tc.wantAttrs {
				if attrs[k] != v {
					t.Errorf("attributes[%q] = %v, want %v", k, attrs[k], v)
				}
			}
			for _, k := range tc.notTop {
				if _, ok := span[k]; ok {
					t.Errorf("span must NOT carry top-level key %q: %+v", k, span)
				}
			}
		})
	}
}
