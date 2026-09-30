package muse

import (
	"bytes"
	"strings"
	"testing"

	providerspi "github.com/openbox-ai/openbox-shift-left/internal/provider"
)

func TestFailClosedDeniesEveryGatedEventWithOneStderrLine(t *testing.T) {
	for _, ev := range []HookName{HookUserPromptSubmit, HookPreToolUse, HookPermissionRequest, HookPreLLMCall} {
		var stderr bytes.Buffer
		if code := RunFailClosed(string(ev), &stderr); code != FaultExitCode {
			t.Errorf("%s: exit = %d, want %d", ev, code, FaultExitCode)
		}
		line := stderr.String()
		if strings.Count(line, "\n") != 1 || !strings.Contains(line, string(ev)) || !strings.HasSuffix(line, "\n") {
			t.Errorf("%s: stderr is not one line naming the event: %q", ev, line)
		}
	}
}

// The successor is registered on gated handlers only, but one that runs for
// anything else must not stop the developer working.
func TestFailClosedLeavesUngatedEventsAlone(t *testing.T) {
	for _, ev := range installedEvents() {
		if ev.Gated() {
			continue
		}
		var stderr bytes.Buffer
		if code := RunFailClosed(string(ev), &stderr); code != 0 || stderr.Len() != 0 {
			t.Errorf("%s: exit = %d, stderr = %q; an event that gates nothing must pass silently", ev, code, stderr.String())
		}
	}
}

// An unreadable successor must not read as permission.
func TestFailClosedRefusesAnUnknownOrMissingEvent(t *testing.T) {
	for _, ev := range []string{"", "nonsense", "pretooluse", "--fail-closed"} {
		var stderr bytes.Buffer
		if code := RunFailClosed(ev, &stderr); code != FaultExitCode || stderr.Len() == 0 {
			t.Errorf("%q: exit = %d, stderr = %q", ev, code, stderr.String())
		}
	}
}

func TestEngineIsAFailClosedRunner(t *testing.T) {
	var e providerspi.HookEngine = Engine{}
	fc, ok := e.(providerspi.FailClosedRunner)
	if !ok {
		t.Fatal("the engine does not implement FailClosedRunner")
	}
	var stderr bytes.Buffer
	if code := fc.RunFailClosed("PreToolUse", &stderr); code != FaultExitCode {
		t.Errorf("exit = %d", code)
	}
}

// If a successor's exit 2 turns out not to deny on some Muse release, the
// fallback is an exit 0 with the event's own schema-valid refusal on stdout.
// Nothing emits it today; these are the exact answers that fallback would
// write, so switching to it is a change to one function and not a new contract.
func TestFallbackRefusalShapesStaySchemaValid(t *testing.T) {
	golden := map[HookName]string{
		HookPreToolUse:        `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"OpenBox gate unavailable"}}`,
		HookUserPromptSubmit:  `{"decision":"block","reason":"OpenBox gate unavailable"}`,
		HookPermissionRequest: `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"OpenBox gate unavailable"}}}`,
		HookPreLLMCall:        `{"decision":"block","reason":"OpenBox gate unavailable"}`,
	}
	for ev, want := range golden {
		line, _ := contractFor(ev).Render("deny", "OpenBox gate unavailable", nil)
		if got := strings.TrimSpace(string(line)); got != want {
			t.Errorf("%s:\n got %s\nwant %s", ev, got, want)
		}
	}
}
