package main

import (
	"strings"
	"testing"
)

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
