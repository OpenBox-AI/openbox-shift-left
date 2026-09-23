package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
)

// installedMachine is a fake machine carrying everything `init` writes, so a
// removal test asserts against the real surfaces rather than a subset one
// author remembered. Every path is under a temp HOME or a temp cwd.
type installedMachine struct {
	home        string
	projectDir  string
	userHooks   string
	localHooks  string
	codexHooks  string
	pluginDir   string
	envFile     string
	devJSON     string
	spoolDirs   []string
	sessionDir  string
	pendingDir  string
	managedFile string
}

func newInstalledMachine(t *testing.T) *installedMachine {
	t.Helper()
	h := newLaneHarness(t)
	nothingIsListening(t)

	openboxHome := filepath.Join(h.home, ".openbox")
	spool := filepath.Join(h.home, "spool")
	sessions := filepath.Join(h.home, "sessions")
	pending := filepath.Join(h.home, "pending-approvals")
	t.Setenv(devconfig.EnvSpoolDir, spool)
	t.Setenv(obgit.EnvSessionDir, sessions)
	t.Setenv(devconfig.EnvPendingApprovalDir, pending)
	t.Setenv("HOME", h.home)
	t.Setenv("CODEX_HOME", filepath.Join(h.home, ".codex"))
	// The org's own file, relocated under the fixture: the real one lives in
	// /etc and a test must neither write nor delete it.
	t.Setenv(devconfig.EnvManagedConfig, filepath.Join(openboxHome, "managed-dev.json"))
	// The flush reaches the data plane when credentials resolve, and this
	// fixture supplies them. Pinned at a dead loopback port so no test can post
	// fixture events at the hosted core.
	t.Setenv(devconfig.EnvBaseURL, "http://127.0.0.1:1")

	projectDir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	m := &installedMachine{
		home:        h.home,
		projectDir:  projectDir,
		userHooks:   gatewayservice.SettingsPath(h.home),
		localHooks:  filepath.Join(projectDir, ".claude", "settings.local.json"),
		codexHooks:  providers.CodexHooksPath(),
		pluginDir:   providers.ClaudePluginDir(),
		envFile:     filepath.Join(openboxHome, ".env"),
		devJSON:     filepath.Join(openboxHome, "dev.json"),
		spoolDirs:   providers.OwnedSpoolDirs(),
		sessionDir:  sessions,
		pendingDir:  pending,
		managedFile: devconfig.ManagedConfigPath(),
	}

	const claudeHooks = `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[` +
		`{"type":"command","command":"\"/o/openbox\" hook claude-code PreToolUse","timeout":60}]}],` +
		`"SessionStart":[{"hooks":[{"type":"command","command":"\"/o/openbox\" hook claude-code SessionStart"}]}]}}`
	const codexHooksBody = `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[` +
		`{"type":"command","command":"\"/o/openbox\" hook codex PreToolUse","timeout":60}]}]}}`

	writeFile(t, m.userHooks, claudeHooks)
	writeFile(t, m.localHooks, claudeHooks)
	writeFile(t, m.codexHooks, codexHooksBody)
	writeFile(t, filepath.Join(m.pluginDir, "bin", "openbox"), "#!/bin/sh\n")
	writeFile(t, filepath.Join(m.pluginDir, "hooks", "hooks.json"), claudeHooks)
	writeFile(t, m.envFile, fixtureEnvFile())
	writeFile(t, m.devJSON, `{"did":"did:openbox:x"}`)
	writeFile(t, m.managedFile, "[policy]\n")
	for _, dir := range m.spoolDirs {
		writeFile(t, filepath.Join(dir, "pending-1.jsonl"), `{"event":"x"}`+"\n")
	}
	writeFile(t, filepath.Join(sessions, "s1.json"), "{}")
	writeFile(t, filepath.Join(pending, "p1.json"), "{}")
	return m
}

// fixtureEnvFile assembles the credential file at run time. A literal secret
// assignment in a source file is rewritten by this repo's own redactor, which
// would leave the fixture asserting against a placeholder. The values are also
// chosen not to collide with ordinary prose, so the INV-1 test cannot pass or
// fail on a word like "seed" appearing in a sentence.
func fixtureEnvFile() string {
	return strings.Join([]string{
		devconfig.EnvAPIKeyDirect + "=" + fixtureAPIKey,
		devconfig.EnvAgentPrivateKey + "=" + fixtureSeed,
		"",
	}, "\n")
}

