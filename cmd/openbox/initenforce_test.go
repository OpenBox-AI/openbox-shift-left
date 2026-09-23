package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// TestInitWritesNoEnforceKeyEverAgain is what makes dropping --enforce safe
// rather than a behaviour change.
//
// A flag that defaults to true cannot express "said nothing". With no CLI
// writer, o.Enforce stays nil, Update.Enforce stays nil, setBoolPtr no-ops, the
// key is never written, and ResolveEnforce reads its default of true. Writing
// the key instead would look identical on a fresh machine and differ on every
// machine that had ever opted out.
//
// The SECOND invocation is asserted explicitly. CLAUDE.md records fifteen green
// tests that missed exactly this class of read/write defect because each ran
// init once.
func TestInitWritesNoEnforceKeyEverAgain(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)

	devPath, err := devconfig.DevConfigWritePath()
	if err != nil {
		t.Fatal(err)
	}

	for _, run := range []string{"first", "second"} {
		a, _, errb := testApp(nil)
		if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
			t.Fatalf("%s init exit = %d; stderr=%q", run, code, errb.String())
		}
		raw, err := os.ReadFile(devPath)
		if err != nil {
			t.Fatalf("%s run: read %s: %v", run, devPath, err)
		}
		if strings.Contains(string(raw), `"enforce"`) {
			t.Errorf("%s run wrote an enforce key, so a later opt-out cannot be told from silence:\n%s",
				run, raw)
		}
		if !devconfig.ResolveEnforce() {
			t.Errorf("%s run: ResolveEnforce() = false with no key present; the default must be ON", run)
		}
		// The one posture field init does write unconditionally, which is why the
		// env override is its only opt-out.
		if !strings.Contains(string(raw), `"install_git_hook"`) {
			t.Errorf("%s run did not persist install_git_hook:\n%s", run, raw)
		}
		if !devconfig.ResolveInstallGitHook() {
			t.Errorf("%s run: the commit-trailer hook is not enabled by default", run)
		}
	}
}

// TestTheEnvOverrideIsTheOnlyEnforceOptOut. The flag is gone, so the escape
// hatch has to be the env var — and it must work without writing the key,
// because a written key would outlive the variable.
func TestTheEnvOverrideIsTheOnlyEnforceOptOut(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	t.Setenv(devconfig.EnvEnforce, "false")

	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	if devconfig.ResolveEnforce() {
		t.Error("OPENBOX_ENFORCE=false did not turn enforcement off")
	}
	devPath, err := devconfig.DevConfigWritePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(devPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"enforce"`) {
		t.Errorf("the env override was persisted into the config, so it would outlive the variable:\n%s", raw)
	}
}

// TestInitTakesOnlyProvider. Every other flag is gone, and a flag that parses
// but does nothing is worse than one that fails: it reads as a knob the
// operator still has.
func TestInitTakesOnlyProvider(t *testing.T) {
	for _, args := range [][]string{
		{"init", "--provider", "claude-code", "--enforce"},
		{"init", "--provider", "claude-code", "--no-enforce"},
		{"init", "--provider", "claude-code", "--full"},
		{"init", "--provider", "claude-code", "--dry-run"},
		{"init", "--provider", "claude-code", "--remove-all"},
		{"init", "--provider", "claude-code", "--gateway"},
		{"init", "--provider", "claude-code", "--telemetry"},
		{"init", "--provider", "claude-code", "--transport"},
		{"init", "--provider", "claude-code", "--install-git-hook"},
		{"init", "--provider", "claude-code", "--scope", "global"},
		{"init", "--provider", "claude-code", "--role", "approver"},
	} {
		isolateHome(t)
		a, _, errb := testApp(nil)
		if code := a.run(args); code == exitOK {
			t.Errorf("%v was accepted; every flag but --provider is gone:\n%s", args, errb.String())
		}
	}
}

// TestInitStillRequiresAProviderAndNamesTheSupportedSet. The one surviving
// flag, and its accepted set is rendered rather than restated so it cannot
// disagree with what Lookup actually resolves.
func TestInitStillRequiresAProviderAndNamesTheSupportedSet(t *testing.T) {
	isolateHome(t)
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init"}); code == exitOK {
		t.Fatal("init without --provider was accepted")
	}
	if !strings.Contains(errb.String(), "claude-code") {
		t.Errorf("the refusal does not name the supported providers:\n%s", errb.String())
	}
}

// TestCodexInstallsTelemetryButNeverTransport: Codex gets the telemetry lane
// (it reads its own config.toml), but never the in-path transport relay --
// its proxy arm is the system PAC, which installs nothing here. Erroring
// because a provider cannot have a lane it never asked for would be a
// regression from the flag era; the right shape is a derivation with a
// printed line instead.
func TestCodexInstallsTelemetryButNeverTransport(t *testing.T) {
	skipUnlessSupervised(t)
	isolateHome(t)
	seedCredentials(t, "codex")
	a, out, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "codex"}); code != exitOK {
		t.Fatalf("codex init exit = %d; stderr=%q", code, errb.String())
	}
	s := out.String()
	if strings.Contains(s, "hooks only") {
		t.Errorf("a codex install still claims hooks-only; the telemetry lane arm did not take:\n%s", s)
	}
	if !strings.Contains(s, "EXPORTS") {
		t.Errorf("a codex install does not disclose the telemetry lane it now installs:\n%s", s)
	}
	// A relay of the Anthropic Messages API is Claude-Code-specific; Codex's
	// proxy arm is the system PAC, which installs nothing here.
	if strings.Contains(s, "INTERCEPTS") {
		t.Errorf("a codex install claims the transport lane, which it cannot have:\n%s", s)
	}
}

// TestAPersistedEnforceOptOutSurvivesAReInstall is the half that a
// fresh-machine test cannot see. The defect CLAUDE.md records was not a missing
// key -- it was a re-run silently putting `true` back over a deliberate opt-out,
// which looks identical on a machine that never opted out.
func TestAPersistedEnforceOptOutSurvivesAReInstall(t *testing.T) {
	home := isolateHome(t)
	seedCredentials(t)

	// Somebody opted out, deliberately, before this install ran.
	off := false
	if err := devconfig.WriteConfig(filepath.Join(home, "dev.json"), devconfig.Update{Enforce: &off}); err != nil {
		t.Fatal(err)
	}
	if devconfig.ResolveEnforce() {
		t.Fatal("the fixture did not take; nothing below would mean anything")
	}

	for _, run := range []string{"first", "second"} {
		a, _, errb := testApp(nil)
		if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
			t.Fatalf("%s init exit = %d; stderr=%q", run, code, errb.String())
		}
		if devconfig.ResolveEnforce() {
			t.Fatalf("the %s install reverted a deliberate enforce opt-out", run)
		}
		cfg := readDevJSON(t, home)
		if cfg.Enforce == nil || *cfg.Enforce {
			t.Fatalf("the %s install rewrote the persisted opt-out: %+v", run, cfg.Enforce)
		}
	}
}
