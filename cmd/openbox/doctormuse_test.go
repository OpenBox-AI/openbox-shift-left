package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// healthyMuse scripts a muse 1.4.0 whose hooks all load and whose policy is
// clean; a test overrides one answer to reach each row's other states.
func healthyMuse() museOutputs {
	return museOutputs{
		"--version":            {Stdout: []byte("muse 1.4.0\n")},
		"exec --provider echo": {Stderr: []byte("Hooks: 12 runnable · 0 warnings\n")},
		"config validate":      {ExitCode: 0},
		"config status":        {Stdout: []byte("managed hooks lane required: false\n")},
	}
}

func with(base museOutputs, key string, r providers.MuseRunResult) museOutputs {
	out := museOutputs{}
	for k, v := range base {
		out[k] = v
	}
	out[key] = r
	return out
}

// withMuseTracingDir points the local-tracing lookup at dir.
func withMuseTracingDir(t *testing.T, dir string) {
	t.Helper()
	prev := museLocalTracingDir
	museLocalTracingDir = func() string { return dir }
	t.Cleanup(func() { museLocalTracingDir = prev })
}

// installedMuse runs a real `init --provider muse` into the isolated machine,
// so doctor reads what the installer wrote rather than a hand-made fixture.
func installedMuse(t *testing.T) {
	t.Helper()
	isolateHomeOnly(t) // each tool's dev.json in its own directory, as on a real machine
	seedCredentials(t, "muse")
	withMuseRunner(t, museVersion("1.4.0"))
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "muse"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	withMuseTracingDir(t, filepath.Join(t.TempDir(), "no-tracing"))
}

// museDoctor runs doctor and returns the Muse section.
func museDoctor(t *testing.T) string {
	t.Helper()
	a, out, errb := testApp(nil)
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d; stderr=%q", code, errb.String())
	}
	s := out.String()
	i := strings.Index(s, "Muse Code (hooks)")
	if i < 0 {
		t.Fatalf("no Muse section in doctor:\n%s", s)
	}
	section := s[i:]
	if j := strings.Index(section[1:], "\nOnly `managed` values"); j > 0 {
		section = section[:j+1]
	}
	return section
}

// flat joins a wrapped value back into one line, so an assertion is about the
// words and not about where the column wrapped them.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func mustContain(t *testing.T, section string, wants ...string) {
	t.Helper()
	f := flat(section)
	for _, w := range wants {
		if !strings.Contains(f, w) {
			t.Errorf("the Muse section does not say %q:\n%s", w, section)
		}
	}
}

func TestDoctorSaysNothingAboutMuseOnAMachineThatNeverSetItUp(t *testing.T) {
	isolateHome(t) // the default runner panics: any muse subprocess fails this test
	a, out, _ := testApp(nil)
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d", code)
	}
	if strings.Contains(out.String(), "Muse Code") {
		t.Errorf("doctor reports a Muse section on a machine without one:\n%s", out.String())
	}
}

func TestDoctorMuseAllSevenRowsHealthy(t *testing.T) {
	installedMuse(t)
	withMuseRunner(t, healthyMuse())
	section := museDoctor(t)
	mustContain(t, section,
		"ok: muse 1.4.0 (supported: >= 1.4.0, tested below 1.5.0)",
		"ok: 12 of 12 OpenBox handlers registered, every gated one with a deny-only successor",
		"ok: Muse found 12 runnable hooks (12 expected), 0 warnings",
		"unverified: no local-tracing log",
		"ok: muse config validates. managed lane required: no",
		"Muse model calls: not recorded (no proxy or telemetry lane can see them); tool, prompt and model-call gating still enforced",
		"ok: 0 events waiting",
		"MCP gating: doc-verified, not empirically confirmed",
		"ok: 0 ungated Muse actions in the last 7 days (see `openbox trace <session>`)",
	)
	if strings.Contains(section, "not installed") {
		t.Errorf("the model-call row must never read as not installed:\n%s", section)
	}
	if strings.Contains(section, "FAIL") || strings.Contains(section, "WARNING") {
		t.Errorf("a healthy machine reports a problem:\n%s", section)
	}
}

