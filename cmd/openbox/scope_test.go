package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
)

// TestEnforceOptOutRoundTrips tHE round-trip. The opt-out was silently un-
// appliable.
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

// TestAFlagThatMovedOrWasRemovedIsRefused. Every shim that used to explain
// where a flag went is gone with the flag, so these are plain parse refusals
// now. What has to survive is the refusal itself plus a usage block that points
// somewhere useful -- a flag silently accepted and ignored would read as a knob
// the operator still has.
func TestAFlagThatMovedOrWasRemovedIsRefused(t *testing.T) {
	for _, flag := range []string{
		// Moved to `openbox auth`.
		"--org", "--agent-name", "--icon", "--description", "--base-url", "--backend-url", "--force",
		// Removed outright.
		"--secret-backend", "--client-id", "--managed-enable", "--local-hooks", "--scope",
		// Collapsed into the one always-on posture.
		"--enforce", "--no-enforce", "--install-git-hook", "--full", "--dry-run",
		// Removal is its own command now.
		"--remove-all", "--remove-gateway", "--remove-telemetry", "--remove-transport",
		// Lanes are derived from the provider.
		"--gateway", "--telemetry", "--transport", "--lane-verbose", "--force-restore",
	} {
		a, _, errb := testApp(nil)
		if code := a.run([]string{"init", "--provider", "claude-code", flag, "x"}); code == exitOK {
			t.Errorf("init accepted %s", flag)
			continue
		}
		if !strings.Contains(errb.String(), "not defined") {
			t.Errorf("%s was refused for the wrong reason: %s", flag, errb.String())
		}
	}
	// The refusal prints the usage block, which has to name where the two
	// commands that took this work over actually live.
	a, _, errb := testApp(nil)
	a.run([]string{"init", "--provider", "claude-code", "--org", "x"})
	for _, want := range []string{"openbox auth", "openbox uninstall", "OPENBOX_ENFORCE=false"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("the usage shown on refusal does not mention %q:\n%s", want, errb.String())
		}
	}
}

func TestCodexInitSaysEverySessionIsGoverned(t *testing.T) {
	isolateHome(t)
	seedCredentials(t, "codex")
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
	if !strings.Contains(out.String(), "mode: ENFORCE") {
		t.Errorf("the install does not state the posture it left the machine in:\n%s", out.String())
	}
}

// TestPlainReInitDoesNotRevertAnEnforceOptOut tHE regression test for the bug
// that shipped past every single-invocation test: a plain `init` re-run
// silently reverted a deliberate `--enforce=false` back to true, because the
// flag defaults to true and its value was assigned to o.Enforce
// unconditionally; so every run wrote enforce:true whether the user had asked
// for it or not.
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
