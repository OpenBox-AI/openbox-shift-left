package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

// serveOneCall relays one POST to https://api.anthropic.com/v1/messages
// through p over a fresh TLS-terminated pipe, mirroring
// TestTransportLaneRecordsThroughTheRealChain's pattern in
// transportcapture_test.go, and returns the response status and body.
func serveOneCall(t *testing.T, p *transport.Proxy, ca *transport.CA, sessionID string) (status int, body string) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	go p.ServeIntercepted(serverConn, "api.anthropic.com:443")

	if err := clientConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	br := bufio.NewReader(clientConn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT status: %v", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 200") {
		t.Fatalf("CONNECT status = %q, want 200", statusLine)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read CONNECT headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("CA PEM did not parse")
	}
	tc := tls.Client(clientConn, &tls.Config{ServerName: "api.anthropic.com", RootCAs: pool})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5","messages":[]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if sessionID != "" {
		req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	}
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if err := req.Write(tc); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// alwaysVerdict is a minimal gateway.Evaluator returning a fixed verdict,
// used to pin transport.WithGate's own plumbing independently of
// haltDecorator's host/session resolution logic, which is tested in
// isolation below.
type alwaysVerdict struct {
	eval  client.Evaluation
	calls int32
}

func (a *alwaysVerdict) Evaluate(context.Context, gateway.Captured) (client.Evaluation, error) {
	atomic.AddInt32(&a.calls, 1)
	return a.eval, nil
}

// TestWithGateWiredIntoTheRelayRefusesBeforeTheDial pins that a Proxy
// built with transport.WithGate actually reaches gateway.Gateway.WithGate
// (proxy.go's own newRelay), so a HALT verdict refuses the call BEFORE the
// upstream dial (the refused loopback upstream never gets a chance to matter:
// a refused call must never even attempt it), while an ALLOW verdict lets the
// call reach -- and fail against -- the same refused upstream, proving the
// gate did not swallow it.
func TestWithGateWiredIntoTheRelayRefusesBeforeTheDial(t *testing.T) {
	ca, err := transport.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	em := &gatewayemit.Emitter{
		Elected: func() bool { return true },
		Lane:    gatewayemit.LaneProxy,
		Deliver: testSpoolDeliver(t.TempDir()),
		DID:     func() string { return "did:aip:00000000-0000-5000-a000-0000000000ca" },
		Warn:    func(string, ...any) {},
	}

	ev := &alwaysVerdict{eval: client.Evaluation{Verdict: client.VerdictHalt, Reason: "test refusal"}}
	p, err := transport.New(transport.Config{Upstream: refusedUpstream}, ca, em,
		transport.WithGate(ev, gatedFn))
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	status, body := serveOneCall(t, p, ca, "sess-plumbing")
	if status != http.StatusForbidden {
		t.Fatalf("HALT-gated call got %d, want the refusal status %d; body=%s", status, http.StatusForbidden, body)
	}
	if !strings.Contains(body, "test refusal") {
		t.Errorf("refusal body does not carry the evaluator's own reason: %s", body)
	}
	if got := atomic.LoadInt32(&ev.calls); got != 1 {
		t.Errorf("evaluator called %d times, want exactly 1 (the gate itself, no retry)", got)
	}

	ev.eval = client.Evaluation{Verdict: client.VerdictAllow}
	status, _ = serveOneCall(t, p, ca, "sess-plumbing")
	if status != http.StatusBadGateway {
		t.Fatalf("ALLOW-gated call got %d, want 502 from the refused upstream (proves the call reached the dial)", status)
	}
}