// The probe is a session. It runs in a throwaway directory, with the trust flag
// the docs name, and the scratch directory is gone afterwards.
func TestDoctorMuseLoadProbeRunsEchoInAScratchDirectory(t *testing.T) {
	installedMuse(t)
	calls := withMuseRunner(t, healthyMuse())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	museDoctor(t)
	var dir string
	for i, args := range calls.Args {
		if strings.HasPrefix(args, "exec ") {
			want := "exec --provider echo --trust-workspace openbox doctor hook probe"
			if args != want {
				t.Errorf("probe argv = %q, want %q", args, want)
			}
			dir = calls.Dirs[i]
		}
	}
	if dir == "" || dir == cwd {
		t.Fatalf("the probe ran in %q (cwd %q); it needs a scratch directory", dir, cwd)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("the scratch directory %s was left behind", dir)
	}
}

func TestDoctorMuseLoadProbeStates(t *testing.T) {
	cases := []struct {
		name   string
		result providers.MuseRunResult
		want   string
	}{
		{"fewer than registered", providers.MuseRunResult{Stderr: []byte("Hooks: 5 runnable · 0 warnings")}, "FAIL: Muse found 5 runnable hooks, fewer than the 12 OpenBox registered"},
		{"more than registered", providers.MuseRunResult{Stderr: []byte("Hooks: 14 runnable · 0 warnings")}, "ok: Muse found 14 runnable hooks (12 expected)"},
		{"foreign warning", providers.MuseRunResult{Stderr: []byte("Hooks: 12 runnable · 1 warnings\nwarning: team hook x has a bad matcher")}, "WARNING: Muse found 12 runnable hooks (12 expected) and 1 warning(s), none naming OpenBox"},
		{"openbox warning", providers.MuseRunResult{Stderr: []byte("Hooks: 10 runnable · 1 warnings\nwarning: hook \"/o/openbox\" hook muse PreToolUse: unknown key")}, "FAIL: Muse loaded 10 hooks and warned about OpenBox's"},
		{"no summary", providers.MuseRunResult{ExitCode: 3, Stderr: []byte("boom")}, "WARNING: the echo probe (exit 3) printed no `Hooks: N runnable` summary"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installedMuse(t)
			withMuseRunner(t, with(healthyMuse(), "exec --provider echo", c.result))
			mustContain(t, museDoctor(t), c.want)
		})
	}
}

func TestDoctorMuseVersionStates(t *testing.T) {
	cases := []struct {
		name      string
		version   providers.MuseRunResult
		err       error
		want      string
		wantProbe bool
	}{
		{"too old", providers.MuseRunResult{Stdout: []byte("muse 1.3.0")}, nil, "FAIL: muse 1.3.0 is older than the supported minimum 1.4.0", false},
		{"newer", providers.MuseRunResult{Stdout: []byte("muse 2.0.0")}, nil, "WARNING: muse 2.0.0 is newer than the range this adapter was tested on", true},
		{"unreadable", providers.MuseRunResult{Stdout: []byte("muse dev")}, nil, "FAIL: muse's version could not be read", false},
		{"not on path", providers.MuseRunResult{}, providers.ErrMuseNotOnPath, "WARNING: muse is not on PATH", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installedMuse(t)
			calls := withMuseRunner(t, healthyMuse())
			base := providers.MuseRunner
			providers.MuseRunner = func(ctx context.Context, dir string, args ...string) (providers.MuseRunResult, error) {
				if strings.Join(args, " ") == "--version" {
					calls.Args = append(calls.Args, "--version")
					return c.version, c.err
				}
				if c.err != nil {
					return providers.MuseRunResult{}, c.err
				}
				return base(ctx, dir, args...)
			}
			section := museDoctor(t)
			mustContain(t, section, c.want)
			if calls.ran("exec ") != c.wantProbe {
				t.Errorf("probe ran = %v, want %v:\n%s", calls.ran("exec "), c.wantProbe, section)
			}
		})
	}
}

