package gatewayemit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/gateway"
)

func newTestEmitter(t *testing.T) (*Emitter, hookflow.Spool, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	spool := hookflow.Spool{Dir: filepath.Join(dir, "cc-spool")}
	var warnings bytes.Buffer
	em := &Emitter{
		Spool: spool,
		DID:   func() string { return testDID },
		Warn:  func(format string, args ...any) { fmt.Fprintf(&warnings, format, args...) },
		// nil Flush: the detached flusher must never be spawned from a test.
	}
	return em, spool, &warnings
}

func spooledEvents(t *testing.T, spool hookflow.Spool, sessionID string) []client.DevEvent {
	t.Helper()
	f, err := os.Open(spool.SessionPath(sessionID))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []client.DevEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		var ev client.DevEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("spool line is not a DevEvent: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

func capturedWithSession(session string) gateway.Captured {
	c := sampleCaptured()
	c.RequestHeaders = map[string]string{
		"Anthropic-Version":        "2023-06-01",
		"X-Claude-Code-Session-Id": session,
	}
	return c
}

// TestEmitSpoolsUnderTheSessionTheHeaderNames is the join. The gateway and the
// hooks describe the same session from two vantage points, and they only meet if
// the gateway files its evidence under the id the hooks already use. Spooling
// under anything else produces records that look like a session and join to
// nothing — and, since only hook-driven flushes drain that path, would never be
// delivered at all.
func TestEmitSpoolsUnderTheSessionTheHeaderNames(t *testing.T) {
	em, spool, _ := newTestEmitter(t)
	em.Emit(context.Background(), capturedWithSession("sess-from-header"))

	evs := spooledEvents(t, spool, "sess-from-header")
	if len(evs) != 1 {
		t.Fatalf("spooled %d events under the header's session, want 1", len(evs))
	}
	if evs[0].EventType != client.EventTurnCompleted {
		t.Errorf("EventType = %q", evs[0].EventType)
	}
	if evs[0].Span == nil || evs[0].Span.HTTPStatus != 200 {
		t.Error("the observed exchange did not survive the spool round-trip")
	}
	if evs[0].GatewayRequestID == "" {
		t.Error("GatewayRequestID lost — activity_id would be empty on delivery")
	}
}

// TestNoSessionHeaderEmitsNothingAndSaysSo is the honest-silence case, and the
// warning is half the requirement.
//
// Whether Claude Code sends x-claude-code-session-id is UNVERIFIED (ADR-0021 §8 /
// probe P0). If it does not, the choice is between inventing a session id and
// emitting nothing. Inventing one files governance records that claim a session
// they cannot join, which is the overstatement this product exists to prevent —
// and they would rot unflushed besides. So the gateway stays silent, and says
// why, because silence alone is indistinguishable from a broken daemon.
func TestNoSessionHeaderEmitsNothingAndSaysSo(t *testing.T) {
	em, spool, warnings := newTestEmitter(t)
	c := sampleCaptured()
	c.RequestHeaders = map[string]string{"Anthropic-Version": "2023-06-01"} // no session header
	em.Emit(context.Background(), c)

	if entries, _ := os.ReadDir(spool.Dir); len(entries) != 0 {
		t.Errorf("spooled %d files with no session id — a synthesized session joins to nothing", len(entries))
	}
	got := warnings.String()
	if got == "" {
		t.Fatal("no warning: a silent gateway is indistinguishable from a broken one")
	}
	if !strings.Contains(strings.ToLower(got), "x-claude-code-session-id") {
		t.Errorf("warning does not name the missing header, so it is not actionable: %q", got)
	}
}

// TestWarningIsEmittedOnce keeps a per-call warning from filling the daemon log:
// ~52 model calls were measured per turn window.
func TestWarningIsEmittedOnce(t *testing.T) {
	em, _, warnings := newTestEmitter(t)
	c := sampleCaptured()
	c.RequestHeaders = map[string]string{"Anthropic-Version": "2023-06-01"}
	for i := 0; i < 5; i++ {
		em.Emit(context.Background(), c)
	}
	if n := strings.Count(strings.ToLower(warnings.String()), "x-claude-code-session-id"); n != 1 {
		t.Errorf("warned %d times, want exactly 1", n)
	}
}

// TestEmitSurvivesAnUnwritableSpool is INV-3 at the relay boundary. This runs
// inside the request goroutine; a governance sensor that panics or propagates an
// error there breaks the developer's model call, which is a strictly worse
// outcome than losing one event.
func TestEmitSurvivesAnUnwritableSpool(t *testing.T) {
	em, _, warnings := newTestEmitter(t)
	// A file where the spool dir should be: MkdirAll fails, Append errors.
	if err := os.WriteFile(em.Spool.Dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Emit panicked into the relay path: %v", r)
		}
	}()
	em.Emit(context.Background(), capturedWithSession("sess-1"))
	if warnings.String() == "" {
		t.Error("a dropped event left no trace at all")
	}
}

// TestSessionHeaderLookupIsCanonical guards the one spelling that works. The
// capture side canonicalizes with textproto.CanonicalMIMEHeaderKey, so a lookup
// for the lowercase wire spelling silently misses on every request and the
// gateway would report "no session id" forever.
func TestSessionHeaderLookupIsCanonical(t *testing.T) {
	em, spool, _ := newTestEmitter(t)
	c := sampleCaptured()
	c.RequestHeaders = map[string]string{"X-Claude-Code-Session-Id": "sess-canon"}
	em.Emit(context.Background(), c)
	if len(spooledEvents(t, spool, "sess-canon")) != 1 {
		t.Error("canonical header key was not matched")
	}
}

// TestAgentIDIsBoundWhenPresent — Claude Code sends the agent header only when an
// agent context exists, so its absence is normal and must not read as a fault.
func TestAgentIDIsBoundWhenPresent(t *testing.T) {
	em, spool, warnings := newTestEmitter(t)
	c := capturedWithSession("sess-agent")
	c.RequestHeaders["X-Claude-Code-Agent-Id"] = "agent-7"
	em.Emit(context.Background(), c)

	evs := spooledEvents(t, spool, "sess-agent")
	if len(evs) != 1 || evs[0].AgentID != "agent-7" {
		t.Fatalf("AgentID not bound: %+v", evs)
	}

	// And its absence is silent.
	em2, spool2, warnings2 := newTestEmitter(t)
	em2.Emit(context.Background(), capturedWithSession("sess-noagent"))
	if evs := spooledEvents(t, spool2, "sess-noagent"); len(evs) != 1 || evs[0].AgentID != "" {
		t.Errorf("a call with no agent header did not emit cleanly: %+v", evs)
	}
	if warnings.String() != "" || warnings2.String() != "" {
		t.Errorf("an optional header produced a warning: %q %q", warnings.String(), warnings2.String())
	}
}

// TestAgentIDNeverPerturbsTheActivityID. AgentID feeds the hook path's ":agent:"
// branch, and the gateway namespace must win regardless — otherwise binding an
// optional attribution field would silently move requirement 8's boundary.
func TestAgentIDNeverPerturbsTheActivityID(t *testing.T) {
	base := EventFor(Identity{SessionID: "s", DeveloperDID: testDID}, "req-1", sampleAt, sampleCaptured())
	withAgent := EventFor(Identity{SessionID: "s", DeveloperDID: testDID, AgentID: "agent-7"}, "req-1", sampleAt, sampleCaptured())
	if base.GatewayRequestID != withAgent.GatewayRequestID {
		t.Fatal("fixture drift")
	}
	if base.EventID != withAgent.EventID {
		t.Error("AgentID changed the idempotency key; a retry from a subagent context would look like a new event")
	}
}

// TestUnusableUpstreamRequestIDFallsBack. The upstream id becomes part of
// activity_id, which core stores and dedupes on, so an oversized or non-printable
// value must not reach it verbatim.
func TestUnusableUpstreamRequestIDFallsBack(t *testing.T) {
	for name, bad := range map[string]string{
		"oversized":       strings.Repeat("x", maxRequestIDLen+1),
		"newline":         "req_1\nInjected: yes",
		"control char":    "req_\x00null",
		"non-ascii":       "req_ünïcode",
		"space separated": "req 1",
	} {
		t.Run(name, func(t *testing.T) {
			em, spool, _ := newTestEmitter(t)
			c := capturedWithSession("sess-bad-id")
			c.ResponseHeaders = map[string]string{"Request-Id": bad}
			em.Emit(context.Background(), c)

			evs := spooledEvents(t, spool, "sess-bad-id")
			if len(evs) != 1 {
				t.Fatalf("spooled %d events", len(evs))
			}
			if evs[0].GatewayRequestID == bad {
				t.Errorf("unusable upstream id was used verbatim: %q", bad)
			}
			if !strings.HasPrefix(evs[0].GatewayRequestID, "gw-") {
				t.Errorf("no fallback id was minted: %q", evs[0].GatewayRequestID)
			}
		})
	}
}

// TestUsableUpstreamRequestIDIsPreferred keeps the bound from throwing away the
// real id — the provider's own id is what makes a stored span joinable to a
// support ticket.
func TestUsableUpstreamRequestIDIsPreferred(t *testing.T) {
	em, spool, _ := newTestEmitter(t)
	c := capturedWithSession("sess-good-id")
	c.ResponseHeaders = map[string]string{"Request-Id": "req_011CSabcDEF123"}
	em.Emit(context.Background(), c)

	evs := spooledEvents(t, spool, "sess-good-id")
	if len(evs) != 1 || evs[0].GatewayRequestID != "req_011CSabcDEF123" {
		t.Errorf("provider request id not used: %+v", evs)
	}
}

// TestTheWarningReturnsAfterTheInterval. A once-per-process warning is not
// detection: a daemon runs for weeks, and a standing fault has to keep saying so.
func TestTheWarningReturnsAfterTheInterval(t *testing.T) {
	em, _, warnings := newTestEmitter(t)
	now := sampleAt
	em.Now = func() time.Time { return now }

	c := sampleCaptured()
	c.RequestHeaders = map[string]string{"Anthropic-Version": "2023-06-01"}

	em.Emit(context.Background(), c)
	em.Emit(context.Background(), c)
	if n := strings.Count(strings.ToLower(warnings.String()), "x-claude-code-session-id"); n != 1 {
		t.Fatalf("warned %d times inside the interval, want 1", n)
	}

	now = now.Add(warnInterval + time.Minute)
	em.Emit(context.Background(), c)
	if n := strings.Count(strings.ToLower(warnings.String()), "x-claude-code-session-id"); n != 2 {
		t.Errorf("warned %d times after the interval elapsed, want 2 — a standing fault went silent", n)
	}
}

// TestDIDIsResolvedLazilySoAuthTakesEffectWithoutARestart. The gateway is a
// supervised daemon that can easily be started before `openbox auth` finishes —
// that is the ordinary order, not an edge case. Resolving the DID once at
// construction meant such a daemon relayed without recording for its entire life,
// with a single startup line as the only witness.
func TestDIDIsResolvedLazilySoAuthTakesEffectWithoutARestart(t *testing.T) {
	em, spool, warnings := newTestEmitter(t)
	did := ""
	em.DID = func() string { return did }

	em.Emit(context.Background(), capturedWithSession("sess-1"))
	if entries, _ := os.ReadDir(spool.Dir); len(entries) != 0 {
		t.Fatal("spooled an event with no DID — nothing could have attributed it")
	}
	if !strings.Contains(warnings.String(), "openbox auth") {
		t.Errorf("the warning does not name the remedy: %q", warnings.String())
	}

	// `openbox auth` runs in another terminal. No restart.
	did = testDID
	em.Emit(context.Background(), capturedWithSession("sess-1"))
	evs := spooledEvents(t, spool, "sess-1")
	if len(evs) != 1 {
		t.Fatalf("spooled %d events after the DID appeared, want 1 — a restart should not be required", len(evs))
	}
	if evs[0].DeveloperDID != testDID {
		t.Errorf("DeveloperDID = %q", evs[0].DeveloperDID)
	}
}

// TestDIDIsCachedOnceResolved keeps the lazy read from becoming a per-call file
// read at ~52 model calls per turn.
func TestDIDIsCachedOnceResolved(t *testing.T) {
	em, _, _ := newTestEmitter(t)
	calls := 0
	em.DID = func() string { calls++; return testDID }

	for i := 0; i < 4; i++ {
		em.Emit(context.Background(), capturedWithSession("sess-1"))
	}
	if calls != 1 {
		t.Errorf("resolved the DID %d times, want 1 once it is known", calls)
	}
}
