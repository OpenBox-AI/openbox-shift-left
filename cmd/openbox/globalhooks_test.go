package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
)

// countAcross sums how many times marker appears in each of the given files,
// skipping the ones that do not exist. Counting across BOTH levels is the
// point: neither file alone can show that the same gate is registered twice.
func countAcross(t *testing.T, marker string, paths ...string) int {
	t.Helper()
	total := 0
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		total += strings.Count(string(raw), marker)
	}
	return total
}

// initInIsolatedProject runs a real `init` in a fresh temp cwd under a fresh
// temp HOME, and returns the output plus both hook files.
func initInIsolatedProject(t *testing.T) (out, userHooks, projectHooks string) {
	t.Helper()
	isolateHome(t)
	seedCredentials(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	a, stdout, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	return stdout.String(), providers.ClaudeUserSettingsPath(), providers.ClaudeProjectSettingsPath(wd)
}

// TestInitSweepsALegacyProjectEntryAtADifferentEngine is the case that is not
// merely untidy. Both scopes derive the engine identically, so an old entry and
// a new one are usually byte-identical commands the tool runs once. A project
// entry naming a DIFFERENT engine path is the shape it will not de-duplicate:
// both fire, and every governed tool call in that project is stored twice.
func TestInitSweepsALegacyProjectEntryAtADifferentEngine(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	projectHooks := providers.ClaudeProjectSettingsPath(wd)
	if err := os.MkdirAll(filepath.Dir(projectHooks), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"permissions":{"allow":["Bash(ls:*)"]},"hooks":{` +
		`"PreToolUse":[{"matcher":"*","hooks":[` +
		`{"type":"command","command":"\"/older/plugins/openbox-observe/bin/openbox\" hook claude-code PreToolUse"},` +
		`{"type":"command","command":"\"/older/plugins/openbox-observe/bin/openbox\" rewake claude-code","asyncRewake":true}]}],` +
		`"Stop":[{"hooks":[{"type":"command","command":"\"/older/plugins/openbox-observe/bin/openbox\" hook claude-code Stop"}]}]}}`
	if err := os.WriteFile(projectHooks, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	a, out, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	userHooks := providers.ClaudeUserSettingsPath()

	// Exactly one gate, and exactly one watcher, across both files.
	for _, marker := range []string{`hook claude-code PreToolUse"`, `rewake claude-code"`, `hook claude-code Stop"`} {
		if n := countAcross(t, marker, userHooks, projectHooks); n != 1 {
			t.Errorf("%q is registered %d time(s) across both files, want exactly 1", marker, n)
		}
	}
	if n := countAcross(t, "/older/plugins", userHooks, projectHooks); n != 0 {
		t.Errorf("the legacy engine path survives in %d place(s)", n)
	}
	// The developer's own settings in that file are not ours to remove.
	raw, err := os.ReadFile(projectHooks)
	if err != nil {
		t.Fatalf("the project settings file was deleted: %v", err)
	}
	if !strings.Contains(string(raw), "Bash(ls:*)") {
		t.Errorf("the sweep took the developer's own settings with it:\n%s", raw)
	}
	// And the sweep is reported: a silent removal from a file inside somebody's
	// repository is worse than a noisy one.
	if !strings.Contains(out.String(), projectHooks) {
		t.Errorf("the run does not name the project file it swept:\n%s", out.String())
	}
}

// TestInitKeepsForeignEntriesInTheUserFile. This file is the developer's, and
// on a managed fleet it may carry the org's hooks too. An OpenBox install that
// removed or reformatted somebody else's entry would be an outage.
func TestInitKeepsForeignEntriesInTheUserFile(t *testing.T) {
	isolateHome(t)
	seedCredentials(t)
	userHooks := providers.ClaudeUserSettingsPath()
	if err := os.MkdirAll(filepath.Dir(userHooks), 0o755); err != nil {
		t.Fatal(err)
	}
	before := `{"model":"opus","permissions":{"allow":["Bash(git:*)"]},"hooks":{` +
		`"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/opt/team/guard --deny-rm","timeout":7}]}]}}`
	if err := os.WriteFile(userHooks, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	raw, err := os.ReadFile(userHooks)
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{`"model":"opus"`, "Bash(git:*)", "guard --deny-rm", `"timeout":7`} {
		if !strings.Contains(strings.ReplaceAll(string(raw), " ", ""), strings.ReplaceAll(kept, " ", "")) {
			t.Errorf("init did not preserve %q:\n%s", kept, raw)
		}
	}
	if !strings.Contains(string(raw), "hook claude-code PreToolUse") {
		t.Errorf("init did not register its own gate alongside the foreign one:\n%s", raw)
	}
}

// TestInitIsIdempotentAcrossBothFiles. CLAUDE.md's discipline: a one-shot test
// passes on state a re-run corrupts, and this exact class of defect shipped
// once already because every test ran init only once.
func TestInitIsIdempotentAcrossBothFiles(t *testing.T) {
	_, userHooks, projectHooks := initInIsolatedProject(t)
	first, err := os.ReadFile(userHooks)
	if err != nil {
		t.Fatal(err)
	}

	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("second init exit = %d; stderr=%q", code, errb.String())
	}
	second, err := os.ReadFile(userHooks)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("a re-run rewrote the settings file:\n%s\n---\n%s", first, second)
	}
	for _, marker := range []string{`hook claude-code PreToolUse"`, `rewake claude-code"`} {
		if n := countAcross(t, marker, userHooks, projectHooks); n != 1 {
			t.Errorf("after two installs %q is registered %d time(s), want 1", marker, n)
		}
	}
}

