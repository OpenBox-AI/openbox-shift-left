package main

import (
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// fakeEngine is a HookEngine whose RunHook runs a test body.
type fakeEngine struct{ run func(event string) }

func (e fakeEngine) RunHook(event string, _ io.Reader, _ io.Writer, _ *log.Logger) { e.run(event) }
func (fakeEngine) Capabilities() []provider.Capability                             { return nil }
func (fakeEngine) HookCeilings() provider.HookCeiling                              { return provider.HookCeiling{} }

// faultEngine additionally tells runHook which exit code its host reads as
// "the hook failed", so a crash is never read as allow.
type faultEngine struct{ fakeEngine }

func (faultEngine) FaultExitCode(event string) int {
	if event == "PreToolUse" {
		return 7
	}
	return 0
}

func withHookEngine(t *testing.T, e provider.HookEngine) {
	t.Helper()
	prev := hookEngineFn
	hookEngineFn = func(string) (provider.HookEngine, error) { return e, nil }
	t.Cleanup(func() { hookEngineFn = prev })
}

// TestHookPanicExitsWithTheEnginesFaultCode a host that fails open on a crash
// must see a crash as a failure, or the gated call runs.
func TestHookPanicExitsWithTheEnginesFaultCode(t *testing.T) {
	isolateHome(t)
	withHookEngine(t, faultEngine{fakeEngine{run: func(string) { panic("boom") }}})
	a, _, errb := testApp(nil)
	if code := a.runHook([]string{"muse", "PreToolUse"}); code != 7 {
		t.Fatalf("gated panic exit = %d, want the engine's fault code 7", code)
	}
	if code := a.runHook([]string{"muse", "PostToolUse"}); code != exitOK {
		t.Fatalf("non-gated panic exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(errb.String(), "recovered from panic") {
		t.Errorf("the panic was not reported on stderr: %q", errb.String())
	}
}

// TestHookPanicWithoutFaultExiterStillExitsZero Claude Code and Codex read a
// non-zero exit differently, so an engine that does not opt in keeps exit 0.
func TestHookPanicWithoutFaultExiterStillExitsZero(t *testing.T) {
	isolateHome(t)
	withHookEngine(t, fakeEngine{run: func(string) { panic("boom") }})
	a, _, _ := testApp(nil)
	if code := a.runHook([]string{"codex", "PreToolUse"}); code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
}

// TestHookHomeFlagRelocatesTheStoreBeforeBind a host that clears the hook's
// environment can still reach a non-default OpenBox home through argv.
func TestHookHomeFlagRelocatesTheStoreBeforeBind(t *testing.T) {
	isolateHome(t)
	home := filepath.Join(t.TempDir(), "obhome")
	var seen string
	withHookEngine(t, fakeEngine{run: func(string) { seen, _ = devconfig.Home() }})
	a, _, _ := testApp(nil)
	if code := a.runHook([]string{"muse", "--home", home, "SessionStart"}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if seen != home {
		t.Fatalf("Home() inside the hook = %q, want %q", seen, home)
	}
}

// TestHookHomeFlagRefusesARelativePath a relative home would resolve against
// whatever cwd the host ran the hook in; on a gated event that is a fault.
func TestHookHomeFlagRefusesARelativePath(t *testing.T) {
	isolateHome(t)
	ran := false
	withHookEngine(t, faultEngine{fakeEngine{run: func(string) { ran = true }}})
	a, _, _ := testApp(nil)
	if code := a.runHook([]string{"muse", "--home", "rel/home", "PreToolUse"}); code != 7 {
		t.Fatalf("exit = %d, want the fault code", code)
	}
	if ran {
		t.Fatal("the engine ran with an unusable home")
	}
}
