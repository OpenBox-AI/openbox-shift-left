package activation

import (
	"context"
	"testing"
)

// withGOOS overrides currentGOOS for one test. sysmacos.go's and
// sysunsupported.go's logic are both plain Go with no OS-specific syscalls,
// so this is enough to exercise either arm from any development machine --
// no build tag, no cross-compiled test binary needed.
func withGOOS(t *testing.T, goos string) {
	t.Helper()
	old := currentGOOS
	currentGOOS = goos
	t.Cleanup(func() { currentGOOS = old })
}

// TestUnsupportedOSNeverCallsRunnerAndPrintsNoManualCommand covers the
// non-darwin arm: the Linux/Windows shapes are unmeasured (no probe has run
// for either), so this arm must ask for nothing and never print a guessed
// command.
func TestUnsupportedOSNeverCallsRunnerAndPrintsNoManualCommand(t *testing.T) {
	for _, goos := range []string{"linux", "windows", "freebsd"} {
		t.Run(goos, func(t *testing.T) {
			withGOOS(t, goos)
			rec := &recorder{t: t}

			outcome, err := ActivateSystemPAC(context.Background(), rec.run, Plan{
				HomeDir: t.TempDir(), PACURL: "http://127.0.0.1:8790/proxy.pac",
				CAPath: "/tmp/ca.pem", CAPEM: []byte("not even valid PEM, and that must not matter"),
			})
			if err != nil {
				t.Fatalf("ActivateSystemPAC: %v", err)
			}
			if outcome.Class != NotAttempted {
				t.Errorf("Class = %v, want NotAttempted", outcome.Class)
			}
			if len(outcome.Manual) != 0 {
				t.Errorf("Manual = %v, want none: the %s command shapes are unmeasured", outcome.Manual, goos)
			}
			if outcome.Entry != nil {
				t.Errorf("Entry = %+v, want nil: nothing was persisted", outcome.Entry)
			}

			report, err := DeactivateSystemPAC(context.Background(), rec.run, SystemEntry{})
			if err != nil {
				t.Fatalf("DeactivateSystemPAC: %v", err)
			}
			if report.Class != NotAttempted || len(report.Manual) != 0 {
				t.Errorf("Deactivate report = %+v, want NotAttempted with no manual commands", report)
			}

			states, err := LiveSystemPAC(context.Background(), rec.run)
			if err != nil || states != nil {
				t.Errorf("LiveSystemPAC = %v, %v; want nil, nil", states, err)
			}

			if len(rec.calls) != 0 {
				t.Errorf("the unsupported-OS arm made %d Runner call(s): %v", len(rec.calls), rec.calls)
			}
		})
	}
}
