package gatewayservice

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, home string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(SettingsPath(home))
	if err != nil {
		t.Fatalf("reading settings: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("settings not valid JSON: %v\n%s", err, raw)
	}
	return m
}

func seed(t *testing.T, home, body string) {
	t.Helper()
	path := SettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWriteEnvPreservesForeignConfiguration is the ownership rule. `init` once
// destroyed configuration by deciding what it owned with an exact-match; here
// anything outside the owned key set is preserved untouched, including inside the
// env block itself.
func TestWriteEnvPreservesForeignConfiguration(t *testing.T) {
	home := t.TempDir()
	seed(t, home, `{
	  "env": {"HTTP_PROXY": "http://corp-proxy:3128", "MY_ORG_FLAG": "1"},
	  "permissions": {"allow": ["Bash(ls)"]},
	  "someFutureKey": {"nested": true}
	}`)

	if _, err := WriteEnv(home, "127.0.0.1:8788"); err != nil {
		t.Fatalf("WriteEnv: %v", err)
	}
	got := read(t, home)

	env, _ := got["env"].(map[string]any)
	if env["HTTP_PROXY"] != "http://corp-proxy:3128" {
		t.Errorf("a foreign env key was lost: %v", env)
	}
	if env["MY_ORG_FLAG"] != "1" {
		t.Errorf("a foreign env key was lost: %v", env)
	}
	if env[EnvKey] != "http://127.0.0.1:8788" {
		t.Errorf("%s = %v", EnvKey, env[EnvKey])
	}
	// Keys this package has never heard of must survive the round-trip. Decoding
	// into a typed struct is how a writer silently deletes them.
	if got["permissions"] == nil || got["someFutureKey"] == nil {
		t.Errorf("unknown top-level keys were dropped: %v", got)
	}
}

// TestPlainReWriteIsIdempotent is the SECOND-INVOCATION test, and it exists
// because this repo has already shipped the bug it guards. Fifteen green enforce
// tests missed a silent opt-out revert because each ran the command exactly once.
func TestPlainReWriteIsIdempotent(t *testing.T) {
	home := t.TempDir()

	if _, err := WriteEnv(home, "127.0.0.1:8788"); err != nil {
		t.Fatalf("first WriteEnv: %v", err)
	}
	first, err := os.ReadFile(SettingsPath(home))
	if err != nil {
		t.Fatal(err)
	}

	replaced, err := WriteEnv(home, "127.0.0.1:8788")
	if err != nil {
		t.Fatalf("second WriteEnv: %v", err)
	}
	second, err := os.ReadFile(SettingsPath(home))
	if err != nil {
		t.Fatal(err)
	}

	if string(first) != string(second) {
		t.Errorf("a plain re-run changed the file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if len(replaced) != 0 {
		t.Errorf("a no-op re-run reported replacements: %v", replaced)
	}
}

// TestWriteEnvReportsWhatItReplaced keeps a silent overwrite from happening. When
// a developer or an org had pointed the variable somewhere else, the change has to
// be visible in the output rather than discovered later.
func TestWriteEnvReportsWhatItReplaced(t *testing.T) {
	home := t.TempDir()
	seed(t, home, `{"env":{"`+EnvKey+`":"https://api.anthropic.com"}}`)

	replaced, err := WriteEnv(home, "127.0.0.1:8788")
	if err != nil {
		t.Fatalf("WriteEnv: %v", err)
	}
	if len(replaced) != 1 {
		t.Fatalf("expected one reported replacement, got %v", replaced)
	}
	if !strings.Contains(replaced[0], "api.anthropic.com") || !strings.Contains(replaced[0], "127.0.0.1:8788") {
		t.Errorf("the report does not name both sides: %q", replaced[0])
	}
}

// TestRemoveEnvRemovesOnlyOwnedKeys is the uninstall counterpart to the ownership
// rule. An org that put its own variables in the env block must not lose them
// because OpenBox was removed.
func TestRemoveEnvRemovesOnlyOwnedKeys(t *testing.T) {
	home := t.TempDir()
	seed(t, home, `{"env":{"`+EnvKey+`":"http://127.0.0.1:8788","HTTP_PROXY":"http://p:3128"},"permissions":{}}`)

	removed, err := RemoveEnv(home)
	if err != nil {
		t.Fatalf("RemoveEnv: %v", err)
	}
	if len(removed) != 1 || removed[0] != EnvKey {
		t.Errorf("removed = %v, want only %s", removed, EnvKey)
	}
	got := read(t, home)
	env, _ := got["env"].(map[string]any)
	if env["HTTP_PROXY"] != "http://p:3128" {
		t.Errorf("uninstall took a foreign key with it: %v", env)
	}
	if _, still := env[EnvKey]; still {
		t.Error("the owned key survived uninstall")
	}
	if got["permissions"] == nil {
		t.Error("uninstall dropped unrelated configuration")
	}
}

// TestRemoveEnvDropsAnEmptyEnvBlock — but only when nothing else is left in it.
func TestRemoveEnvDropsAnEmptyEnvBlock(t *testing.T) {
	home := t.TempDir()
	seed(t, home, `{"env":{"`+EnvKey+`":"http://127.0.0.1:8788"}}`)

	if _, err := RemoveEnv(home); err != nil {
		t.Fatalf("RemoveEnv: %v", err)
	}
	got := read(t, home)
	if _, present := got["env"]; present {
		t.Errorf("an empty env block was left behind: %v", got)
	}
}

// TestRemoveEnvOnAnUntouchedMachineIsANoOp — uninstall must not create files or
// error when there was nothing to remove.
func TestRemoveEnvOnAnUntouchedMachineIsANoOp(t *testing.T) {
	home := t.TempDir()
	removed, err := RemoveEnv(home)
	if err != nil {
		t.Fatalf("RemoveEnv: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed %v from a machine with no settings", removed)
	}
	if _, err := os.Stat(SettingsPath(home)); !os.IsNotExist(err) {
		t.Error("uninstall created a settings file")
	}
}

// TestUnparseableSettingsIsRefusedNotClobbered — a file we cannot parse is a file
// we cannot safely rewrite. Overwriting it would destroy configuration nobody
// asked us to touch.
func TestUnparseableSettingsIsRefusedNotClobbered(t *testing.T) {
	home := t.TempDir()
	const broken = `{"env": {"A": 1`
	seed(t, home, broken)

	if _, err := WriteEnv(home, "127.0.0.1:8788"); err == nil {
		t.Fatal("WriteEnv rewrote a file it could not parse")
	}
	raw, err := os.ReadFile(SettingsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != broken {
		t.Errorf("the unparseable file was modified:\n%s", raw)
	}
}

// TestCurrentEnvIsTheReadSide — the opt-out rule needs reads and writes checked
// separately, which is the general lesson from the enforce-flag bug.
func TestCurrentEnvIsTheReadSide(t *testing.T) {
	home := t.TempDir()
	if _, present := CurrentEnv(home); present {
		t.Error("reported a value on a machine with no settings")
	}
	if _, err := WriteEnv(home, "127.0.0.1:8788"); err != nil {
		t.Fatal(err)
	}
	v, present := CurrentEnv(home)
	if !present || v != "http://127.0.0.1:8788" {
		t.Errorf("CurrentEnv = %q, %v", v, present)
	}
	if _, err := RemoveEnv(home); err != nil {
		t.Fatal(err)
	}
	if _, present := CurrentEnv(home); present {
		t.Error("value still reported after removal")
	}
}
