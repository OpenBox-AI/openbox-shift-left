package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/muse"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
	"github.com/tidwall/sjson"
)

// healthyMuse scripts a muse 1.4.0 whose hooks all load and whose policy is
// clean; a test overrides one answer to reach each row's other states.
//
// Muse prints its `Hooks:` summary only when a hook warns, so a clean probe
// prints nothing; `muse config status` lists the four enterprise sources.
func healthyMuse() museOutputs {
	return museOutputs{
		"--version":            {Stdout: []byte("muse 1.4.0\n")},
		"exec --provider echo": {},
		"config status":        {Stdout: []byte(absentPolicyStatus)},
	}
}

const absentPolicyStatus = `Enterprise configuration status
Generation: sha256:0000
Sources:
  plane=defaults source_class=system_file state=absent
  plane=policy source_class=system_file state=absent
  plane=defaults source_class=macos_managed_preferences state=absent
  plane=policy source_class=macos_managed_preferences state=absent
`

// withProbedMuse is withMuseRunner for a muse whose hooks run: the echo probe
// leaves the `hook.in` record an OpenBox handler writes when Muse invokes it.
// The record goes to a scratch trace, so nothing reaches the developer's own.
func withProbedMuse(t *testing.T, outputs museOutputs) *museCalls {
	t.Helper()
	calls := withMuseRunner(t, outputs)
	evidenceTrace(t)
	inner := providers.MuseRunner
	providers.MuseRunner = func(ctx context.Context, dir string, args ...string) (providers.MuseRunResult, error) {
		res, err := inner(ctx, dir, args...)
		if err == nil && len(args) > 0 && args[0] == "exec" {
			trace.Emit(trace.Record{Provider: "muse", SessionID: "probe", EventType: "SessionStart", Stage: trace.StageHookIn, Outcome: "ok"})
		}
		return res, err
	}
	return calls
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
	withReceiverListening(t, true)
}

// withReceiverListening answers doctor's probe of the telemetry receiver's
// port. The fake supervisor reports nothing listening; a healthy machine has
// the receiver up.
func withReceiverListening(t *testing.T, up bool) {
	t.Helper()
	prev := portOccupied
	portOccupied = func(string) (bool, string) { return up, "" }
	t.Cleanup(func() { portOccupied = prev })
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
	withProbedMuse(t, healthyMuse())
	section := museDoctor(t)
	mustContain(t, section,
		"ok: muse 1.4.0 (supported: >= 1.4.0, tested below 1.5.0)",
		"ok: "+museHandlers(0)+" OpenBox handlers registered, every gated one with a deny-only successor",
		"ok: Muse loaded the hooks with 0 warnings and OpenBox's handlers ran during the probe",
		"unverified: no local-tracing log",
		"ok: no policy present (system_file absent, macos_managed_preferences absent). managed lane required: unknown",
		"Muse model calls: recorded by the telemetry lane (model, tokens, response id; bodies are reported in the content rows below); tool, prompt and model-call gating still enforced",
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
	calls := withProbedMuse(t, healthyMuse())
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
	n := muse.ExpectedHandlers()
	cases := []struct {
		name   string
		result providers.MuseRunResult
		want   string
	}{
		{"fewer than registered", providers.MuseRunResult{Stderr: []byte("Hooks: 5 runnable · 1 warnings")}, fmt.Sprintf("FAIL: Muse found 5 runnable hooks, fewer than the %d OpenBox registered", n)},
		{"more than registered", providers.MuseRunResult{Stderr: []byte(fmt.Sprintf("Hooks: %d runnable · 1 warnings\nwarning: team hook x has a bad matcher", n+1))}, fmt.Sprintf("WARNING: Muse found %d runnable hooks (%d expected) and 1 warning(s), none naming OpenBox", n+1, n)},
		{"foreign warning", providers.MuseRunResult{Stderr: []byte(fmt.Sprintf("Hooks: %d runnable · 1 warnings\nwarning: team hook x has a bad matcher", n))}, fmt.Sprintf("WARNING: Muse found %d runnable hooks (%d expected) and 1 warning(s), none naming OpenBox", n, n)},
		{"openbox warning", providers.MuseRunResult{Stderr: []byte("Hooks: 10 runnable · 1 warnings\nwarning: hook \"/o/openbox\" hook muse PreToolUse: unknown key")}, "FAIL: Muse loaded 10 hooks and warned about OpenBox's"},
		{"clean load prints nothing", providers.MuseRunResult{}, "ok: Muse loaded the hooks with 0 warnings and OpenBox's handlers ran during the probe"},
		{"failed with no summary", providers.MuseRunResult{ExitCode: 3, Stderr: []byte("boom")}, "WARNING: the echo probe exited 3 without a `Hooks: N runnable` summary"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installedMuse(t)
			withProbedMuse(t, with(healthyMuse(), "exec --provider echo", c.result))
			mustContain(t, museDoctor(t), c.want)
		})
	}
}