// TestWithGateANilSelectorStillGates: an evaluator wired with no selector must
// not silently gate nothing while the caller believes refusal is on; the
// relay reads a nil selector as every POST.
func TestWithGateANilSelectorStillGates(t *testing.T) {
	ca, err := transport.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	em := &gatewayemit.Emitter{
		Elected: func() bool { return true },
		Lane:    gatewayemit.LaneProxy,
		Deliver: testSpoolDeliver(t.TempDir()),
		DID:     func() string { return "did:aip:00000000-0000-5000-a000-0000000000ca" },
		Warn:    func(string, ...any) {},
	}
	ev := &alwaysVerdict{eval: client.Evaluation{Verdict: client.VerdictHalt, Reason: "test refusal"}}
	p, err := transport.New(transport.Config{Upstream: refusedUpstream}, ca, em, transport.WithGate(ev, nil))
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if status, body := serveOneCall(t, p, ca, "sess-nil-selector"); status != http.StatusForbidden {
		t.Fatalf("a POST through a gate wired with a nil selector got %d, want %d; body=%s", status, http.StatusForbidden, body)
	}
}

// TestHaltDecoratorRefusesALatchedRun exercises haltDecorator.Evaluate
// directly: a HALT written by ANY lane
// (hookflow.WriteSessionHalt, standing in for the shared Deliver seam)
// latches the run, and the NEXT relayed call for that same session then
// reads VerdictHalt back with the latch's own reason -- with no delegate
// consulted at all, since a latched run answers before next is ever reached.
func TestHaltDecoratorRefusesALatchedRun(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(t.TempDir(), "enforcements.jsonl"))
	t.Setenv(obgit.EnvSessionDir, t.TempDir())
	const sessionID = "sess-cross-lane-halt-1"
	c := gateway.Captured{
		HTTPURL:        "https://api.anthropic.com/v1/messages",
		RequestHeaders: map[string]string{"X-Claude-Code-Session-Id": sessionID},
	}

	eval, err := (haltDecorator{}).Evaluate(context.Background(), c)
	if err != nil {
		t.Fatalf("Evaluate (unlatched): %v", err)
	}
	if eval.Verdict != client.VerdictAllow {
		t.Fatalf("unlatched session resolved verdict %q, want ALLOW", eval.Verdict)
	}

	// A HALT delivered by ANY lane (here standing in for an :otel: or hook
	// delivery through the shared Deliver seam) latches the run this session
	// currently belongs to -- at generation 0 the run IS the session id.
	hookflow.WriteSessionHalt(discardMainLogger(), sessionID, client.Evaluation{
		Verdict: client.VerdictHalt, Reason: "secrets policy", PolicyID: "pol-1",
	})

	eval, err = (haltDecorator{}).Evaluate(context.Background(), c)
	if err != nil {
		t.Fatalf("Evaluate (latched): %v", err)
	}
	if eval.Verdict != client.VerdictHalt {
		t.Fatalf("latched session resolved verdict %q, want HALT", eval.Verdict)
	}
	if !strings.Contains(eval.Reason, "secrets policy") {
		t.Errorf("resolved reason %q does not carry the latch's own text", eval.Reason)
	}
}

