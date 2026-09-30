package activation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func museSettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// TestResolveMuseElection: Muse has one lane, telemetry, and it is routed only
// when Muse's own settings export to THIS receiver on loopback. Loopback is the
// discriminator: electing a producer that cannot see the call would silence
// nothing and record nothing.
func TestResolveMuseElection(t *testing.T) {
	const addr = "127.0.0.1:8789"
	for _, tc := range []struct {
		name       string
		body       string // "" means no file
		wantLane   Lane
		wantInWhy  string
		wantProblm bool
	}{
		{"routed", `{"telemetry":{"enabled":true,"destination":"external","endpoint":"http://127.0.0.1:8789"}}`, LaneTelemetry, "only lane", false},
		{"routed by localhost", `{"telemetry":{"enabled":true,"destination":"external","endpoint":"http://localhost:8789"}}`, LaneTelemetry, "", false},
		{"file absent", "", "", "does not exist", false},
		{"no telemetry block", `{"hooks":{}}`, "", "no telemetry", false},
		{"disabled", `{"telemetry":{"enabled":false,"destination":"external","endpoint":"http://127.0.0.1:8789"}}`, "", "enabled", false},
		{"enabled absent", `{"telemetry":{"destination":"external","endpoint":"http://127.0.0.1:8789"}}`, "", "enabled", false},
		{"meta destination", `{"telemetry":{"enabled":true,"destination":"meta","endpoint":"http://127.0.0.1:8789"}}`, "", "destination", false},
		{"destination absent", `{"telemetry":{"enabled":true,"endpoint":"http://127.0.0.1:8789"}}`, "", "destination", false},
		{"non-loopback endpoint", `{"telemetry":{"enabled":true,"destination":"external","endpoint":"https://otel.corp.example"}}`, "", "loopback", false},
		{"another loopback port", `{"telemetry":{"enabled":true,"destination":"external","endpoint":"http://127.0.0.1:4318"}}`, "", "not this receiver", false},
		{"endpoint absent", `{"telemetry":{"enabled":true,"destination":"external"}}`, "", "loopback", false},
		{"telemetry is not an object", `{"telemetry":true}`, "", "no telemetry", false},
		{"unparseable file", `{"telemetry":`, "", "could not be read", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := ResolveMuseElection(museSettings(t, tc.body), addr)
			if e.Elected != tc.wantLane {
				t.Errorf("Elected = %q, want %q (reason: %s)", e.Elected, tc.wantLane, e.Reason)
			}
			if (e.SettingsProblem != "") != tc.wantProblm {
				t.Errorf("SettingsProblem = %q, want a problem: %v", e.SettingsProblem, tc.wantProblm)
			}
			if !strings.Contains(e.Reason, tc.wantInWhy) {
				t.Errorf("Reason = %q, want it to contain %q", e.Reason, tc.wantInWhy)
			}
			if tc.wantLane != "" && (len(e.Routed) != 1 || e.Routed[0] != LaneTelemetry) {
				t.Errorf("Routed = %v", e.Routed)
			}
			if tc.wantLane == "" && len(e.Routed) != 0 {
				t.Errorf("Routed = %v for an election that named nobody", e.Routed)
			}
		})
	}
}

func TestResolveMuseElectionPathProblems(t *testing.T) {
	if e := ResolveMuseElection("", "127.0.0.1:8789"); e.SettingsProblem == "" || e.Elected != "" {
		t.Errorf("empty path: %+v, want a settings problem and no lane", e)
	}
	if e := ResolveMuseElection("relative/settings.json", "127.0.0.1:8789"); e.SettingsProblem == "" || !strings.Contains(e.SettingsProblem, "not absolute") {
		t.Errorf("relative path: %+v", e)
	}
	if e := ResolveMuseElection(t.TempDir(), "127.0.0.1:8789"); e.SettingsProblem == "" {
		t.Errorf("a directory where the file should be: %+v", e)
	}
}

// TestResolveMuseElectionWithoutAReceiverAddrChecksLoopbackOnly: a caller that
// does not know the receiver's address (none today beyond tests) still gets the
// loopback discriminator.
func TestResolveMuseElectionWithoutAReceiverAddrChecksLoopbackOnly(t *testing.T) {
	e := ResolveMuseElection(museSettings(t, `{"telemetry":{"enabled":true,"destination":"external","endpoint":"http://127.0.0.1:1"}}`), "")
	if e.Elected != LaneTelemetry {
		t.Errorf("Elected = %q (%s)", e.Elected, e.Reason)
	}
}
