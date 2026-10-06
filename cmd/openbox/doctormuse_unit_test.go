package main

import (
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

// writeTelemetryUnit replaces the installed telemetry unit with spec's.
func writeTelemetryUnit(t *testing.T, spec laneservice.Spec) {
	t.Helper()
	a, _, _ := testApp(nil)
	if _, err := spec.WriteUnit(runtime.GOOS, a.homeDir(), "/usr/local/bin/openbox"); err != nil {
		t.Fatal(err)
	}
}

// The model-call row claims the telemetry daemon records Muse, so it checks what
// the installed unit says the daemon was started with, not only that something
// listens on a port: a unit without --muse-settings builds no Muse emitter.
func TestDoctorMuseModelCallsFollowTheInstalledTelemetryUnit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the unit's arguments are not readable from a file on windows")
	}
	const recorded = "Muse model calls: recorded by the telemetry lane"
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		want  string
	}{
		{"unit without --muse-settings", func(t *testing.T) {
			writeTelemetryUnit(t, laneservice.Telemetry(telemetry.DefaultAddr, "", false))
		}, "carries no --muse-settings, so its daemon builds no Muse emitter"},
		{"unit reading another settings file", func(t *testing.T) {
			writeTelemetryUnit(t, laneservice.Telemetry(telemetry.DefaultAddr, "", false).WithMuseSettings("/elsewhere/settings.json"))
		}, "reads Muse's settings from /elsewhere/settings.json"},
		{"unit not installed", func(t *testing.T) {
			a, _, _ := testApp(nil)
			path := laneservice.Telemetry(telemetry.DefaultAddr, "", false).UnitPath(runtime.GOOS, a.homeDir())
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, "the telemetry unit is not installed"},
		{"Muse's own posture is off", func(t *testing.T) {
			t.Setenv(devconfig.EnvTelemetry, "false")
		}, "Muse's own posture telemetry=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installedMuse(t)
			withProbedMuse(t, healthyMuse())
			tc.setup(t)
			section := museDoctor(t)
			mustContain(t, section, "WARNING: Muse model calls: not recorded", tc.want)
			if strings.Contains(flat(section), recorded) {
				t.Errorf("a daemon that cannot record Muse reads as recording:\n%s", section)
			}
		})
	}
}

// The daemon matches Muse's endpoint against its own --addr, so a unit
// listening elsewhere is the receiver Muse must point at, and the port probe is
// of that address.
func TestDoctorMuseModelCallsUseTheUnitsOwnAddress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the unit's arguments are not readable from a file on windows")
	}
	installedMuse(t)
	withProbedMuse(t, healthyMuse())
	const other = "127.0.0.1:4318"
	writeTelemetryUnit(t, laneservice.Telemetry(other, "", false).WithMuseSettings(providers.MuseSettingsPath()))
	var probed []string
	prev := portOccupied
	portOccupied = func(addr string) (bool, string) { probed = append(probed, addr); return true, "" }
	t.Cleanup(func() { portOccupied = prev })

	// Muse still exports to the default port: not this daemon's.
	mustContain(t, museDoctor(t), "is not this receiver (127.0.0.1:4318)")

	raw, err := os.ReadFile(providers.MuseSettingsPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(providers.MuseSettingsPath(), []byte(strings.Replace(string(raw), "127.0.0.1:8789", other, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	probed = nil
	mustContain(t, museDoctor(t), "Muse model calls: recorded by the telemetry lane")
	if !slices.Contains(probed, other) {
		t.Errorf("the port probe asked %v, want the unit's own address %s", probed, other)
	}
}

func TestParseUnitArgsRecoversWhatLaneserviceRendered(t *testing.T) {
	spec := laneservice.Telemetry("127.0.0.1:8789", `/a "b"/100%/settings.json`, false).WithMuseSettings("/m/settings.json")
	want := spec.Argv("/usr/local/bin/openbox")
	for goos, text := range map[string]string{
		"darwin": spec.LaunchdPlist("/home/x", "/usr/local/bin/openbox"),
		"linux":  spec.SystemdUnit("/usr/local/bin/openbox"),
	} {
		if got := parseUnitArgs(goos, text); !slices.Equal(got, want) {
			t.Errorf("%s: parsed %q, want %q", goos, got, want)
		}
	}
	if got := parseUnitArgs("windows", "anything"); got != nil {
		t.Errorf("windows: parsed %q from a file this OS does not have", got)
	}
}
