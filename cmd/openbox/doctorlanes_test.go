package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
)

// settings.json is Claude Code's election alone. On a machine where Claude Code
// is not installed, its "no lane emits" verdict is not about this machine, and
// Muse's telemetry lane does record model calls.
func TestDoctorLaneBlockDoesNotDenyModelCallsWhenClaudeCodeIsAbsent(t *testing.T) {
	installedMuse(t)
	withProbedMuse(t, healthyMuse())
	a, out, _ := testApp(nil)
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d", code)
	}
	got := flat(out.String())
	if strings.Contains(got, "ABSENT rather than merely incomplete") || strings.Contains(got, "elected (none)") {
		t.Errorf("a Muse-only machine is told no lane emits model-call turns:\n%s", out.String())
	}
	if !strings.Contains(got, "Claude Code is not installed, so its lane election does not apply") ||
		!strings.Contains(got, "`model calls` row") {
		t.Errorf("the pointer to the Codex and Muse sections is missing:\n%s", out.String())
	}
}

// With Claude Code present the election lines are unchanged in substance and
// say whose they are.
func TestDoctorLaneBlockKeepsClaudeCodeElectionWhenPresent(t *testing.T) {
	installedMuse(t)
	withProbedMuse(t, healthyMuse())
	a, out, _ := testApp(nil)
	settings := gatewayservice.SettingsPath(a.homeDir())
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d", code)
	}
	got := flat(out.String())
	if !strings.Contains(got, "elected (none)") || !strings.Contains(got, "ABSENT rather than merely incomplete") {
		t.Errorf("Claude Code's election output changed:\n%s", out.String())
	}
	if !strings.Contains(got, "Claude Code's election") {
		t.Errorf("the election lines do not say they are Claude Code's:\n%s", out.String())
	}
	if strings.Contains(got, "Claude Code is not installed") {
		t.Errorf("the absent row printed with Claude Code present:\n%s", out.String())
	}
}

// The generic lane block is built from Claude Code's settings.json election. On
// a machine where Muse is what uses the telemetry receiver, it must not say the
// lane is unconfigured.
func TestDoctorLaneBlockDoesNotMisreportTheTelemetryLaneOnAMuseOnlyMachine(t *testing.T) {
	installedMuse(t)
	withProbedMuse(t, healthyMuse())
	a, out, errb := testApp(nil)
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d; stderr=%q", code, errb.String())
	}
	got := flat(out.String())
	if strings.Contains(out.String(), "\nTelemetry lane\n") {
		t.Errorf("a lane Muse uses gets the unhealthy block:\n%s", out.String())
	}
	if !strings.Contains(got, "telemetry listening on 127.0.0.1:8789; routed") {
		t.Errorf("the telemetry lane Muse uses is not summarized as routed:\n%s", out.String())
	}
}

// When the lane is down, its block names who uses it instead of denying it is
// configured.
func TestDoctorTelemetryLaneBlockNamesMuseWhenTheReceiverIsDown(t *testing.T) {
	installedMuse(t)
	withProbedMuse(t, healthyMuse())
	withReceiverListening(t, false)
	a, out, _ := testApp(nil)
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d", code)
	}
	got := flat(out.String())
	if !strings.Contains(got, "configured yes; muse from their own config") || !strings.Contains(got, "reachable NO; nothing is listening") {
		t.Errorf("the down lane Muse uses is not reported as configured and unreachable:\n%s", out.String())
	}
}
