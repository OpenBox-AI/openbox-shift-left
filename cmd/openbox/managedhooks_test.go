package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookFiles seeds the four settings levels this reader consults and points the
// reader at them. The managed path is an override because the real one is
// /Library/Application Support or /etc: a test must neither read the machine's
// nor write there.
type hookFiles struct {
	managed, user, project, local string
}

func seedHookFiles(t *testing.T) *hookFiles {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	project := filepath.Join(root, "project")
	for _, dir := range []string{filepath.Join(home, ".claude"), filepath.Join(project, ".claude")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &hookFiles{
		managed: filepath.Join(root, "managed-settings.json"),
		user:    filepath.Join(home, ".claude", "settings.json"),
		project: filepath.Join(project, ".claude", "settings.json"),
		local:   filepath.Join(project, ".claude", "settings.local.json"),
	}
	t.Setenv(envManagedSettingsPath, f.managed)
	t.Setenv("HOME", home)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return f
}

func (f *hookFiles) write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNoSettingsAtAllIsNotBlocked. Absence is the ordinary machine, and a
// reader that called it blocked would train every reader to ignore the finding.
func TestNoSettingsAtAllIsNotBlocked(t *testing.T) {
	seedHookFiles(t)
	if state := resolveHookBlock(); state.blocked {
		t.Errorf("a machine with no settings files was reported blocked: %+v", state)
	}
}

// TestAllowManagedHooksOnlyBlocksOurs. It is a managed-only key, and it blocks
// user, project, local AND plugin hooks — everything except hooks the managed
// policy itself declares. Ours are user-level, so they never run.
func TestAllowManagedHooksOnlyBlocksOurs(t *testing.T) {
	f := seedHookFiles(t)
	f.write(t, f.managed, `{"allowManagedHooksOnly": true}`)
	state := resolveHookBlock()
	if !state.blocked {
		t.Fatalf("allowManagedHooksOnly: true was not reported as blocking: %+v", state)
	}
	if !strings.Contains(state.summary+strings.Join(state.detail, " "), "allowManagedHooksOnly") {
		t.Errorf("the finding does not name the key: %+v", state)
	}
	if !strings.Contains(state.summary+strings.Join(state.detail, " "), f.managed) {
		t.Errorf("the finding does not name the file to look in: %+v", state)
	}
}

// TestStrictPluginOnlyCustomizationBlocksInBothShapes. The reference defines an
// object with a .hooks sub-key; the template this repo ships writes a bare
// boolean. Handling one shape and not the other reports a governed machine as
// governed when it is not.
func TestStrictPluginOnlyCustomizationBlocksInBothShapes(t *testing.T) {
	for name, body := range map[string]string{
		"object": `{"strictPluginOnlyCustomization": {"hooks": true}}`,
		"bare":   `{"strictPluginOnlyCustomization": true}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := seedHookFiles(t)
			f.write(t, f.managed, body)
			state := resolveHookBlock()
			if !state.blocked {
				t.Fatalf("the %s shape was not reported as blocking: %+v", name, state)
			}
			if !strings.Contains(state.summary+strings.Join(state.detail, " "), "strictPluginOnlyCustomization") {
				t.Errorf("the finding does not name the key: %+v", state)
			}
		})
	}
}

// TestStrictPluginOnlyCustomizationForAnotherCategoryDoesNotBlock. The object
// form names categories; locking skills says nothing about hooks.
func TestStrictPluginOnlyCustomizationForAnotherCategoryDoesNotBlock(t *testing.T) {
	f := seedHookFiles(t)
	f.write(t, f.managed, `{"strictPluginOnlyCustomization": {"skills": true}}`)
	if state := resolveHookBlock(); state.blocked {
		t.Errorf("a lock on skills was read as a lock on hooks: %+v", state)
	}
}

// TestTheseKeysAreManagedOnly. Both are settable only at the managed level, so
// a developer writing either into their own settings file must not be reported
// as having disabled their governance — and must not be able to.
func TestTheseKeysAreManagedOnly(t *testing.T) {
	for _, body := range []string{
		`{"allowManagedHooksOnly": true}`,
		`{"strictPluginOnlyCustomization": {"hooks": true}}`,
	} {
		f := seedHookFiles(t)
		f.write(t, f.user, body)
		f.write(t, f.project, body)
		if state := resolveHookBlock(); state.blocked {
			t.Errorf("a managed-only key honoured from a user or project file (%s): %+v", body, state)
		}
	}
}

// TestDisableAllHooksIsResolvedByPrecedenceNotPresence. Documented explicitly:
// a project's false overrides a user's true. A reader that reported "blocked"
// because it found the key somewhere would be wrong on exactly the machine
// where somebody turned it back on.
func TestDisableAllHooksIsResolvedByPrecedenceNotPresence(t *testing.T) {
	f := seedHookFiles(t)
	f.write(t, f.user, `{"disableAllHooks": true}`)
	f.write(t, f.project, `{"disableAllHooks": false}`)
	if state := resolveHookBlock(); state.blocked {
		t.Errorf("presence beat precedence: a project false must override a user true: %+v", state)
	}

	// And the other direction: the higher-precedence file turning it on wins.
	g := seedHookFiles(t)
	g.write(t, g.user, `{"disableAllHooks": false}`)
	g.write(t, g.project, `{"disableAllHooks": true}`)
	state := resolveHookBlock()
	if !state.blocked {
		t.Errorf("a project true did not override a user false: %+v", state)
	}
	if !strings.Contains(state.summary+strings.Join(state.detail, " "), g.project) {
		t.Errorf("the finding does not name the file that decided it: %+v", state)
	}
}

// TestLocalSettingsOutrankProject for disableAllHooks, the same way they do for
// every other key.
func TestLocalSettingsOutrankProject(t *testing.T) {
	f := seedHookFiles(t)
	f.write(t, f.project, `{"disableAllHooks": true}`)
	f.write(t, f.local, `{"disableAllHooks": false}`)
	if state := resolveHookBlock(); state.blocked {
		t.Errorf("a local false did not override a project true: %+v", state)
	}
}

// TestManagedDisableAllHooksCannotBeOverridden. Documented verbatim: only
// disableAllHooks at the managed level can disable managed hooks, and managed
// always wins. So a project's false must not read as re-enabling anything.
func TestManagedDisableAllHooksCannotBeOverridden(t *testing.T) {
	f := seedHookFiles(t)
	f.write(t, f.managed, `{"disableAllHooks": true}`)
	f.write(t, f.project, `{"disableAllHooks": false}`)
	state := resolveHookBlock()
	if !state.blocked {
		t.Fatalf("a project false overrode a managed true: %+v", state)
	}
	if !strings.Contains(state.summary+strings.Join(state.detail, " "), f.managed) {
		t.Errorf("the finding does not name the managed file: %+v", state)
	}
}

// TestManagedHooksAreNotMisreportedAsUngoverned. If the org's own policy
// installs hooks, a user-level disableAllHooks turns OFF ours and leaves the
// org's running. "Nothing is governed" would be the wrong answer, and the
// dangerous one: it invites someone to go looking for a governance gap that
// is not there while missing the one that is.
func TestManagedHooksAreNotMisreportedAsUngoverned(t *testing.T) {
	f := seedHookFiles(t)
	f.write(t, f.managed, `{"hooks": {"PreToolUse": [{"matcher": "*", "hooks": [`+
		`{"type": "command", "command": "/opt/org/guard"}]}]}}`)
	f.write(t, f.user, `{"disableAllHooks": true}`)
	state := resolveHookBlock()
	if !state.blocked {
		t.Fatalf("a user-level disableAllHooks did not turn our hooks off: %+v", state)
	}
	all := state.summary + " " + strings.Join(state.detail, " ")
	if !strings.Contains(all, "managed") {
		t.Errorf("the finding does not say the machine is still governed by managed policy: %+v", state)
	}
}

// TestAnUnreadableManagedFileIsReportedNotAssumed. "Not managed" and "managed
// and unparsable" are different machines, and a reader that conflated them
// would call an unknown state healthy.
func TestAnUnreadableManagedFileIsReportedNotAssumed(t *testing.T) {
	f := seedHookFiles(t)
	f.write(t, f.managed, `{not json`)
	state := resolveHookBlock()
	all := state.summary + " " + strings.Join(state.detail, " ")
	if !strings.Contains(all, f.managed) || !strings.Contains(strings.ToLower(all), "could not") {
		t.Errorf("an unparsable managed file was not reported as unknown: %+v", state)
	}
}

// TestTheDefaultMachineSaysHooksRun. The healthy answer has to be stated, not
// left as the absence of a warning: doctor's whole job here is answering what
// absence cannot.
func TestTheDefaultMachineSaysHooksRun(t *testing.T) {
	f := seedHookFiles(t)
	f.write(t, f.user, `{"model": "opus"}`)
	state := resolveHookBlock()
	if state.blocked {
		t.Fatalf("an ordinary machine was reported blocked: %+v", state)
	}
	if state.summary == "" {
		t.Error("the healthy case reported nothing at all")
	}
}
