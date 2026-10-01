package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/muse"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/telemetryemit"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

const contentSession = "sess-c1"

// museContentFixture lays the shared journal fixture out under today's date,
// as Muse does, and returns the sessions root.
func museContentFixture(t *testing.T, body string) (root, logPath string) {
	t.Helper()
	if body == "" {
		raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "adapters", "muse", "testdata", "session-jsonl-content.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		body = string(raw)
	}
	root = t.TempDir()
	now := time.Now()
	logPath = filepath.Join(root, now.Format("2006"), now.Format("01"), now.Format("02"), contentSession, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, logPath
}

func museSource(root, spool string) *museContentSource {
	return &museContentSource{
		SessionsRoot: root,
		SpoolDir:     spool,
		Capture:      func() bool { return true },
		Poll:         10 * time.Millisecond,
	}
}

func enrichOnce(t *testing.T, s *museContentSource, id string, within time.Duration) telemetryemit.Result {
	t.Helper()
	return s.Enricher().Enrich(context.Background(), telemetryemit.Call{
		Session: contentSession, RequestID: id, Deadline: time.Now().Add(within),
	})
}

func missReasons(r telemetryemit.Result) map[string]string {
	out := map[string]string{}
	for _, m := range r.Misses {
		out[m.Part] = m.Reason
	}
	return out
}

func TestMuseEnricherJoinsTheStashedRequestAndTheJournalReply(t *testing.T) {
	root, _ := museContentFixture(t, "")
	spool := t.TempDir()
	if err := muse.PutRequest(spool, "resp_b", muse.RequestEntry{SessionID: contentSession, Body: `{"messages":[{"role":"user"}]}`}, true); err != nil {
		t.Fatal(err)
	}
	res := enrichOnce(t, museSource(root, spool), "resp_b", 2*time.Second)
	if res.Request != `{"messages":[{"role":"user"}]}` {
		t.Errorf("request = %q", res.Request)
	}
	if !strings.Contains(res.Response, `"response_id":"resp_b"`) || !strings.Contains(res.Response, "Neutral final reply.") {
		t.Errorf("response = %q", res.Response)
	}
	if len(res.Misses) != 0 {
		t.Errorf("misses = %v", res.Misses)
	}
	if _, ok := muse.TakeRequest(spool, "resp_b"); ok {
		t.Error("the stashed request was not consumed; content must not outlive one emit")
	}
}

func TestMuseEnricherRequestAndResponseAreIndependent(t *testing.T) {
	root, _ := museContentFixture(t, "")
	res := enrichOnce(t, museSource(root, t.TempDir()), "resp_a", 150*time.Millisecond)
	if res.Request != "" || !strings.Contains(res.Response, `"name":"bash"`) {
		t.Fatalf("request=%q response=%q; the reply must ship without a stash", res.Request, res.Response)
	}
	if got := missReasons(res); got["request"] != "stash_absent" || len(got) != 1 {
		t.Errorf("misses = %v, want only request stash_absent", res.Misses)
	}
}

func TestMuseEnricherWaitsForALateJournal(t *testing.T) {
	root, logPath := museContentFixture(t, "{}\n")
	// Replace the placeholder with the real journal 300ms in.
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "adapters", "muse", "testdata", "session-jsonl-content.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.WriteFile(logPath, raw, 0o600)
	}()
	res := enrichOnce(t, museSource(root, t.TempDir()), "resp_b", 3*time.Second)
	if !strings.Contains(res.Response, "Neutral final reply.") {
		t.Fatalf("a journal that appeared within the deadline was not read: %+v", res)
	}
}

func TestMuseEnricherNeverFoundIsLogAbsentAtTheDeadline(t *testing.T) {
	root, _ := museContentFixture(t, "")
	start := time.Now()
	res := enrichOnce(t, museSource(root, t.TempDir()), "resp_unknown", 250*time.Millisecond)
	if res.Response != "" {
		t.Errorf("response = %q for an id nothing recorded", res.Response)
	}
	if got := missReasons(res); got["response"] != "log_absent" {
		t.Errorf("misses = %v", res.Misses)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("the enricher overran its deadline")
	}
}

