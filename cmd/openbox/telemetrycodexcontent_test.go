package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/telemetryemit"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

const codexRolloutThread = "0000aaaa-0000-4000-8000-000000000001"

func codexRolloutFixture(t *testing.T, transform func(string) string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "adapters", "codex", "testdata", "rollout-content.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if transform != nil {
		body = transform(body)
	}
	return body
}

func writeCodexRollout(t *testing.T, root, body string) string {
	t.Helper()
	path := filepath.Join(root, "2026", "10", "01", "rollout-2026-10-01T16-08-54-"+codexRolloutThread+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func codexSource(root string) *codexContentSource {
	return &codexContentSource{SessionsRoot: root, Capture: func() bool { return true }, Poll: 10 * time.Millisecond}
}

func intp(v int) *int { return &v }

// callTwo is the rollout fixture's second call, as the telemetry record names it.
func callTwo(within time.Duration) telemetryemit.Call {
	return telemetryemit.Call{
		Session: codexRolloutThread, RequestID: "call-x",
		At:       time.Date(2026, 10, 1, 9, 9, 7, 400e6, time.UTC),
		Tokens:   &client.Tokens{Input: intp(1100), Output: intp(60), CacheRead: intp(1000)},
		Deadline: time.Now().Add(within),
	}
}

func TestCodexEnricherBuildsRequestAndResponseFromTheRollout(t *testing.T) {
	root := t.TempDir()
	writeCodexRollout(t, root, codexRolloutFixture(t, nil))
	res := codexSource(root).Enricher().Enrich(context.Background(), callTwo(2*time.Second))
	if len(res.Misses) != 0 {
		t.Fatalf("misses = %v", res.Misses)
	}
	for _, want := range []string{"Neutral user prompt one.", "Neutral assistant text one.", "neutral tool output one"} {
		if !strings.Contains(res.Request, want) {
			t.Errorf("request lacks %q: %s", want, res.Request)
		}
	}
	if strings.Contains(res.Request, "Neutral summary two.") {
		t.Error("the call's own output is in its request")
	}
	if !strings.Contains(res.Response, `"response_id":"resp_two"`) || !strings.Contains(res.Response, "Neutral summary two.") ||
		!strings.Contains(res.Response, `"name":"shell"`) {
		t.Errorf("response = %s", res.Response)
	}
}

func TestCodexEnricherRedactsBeforeAttach(t *testing.T) {
	secret := "AKIA" + "IOSFODNN7EXAMPLE"
	root := t.TempDir()
	writeCodexRollout(t, root, codexRolloutFixture(t, func(b string) string {
		return strings.Replace(strings.Replace(b, "Neutral user prompt one.", "my key is "+secret, 1), "neutral tool output two", "out "+secret, 1)
	}))
	// Call three sees both the prompt and the second tool's output as input.
	c := callTwo(2 * time.Second)
	c.At = time.Date(2026, 10, 1, 9, 9, 11, 159e6, time.UTC)
	c.Tokens = &client.Tokens{Input: intp(1200), Output: intp(30), CacheRead: intp(1100)}
	res := codexSource(root).Enricher().Enrich(context.Background(), c)
	if res.Request == "" || strings.Contains(res.Request, secret) || strings.Contains(res.Response, secret) {
		t.Errorf("secret reached a body (or no body): %s", res.Request)
	}
}

func TestCodexEnricherIsOffWithoutCaptureOrRoot(t *testing.T) {
	root := t.TempDir()
	path := writeCodexRollout(t, root, codexRolloutFixture(t, nil))
	off := &codexContentSource{SessionsRoot: root, Capture: func() bool { return false }}
	if off.Enricher().Enabled() {
		t.Error("enabled with content_capture off")
	}
	if (&codexContentSource{Capture: func() bool { return true }}).Enricher().Enabled() {
		t.Error("enabled with no sessions root")
	}
	if (&codexContentSource{SessionsRoot: root}).Enricher().Enabled() {
		t.Error("enabled with no capture resolver")
	}
	_ = path
}

func TestCodexEnricherWaitsForALateRollout(t *testing.T) {
	root := t.TempDir()
	body := codexRolloutFixture(t, nil)
	go func() {
		time.Sleep(300 * time.Millisecond)
		writeCodexRollout(t, root, body)
	}()
	res := codexSource(root).Enricher().Enrich(context.Background(), callTwo(3*time.Second))
	if res.Request == "" || res.Response == "" || len(res.Misses) != 0 {
		t.Fatalf("a rollout that appeared within the deadline was not read: %+v", res)
	}
}

func TestCodexEnricherMissReasons(t *testing.T) {
	root := t.TempDir()
	start := time.Now()
	res := codexSource(root).Enricher().Enrich(context.Background(), callTwo(200*time.Millisecond))
	if got := missReasons(res); got["both"] != "log_absent" || res.Request+res.Response != "" {
		t.Errorf("no rollout: %v", res.Misses)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Error("gave up before the deadline on a rollout that may still be written")
	}

	writeCodexRollout(t, root, codexRolloutFixture(t, nil))
	c := callTwo(200 * time.Millisecond)
	c.Tokens = &client.Tokens{Input: intp(1101), Output: intp(60), CacheRead: intp(1000)}
	if got := missReasons(codexSource(root).Enricher().Enrich(context.Background(), c)); got["both"] != "no_join" {
		t.Errorf("token mismatch: %v", got)
	}

	// No counts to join on: nothing is read.
	c = callTwo(time.Second)
	c.Tokens = &client.Tokens{Input: intp(1100)}
	if got := missReasons(codexSource(root).Enricher().Enrich(context.Background(), c)); got["both"] != "no_join" {
		t.Errorf("no counts: %v", got)
	}

	// Drift stops at once: it does not wait out the deadline.
	drift, err := os.ReadFile(filepath.Join("..", "..", "internal", "adapters", "codex", "testdata", "rollout-content-drift.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	droot := t.TempDir()
	writeCodexRollout(t, droot, string(drift))
	start = time.Now()
	res = codexSource(droot).Enricher().Enrich(context.Background(), callTwo(5*time.Second))
	if got := missReasons(res); got["both"] != "unverified" || res.Request+res.Response != "" {
		t.Errorf("drift: %v", res.Misses)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("drift waited out the deadline")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := missReasons(codexSource(t.TempDir()).Enricher().Enrich(ctx, callTwo(5*time.Second))); got["both"] != "shutdown" {
		t.Errorf("cancelled: %v", got)
	}
}

// The daemon's own constructor, end to end: Codex's live-shaped export in,
// a Started/Completed pair out with the rollout's bodies on the right halves
// and a content-free finding for the call that could not be joined.
func TestCodexTelemetryEmitterEnrichesFromTheRollout(t *testing.T) {
	traceDir := t.TempDir()
	defer trace.SetDefault(&trace.Writer{Dir: traceDir})()
	root := t.TempDir()
	writeCodexRollout(t, root, codexRolloutFixture(t, nil))

	delivered := &deliveredEvents{}
	yes := true
	em := newCodexTelemetryEmitter(codexElectionPaths{}, func() bool { return true }, &yes,
		func() string { return routeDID }, delivered.Deliver, func(string, ...any) {}, codexSource(root).Enricher())
	em.Lifetime = context.Background()
	em.Enrich.Wait = 400 * time.Millisecond

	rec, err := telemetry.New(telemetry.Config{Addr: "127.0.0.1:0"}, telemetry.WithEmitter(newTelemetryRouter(map[string]*telemetryemit.Emitter{"codex": em})))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 9, 9, 7, 400e6, time.UTC)
	if err := rec.ConsumeLogsJSON(context.Background(), []byte(otlpCodexSSECompleted(codexRolloutThread, at, 1100, 60, 1000))); err != nil {
		t.Fatal(err)
	}
	// A second call no rollout record matches.
	if err := rec.ConsumeLogsJSON(context.Background(), []byte(otlpCodexSSECompleted(codexRolloutThread, at, 7, 7, 7))); err != nil {
		t.Fatal(err)
	}
	em.Wait()

	var withBodies, bare int
	for _, ev := range delivered.evts {
		if ev.Span == nil {
			t.Fatal("event without a span")
		}
		switch {
		case ev.EventType == client.EventTurnStarted && strings.Contains(ev.Span.RequestBody, "Neutral user prompt one."):
			withBodies++
		case ev.EventType == client.EventTurnCompleted && strings.Contains(ev.Span.ResponseBody, "resp_two"):
			withBodies++
		case ev.Span.RequestBody == "" && ev.Span.ResponseBody == "":
			bare++
		}
	}
	if len(delivered.evts) != 4 || withBodies != 2 || bare != 2 {
		t.Fatalf("%d event(s), %d with bodies, %d metadata-only; want 4/2/2", len(delivered.evts), withBodies, bare)
	}
	recs, _, err := trace.Read(traceDir, func(r trace.Record) bool { return r.Outcome == "codex.content" })
	if err != nil || len(recs) != 1 || recs[0].Detail["reason"] != "no_join" {
		t.Fatalf("findings = %+v, err %v; want one no_join", recs, err)
	}
	if b := strings.ToLower(strings.Join([]string{recs[0].Detail["reason"].(string), recs[0].Detail["part"].(string)}, " ")); strings.Contains(b, "neutral") {
		t.Error("a finding carried content")
	}
}

// Without the capture posture the rollout is never opened. The file is made
// unreadable: had a read been attempted it would have filed a log_absent
// finding, and none is filed.
func TestCodexTelemetryEmitterWithCaptureOffShipsMetadataOnly(t *testing.T) {
	traceDir := t.TempDir()
	defer trace.SetDefault(&trace.Writer{Dir: traceDir})()
	root := t.TempDir()
	path := writeCodexRollout(t, root, codexRolloutFixture(t, nil))
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o600)

	var calls atomic.Int32
	src := &codexContentSource{SessionsRoot: root, Capture: func() bool { calls.Add(1); return false }}
	delivered := &deliveredEvents{}
	yes := true
	em := newCodexTelemetryEmitter(codexElectionPaths{}, func() bool { return true }, &yes,
		func() string { return routeDID }, delivered.Deliver, func(string, ...any) {}, src.Enricher())
	rec, err := telemetry.New(telemetry.Config{Addr: "127.0.0.1:0"}, telemetry.WithEmitter(newTelemetryRouter(map[string]*telemetryemit.Emitter{"codex": em})))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 9, 9, 7, 400e6, time.UTC)
	if err := rec.ConsumeLogsJSON(context.Background(), []byte(otlpCodexSSECompleted(codexRolloutThread, at, 1100, 60, 1000))); err != nil {
		t.Fatal(err)
	}
	em.Wait()
	if len(delivered.evts) != 2 || calls.Load() == 0 {
		t.Fatalf("%d event(s), posture consulted %d time(s)", len(delivered.evts), calls.Load())
	}
	for _, ev := range delivered.evts {
		if ev.Span.RequestBody != "" || ev.Span.ResponseBody != "" {
			t.Errorf("a body rode with capture off: %+v", ev.Span)
		}
	}
	if recs, _, _ := trace.Read(traceDir, func(r trace.Record) bool { return r.Outcome == "codex.content" }); len(recs) != 0 {
		t.Errorf("capture off still read the rollout: %+v", recs)
	}
}

// The unit carries the sessions directory only for a machine whose Codex lane is
// installed, resolved from $CODEX_HOME at init (a daemon has no $HOME), and a
// later install of another tool keeps it.
func TestTheTelemetryUnitCarriesCodexSessionsOnlyForACodexInstall(t *testing.T) {
	unit := func(t *testing.T, h *laneHarness) string {
		t.Helper()
		raw, err := os.ReadFile(laneservice.Telemetry("", "", false).UnitPath(runtime.GOOS, h.home))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	addr := "127.0.0.1:18789"

	t.Run("claude code only", func(t *testing.T) {
		h, _ := museLaneHarness(t, museOwnSettings)
		t.Setenv("CODEX_HOME", filepath.Join(h.home, ".codex"))
		a, _, _ := testApp(map[string]string{"HOME": h.home})
		if _, err := a.setupTelemetry(h.home, addr, false); err != nil {
			t.Fatal(err)
		}
		if u := unit(t, h); strings.Contains(u, laneservice.CodexSessionsFlag) {
			t.Errorf("unit carries a Codex flag with no Codex install behind it:\n%s", u)
		}
	})
	t.Run("codex, then muse keeps it", func(t *testing.T) {
		h, _ := museLaneHarness(t, museOwnSettings)
		codexHome := filepath.Join(h.home, ".codex")
		t.Setenv("CODEX_HOME", codexHome)
		a, _, _ := testApp(map[string]string{"HOME": h.home})
		if _, err := a.setupCodexTelemetry(h.home, addr, false); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(codexHome, "sessions")
		if want != providers.CodexSessionsRoot() {
			t.Fatalf("SessionsRoot() = %q, want %q", providers.CodexSessionsRoot(), want)
		}
		if u := unit(t, h); !strings.Contains(u, laneservice.CodexSessionsFlag) || !strings.Contains(u, want) {
			t.Errorf("the unit does not carry %s:\n%s", want, u)
		}
		if _, err := a.setupMuseTelemetry(h.home, addr, false); err != nil {
			t.Fatal(err)
		}
		if u := unit(t, h); !strings.Contains(u, laneservice.CodexSessionsFlag) {
			t.Errorf("a Muse install stripped the Codex sessions directory:\n%s", u)
		}
	})
}

// A record whose enrichment is still waiting when the daemon is told to stop
// must still be delivered: shutdown waits for the pending enrichments before it
// closes the delivery queues, or each of them is refused by a closing queue and
// the pair is lost.
func TestTelemetryCommandDeliversInFlightEnrichmentOnShutdown(t *testing.T) {
	memhttptest.RequireBind(t)

	t.Setenv(devconfig.EnvHome, t.TempDir())
	t.Setenv(devconfig.EnvSpoolRoot, t.TempDir())
	t.Setenv(devconfig.EnvConfigPath, "")
	t.Setenv(devconfig.EnvDID, "")
	t.Setenv("OPENBOX_REALTIME", "0")

	codexFake := fakecore.New(t, fakecore.Script{})
	seedV3ToolIdentity(t, "codex", codexFake.URL())

	addr := freeLoopbackAddr(t)
	codexConfigPath := filepath.Join(t.TempDir(), "config.toml")
	if err := providers.WriteCodexOtel(codexConfigPath, "http://"+addr+"/v1/logs"); err != nil {
		t.Fatalf("seed WriteCodexOtel: %v", err)
	}
	// An empty sessions root: the enrichment polls for a rollout that never
	// comes, so it is in flight for its whole 2 s wait unless shutdown ends it.
	sessions := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	a, _, errb := testApp(nil)
	a.telemetryCtx = ctx
	a.telemetryReady = func(bound string) { ready <- bound }
	done := make(chan int, 1)
	go func() {
		done <- a.runTelemetry([]string{"--addr", addr, "--codex-settings", codexConfigPath, "--codex-sessions", sessions, "--elected"})
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("the receiver never reported ready; stderr: %s", errb.String())
	}

	resp, err := http.Post("http://"+addr+"/v1/logs", "application/json",
		strings.NewReader(otlpCodexSSECompleted("codex-conv-shutdown", time.Now(), 4, 88, 0)))
	if err != nil {
		t.Fatalf("exporting to the receiver: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	time.Sleep(150 * time.Millisecond) // the enrichment is now waiting on the rollout
	if n := len(codexFake.Inbox()); n != 0 {
		t.Fatalf("the pair was delivered before shutdown (%d event(s)); the case proves nothing", n)
	}

	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("telemetry exited %d; stderr: %s", code, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("telemetry did not stop; stderr: %s", errb.String())
	}
	if n := len(codexFake.Inbox()); n != 2 {
		t.Errorf("the in-flight enrichment's pair was lost at shutdown: %d event(s), want 2; stderr: %s", n, errb.String())
	}
}

// identicalCountsRollout is the fixture with call three re-counted to equal call
// two's counts and re-timed 20 s after it, the way two similar calls of one
// thread look. withThird=false cuts the file before call three's record is
// written, as a poll sees it a moment after the SSE record arrived.
func identicalCountsRollout(t *testing.T, withThird bool) string {
	t.Helper()
	body := codexRolloutFixture(t, func(b string) string {
		b = strings.Replace(b, `"input_tokens":1200`, `"input_tokens":1100`, -1)
		b = strings.Replace(b, `"output_tokens":30`, `"output_tokens":60`, -1)
		b = strings.Replace(b, `"cached_input_tokens":1100`, `"cached_input_tokens":1000`, -1)
		return strings.Replace(b, "2026-10-01T09:09:11.159Z", "2026-10-01T09:09:27.400Z", -1)
	})
	if withThird {
		return body
	}
	var keep []string
	for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.Contains(l, "resp_three") || strings.Contains(l, "Neutral final reply.") {
			break
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n") + "\n"
}

func callWith(requestID string, at time.Time, within time.Duration) telemetryemit.Call {
	return telemetryemit.Call{Session: codexRolloutThread, RequestID: requestID, At: at,
		Tokens:   &client.Tokens{Input: intp(1100), Output: intp(60), CacheRead: intp(1000)},
		Deadline: time.Now().Add(within)}
}

// A later call whose record is not written yet must not join an earlier call
// that has the same counts: its content would be the wrong call's.
func TestCodexEnricherNeverJoinsAnEarlierCallsRecordWithIdenticalCounts(t *testing.T) {
	root := t.TempDir()
	path := writeCodexRollout(t, root, identicalCountsRollout(t, false))
	enr := codexSource(root).Enricher()
	callA := callWith("call-a", time.Date(2026, 10, 1, 9, 9, 7, 400e6, time.UTC), time.Second)
	callB := callWith("call-b", time.Date(2026, 10, 1, 9, 9, 27, 400e6, time.UTC), 250*time.Millisecond)

	a := enr.Enrich(context.Background(), callA)
	if !strings.Contains(a.Response, "resp_two") {
		t.Fatalf("call A did not join its own record: %+v", a)
	}
	b := enr.Enrich(context.Background(), callB)
	if b.Request != "" || b.Response != "" || missReasons(b)["both"] != "no_join" {
		t.Fatalf("call B joined A's record (or shipped content): req=%q resp=%q misses=%v", b.Request, b.Response, b.Misses)
	}

	// Its own record appears later in the wait: it now joins B's.
	callB.Deadline = time.Now().Add(3 * time.Second)
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := os.WriteFile(path, []byte(identicalCountsRollout(t, true)), 0o600); err != nil {
			t.Error(err)
		}
	}()
	b = enr.Enrich(context.Background(), callB)
	if !strings.Contains(b.Response, "resp_three") || strings.Contains(b.Response, "resp_two") {
		t.Fatalf("call B did not join its own record once written: %+v", b)
	}
}

// A duplicate response.completed for one call (a retried stream) is a second id
// for the same rollout record, and must not ship the same content twice.
func TestCodexEnricherShipsOneRecordsContentOnlyOnce(t *testing.T) {
	root := t.TempDir()
	writeCodexRollout(t, root, codexRolloutFixture(t, nil))
	enr := codexSource(root).Enricher()
	at := time.Date(2026, 10, 1, 9, 9, 7, 400e6, time.UTC)
	first := enr.Enrich(context.Background(), callWith("call-1", at, time.Second))
	if first.Response == "" {
		t.Fatal("first did not join")
	}
	// The same id again (a redelivered record) is the same pair: still joined.
	if again := enr.Enrich(context.Background(), callWith("call-1", at, time.Second)); again.Response == "" {
		t.Error("a redelivery under the same id lost its content")
	}
	dup := enr.Enrich(context.Background(), callWith("call-2", at.Add(50*time.Millisecond), 250*time.Millisecond))
	if dup.Request != "" || dup.Response != "" || missReasons(dup)["both"] != "no_join" {
		t.Errorf("a second id shipped the first call's content: %+v", dup)
	}
}

// A missing cached count is zero, not an unjoinable record.
func TestCodexEnricherTreatsAMissingCachedCountAsZero(t *testing.T) {
	root := t.TempDir()
	writeCodexRollout(t, root, codexRolloutFixture(t, func(b string) string {
		// Call one's own counts carry no cached tokens.
		return strings.Replace(b, `"cached_input_tokens":400`, `"cached_input_tokens":0`, -1)
	}))
	c := telemetryemit.Call{Session: codexRolloutThread, RequestID: "call-z", At: time.Date(2026, 10, 1, 9, 9, 0, 900e6, time.UTC),
		Tokens: &client.Tokens{Input: intp(1000), Output: intp(50)}, Deadline: time.Now().Add(time.Second)}
	res := codexSource(root).Enricher().Enrich(context.Background(), c)
	if res.Response == "" || len(res.Misses) != 0 {
		t.Errorf("a missing cached_token_count was not read as zero: %+v", res)
	}
}
