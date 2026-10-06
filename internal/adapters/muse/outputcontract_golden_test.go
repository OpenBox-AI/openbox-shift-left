package muse

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// Muse discards an answer it does not accept and reads that as allow, so each
// gated event has a closed set of keys and these goldens are that set, spelled
// out. A key added to a contract has to be added here on purpose.

type contractCase struct {
	name     string
	contract hookflow.OutputContract
	event    HookName
	// topKeys and hsoKeys are the only keys the answer may carry.
	topKeys []string
	hsoKeys []string
}

var contractCases = []contractCase{
	{"PreToolUse", contract, HookPreToolUse,
		[]string{"hookSpecificOutput"},
		[]string{"hookEventName", "permissionDecision", "permissionDecisionReason", "updatedInput"}},
	{"UserPromptSubmit", promptContract, HookUserPromptSubmit,
		[]string{"decision", "reason"}, nil},
	{"PermissionRequest", permissionContract, HookPermissionRequest,
		[]string{"hookSpecificOutput"},
		[]string{"hookEventName", "decision"}},
	{"PreLLMCall", llmContract, HookPreLLMCall,
		[]string{"decision", "reason"}, nil},
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func subset(got, allowed []string) []string {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	var extra []string
	for _, g := range got {
		if !ok[g] {
			extra = append(extra, g)
		}
	}
	return extra
}

// allStrings returns every key and string value anywhere in a JSON document.
func allStrings(v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			out = append(out, k)
			out = append(out, allStrings(e)...)
		}
	case []any:
		for _, e := range t {
			out = append(out, allStrings(e)...)
		}
	case string:
		out = append(out, t)
	}
	return out
}

func decodeAnswer(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("answer is not a JSON object: %v (%q)", err, line)
	}
	return m
}

