package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
)

// TestTelemetryCommandActuallyRecords is the control test, and it is the
// reason this file exists rather than more unit tests. A fake at each end of a
// seam proves nothing about the seam.
//
// This daemon has no spool of its own: it signs and delivers in-process
// through a pre-resolved client.Client (resolveProviderIdentities), so the
// seam this test proves runs end to end is receiver -> mapper -> DeliverPool
// -> a real signed /evaluate call, and
// the assertion moves from a spool file to the fake core's inbox.
func TestTelemetryCommandActuallyRecords(t *testing.T) {
	memhttptest.RequireBind(t)

	fake := fakecore.New(t, fakecore.Script{})
	t.Setenv(devconfig.EnvHome, t.TempDir())
	// Every LaneQueue now appends into a real, provider-scoped spool dir
	// (devconfig.SpoolDir/providers.CodexSpoolDir); without this, an
	// isolated OPENBOX_HOME alone would still resolve the developer's real
	// spool (SpoolDir does not consult OPENBOX_HOME at all).
	t.Setenv(devconfig.EnvSpoolRoot, t.TempDir())
	t.Setenv(devconfig.EnvBaseURL, fake.URL())
	seedV3EnvIdentity(t)
	t.Setenv("OPENBOX_REALTIME", "0")

	addr := freeLoopbackAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan string, 1)
	a, _, errb := testApp(nil)
	a.telemetryCtx = ctx
	a.telemetryReady = func(bound string) { ready <- bound }

	done := make(chan int, 1)
	// Without it this test would pass by recording nothing, which is the exact
	// failure it exists to catch.
	go func() { done <- a.runTelemetry([]string{"--addr", addr, "--elected", "--verbose"}) }()

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("the receiver never reported ready; stderr: %s", errb.String())
	}

	const session = "otel-seam-session"
	const requestID = "req_011SeamControlTest"
	post := func(t *testing.T, body string) {
		t.Helper()
		resp, err := http.Post("http://"+addr+"/v1/logs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("exporting to the receiver: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("receiver rejected the export: status %d", resp.StatusCode)
		}
	}
	post(t, otlpAPIRequest(session, requestID))

	// Delivery is asynchronous now (a bounded pool, not a synchronous spool
	// append), so the fake core's inbox has to be waited for rather than read
	// once immediately after the export returns.
	// The wire's event_type is the generic ActivityStarted/ActivityCompleted
	// pair -- "for a model call llm_completion IS the activity" (CLAUDE.md) --
	// never client.DevEvent's own TurnStarted/TurnCompleted Go-level value.
	var completed, started fakecore.Received
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range fake.Inbox() {
			switch r.EventType() {
			case "ActivityCompleted":
				completed = r
			case "ActivityStarted":
				started = r
			}
		}
		if completed.Raw != nil && started.Raw != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
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

	if completed.Raw == nil {
		var types []string
		for _, r := range fake.Inbox() {
			types = append(types, r.EventType())
		}
		t.Fatalf("the fake core received no ActivityCompleted event; hits=%d inbox=%d types=%v rejections=%v; "+
			"stderr: %s", fake.Hits(), len(fake.Inbox()), types, fake.Rejections(), errb.String())
	}
	if started.Raw == nil {
		t.Fatal("no ActivityStarted half reached core; this lane used to emit only the close, so every " +
			"model-call row it produced was unpaired")
	}
	if refused := fake.Rejections(); len(refused) > 0 {
		t.Fatalf("the fake core refused a delivered event: %s", strings.Join(refused, " | "))
	}

	// The wire pairs a turn's two halves on activity_id (session:lane:request-id),
	// never a bare otel_request_id field -- that was DevEvent's own Go-side
	// name, which buildPayload does not carry onto the wire verbatim.
	ev, startedEv := completed.Body, started.Body

	activityID, _ := ev["activity_id"].(string)
	if activityID == "" || activityID != startedEv["activity_id"] {
		t.Errorf("the pair split across activity ids (%v, %v)", startedEv["activity_id"], ev["activity_id"])
	}
	if !strings.HasSuffix(activityID, ":"+requestID) {
		t.Errorf("activity_id = %q, does not carry the request id %q; turnActivityIDFor would build an empty one without it", activityID, requestID)
	}
	startedMeta, _ := startedEv["metadata"].(map[string]any)
	if _, present := startedMeta["tokens"]; present {
		t.Error("the opening half carries tokens, claiming a spend before the turn ran")
	}
	if got := ev["activity_type"]; got != "llm_completion" {
		t.Errorf("activity_type = %v, want llm_completion; core cannot classify the turn without it", got)
	}
	meta, _ := ev["metadata"].(map[string]any)
	if meta == nil {
		t.Fatalf("no metadata on the delivered event: %v", ev)
	}
	if got := meta["model"]; got != "claude-opus-4-8" {
		t.Errorf("metadata.model = %v; core's aggregation key", got)
	}
	if got := meta["tool_name"]; got != "claude-code" {
		t.Errorf("metadata.tool_name = %v, want the default claude-code", got)
	}
	tokens, _ := meta["tokens"].(map[string]any)
	if tokens == nil {
		t.Fatalf("no metadata.tokens on the delivered event, which is this lane's whole payload: %v", ev)
	}
	for k, want := range map[string]float64{
		"input": 2, "output": 173, "cache_read": 90485, "cache_creation_input": 333,
	} {
		got, ok := tokens[k].(float64)
		if !ok {
			t.Errorf("tokens.%s absent: %v", k, tokens)
			continue
		}
		if got != want {
			t.Errorf("tokens.%s = %v, want %v", k, got, want)
		}
	}
	// "event(s)", not "turns": one api_request now delivers BOTH halves of its
	// activity, and the counter follows delivery. Calling that a turn count
	// reported 2x the turns on the daemon's one operator-facing line.
	if !strings.Contains(errb.String(), "2 event(s) recorded") {
		t.Errorf("the daemon never reported what it recorded; stderr: %s", errb.String())
	}
}