const (
	fixtureAPIKey = "obx" + "_" + "notarealkey" + "ZZQQ"
	fixtureSeed   = "notarealsigning" + "seedvalue" + "ZZQQ"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (m *installedMachine) app(t *testing.T) (*app, *strings.Builder) {
	t.Helper()
	a, out, errb := testApp(map[string]string{
		"HOME":                m.home,
		devconfig.EnvHome:     filepath.Join(m.home, ".openbox"),
		devconfig.EnvSpoolDir: m.spoolDirs[0],
	})
	var combined strings.Builder
	a.stdout, a.stderr = &combined, &combined
	_, _ = out, errb
	return a, &combined
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestUninstallOnACleanMachineIsSuccess. Uninstall detects rather than being
// told, so "nothing here" is an ordinary outcome and must not read as failure.
func TestUninstallOnACleanMachineIsSuccess(t *testing.T) {
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	nothingIsListening(t)
	t.Setenv("HOME", h.home)
	t.Setenv("CODEX_HOME", filepath.Join(h.home, ".codex"))
	t.Setenv(devconfig.EnvSpoolDir, filepath.Join(h.home, "spool"))

	a, _, _ := testApp(map[string]string{"HOME": h.home, devconfig.EnvHome: filepath.Join(h.home, ".openbox")})
	var out strings.Builder
	a.stdout, a.stderr = &out, &out
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d on a clean machine:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "nothing to remove") {
		t.Errorf("a clean machine was not reported as such:\n%s", out.String())
	}
}

// TestUninstallRemovesEverySurface is the whole promise. Every surface is
// walked unconditionally rather than branched on a detected provider: a missed
// one leaves a hook that keeps firing after the command reported success.
func TestUninstallRemovesEverySurface(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	a, out := m.app(t)

	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	for name, path := range map[string]string{
		"plugin bundle":  m.pluginDir,
		"credentials":    m.envFile,
		"posture":        m.devJSON,
		"session record": m.sessionDir,
		"pending approv": m.pendingDir,
	} {
		if exists(path) {
			t.Errorf("%s survived at %s:\n%s", name, path, out.String())
		}
	}
	for _, dir := range m.spoolDirs {
		if exists(dir) {
			t.Errorf("spool survived at %s", dir)
		}
	}
	for _, path := range []string{m.userHooks, m.localHooks, m.codexHooks} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // removed with the file is also removed
		}
		for _, gone := range []string{"hook claude-code", "hook codex", "rewake claude-code"} {
			if strings.Contains(string(raw), gone) {
				t.Errorf("%s still registers %q:\n%s", path, gone, raw)
			}
		}
	}
	// The org's file is a mandate, not ours; removing it silently downgrades a
	// governed machine.
	if !exists(m.managedFile) {
		t.Errorf("uninstall deleted the org-owned %s", m.managedFile)
	}
	if !strings.Contains(out.String(), m.managedFile) {
		t.Errorf("the report does not say the org-owned file was kept:\n%s", out.String())
	}
}

// TestUninstallPrintsTheBlastRadiusBeforeDeleting. The purge destroys an
// unrecoverable signing seed and any undelivered evidence, so the operator has
// to see the list before the first byte is lost.
func TestUninstallPrintsTheBlastRadiusBeforeDeleting(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	s := out.String()
	inventory := strings.Index(s, m.pluginDir)
	deletion := strings.Index(s, "deleted")
	if inventory < 0 || deletion < 0 || inventory > deletion {
		t.Errorf("the inventory does not precede the deletions:\n%s", s)
	}
	// The loss has to be named, not implied.
	for _, want := range []string{"cannot be re-retrieved", "openbox auth"} {
		if !strings.Contains(s, want) {
			t.Errorf("the report does not say %q:\n%s", want, s)
		}
	}
}

// TestUninstallKeepsForeignHookEntries. These files belong to the developer and
// their org; a corporate hook removed by an uninstall is an outage.
func TestUninstallKeepsForeignHookEntries(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	const foreign = `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[` +
		`{"type":"command","command":"/opt/team/guard --deny-rm","timeout":4}]},` +
		`{"matcher":"*","hooks":[{"type":"command","command":"\"/o/openbox\" hook claude-code PreToolUse"}]}]}}`
	writeFile(t, m.userHooks, foreign)
	writeFile(t, m.localHooks, foreign)

	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	for _, path := range []string{m.userHooks, m.localHooks} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("a file holding a foreign hook was deleted: %v", err)
		}
		if !strings.Contains(string(raw), "guard --deny-rm") {
			t.Errorf("%s lost its foreign hook:\n%s", path, raw)
		}
		if strings.Contains(string(raw), "hook claude-code") {
			t.Errorf("%s kept ours:\n%s", path, raw)
		}
	}
}

