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

	"github.com/openbox-ai/openbox-shift-left/client/memhttptest"
)

// TestTelemetryCommandActuallyRecords is the control test, and it is the reason
// this file exists rather than more unit tests.
//
// Package telemetry tested its receiver against a stub Emitter; package
// telemetryemit tested its mapper against hand-built records. Both suites were
// green while NOTHING joined them — which is precisely the shape that let the
// gateway relay perfectly and discard every capture it made. A fake at each end
// of a seam proves nothing about the seam.
//
// So this drives the REAL command: the real receiver binds a real port, a real
// OTLP/JSON export is POSTed to it, and the assertion reads the resulting
// governance event back off disk. No fake at either end. It fails if any link in
// that chain is missing.
func TestTelemetryCommandActuallyRecords(t *testing.T) {
	memhttptest.RequireBind(t)

	spoolDir := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spoolDir)
	t.Setenv("OPENBOX_AGENT_DID", "did:aip:7f3c9b2e-0000-5000-a000-00000000feed")
	// The detached flusher must never be spawned from a test binary.
	t.Setenv("OPENBOX_REALTIME", "0")

	addr := freeLoopbackAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan string, 1)
	a, _, errb := testApp(nil)
	a.telemetryCtx = ctx
	a.telemetryReady = func(bound string) { ready <- bound }

	done := make(chan int, 1)
	// --elected, because the zero value suppresses every emission on purpose.
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

	// Stop the daemon so its spool writes are complete before reading.
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("telemetry exited %d; stderr: %s", code, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("telemetry did not stop; stderr: %s", errb.String())
	}

	ev := readSpooledEvent(t, spoolDir, session)

	// These are DevEvent field names, deliberately. The spool holds the struct;
	// its translation into activity_type / activity_output / the :otel:
	// activity_id happens in buildPayload on the way out, and the sentinel test
	// asserts that on real POSTed bytes. Asserting wire names here is what made
	// this test claim an empty spool while the lane worked.
	if got := ev["event_type"]; got != "TurnCompleted" {
		t.Errorf("event_type = %v, want TurnCompleted", got)
	}
	if got := ev["otel_request_id"]; got != requestID {
		t.Errorf("otel_request_id = %v, want %q — without it turnActivityIDFor returns an EMPTY activity_id", got, requestID)
	}
	if got := ev["model"]; got != "claude-opus-4-8" {
		t.Errorf("model = %v — core's aggregation key", got)
	}
	tokens, _ := ev["tokens"].(map[string]any)
	if tokens == nil {
		t.Fatalf("no tokens on the spooled event, which is this lane's whole payload: %v", ev)
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
	span, _ := ev["span"].(map[string]any)
	if span == nil {
		t.Error("no span — core cannot classify the turn as llm_completion without one")
	} else if got := span["semantic_type"]; got != "llm_completion" {
		t.Errorf("span.semantic_type = %v", got)
	}
	// stderr must SAY it recorded. launchd sends stdio to /dev/null unless told
	// otherwise, so this line is the only signal a reachable-but-silent lane has.
	if !strings.Contains(errb.String(), "turns recorded") {
		t.Errorf("the daemon never reported what it recorded; stderr: %s", errb.String())
	}
}

// TestTelemetryCommandRecordsNothingWhenNotElected is the other half, and it is
// not a formality.
//
// The election is a correctness invariant: two lanes emitting the same turn
// doubles every token count downstream, with no id collision and no error. The
// default must therefore be silence, and a test that only ever runs with
// --elected would not notice if the flag stopped being consulted.
func TestTelemetryCommandRecordsNothingWhenNotElected(t *testing.T) {
	memhttptest.RequireBind(t)

	spoolDir := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spoolDir)
	t.Setenv("OPENBOX_AGENT_DID", "did:aip:7f3c9b2e-0000-5000-a000-00000000feed")
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
	// The export must still be ACCEPTED. Rejecting it would make the tool retry
	// and eventually surface an error, degrading the session this lane observes.
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
		t.Errorf("an UNELECTED lane wrote %d spool file(s): %v — this doubles every token count wherever another lane is also emitting", len(files), files)
	}
	if !strings.Contains(errb.String(), "NOT elected") {
		t.Errorf("startup did not announce the unelected state; a silent non-recording lane is indistinguishable from a broken one. stderr: %s", errb.String())
	}
}

// freeLoopbackAddr reserves a port and releases it.
//
// The receiver reports its CONFIGURED address, not the bound one, so ":0" would
// leave the test unable to find the port. The window between close and re-bind is
// a small race accepted here rather than adding a bound-address accessor to the
// receiver for a test's benefit.
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

// otlpAPIRequest builds a real OTLP/JSON logs export carrying one api_request.
//
// The shape and the attribute names come from a measured desktop corpus
// (claude-code-desktop 1.37937.3) — see
// plans/.../reports/measure-260828-otel-attribute-inventory.md. Values arrive as
// the provider types them: the token counts as intValue, the model as
// stringValue.
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

// spoolFiles lists the spool's files, for the negative assertion.
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

// readSpooledEvent returns the first spooled event for a session, decoded.
func readSpooledEvent(t *testing.T, spoolDir, session string) map[string]any {
	t.Helper()
	files := spoolFiles(t, spoolDir)
	if len(files) == 0 {
		t.Fatalf("the spool is EMPTY — the receiver accepted the export and recorded nothing, which is the seam this test exists to prove")
	}
	for _, rel := range files {
		raw, err := os.ReadFile(filepath.Join(spoolDir, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue
			}
			// openbox_session_id, NOT session_id. The spool stores the
			// pre-wire DevEvent; `session_id`/`run_id` are names buildPayload
			// mints on the way out. Reading the struct with the wire's
			// vocabulary is what made this test report an empty spool while the
			// lane was working correctly.
			if s, _ := ev["openbox_session_id"].(string); s == session {
				return ev
			}
		}
	}
	t.Fatalf("no spooled event for session %q; files: %v", session, files)
	return nil
}