// TestTelemetryCommandRecordsNothingWhenNotElected is the other half, and it
// is not a formality. The default must therefore be silence, and a test that
// only ever runs with --elected would not notice if the flag stopped being
// consulted.
func TestTelemetryCommandRecordsNothingWhenNotElected(t *testing.T) {
	memhttptest.RequireBind(t)

	spoolDir := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spoolDir)
	t.Setenv(devconfig.EnvAgentID, "7f3c9b2e-0000-5000-a000-00000000feed")
	t.Setenv("OPENBOX_REALTIME", "0")

	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan string, 1)
	a, _, errb := testApp(nil)
	a.telemetryCtx = ctx
	a.telemetryReady = func(bound string) { ready <- bound }

	done := make(chan int, 1)
	go func() { done <- a.runTelemetry([]string{"--addr", addr}) }() // no --elected

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("receiver never ready; stderr: %s", errb.String())
	}

	resp, err := http.Post("http://"+addr+"/v1/logs", "application/json",
		strings.NewReader(otlpAPIRequest("unelected-session", "req_011Unelected")))
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("an unelected receiver REJECTED the export (status %d); this lane must stay additive", resp.StatusCode)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("telemetry did not stop; stderr: %s", errb.String())
	}

	if files := spoolFiles(t, spoolDir); len(files) != 0 {
		t.Errorf("an UNELECTED lane wrote %d spool file(s): %v; this doubles every token count wherever another lane is also emitting", len(files), files)
	}
	// Every lane now says this in the same words, from one reporter, so the state
	// that must never be phrased as a routing decision -- "the settings could not
	// be read at all" -- cannot be worded differently per lane.
	if !strings.Contains(errb.String(), "NOT the elected producer") {
		t.Errorf("startup did not announce the unelected state; a silent non-recording lane is indistinguishable from a broken one. stderr: %s", errb.String())
	}
}

