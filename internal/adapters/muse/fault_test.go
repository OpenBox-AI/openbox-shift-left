package muse

import (
	"strings"
	"testing"
	"time"
)

// Muse reads exit 0 with no usable answer as allow, so a crash on a gated event
// must reach `openbox hook`'s recover (which exits FaultExitCode) instead of
// being swallowed into an exit 0.

func TestFaultExitCodeIsNonZeroOnlyForGatedEvents(t *testing.T) {
	e := Engine{}
	for _, gated := range []string{"UserPromptSubmit", "PreToolUse", "PermissionRequest", "PreLLMCall"} {
		if got := e.FaultExitCode(gated); got != FaultExitCode || got == 0 {
			t.Errorf("FaultExitCode(%s) = %d, want %d", gated, got, FaultExitCode)
		}
	}
	for _, other := range []string{"SessionStart", "PostToolUse", "PostToolUseFailure", "PostLLMCall", "SessionEnd", "Stop", "SubagentStart", "Notification", "flush", "", "nonsense"} {
		if got := e.FaultExitCode(other); got != 0 {
			t.Errorf("FaultExitCode(%s) = %d, want 0", other, got)
		}
	}
}

// panics runs f and reports the value it panicked with.
func panics(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}

func TestPanicOnAGatedEventIsNotSwallowed(t *testing.T) {
	setHookEnv(t)
	prev := nowFn
	nowFn = func() time.Time { panic("mapper fault") }
	t.Cleanup(func() { nowFn = prev })

	for _, tc := range []struct{ hook, file string }{
		{"UserPromptSubmit", "user-prompt-submit"},
		{"PreToolUse", "pre-tool-use-bash"},
		{"PermissionRequest", "permission-request"},
		{"PreLLMCall", "pre-llm-call"},
	} {
		got := panics(func() { runHook(t, tc.hook, fixture(t, tc.file, "s-fault")) })
		if got != "mapper fault" {
			t.Errorf("%s: panic = %v; a recovered gated panic exits 0, which Muse reads as an allow", tc.hook, got)
		}
	}
}

func TestPanicOnANonGatedEventIsLoggedAndSwallowed(t *testing.T) {
	setHookEnv(t)
	prev := nowFn
	nowFn = func() time.Time { panic("mapper fault") }
	t.Cleanup(func() { nowFn = prev })

	for _, tc := range []struct{ hook, file string }{
		{"SessionStart", "session-start-startup"},
		{"PostToolUse", "post-tool-use"},
		{"PostLLMCall", "post-llm-call"},
		{"SessionEnd", "session-end"},
	} {
		var stderr string
		if got := panics(func() { _, stderr = runHook(t, tc.hook, fixture(t, tc.file, "s-fault")) }); got != nil {
			t.Errorf("%s: panicked out of RunHook: %v", tc.hook, got)
		}
		if !strings.Contains(stderr, "recovered: mapper fault") {
			t.Errorf("%s: the recovery was not logged: %q", tc.hook, stderr)
		}
	}
}
