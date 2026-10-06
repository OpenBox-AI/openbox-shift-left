package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
)

// museOutputs maps a muse argv to what the fake binary prints; an argv it does
// not know exits 64 so a test notices a command it did not script.
type museOutputs map[string]providers.MuseRunResult

// museCalls records what the fake muse was asked to run and where.
type museCalls struct{ Args, Dirs []string }

func (c *museCalls) ran(prefix string) bool {
	for _, a := range c.Args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// withMuseRunner replaces every muse subprocess for one test and records what
// was run. The real binary is never reached: TestMain refuses it by default.
func withMuseRunner(t *testing.T, outputs museOutputs) *museCalls {
	t.Helper()
	prev := providers.MuseRunner
	calls := &museCalls{}
	providers.MuseRunner = func(_ context.Context, dir string, args ...string) (providers.MuseRunResult, error) {
		key := strings.Join(args, " ")
		calls.Args, calls.Dirs = append(calls.Args, key), append(calls.Dirs, dir)
		if r, ok := outputs[key]; ok {
			return r, nil
		}
		if strings.HasPrefix(key, "exec --provider echo") {
			if r, ok := outputs["exec --provider echo"]; ok {
				return r, nil
			}
		}
		return providers.MuseRunResult{ExitCode: 64, Stderr: []byte("unscripted: " + key)}, nil
	}
	t.Cleanup(func() { providers.MuseRunner = prev })
	return calls
}

func museVersion(v string) museOutputs {
	return museOutputs{"--version": {Stdout: []byte("muse " + v + "\n")}}
}

func museSettingsFile(t *testing.T) string {
	t.Helper()
	return providers.MuseSettingsPath()
}

// TestInitMuseWritesNoStrayPostureKeyOnEitherRun is the two-init invariant for
// Muse: a posture field init says nothing about must stay unset on the first
// run and the second, or a written default becomes indistinguishable from a
// choice. A test that runs init once cannot see that class of defect.
func TestInitMuseWritesNoStrayPostureKeyOnEitherRun(t *testing.T) {
	isolateHome(t)
	seedCredentials(t, "muse")
	withMuseRunner(t, museVersion("1.4.0"))

	devPath, err := devconfig.DevConfigWritePath()
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"first", "second"} {
		a, _, errb := testApp(nil)
		if code := a.run([]string{"init", "--provider", "muse"}); code != exitOK {
			t.Fatalf("%s init exit = %d; stderr=%q", run, code, errb.String())
		}
		raw, err := os.ReadFile(devPath)
		if err != nil {
			t.Fatalf("%s run: read %s: %v", run, devPath, err)
		}
		for _, stray := range []string{`"enforce"`, `"fail_closed"`, `"tier2"`, `"findings"`, `"content_capture"`} {
			if strings.Contains(string(raw), stray) {
				t.Errorf("%s run wrote %s although init said nothing about it:\n%s", run, stray, raw)
			}
		}
		if !strings.Contains(string(raw), `"install_git_hook"`) {
			t.Errorf("%s run did not persist install_git_hook:\n%s", run, raw)
		}
		if !devconfig.ResolveEnforce() {
			t.Errorf("%s run: ResolveEnforce() = false", run)
		}
	}
}

func TestInitMuseRegistersTheHandlersAndIsIdempotent(t *testing.T) {
	isolateHome(t)
	seedCredentials(t, "muse")
	withMuseRunner(t, museVersion("1.4.0"))

	var first string
	for _, run := range []string{"first", "second"} {
		a, out, errb := testApp(nil)
		if code := a.run([]string{"init", "--provider", "muse"}); code != exitOK {
			t.Fatalf("%s init exit = %d; stderr=%q", run, code, errb.String())
		}
		raw, err := os.ReadFile(museSettingsFile(t))
		if err != nil {
			t.Fatal(err)
		}
		if run == "first" {
			first = string(raw)
			if !strings.Contains(out.String(), "MUSE SESSION") {
				t.Errorf("init does not say what it governs:\n%s", out.String())
			}
			if n := strings.Count(first, "hook muse "); n < 11 {
				t.Errorf("%d hook commands registered:\n%s", n, first)
			}
		} else if string(raw) != first {
			t.Errorf("a second init changed settings.json:\n%s", raw)
		}
	}
}

