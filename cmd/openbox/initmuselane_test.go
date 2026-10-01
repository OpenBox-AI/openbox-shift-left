package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

const museOwnSettings = "{\n  \"schema_version\": 1,\n  \"theme\": \"dark\"\n}\n"

// museLaneHarness is newLaneHarness with Muse's settings.json seeded under the
// harness home, which os.UserHomeDir resolves through $HOME.
func museLaneHarness(t *testing.T, settings string) (*laneHarness, string) {
	t.Helper()
	skipUnlessSupervised(t)
	h := newLaneHarness(t)
	t.Setenv("HOME", h.home)
	path := providers.MuseSettingsPath()
	if settings != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return h, path
}

func TestMuseIsLaneCapableForTelemetryOnly(t *testing.T) {
	if !laneCapable("muse") {
		t.Error("laneCapable(muse) = false")
	}
	if hasTransportArm(provider.Muse) {
		t.Error("muse has no transport arm: it ignores the system proxy and rejects the relay's certificate")
	}
	for _, installing := range []provider.Name{provider.Muse, provider.ClaudeCode, provider.Codex} {
		if hasProvider(derivedTransportProviders(t.TempDir(), installing), provider.Muse) {
			t.Errorf("the transport set for an install of %s names muse", installing)
		}
	}
}

// TestMuseTelemetryIsWrittenOnlyAfterTheDaemonIsProvenUp: the same safety
// property every lane has. The settings are read from inside the readiness
// probe, because the file is the invariant: pointing Muse at a port with
// nothing behind it would make its export fail while init printed success.
func TestMuseTelemetryIsWrittenOnlyAfterTheDaemonIsProvenUp(t *testing.T) {
	h, settings := museLaneHarness(t, museOwnSettings)
	a, _, _ := testApp(map[string]string{"HOME": h.home})

	var atProbe string
	probed := false
	realWait := waitForListenerFn
	waitForListenerFn = func(addr string, d time.Duration) bool {
		probed = true
		b, _ := os.ReadFile(settings)
		atProbe = string(b)
		return realWait(addr, d)
	}
	t.Cleanup(func() { waitForListenerFn = realWait })

	if _, err := a.setupMuseTelemetry(h.home, "127.0.0.1:18789", false); err != nil {
		t.Fatalf("setupMuseTelemetry: %v", err)
	}
	if !probed {
		t.Fatal("the readiness probe never ran, so this test proves nothing about the order")
	}
	if atProbe != museOwnSettings {
		t.Errorf("settings.json changed before the daemon was proven listening:\n%s", atProbe)
	}
	raw, _ := os.ReadFile(settings)
	tel := gjson.GetBytes(raw, "telemetry")
	if !tel.Get("enabled").Bool() || tel.Get("destination").String() != "external" || tel.Get("endpoint").String() != "http://127.0.0.1:18789" {
		t.Errorf("telemetry = %s", tel.Raw)
	}
	if gjson.GetBytes(raw, "theme").String() != "dark" {
		t.Error("a foreign key was lost")
	}
}

// TestAMuseInstallThatFailsAfterTheUnitIsWrittenRemovesTheUnitAndTouchesNoSettings
// covers the three ways past WriteUnit: nothing ever listens, the supervisor
// refuses the unit, and the pointer write itself is refused.
func TestAMuseInstallThatFailsAfterTheUnitIsWrittenRemovesTheUnitAndTouchesNoSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
		arrange  func(h *laneHarness)
		wantErr  string
		// unitNeverWritten: the refusal is decided before the unit is touched.
		unitNeverWritten bool
	}{
		{"never listens", museOwnSettings, func(h *laneHarness) { h.listening = false }, "did not start listening", false},
		{"supervisor refuses", museOwnSettings, func(h *laneHarness) { h.startFails = true }, "NOT written", false},
		{"settings Muse cannot read", "{\"schema_version\": 1,", func(*laneHarness) {}, "refusing to modify", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, settings := museLaneHarness(t, tc.settings)
			tc.arrange(h)
			a, out, _ := testApp(map[string]string{"HOME": h.home})

			_, err := a.setupMuseTelemetry(h.home, "127.0.0.1:18789", false)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if len(h.units) != 0 {
				t.Errorf("a failed install left units behind: %v", h.units)
			}
			if !tc.unitNeverWritten && !strings.Contains(out.String(), "rolled back") {
				t.Errorf("the rollback was silent:\n%s", out.String())
			}
			if tc.unitNeverWritten && len(h.commands) != 0 {
				t.Errorf("a refusal decided from Muse's files still drove the supervisor: %v", h.commands)
			}
			if b, _ := os.ReadFile(settings); string(b) != tc.settings {
				t.Errorf("settings.json changed on a failed install:\n%s", b)
			}
			if _, err := os.Stat(providers.MusePriorSettingsPath(h.home)); !os.IsNotExist(err) {
				t.Error("a restore record was written for a pointer that was never written")
			}
		})
	}
}

