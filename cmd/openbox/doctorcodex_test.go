package main

import (
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
)

// TestDoctorSaysNothingAboutCodexOnAMachineThatNeverConfiguredIt pins the
// byte-identical claim: a machine with no OpenBox-owned Codex [otel] block
// must not gain a Codex section in `doctor`.
func TestDoctorSaysNothingAboutCodexOnAMachineThatNeverConfiguredIt(t *testing.T) {
	isolateHome(t)
	a, out, errb := testApp(nil)
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d; stderr=%q", code, errb.String())
	}
	s := out.String()
	if strings.Contains(s, "Codex telemetry lane") {
		t.Errorf("doctor reports a Codex lane section with no owned [otel] block:\n%s", s)
	}
	if strings.Contains(s, "Codex proxy/transport lane") {
		t.Errorf("doctor reports a Codex proxy section with no owned [otel] block:\n%s", s)
	}
}

// TestDoctorReportsAnElectedCodexLaneWithNothingListening is the WARNING this
// section exists for: an owned [otel] block that elects the telemetry lane
// while nothing answers on the port is a machine whose Codex model calls are
// silently unrecorded.
func TestDoctorReportsAnElectedCodexLaneWithNothingListening(t *testing.T) {
	isolateHome(t)
	nothingIsListening(t)
	configPath := providers.CodexConfigTOMLPath()
	if err := providers.WriteCodexOtel(configPath, "http://127.0.0.1:4318/v1/logs"); err != nil {
		t.Fatalf("seed WriteCodexOtel: %v", err)
	}

	a, out, errb := testApp(nil)
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d; stderr=%q", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "Codex telemetry lane") {
		t.Fatalf("doctor does not report the Codex lane section with an owned [otel] block:\n%s", s)
	}
	if !strings.Contains(s, "elected") || !strings.Contains(s, "telemetry") {
		t.Errorf("doctor does not report the Codex lane as elected:\n%s", s)
	}
	if !strings.Contains(s, "NO; nothing is listening") {
		t.Errorf("doctor does not report the Codex lane as unreachable:\n%s", s)
	}
	if !strings.Contains(s, "WARNING") {
		t.Errorf("an elected-but-unreachable Codex lane did not warn:\n%s", s)
	}
	if !strings.Contains(s, "Codex proxy/transport lane") || !strings.Contains(s, "UNVERIFIED") {
		t.Errorf("doctor does not disclose the unverified Codex proxy arm:\n%s", s)
	}
}