// TestHaltDecoratorDoesNotLatchAfterADeliveryFailureIsRecorded: an unaccepted
// event (hookflow.RecordDeliveryFailure, not a HALT verdict) no longer
// latches the run -- it only writes a trace/log finding -- so the run's NEXT
// relayed call is still resolved ALLOW by haltDecorator, for both the
// session the failure was recorded against and an unrelated one. Only a real
// server HALT verdict (WriteSessionHalt) latches; that half is pinned by
// TestHaltDecoratorRefusesALatchedRun above.
func TestHaltDecoratorDoesNotLatchAfterADeliveryFailureIsRecorded(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(t.TempDir(), "enforcements.jsonl"))
	t.Setenv(obgit.EnvSessionDir, t.TempDir())
	const recordedSession = "sess-delivery-failure-1"
	const unrelatedSession = "sess-delivery-failure-unrelated"

	recordedCall := gateway.Captured{
		HTTPURL:        "https://api.anthropic.com/v1/messages",
		RequestHeaders: map[string]string{"X-Claude-Code-Session-Id": recordedSession},
	}
	unrelatedCall := gateway.Captured{
		HTTPURL:        "https://api.anthropic.com/v1/messages",
		RequestHeaders: map[string]string{"X-Claude-Code-Session-Id": unrelatedSession},
	}

	if eval, err := (haltDecorator{}).Evaluate(context.Background(), recordedCall); err != nil || eval.Verdict != client.VerdictAllow {
		t.Fatalf("Evaluate (before any failure): verdict=%q err=%v, want ALLOW", eval.Verdict, err)
	}

	// Stand in for a hook drainer or a lane daemon's own OnFailure, the same
	// writer every one of them shares (hookflow.NewEngine wires it as the
	// default Spool.OnFailure): core did not accept this event, but that no
	// longer latches the run -- only a trace/log finding is written.
	hookflow.RecordDeliveryFailure(discardMainLogger(), client.DevEvent{
		EventID: "e1", EventType: client.EventToolCall, SessionID: recordedSession,
	}, client.ErrDelivery)

	if _, halted := hookflow.SessionHalted(recordedSession); halted {
		t.Fatalf("a recorded delivery failure must not latch the run")
	}

	eval, err := (haltDecorator{}).Evaluate(context.Background(), recordedCall)
	if err != nil {
		t.Fatalf("Evaluate (after recorded failure): %v", err)
	}
	if eval.Verdict != client.VerdictAllow {
		t.Fatalf("the run's next relayed call resolved verdict %q, want ALLOW (the next call tries again)", eval.Verdict)
	}

	eval, err = (haltDecorator{}).Evaluate(context.Background(), unrelatedCall)
	if err != nil {
		t.Fatalf("Evaluate (unrelated session): %v", err)
	}
	if eval.Verdict != client.VerdictAllow {
		t.Fatalf("an unrelated session resolved verdict %q, want ALLOW", eval.Verdict)
	}
}

// The other half of the contract -- a real server HALT verdict
// (WriteSessionHalt), unlike a recorded delivery failure, still refuses the
// run's next relayed call -- is already pinned above by
// TestHaltDecoratorRefusesALatchedRun.

// TestHaltDecoratorDoesNotRefuseAContinuedRunThatDidNotHalt pins the run
// keying: the run a session CURRENTLY
// belongs to is what gets consulted, never the session id or an ancestor
// run. A `/clear`/`--resume` bump onto a fresh, unlatched run must not
// inherit an old run's HALT.
func TestHaltDecoratorDoesNotRefuseAContinuedRunThatDidNotHalt(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(t.TempDir(), "enforcements.jsonl"))
	t.Setenv(obgit.EnvSessionDir, t.TempDir())
	const sessionID = "sess-continued-2"
	c := gateway.Captured{
		HTTPURL:        "https://api.anthropic.com/v1/messages",
		RequestHeaders: map[string]string{"X-Claude-Code-Session-Id": sessionID},
	}

	// Latch generation 0 (run == session id).
	hookflow.WriteSessionHalt(discardMainLogger(), sessionID, client.Evaluation{
		Verdict: client.VerdictHalt, Reason: "old run halted",
	})
	if eval, err := (haltDecorator{}).Evaluate(context.Background(), c); err != nil || eval.Verdict != client.VerdictHalt {
		t.Fatalf("generation 0 resolved (verdict=%q, err=%v), want HALT before continuing the run", eval.Verdict, err)
	}

	// Continue the session onto a fresh run (a `--resume`); the new run was
	// never itself latched.
	if _, err := (obgit.RunStore{}).Bump(sessionID); err != nil {
		t.Fatalf("Bump: %v", err)
	}

	eval, err := (haltDecorator{}).Evaluate(context.Background(), c)
	if err != nil {
		t.Fatalf("Evaluate (continued run): %v", err)
	}
	if eval.Verdict != client.VerdictAllow {
		t.Fatalf("the continued (unlatched) run resolved verdict %q, want ALLOW", eval.Verdict)
	}
}

