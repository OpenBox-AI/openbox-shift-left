package codex

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// TestGatingHooksAreNeverAsync is the Codex twin of Claude Code's test of the
// same name, which Codex lacked.
//
// Codex applies a hook's control effects -- a deny, an updatedInput rewrite --
// ONLY for a synchronous handler. Register a gating hook with `async: true` and
// Codex still runs it, still reads its output, and then silently discards the
// decision: the tool proceeds, the audit records an enforcement that never
// happened, and nothing anywhere reports a problem. There is no error and no log
// line to notice.
//
// The installer does not set `async` today, and `commandHandler` has no field
// that could spell it. Both halves are asserted, because the structural half is
// the one that survives someone adding a field in good faith.
func TestGatingHooksAreNeverAsync(t *testing.T) {
	t.Run("the handler type cannot express async", func(t *testing.T) {
		rt := reflect.TypeOf(commandHandler{})
		for i := 0; i < rt.NumField(); i++ {
			name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
			if strings.EqualFold(name, "async") {
				t.Errorf("commandHandler declares an %q field; a gating hook that carries it has its "+
					"deny silently discarded by Codex while the tool runs anyway", name)
			}
		}
	})

	t.Run("no registered gating hook carries async", func(t *testing.T) {
		inst, hooksPath, _ := testInstaller(t)
		if err := inst.Install(CredentialRef{AgentID: testAgentID}); err != nil {
			t.Fatalf("install: %v", err)
		}
		hooks, _ := readHooks(t, hooksPath)["hooks"].(map[string]any)
		if hooks == nil {
			t.Fatal("no hooks object written")
		}
		// The gating classes: the two that can hold a decision, plus the prompt
		// gate, whose block/halt is equally a control effect.
		for _, ev := range []HookName{HookPreToolUse, HookPermissionRequest, HookUserPromptSubmit} {
			groups, ok := hooks[string(ev)].([]any)
			if !ok || len(groups) == 0 {
				t.Errorf("%s: no handler registered", ev)
				continue
			}
			raw, _ := json.Marshal(groups)
			if strings.Contains(string(raw), `"async"`) {
				t.Errorf("%s: a gating hook must be synchronous, or Codex discards its decision "+
					"while the tool runs: %s", ev, raw)
			}
		}
	})
}

// TestUpdatedInputCommandStaysAString pins the other half of the measured
// fail-open hazard.
//
// Codex requires `updatedInput.command` to be a JSON **string**. Emit anything
// else and it rejects the whole output — and a rejected PreToolUse output means
// the tool runs with the ORIGINAL, unredacted input. Reproduced live on
// 0.150.0-alpha.8: a gate that emitted `command` as an array logged
// `hook returned updatedInput without string field 'command'` and the original
// command executed.
//
// RedactToolInput already guarantees this by construction — it only engages when
// the field unmarshals as a non-empty string, and writes back a marshalled
// string. This asserts the guarantee rather than trusting it, across the shapes
// a Codex tool_input actually takes.
func TestUpdatedInputCommandStaysAString(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"bash string command", `{"command":"echo hi"}`},
		{"apply_patch body", `{"command":"*** Begin Patch\n*** Add File: a\n+b\n*** End Patch"}`},
		{"command alongside other keys", `{"command":"ls","timeout":30,"cwd":"/tmp"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := hookflow.RedactToolInput(json.RawMessage(tc.input), "REDACTED-BODY", contentFieldKeys)
			if len(out) == 0 {
				t.Fatalf("expected a rebuilt updatedInput for %s", tc.input)
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("updatedInput is not an object: %v (%s)", err, out)
			}
			raw, present := obj["command"]
			if !present {
				t.Fatalf("updatedInput dropped `command`; Codex fails the call open to the original input: %s", out)
			}
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Errorf("`command` must stay a JSON string or Codex discards the whole output and runs "+
					"the ORIGINAL command; got %s", raw)
			}
			if s != "REDACTED-BODY" {
				t.Errorf("`command` = %q, want the redacted body", s)
			}
		})
	}

	// The shapes that must produce NOTHING rather than a malformed rewrite: with
	// no rewrite emitted, the contract writes no `allow` either, so the call is
	// simply governed without one.
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"no command key", `{"path":"/etc/hosts"}`},
		{"command already an array", `{"command":["/bin/sh","-lc","echo hi"]}`},
		{"empty command", `{"command":""}`},
		{"not an object", `["a","b"]`},
	} {
		t.Run("refuses: "+tc.name, func(t *testing.T) {
			if out := hookflow.RedactToolInput(json.RawMessage(tc.input), "REDACTED-BODY", contentFieldKeys); len(out) != 0 {
				t.Errorf("expected no rewrite for %s, got %s", tc.input, out)
			}
		})
	}
}

// TestRenderNeverEmitsAllowWithoutAStringCommand closes the loop at the contract
// boundary: even if a caller handed Render a malformed updatedInput, the emitted
// line must never pair `allow` with a non-string command — that combination is
// the exact one that fails open.
func TestRenderNeverEmitsAllowWithoutAStringCommand(t *testing.T) {
	for _, bad := range []string{
		`{"command":["/bin/sh","-lc","x"]}`,
		`{"command":123}`,
		`{"command":null}`,
	} {
		line, applied := contract.Render("", "", json.RawMessage(bad))
		if applied != codexDecisionAllow {
			continue // did not take the rewrite branch at all; nothing to check
		}
		var o preToolUseOutput
		if err := json.Unmarshal(line, &o); err != nil {
			t.Fatalf("unmarshal rendered line: %v", err)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(o.HookSpecificOutput.UpdatedInput, &obj); err != nil {
			t.Errorf("rendered a non-object updatedInput alongside allow: %s", line)
			continue
		}
		var s string
		if err := json.Unmarshal(obj["command"], &s); err != nil {
			t.Errorf("rendered allow with a non-string command, which Codex rejects and then runs the "+
				"ORIGINAL command: %s", line)
		}
	}
}