func TestContractGoldens(t *testing.T) {
	const reason = "OpenBox governance: nope"
	updated := json.RawMessage(`{"file_path":"/tmp/a","content":"[scrubbed]"}`)

	tests := []struct {
		contract    string
		decision    string
		updated     json.RawMessage
		want        string
		wantApplied string
	}{
		{"PreToolUse", "", nil, "", ""},
		{"PreToolUse", hookflow.DecisionDeny, nil,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"OpenBox governance: nope"}}`, "deny"},
		{"PreToolUse", hookflow.DecisionHalt, nil,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"OpenBox governance: nope"}}`, hookflow.DecisionHalt},
		{"PreToolUse", "", updated,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"file_path":"/tmp/a","content":"[scrubbed]"}}}`, applyUpdated},

		{"UserPromptSubmit", "", nil, "", ""},
		{"UserPromptSubmit", hookflow.DecisionDeny, nil,
			`{"decision":"block","reason":"OpenBox governance: nope"}`, "block"},
		{"UserPromptSubmit", hookflow.DecisionHalt, nil,
			`{"decision":"block","reason":"OpenBox governance: nope"}`, hookflow.DecisionHalt},

		{"PermissionRequest", "", nil, "", ""},
		{"PermissionRequest", hookflow.DecisionDeny, nil,
			`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"OpenBox governance: nope"}}}`, "deny"},
		{"PermissionRequest", hookflow.DecisionHalt, nil,
			`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"OpenBox governance: nope"}}}`, hookflow.DecisionHalt},

		{"PreLLMCall", "", nil, "", ""},
		{"PreLLMCall", hookflow.DecisionDeny, nil,
			`{"decision":"block","reason":"OpenBox governance: nope"}`, "block"},
		{"PreLLMCall", hookflow.DecisionHalt, nil,
			`{"decision":"block","reason":"OpenBox governance: nope"}`, hookflow.DecisionHalt},
	}
	byName := map[string]contractCase{}
	for _, c := range contractCases {
		byName[c.name] = c
	}
	for _, tc := range tests {
		c := byName[tc.contract]
		t.Run(tc.contract+"/"+tc.decision+"/"+string(tc.updated), func(t *testing.T) {
			line, applied := c.contract.Render(tc.decision, reason, tc.updated)
			if string(line) != tc.want {
				t.Fatalf("answer = %s\nwant    %s", line, tc.want)
			}
			if applied != tc.wantApplied {
				t.Fatalf("applied = %q, want %q", applied, tc.wantApplied)
			}
		})
	}
}

// rejected are the keys and values Muse either discards the whole answer for
// (discarded means allow) or reads as a grant.
var rejected = map[string]bool{
	"continue": true, "stopReason": true, "suppressOutput": true, "allow": true,
	"ask": true, "interrupt": true, "updatedPermissions": true,
}

// Every contract is held to its key set for every decision it can be asked to
// render, REQUIRE_APPROVAL's included, and never carries the two shapes Muse
// turns into an allow or rejects outright.
func TestContractKeySetsAreClosed(t *testing.T) {
	updated := json.RawMessage(`{"command":"echo [scrubbed]"}`)
	for _, c := range contractCases {
		decisions := []string{"", hookflow.DecisionDeny, hookflow.DecisionHalt, c.contract.ApprovalDecision()}
		for _, d := range decisions {
			for _, up := range []json.RawMessage{nil, updated} {
				line, _ := c.contract.Render(d, "because", up)
				if len(line) == 0 {
					continue
				}
				m := decodeAnswer(t, line)
				if extra := subset(keysOf(m), c.topKeys); len(extra) > 0 {
					t.Errorf("%s/%q: top-level keys outside the contract: %v", c.name, d, extra)
				}
				if hso, ok := m["hookSpecificOutput"].(map[string]any); ok {
					if extra := subset(keysOf(hso), c.hsoKeys); len(extra) > 0 {
						t.Errorf("%s/%q: hookSpecificOutput keys outside the contract: %v", c.name, d, extra)
					}
				}
				for _, s := range allStrings(m) {
					if rejected[s] {
						t.Errorf("%s/%q: rendered %q, which Muse rejects or reads as an allow: %s", c.name, d, s, line)
					}
				}
			}
		}
	}
}

// REQUIRE_APPROVAL renders a refusal on every Muse contract: the gate never
// renders an ask, and ApprovalDecision is the literal the engine maps it to.
func TestApprovalDecisionDenies(t *testing.T) {
	for _, c := range contractCases {
		if got := c.contract.ApprovalDecision(); got != hookflow.DecisionDeny {
			t.Errorf("%s: ApprovalDecision() = %q, want %q", c.name, got, hookflow.DecisionDeny)
		}
		line, _ := c.contract.Render(c.contract.ApprovalDecision(), "needs a human", nil)
		if len(line) == 0 {
			t.Errorf("%s: REQUIRE_APPROVAL rendered nothing, which Muse reads as a proceed", c.name)
		}
	}
}

// An updatedInput on a refusal or a proceed-less event is never attached to a
// deny, and a proceed on PermissionRequest never writes an allow.
func TestPermissionRequestProceedWritesNothing(t *testing.T) {
	line, applied := permissionContract.Render("", "", json.RawMessage(`{"command":"x"}`))
	if len(line) != 0 || applied != "" {
		t.Fatalf("proceed rendered %q (%q); PermissionRequest is deny-or-nothing", line, applied)
	}
}

// An updatedInput that is not a JSON object is refused rather than emitted:
// Muse would reject the answer and run the call with the original input.
func TestUpdatedInputMustBeAnObject(t *testing.T) {
	for _, bad := range []string{`"str"`, `[1]`, `null`, `not json`, ``} {
		if line, _ := contract.Render("", "", json.RawMessage(bad)); len(line) != 0 {
			t.Errorf("updatedInput %q rendered %s", bad, line)
		}
	}
}

// A refusal with no text would be rejected by Muse and so read as an allow.
func TestRefusalAlwaysCarriesAReason(t *testing.T) {
	for _, c := range contractCases {
		line, _ := c.contract.Render(hookflow.DecisionDeny, "", nil)
		if !bytes.Contains(line, []byte(fallbackReason)) {
			t.Errorf("%s: an empty reason rendered %s, want the generic reason", c.name, line)
		}
	}
}

func reasonOf(t *testing.T, c contractCase, line []byte) string {
	t.Helper()
	m := decodeAnswer(t, line)
	if r, ok := m["reason"].(string); ok {
		return r
	}
	hso := m["hookSpecificOutput"].(map[string]any)
	if r, ok := hso["permissionDecisionReason"].(string); ok {
		return r
	}
	return hso["decision"].(map[string]any)["message"].(string)
}

// Caps count bytes and cut runes: a CJK reason is three bytes a rune, so a
// rune-counted cap would triple the limit.
func TestReasonIsCappedInBytesAndCutOnARuneBoundary(t *testing.T) {
	cjk := strings.Repeat("漢", 10000) // 30000 bytes
	for _, c := range contractCases {
		line, _ := c.contract.Render(hookflow.DecisionDeny, cjk, nil)
		r := reasonOf(t, c, line)
		if len(r) > maxReasonBytes {
			t.Errorf("%s: reason is %d bytes, cap is %d", c.name, len(r), maxReasonBytes)
		}
		if !utf8.ValidString(r) {
			t.Errorf("%s: the cap split a rune", c.name)
		}
		if len(r) < maxReasonBytes-3 {
			t.Errorf("%s: reason cut to %d bytes, far under the cap", c.name, len(r))
		}
	}
}

// The reason cap is on the raw text, but the answer is what Muse bounds: JSON
// escaping can expand a byte six-fold, so the whole answer is held under 16 KiB
// by shrinking the reason, never by sending an over-long line Muse would reject.
func TestAnswerStaysUnderTheStdoutCap(t *testing.T) {
	for _, nasty := range []string{strings.Repeat("\x01", 20000), strings.Repeat("<", 20000), strings.Repeat(" ", 20000)} {
		for _, c := range contractCases {
			line, _ := c.contract.Render(hookflow.DecisionDeny, nasty, nil)
			if len(line) == 0 {
				t.Fatalf("%s: a long reason rendered nothing", c.name)
			}
			if len(line)+1 >= maxStdoutBytes {
				t.Errorf("%s: answer is %d bytes, Muse fails an output of %d or more", c.name, len(line)+1, maxStdoutBytes)
			}
			if !json.Valid(line) {
				t.Errorf("%s: answer is not valid JSON", c.name)
			}
		}
	}
}

func TestSystemMessageIsCappedInBytes(t *testing.T) {
	cjk := strings.Repeat("漢", 5000)
	in, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{"hookEventName": "UserPromptSubmit", "additionalContext": cjk},
		"systemMessage":      cjk,
	})
	out := capFindingsOutput(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	sm := m["systemMessage"].(string)
	if len(sm) > maxSystemMessageBytes || !utf8.ValidString(sm) {
		t.Fatalf("systemMessage is %d bytes (valid utf8 %v), cap is %d", len(sm), utf8.ValidString(sm), maxSystemMessageBytes)
	}
	ac := m["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
	if len(ac) > maxSystemMessageBytes {
		t.Fatalf("additionalContext is %d bytes", len(ac))
	}
}