func TestDoctorMusePolicyRow(t *testing.T) {
	cases := []struct {
		name     string
		validate providers.MuseRunResult
		status   string
		want     string
	}{
		{"valid", providers.MuseRunResult{}, "managed hooks lane required: true", "ok: muse config validates. managed lane required: yes"},
		{"inactive members", providers.MuseRunResult{ExitCode: 4}, "managed hooks required = no", "WARNING: muse config is valid but has inactive members (exit 4). managed lane required: no"},
		{"rejected", providers.MuseRunResult{ExitCode: 1}, "", "FAIL: muse rejects its config (exit 1)"},
		{"unknown exit", providers.MuseRunResult{ExitCode: 9}, "", "WARNING: `muse config validate` exited 9"},
		{"unparsed status", providers.MuseRunResult{}, "everything is fine", "managed lane required: unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installedMuse(t)
			outs := with(healthyMuse(), "config validate", c.validate)
			outs = with(outs, "config status", providers.MuseRunResult{Stdout: []byte(c.status)})
			withMuseRunner(t, outs)
			mustContain(t, museDoctor(t), c.want)
		})
	}
}

func TestDoctorMuseReportsACountShortOfTheExpectedAndAMissingHome(t *testing.T) {
	installedMuse(t) // installed under a non-default OPENBOX_HOME
	path := providers.MuseSettingsPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := devconfig.Home()
	stripped := strings.ReplaceAll(string(raw), `--home \"`+home+`\" `, "")
	if stripped == string(raw) {
		t.Fatalf("the install did not bake %s into its handlers:\n%s", home, raw)
	}
	if err := os.WriteFile(path, []byte(stripped), 0o600); err != nil {
		t.Fatal(err)
	}
	withMuseRunner(t, healthyMuse())
	mustContain(t, museDoctor(t), "WARNING: 12 of 12 OpenBox handlers registered; --home does not match this machine's OpenBox home on")

	// And a handler that is simply gone.
	if err := os.WriteFile(path, []byte(strings.Replace(stripped, "hook muse SessionEnd", "hook codex SessionEnd", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	mustContain(t, museDoctor(t), "11 of 12 OpenBox handlers registered", "no OpenBox handler for [SessionEnd]")
}

func TestDoctorMuseFlagsASettingsFileMuseCannotRead(t *testing.T) {
	installedMuse(t)
	if err := os.WriteFile(providers.MuseSettingsPath(), []byte(`{"hooks": `), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := withMuseRunner(t, healthyMuse())
	section := museDoctor(t)
	mustContain(t, section, "FAIL: Muse cannot read this file", "drops EVERY hook", "not run: there are no readable OpenBox handlers to load")
	if calls.ran("exec ") {
		t.Error("the probe ran against a file nothing could load")
	}
}

func TestDoctorMuseFlagsAnEngineThatIsGone(t *testing.T) {
	installedMuse(t)
	path := providers.MuseSettingsPath()
	raw, _ := os.ReadFile(path)
	exe, _ := os.Executable()
	gone := strings.ReplaceAll(string(raw), exe, filepath.Join(t.TempDir(), "no-such-openbox"))
	if gone == string(raw) {
		t.Fatal("the installed handlers do not name this binary")
	}
	if err := os.WriteFile(path, []byte(gone), 0o600); err != nil {
		t.Fatal(err)
	}
	withMuseRunner(t, healthyMuse())
	mustContain(t, museDoctor(t), "the engine they run does not exist", "no-such-openbox")
}

// A gated handler that can fail open or be skipped is FAIL, the tier of a
// missing engine, not a warning.
func TestDoctorMuseRatesAGatedHandlerThatCannotGovernAsFail(t *testing.T) {
	mutations := map[string]func(group, handler map[string]any){
		"no successor":  func(_, h map[string]any) { delete(h, "onFailure") },
		"async":         func(_, h map[string]any) { h["async"] = true },
		"matcher":       func(g, _ map[string]any) { g["matcher"] = "Bash" },
		"short timeout": func(_, h map[string]any) { h["timeout"] = 5 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			installedMuse(t)
			path := providers.MuseSettingsPath()
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			group := doc["hooks"].(map[string]any)["PreToolUse"].([]any)[0].(map[string]any)
			mutate(group, group["hooks"].([]any)[0].(map[string]any))
			out, _ := json.Marshal(doc)
			if err := os.WriteFile(path, out, 0o600); err != nil {
				t.Fatal(err)
			}
			withMuseRunner(t, healthyMuse())
			mustContain(t, museDoctor(t), "FAIL: 12 of 12 OpenBox handlers registered, but a gated one can fail open or be skipped")
		})
	}
}

func TestDoctorMuseNoSettingsFile(t *testing.T) {
	installedMuse(t)
	if err := os.Remove(providers.MuseSettingsPath()); err != nil {
		t.Fatal(err)
	}
	withMuseRunner(t, healthyMuse())
	mustContain(t, museDoctor(t), "FAIL: no settings file, so no hook is registered")
}

func TestDoctorMuseHookRunEvidence(t *testing.T) {
	dir := t.TempDir()
	log := strings.Join([]string{
		`2026-09-30T10:00:00Z INFO hook.execution.terminal event=PreToolUse status="completed"`,
		`2026-09-30T10:00:01Z INFO something else`,
		`2026-09-30T10:00:02Z INFO hook.execution.terminal event=PreToolUse status="failed"`,
		`2026-09-30T10:00:03Z INFO hook.execution.terminal event=SessionStart status="completed"`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "boot.log"), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	installedMuse(t)
	withMuseTracingDir(t, dir)
	withMuseRunner(t, healthyMuse())
	mustContain(t, museDoctor(t), "ok: 3 hook run(s) recorded in boot.log, 2 completed")

	// A log with no hook record is unverified, never a failure.
	if err := os.WriteFile(filepath.Join(dir, "boot.log"), []byte("nothing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	section := museDoctor(t)
	mustContain(t, section, "unverified: boot.log records no hook run yet")
	if strings.Contains(section, "FAIL") {
		t.Errorf("an absent hook-run record failed:\n%s", section)
	}
}

func TestDoctorMuseHookRunsAreUnverifiedOnAVersionOutsideTheTestedRange(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "boot.log"), []byte(`hook.execution.terminal status="completed"`), 0o600); err != nil {
		t.Fatal(err)
	}
	installedMuse(t)
	withMuseTracingDir(t, dir)
	withMuseRunner(t, with(healthyMuse(), "--version", providers.MuseRunResult{Stdout: []byte("muse 1.9.0")}))
	mustContain(t, museDoctor(t), "unverified: Muse's local-tracing log is documented for the tested versions only")
}

func TestDoctorMuseSpoolRowPointsAtFlush(t *testing.T) {
	installedMuse(t)
	withMuseRunner(t, healthyMuse())
	sp := hookflow.Spool{Dir: providers.SpoolDirFor("muse")}
	if err := os.MkdirAll(sp.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	carry := filepath.Join(sp.Dir, "undelivered-s1.jsonl")
	if err := os.WriteFile(carry, []byte(`{"e":1}`+"\n"+`{"e":2}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if sp.BacklogCount() == 0 {
		t.Skip("the spool does not count this fixture as a backlog")
	}
	mustContain(t, museDoctor(t), "event(s) waiting in", "`openbox hook muse flush` delivers them now")
}

func TestParseMuseLoadProbe(t *testing.T) {
	c, ok := parseMuseLoadProbe("starting\nHooks: 12 runnable · 0 warnings\nbye")
	if !ok || c.Runnable != 12 || c.Warnings != 0 || len(c.OpenBoxWarnings) != 0 {
		t.Errorf("= %+v, %v", c, ok)
	}
	c, ok = parseMuseLoadProbe("Hooks: 3 runnable • 2 warnings")
	if !ok || c.Runnable != 3 || c.Warnings != 2 {
		t.Errorf("a different separator = %+v, %v", c, ok)
	}
	if _, ok := parseMuseLoadProbe("no summary here"); ok {
		t.Error("parsed a summary that is not there")
	}
}

func TestParseMuseManagedLane(t *testing.T) {
	for in, want := range map[string]string{
		"managed hooks lane required: true":  "yes",
		"Managed hook lane required = yes":   "yes",
		"managed_hooks_required: enabled":    "yes",
		"managed hooks lane required: false": "no",
		"managed hooks required: off":        "no",
		"nothing relevant":                   "unknown",
		"":                                   "unknown",
	} {
		if got := parseMuseManagedLane(in); got != want {
			t.Errorf("parseMuseManagedLane(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMuseHookRuns(t *testing.T) {
	c := parseMuseHookRuns("a\nhook.execution.terminal status=\"completed\"\nhook.execution.terminal {\"status\":\"completed\"}\nhook.execution.terminal status=\"timeout\"\n")
	if c.Terminal != 3 || c.Completed != 2 {
		t.Errorf("= %+v", c)
	}
}

// evidenceTrace points the local trace at a scratch directory and returns it,
// so a test can leave the reconciler's findings where doctor reads them.
func evidenceTrace(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "trace")
	t.Cleanup(trace.SetDefault(&trace.Writer{Dir: dir, Proc: "test"}))
	return dir
}

func TestDoctorMuseCountsUngatedActions(t *testing.T) {
	installedMuse(t)
	withMuseRunner(t, healthyMuse())
	evidenceTrace(t)
	for i := 0; i < 3; i++ {
		trace.Emit(trace.Record{Provider: "muse", SessionID: "s-1", Stage: trace.StageEvidenceGap, Outcome: "no_gate_record"})
	}
	section := museDoctor(t)
	mustContain(t, section,
		"WARNING: 3 ungated Muse actions in the last 7 days (see `openbox trace <session>`)",
		"256 KiB hook limit",
	)
}

func TestDoctorMuseSaysUnverifiedWhenTheReconcilerDisabledItself(t *testing.T) {
	installedMuse(t)
	withMuseRunner(t, healthyMuse())
	evidenceTrace(t)
	trace.Emit(trace.Record{Provider: "muse", SessionID: "s-1", Stage: trace.StageEvidenceReconcile, Outcome: "disabled"})
	section := museDoctor(t)
	mustContain(t, section, "unverified: the session-log reconciler stopped on a journal line it does not recognise")
	if strings.Contains(section, "ok: 0 ungated") {
		t.Errorf("an unverified reconciler reads as clean:\n%s", section)
	}
}

// TestDoctorMuseSaysWhyNoProxyLaneRecordsItsModelCalls the unrecorded row is
// only actionable with its reason: whether the relay can see Muse at all
// depends on the OS, and on macOS on what Muse was never shown to do.
func TestDoctorMuseSaysWhyNoProxyLaneRecordsItsModelCalls(t *testing.T) {
	for _, tc := range []struct {
		pac  bool
		want string
	}{
		{true, "proxy: not built; Muse is not shown to trust the relay's CA or follow the system PAC, and sends no session carrier the relay could attribute"},
		{false, "proxy: none; this OS has no system proxy activation, so the relay cannot see Muse"},
	} {
		prev := systemPACSupportedFn
		systemPACSupportedFn = func() bool { return tc.pac }
		installedMuse(t)
		withMuseRunner(t, healthyMuse())
		section := museDoctor(t)
		systemPACSupportedFn = prev
		mustContain(t, section, tc.want)
	}
}
