package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// TestInitWritesNoEnforceKeyEverAgain is what makes dropping --enforce safe
// rather than a behaviour change.
//
// There is no CredentialRef/Update field for enforce at all any more (no
// install-time knob left, since ResolveEnforce always reports true), so
// nothing `init` does can ever write the key. Writing it would look
// identical on a fresh machine and differ on every machine that had a
// leftover opt-out sitting on disk.
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
			t.Errorf("%s run wrote an enforce key:\n%s", run, raw)
		}
		if !devconfig.ResolveEnforce() {
			t.Errorf("%s run: ResolveEnforce() = false; it must always report true now", run)
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

// TestTheEnvOverrideNoLongerOptsOutOfEnforce inverts the old opt-out
// contract: OPENBOX_ENFORCE=false is a deprecated, ignored key now (every
// gated tool call is evaluated unconditionally), so it must not turn
// enforcement off, and it must not be persisted into dev.json either -- the
// key is parsed only so it can warn.
func TestTheEnvOverrideNoLongerOptsOutOfEnforce(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	t.Setenv(devconfig.EnvEnforce, "false")

	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	if !devconfig.ResolveEnforce() {
		t.Error("OPENBOX_ENFORCE=false must be ignored; enforcement is always on now")
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
		t.Errorf("the ignored env override was persisted into the config:\n%s", raw)
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

// TestAPersistedEnforceOptOutIsIgnoredAcrossReInstall inverts the old
// survives-a-reinstall contract: a leftover `"enforce": false` from before
// this change is a dead, ignored key now, not an opt-out. It stays on disk
// (a plain re-init never touches a field it was not given), but it must not
// turn enforcement off, on the first install or any later one.
func TestAPersistedEnforceOptOutIsIgnoredAcrossReInstall(t *testing.T) {
	home := isolateHome(t)

	// A leftover opt-out from before enforcement was made unconditional, or a
	// hand-edited file -- either way, a dev.json nobody generates any more but
	// that still has to parse and be ignored. Written raw, and BEFORE
	// seeding credentials: Update has no Enforce field left to seed it
	// through WriteConfig, and seedCredentials' own WriteConfig call merges
	// over whatever is already on disk rather than replacing it, so writing
	// this first is what lets the seeded agent id and this leftover key
	// coexist.
	devPath := filepath.Join(home, "dev.json")
	off := false
	seed, err := json.Marshal(devconfig.DevConfig{Enforce: &off})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(devPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	seedCredentials(t)
	if !devconfig.ResolveEnforce() {
		t.Fatal("a persisted enforce:false must be ignored; ResolveEnforce must still report true")
	}

	for _, run := range []string{"first", "second"} {
		a, _, errb := testApp(nil)
		if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
			t.Fatalf("%s init exit = %d; stderr=%q", run, code, errb.String())
		}
		if !devconfig.ResolveEnforce() {
			t.Fatalf("the %s install let a dead enforce:false opt out of enforcement", run)
		}
	}
}
