package activation

import (
	"os"
	"path/filepath"
	"strings"
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
	e := ResolveCodexElection(path, "", "")
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
	e := ResolveCodexElection(path, "", "")
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
	e := ResolveCodexElection(path, "", "")
	if e.Elected != "" {
		t.Errorf("Elected = %q, want none; a non-loopback endpoint must not elect this lane", e.Elected)
	}
}

func TestResolveCodexElectionAbsentFileElectsNobodyQuietly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist", "config.toml")
	e := ResolveCodexElection(path, "", "")
	if !e.Usable() {
		t.Errorf("an absent config.toml is not a read failure; Usable() = false, SettingsProblem = %q", e.SettingsProblem)
	}
	if e.Elected != "" {
		t.Errorf("Elected = %q, want none", e.Elected)
	}
}

func TestResolveCodexElectionUnreadablePathIsReported(t *testing.T) {
	e := ResolveCodexElection("relative/config.toml", "", "")
	if e.Usable() {
		t.Fatal("a relative path must not be usable: it resolves against a daemon's working directory, not the developer's home")
	}
}

func TestResolveCodexElectionUnparsableTOMLIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("not [ valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := ResolveCodexElection(path, "", "")
	if e.Usable() {
		t.Fatal("unparsable TOML must be reported as a read failure, not read as \"no lane routed\"")
	}
}

const (
	loopbackOtel = "[otel]\n[otel.exporter.otlp-http]\nendpoint = \"http://127.0.0.1:4318/v1/logs\"\n"
	committedAt  = "2026-09-30T10:00:00Z"
)

// codexFixture lays out the three files a daemon is handed paths to: Codex's
// config.toml, the activation record and the observed marker.
type codexFixture struct {
	config, record, marker string
}

func newCodexFixture(t *testing.T, configBody string) codexFixture {
	t.Helper()
	dir := t.TempDir()
	f := codexFixture{
		config: filepath.Join(dir, "config.toml"),
		record: filepath.Join(dir, "activation.json"),
		marker: filepath.Join(dir, "spool", "relay-observed", "codex"),
	}
	if err := os.WriteFile(f.config, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f codexFixture) writeRecord(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(f.record, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f codexFixture) writeMarker(t *testing.T, stamp string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.marker, []byte(stamp+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f codexFixture) resolve() Election {
	return ResolveCodexElection(f.config, f.record, f.marker)
}

func committedRecord(providers string) string {
	return `{"schema":"x","system":{"schema":"y","pac_activated":true,"activated_at":"` + committedAt +
		`","providers":[` + providers + `]}}`
}

// TestResolveCodexElectionTable is the whole matrix of the two routed lanes
// and the evidence the proxy arm needs, including the states where a Codex
// that ignores the PAC must leave telemetry elected.
func TestResolveCodexElectionTable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		record string // "" = no file
		marker string // "" = none; "current" = the record's commit time
		want   Lane
		routed int
	}{
		{"otel only", loopbackOtel, "", "", LaneTelemetry, 1},
		{"PAC only, observed", "model = \"o3\"\n", committedRecord(`"codex"`), "current", LaneTransport, 1},
		{"PAC only, not yet observed", "model = \"o3\"\n", committedRecord(`"codex"`), "", "", 0},
		{"otel and PAC but nothing observed: telemetry stays", loopbackOtel, committedRecord(`"codex"`), "", LaneTelemetry, 1},
		{"otel and PAC and observed: relay outranks telemetry", loopbackOtel, committedRecord(`"codex"`), "current", LaneTransport, 2},
		{"pending record", loopbackOtel,
			`{"system":{"pending":true,"pac_activated":false,"activated_at":"` + committedAt + `","providers":["codex"]}}`,
			"current", LaneTelemetry, 1},
		{"committed record without codex", loopbackOtel, committedRecord(`"claude-code"`), "current", LaneTelemetry, 1},
		{"stale marker from an earlier activation", loopbackOtel, committedRecord(`"codex"`), "2026-01-01T00:00:00Z", LaneTelemetry, 1},
		{"base_url off the codex rows",
			"model_provider = \"x\"\n[model_providers.x]\nbase_url = \"https://llm.example.com/v1\"\n" + loopbackOtel,
			committedRecord(`"codex"`), "current", LaneTelemetry, 1},
		{"base_url on a codex row",
			"model_provider = \"x\"\n[model_providers.x]\nbase_url = \"https://api.openai.com/v1\"\n" + loopbackOtel,
			committedRecord(`"codex"`), "current", LaneTransport, 2},
		{"model provider with no base_url", "model_provider = \"x\"\n" + loopbackOtel,
			committedRecord(`"codex"`), "current", LaneTelemetry, 1},
		{"unreadable record", loopbackOtel, "{not json", "current", LaneTelemetry, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCodexFixture(t, tc.config)
			if tc.record != "" {
				f.writeRecord(t, tc.record)
			}
			switch tc.marker {
			case "":
			case "current":
				f.writeMarker(t, committedAt)
			default:
				f.writeMarker(t, tc.marker)
			}
			e := f.resolve()
			if e.Elected != tc.want || len(e.Routed) != tc.routed {
				t.Errorf("Elected=%q Routed=%v; want %q with %d routed (%s)", e.Elected, e.Routed, tc.want, tc.routed, e.Reason)
			}
			if !e.Usable() {
				t.Errorf("a record problem must never make the election undecidable: %s", e.SettingsProblem)
			}
		})
	}
}