// TestTheSharedTelemetryUnitKeepsEveryToolsSettingsPath: one receiver serves
// all three tools, so an install for any one keeps the paths of the tools
// already installed, in either order -- and leaves out a tool never touched.
func TestTheSharedTelemetryUnitKeepsEveryToolsSettingsPath(t *testing.T) {
	unit := func(t *testing.T, h *laneHarness) string {
		t.Helper()
		raw, err := os.ReadFile(laneservice.Telemetry("", "", false).UnitPath(runtime.GOOS, h.home))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	addr := "127.0.0.1:18789"

	t.Run("a Claude Code only install carries no Muse path", func(t *testing.T) {
		h, _ := museLaneHarness(t, museOwnSettings)
		t.Setenv("CODEX_HOME", filepath.Join(h.home, ".codex"))
		a, _, _ := testApp(map[string]string{"HOME": h.home})
		if _, err := a.setupTelemetry(h.home, addr, false); err != nil {
			t.Fatal(err)
		}
		if u := unit(t, h); strings.Contains(u, laneservice.MuseSettingsFlag) {
			t.Errorf("unit carries %s with no Muse install behind it:\n%s", laneservice.MuseSettingsFlag, u)
		}
	})

	t.Run("a Muse install carries the path", func(t *testing.T) {
		h, settings := museLaneHarness(t, museOwnSettings)
		t.Setenv("CODEX_HOME", filepath.Join(h.home, ".codex"))
		a, _, _ := testApp(map[string]string{"HOME": h.home})
		if _, err := a.setupMuseTelemetry(h.home, addr, false); err != nil {
			t.Fatal(err)
		}
		u := unit(t, h)
		if !strings.Contains(u, laneservice.MuseSettingsFlag) || !strings.Contains(u, settings) {
			t.Errorf("the unit does not carry Muse's settings path:\n%s", u)
		}
		if strings.Contains(u, laneservice.CodexSettingsFlag) {
			t.Errorf("a Muse-only install carries Codex's path:\n%s", u)
		}
	})

	t.Run("a later Claude Code or Codex install keeps it", func(t *testing.T) {
		h, settings := museLaneHarness(t, museOwnSettings)
		t.Setenv("CODEX_HOME", filepath.Join(h.home, ".codex"))
		a, _, _ := testApp(map[string]string{"HOME": h.home})
		if _, err := a.setupMuseTelemetry(h.home, addr, false); err != nil {
			t.Fatal(err)
		}
		for name, setup := range map[string]func(string, string, bool) (int, error){
			"claude-code": a.setupTelemetry, "codex": a.setupCodexTelemetry,
		} {
			if _, err := setup(h.home, addr, false); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if u := unit(t, h); !strings.Contains(u, laneservice.MuseSettingsFlag) || !strings.Contains(u, settings) {
				t.Errorf("a %s install stripped Muse's settings path from the shared unit:\n%s", name, u)
			}
		}
	})

	t.Run("a Muse install after Codex keeps Codex's", func(t *testing.T) {
		h, _ := museLaneHarness(t, museOwnSettings)
		t.Setenv("CODEX_HOME", filepath.Join(h.home, ".codex"))
		a, _, _ := testApp(map[string]string{"HOME": h.home})
		if _, err := a.setupCodexTelemetry(h.home, addr, false); err != nil {
			t.Fatal(err)
		}
		if _, err := a.setupMuseTelemetry(h.home, addr, false); err != nil {
			t.Fatal(err)
		}
		if u := unit(t, h); !strings.Contains(u, laneservice.CodexSettingsFlag) {
			t.Errorf("a Muse install stripped Codex's settings path:\n%s", u)
		}
	})
}

// TestInitMuseInstallsTheTelemetryLaneAndASecondInitChangesNothing is the
// two-init invariant for the lane: the settings file, the restore record and
// the unit are byte-identical after the second run, and the record still holds
// the developer's original absence rather than OpenBox's own value.
func TestInitMuseInstallsTheTelemetryLaneAndASecondInitChangesNothing(t *testing.T) {
	skipUnlessSupervised(t)
	isolateHome(t)
	seedCredentials(t, "muse")
	withMuseRunner(t, museVersion("1.4.0"))
	home := os.Getenv("HOME")

	snapshot := func() (settings, record, unit string) {
		s, _ := os.ReadFile(providers.MuseSettingsPath())
		r, _ := os.ReadFile(providers.MusePriorSettingsPath(home))
		u, _ := os.ReadFile(laneservice.Telemetry("", "", false).UnitPath(runtime.GOOS, home))
		return string(s), string(r), string(u)
	}

	a, out, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "muse"}); code != exitOK {
		t.Fatalf("first init exit = %d; stderr=%q", code, errb.String())
	}
	s1, r1, u1 := snapshot()
	tel := gjson.Get(s1, "telemetry")
	if !tel.Get("enabled").Bool() || tel.Get("destination").String() != "external" ||
		tel.Get("endpoint").String() != "http://"+telemetry.DefaultAddr {
		t.Fatalf("Muse's telemetry was not pointed at the receiver: %s", tel.Raw)
	}
	if !strings.Contains(u1, laneservice.MuseSettingsFlag) {
		t.Errorf("the unit does not carry %s:\n%s", laneservice.MuseSettingsFlag, u1)
	}
	if gjson.Get(r1, "keys.telemetry.present").Bool() {
		t.Errorf("the record claims a value existed before init:\n%s", r1)
	}
	// Metadata only: the disclosure says so, and says where the export used to go.
	for _, want := range []string{"REDIRECTED", "no content", "openbox uninstall"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the install report lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "prompt and tool") {
		t.Errorf("the report claims Muse's export carries prompt and tool content:\n%s", out.String())
	}

	b, out2, errb2 := testApp(nil)
	if code := b.run([]string{"init", "--provider", "muse"}); code != exitOK {
		t.Fatalf("second init exit = %d; stderr=%q\n%s", code, errb2.String(), out2.String())
	}
	s2, r2, u2 := snapshot()
	if s2 != s1 || r2 != r1 || u2 != u1 {
		t.Errorf("a second init changed something:\nsettings same=%v record same=%v unit same=%v", s2 == s1, r2 == r1, u2 == u1)
	}
}

