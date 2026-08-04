package decision

import (
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// STORY-E6-S8 AC-1: native builder verdict parity. These pin the evaluator to
// the semantics the backend's builder→rego compilation (policy-builder.util.ts)
// produces, so its verdict equals what core's external OPA would return.

func cond(field, op, transform, value, valueType string) PolicyBuilderCondition {
	return PolicyBuilderCondition{Field: field, Operator: op, Transform: transform, Value: value, ValueType: valueType}
}

func TestResolvePath(t *testing.T) {
	input := map[string]any{
		"event_type": "ToolCall",
		"span_count": 1,
		"spans": []any{
			map[string]any{
				"file_path":     "/etc/passwd",
				"semantic_type": "file_write",
				"attributes":    map[string]any{"command": "rm -rf /"},
			},
			map[string]any{
				"file_path":     "/tmp/ok",
				"semantic_type": "file_read",
				"attributes":    map[string]any{},
			},
		},
	}
	cases := []struct {
		field string
		want  []any
	}{
		{"event_type", []any{"ToolCall"}},
		{"input.event_type", []any{"ToolCall"}}, // input. prefix stripped
		{"spans[_].file_path", []any{"/etc/passwd", "/tmp/ok"}},
		{"spans[_].semantic_type", []any{"file_write", "file_read"}},
		{"spans[_].attributes.command", []any{"rm -rf /"}}, // only span 0 has it
		{"missing", nil},
		{"spans[_].nope", nil},
	}
	for _, tc := range cases {
		got := resolvePath(input, tc.field)
		if len(got) != len(tc.want) {
			t.Errorf("resolvePath(%q) = %v, want %v", tc.field, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("resolvePath(%q)[%d] = %v, want %v", tc.field, i, got[i], tc.want[i])
			}
		}
	}
}

func TestConditionHolds_Operators(t *testing.T) {
	input := map[string]any{
		"span_count": float64(3),
		"spans": []any{
			map[string]any{
				"file_path":     "/etc/passwd",
				"semantic_type": "file_write",
				"attributes":    map[string]any{"command": "RM -rf /tmp", "count": float64(7)},
			},
		},
	}
	tests := []struct {
		name string
		c    PolicyBuilderCondition
		want bool
	}{
		// equals — typed. string match.
		{"equals string hit", cond("spans[_].semantic_type", "equals", "value", "file_write", "string"), true},
		{"equals string miss", cond("spans[_].semantic_type", "equals", "value", "file_read", "string"), false},
		// equals number vs string value type: 1 == "7" false (type-sensitive).
		{"equals number type-sensitive", cond("spans[_].attributes.count", "equals", "value", "7", "string"), false},
		{"equals number hit", cond("spans[_].attributes.count", "equals", "value", "7", "number"), true},
		// not_equals.
		{"not_equals hit", cond("spans[_].semantic_type", "not_equals", "value", "file_read", "string"), true},
		{"not_equals miss", cond("spans[_].semantic_type", "not_equals", "value", "file_write", "string"), false},
		// contains — case-insensitive substring, value treated as string regardless.
		{"contains ci hit", cond("spans[_].attributes.command", "contains", "value", "rm -rf", "string"), true},
		{"contains miss", cond("spans[_].attributes.command", "contains", "value", "curl", "string"), false},
		// ordering — numeric when valueType=number.
		{"gt hit", cond("spans[_].attributes.count", "greater_than", "value", "5", "number"), true},
		{"gt miss", cond("spans[_].attributes.count", "greater_than", "value", "9", "number"), false},
		{"gte eq", cond("spans[_].attributes.count", "greater_than_or_equal", "value", "7", "number"), true},
		{"lt hit", cond("spans[_].attributes.count", "less_than", "value", "9", "number"), true},
		{"lte eq", cond("spans[_].attributes.count", "less_than_or_equal", "value", "7", "number"), true},
		// exists / not_exists.
		{"exists hit", cond("spans[_].file_path", "exists", "value", "", "string"), true},
		{"exists miss", cond("spans[_].nope", "exists", "value", "", "string"), false},
		{"not_exists hit", cond("spans[_].nope", "not_exists", "value", "", "string"), true},
		{"not_exists miss", cond("spans[_].file_path", "not_exists", "value", "", "string"), false},
		// count transform: [_] path → count of matched elements.
		{"count[_] eq", cond("spans[_].file_path", "equals", "count", "1", "number"), true},
		{"count[_] gt", cond("spans[_].file_path", "greater_than", "count", "0", "number"), true},
		// undefined path never holds for a comparison (rego undefined).
		{"gt undefined", cond("spans[_].nope", "greater_than", "value", "0", "number"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.c
			if got := conditionHolds(&c, input); got != tc.want {
				t.Errorf("conditionHolds(%+v) = %v, want %v", tc.c, got, tc.want)
			}
		})
	}
}

// TestConditionHolds_CountForcesNumeric pins G3-F1: a `count` condition compares
// NUMERICALLY even when the stored valueType is (wrongly) "string" — the backend
// forces valueType=number for count, and we mirror it, so the float count target
// is never compared against a quoted-string literal (which would silently never
// match under regoCompare's cross-type ordering).
func TestConditionHolds_CountForcesNumeric(t *testing.T) {
	input := map[string]any{"spans": []any{
		map[string]any{"file_path": "/a"},
		map[string]any{"file_path": "/b"},
	}}
	// count of spans[_].file_path == 2, valueType LEFT as "string".
	eq := cond("spans[_].file_path", "equals", "count", "2", "string")
	if !conditionHolds(&eq, input) {
		t.Errorf("count equals should compare numerically despite valueType=string (2 == 2)")
	}
	gt := cond("spans[_].file_path", "greater_than", "count", "1", "string")
	if !conditionHolds(&gt, input) {
		t.Errorf("count greater_than should compare numerically despite valueType=string (2 > 1)")
	}
	miss := cond("spans[_].file_path", "equals", "count", "3", "string")
	if conditionHolds(&miss, input) {
		t.Errorf("count equals 3 must not hold (count is 2)")
	}
}

func TestConditionHolds_BooleanValueType(t *testing.T) {
	input := map[string]any{"spans": []any{map[string]any{"attributes": map[string]any{"flag": true}}}}
	yes := cond("spans[_].attributes.flag", "equals", "value", "true", "boolean")
	no := cond("spans[_].attributes.flag", "equals", "value", "false", "boolean")
	if !conditionHolds(&yes, input) {
		t.Errorf("bool true == true should hold")
	}
	if conditionHolds(&no, input) {
		t.Errorf("bool true == false should not hold")
	}
}

// TestBuilderEvaluator_FirstMatch is the crux: precedence is FIRST-MATCH by rule
// order, NOT max-severity. An earlier ALLOW rule wins over a later BLOCK rule for
// the same event (the reverse of bundleEvaluator).
func TestBuilderEvaluator_FirstMatch(t *testing.T) {
	cfg := &PolicyBuilderConfig{Version: 1, Rules: []PolicyBuilderRule{
		{
			Decision: "ALLOW", Reason: "allowlisted first", MatchMode: "all",
			Conditions: []PolicyBuilderCondition{cond("spans[_].attributes.command", "contains", "value", "rm -rf", "string")},
		},
		{
			Decision: "BLOCK", Reason: "would block but loses to the earlier rule", MatchMode: "all",
			Conditions: []PolicyBuilderCondition{cond("spans[_].attributes.command", "contains", "value", "rm -rf", "string")},
		},
	}}
	e := newBuilderEvaluator(cfg, "pol-1")
	req := DecisionRequest{
		SessionID: "s", EventType: client.EventToolCall,
		Tool:       client.Tool{Name: "Bash", Kind: client.ToolShell},
		Attributes: map[string]any{"command": "rm -rf /tmp/x"},
	}
	ev := e.Evaluate(req)
	if ev.Verdict != client.VerdictAllow {
		t.Fatalf("first-match: verdict = %v, want ALLOW (earlier rule wins over later BLOCK)", ev.Verdict)
	}
	if ev.Reason != "allowlisted first" || ev.PolicyID != "pol-1" {
		t.Errorf("first-match: reason/policy = %q/%q", ev.Reason, ev.PolicyID)
	}
}

func TestBuilderEvaluator_MatchModeAllVsAny(t *testing.T) {
	req := DecisionRequest{
		SessionID: "s", EventType: client.EventToolCall,
		Tool:       client.Tool{Name: "Write", Kind: client.ToolFile},
		Attributes: map[string]any{"file_path": "/etc/passwd", "file_operation": "write"},
	}
	// "all": both conditions must hold. One holds (file_write), one does not (path
	// contains /nope) → no match → default ALLOW.
	allCfg := &PolicyBuilderConfig{Version: 1, Rules: []PolicyBuilderRule{{
		Decision: "BLOCK", MatchMode: "all", Conditions: []PolicyBuilderCondition{
			cond("spans[_].semantic_type", "equals", "value", "file_write", "string"),
			cond("spans[_].file_path", "contains", "value", "/nope", "string"),
		},
	}}}
	if v := newBuilderEvaluator(allCfg, "p").Evaluate(req).Verdict; v != client.VerdictAllow {
		t.Errorf("matchMode all with one false condition: verdict = %v, want ALLOW", v)
	}
	// "any": the one that holds is enough → BLOCK.
	anyCfg := &PolicyBuilderConfig{Version: 1, Rules: []PolicyBuilderRule{{
		Decision: "BLOCK", MatchMode: "any", Conditions: []PolicyBuilderCondition{
			cond("spans[_].semantic_type", "equals", "value", "file_write", "string"),
			cond("spans[_].file_path", "contains", "value", "/nope", "string"),
		},
	}}}
	if v := newBuilderEvaluator(anyCfg, "p").Evaluate(req).Verdict; v != client.VerdictBlock {
		t.Errorf("matchMode any with one true condition: verdict = %v, want BLOCK", v)
	}
}

func TestBuilderEvaluator_DefaultAllow(t *testing.T) {
	cfg := &PolicyBuilderConfig{Version: 1, Rules: []PolicyBuilderRule{{
		Decision: "BLOCK", MatchMode: "all",
		Conditions: []PolicyBuilderCondition{cond("spans[_].file_path", "contains", "value", "/secret", "string")},
	}}}
	req := DecisionRequest{
		SessionID: "s", EventType: client.EventToolCall,
		Tool:       client.Tool{Name: "Read", Kind: client.ToolFile},
		Attributes: map[string]any{"file_path": "/tmp/harmless", "file_operation": "read"},
	}
	if v := newBuilderEvaluator(cfg, "p").Evaluate(req).Verdict; v != client.VerdictAllow {
		t.Errorf("no rule matches → default ALLOW; got %v", v)
	}
}

// TestBuilderEvaluator_DecisionMapping covers all four builder decisions →
// verdicts via decisionToVerdict (uppercase, case-insensitive).
func TestBuilderEvaluator_DecisionMapping(t *testing.T) {
	req := DecisionRequest{
		SessionID: "s", EventType: client.EventToolCall,
		Tool:       client.Tool{Name: "Bash", Kind: client.ToolShell},
		Attributes: map[string]any{"command": "danger"},
	}
	for decision, want := range map[string]client.Verdict{
		"ALLOW":            client.VerdictAllow,
		"REQUIRE_APPROVAL": client.VerdictRequireApproval,
		"BLOCK":            client.VerdictBlock,
		"HALT":             client.VerdictHalt,
	} {
		cfg := &PolicyBuilderConfig{Version: 1, Rules: []PolicyBuilderRule{{
			Decision: decision, MatchMode: "all",
			Conditions: []PolicyBuilderCondition{cond("spans[_].attributes.command", "contains", "value", "danger", "string")},
		}}}
		if v := newBuilderEvaluator(cfg, "p").Evaluate(req).Verdict; v != want {
			t.Errorf("decision %q → %v, want %v", decision, v, want)
		}
	}
}