// A clean load is silent, so silence alone proves nothing: with no muse hook
// invocation in the trace after the probe began, the handlers did not run.
func TestDoctorMuseCleanProbeNeedsAHookToHaveRun(t *testing.T) {
	installedMuse(t)
	withMuseRunner(t, healthyMuse()) // a muse that loads nothing: no hook.in record
	evidenceTrace(t)
	section := museDoctor(t)
	mustContain(t, section, "WARNING: hooks did not run during the probe")
	if strings.Contains(section, "ok: Muse loaded") {
		t.Errorf("a silent probe with no hook run reads as ok:\n%s", section)
	}
}

// A hook record from before the probe began is not evidence the probe's own
// session ran a handler.
func TestDoctorMuseIgnoresAHookRecordOlderThanTheProbe(t *testing.T) {
	installedMuse(t)
	withMuseRunner(t, healthyMuse())
	dir := evidenceTrace(t)
	old := time.Now().Add(-time.Hour)
	line, err := json.Marshal(trace.Record{TS: old, Provider: "muse", SessionID: "earlier", Stage: trace.StageHookIn, Outcome: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trace-"+old.UTC().Format("2006-01-02")+".jsonl"), append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	mustContain(t, museDoctor(t), "WARNING: hooks did not run during the probe")
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
			calls := withProbedMuse(t, healthyMuse())
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
	const presentStatus = `Enterprise configuration status
Generation: sha256:1
Sources:
  plane=defaults source_class=system_file state=absent
  plane=policy source_class=system_file state=present
  plane=policy source_class=macos_managed_preferences state=absent
`
	cases := []struct {
		name   string
		status providers.MuseRunResult
		want   []string
	}{
		{"absent everywhere", providers.MuseRunResult{Stdout: []byte(absentPolicyStatus)},
			[]string{"ok: no policy present (system_file absent, macos_managed_preferences absent). managed lane required: unknown"}},
		{"present", providers.MuseRunResult{Stdout: []byte(presentStatus)},
			[]string{"ok: policy present (system_file present, macos_managed_preferences absent). managed lane required: unknown"}},
		{"present and says required", providers.MuseRunResult{Stdout: []byte(presentStatus + "managed hooks lane required: true\n")},
			[]string{"managed lane required: yes", "see deployments/managed/muse/README-mdm.md"}},
		{"not parsed when no policy is present", providers.MuseRunResult{Stdout: []byte(absentPolicyStatus + "managed hooks lane required: true\n")},
			[]string{"managed lane required: unknown"}},
		{"rejected source", providers.MuseRunResult{Stdout: []byte("Sources:\n  plane=policy source_class=system_file state=invalid\n")},
			[]string{"FAIL: a Muse policy source is not valid (system_file invalid)"}},
		{"status fails", providers.MuseRunResult{ExitCode: 2},
			[]string{"WARNING: `muse config status` exited 2", "managed lane required: unknown"}},
		{"no source lines", providers.MuseRunResult{Stdout: []byte("everything is fine")},
			[]string{"unverified: `muse config status` listed no policy source. managed lane required: unknown"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installedMuse(t)
			calls := withProbedMuse(t, with(healthyMuse(), "config status", c.status))
			mustContain(t, museDoctor(t), c.want...)
			if calls.ran("config validate") {
				t.Error("`muse config validate` needs --plane and --file and checks one document; doctor must not call it bare")
			}
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
	withProbedMuse(t, healthyMuse())
	mustContain(t, museDoctor(t), "WARNING: "+museHandlers(0)+" OpenBox handlers registered; --home does not match this machine's OpenBox home on")

	// And a handler that is simply gone.
	if err := os.WriteFile(path, []byte(strings.Replace(stripped, "hook muse SessionEnd", "hook codex SessionEnd", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	mustContain(t, museDoctor(t), museHandlers(1)+" OpenBox handlers registered", "no OpenBox handler for [SessionEnd]")
}

func TestDoctorMuseFlagsASettingsFileMuseCannotRead(t *testing.T) {
	installedMuse(t)
	if err := os.WriteFile(providers.MuseSettingsPath(), []byte(`{"hooks": `), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := withProbedMuse(t, healthyMuse())
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
	withProbedMuse(t, healthyMuse())
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
			withProbedMuse(t, healthyMuse())
			mustContain(t, museDoctor(t), "FAIL: "+museHandlers(0)+" OpenBox handlers registered, but a gated one can fail open or be skipped")
		})
	}
}

func TestDoctorMuseNoSettingsFile(t *testing.T) {
	installedMuse(t)
	if err := os.Remove(providers.MuseSettingsPath()); err != nil {
		t.Fatal(err)
	}
	withProbedMuse(t, healthyMuse())
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
	withProbedMuse(t, healthyMuse())
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
	withProbedMuse(t, with(healthyMuse(), "--version", providers.MuseRunResult{Stdout: []byte("muse 1.9.0")}))
	mustContain(t, museDoctor(t), "unverified: Muse's local-tracing log is documented for the tested versions only")
}

func TestDoctorMuseSpoolRowPointsAtFlush(t *testing.T) {
	installedMuse(t)
	withProbedMuse(t, healthyMuse())
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
	c, ok := parseMuseLoadProbe("starting\nHooks: 12 runnable · 1 warnings\nbye")
	if !ok || c.Runnable != 12 || c.Warnings != 1 || len(c.OpenBoxWarnings) != 0 {
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

func TestParseMuseConfigStatus(t *testing.T) {
	got := parseMuseConfigStatus(absentPolicyStatus)
	if len(got) != 4 {
		t.Fatalf("sources = %+v", got)
	}
	if got[1] != (museConfigSource{Plane: "policy", Class: "system_file", State: "absent"}) {
		t.Errorf("second source = %+v", got[1])
	}
	if got := parseMuseConfigStatus("Enterprise configuration status\nGeneration: sha256:x\n"); len(got) != 0 {
		t.Errorf("parsed sources that are not there: %+v", got)
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
	withProbedMuse(t, healthyMuse())
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
	withProbedMuse(t, healthyMuse())
	evidenceTrace(t)
	trace.Emit(trace.Record{Provider: "muse", SessionID: "s-1", Stage: trace.StageEvidenceReconcile, Outcome: "disabled"})
	section := museDoctor(t)
	mustContain(t, section, "unverified: the session-log reconciler stopped because Muse's session.jsonl no longer looks like the format observed")
	if strings.Contains(section, "ok: 0 ungated") {
		t.Errorf("an unverified reconciler reads as clean:\n%s", section)
	}
}

// TestDoctorMuseSaysWhyThereIsNoProxyLane: measured on Muse 1.4.1, so the same
// on every OS.
func TestDoctorMuseSaysWhyThereIsNoProxyLane(t *testing.T) {
	for _, pac := range []bool{true, false} {
		prev := systemPACSupportedFn
		systemPACSupportedFn = func() bool { return pac }
		installedMuse(t)
		withProbedMuse(t, healthyMuse())
		section := museDoctor(t)
		systemPACSupportedFn = prev
		mustContain(t, section, "proxy: none: Muse ignores the system proxy and rejects the relay's certificate")
	}
}

// TestDoctorMuseSaysWhyModelCallsAreNotRecorded: the model-call row is only
// actionable with its reason, one per way the lane can be off.
func TestDoctorMuseSaysWhyModelCallsAreNotRecorded(t *testing.T) {
	const gating = "tool, prompt and model-call gating still enforced"
	edit := func(t *testing.T, mutate func(string) string) {
		t.Helper()
		path := providers.MuseSettingsPath()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(mutate(string(raw))), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(string) string
		up     bool
		muse   museOutputs
		want   string
	}{
		{"the telemetry key was removed", func(s string) string { return sjsonDelete(t, s, "telemetry") }, true, healthyMuse(),
			"WARNING: Muse model calls: not recorded by the telemetry lane: Muse's settings.json has no telemetry block, so Muse exports to its own destinations."},
		{"the destination is Meta's", func(s string) string {
			return strings.Replace(s, `"destination":"external"`, `"destination":"meta"`, 1)
		}, true, healthyMuse(),
			`Muse's telemetry.destination is "meta", not "external"`},
		{"the endpoint left loopback", func(s string) string {
			return strings.Replace(s, "http://127.0.0.1:8789", "https://otel.corp.example", 1)
		}, true, healthyMuse(),
			"is not a loopback URL"},
		{"the endpoint points at another port", func(s string) string { return strings.Replace(s, "127.0.0.1:8789", "127.0.0.1:4318", 1) }, true, healthyMuse(),
			"is not this receiver (127.0.0.1:8789)"},
		{"telemetry is switched off", func(s string) string { return strings.Replace(s, `"enabled":true`, `"enabled":false`, 1) }, true, healthyMuse(),
			"Muse's telemetry.enabled is not true"},
		{"the receiver is not listening", func(s string) string { return s }, false, healthyMuse(),
			"Muse's telemetry is pointed at this receiver but nothing is listening on 127.0.0.1:8789. `openbox init --provider muse` brings the receiver back; " + gating},
		{"a policy forces telemetry off", func(s string) string { return s }, true,
			with(healthyMuse(), "config status", providers.MuseRunResult{Stdout: []byte(absentPolicyStatus +
				"  plane=policy source_class=file state=present\n  privacy.telemetry: force_off\n")}),
			"WARNING: Muse model calls: not recorded; a policy forces privacy.telemetry off, so Muse exports nothing; " + gating},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installedMuse(t)
			withProbedMuse(t, tc.muse)
			withReceiverListening(t, tc.up)
			edit(t, tc.mutate)
			section := museDoctor(t)
			mustContain(t, section, tc.want)
			if strings.Contains(flat(section), "Muse model calls: recorded by the telemetry lane") {
				t.Errorf("a lane that is off reads as recording:\n%s", section)
			}
		})
	}
}

// TestDoctorMuseSaysWhenAPolicyMayForceTelemetryOff: `muse config status`
// names policy sources but not what they require, so a present policy is
// unverified, never assumed to allow the export and never assumed to forbid it.
func TestDoctorMuseSaysWhenAPolicyMayForceTelemetryOff(t *testing.T) {
	installedMuse(t)
	present := strings.Replace(absentPolicyStatus, "plane=policy source_class=system_file state=absent", "plane=policy source_class=system_file state=present", 1)
	withProbedMuse(t, with(healthyMuse(), "config status", providers.MuseRunResult{Stdout: []byte(present)}))
	section := museDoctor(t)
	mustContain(t, section,
		"unverified: whether a policy forces privacy.telemetry off",
		"Muse model calls: recorded by the telemetry lane")
}

func TestParseMuseTelemetryPolicy(t *testing.T) {
	for out, want := range map[string]string{
		"privacy.telemetry: force_off":   museTelemetryPolicyForcedOff,
		"privacy.telemetry = forced-off": museTelemetryPolicyForcedOff,
		"privacy_telemetry force off":    museTelemetryPolicyForcedOff,
		"privacy.telemetry: default":     museTelemetryPolicyUnknown,
		"privacy.analytics: force_off":   museTelemetryPolicyUnknown,
		absentPolicyStatus:               museTelemetryPolicyUnknown,
		"":                               museTelemetryPolicyUnknown,
	} {
		if got := parseMuseTelemetryPolicy(out); got != want {
			t.Errorf("parseMuseTelemetryPolicy(%q) = %q, want %q", out, got, want)
		}
	}
}

// sjsonDelete removes one top-level key from a settings document.
func sjsonDelete(t *testing.T, doc, key string) string {
	t.Helper()
	out, err := sjson.Delete(doc, key)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// museHandlers renders doctor's "N of M" for a machine missing `missing`
// handlers, from the adapter's own count rather than a literal that every new
// hook would stale.
func museHandlers(missing int) string {
	n := muse.ExpectedHandlers()
	return fmt.Sprintf("%d of %d", n-missing, n)
}
