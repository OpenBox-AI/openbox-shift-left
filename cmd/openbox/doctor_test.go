package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorWarnsWhenTwoEnginesAreRegistered two engines registered in one
// project is a silent, self-inflicted double-count of every governed tool
// call: both fire, both store, and an operator reading the result sees broken
// tool-health numbers with nothing pointing at the cause.
func TestDoctorWarnsWhenTwoEnginesAreRegistered(t *testing.T) {
	out := inDirWithSettings(t, map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{
			map[string]any{"matcher": "*", "hooks": []any{
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" hook claude-code PreToolUse`},
			}},
			map[string]any{"matcher": "*", "hooks": []any{
				map[string]any{"type": "command", "command": `"/opt/b/bin/openbox" hook claude-code PreToolUse`},
			}},
		},
	}})

	if !strings.Contains(out, "WARNING") {
		t.Errorf("two registered engines produced no WARNING:\n%s", out)
	}
	for _, want := range []string{"/opt/a/bin/openbox", "/opt/b/bin/openbox", "openbox init", "TWICE"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output does not mention %q:\n%s", want, out)
		}
	}
}

// TestDoctorDoesNotWarnOnASingleEngine a healthy install must not warn;
// PreToolUse legitimately carries two of our handlers (the gate and the
// approval watcher), so counting handlers per event rather than per invocation
// would warn on every correct install and train the reader to ignore the one
// that matters.
func TestDoctorDoesNotWarnOnASingleEngine(t *testing.T) {
	out := inDirWithSettings(t, map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{
			map[string]any{"matcher": "*", "hooks": []any{
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" hook claude-code PreToolUse`},
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" rewake claude-code`},
			}},
		},
		"Stop": []any{
			map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" hook claude-code Stop`},
			}},
		},
	}})

	if strings.Contains(out, "WARNING: ") && strings.Contains(out, "engines are registered") {
		t.Errorf("a single-engine install warned:\n%s", out)
	}
	if !strings.Contains(out, "/opt/a/bin/openbox") {
		t.Errorf("doctor did not report the registered engine:\n%s", out)
	}
}

// TestDoctorWarnsWhenOneInvocationIsRegisteredTwice the same invocation
// registered twice at ONE path is the same defect, and it is what an unquoted-
// path edge case or a hand-edited file leaves behind.
func TestDoctorWarnsWhenOneInvocationIsRegisteredTwice(t *testing.T) {
	out := inDirWithSettings(t, map[string]any{"hooks": map[string]any{
		"Stop": []any{
			map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" hook claude-code Stop`},
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" hook claude-code Stop`},
			}},
		},
	}})
	if !strings.Contains(out, "more than once") || !strings.Contains(out, "Stop") {
		t.Errorf("a doubly-registered event produced no warning:\n%s", out)
	}
}

// TestDoctorReportsAnAbsentProjectHookFileAsAFact an absent file is the normal
// state; a global-scope install, or any directory that was never initialized.
// It must read as a fact about this directory, not as a fault, and never as
// "not governed".
func TestDoctorReportsAnAbsentProjectHookFileAsAFact(t *testing.T) {
	out, code := runDoctorIn(t, t.TempDir())
	if code != exitOK {
		t.Errorf("doctor exit = %d, want %d; an optional check must not change the exit code", code, exitOK)
	}
	if !strings.Contains(out, "(absent)") {
		t.Errorf("absent settings file not reported:\n%s", out)
	}
	if strings.Contains(out, "WARNING: 1 OpenBox") || strings.Contains(out, "engines are registered") {
		t.Errorf("absent settings file produced a warning:\n%s", out)
	}
}

// TestDoctorSurvivesInvalidProjectSettingsJSON doctor must survive a settings
// file it cannot parse: this command is what a developer runs when something
// is already wrong.
func TestDoctorSurvivesInvalidProjectSettingsJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runDoctorIn(t, dir)
	if code != exitOK {
		t.Errorf("doctor exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(out, "could not be read") {
		t.Errorf("invalid JSON not reported as a condition:\n%s", out)
	}
	if !strings.Contains(out, "What this does and does not prove") {
		t.Errorf("doctor stopped early instead of continuing past the failed check:\n%s", out)
	}
}

func inDirWithSettings(t *testing.T, settings map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runDoctorIn(t, dir)
	if code != exitOK {
		t.Fatalf("doctor exit = %d, want %d", code, exitOK)
	}
	return out
}

func runDoctorIn(t *testing.T, dir string) (string, int) {
	t.Helper()
	saved, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(saved) })

	t.Setenv("OPENBOX_HOME", t.TempDir())
	var out, errb bytes.Buffer
	a := &app{stdout: &out, stderr: &errb, getenv: os.Getenv}
	code := a.runDoctor(nil)
	return out.String() + errb.String(), code
}
