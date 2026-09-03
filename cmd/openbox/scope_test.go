package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
)

// TestEnforceOptOutRoundTrips tHE round-trip. The opt-out was silently un-
// appliable.
func TestEnforceOptOutRoundTrips(t *testing.T) {
	for _, flag := range []string{"--enforce=false", "--no-enforce"} {
		t.Run(flag, func(t *testing.T) {
			home := isolateHome(t)
			seedCredentials(t)
			a, _, errb := testApp(nil)
			if code := a.run([]string{"init", "--provider", "claude-code", flag}); code != exitOK {
				t.Fatalf("exit = %d; stderr=%q", code, errb.String())
			}

			// The field must actually be IN the file; not dropped by omitempty.
			raw, err := os.ReadFile(filepath.Join(home, "dev.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"enforce": false`) {
				t.Fatalf("the opt-out was not persisted (this is the omitempty bug):\n%s", raw)
			}
			// Re-reading it must still be false.
			cfg := readDevJSON(t, home)
			if cfg.Enforce == nil {
				t.Fatal("enforce came back absent after being written false")
			}
			if *cfg.Enforce {
				t.Error("enforce came back true after being written false")
			}
			// And the resolver must agree, despite the default being on.
			if devconfig.ResolveEnforce() {
				t.Error("ResolveEnforce() reports enforcing after an explicit opt-out")
			}
		})
	}
}

func TestEnforceAndNoEnforceAreMutuallyExclusive(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code", "--enforce", "--no-enforce"}); code != exitError {
		t.Fatalf("exit = %d, want an error", code)
	}
	if !strings.Contains(errb.String(), "mutually exclusive") {
		t.Errorf("error = %q", errb.String())
	}
}

// TestTurningEnforceOffIsAnnouncedEvenFromAnAbsentField turning enforcement
// off is announced.
func TestTurningEnforceOffIsAnnouncedEvenFromAnAbsentField(t *testing.T) {
	home := isolateHome(t)
	seedCredentials(t)
	if cfg := readDevJSON(t, home); cfg.Enforce != nil {
		t.Fatalf("precondition: enforce should be absent, got %v", cfg.Enforce)
	}
	a, out, _ := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code", "--enforce=false"}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "turning ENFORCE off") {
		t.Errorf("a downgrade from the effective posture must be announced:\n%s", out.String())
	}
}

// TestEnforceEnvOverride the env override still wins over the config, in both
// directions.
func TestEnforceEnvOverride(t *testing.T) {
	home := isolateHome(t)
	f := false
	if err := devconfig.WriteConfig(filepath.Join(home, "dev.json"), devconfig.Update{Enforce: &f}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(devconfig.EnvEnforce, "1")
	if !devconfig.ResolveEnforce() {
		t.Error("OPENBOX_ENFORCE=1 must override a config false")
	}
	t.Setenv(devconfig.EnvEnforce, "0")
	if devconfig.ResolveEnforce() {
		t.Error("OPENBOX_ENFORCE=0 must override the default on")
	}
}

// TestEveryMovedFlagErrorsNamingAuth silent acceptance of a flag that no
// longer does anything is worse than removing it loudly: a script passing
// --base-url would keep exiting 0 while the URL went nowhere.
func TestEveryMovedFlagErrorsNamingAuth(t *testing.T) {
	for _, tc := range []struct{ flag, value string }{
		{"--org", "acme"},
		{"--agent-name", "dev-x"},
		{"--icon", "🤖"},
		{"--description", "an agent"},
		{"--base-url", "https://core.internal"},
		{"--backend-url", "https://api.internal"}} {
		t.Run(tc.flag, func(t *testing.T) {
			isolateHome(t)
			seedCredentials(t)
			a, _, errb := testApp(nil)
			code := a.run([]string{"init", "--provider", "claude-code", tc.flag, tc.value})
			if code != exitError {
				t.Fatalf("exit = %d, want an error; a moved flag must not be silently accepted", code)
			}
			s := errb.String()
			if !strings.Contains(s, tc.flag) {
				t.Errorf("error should name %s:\n%s", tc.flag, s)
			}
			if !strings.Contains(s, "openbox auth") {
				t.Errorf("error should point at `openbox auth`:\n%s", s)
			}
		})
	}
}

func TestMovedForceFlagErrors(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code", "--force"}); code != exitError {
		t.Fatalf("exit = %d, want an error", code)
	}
	if !strings.Contains(errb.String(), "openbox auth") {
		t.Errorf("error = %q", errb.String())
	}
}

func TestRemovedFlagsError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantText string
	}{
		{"client-id", []string{"--client-id", "x"}, "no control-plane call"},
		{"managed-enable", []string{"--managed-enable"}, "managed-settings"}} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			seedCredentials(t)
			a, _, errb := testApp(nil)
			args := append([]string{"init", "--provider", "claude-code"}, tc.args...)
			if code := a.run(args); code != exitError {
				t.Fatalf("exit = %d, want an error", code)
			}
			if !strings.Contains(errb.String(), tc.wantText) {
				t.Errorf("error should explain the removal (%q):\n%s", tc.wantText, errb.String())
			}
		})
	}
}

