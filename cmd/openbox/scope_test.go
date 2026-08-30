package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
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
		{"--backend-url", "https://api.internal"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			isolateHome(t)
			seedCredentials(t)
			a, _, errb := testApp(nil)
			code := a.run([]string{"init", "--provider", "claude-code", tc.flag, tc.value})
			if code != exitError {
				t.Fatalf("exit = %d, want an error — a moved flag must not be silently accepted", code)
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
		{"managed-enable", []string{"--managed-enable"}, "managed-settings"},
	} {
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

// --local-hooks keeps working for one release, warning once, because project
// scope is now the default and most callers can just drop it.
func TestLocalHooksIsADeprecatedAliasForScopeLocal(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	dir := t.TempDir()
	a, out, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code", "--local-hooks", dir}); code != exitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "deprecated") {
		t.Errorf("--local-hooks should warn to stderr:\n%s", errb.String())
	}
	if strings.Contains(out.String(), "deprecated") {
		t.Errorf("the deprecation warning must not go to stdout:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "settings.local.json")); err != nil {
		t.Errorf("--local-hooks should still merge project hooks: %v", err)
	}
}

func TestLocalHooksAndScopeTogetherIsAnError(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code", "--local-hooks", ".", "--scope", "local"}); code != exitError {
		t.Fatalf("exit = %d, want an error", code)
	}
	if !strings.Contains(errb.String(), "cannot both") {
		t.Errorf("error = %q", errb.String())
	}
}

func TestInvalidScopeIsRejected(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code", "--scope", "everywhere"}); code != exitError {
		t.Fatalf("exit = %d, want an error", code)
	}
	if !strings.Contains(errb.String(), "not valid") {
		t.Errorf("error = %q", errb.String())
	}
}

// TestCodexRejectsLocalScope codex hooks are user-wide; a repo-level
// .codex/hooks.json is a location its installer deliberately does not touch.
// So project scope errors rather than silently governing everything.
func TestCodexRejectsLocalScope(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "codex", "--scope", "local"}); code != exitError {
		t.Fatalf("exit = %d, want an error", code)
	}
	s := errb.String()
	if !strings.Contains(s, "user-wide") {
		t.Errorf("the error should explain WHY project scope is unavailable:\n%s", s)
	}
	if !strings.Contains(s, "claude-code") {
		t.Errorf("the error should name the provider that does support it:\n%s", s)
	}
}

// TestCodexBareInitResolvesGlobalAndSaysSo a bare codex install resolves to
// global and says SO. Silently governing every Codex session when the user
// asked for one project would over-deliver governance without consent.
func TestCodexBareInitResolvesGlobalAndSaysSo(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex-home"))
	a, out, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "codex"}); code != exitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "GLOBAL scope") {
		t.Errorf("an inferred scope must be stated:\n%s", s)
	}
	if !strings.Contains(s, "EVERY CODEX SESSION") {
		t.Errorf("the closing report must state the real coverage:\n%s", s)
	}
}

// TestPrintGovernedScopeStatesTheTruth this string is the one place a user
// learns the truth about coverage, so its content is pinned rather than left
// to drift.
func TestPrintGovernedScopeStatesTheTruth(t *testing.T) {
	t.Run("local names the directory and the gap", func(t *testing.T) {
		a, out, _ := testApp(nil)
		a.printGovernedScope(optionsFor("claude-code", "/tmp/my-project"), scopeLocal)
		s := out.String()
		if !strings.Contains(s, "/tmp/my-project") {
			t.Errorf("must name the governed directory:\n%s", s)
		}
		if !strings.Contains(s, "not governed") || !strings.Contains(s, "absence of events is not evidence") {
			t.Errorf("must state what is NOT covered:\n%s", s)
		}
		if !strings.Contains(s, "do not commit") {
			t.Errorf("must warn against committing the settings file:\n%s", s)
		}
		for _, banned := range []string{"ambient", "every project", "machine-wide"} {
			if strings.Contains(strings.ToLower(s), banned) {
				t.Errorf("project scope must not imply broader coverage (%q):\n%s", banned, s)
			}
		}
	})

	t.Run("global says nothing is governed yet", func(t *testing.T) {
		a, out, _ := testApp(nil)
		a.printGovernedScope(optionsFor("claude-code", ""), scopeGlobal)
		s := out.String()
		if !strings.Contains(s, "NOTHING YET") {
			t.Errorf("global scope governs nothing until managed settings land:\n%s", s)
		}
		if !strings.Contains(s, "enabledPlugins") {
			t.Errorf("must print the snippet it cannot apply:\n%s", s)
		}
	})
}

func optionsFor(providerName, projectDir string) devinit.Options {
	return devinit.Options{Provider: providerName, ProjectDir: projectDir}
}

// TestNoInstallOutputClaimsAmbientCoverageAfterAProjectScopedInstall the whole
// install output must not overstate coverage, not just the scope block.
func TestNoInstallOutputClaimsAmbientCoverageAfterAProjectScopedInstall(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	a, out, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	lower := strings.ToLower(out.String())
	for _, banned := range []string{"governance is ambient", "every project", "machine-wide", "all sessions"} {
		if strings.Contains(lower, banned) {
			t.Errorf("a project-scoped install must not imply broader coverage (%q):\n%s", banned, out.String())
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
		full := append([]string{"init", "--provider", "claude-code", "--scope", "global"}, args...)
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
		t.Fatal("the opt-out was erased by a plain re-run — an absent field re-defaults to ON")
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
	if code := a.run([]string{"init", "--provider", "claude-code", "--scope", "global"}); code != exitOK {
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