// TestUninstallRemovesAnOwnedCodexOtelBlock is the additive config.toml
// surface: the sweep must remove an OpenBox-owned [otel] block and report
// it, alongside the hook surfaces.
func TestUninstallRemovesAnOwnedCodexOtelBlock(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	configPath := providers.CodexConfigTOMLPath()
	if err := providers.WriteCodexOtel(configPath, "http://127.0.0.1:4318/v1/logs"); err != nil {
		t.Fatalf("seed WriteCodexOtel: %v", err)
	}

	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	if providers.HasOwnedCodexOtel(configPath) {
		t.Errorf("the owned [otel] block survived uninstall:\n%s", out.String())
	}
	if !strings.Contains(out.String(), configPath) {
		t.Errorf("the report does not name the config.toml surface it swept:\n%s", out.String())
	}
}

// TestUninstallLeavesAForeignCodexOtelBlockIntact mirrors
// TestUninstallKeepsForeignHookEntries for the config.toml surface: a
// developer's own OTel setup is not OpenBox's to delete.
func TestUninstallLeavesAForeignCodexOtelBlockIntact(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	configPath := providers.CodexConfigTOMLPath()
	const foreign = "[otel]\nenvironment = \"my-own-otel-setup\"\n"
	writeFile(t, configPath, foreign)

	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("a file holding a foreign [otel] block was deleted: %v", err)
	}
	if !strings.Contains(string(raw), "my-own-otel-setup") {
		t.Errorf("config.toml lost its foreign [otel] block:\n%s", raw)
	}
}

// TestUninstallSweepsAStaleDeliveryStatusFile is the completeness fix: a
// lane daemon's persisted DeliverPool status (hookflow.StatusPersister) must
// not survive uninstall, or a later `doctor` run reports a dropped-record
// count for a lane that no longer exists on this machine.
func TestUninstallSweepsAStaleDeliveryStatusFile(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	openboxHome := filepath.Join(m.home, ".openbox")
	statusPath := hookflow.DeliverStatusPath(openboxHome, "telemetry")
	writeFile(t, statusPath, `{"dropped":3,"since":"2026-09-20T00:00:00Z"}`)

	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	if fileExists(statusPath) {
		t.Errorf("a stale delivery-status file survived uninstall: %s", statusPath)
	}
}

// TestUninstallOnACodexOnlyTelemetryMachineReportsNoUnrecordedLane is the
// false-diagnostic fix: setupCodexTelemetry never calls activation.Activate
// (Codex's [otel] ownership lives in config.toml's own parsed marker, so a
// mixed CC+Codex machine's two telemetry installs never collide on one
// activation-record key), so activation.ActiveLanes reports zero lanes on a
// Codex-only machine even though the telemetry unit is genuinely, healthily
// running. laneResidue must not read that as an interrupted removal.
//
// This goes through the REAL install path (a.run(["init", ...])), not a
// hand-seeded fixture: newInstalledMachine's CC-activated fixture (used by
// the config.toml sweep tests elsewhere in this file) already carries a
// claude-code activation record, so inv.lanes is never empty there and this
// bug cannot surface in that fixture.
func TestUninstallOnACodexOnlyTelemetryMachineReportsNoUnrecordedLane(t *testing.T) {
	skipUnlessSupervised(t)
	isolateHome(t)
	seedCredentials(t, "codex")

	a, out, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "codex"}); code != exitOK {
		t.Fatalf("codex init exit = %d; stderr=%q", code, errb.String())
	}
	t.Cleanup(func() {
		cleanup, _, cleanupErr := testApp(nil)
		if code := cleanup.run([]string{"uninstall"}); code != exitOK {
			t.Logf("cleanup uninstall exit = %d; stderr=%q", code, cleanupErr.String())
		}
	})

	// The same home runUninstall itself resolves (a.gatewayHome(), which is
	// $HOME -- an OS-level convention for LaunchAgents/systemd user units --
	// never devconfig.Home()/OPENBOX_HOME, which is a different directory in
	// this fixture). Calling uninstallInventory with any other path would
	// prove nothing about the real command.
	home, code := a.gatewayHome()
	if home == "" {
		t.Fatalf("could not resolve $HOME: exit %d", code)
	}
	inv := a.uninstallInventory(home)
	if !inv.codexOtelPresent {
		t.Fatal("fixture drift: codex init did not write an owned [otel] block; the rest of this test proves nothing")
	}
	if len(inv.lanes) != 0 {
		t.Fatalf("fixture drift: codex's telemetry install registered an activation-record lane (%v); "+
			"setupCodexTelemetry is supposed to own its state entirely through config.toml's marker instead", inv.lanes)
	}
	if inv.unrecordedLane {
		t.Error("a healthy Codex-only telemetry install must not report an unrecorded lane; " +
			"the telemetry unit existing with no activation record is that install's normal, healthy shape")
	}

	out.Reset()
	a.printInventory(inv)
	if strings.Contains(out.String(), "with no activation record behind it") {
		t.Errorf("the inventory report still claims interrupted-removal residue on a healthy machine:\n%s", out.String())
	}
}

