package main

import (
	"io"
	"log"
	"strings"
	"testing"

	muse "github.com/openbox-ai/openbox-shift-left/internal/adapters/muse"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// panickingMuse is the real Muse engine, so its own FaultExitCode is what
// runHook asks, with a RunHook that crashes: the crash runHook's recover
// converts into an exit code.
type panickingMuse struct{ muse.Engine }

func (panickingMuse) RunHook(string, io.Reader, io.Writer, *log.Logger) { panic("boom") }

// Muse reads exit 0 with no usable answer as allow, so a crash on a gated event
// has to leave `openbox hook muse` non-zero, and a crash that gates nothing has
// to stay 0.
func TestMuseGatedCrashExitsNonZero(t *testing.T) {
	isolateHome(t)
	withHookEngine(t, panickingMuse{})
	for _, event := range []string{"UserPromptSubmit", "PreToolUse", "PermissionRequest", "PreLLMCall"} {
		a, _, errb := testApp(nil)
		if code := a.runHook([]string{"muse", event}); code != muse.FaultExitCode || code == exitOK {
			t.Errorf("%s: crash exit = %d, want %d", event, code, muse.FaultExitCode)
		}
		if !strings.Contains(errb.String(), "recovered from panic") {
			t.Errorf("%s: the crash was not reported on stderr: %q", event, errb.String())
		}
	}
	for _, event := range []string{"SessionStart", "PostToolUse", "PostLLMCall", "SessionEnd"} {
		a, _, _ := testApp(nil)
		if code := a.runHook([]string{"muse", event}); code != exitOK {
			t.Errorf("%s: crash exit = %d, want 0 (it gates nothing)", event, code)
		}
	}
}

// A malformed --home on a gated event is a fault too: the engine never runs.
func TestMuseGatedBadHomeExitsNonZero(t *testing.T) {
	isolateHome(t)
	ran := false
	withHookEngine(t, faultTracker{panickingMuse{}, &ran})
	a, _, _ := testApp(nil)
	if code := a.runHook([]string{"muse", "--home", "rel/home", "PreLLMCall"}); code != muse.FaultExitCode {
		t.Fatalf("exit = %d, want %d", code, muse.FaultExitCode)
	}
	if ran {
		t.Fatal("the engine ran with an unusable home")
	}
}

type faultTracker struct {
	panickingMuse
	ran *bool
}

func (f faultTracker) RunHook(string, io.Reader, io.Writer, *log.Logger) { *f.ran = true }

var _ provider.FaultExiter = panickingMuse{}
