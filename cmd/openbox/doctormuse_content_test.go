package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// emitContentMiss files the record the content enrichers and the Stop hook
// write when a body could not be attached: stage capture.outcome, the
// provider's own outcome label, the reason in detail.
func emitContentMiss(outcome, reason string) {
	trace.Emit(trace.Record{
		Lane:      "telemetry",
		SessionID: "s-1",
		Stage:     trace.StageCapture,
		Outcome:   outcome,
		Detail:    map[string]any{"part": "response", "reason": reason},
	})
}

// installedMuseWithoutSessions rewrites the telemetry unit into the shape an
// install from before content capture left behind: --settings and
// --muse-settings, no --muse-sessions.
func installedMuseWithoutSessions(t *testing.T) {
	t.Helper()
	installedMuse(t)
	spec := laneservice.Telemetry(telemetry.DefaultAddr, "/home/dev/.claude/settings.json", false).
		WithMuseSettings(providers.MuseSettingsPath())
	writeTelemetryUnit(t, spec)
	a, _, _ := testApp(nil)
	args := a.readTelemetryUnit().args
	if !slices.Contains(args, laneservice.MuseSettingsFlag) || slices.Contains(args, laneservice.MuseSessionsFlag) {
		t.Fatalf("the fixture unit's argv is %q; it must carry --muse-settings and no --muse-sessions", args)
	}
}

func TestDoctorMuseContentSourceWarnsWhenTheUnitLacksTheSessionsFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the unit's arguments are not readable from a file on windows")
	}
	installedMuseWithoutSessions(t)
	withProbedMuse(t, healthyMuse())
	section := museDoctor(t)
	mustContain(t, section,
		"WARNING: Muse model-call content: not recorded; the telemetry unit",
		"carries no --muse-sessions",
		"`openbox init --provider muse` reinstalls it",
	)
}

func TestDoctorMuseContentSourceIsQuietWhenContentCaptureIsOff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the unit's arguments are not readable from a file on windows")
	}
	installedMuseWithoutSessions(t)
	withProbedMuse(t, healthyMuse())
	t.Setenv(devconfig.EnvContentCapture, "0")
	section := museDoctor(t)
	if strings.Contains(flat(section), "--muse-sessions") {
		t.Errorf("a machine that opted out of content capture is told to reinstall for it:\n%s", section)
	}
	mustContain(t, section, "content_capture is off")
}

func TestDoctorMuseContentSourceIsOkOnAFreshInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the unit's arguments are not readable from a file on windows")
	}
	installedMuse(t)
	withProbedMuse(t, healthyMuse())
	section := museDoctor(t)
	mustContain(t, section, "ok: Muse model-call content: the telemetry unit reads Muse's session journal")
	if strings.Contains(flat(section), "model-call content: not recorded") {
		t.Errorf("a fresh install reads as not recording content:\n%s", section)
	}
}

func TestDoctorMuseContentFormatIsUnverifiedAfterADriftFinding(t *testing.T) {
	installedMuse(t)
	withProbedMuse(t, healthyMuse())
	emitContentMiss("muse.content", "unverified")
	section := museDoctor(t)
	mustContain(t, section, "unverified: Muse's session journal no longer looks like the format the content reader was built on")
}

func TestDoctorMuseContentFormatIsOkWithoutADriftFinding(t *testing.T) {
	installedMuse(t)
	withProbedMuse(t, healthyMuse())
	emitContentMiss("muse.content", "timeout")
	mustContain(t, museDoctor(t), "ok: no drift in Muse's session journal recorded in the last 7 days")
}

func TestDoctorMuseContentFormatIsUnverifiedOnAnUntestedMuse(t *testing.T) {
	installedMuse(t)
	withProbedMuse(t, with(healthyMuse(), "--version", providers.MuseRunResult{Stdout: []byte("muse 1.5.0\n")}))
	mustContain(t, museDoctor(t), "unverified: Muse 1.5.0 is above the tested range, so its session journal has not been checked against the content reader")
}

func TestDoctorMuseContentRateCountsMissesByReason(t *testing.T) {
	installedMuse(t)
	withProbedMuse(t, healthyMuse())
	for _, reason := range []string{"timeout", "timeout", "stash_absent"} {
		emitContentMiss("muse.content", reason)
	}
	// Another provider's misses are not Muse's.
	emitContentMiss("codex.content", "no_join")
	mustContain(t, museDoctor(t), "3 content miss record(s) in the last 7 days (stash_absent 1, timeout 2)")
}