// TestUninstallDeletesCredentialsEvenOnPartialFailure is the owner's ruling,
// and its condition: keeping .env alone preserves nothing retryable, because
// the spool is deleted unconditionally. One rule, reported precisely.
func TestUninstallDeletesCredentialsEvenOnPartialFailure(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	writeFile(t, m.codexHooks, `{"hooks": {"PreToolUse": [`) // unparsable

	a, out := m.app(t)
	code := a.runUninstall(nil)
	if code == exitOK {
		t.Errorf("a surface that could not be cleaned reported success:\n%s", out.String())
	}
	if exists(m.envFile) {
		t.Errorf("credentials survived a partial failure at %s", m.envFile)
	}
	s := out.String()
	// The consequence has to be the accurate one. A file the tool cannot parse
	// is not a hook that keeps firing -- the tool applies NO hooks from it,
	// including the developer's own -- and saying "still firing" would send the
	// operator looking for governance that is not happening.
	for _, want := range []string{m.codexHooks, "applies NO hooks", "uninstall` again"} {
		if !strings.Contains(s, want) {
			t.Errorf("the report does not name the consequence %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "keeps firing") {
		t.Errorf("an unparsable file was described as still firing:\n%s", s)
	}
}

// TestUninstallWithoutCredentialsStillCompletes. CLAUDE.md's ordering rule:
// removal runs before the credential gate, so a missing credential downgrades
// the flush to reported loss rather than refusing the whole command.
func TestUninstallWithoutCredentialsStillCompletes(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	if err := os.Remove(m.envFile); err != nil {
		t.Fatal(err)
	}
	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d without credentials:\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "flush") {
		t.Errorf("the report says nothing about the flush it could not do:\n%s", s)
	}
	// The backlog it could not deliver is loss, and has to read as loss.
	if !strings.Contains(s, "undelivered") && !strings.Contains(s, "DESTROYED") {
		t.Errorf("undelivered evidence was destroyed without being reported:\n%s", s)
	}
}

// TestUninstallDeDuplicatesASharedSpool. OPENBOX_SPOOL_DIR overrides the whole
// path, so both adapters resolve to one directory and a report that took the
// list at face value would claim two deletions.
func TestUninstallDeDuplicatesASharedSpool(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	if len(m.spoolDirs) != 1 {
		t.Fatalf("the override should collapse the spools, got %v", m.spoolDirs)
	}
	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	// Counted by row rather than by a padded literal: the column width is a
	// layout choice, and pinning it here fails the next time it changes without
	// saying anything about de-duplication.
	n := 0
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "deleted ") && strings.Contains(line, m.spoolDirs[0]) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the shared spool was reported %d times, want 1:\n%s", n, out.String())
	}
}

// TestUninstallIsIdempotent. A one-shot test passes on state a re-run corrupts,
// which is why the second invocation is asserted rather than assumed.
func TestUninstallIsIdempotent(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	a, first := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("first exit = %d:\n%s", code, first.String())
	}
	b, second := m.app(t)
	if code := b.runUninstall(nil); code != exitOK {
		t.Fatalf("second exit = %d:\n%s", code, second.String())
	}
	if strings.Contains(second.String(), "deleted        ") {
		t.Errorf("the second run deleted something:\n%s", second.String())
	}
	if !strings.Contains(second.String(), "nothing to remove") {
		t.Errorf("the second run did not report a clean machine:\n%s", second.String())
	}
}

