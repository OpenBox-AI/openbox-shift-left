package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

func museFixtureJSON(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "telemetry", "testdata", "muse", "logs.json"))
	if err != nil {
		t.Fatalf("read the scrubbed Muse fixture: %v", err)
	}
	return raw
}

// TestMuseFixtureThroughTheChainYieldsOnePairPerModelCall replays the scrubbed
// Muse 1.4.1 export (six records: session_start, tool_call, turn_end and three
// model_calls, one of them a subagent's) through decode, routing and mapping.
func TestMuseFixtureThroughTheChainYieldsOnePairPerModelCall(t *testing.T) {
	delivered := &deliveredEvents{}
	emitters := routedEmitters(delivered, "claude-code", "codex", "muse")
	// The hook path recorded the subagent's parent.
	emitters["muse"].Mapper.WithParentOf(func(child string) string {
		if child == "feed0008-0000-4000-8000-000000000008" {
			return "feed0001-0000-4000-8000-000000000001"
		}
		return ""
	})
	rec, err := telemetry.New(telemetry.Config{Addr: "127.0.0.1:0"}, telemetry.WithEmitter(newTelemetryRouter(emitters)))
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.ConsumeLogsJSON(context.Background(), museFixtureJSON(t)); err != nil {
		t.Fatal(err)
	}

	events := delivered.asMaps(t)
	if len(events) != 6 {
		t.Fatalf("delivered %d events from 3 model calls, want 3 pairs", len(events))
	}
	emitted, drops := emitters["muse"].Stats()
	if emitted != 6 || drops["unhandled-event"] != 3 {
		t.Errorf("muse emitter: emitted=%d drops=%v, want 6 emitted and the 3 other events skipped as unhandled", emitted, drops)
	}
	for _, ev := range events {
		if ev["tool"].(map[string]any)["name"] != "muse" {
			t.Errorf("event routed to %v", ev["tool"])
		}
	}
	sessions := map[string]int{}
	for _, ev := range events {
		sessions[ev["openbox_session_id"].(string)]++
	}
	if len(sessions) != 1 {
		t.Errorf("events span sessions %v; the subagent's call must fold into the session that spawned it", sessions)
	}
	if _, drops := emitters["claude-code"].Stats(); len(drops) != 0 {
		t.Errorf("the Claude Code emitter saw Muse records: %v", drops)
	}
}

// TestMuseFixtureSubagentWithNoRecordedLinkStaysItsOwnSession: the export names
// a root for the subagent, but the hook path recorded no parent, so its hook
// rows are in a session of its own and its model calls must be too.
func TestMuseFixtureSubagentWithNoRecordedLinkStaysItsOwnSession(t *testing.T) {
	delivered := &deliveredEvents{}
	emitters := routedEmitters(delivered, "muse")
	rec, err := telemetry.New(telemetry.Config{Addr: "127.0.0.1:0"}, telemetry.WithEmitter(newTelemetryRouter(emitters)))
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.ConsumeLogsJSON(context.Background(), museFixtureJSON(t)); err != nil {
		t.Fatal(err)
	}
	sessions := map[string]int{}
	for _, ev := range delivered.asMaps(t) {
		sessions[ev["openbox_session_id"].(string)]++
	}
	if len(sessions) != 2 {
		t.Errorf("events span sessions %v; a child nothing links stays a session of its own", sessions)
	}
}

