package activation

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCCArmIsUnchangedByTheCodexArm is a fixed before/after table:
// ResolveElection (the Claude Code arm) is not edited by codexelection.go at all, so its answer for every representative settings
// input must be identical to what it was before this file existed. Recorded
// here as a fixed table rather than "trust the diff", so a future change that
// touches producer.go/settingsread.go and shifts one of these answers fails
// loudly.
func TestCCArmIsUnchangedByTheCodexArm(t *testing.T) {
	for _, tc := range []struct {
		name        string
		settingsEnv string // raw settings.json body
		wantElected Lane
		wantRouted  int
	}{
		{
			name:        "nothing routed",
			settingsEnv: `{"hooks":{}}`,
			wantElected: "",
			wantRouted:  0,
		},
		{
			name:        "transport routed at loopback",
			settingsEnv: `{"env":{"HTTPS_PROXY":"http://127.0.0.1:8790"}}`,
			wantElected: LaneTransport,
			wantRouted:  1,
		},
		{
			name:        "gateway routed at loopback",
			settingsEnv: `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8788"}}`,
			wantElected: LaneGateway,
			wantRouted:  1,
		},
		{
			name: "telemetry routed at loopback",
			settingsEnv: `{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"1",` +
				`"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":"http://127.0.0.1:4318/v1/logs"}}`,
			wantElected: LaneTelemetry,
			wantRouted:  1,
		},
		{
			name: "telemetry env true but endpoint not loopback: not routed",
			settingsEnv: `{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"1",` +
				`"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":"https://corp-otel.example/v1/logs"}}`,
			wantElected: "",
			wantRouted:  0,
		},
		{
			name: "transport and gateway both routed: gateway (base URL) outranks transport",
			settingsEnv: `{"env":{"HTTPS_PROXY":"http://127.0.0.1:8790",` +
				`"ANTHROPIC_BASE_URL":"http://127.0.0.1:8788"}}`,
			wantElected: LaneGateway,
			wantRouted:  2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(path, []byte(tc.settingsEnv), 0o644); err != nil {
				t.Fatal(err)
			}
			e := ResolveElection(path)
			if e.Elected != tc.wantElected {
				t.Errorf("Elected = %q, want %q", e.Elected, tc.wantElected)
			}
			if len(e.Routed) != tc.wantRouted {
				t.Errorf("len(Routed) = %d, want %d (%v)", len(e.Routed), tc.wantRouted, e.Routed)
			}
		})
	}
}

func TestResolveCodexElectionRoutedAtLoopback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[otel]\nenvironment = \"openbox-shift-left\"\n\n[otel.exporter.otlp-http]\n" +
		"endpoint = \"http://127.0.0.1:4318/v1/logs\"\nprotocol = \"binary\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	e := ResolveCodexElection(path)
	if e.Elected != LaneTelemetry {
		t.Errorf("Elected = %q, want %q", e.Elected, LaneTelemetry)
	}
	if !e.Usable() {
		t.Errorf("Usable() = false; SettingsProblem = %q", e.SettingsProblem)
	}
	if len(e.Routed) != 1 || e.Routed[0] != LaneTelemetry {
		t.Errorf("Routed = %v, want [telemetry]", e.Routed)
	}
}

// TestResolveCodexElectionNeverReadsClaudeCodeEnv: never reuse CLAUDE_CODE_ENABLE_TELEMETRY. Setting it in the
// process environment must have zero effect on the Codex arm, which reads
// only config.toml.
func TestResolveCodexElectionNeverReadsClaudeCodeEnv(t *testing.T) {
	t.Setenv("CLAUDE_CODE_ENABLE_TELEMETRY", "1")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("model = \"o3\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := ResolveCodexElection(path)
	if e.Elected != "" {
		t.Errorf("Elected = %q; the Codex arm must not be routed by a CC env var, only its own config.toml", e.Elected)
	}
}

func TestResolveCodexElectionNotRoutedOnANonLoopbackEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[otel]\nenvironment = \"openbox-shift-left\"\n\n[otel.exporter.otlp-http]\n" +
		"endpoint = \"https://corp-otel.example/v1/logs\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	e := ResolveCodexElection(path)
	if e.Elected != "" {
		t.Errorf("Elected = %q, want none; a non-loopback endpoint must not elect this lane", e.Elected)
	}
}

func TestResolveCodexElectionAbsentFileElectsNobodyQuietly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist", "config.toml")
	e := ResolveCodexElection(path)
	if !e.Usable() {
		t.Errorf("an absent config.toml is not a read failure; Usable() = false, SettingsProblem = %q", e.SettingsProblem)
	}
	if e.Elected != "" {
		t.Errorf("Elected = %q, want none", e.Elected)
	}
}

func TestResolveCodexElectionUnreadablePathIsReported(t *testing.T) {
	e := ResolveCodexElection("relative/config.toml")
	if e.Usable() {
		t.Fatal("a relative path must not be usable: it resolves against a daemon's working directory, not the developer's home")
	}
}

func TestResolveCodexElectionUnparsableTOMLIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("not [ valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := ResolveCodexElection(path)
	if e.Usable() {
		t.Fatal("unparsable TOML must be reported as a read failure, not read as \"no lane routed\"")
	}
}