// TestUninstallPromisesNoWaitForHookRemoval. Claude Code's file
// watcher picks up settings edits at once, so a successful removal ends
// governance of live sessions immediately: telling an operator to restart
// implies a window that does not exist. Lane env keys are the opposite, and
// that caveat must be the only one.
func TestUninstallPromisesNoWaitForHookRemoval(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "restart") && !strings.Contains(lower, "new session") {
			continue
		}
		if !strings.Contains(lower, "env") && !strings.Contains(lower, "lane") && !strings.Contains(lower, "daemon") {
			t.Errorf("a restart instruction that is not about lane env keys: %q", line)
		}
	}
}

// TestUninstallRejectsFlagsAndArguments. It detects what is installed; a flag
// that looks accepted and does nothing is worse than a refusal.
func TestUninstallRejectsFlagsAndArguments(t *testing.T) {
	a, _, _ := testApp(map[string]string{"HOME": t.TempDir()})
	var out strings.Builder
	a.stdout, a.stderr = &out, &out
	for _, args := range [][]string{{"--provider", "claude-code"}, {"claude-code"}, {"--remove-all"}} {
		if code := a.runUninstall(args); code == exitOK {
			t.Errorf("uninstall %v was accepted:\n%s", args, out.String())
		}
	}
}

// TestUninstallIsListedInUsage. A command nobody can find is a command that
// does not exist.
func TestUninstallIsListedInUsage(t *testing.T) {
	a, _, errb := testApp(nil)
	a.usage()
	if !strings.Contains(errb.String(), "openbox uninstall") {
		t.Errorf("usage does not list uninstall:\n%s", errb.String())
	}
}

// TestUninstallNeverPrintsASecret holds INV-1 on the one path that reads the
// credential file in order to delete it.
func TestUninstallNeverPrintsASecret(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	for _, secret := range []string{fixtureAPIKey, fixtureSeed} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("a credential value reached the output (%q):\n%s", secret, out.String())
		}
	}
}

// TestUninstallReportsTheSpoolBacklogItIsAboutToDestroy. The owner's condition
// on the full-purge ruling: the loss is reported, not silent.
func TestUninstallReportsTheSpoolBacklogItIsAboutToDestroy(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	if n := (hookflow.Spool{Dir: m.spoolDirs[0]}).BacklogCount(); n == 0 {
		t.Fatalf("the fixture seeded no backlog, so this test would assert nothing")
	}
	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "flushing") {
		t.Errorf("no flush was announced before the spool was destroyed:\n%s", out.String())
	}
}

// TestUninstallSweepsALegacyProjectOnARerun is the remedy for the one residue
// this command cannot find on its own. A project initialized before hooks moved
// to user scope still carries entries naming the plugin-bundle engine, which
// the first uninstall deleted — so the tool reports a failed hook there on
// every call. Re-running from inside that project has to clean it, at whatever
// engine path it was registered with, and must not report a clean machine.
func TestUninstallSweepsALegacyProjectOnARerun(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	a, first := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("first uninstall exit = %d:\n%s", code, first.String())
	}

	// A second project, registered by an older install at a different engine.
	legacy := t.TempDir()
	legacyHooks := filepath.Join(legacy, ".claude", "settings.local.json")
	writeFile(t, legacyHooks, `{"permissions":{"allow":["Bash(ls:*)"]},`+
		`"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command",`+
		`"command":"\"/older/plugins/openbox-observe/bin/openbox\" hook claude-code PreToolUse"}]}]}}`)
	if err := os.Chdir(legacy); err != nil {
		t.Fatal(err)
	}

	b, second := m.app(t)
	if code := b.runUninstall(nil); code != exitOK {
		t.Fatalf("re-run exit = %d:\n%s", code, second.String())
	}
	raw, err := os.ReadFile(legacyHooks)
	if err != nil {
		t.Fatalf("the legacy project's settings file was deleted: %v", err)
	}
	if strings.Contains(string(raw), "hook claude-code") {
		t.Errorf("the legacy registration survived a re-run from its own project:\n%s", raw)
	}
	if !strings.Contains(string(raw), "Bash(ls:*)") {
		t.Errorf("the re-run took the developer's own settings with it:\n%s", raw)
	}
	if strings.Contains(second.String(), "nothing to remove") {
		t.Errorf("a project that still carried a firing hook was reported clean:\n%s", second.String())
	}
}