// TestResolveCodexElectionOldUnitWithoutARecordPathStaysTelemetry is the
// upgrade case: a unit written before the record path existed resolves
// exactly as it always did.
func TestResolveCodexElectionOldUnitWithoutARecordPathStaysTelemetry(t *testing.T) {
	f := newCodexFixture(t, loopbackOtel)
	if e := ResolveCodexElection(f.config, "", ""); e.Elected != LaneTelemetry {
		t.Fatalf("Elected = %q, want telemetry", e.Elected)
	}
}

// TestMarkCodexProxyObservedWritesOnlyUnderACommittedRecord pins the
// evidence rules: nothing while the record is pending, absent, or without
// Codex; the commit time once it is committed; and a re-activation (new
// commit time) makes the old marker stale without anyone deleting it.
func TestMarkCodexProxyObservedWritesOnlyUnderACommittedRecord(t *testing.T) {
	f := newCodexFixture(t, loopbackOtel)
	mark := func() bool {
		t.Helper()
		ok, err := MarkCodexProxyObserved(f.record, f.marker)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if mark() {
		t.Error("wrote a marker with no record at all")
	}
	f.writeRecord(t, `{"system":{"pending":true,"providers":["codex"]}}`)
	if mark() {
		t.Error("wrote a marker under a pending record")
	}
	f.writeRecord(t, committedRecord(`"claude-code"`))
	if mark() {
		t.Error("wrote a marker under a record that does not list codex")
	}
	f.writeRecord(t, committedRecord(`"codex"`))
	if !mark() {
		t.Fatal("did not write under a committed record that lists codex")
	}
	if mark() {
		t.Error("rewrote an identical marker")
	}
	if e := f.resolve(); e.Elected != LaneTransport {
		t.Fatalf("Elected = %q after the marker, want transport", e.Elected)
	}
	f.writeRecord(t, strings.Replace(committedRecord(`"codex"`), committedAt, "2026-09-30T11:00:00Z", 1))
	if e := f.resolve(); e.Elected != LaneTelemetry {
		t.Errorf("Elected = %q after a re-activation, want telemetry until the relay sees Codex again", e.Elected)
	}
}

func TestRemoveSystemProviderDropsOnlyThatProvider(t *testing.T) {
	home := t.TempDir()
	rec := filepath.Join(home, ".openbox", "activation.json")
	if err := os.MkdirAll(filepath.Dir(rec), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec, []byte(committedRecord(`"claude-code","codex"`)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSystemProvider(home, "codex"); err != nil {
		t.Fatal(err)
	}
	entry, err := LoadSystemEntryAt(rec)
	if err != nil || entry == nil {
		t.Fatalf("LoadSystemEntryAt = %v, %v", entry, err)
	}
	if len(entry.Providers) != 1 || entry.Providers[0] != "claude-code" || !entry.PACActivated {
		t.Errorf("entry = %+v", entry)
	}
	if err := RemoveSystemProvider(home, "codex"); err != nil {
		t.Errorf("removing an absent provider must be a no-op: %v", err)
	}
}

// TestAReactivationInTheSameSecondStillInvalidatesEvidence: the commit time
// has second resolution, so the activation's nanosecond id is what the marker
// stores when a record has one; a record without one falls back to the commit
// time.
func TestAReactivationInTheSameSecondStillInvalidatesEvidence(t *testing.T) {
	f := newCodexFixture(t, loopbackOtel)
	withID := func(id string) string {
		return `{"system":{"pac_activated":true,"activated_at":"` + committedAt + `","activation_id":"` + id + `","providers":["codex"]}}`
	}
	f.writeRecord(t, withID("2026-09-30T10:00:00.111111111Z"))
	if ok, err := MarkCodexProxyObserved(f.record, f.marker); err != nil || !ok {
		t.Fatalf("mark = %v, %v", ok, err)
	}
	if e := f.resolve(); e.Elected != LaneTransport {
		t.Fatalf("Elected = %q, want transport", e.Elected)
	}
	f.writeRecord(t, withID("2026-09-30T10:00:00.999999999Z"))
	if e := f.resolve(); e.Elected != LaneTelemetry {
		t.Errorf("Elected = %q after a same-second re-activation, want telemetry", e.Elected)
	}
}