// TestInitMuseThenUninstallRestoresTheDevelopersSettings is the reversal
// promise: everything in settings.json that was not an OpenBox handler comes
// back byte for byte.
func TestInitMuseThenUninstallRestoresTheDevelopersSettings(t *testing.T) {
	skipUnlessSupervised(t)
	isolateHome(t)
	seedCredentials(t, "muse")
	withMuseRunner(t, museVersion("1.4.0"))

	const original = "{\n  \"schema_version\": 1,\n  \"theme\": \"dark\",\n  \"hooks\": {\n" +
		"    \"PreToolUse\": [\n      {\"matcher\": \"Bash\", \"hooks\": [{\"type\": \"command\", \"command\": \"/opt/team/guard\"}]}\n    ]\n  }\n}\n"
	path := museSettingsFile(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "muse"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	if raw, _ := os.ReadFile(path); string(raw) == original {
		t.Fatal("init changed nothing")
	}

	u, out, _ := testApp(nil)
	var combined strings.Builder
	u.stdout, u.stderr = &combined, &combined
	_ = out
	if code := u.runUninstall(nil); code != exitOK {
		t.Fatalf("uninstall exit = %d:\n%s", code, combined.String())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("uninstall deleted the developer's settings file: %v", err)
	}
	if string(raw) != original {
		t.Errorf("settings.json did not come back:\n--- want\n%s\n--- got\n%s", original, raw)
	}
}

// TestInitMuseRefusesAnOldMuseAndWritesNothing: the refusal comes before any
// registration, so there is no agent, credential, posture or hook left behind.
func TestInitMuseRefusesAnOldMuseAndWritesNothing(t *testing.T) {
	home := isolateHome(t)
	withMuseRunner(t, museVersion("1.3.2"))
	before := dirNames(t, home)

	a, _, errb := testApp(nil) // no registrar: reaching registration would panic
	if code := a.run([]string{"init", "--provider", "muse"}); code == exitOK {
		t.Fatal("init accepted muse 1.3.2")
	}
	if msg := errb.String(); !strings.Contains(msg, "1.4.0") || !strings.Contains(msg, "1.3.2") {
		t.Errorf("the refusal does not name both versions:\n%s", msg)
	}
	if _, err := os.Stat(museSettingsFile(t)); err == nil {
		t.Error("settings.json was written")
	}
	if after := dirNames(t, home); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("init changed the OpenBox home: before %v, after %v", before, after)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestInitMuseProceedsWithAWarningWhenMuseIsMissingOrNewer(t *testing.T) {
	for name, runner := range map[string]museOutputs{
		"newer than tested": museVersion("1.9.0"),
	} {
		t.Run(name, func(t *testing.T) {
			isolateHome(t)
			seedCredentials(t, "muse")
			withMuseRunner(t, runner)
			a, _, errb := testApp(nil)
			if code := a.run([]string{"init", "--provider", "muse"}); code != exitOK {
				t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
			}
			if !strings.Contains(errb.String(), "warning:") {
				t.Errorf("no warning:\n%s", errb.String())
			}
			if _, err := os.Stat(museSettingsFile(t)); err != nil {
				t.Errorf("the hooks were not installed: %v", err)
			}
		})
	}

	t.Run("not on PATH", func(t *testing.T) {
		isolateHome(t)
		seedCredentials(t, "muse")
		prev := providers.MuseRunner
		providers.MuseRunner = func(context.Context, string, ...string) (providers.MuseRunResult, error) {
			return providers.MuseRunResult{}, providers.ErrMuseNotOnPath
		}
		t.Cleanup(func() { providers.MuseRunner = prev })
		a, _, errb := testApp(nil)
		if code := a.run([]string{"init", "--provider", "muse"}); code != exitOK {
			t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
		}
		if !strings.Contains(errb.String(), "not on PATH") {
			t.Errorf("no warning about the missing binary:\n%s", errb.String())
		}
		if _, err := os.Stat(museSettingsFile(t)); err != nil {
			t.Errorf("the hooks were not installed: %v", err)
		}
	})
}

// TestInitMuseBakesANonDefaultHomeIntoEveryHandler: Muse clears the hook's
// environment, so a home given only as OPENBOX_HOME would silently govern
// nothing. (The default home needing no argument is pinned in the adapter.)
func TestInitMuseBakesANonDefaultHomeIntoEveryHandler(t *testing.T) {
	home := isolateHome(t) // OPENBOX_HOME is a temp dir, which is not the default
	seedCredentials(t, "muse")
	withMuseRunner(t, museVersion("1.4.0"))
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "muse"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	raw, err := os.ReadFile(museSettingsFile(t))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(`--home "`+home+`"`, `"`, `\"`)
	if n, total := strings.Count(string(raw), want), strings.Count(string(raw), "hook muse "); n != total || total == 0 {
		t.Errorf("%d of %d hook commands carry %s:\n%s", n, total, want, raw)
	}
}
