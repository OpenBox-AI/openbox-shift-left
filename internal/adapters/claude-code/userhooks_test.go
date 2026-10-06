package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestUserSettingsPathResolvesTheSameHomeAsTheBundle. The hook command names
// the engine inside the plugin bundle, so if these two disagreed about which
// home they mean, the file would register a path that does not exist.
func TestUserSettingsPathResolvesTheSameHomeAsTheBundle(t *testing.T) {
	got := UserSettingsPath()
	if !filepath.IsAbs(got) {
		t.Fatalf("UserSettingsPath() = %q, which is not absolute; it would resolve against the cwd", got)
	}
	if base := filepath.Base(got); base != "settings.json" {
		t.Errorf("UserSettingsPath() = %q; want it to end in settings.json", got)
	}
	// Same home, same depth: both derive from os.UserHomeDir with a $HOME
	// fallback, so one cannot drift onto a different account than the other.
	settingsHome := filepath.Dir(filepath.Dir(got))
	bundleHome := filepath.Dir(filepath.Dir(filepath.Dir(DefaultPluginDir())))
	if settingsHome != bundleHome {
		t.Errorf("the settings file resolves home as %q but the bundle resolves it as %q; "+
			"the hooks would name an engine path that does not exist", settingsHome, bundleHome)
	}
}

// TestProjectSettingsPathIsTheOneDefinition. The sweep, the audit and every
// test address the same file; a second join is how they drift apart.
func TestProjectSettingsPathIsTheOneDefinition(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, ".claude", "settings.local.json")
	if got := ProjectSettingsPath(dir); got != want {
		t.Errorf("ProjectSettingsPath(%q) = %q; want %q", dir, got, want)
	}
}

// TestSweepProjectHooksTakesOursAndKeepsTheirs. An install now governs every
// session on this machine, so an entry left in a project file is a second
// registration of the same gate. Taking it out is the install's job; taking
// anything else out would be an outage in the developer's own tooling.
func TestSweepProjectHooksTakesOursAndKeepsTheirs(t *testing.T) {
	dir := t.TempDir()
	path := ProjectSettingsPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{
	  "permissions": {"allow": ["Bash(ls:*)"]},
	  "hooks": {
	    "PreToolUse": [
	      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/opt/team/guard", "timeout": 4}]},
	      {"matcher": "*", "hooks": [{"type": "command", "command": "\"/older/openbox\" hook claude-code PreToolUse"}]}
	    ]
	  }
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := SweepProjectHooks(dir)
	if err != nil {
		t.Fatalf("SweepProjectHooks: %v", err)
	}
	if len(removed) != 1 || !strings.Contains(removed[0], "PreToolUse") {
		t.Errorf("removed = %v; want the one PreToolUse registration", removed)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hook claude-code") {
		t.Errorf("the project registration survived the sweep:\n%s", raw)
	}
	for _, kept := range []string{"/opt/team/guard", "Bash(ls:*)"} {
		if !strings.Contains(string(raw), kept) {
			t.Errorf("the sweep took %q with it:\n%s", kept, raw)
		}
	}
}

// TestSweepProjectHooksOnAnAbsentFileIsANoOp. Every install runs the sweep,
// and most directories were never initialized; creating a file there would put
// an OpenBox artifact in a repo that never asked for one.
func TestSweepProjectHooksOnAnAbsentFileIsANoOp(t *testing.T) {
	dir := t.TempDir()
	removed, err := SweepProjectHooks(dir)
	if err != nil || removed != nil {
		t.Fatalf("SweepProjectHooks on a clean directory = %v, %v; want nil, nil", removed, err)
	}
	if _, err := os.Stat(ProjectSettingsPath(dir)); !os.IsNotExist(err) {
		t.Error("the sweep created a settings file in a directory that had none")
	}
}

// TestSweepProjectHooksRefusesAnUnparsableFile rather than editing it: sjson
// writes into a malformed document without complaint, and a settings file
// Claude Code cannot parse stops every hook in it applying — including the
// developer's own — while reporting nothing.
func TestSweepProjectHooksRefusesAnUnparsableFile(t *testing.T) {
	dir := t.TempDir()
	path := ProjectSettingsPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepProjectHooks(dir); err == nil {
		t.Fatal("SweepProjectHooks accepted a malformed settings file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{not json" {
		t.Errorf("a file it refused to parse was rewritten anyway:\n%s", raw)
	}
}

// TestWriteHooksToAUserFileCreatesItsParent. The user-wide path is the default
// now, and a machine whose ~/.claude does not exist yet must still be
// governable; the old project-scoped writer refused a directory that was not
// already there.
func TestWriteHooksToAUserFileCreatesItsParent(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := writeHooks(path, "/opt/openbox/bin/openbox"); err != nil {
		t.Fatalf("writeHooks: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the writer did not create the file: %v", err)
	}
	hooks := gjson.GetBytes(raw, "hooks")
	if !hooks.Exists() {
		t.Fatalf("no hooks block was written:\n%s", raw)
	}
	if got, want := len(hooks.Map()), len(localHookEvents); got != want {
		t.Errorf("wrote %d events, want all %d; an unregistered event is a silently ungoverned one", got, want)
	}
}