// TestUninstallRestoresMusesTelemetryExactly is the reversal for the lane: the
// previous value comes back byte for byte, an absent one is deleted, and a
// value the developer changed after init is left and said so.
func TestUninstallRestoresMusesTelemetryExactly(t *testing.T) {
	const withPrior = "{\n  \"schema_version\": 1,\n  \"telemetry\": {\"enabled\": true,  \"destination\": \"meta\"},\n  \"theme\": \"dark\"\n}\n"
	for _, tc := range []struct {
		name, original string
		drift          bool
		wantOut        string
	}{
		{"prior value", withPrior, false, "restored"},
		{"no prior value", museOwnSettings, false, "removed"},
		{"changed after init", museOwnSettings, true, "left alone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skipUnlessSupervised(t)
			isolateHome(t)
			seedCredentials(t, "muse")
			withMuseRunner(t, museVersion("1.4.0"))
			path := providers.MuseSettingsPath()
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.original), 0o600); err != nil {
				t.Fatal(err)
			}

			a, _, errb := testApp(nil)
			if code := a.run([]string{"init", "--provider", "muse"}); code != exitOK {
				t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
			}
			installed, _ := os.ReadFile(path)
			if tc.drift {
				edited := strings.Replace(string(installed), "http://"+telemetry.DefaultAddr, "https://otel.corp.example", 1)
				if edited == string(installed) {
					t.Fatal("the endpoint was not where the test expected")
				}
				if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
					t.Fatal(err)
				}
				installed = []byte(edited)
			}

			u, _, _ := testApp(nil)
			var combined strings.Builder
			u.stdout, u.stderr = &combined, &combined
			if code := u.runUninstall(nil); code != exitOK {
				t.Fatalf("uninstall exit = %d:\n%s", code, combined.String())
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("uninstall deleted the developer's settings file: %v", err)
			}
			if !strings.Contains(combined.String(), tc.wantOut) {
				t.Errorf("the uninstall report lacks %q:\n%s", tc.wantOut, combined.String())
			}
			if tc.drift {
				if gjson.GetBytes(got, "telemetry.endpoint").String() != "https://otel.corp.example" {
					t.Errorf("uninstall overwrote a value the developer changed:\n%s", got)
				}
				if gjson.GetBytes(got, "hooks").Exists() {
					t.Errorf("the hooks survived uninstall:\n%s", got)
				}
			} else if string(got) != tc.original {
				t.Errorf("settings.json did not come back:\n--- want\n%s\n--- got\n%s", tc.original, got)
			}
			if _, err := os.Stat(providers.MusePriorSettingsPath(os.Getenv("HOME"))); !os.IsNotExist(err) {
				t.Error("the restore record survived uninstall")
			}
			_ = installed
		})
	}
}