// TestInitSaysNothingIsGovernedWhenTheMachineBlocksHooks is the failure this
// phase exists to make visible. On an org-managed machine the hooks can be
// installed, correct, and never run: the install prints success, and the first
// evidence of the gap is an empty dashboard, which reads as a broken product
// rather than a locked machine.
func TestInitSaysNothingIsGovernedWhenTheMachineBlocksHooks(t *testing.T) {
	for name, managed := range map[string]string{
		"allowManagedHooksOnly":                `{"allowManagedHooksOnly": true}`,
		"strictPluginOnlyCustomization object": `{"strictPluginOnlyCustomization": {"hooks": true}}`,
		"strictPluginOnlyCustomization bare":   `{"strictPluginOnlyCustomization": true}`,
		"disableAllHooks at the managed level": `{"disableAllHooks": true}`,
	} {
		t.Run(name, func(t *testing.T) {
			isolateHome(t)
			seedCredentials(t)
			managedPath := filepath.Join(t.TempDir(), "managed-settings.json")
			if err := os.WriteFile(managedPath, []byte(managed), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv(envManagedSettingsPath, managedPath)

			a, out, errb := testApp(nil)
			if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
				t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
			}
			s := out.String()
			if !strings.Contains(s, "NOTHING IS GOVERNED BY THIS INSTALL") {
				t.Errorf("a machine that blocks hooks was reported as governed:\n%s", s)
			}
			if !strings.Contains(s, managedPath) {
				t.Errorf("the notice does not name the file to look in:\n%s", s)
			}
		})
	}
}

// TestInitDoesNotCryBlockedOnAnOrdinaryMachine. The notice has to be rare, or
// every reader learns to skip it.
func TestInitDoesNotCryBlockedOnAnOrdinaryMachine(t *testing.T) {
	t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))
	out, _, _ := initInIsolatedProject(t)
	if strings.Contains(out, "NOTHING IS GOVERNED") {
		t.Errorf("an unmanaged machine got the blocked notice:\n%s", out)
	}
}

// TestDoctorReportsBothLevelsAndTheCrossLevelCondition. Neither file alone
// shows a gate registered at two different engine paths, which is the shape
// the tool does not de-duplicate.
func TestDoctorReportsBothLevelsAndTheCrossLevelCondition(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(envManagedSettingsPath, filepath.Join(t.TempDir(), "absent.json"))

	userHooks := filepath.Join(home, ".claude", "settings.json")
	projectHooks := filepath.Join(dir, ".claude", "settings.local.json")
	for path, engine := range map[string]string{userHooks: "/new/openbox", projectHooks: "/old/openbox"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command",` +
			`"command":"\"` + engine + `\" hook claude-code PreToolUse"}]}]}}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out, code := runDoctorIn(t, dir)
	if code != exitOK {
		t.Fatalf("doctor exit = %d", code)
	}
	for _, want := range []string{"user-wide", "this project", "/new/openbox", "/old/openbox", "stored TWICE"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor does not report %q:\n%s", want, out)
		}
	}
}

// TestDoctorSaysWhetherHooksCanRunAtAll. Answering what absence cannot is this
// command's job, so the healthy answer is stated rather than left implicit.
func TestDoctorSaysWhetherHooksCanRunAtAll(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	managedPath := filepath.Join(t.TempDir(), "managed-settings.json")
	if err := os.WriteFile(managedPath, []byte(`{"allowManagedHooksOnly": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envManagedSettingsPath, managedPath)

	out, code := runDoctorIn(t, dir)
	if code != exitOK {
		t.Fatalf("doctor exit = %d", code)
	}
	if !strings.Contains(out, "can they run") || !strings.Contains(out, "allowManagedHooksOnly") {
		t.Errorf("doctor does not report the blocking key:\n%s", out)
	}
}