func TestDoctorMuseContentRateIgnoresRecordsOlderThanTheWindow(t *testing.T) {
	installedMuse(t)
	withProbedMuse(t, healthyMuse())
	dir := evidenceTrace(t)
	old := time.Now().Add(-9 * 24 * time.Hour)
	line, err := json.Marshal(trace.Record{TS: old, Stage: trace.StageCapture, Outcome: "muse.content", Detail: map[string]any{"reason": "unverified"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trace-"+old.UTC().Format("2006-01-02")+".jsonl"), append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	section := museDoctor(t)
	mustContain(t, section, "ok: no content misses recorded in the last 7 days")
	if strings.Contains(flat(section), "unverified: Muse's session journal no longer looks") {
		t.Errorf("a nine-day-old drift finding still reads as current:\n%s", section)
	}
}

// Codex's content finding lives in the Codex telemetry-lane section.
func TestDoctorCodexContentUnverifiedAfterADriftFinding(t *testing.T) {
	out := doctorCodexContent(t, func() { emitContentMiss("codex.content", "unverified") })
	for _, want := range []string{
		"unverified: Codex's rollout no longer looks like the format the content reader was built on",
		"1 content miss record(s) in the last 7 days (unverified 1)",
	} {
		if !strings.Contains(flat(out), want) {
			t.Errorf("the Codex lane section does not say %q:\n%s", want, out)
		}
	}
}

func TestDoctorCodexContentIsOkWithNoMisses(t *testing.T) {
	out := doctorCodexContent(t, func() {})
	for _, want := range []string{
		"ok: no drift in Codex's rollout recorded in the last 7 days",
		"ok: no content misses recorded in the last 7 days",
	} {
		if !strings.Contains(flat(out), want) {
			t.Errorf("the Codex lane section does not say %q:\n%s", want, out)
		}
	}
}

func TestDoctorCodexContentIgnoresMuseMisses(t *testing.T) {
	out := doctorCodexContent(t, func() { emitContentMiss("muse.content", "unverified") })
	if strings.Contains(flat(out), "Codex's rollout no longer looks") {
		t.Errorf("a Muse drift finding reads as Codex's:\n%s", out)
	}
}

// A Codex install from before rollout content has a telemetry unit with
// --codex-settings and no --codex-sessions: its daemon cannot find the rollout.
func TestDoctorCodexContentSourceWarnsWhenTheUnitLacksTheSessionsFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the unit's arguments are not readable from a file on windows")
	}
	out := doctorCodexContent(t, func() {
		writeTelemetryUnit(t, laneservice.Telemetry(telemetry.DefaultAddr, "", false).WithCodexSettings(providers.CodexConfigTOMLPath()))
	})
	for _, want := range []string{
		"WARNING: Codex model-call content: not recorded; the telemetry unit",
		"carries no --codex-sessions",
		"`openbox init --provider codex` reinstalls it",
	} {
		if !strings.Contains(flat(out), want) {
			t.Errorf("the Codex lane section does not say %q:\n%s", want, out)
		}
	}
}

func TestDoctorCodexContentSourceIsOkWhenTheUnitCarriesTheSessionsFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the unit's arguments are not readable from a file on windows")
	}
	out := doctorCodexContent(t, func() {
		writeTelemetryUnit(t, laneservice.Telemetry(telemetry.DefaultAddr, "", false).
			WithCodexSettings(providers.CodexConfigTOMLPath()).WithCodexSessions("/home/dev/.codex/sessions"))
	})
	if !strings.Contains(flat(out), "ok: Codex model-call content: the telemetry unit reads Codex's rollout from /home/dev/.codex/sessions") {
		t.Errorf("a unit with --codex-sessions is not reported ok:\n%s", out)
	}
}

func TestDoctorCodexContentSourceIsQuietWhenContentCaptureIsOff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the unit's arguments are not readable from a file on windows")
	}
	out := doctorCodexContent(t, func() {
		t.Setenv(devconfig.EnvContentCapture, "0")
		writeTelemetryUnit(t, laneservice.Telemetry(telemetry.DefaultAddr, "", false).WithCodexSettings(providers.CodexConfigTOMLPath()))
	})
	if strings.Contains(flat(out), "--codex-sessions") {
		t.Errorf("a machine that opted out of content capture is told to reinstall for it:\n%s", out)
	}
	if !strings.Contains(flat(out), "content_capture is off") {
		t.Errorf("the section does not say content_capture is off:\n%s", out)
	}
}

func doctorCodexContent(t *testing.T, seed func()) string {
	t.Helper()
	home := isolateHome(t)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))
	nothingIsListening(t)
	withSystemPACSupport(t, true)
	if err := providers.WriteCodexOtel(providers.CodexConfigTOMLPath(), "http://127.0.0.1:4318/v1/logs"); err != nil {
		t.Fatalf("seed WriteCodexOtel: %v", err)
	}
	evidenceTrace(t)
	seed()
	a, out, errb := testApp(map[string]string{"HOME": home})
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d; stderr=%q", code, errb.String())
	}
	s := out.String()
	i := strings.Index(s, "Codex telemetry lane")
	if i < 0 {
		t.Fatalf("no Codex lane section:\n%s", s)
	}
	section := s[i:]
	if j := strings.Index(section, "Codex proxy/transport lane"); j > 0 {
		section = section[:j]
	}
	return section
}