// TestTelemetryCommandRecordsCodexAndClaudeCodeSeparately pins the core claim
// end to end: one receiver, two providers, and a Codex record is signed and
// delivered by the Codex agent's own client, attributed to its own DID,
// never the Claude Code one -- and vice versa. Each tool gets its own fake
// core so the test can verify the SIGNING identity, not merely the DID field
// on the body.
func TestTelemetryCommandRecordsCodexAndClaudeCodeSeparately(t *testing.T) {
	memhttptest.RequireBind(t)

	home := t.TempDir()
	t.Setenv(devconfig.EnvHome, home)
	t.Setenv(devconfig.EnvSpoolRoot, t.TempDir())
	t.Setenv(devconfig.EnvConfigPath, "") // per-tool dev.json, not one pinned file
	t.Setenv(devconfig.EnvDID, "")        // no exported DID: it would outrank every per-tool store
	t.Setenv("OPENBOX_REALTIME", "0")

	ccFake := fakecore.New(t, fakecore.Script{})
	codexFake := fakecore.New(t, fakecore.Script{})

	seedIdentity := func(tool string, fake *fakecore.Server) {
		t.Helper()
		seedV3ToolIdentity(t, tool, fake.URL())
	}
	seedIdentity("claude-code", ccFake)
	seedIdentity("codex", codexFake)

	addr := freeLoopbackAddr(t)
	codexConfigPath := filepath.Join(t.TempDir(), "config.toml")
	if err := providers.WriteCodexOtel(codexConfigPath, "http://"+addr+"/v1/logs"); err != nil {
		t.Fatalf("seed WriteCodexOtel: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	a, _, errb := testApp(nil)
	a.telemetryCtx = ctx
	a.telemetryReady = func(bound string) { ready <- bound }

	done := make(chan int, 1)
	go func() {
		done <- a.runTelemetry([]string{"--addr", addr, "--codex-settings", codexConfigPath, "--elected", "--verbose"})
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("the receiver never reported ready; stderr: %s", errb.String())
	}

	post := func(t *testing.T, body string) {
		t.Helper()
		resp, err := http.Post("http://"+addr+"/v1/logs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("exporting to the receiver: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("receiver rejected the export: status %d", resp.StatusCode)
		}
	}
	post(t, otlpAPIRequest("cc-session", "req_cc_seam"))
	post(t, otlpCodexAPIRequest("codex-conv-1", "req_codex_seam"))

	waitForInbox(ccFake, 2)
	waitForInbox(codexFake, 2)

	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("telemetry exited %d; stderr: %s", code, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("telemetry did not stop; stderr: %s", errb.String())
	}

	if refused := ccFake.Rejections(); len(refused) > 0 {
		t.Fatalf("the claude-code fake refused a delivered event: %s", strings.Join(refused, " | "))
	}
	if refused := codexFake.Rejections(); len(refused) > 0 {
		t.Fatalf("the codex fake refused a delivered event: %s", strings.Join(refused, " | "))
	}

	if n := len(ccFake.Inbox()); n != 2 {
		t.Fatalf("claude-code's fake core received %d event(s), want the pair", n)
	}
	if n := len(codexFake.Inbox()); n != 2 {
		t.Fatalf("codex's fake core received %d event(s), want the pair", n)
	}
	for _, r := range ccFake.Inbox() {
		meta, _ := r.Body["metadata"].(map[string]any)
		if got := meta["tool_name"]; got != "claude-code" {
			t.Errorf("claude-code's fake core received a record tagged %v, want claude-code", got)
		}
		if strings.Contains(r.Body["activity_id"].(string), "req_codex_seam") {
			t.Error("the Codex record was delivered to claude-code's core")
		}
	}
	for _, r := range codexFake.Inbox() {
		meta, _ := r.Body["metadata"].(map[string]any)
		if got := meta["tool_name"]; got != "codex" {
			t.Errorf("codex's fake core received a record tagged %v, want codex", got)
		}
		if strings.Contains(r.Body["activity_id"].(string), "req_cc_seam") {
			t.Error("the claude-code record was delivered to codex's core")
		}
	}
}

// TestTelemetryCommandDrainsInFlightDeliveryOnShutdown is the shutdown-drain
// fix: a slow-but-healthy delivery accepted right before the stop signal must
// still reach core, not get killed by os.Exit right behind runTelemetry's
// return. The fake core holds its response long enough that a shutdown
// arriving immediately after the export would race an undrained daemon.
func TestTelemetryCommandDrainsInFlightDeliveryOnShutdown(t *testing.T) {
	memhttptest.RequireBind(t)

	fake := fakecore.New(t, fakecore.Script{Delay: 500 * time.Millisecond})
	t.Setenv(devconfig.EnvHome, t.TempDir())
	t.Setenv(devconfig.EnvSpoolRoot, t.TempDir())
	t.Setenv(devconfig.EnvBaseURL, fake.URL())
	seedV3EnvIdentity(t)
	t.Setenv("OPENBOX_REALTIME", "0")

	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan string, 1)
	a, _, errb := testApp(nil)
	a.telemetryCtx = ctx
	a.telemetryReady = func(bound string) { ready <- bound }

	done := make(chan int, 1)
	go func() {
		// A generous shutdown-grace (well under the pool's 10s per-emit
		// timeout, well over the fake's 500ms delay) is what gives the drain
		// room to actually wait rather than immediately hit its own deadline.
		done <- a.runTelemetry([]string{"--addr", addr, "--elected", "--shutdown-grace", "5s"})
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("the receiver never reported ready; stderr: %s", errb.String())
	}

	resp, err := http.Post("http://"+addr+"/v1/logs", "application/json",
		strings.NewReader(otlpAPIRequest("drain-session", "req_drain")))
	if err != nil {
		t.Fatalf("exporting to the receiver: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("receiver rejected the export: status %d", resp.StatusCode)
	}

	// Stop immediately: the export above only just got submitted into the
	// pool, and the fake core is still holding its response. Without the
	// drain fix this races an in-flight delivery against os.Exit.
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("telemetry exited %d; stderr: %s", code, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("telemetry did not stop; stderr: %s", errb.String())
	}

	if n := len(fake.Inbox()); n != 2 {
		t.Errorf("the in-flight delivery was not drained before exit: got %d event(s), want the pair; stderr: %s", n, errb.String())
	}
	if strings.Contains(errb.String(), "abandoned") {
		t.Errorf("a delivery that finished within the drain deadline must not be reported abandoned; stderr: %s", errb.String())
	}
}

// TestTelemetryCommandAbandonsAndCountsADeliveryPastTheDrainDeadline is the
// asymmetric case: a shutdown-grace shorter than a slow delivery must not
// hang the daemon forever. Unlike the transport lane's own chat pool (which
// really does lose an abandoned delivery), a LaneQueue's own abandoned drain
// leaves the record durably spooled -- this daemon's shutdown log names it
// "abandoned" for the operator, but it is deliberately NOT added to the
// dropped total here (LaneQueue.Close's own doc): it survives on disk and
// will be counted, exactly once, by whichever drainer reclaims it, so
// counting it again at this exact moment would double it.
func TestTelemetryCommandAbandonsButDoesNotDoubleCountADeliveryPastTheDrainDeadline(t *testing.T) {
	memhttptest.RequireBind(t)

	fake := fakecore.New(t, fakecore.Script{Delay: 2 * time.Second})
	t.Setenv(devconfig.EnvHome, t.TempDir())
	t.Setenv(devconfig.EnvSpoolRoot, t.TempDir())
	t.Setenv(devconfig.EnvBaseURL, fake.URL())
	seedV3EnvIdentity(t)
	t.Setenv("OPENBOX_REALTIME", "0")

	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan string, 1)
	a, _, errb := testApp(nil)
	a.telemetryCtx = ctx
	a.telemetryReady = func(bound string) { ready <- bound }

	done := make(chan int, 1)
	go func() {
		done <- a.runTelemetry([]string{"--addr", addr, "--elected", "--shutdown-grace", "100ms"})
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("the receiver never reported ready; stderr: %s", errb.String())
	}

	resp, err := http.Post("http://"+addr+"/v1/logs", "application/json",
		strings.NewReader(otlpAPIRequest("abandon-session", "req_abandon")))
	if err != nil {
		t.Fatalf("exporting to the receiver: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("telemetry exited %d; stderr: %s", code, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("telemetry did not stop; a short shutdown-grace must still bound the drain; stderr: %s", errb.String())
	}

	if !strings.Contains(errb.String(), "abandoned") {
		t.Errorf("a delivery still running past the drain deadline must be reported abandoned; stderr: %s", errb.String())
	}
	if !strings.Contains(errb.String(), "delivery pool dropped=0") {
		t.Errorf("an abandoned-but-not-lost LaneQueue drain must NOT be counted in the dropped total "+
			"(it survives on disk for the next drainer to count once, for real); stderr: %s", errb.String())
	}

	// Let the abandoned drain's own background goroutine actually finish
	// (the fake's 2s delay, not a hang) BEFORE this test function returns:
	// otherwise it keeps running past t.Setenv's own teardown and races the
	// fake core's Cleanup-driven shutdown with a real network call using an
	// environment that no longer isolates it -- exactly the kind of leak the
	// hermeticity guard exists to catch.
	waitForInbox(fake, 2)
}

// TestTelemetryCommandHonoursEachToolsOwnRecordingPosture is the other half
// of the privacy-posture fix (the content-capture half is proven in
// laneidentity_test.go): the daemon's per-tool recording gate
// (telemetryPostureFor) must read each tool's OWN dev.json `telemetry` key,
// never whichever tool the daemon bound at startup for unrelated setup work.
// Codex opts out here; claude-code is left at its default (on), and the two
// must diverge.
func TestTelemetryCommandHonoursEachToolsOwnRecordingPosture(t *testing.T) {
	memhttptest.RequireBind(t)

	home := t.TempDir()
	t.Setenv(devconfig.EnvHome, home)
	t.Setenv(devconfig.EnvSpoolRoot, t.TempDir())
	t.Setenv(devconfig.EnvConfigPath, "")
	t.Setenv(devconfig.EnvDID, "")
	t.Setenv("OPENBOX_REALTIME", "0")

	ccFake := fakecore.New(t, fakecore.Script{})
	codexFake := fakecore.New(t, fakecore.Script{})

	seedIdentity := func(tool string, fake *fakecore.Server) {
		t.Helper()
		seedV3ToolIdentity(t, tool, fake.URL())
	}
	seedIdentity("claude-code", ccFake)
	seedIdentity("codex", codexFake)

	// Update has no Telemetry field; patch codex's dev.json directly, the way
	// the devconfig package's own posture tests do for fields Update omits.
	codexCfgPath, err := devconfig.DevConfigWritePathFor("codex")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(codexCfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["telemetry"] = false
	patched, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexCfgPath, patched, 0o600); err != nil {
		t.Fatal(err)
	}

	addr := freeLoopbackAddr(t)
	codexConfigPath := filepath.Join(t.TempDir(), "config.toml")
	if err := providers.WriteCodexOtel(codexConfigPath, "http://"+addr+"/v1/logs"); err != nil {
		t.Fatalf("seed WriteCodexOtel: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	a, _, errb := testApp(nil)
	a.telemetryCtx = ctx
	a.telemetryReady = func(bound string) { ready <- bound }

	done := make(chan int, 1)
	go func() {
		done <- a.runTelemetry([]string{"--addr", addr, "--codex-settings", codexConfigPath, "--elected", "--verbose"})
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("the receiver never reported ready; stderr: %s", errb.String())
	}

	post := func(t *testing.T, body string) {
		t.Helper()
		resp, err := http.Post("http://"+addr+"/v1/logs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("exporting to the receiver: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("receiver rejected the export: status %d", resp.StatusCode)
		}
	}
	post(t, otlpAPIRequest("cc-session", "req_cc_posture"))
	post(t, otlpCodexAPIRequest("codex-conv-posture", "req_codex_posture"))

	// claude-code must still record (its own posture is untouched); wait for
	// it, then give codex's silence a moment to prove it stays silent rather
	// than merely racing a slow delivery.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(ccFake.Inbox()) < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("telemetry exited %d; stderr: %s", code, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("telemetry did not stop; stderr: %s", errb.String())
	}

	if n := len(ccFake.Inbox()); n != 2 {
		t.Errorf("claude-code's own posture is untouched and must still record; got %d event(s)", n)
	}
	if n := len(codexFake.Inbox()); n != 0 {
		t.Errorf("codex opted out of telemetry in its own dev.json and must record NOTHING; got %d event(s): %v", n, codexFake.Inbox())
	}
	if !strings.Contains(errb.String(), "codex's own posture telemetry=false") {
		t.Errorf("the daemon never announced codex's own recording posture; stderr: %s", errb.String())
	}
}

// otlpCodexAPIRequest is otlpAPIRequest's Codex shape: conversation.id in
// place of session.id, which is what the probe recorded Codex actually
// exports -- a minimal, scoped attributability, not a full session-key
// package.
func otlpCodexAPIRequest(conversationID, requestID string) string {
	attr := func(k, v string) string {
		return fmt.Sprintf(`{"key":%q,"value":{"stringValue":%q}}`, k, v)
	}
	intAttr := func(k string, v int) string {
		return fmt.Sprintf(`{"key":%q,"value":{"intValue":"%d"}}`, k, v)
	}
	attrs := strings.Join([]string{
		attr("event.name", "api_request"),
		attr("conversation.id", conversationID),
		attr("request_id", requestID),
		attr("model", "gpt-5-codex"),
		intAttr("input_tokens", 4),
		intAttr("output_tokens", 88),
		intAttr("duration_ms", 1000),
	}, ",")
	now := time.Now().UnixNano()
	return fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":[%s]},"scopeLogs":[{"logRecords":[{"timeUnixNano":"%d","attributes":[%s]}]}]}]}`,
		attr("service.name", "codex-cli"), now, attrs)
}

// seedV3EnvIdentity sets a complete v3 identity purely through the
// environment, for the single-fake tests here: fakecore's process-wide
// identity (workload key normalized to the single-line base64 DER form
// WriteEnvFile-based fixtures use elsewhere, since the raw PEM contains
// newlines this format cannot represent).
func seedV3EnvIdentity(t *testing.T) {
	t.Helper()
	t.Setenv(devconfig.EnvAgentID, fakecore.AgentID())
	t.Setenv(devconfig.EnvAPIKeyDirect, fakecore.APIKey())
	workloadKey, err := workloadauth.NormalizePrivateKey(fakecore.WorkloadPrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(devconfig.EnvWorkloadPrivateKey, workloadKey)
}

// seedV3ToolIdentity writes a complete v3 identity to tool's own store, for
// the two-fake tests proving each tool resolves its own identity. Every
// fakecore.Server shares the same process-wide v3 identity, so tool
// separation here comes from the store (per-tool .env/dev.json) and BaseURL,
// not from a distinct credential.
func seedV3ToolIdentity(t *testing.T, tool, baseURL string) {
	t.Helper()
	envPath, err := devconfig.EnvFilePathFor(tool)
	if err != nil {
		t.Fatal(err)
	}
	workloadKey, err := workloadauth.NormalizePrivateKey(fakecore.WorkloadPrivateKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteEnvFile(envPath, map[string]string{
		devconfig.EnvAPIKeyDirect:       fakecore.APIKey(),
		devconfig.EnvWorkloadPrivateKey: workloadKey,
	}); err != nil {
		t.Fatal(err)
	}
	cfgPath, err := devconfig.DevConfigWritePathFor(tool)
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteConfig(cfgPath, devconfig.Update{
		AgentID: fakecore.AgentID(), IdentityMethod: devconfig.IdentityMethodKeycloakWorkload, BaseURL: baseURL,
	}); err != nil {
		t.Fatal(err)
	}
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot reserve a loopback port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func otlpAPIRequest(session, requestID string) string {
	attr := func(k, v string) string {
		return fmt.Sprintf(`{"key":%q,"value":{"stringValue":%q}}`, k, v)
	}
	intAttr := func(k string, v int) string {
		return fmt.Sprintf(`{"key":%q,"value":{"intValue":"%d"}}`, k, v)
	}
	attrs := strings.Join([]string{
		attr("event.name", "api_request"),
		attr("session.id", session),
		attr("request_id", requestID),
		attr("model", "claude-opus-4-8"),
		intAttr("input_tokens", 2),
		intAttr("output_tokens", 173),
		intAttr("cache_read_tokens", 90485),
		intAttr("cache_creation_tokens", 333),
		intAttr("duration_ms", 4210),
	}, ",")
	now := time.Now().UnixNano()
	return fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":[%s]},"scopeLogs":[{"logRecords":[{"timeUnixNano":"%d","attributes":[%s]}]}]}]}`,
		attr("service.name", "claude-code-desktop"), now, attrs)
}

// waitForInbox polls fake.Inbox() until it holds at least want events or 10s
// elapses; a timeout leaves whatever the caller's own next assertion reports,
// rather than failing here with a less specific message.
func waitForInbox(fake *fakecore.Server, want int) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(fake.Inbox()) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func spoolFiles(t *testing.T, spoolDir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(spoolDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		rel, _ := filepath.Rel(spoolDir, path)
		out = append(out, rel)
		return nil
	})
	return out
}