// TestMuseUninstallKeepsTheRecordWhenTheRestoreCannotRun: a record that cannot
// be used is the only way back to the developer's value, so it is kept, and the
// uninstall says which restore is owed.
func TestMuseUninstallKeepsTheRecordWhenTheRestoreCannotRun(t *testing.T) {
	skipUnlessSupervised(t)
	isolateHome(t)
	seedCredentials(t, "muse")
	withMuseRunner(t, museVersion("1.4.0"))
	path := providers.MuseSettingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\n  \"schema_version\": 1,\n  \"telemetry\": {\"enabled\": false}\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "muse"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	recPath := providers.MusePriorSettingsPath(os.Getenv("HOME"))
	rec, _ := os.ReadFile(recPath)
	broken := strings.Replace(string(rec), `"raw": "{\"enabled\": false}"`, `"raw": "{\"enabled\": "`, 1)
	if broken == string(rec) {
		t.Fatalf("the record did not hold the prior value as the test expected:\n%s", rec)
	}
	if err := os.WriteFile(recPath, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	u, _, _ := testApp(nil)
	var combined strings.Builder
	u.stdout, u.stderr = &combined, &combined
	_ = u.runUninstall(nil)
	if _, err := os.Stat(recPath); err != nil {
		t.Errorf("the record was purged although the restore did not complete: %v\n%s", err, combined.String())
	}
	if !strings.Contains(combined.String(), "telemetry restore against") {
		t.Errorf("the report does not say which restore is owed:\n%s", combined.String())
	}
}

// TestARefusedMuseInstallLeavesAWorkingSharedTelemetryUnitAlone: a machine with
// Claude Code's telemetry unit installed and listening, then a Muse install that
// is refused for a reason that depends only on Muse's files. The refusal is
// decided before the unit is touched, so the unit survives byte for byte and
// nothing is restarted.
func TestARefusedMuseInstallLeavesAWorkingSharedTelemetryUnitAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, h *laneHarness, settings string)
	}{
		{"settings Muse cannot read", func(t *testing.T, _ *laneHarness, settings string) {
			if err := os.WriteFile(settings, []byte(`{"schema_version": 1,`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a telemetry value changed since OpenBox set it", func(t *testing.T, h *laneHarness, settings string) {
			a, _, _ := testApp(map[string]string{"HOME": h.home})
			if _, err := a.setupMuseTelemetry(h.home, "127.0.0.1:18789", false); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(settings)
			if err := os.WriteFile(settings, []byte(strings.Replace(string(b), "http://127.0.0.1:18789", "https://otel.corp.example", 1)), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, settings := museLaneHarness(t, museOwnSettings)
			t.Setenv("CODEX_HOME", filepath.Join(h.home, ".codex"))
			a, _, _ := testApp(map[string]string{"HOME": h.home})
			if _, err := a.setupTelemetry(h.home, "127.0.0.1:18789", false); err != nil {
				t.Fatal(err)
			}
			unitPath := laneservice.Telemetry("", "", false).UnitPath(runtime.GOOS, h.home)
			before, err := os.ReadFile(unitPath)
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, h, settings)
			// The setup above may itself have rewritten the unit; what must not
			// change is what the refused install sees.
			before, _ = os.ReadFile(unitPath)
			h.commands = nil

			if _, err := a.setupMuseTelemetry(h.home, "127.0.0.1:18789", false); err == nil {
				t.Fatal("the install was not refused")
			}
			after, err := os.ReadFile(unitPath)
			if err != nil {
				t.Fatalf("the shared unit was removed by a refused Muse install: %v", err)
			}
			if string(after) != string(before) {
				t.Errorf("the shared unit changed:\n%s\n---\n%s", before, after)
			}
			if len(h.units) == 0 {
				t.Error("the unit is gone from the supervisor")
			}
			if len(h.commands) != 0 {
				t.Errorf("a refused install still drove the supervisor: %v", h.commands)
			}
		})
	}
}

// TestALaterFailureNeverRemovesAUnitThatWasAlreadyInstalled: for a refusal that
// only shows once the unit is already rewritten (the pointer write), the unit
// that existed before stays.
func TestALaterFailureNeverRemovesAUnitThatWasAlreadyInstalled(t *testing.T) {
	h, _ := museLaneHarness(t, museOwnSettings)
	t.Setenv("CODEX_HOME", filepath.Join(h.home, ".codex"))
	a, _, _ := testApp(map[string]string{"HOME": h.home})
	if _, err := a.setupTelemetry(h.home, "127.0.0.1:18789", false); err != nil {
		t.Fatal(err)
	}
	unitPath := laneservice.Telemetry("", "", false).UnitPath(runtime.GOOS, h.home)

	// Codex's pointer is refused (a foreign [otel] block) only after the unit step.
	codexConfig := providers.CodexConfigTOMLPath()
	if err := os.MkdirAll(filepath.Dir(codexConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexConfig, []byte("[otel]\nenvironment = \"mine\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.setupCodexTelemetry(h.home, "127.0.0.1:18789", false); err == nil {
		t.Fatal("a foreign [otel] block was overwritten")
	}
	if _, err := os.Stat(unitPath); err != nil {
		t.Errorf("a refused Codex install removed the shared unit: %v", err)
	}
	if len(h.units) == 0 {
		t.Error("the unit is gone from the supervisor")
	}
}

// TestMuseUninstallKeepsTheTelemetryUnitWhileMusePointsAtIt: uninstall reverses
// install ordering, so the pointer comes out before the unit. When Muse's
// settings cannot be edited the pointer is still there, and taking the daemon
// down would leave Muse exporting to a dead port.
func TestMuseUninstallKeepsTheTelemetryUnitWhileMusePointsAtIt(t *testing.T) {
	skipUnlessSupervised(t)
	isolateHome(t)
	seedCredentials(t, "muse")
	withMuseRunner(t, museVersion("1.4.0"))
	path := providers.MuseSettingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(museOwnSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "muse"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	home := os.Getenv("HOME")
	unit := laneservice.Telemetry("", "", false).UnitPath(runtime.GOOS, home)
	if !fileExists(unit) {
		t.Fatalf("init left no telemetry unit at %s", unit)
	}
	installed, _ := os.ReadFile(path)
	future := strings.Replace(string(installed), `"schema_version": 1`, `"schema_version": 2`, 1)
	if future == string(installed) {
		t.Fatalf("schema_version was not where the test expected:\n%s", installed)
	}
	if err := os.WriteFile(path, []byte(future), 0o600); err != nil {
		t.Fatal(err)
	}

	u, _, _ := testApp(nil)
	var combined strings.Builder
	u.stdout, u.stderr = &combined, &combined
	if code := u.runUninstall(nil); code == exitOK {
		t.Fatalf("uninstall reported success with Muse still pointed at the lane:\n%s", combined.String())
	}
	if !fileExists(unit) {
		t.Errorf("the telemetry unit was removed while Muse still points at it:\n%s", combined.String())
	}
	if !strings.Contains(combined.String(), "telemetry daemon was kept") {
		t.Errorf("the report does not say the daemon was kept:\n%s", combined.String())
	}
	if got, _ := os.ReadFile(path); string(got) != future {
		t.Errorf("settings.json was edited despite the refusal:\n%s", got)
	}
}

// TestMuseInitThenUninstallOnAMachineWithoutSettingsLeavesNone: init creates
// Muse's settings.json, so uninstall takes it away again rather than leaving a
// bare schema_version behind.
func TestMuseInitThenUninstallOnAMachineWithoutSettingsLeavesNone(t *testing.T) {
	skipUnlessSupervised(t)
	isolateHome(t)
	seedCredentials(t, "muse")
	withMuseRunner(t, museVersion("1.4.0"))
	path := providers.MuseSettingsPath()
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "muse"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	if !fileExists(path) {
		t.Fatal("init did not create settings.json")
	}
	u, _, _ := testApp(nil)
	var combined strings.Builder
	u.stdout, u.stderr = &combined, &combined
	if code := u.runUninstall(nil); code != exitOK {
		t.Fatalf("uninstall exit = %d:\n%s", code, combined.String())
	}
	if fileExists(path) {
		got, _ := os.ReadFile(path)
		t.Errorf("uninstall left the settings file init created:\n%s", got)
	}
}
