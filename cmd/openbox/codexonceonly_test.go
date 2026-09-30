package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

const (
	onceOnlyCommittedAt = "2026-09-30T10:00:00Z"
	onceOnlyOtel        = "[otel]\n[otel.exporter.otlp-http]\nendpoint = \"http://127.0.0.1:4318/v1/logs\"\n"
	onceOnlyDID         = "did:aip:00000000-0000-5000-a000-0000000000c2"
)

// codexElectionFixture lays out the three files both daemons are handed. It
// is the only state the election reads, so driving it drives every state.
type codexElectionFixture struct {
	paths codexElectionPaths
}

func newCodexElectionFixture(t *testing.T) codexElectionFixture {
	t.Helper()
	dir := t.TempDir()
	return codexElectionFixture{paths: codexElectionPaths{
		config:    filepath.Join(dir, "config.toml"),
		pacRecord: filepath.Join(dir, "activation.json"),
		marker:    filepath.Join(dir, "codex-spool", "relay-observed", "codex"),
	}}
}

func (f codexElectionFixture) writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f codexElectionFixture) config(t *testing.T, otel bool) {
	t.Helper()
	body := "model = \"gpt-5\"\n"
	if otel {
		body += onceOnlyOtel
	}
	f.writeFile(t, f.paths.config, body)
}

func onceOnlyRecord(pending bool) string {
	if pending {
		return `{"system":{"pending":true,"pac_activated":false,"providers":["claude-code","codex"]}}`
	}
	return `{"system":{"pac_activated":true,"activated_at":"` + onceOnlyCommittedAt + `","providers":["claude-code","codex"]}}`
}

// codexProducers is one Codex call fed to BOTH daemons' producers, with how
// many records each made of it.
type codexProducers struct {
	relayRecords, telemetryRecords int
}

// feedCodexCallToBothDaemons sends the same Codex model call down each lane:
// the relay sees the wire request (carrier header = thread id), telemetry
// receives Codex's own api_request export for the same thread. Both emitters
// are built by the daemons' own constructors over the same election paths.
func feedCodexCallToBothDaemons(t *testing.T, paths codexElectionPaths) codexProducers {
	t.Helper()
	t.Setenv(obgit.EnvSessionDir, t.TempDir())
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	var mu sync.Mutex
	var relayN, telemetryN int
	counter := func(n *int) func(context.Context, client.DevEvent) bool {
		return func(context.Context, client.DevEvent) bool {
			mu.Lock()
			defer mu.Unlock()
			*n++
			return true
		}
	}

	relay := &gatewayemit.Emitter{
		Lane:              gatewayemit.LaneProxy,
		Deliver:           counter(&relayN),
		Warn:              func(string, ...any) {},
		CandidatesForHost: transport.CandidatesForHost,
		Providers: relayProviderLanes(map[string]providerIdentity{"codex": {DID: onceOnlyDID}},
			filepath.Join(t.TempDir(), "settings.json"), paths, nil),
	}
	relay.Emit(context.Background(), gateway.Captured{
		HTTPMethod:     "POST",
		HTTPURL:        "https://api.openai.com/v1/responses",
		HTTPStatus:     200,
		RequestHeaders: map[string]string{"X-Client-Request-Id": "thread-once"},
		ResponseHeaders: map[string]string{
			"Request-Id": "req_once",
		},
		RequestBody:  `{"model":"gpt-5"}`,
		ResponseBody: `{"id":"resp_1"}`,
		StartedAt:    time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		EndedAt:      time.Date(2026, 9, 30, 12, 0, 2, 0, time.UTC),
	})

	tel := newCodexTelemetryEmitter(paths, func() bool { return true }, nil,
		func() string { return onceOnlyDID }, counter(&telemetryN), func(string, ...any) {})
	if err := tel.Emit(context.Background(), telemetry.Record{
		Signal:    telemetry.SignalLogs,
		EventName: "api_request",
		Timestamp: time.Date(2026, 9, 30, 12, 0, 1, 0, time.UTC),
		Attrs: map[string]string{
			"event.name":        "api_request",
			"conversation.id":   "thread-once",
			"model":             "gpt-5",
			"input_tokens":      "10",
			"output_tokens":     "5",
			"duration_ms":       "2000",
			"request_id":        "req_once",
			"client_request_id": "thread-once",
		},
	}); err != nil {
		t.Fatalf("telemetry Emit: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return codexProducers{relayRecords: relayN, telemetryRecords: telemetryN}
}

// TestACodexCallIsRecordedByExactlyOneDaemonInEveryElectionState is the
// once-only invariant: whatever the three election files say, the same call
// fed to both daemons' emitters is recorded by one of them when any lane is
// routed (never zero), and never by both.
func TestACodexCallIsRecordedByExactlyOneDaemonInEveryElectionState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		otel        bool
		record      string // "" = no record
		marker      string // "" = none
		wantRelay   bool
		wantTelemtr bool
	}{
		{name: "nothing routed"},
		{name: "otel only", otel: true, wantTelemtr: true},
		{name: "otel, PAC committed, relay has not seen Codex", otel: true, record: onceOnlyRecord(false), wantTelemtr: true},
		{name: "otel, pending PAC, stale evidence", otel: true, record: onceOnlyRecord(true), marker: onceOnlyCommittedAt, wantTelemtr: true},
		{name: "otel, PAC committed, marker from another activation", otel: true, record: onceOnlyRecord(false), marker: "2026-01-01T00:00:00Z", wantTelemtr: true},
		{name: "otel, PAC committed, relay has seen Codex", otel: true, record: onceOnlyRecord(false), marker: onceOnlyCommittedAt, wantRelay: true},
		{name: "PAC committed and observed, no otel", record: onceOnlyRecord(false), marker: onceOnlyCommittedAt, wantRelay: true},
		{name: "PAC committed, not observed, no otel"},
		{name: "unreadable record", otel: true, record: "{not json", marker: onceOnlyCommittedAt, wantTelemtr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCodexElectionFixture(t)
			f.config(t, tc.otel)
			if tc.record != "" {
				f.writeFile(t, f.paths.pacRecord, tc.record)
			}
			if tc.marker != "" {
				f.writeFile(t, f.paths.marker, tc.marker+"\n")
			}
			got := feedCodexCallToBothDaemons(t, f.paths)
			if (got.relayRecords > 0) != tc.wantRelay || (got.telemetryRecords > 0) != tc.wantTelemtr {
				t.Errorf("relay recorded %d event(s), telemetry %d; want relay=%v telemetry=%v",
					got.relayRecords, got.telemetryRecords, tc.wantRelay, tc.wantTelemtr)
			}
			if got.relayRecords > 0 && got.telemetryRecords > 0 {
				t.Errorf("one call was recorded by both daemons")
			}
		})
	}
}