func TestMuseEnricherDriftIsUnverifiedAndStopsEarly(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "adapters", "muse", "testdata", "session-jsonl-content-drift.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	root, _ := museContentFixture(t, string(raw))
	start := time.Now()
	res := enrichOnce(t, museSource(root, t.TempDir()), "resp_a", 5*time.Second)
	if res.Response != "" || missReasons(res)["response"] != "unverified" {
		t.Errorf("drift: response=%q misses=%v", res.Response, res.Misses)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Error("an unverified journal kept the poll running to the deadline")
	}
}

func TestMuseEnricherRedactsTheReplyBeforeItIsReturned(t *testing.T) {
	root, _ := museContentFixture(t, "")
	s := museSource(root, t.TempDir())
	s.Redact = func(in string) string { return strings.ReplaceAll(in, "Neutral final reply.", "[REDACTED]") }
	res := enrichOnce(t, s, "resp_b", time.Second)
	if strings.Contains(res.Response, "Neutral final reply.") || !strings.Contains(res.Response, "[REDACTED]") {
		t.Errorf("response = %q", res.Response)
	}
}

func TestMuseEnricherIsDisabledWithoutCaptureOrASessionsRoot(t *testing.T) {
	root, _ := museContentFixture(t, "")
	for name, s := range map[string]*museContentSource{
		"capture off": {SessionsRoot: root, SpoolDir: t.TempDir(), Capture: func() bool { return false }},
		"no root":     {SpoolDir: t.TempDir(), Capture: func() bool { return true }},
	} {
		if s.Enricher().Enabled() {
			t.Errorf("%s: enricher is enabled", name)
		}
	}
	if !museSource(root, t.TempDir()).Enricher().Enabled() {
		t.Error("capture on with a root must enable the enricher")
	}
}

func TestMuseEnricherNeverJoinsTheEchoProvider(t *testing.T) {
	root, _ := museContentFixture(t, "")
	res := enrichOnce(t, museSource(root, t.TempDir()), muse.EchoResponseID, time.Second)
	if res.Request != "" || res.Response != "" {
		t.Errorf("echo id was joined: %+v", res)
	}
}

func TestMuseEmitterWithContentShipsBodiesThroughTheRealMapper(t *testing.T) {
	root, _ := museContentFixture(t, "")
	spool := t.TempDir()
	if err := muse.PutRequest(spool, "resp_b", muse.RequestEntry{SessionID: contentSession, Body: `{"messages":[]}`}, true); err != nil {
		t.Fatal(err)
	}
	delivered := &deliveredEvents{}
	force := true
	em := newMuseTelemetryEmitter("", "127.0.0.1:0", spool, func() bool { return true }, &force,
		func() string { return "did:test" }, delivered.Deliver, nil, museSource(root, spool).Enricher())
	rec := telemetry.Record{
		Signal: telemetry.SignalLogs, EventName: "model_call", Timestamp: time.Now(),
		Attrs: map[string]string{
			"event_name": "model_call", "session_id": contentSession, "gen_ai_response_id": "resp_b",
			"gen_ai_request_model": "muse-spark", "duration_ms": "900",
		},
	}
	if err := em.Emit(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	em.Wait()
	delivered.mu.Lock()
	evs := append([]client.DevEvent(nil), delivered.evts...)
	delivered.mu.Unlock()
	if len(evs) != 2 {
		t.Fatalf("delivered %d", len(evs))
	}
	if evs[0].EventType != client.EventTurnStarted || evs[0].Span.RequestBody != `{"messages":[]}` {
		t.Errorf("Started = %+v", evs[0].Span)
	}
	if !strings.Contains(evs[1].Span.ResponseBody, "Neutral final reply.") {
		t.Errorf("Completed = %+v", evs[1].Span)
	}
}