// TestHaltDecoratorHeaderlessCallIsNeverLatched: a call this relay cannot
// attribute to a session (no X-Claude-Code-Session-Id) must fall through to
// next/allow rather than being treated as latched -- the same skip-and-count
// posture applies everywhere else a carrier is unresolvable.
func TestHaltDecoratorHeaderlessCallIsNeverLatched(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(t.TempDir(), "enforcements.jsonl"))
	t.Setenv(obgit.EnvSessionDir, t.TempDir())

	eval, err := (haltDecorator{}).Evaluate(context.Background(), gateway.Captured{
		HTTPURL:        "https://api.anthropic.com/v1/messages",
		RequestHeaders: map[string]string{},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if eval.Verdict != client.VerdictAllow {
		t.Errorf("headerless call resolved verdict %q, want ALLOW (skip and count, never latch)", eval.Verdict)
	}
}

// TestHaltDecoratorUnrecognizedHostIsNeverLatched: a host outside every
// installed provider's table (never intercepted in production, since only an
// allowlisted host reaches this relay at all) must not be treated as
// latched either -- the decorator falls through rather than guessing a
// provider.
func TestHaltDecoratorUnrecognizedHostIsNeverLatched(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(t.TempDir(), "enforcements.jsonl"))
	t.Setenv(obgit.EnvSessionDir, t.TempDir())

	eval, err := (haltDecorator{}).Evaluate(context.Background(), gateway.Captured{
		HTTPURL:        "https://example.com/v1/messages",
		RequestHeaders: map[string]string{"X-Claude-Code-Session-Id": "sess-unrelated-host"},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if eval.Verdict != client.VerdictAllow {
		t.Errorf("unrecognized-host call resolved verdict %q, want ALLOW", eval.Verdict)
	}
}

// TestCrossLaneHaltAllowsAnUnlatchedSessionThroughANilDelegate pins
// haltDecorator's own safe default end to end through a real transport.Proxy:
// with nothing latched and no real /evaluate evaluator chained behind it yet
// (next is nil for now), every call is allowed and forwarded, exactly as
// before this seam existed.
func TestCrossLaneHaltAllowsAnUnlatchedSessionThroughANilDelegate(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(t.TempDir(), "enforcements.jsonl"))
	t.Setenv(obgit.EnvSessionDir, t.TempDir())

	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"type":"message"}`)
	}))
	t.Cleanup(upstream.Close)

	ca, err := transport.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	em := &gatewayemit.Emitter{
		Elected: func() bool { return true },
		Lane:    gatewayemit.LaneProxy,
		Deliver: testSpoolDeliver(t.TempDir()),
		DID:     func() string { return "did:aip:00000000-0000-5000-a000-0000000000ca" },
		Warn:    func(string, ...any) {},
	}
	p, err := transport.New(transport.Config{Upstream: upstream.URL}, ca, em,
		transport.WithGate(haltDecorator{}, gatedFn))
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	for i := 0; i < 3; i++ {
		if status, body := serveOneCall(t, p, ca, "sess-never-halted"); status != http.StatusOK {
			t.Fatalf("call %d: got %d, want 200; body=%s", i, status, body)
		}
	}
	if got := atomic.LoadInt32(&upstreamHits); got != 3 {
		t.Errorf("upstream hits = %d, want 3 (every call forwarded)", got)
	}
}

// discardMainLogger is a throwaway *log.Logger for a test that only cares
// about the latch file WriteSessionHalt writes, not its own diagnostics.
func discardMainLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// waitUntilAttempted polls fake.AttemptsByKey() until every key it has seen
// so far reached exactly want attempts, or timeout passes -- a LaneQueue's
// own drain is asynchronous (Kick starts a goroutine), and unlike Close this
// never stops the queue from accepting further work, so a caller that still
// needs the queue for a LATER call (unlike a plain drain-to-completion at
// the end of a test) can use this instead of Close to wait out one drain
// pass.
func waitUntilAttempted(t *testing.T, fake *fakecore.Server, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		attempts := fake.AttemptsByKey()
		if len(attempts) > 0 {
			done := true
			for _, n := range attempts {
				if n < want {
					done = false
					break
				}
			}
			if done {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempts never reached %d within %s: %v", want, timeout, attempts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHaltDecoratorRecordsALatchedRefusal: a latched relayed refusal must leave
// a trace even when this lane is not the elected producer, where the relay's
// own event is skipped by the election and core never hears of the call. The
// enforcement audit is that trace, the same sink a replayed hook refusal
// writes to.
func TestHaltDecoratorRecordsALatchedRefusal(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(t.TempDir(), "enforcements.jsonl"))
	t.Setenv(obgit.EnvSessionDir, t.TempDir())
	auditPath := filepath.Join(t.TempDir(), "enforcements.jsonl")
	t.Setenv(devconfig.EnvEnforcementFile, auditPath)
	const sessionID = "sess-recorded-refusal"
	c := gateway.Captured{
		HTTPURL:        "https://api.anthropic.com/v1/messages",
		RequestHeaders: map[string]string{"X-Claude-Code-Session-Id": sessionID},
	}

	if _, err := (haltDecorator{}).Evaluate(context.Background(), c); err != nil {
		t.Fatalf("Evaluate (unlatched): %v", err)
	}
	if _, err := os.Stat(auditPath); !os.IsNotExist(err) {
		t.Fatalf("an allowed call wrote the enforcement audit (stat err %v)", err)
	}

	hookflow.WriteSessionHalt(discardMainLogger(), sessionID, client.Evaluation{
		Verdict: client.VerdictHalt, Reason: "secrets policy", PolicyID: "pol-1",
	})
	if _, err := (haltDecorator{}).Evaluate(context.Background(), c); err != nil {
		t.Fatalf("Evaluate (latched): %v", err)
	}
	body, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("a latched refusal left no enforcement record: %v", err)
	}
	var rec hookflow.EnforcementRecord
	if err := json.Unmarshal(bytes.TrimSpace(body), &rec); err != nil {
		t.Fatalf("enforcement record is not one JSON line: %v\n%s", err, body)
	}
	if rec.SessionID != sessionID || rec.Verdict != string(client.VerdictHalt) || rec.Source != hookflow.SourceSessionHalt {
		t.Errorf("enforcement record = %+v; want session %q, verdict HALT, source %q", rec, sessionID, hookflow.SourceSessionHalt)
	}
}

// TestLaneRecordCoreOutageDoesNotLatchAndTheNextCallTriesAgain proves that
// core refusing a lane record (a 503 on every v3 evaluate request) gives
// that record exactly one attempt and one retry --
// through a REAL LaneQueue drain, not a hand-called RecordDeliveryFailure --
// but no longer latches the run: that gated call is denied (nothing reaches
// core for it beyond its one retry), while the run's NEXT relayed call is
// resolved ALLOW by haltDecorator (no persisted latch to read) and, once
// core recovers, the run's next lane record actually reaches and is accepted
// by core -- proving "the next one tries again" rather than staying stuck
// denying forever.
//
// The relay's own capture records g.upstream+RequestURI as HTTPURL (gateway/
// proxy.go), not the intercepted TLS host, so a test pinning Upstream to a
// refused loopback port (as this one does, to stay network-free) would make
// haltDecorator's own host-table lookup miss -- that mismatch exists only
// under a forced Upstream override, never in production (a.runTransport
// leaves Upstream empty, so UpstreamFor(host) resolves the real intercepted
// host). Proving the refusal itself through the real relay would therefore
// prove an artifact of this test's own plumbing, not the daemon's; the direct
// call below is what the existing test already established is equivalent.
func TestLaneRecordCoreOutageDoesNotLatchAndTheNextCallTriesAgain(t *testing.T) {
	memhttptest.RequireBind(t)

	t.Setenv(devconfig.EnvSpoolRoot, t.TempDir())
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(t.TempDir(), "enforcements.jsonl"))
	t.Setenv(obgit.EnvSessionDir, t.TempDir())
	t.Setenv(devconfig.EnvHome, t.TempDir())

	fake := fakecore.New(t, fakecore.Script{})
	fake.SetOutage(true)
	t.Setenv(devconfig.EnvBaseURL, fake.URL())
	seedV3EnvIdentity(t)

	logger := log.New(io.Discard, "", 0)
	identities := resolveProviderIdentities(logger.Printf)
	queues := laneQueues(identities, logger)

	ca, err := transport.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	em := &gatewayemit.Emitter{
		Lane:    gatewayemit.LaneProxy,
		Elected: func() bool { return true },
		Deliver: routeLaneRecord(queues, nil, logger, "transport"),
		DID:     devconfig.ResolveDIDOrEmpty,
		Warn:    logger.Printf,
	}
	p, err := transport.New(transport.Config{Upstream: refusedUpstream}, ca, em)
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	const sessionID = "sess-lane-outage-nolatch-1"
	serveOneCall(t, p, ca, sessionID)

	// Wait for the queue's own drain to go fully idle -- BOTH halves of the
	// captured call attempted, not merely the first -- before snapshotting
	// anything: a snapshot taken too early can race the paired event's own
	// attempt landing at the fake core moments later. Polled rather than
	// closed: this queue is reused for a second call below, and Close is a
	// one-shot shutdown that would silently stop it from ever draining that
	// second call's own append (Kick's own doc: a Deliver landing after
	// Close leaves the record spooled, undrained).
	waitUntilAttempted(t, fake, 2, 5*time.Second)

	if _, halted := hookflow.SessionHalted(sessionID); halted {
		t.Fatalf("a delivery failure must no longer latch the run; fake evaluate attempts=%d",
			fake.V3EvaluateAttempts())
	}

	attemptsAfterFirst := fake.AttemptsByKey()
	if len(attemptsAfterFirst) == 0 {
		t.Fatal("no v3 evaluate attempt reached the fake core for the first call")
	}
	// A 503 is transient: each record gets one attempt and exactly one retry.
	for key, n := range attemptsAfterFirst {
		if n != 2 {
			t.Errorf("event (idempotency key %s) was attempted %d time(s), want exactly 2 (one attempt, one retry)", key, n)
		}
	}

	// The run's own next relayed call: haltDecorator finds no persisted
	// latch (a recorded delivery failure never wrote one) and allows it.
	c := gateway.Captured{
		HTTPURL:        "https://api.anthropic.com/v1/messages",
		RequestHeaders: map[string]string{"X-Claude-Code-Session-Id": sessionID},
	}
	eval, err := (haltDecorator{logger: logger}).Evaluate(context.Background(), c)
	if err != nil {
		t.Fatalf("Evaluate (should not be latched): %v", err)
	}
	if eval.Verdict != client.VerdictAllow {
		t.Fatalf("the run's next relayed call resolved verdict %q, want ALLOW", eval.Verdict)
	}

	// Core recovers; the run's next lane record must actually try again and
	// be accepted, not stay stuck denying forever.
	fake.SetOutage(false)
	serveOneCall(t, p, ca, sessionID)
	idleCtx2, idleCancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	queues["claude-code"].Close(idleCtx2)
	idleCancel2()

	if len(fake.Inbox()) == 0 {
		t.Fatalf("no event was accepted by core after recovery: the next call did not try again")
	}
	if _, halted := hookflow.SessionHalted(sessionID); halted {
		t.Fatalf("the run must still be unlatched after a successful delivery")
	}
}