func pointedMuseSettings(t *testing.T, addr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	body := fmt.Sprintf(`{"schema_version":1,"telemetry":{"enabled":true,"destination":"external","endpoint":"http://%s"}}`, addr)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func museExportBody(t *testing.T) []byte {
	t.Helper()
	ld, err := (&plog.JSONUnmarshaler{}).UnmarshalLogs(museFixtureJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	proto, err := (&plog.ProtoMarshaler{}).MarshalLogs(ld)
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(proto)
	_ = zw.Close()
	return gz.Bytes()
}

func postMuseExport(t *testing.T, addr string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+telemetry.MuseLogsPath, bytes.NewReader(museExportBody(t)))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("exporting to the receiver: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("receiver rejected the export: status %d", resp.StatusCode)
	}
}

// runMuseDaemon starts the real telemetry command against a fake core holding
// Muse's own identity, with the settings path given only through the unit's
// flag, and returns once it is listening.
func runMuseDaemon(t *testing.T, settings string, addr string, extra ...string) (stop func() (stderr string), fake *fakecore.Server) {
	t.Helper()
	memhttptest.RequireBind(t)
	t.Setenv(devconfig.EnvHome, t.TempDir())
	t.Setenv(devconfig.EnvSpoolRoot, t.TempDir())
	t.Setenv(devconfig.EnvConfigPath, "")
	t.Setenv(devconfig.EnvDID, "")
	t.Setenv("OPENBOX_REALTIME", "0")
	fake = fakecore.New(t, fakecore.Script{})
	seedV3ToolIdentity(t, "muse", fake.URL())

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	a, _, errb := testApp(nil)
	a.telemetryCtx = ctx
	a.telemetryReady = func(bound string) { ready <- bound }
	done := make(chan int, 1)
	args := append([]string{"--addr", addr, "--muse-settings", settings, "--verbose"}, extra...)
	go func() { done <- a.runTelemetry(args) }()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatalf("the receiver never reported ready; stderr: %s", errb.String())
	}
	return func() string {
		cancel()
		select {
		case code := <-done:
			if code != exitOK {
				t.Fatalf("telemetry exited %d; stderr: %s", code, errb.String())
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("telemetry did not stop; stderr: %s", errb.String())
		}
		return errb.String()
	}, fake
}

// TestTelemetryCommandRecordsMuseModelCalls is the socket control for the Muse
// lane: a real daemon, a gzip protobuf export on Muse's own path, the election
// derived from the settings file the unit's --muse-settings names (no --elected),
// and the pairs arriving at core under Muse's own identity.
func TestTelemetryCommandRecordsMuseModelCalls(t *testing.T) {
	addr := freeLoopbackAddr(t)
	stop, fake := runMuseDaemon(t, pointedMuseSettings(t, addr), addr)
	postMuseExport(t, addr)
	waitForInbox(fake, 6)
	stderr := stop()

	if refused := fake.Rejections(); len(refused) > 0 {
		t.Fatalf("core refused a delivered event: %s", strings.Join(refused, " | "))
	}
	inbox := fake.Inbox()
	if len(inbox) != 6 {
		t.Fatalf("core received %d events, want 3 pairs; stderr: %s", len(inbox), stderr)
	}
	ids := map[string][]string{}
	for _, r := range inbox {
		meta, _ := r.Body["metadata"].(map[string]any)
		if meta["tool_name"] != "muse" {
			t.Errorf("an event reached core tagged %v, want muse", meta["tool_name"])
		}
		if got := r.Body["activity_type"]; got != "llm_completion" {
			t.Errorf("activity_type = %v", got)
		}
		id, _ := r.Body["activity_id"].(string)
		if !strings.Contains(id, ":otel:") {
			t.Errorf("activity_id %q is not in the :otel: namespace", id)
		}
		ids[id] = append(ids[id], r.EventType())
	}
	if len(ids) != 3 {
		t.Errorf("%d distinct activities, want 3: %v", len(ids), ids)
	}
	for id, types := range ids {
		if len(types) != 2 {
			t.Errorf("activity %s has %v, want exactly a Started and a Completed row", id, types)
		}
	}
	if !strings.Contains(stderr, "elected producer of Muse model-call turns") {
		t.Errorf("startup did not announce the Muse election; stderr: %s", stderr)
	}
}

// TestTelemetryCommandRecordsNothingForMuseWhenTheExportPointsElsewhere: the
// election is the gate. The same export, with Muse's settings not pointed at
// this receiver, records nothing -- and says so at startup.
func TestTelemetryCommandRecordsNothingForMuseWhenTheExportPointsElsewhere(t *testing.T) {
	addr := freeLoopbackAddr(t)
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte(`{"schema_version":1,"telemetry":{"enabled":true,"destination":"meta"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stop, fake := runMuseDaemon(t, settings, addr)
	postMuseExport(t, addr)
	time.Sleep(300 * time.Millisecond)
	stderr := stop()
	if n := len(fake.Inbox()); n != 0 {
		t.Errorf("core received %d events though Muse's export does not point at this receiver", n)
	}
	if !strings.Contains(stderr, "NOT the elected producer of Muse model-call turns") {
		t.Errorf("startup did not announce the unelected state; stderr: %s", stderr)
	}
}

// TestMuseEmitterStaysIdleUntilSettingsPointAtIt: the election is re-read per
// record, so a settings file written AFTER the daemon started (install starts
// the daemon first) takes effect with no restart.
func TestMuseEmitterStaysIdleUntilSettingsPointAtIt(t *testing.T) {
	delivered := &deliveredEvents{}
	settings := filepath.Join(t.TempDir(), "settings.json")
	em := newMuseTelemetryEmitter(settings, "127.0.0.1:8789", t.TempDir(), func() bool { return true }, nil,
		func() string { return routeDID }, delivered.Deliver, func(string, ...any) {})
	rec := museRecord

	_ = em.Emit(context.Background(), rec)
	if n := len(delivered.asMaps(t)); n != 0 {
		t.Fatalf("emitted %d events before Muse's settings exist", n)
	}
	if err := os.WriteFile(settings, []byte(`{"telemetry":{"enabled":true,"destination":"external","endpoint":"http://127.0.0.1:8789"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = em.Emit(context.Background(), rec)
	if n := len(delivered.asMaps(t)); n != 2 {
		t.Fatalf("emitted %d events once the settings point here, want the pair", n)
	}
	// And the posture gate: recording=false suppresses even an elected lane.
	quiet := newMuseTelemetryEmitter(settings, "127.0.0.1:8789", t.TempDir(), func() bool { return false }, nil,
		func() string { return routeDID }, delivered.Deliver, func(string, ...any) {})
	_ = quiet.Emit(context.Background(), rec)
	if n := len(delivered.asMaps(t)); n != 2 {
		t.Errorf("a telemetry=false posture still emitted (%d events total)", n)
	}
}