// TestACodexConfigPathlessRelayNeverRecordsCodex: a relay whose unit carries
// no Codex config path (no Codex among its providers) has no proxy arm, so
// it records nothing for Codex however the record reads.
func TestACodexConfigPathlessRelayNeverRecordsCodex(t *testing.T) {
	f := newCodexElectionFixture(t)
	f.config(t, true)
	f.writeFile(t, f.paths.pacRecord, onceOnlyRecord(false))
	f.writeFile(t, f.paths.marker, onceOnlyCommittedAt+"\n")
	paths := f.paths
	paths.config = ""
	if got := feedCodexCallToBothDaemons(t, paths); got.relayRecords != 0 {
		t.Errorf("a relay with no Codex config path recorded %d event(s)", got.relayRecords)
	}
}

// TestTheGateRecordsEvidenceOnlyForACodexModelCompletion: the relay's marker is
// the proof Codex really routes through it, so it must not be written by a
// browser's chatgpt.com traffic, a Claude Code call, a non-completion POST, a
// request with only the generic request-id carrier (any OpenAI SDK script sends
// one), anything without Codex's originator header, a refused call, or while
// the record is pending.
func TestTheGateRecordsEvidenceOnlyForACodexModelCompletion(t *testing.T) {
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(t.TempDir(), "enforcements.jsonl"))
	t.Setenv(obgit.EnvSessionDir, t.TempDir())
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	codexCall := func(url string, headers map[string]string) gateway.Captured {
		return gateway.Captured{HTTPMethod: "POST", HTTPURL: url, RequestHeaders: headers}
	}
	codexHeaders := map[string]string{"X-Client-Request-Id": "t1", "Originator": "codex_cli_rs"}
	for _, tc := range []struct {
		name   string
		record string
		call   gateway.Captured
		halt   bool
		want   bool
	}{
		{"codex completion under a committed record", onceOnlyRecord(false),
			codexCall("https://api.openai.com/v1/responses", codexHeaders), false, true},
		{"codex completion on the chatgpt host", onceOnlyRecord(false),
			codexCall("https://chatgpt.com/backend-api/codex/responses", codexHeaders), false, true},
		{"request-id carrier alone, as any SDK script sends it", onceOnlyRecord(false),
			codexCall("https://api.openai.com/v1/responses", map[string]string{"X-Client-Request-Id": "t1"}), false, false},
		{"originator of another client", onceOnlyRecord(false),
			codexCall("https://api.openai.com/v1/responses", map[string]string{"X-Client-Request-Id": "t1", "Originator": "some_sdk"}), false, false},
		{"codex completion while the record is pending", onceOnlyRecord(true),
			codexCall("https://api.openai.com/v1/responses", codexHeaders), false, false},
		{"codex completion refused because its run is latched", onceOnlyRecord(false),
			codexCall("https://api.openai.com/v1/responses", codexHeaders), true, false},
		{"browser chatgpt.com traffic with no carrier", onceOnlyRecord(false),
			codexCall("https://chatgpt.com/backend-api/conversation", nil), false, false},
		{"carrier on a path that is not a completion", onceOnlyRecord(false),
			codexCall("https://api.openai.com/v1/models", codexHeaders), false, false},
		{"claude-code call", onceOnlyRecord(false),
			codexCall("https://api.anthropic.com/v1/messages", map[string]string{"X-Claude-Code-Session-Id": "s1"}), false, false},
		{"completion on a host no codex row covers", onceOnlyRecord(false),
			codexCall("https://example.com/v1/responses", codexHeaders), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCodexElectionFixture(t)
			f.writeFile(t, f.paths.pacRecord, tc.record)
			if tc.halt {
				hookflow.WriteSessionHalt(discardMainLogger(), "t1", client.Evaluation{Verdict: client.VerdictHalt, Reason: "test"})
			}
			h := haltDecorator{onCodexCall: codexObserver(f.paths, nil)}
			if _, err := h.Evaluate(context.Background(), tc.call); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(f.paths.marker)
			if got := err == nil; got != tc.want {
				t.Errorf("marker written = %v, want %v", got, tc.want)
			}
		})
	}
}
