package sidecar

import (
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
)

func toolCall(name string, kind client.ToolKind, attrs map[string]any) DecisionRequest {
	return DecisionRequest{
		Protocol:     ProtocolVersion,
		SessionID:    "sess-1",
		DeveloperDID: "did:aip:dev1",
		EventType:    client.EventToolCall,
		Tool:         client.Tool{Name: name, Kind: kind},
		Attributes:   attrs,
	}
}

func TestDecisionToVerdict(t *testing.T) {
	cases := map[string]client.Verdict{
		"continue":         client.VerdictAllow,
		"allow":            client.VerdictAllow,
		"CONSTRAIN":        client.VerdictConstrain,
		"require_approval": client.VerdictRequireApproval,
		"require-approval": client.VerdictRequireApproval,
		"block":            client.VerdictBlock,
		"stop":             client.VerdictHalt,
		"halt":             client.VerdictHalt,
		"":                 client.VerdictAllow,
		"gibberish":        client.VerdictAllow, // unknown never denies
	}
	for in, want := range cases {
		if got := decisionToVerdict(in); got != want {
			t.Errorf("decisionToVerdict(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBundleEvaluator_DefaultAllowWhenNoRuleMatches(t *testing.T) {
	b := &Bundle{Version: "v1", Rules: []Rule{
		{Match: RuleMatch{ToolName: "Bash", AttributeContains: map[string]string{"command": "rm -rf /"}}, Decision: "block", Reason: "destructive"},
	}}
	e := newBundleEvaluator(b)
	got := e.Evaluate(toolCall("Bash", client.ToolShell, map[string]any{"command": "ls -la"}))
	if got.Verdict != client.VerdictAllow {
		t.Fatalf("non-matching call: got %q, want ALLOW", got.Verdict)
	}
}

func TestBundleEvaluator_BlockMatch(t *testing.T) {
	b := &Bundle{Version: "v1", Rules: []Rule{
		{ID: "no-rmrf", Match: RuleMatch{ToolName: "Bash", AttributeContains: map[string]string{"command": "rm -rf /"}},
			Decision: "block", Reason: "destructive delete", PolicyID: "pol-1"},
	}}
	e := newBundleEvaluator(b)
	got := e.Evaluate(toolCall("Bash", client.ToolShell, map[string]any{"command": "sudo rm -rf / --no-preserve-root"}))
	if got.Verdict != client.VerdictBlock {
		t.Fatalf("got %q, want BLOCK", got.Verdict)
	}
	if !got.WouldBlock() {
		t.Errorf("WouldBlock() = false, want true")
	}
	if got.Reason != "destructive delete" || got.PolicyID != "pol-1" {
		t.Errorf("reason/policy not surfaced: %+v", got)
	}
}

func TestBundleEvaluator_MaxSeverityAcrossMatches(t *testing.T) {
	// Two rules match the same call; the evaluator must return the HIGHER severity
	// (BLOCK over CONSTRAIN), mirroring core's HighestPriorityVerdict — never
	// under-block on overlapping rules.
	b := &Bundle{Version: "v1", Rules: []Rule{
		{ID: "constrain-etc", Match: RuleMatch{AttributeContains: map[string]string{"file_path": "/etc"}}, Decision: "constrain"},
		{ID: "block-passwd", Match: RuleMatch{AttributeContains: map[string]string{"file_path": "/etc/passwd"}}, Decision: "block", Reason: "sensitive"},
	}}
	e := newBundleEvaluator(b)
	got := e.Evaluate(toolCall("Edit", client.ToolFile, map[string]any{"file_path": "/etc/passwd"}))
	if got.Verdict != client.VerdictBlock {
		t.Fatalf("got %q, want BLOCK (max severity)", got.Verdict)
	}
}

func TestBundleEvaluator_MaxSeverityWhenHighComesFirst(t *testing.T) {
	// The headline safety property: a BLOCK rule that appears BEFORE a matching
	// lower-severity rule must still win — this distinguishes max-severity from a
	// last-match-wins bug (G3 F3). Ordered BLOCK-first, CONSTRAIN-second.
	b := &Bundle{Version: "v1", Rules: []Rule{
		{ID: "block-passwd", Match: RuleMatch{AttributeContains: map[string]string{"file_path": "/etc/passwd"}}, Decision: "block", Reason: "sensitive"},
		{ID: "constrain-etc", Match: RuleMatch{AttributeContains: map[string]string{"file_path": "/etc"}}, Decision: "constrain"},
	}}
	e := newBundleEvaluator(b)
	got := e.Evaluate(toolCall("Edit", client.ToolFile, map[string]any{"file_path": "/etc/passwd"}))
	if got.Verdict != client.VerdictBlock {
		t.Fatalf("BLOCK-first + later CONSTRAIN match: got %q, want BLOCK", got.Verdict)
	}
	if got.Reason != "sensitive" {
		t.Errorf("expected the BLOCK rule's metadata, got %q", got.Reason)
	}
}

func TestBundleEvaluator_MatchDimensions(t *testing.T) {
	b := &Bundle{Version: "v1", Rules: []Rule{
		{ID: "mcp-only", Match: RuleMatch{ToolKind: "mcp", MCPServer: "github"}, Decision: "require_approval"},
	}}
	e := newBundleEvaluator(b)

	// Wrong kind → no match → allow.
	if got := e.Evaluate(toolCall("x", client.ToolShell, nil)); got.Verdict != client.VerdictAllow {
		t.Errorf("shell against mcp rule: got %q, want ALLOW", got.Verdict)
	}
	// Right kind + server → match.
	req := DecisionRequest{SessionID: "s", EventType: client.EventToolCall,
		Tool: client.Tool{Name: "create_pr", Kind: client.ToolMCP, MCPServer: "github"}}
	if got := e.Evaluate(req); got.Verdict != client.VerdictRequireApproval {
		t.Errorf("mcp github: got %q, want REQUIRE_APPROVAL", got.Verdict)
	}
}

func TestBundleEvaluator_NilBundleAllows(t *testing.T) {
	e := newBundleEvaluator(nil)
	if got := e.Evaluate(toolCall("Bash", client.ToolShell, nil)); got.Verdict != client.VerdictAllow {
		t.Errorf("nil bundle: got %q, want ALLOW", got.Verdict)
	}
}

func TestVerdictPriorityOrder(t *testing.T) {
	order := []client.Verdict{
		client.VerdictUnknown, client.VerdictAllow, client.VerdictConstrain,
		client.VerdictRequireApproval, client.VerdictBlock, client.VerdictHalt,
	}
	for i := 1; i < len(order); i++ {
		if verdictPriority(order[i]) <= verdictPriority(order[i-1]) {
			t.Errorf("priority not strictly increasing at %q vs %q", order[i], order[i-1])
		}
	}
}