// TestCodexInitSaysEverySessionIsGoverned. Codex hooks were always user-wide;
// now every provider is, so this is the shared story rather than an exception.
func TestCodexInitSaysEverySessionIsGoverned(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex-home"))
	a, out, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "codex"}); code != exitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "EVERY CODEX SESSION") {
		t.Errorf("the closing report must state the real coverage:\n%s", s)
	}
}

// TestPrintGovernedScopeStatesTheTruth this string is the one place a user
// learns the truth about coverage, so its content is pinned rather than left
// to drift. The truth inverted with this phase: one install governs every
// session on this machine, and it takes effect without a restart.
func TestPrintGovernedScopeStatesTheTruth(t *testing.T) {
	isolateHome(t)
	a, out, _ := testApp(nil)
	a.printGovernedScope(optionsFor("claude-code", ""))
	s := out.String()
	for _, want := range []string{"EVERY SESSION", "IMMEDIATELY", "nothing to restart"} {
		if !strings.Contains(s, want) {
			t.Errorf("the scope statement does not say %q:\n%s", want, s)
		}
	}
	if !strings.Contains(s, providers.ClaudeUserSettingsPath()) {
		t.Errorf("must name the file that changed:\n%s", s)
	}
	// The old story sent the reader to an administrator to turn governance on.
	// It is on; saying otherwise invites a hunt for a gap that is not there.
	for _, banned := range []string{"NOTHING YET", "enabledPlugins", "pending", "--scope"} {
		if strings.Contains(s, banned) {
			t.Errorf("the scope statement still implies activation is pending (%q):\n%s", banned, s)
		}
	}
}

func optionsFor(providerName, projectDir string) devinit.Options {
	return devinit.Options{Provider: providerName, ProjectDir: projectDir}
}

// TestInstallOutputDoesNotUnderstateCoverage. The failure direction reversed
// with this phase. It used to be over-claiming; now the install really does
// govern every session, so the danger is a leftover sentence telling somebody
// that one directory is covered or that an administrator has to finish the job.
func TestInstallOutputDoesNotUnderstateCoverage(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	a, out, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	lower := strings.ToLower(out.String())
	for _, banned := range []string{"this project only", "nothing yet", "activation is pending", "--scope"} {
		if strings.Contains(lower, strings.ToLower(banned)) {
			t.Errorf("the install understates what it governs (%q):\n%s", banned, out.String())
		}
	}
	if !strings.Contains(out.String(), "Nothing to run") {
		t.Errorf("the install should still state that nothing needs to be kept running:\n%s", out.String())
	}
}

// TestPlainReInitDoesNotRevertAnEnforceOptOut tHE regression test for the bug
// that shipped past every single-invocation test: a plain `init` re-run
// silently reverted a deliberate `--enforce=false` back to true, because the
// flag defaults to true and its value was assigned to o.Enforce
// unconditionally; so every run wrote enforce:true whether the user had asked
// for it or not.
func TestPlainReInitDoesNotRevertAnEnforceOptOut(t *testing.T) {
	home := isolateHome(t)
	seedCredentials(t)

	run := func(t *testing.T, args ...string) {
		t.Helper()
		a, _, errb := testApp(nil)
		full := append([]string{"init", "--provider", "claude-code"}, args...)
		if code := a.run(full); code != exitOK {
			t.Fatalf("%v exit = %d; stderr=%q", full, code, errb.String())
		}
	}

	run(t, "--enforce=false")
	if cfg := readDevJSON(t, home); cfg.Enforce == nil || *cfg.Enforce {
		t.Fatalf("precondition failed: opt-out not stored, got %v", cfg.Enforce)
	}

	run(t)
	cfg := readDevJSON(t, home)
	if cfg.Enforce == nil {
		t.Fatal("the opt-out was erased by a plain re-run; an absent field re-defaults to ON")
	}
	if *cfg.Enforce {
		t.Error("a plain re-run silently turned enforcement back on; the opt-out must persist ")
	}
	if devconfig.ResolveEnforce() {
		t.Error("ResolveEnforce() reports enforcing after an opt-out survived a re-run")
	}

	run(t, "--enforce")
	if cfg := readDevJSON(t, home); cfg.Enforce == nil || !*cfg.Enforce {
		t.Errorf("--enforce did not turn it back on, got %v", cfg.Enforce)
	}
}

// TestBareInitEnforcesWithoutWritingTheField a bare install on a machine with
// no prior config must still enforce; via the resolver default, not by writing
// a literal true.
func TestBareInitEnforcesWithoutWritingTheField(t *testing.T) {
	home := isolateHome(t)
	seedCredentials(t)
	a, out, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	if cfg := readDevJSON(t, home); cfg.Enforce != nil {
		t.Errorf("a bare init wrote enforce=%v; it must say nothing so an opt-out can survive", *cfg.Enforce)
	}
	if !devconfig.ResolveEnforce() {
		t.Error("a bare init must resolve to ENFORCE, via the default")
	}
	if !strings.Contains(out.String(), "ENFORCE") {
		t.Errorf("the install must report the enforcing posture:\n%s", out.String())
	}
}