// TestUninstallReportsTheProjectFileItChecked. "Only partly cleaned" is only
// actionable if the operator can see which file this run actually swept.
func TestUninstallReportsTheProjectFileItChecked(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), m.localHooks) {
		t.Errorf("the report does not name the project file it swept (%s):\n%s", m.localHooks, out.String())
	}
	// And the residue for other projects must not be described as inert: those
	// entries name a deleted engine and fail loudly on every tool call.
	s := out.String()
	idx := strings.Index(s, "settings.local.json in any project other than")
	if idx < 0 {
		t.Fatalf("the report does not mention project residue at all:\n%s", s)
	}
	window := s[idx:min(len(s), idx+600)]
	if !strings.Contains(window, "FAILED HOOK") {
		t.Errorf("project residue is not described as failing loudly:\n%s", window)
	}
}

// TestUninstallKeepsWhatARetryNeedsWhenALaneRefuses is the case the report's
// own remedy depends on. A conflicted deactivate leaves the daemon loaded and
// its env keys routed, and the activation record is the only thing that can
// restore those keys — so purging it made "resolve it and run uninstall again"
// impossible, and the second run reported a clean machine at a daemon that was
// still intercepting model calls.
func TestUninstallKeepsWhatARetryNeedsWhenALaneRefuses(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)

	// A transport lane that is routed and whose env value changed after OpenBox
	// set it: deactivate refuses rather than overwriting somebody's edit.
	settings := gatewayservice.SettingsPath(m.home)
	writeFile(t, settings, `{"env":{"HTTPS_PROXY":"http://127.0.0.1:9999"},`+
		`"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"\"/o/openbox\" hook claude-code SessionStart"}]}]}}`)
	record := activation.RecordPath(m.home)
	writeFile(t, record, `{"schema":"openbox.dev-runtime.activation/v1","lanes":{"transport":{`+
		`"managed":{"HTTPS_PROXY":"http://127.0.0.1:8790"},`+
		`"original":{"HTTPS_PROXY":{"present":false}},`+
		`"settings_path":"`+settings+`"}}}`)

	a, out := m.app(t)
	code := a.runUninstall(nil)
	s := out.String()
	if code == exitOK {
		t.Errorf("a refused deactivate reported success:\n%s", s)
	}
	if _, err := os.Stat(record); err != nil {
		t.Errorf("the activation record was purged despite a routed lane, so nothing can restore "+
			"its env keys: %v\n%s", err, s)
	}
	if !strings.Contains(s, "HTTPS_PROXY") {
		t.Errorf("the report does not name the key that blocked the removal:\n%s", s)
	}
	if !strings.Contains(s, "still routed") && !strings.Contains(s, "STILL ROUTED") {
		t.Errorf("the report does not say the lane is still routed:\n%s", s)
	}

	// And the re-run must not claim a clean machine while that lane is live.
	b, second := m.app(t)
	b.runUninstall(nil)
	if strings.Contains(second.String(), "nothing to remove") {
		t.Errorf("the re-run reported a clean machine with a lane still routed:\n%s", second.String())
	}
}

// TestUninstallDoesNotClaimALaneIsRoutedOnAPlatformWithNoDaemons. The unit
// renderer refuses an OS it has no packaging for, and reading that refusal as a
// removal failure told an operator their lanes were still intercepting model
// calls on a machine that cannot run one.
func TestUninstallDoesNotClaimALaneIsRoutedOnAPlatformWithNoDaemons(t *testing.T) {
	if code := (removalResult{}); !code.ok() {
		t.Fatal("an empty result must read as success")
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"unsupported platform", laneservice.Telemetry("", "", false).UnsupportedPlatform("windows"), false},
		{"changed value", errors.New("activation: HTTPS_PROXY changed since OpenBox set it; refusing to overwrite"), true},
		{"unit write failed", errors.New("laneservice: removing /x: permission denied"), false},
	} {
		if got := isActivationConflict(tc.err); got != tc.want {
			t.Errorf("%s: isActivationConflict = %v, want %v (only a refused deactivate leaves a lane routed)",
				tc.name, got, tc.want)
		}
	}
	if !isUnsupportedPlatform(laneservice.Telemetry("", "", false).UnsupportedPlatform("windows")) {
		t.Error("the platform refusal is not recognised, so a Windows uninstall would always exit 1")
	}
}
