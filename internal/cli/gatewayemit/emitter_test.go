package gatewayemit

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

// fakeDelivery stands in for a lane daemon's bounded pool: it captures every
// accepted event in memory, keyed by session, and can simulate a saturated
// pool (Accept = false) the way the real pool's Submit returns false rather
// than blocking.
type fakeDelivery struct {
	mu     sync.Mutex
	events map[string][]client.DevEvent
	Accept bool
}

func newFakeDelivery() *fakeDelivery {
	return &fakeDelivery{events: map[string][]client.DevEvent{}, Accept: true}
}

func (f *fakeDelivery) Deliver(_ context.Context, ev client.DevEvent) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Accept {
		return false
	}
	f.events[ev.SessionID] = append(f.events[ev.SessionID], ev)
	return true
}

func (f *fakeDelivery) forSession(sessionID string) []client.DevEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]client.DevEvent(nil), f.events[sessionID]...)
}

// any reports whether anything was accepted for any session, the in-memory
// equivalent of the old "did the spool directory get any files" check.
func (f *fakeDelivery) any() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, evs := range f.events {
		if len(evs) > 0 {
			return true
		}
	}
	return false
}

// sessionsWithEvents counts distinct sessions that received at least one
// accepted event, the in-memory equivalent of counting spool session files
// (one file per session, however many events are appended to it).
func (f *fakeDelivery) sessionsWithEvents() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, evs := range f.events {
		if len(evs) > 0 {
			n++
		}
	}
	return n
}

func newTestEmitter(t *testing.T) (*Emitter, *fakeDelivery, *bytes.Buffer) {
	t.Helper()
	delivery := newFakeDelivery()
	var warnings bytes.Buffer
	em := &Emitter{
		Lane:    LaneGateway,
		Deliver: delivery.Deliver,
		DID:     func() string { return testDID },
		Warn:    func(format string, args ...any) { fmt.Fprintf(&warnings, format, args...) },
		// Elected must be set explicitly, here and in production: a nil gate is a
		// wiring defect rather than a setting, because an emitter that cannot tell
		// whether it is this machine's producer must not guess in either direction.
		Elected: func() bool { return true },
	}
	return em, delivery, &warnings
}

func spooledEvents(_ *testing.T, delivery *fakeDelivery, sessionID string) []client.DevEvent {
	return delivery.forSession(sessionID)
}

func capturedWithSession(session string) gateway.Captured {
	c := sampleCaptured()
	c.RequestHeaders = map[string]string{
		"Anthropic-Version":        "2023-06-01",
		"X-Claude-Code-Session-Id": session,
	}
	return c
}

// TestEmitSpoolsUnderTheSessionTheHeaderNames is the join. Spooling under
// anything else produces records that look like a session and join to nothing;
// and, since only hook-driven flushes drain that path, would never be
// delivered at all.
func TestEmitSpoolsUnderTheSessionTheHeaderNames(t *testing.T) {
	em, spool, _ := newTestEmitter(t)
	em.Emit(context.Background(), capturedWithSession("sess-from-header"))

	evs := spooledEvents(t, spool, "sess-from-header")
	// Two, and in this order: one relayed call is one activity with two halves,
	// and the spool delivers in append order, so the Started half reaches the
	// control plane before its Completed half is looked up by activity_id.
	if len(evs) != 2 {
		t.Fatalf("spooled %d events under the header's session, want the pair", len(evs))
	}
	if evs[0].EventType != client.EventTurnStarted || evs[1].EventType != client.EventTurnCompleted {
		t.Errorf("EventTypes = %q then %q, want TurnStarted then TurnCompleted", evs[0].EventType, evs[1].EventType)
	}
	if evs[1].Span == nil || evs[1].Span.HTTPStatus != 200 {
		t.Error("the observed exchange did not survive the spool round-trip")
	}
	if evs[0].GatewayRequestID == "" {
		t.Error("GatewayRequestID lost; activity_id would be empty on delivery")
	}
}

// TestFlushFiresOnceNotOncePerEvent pins the gateway.go regression: a
// captured call's two halves (Started, Completed) must trigger the nudge
// ONCE, after both are accepted, not once per event submitted to Deliver.
func TestFlushFiresOnceNotOncePerEvent(t *testing.T) {
	em, delivery, _ := newTestEmitter(t)
	var flushes int
	var lastSession string
	em.Flush = func(sessionID string) { flushes++; lastSession = sessionID }

	em.Emit(context.Background(), capturedWithSession("sess-flush-once"))

	if len(delivery.forSession("sess-flush-once")) != 2 {
		t.Fatalf("fixture drift: expected the pair delivered before checking Flush")
	}
	if flushes != 1 {
		t.Errorf("Flush fired %d time(s) for one captured call carrying 2 events, want exactly 1", flushes)
	}
	if lastSession != "sess-flush-once" {
		t.Errorf("Flush session = %q, want %q", lastSession, "sess-flush-once")
	}
}

// TestFlushDoesNotFireWhenDeliveryIsAbandoned mirrors the old Spool-backed
// behavior: a dropped Started must skip Completed AND the nudge, since
// nothing later would exist for a flush to do anything useful with.
func TestFlushDoesNotFireWhenDeliveryIsAbandoned(t *testing.T) {
	em, delivery, _ := newTestEmitter(t)
	delivery.Accept = false
	var flushes int
	em.Flush = func(sessionID string) { flushes++ }

	em.Emit(context.Background(), capturedWithSession("sess-flush-abandoned"))

	if flushes != 0 {
		t.Errorf("Flush fired %d time(s) after every event was dropped, want 0", flushes)
	}
}

// TestNoSessionHeaderEmitsNothingAndSaysSo is the honest-silence case, and the
// warning is half the requirement. Inventing one files governance records that
// claim a session they cannot join, which is the overstatement this product
// exists to prevent; and they would rot unflushed besides.
func TestNoSessionHeaderEmitsNothingAndSaysSo(t *testing.T) {
	em, spool, warnings := newTestEmitter(t)
	c := sampleCaptured()
	c.RequestHeaders = map[string]string{"Anthropic-Version": "2023-06-01"} // no session header
	em.Emit(context.Background(), c)

	if spool.any() {
		t.Error("delivered an event with no session id; a synthesized session joins to nothing")
	}
	got := warnings.String()
	if got == "" {
		t.Fatal("no warning: a silent gateway is indistinguishable from a broken one")
	}
	if !strings.Contains(strings.ToLower(got), "x-claude-code-session-id") {
		t.Errorf("warning does not name the missing header, so it is not actionable: %q", got)
	}
}

// TestWarningIsEmittedOnce keeps a per-call warning from filling the daemon
// log: ~52 model calls were measured per turn window.
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

// TestEmitSurvivesADeliverDrop is INV-3 at the relay boundary: a saturated
// delivery pool's accepted risk of losing a record must never panic or
// block the relay, only warn.
func TestEmitSurvivesADeliverDrop(t *testing.T) {
	em, delivery, warnings := newTestEmitter(t)
	delivery.Accept = false
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Emit panicked into the relay path: %v", r)
		}
	}()
	em.Emit(context.Background(), capturedWithSession("sess-1"))
	if warnings.String() == "" {
		t.Error("a dropped event left no trace at all")
	}
	if delivery.any() {
		t.Error("delivery.Accept was false; nothing should have been recorded as accepted")
	}
}

// TestSessionHeaderLookupIsCanonical guards the one spelling that works. The
// capture side canonicalizes with textproto.CanonicalMIMEHeaderKey, so a
// lookup for the lowercase wire spelling silently misses on every request and
// the gateway would report "no session id" forever.
func TestSessionHeaderLookupIsCanonical(t *testing.T) {
	em, spool, _ := newTestEmitter(t)
	c := sampleCaptured()
	c.RequestHeaders = map[string]string{"X-Claude-Code-Session-Id": "sess-canon"}
	em.Emit(context.Background(), c)
	if len(spooledEvents(t, spool, "sess-canon")) != 2 {
		t.Error("canonical header key was not matched")
	}
}

// TestAgentIDIsBoundWhenPresent; Claude Code sends the agent header only when
// an agent context exists, so its absence is normal and must not read as a
// fault.
func TestAgentIDIsBoundWhenPresent(t *testing.T) {
	em, spool, warnings := newTestEmitter(t)
	c := capturedWithSession("sess-agent")
	c.RequestHeaders["X-Claude-Code-Agent-Id"] = "agent-7"
	em.Emit(context.Background(), c)

	evs := spooledEvents(t, spool, "sess-agent")
	if len(evs) != 2 {
		t.Fatalf("spooled %d events, want the pair", len(evs))
	}
	for _, ev := range evs {
		// Both halves, or the two rows of one activity disagree about whose it is.
		if ev.AgentID != "agent-7" {
			t.Errorf("%s: AgentID not bound: %+v", ev.EventType, ev)
		}
	}

	em2, spool2, warnings2 := newTestEmitter(t)
	em2.Emit(context.Background(), capturedWithSession("sess-noagent"))
	if evs := spooledEvents(t, spool2, "sess-noagent"); len(evs) != 2 || evs[0].AgentID != "" || evs[1].AgentID != "" {
		t.Errorf("a call with no agent header did not emit cleanly: %+v", evs)
	}
	if warnings.String() != "" || warnings2.String() != "" {
		t.Errorf("an optional header produced a warning: %q %q", warnings.String(), warnings2.String())
	}
}

// TestAgentIDNeverPerturbsTheActivityID. AgentID feeds the hook path's
// ":agent:" branch, and the gateway namespace must win regardless; otherwise
// binding an optional attribution field would silently move requirement 8's
// boundary.
func TestAgentIDNeverPerturbsTheActivityID(t *testing.T) {
	base := mustEvent(LaneGateway, Identity{SessionID: "s", DeveloperDID: testDID}, "req-1", sampleAt, sampleCaptured())
	withAgent := mustEvent(LaneGateway, Identity{SessionID: "s", DeveloperDID: testDID, AgentID: "agent-7"}, "req-1", sampleAt, sampleCaptured())
	if base.GatewayRequestID != withAgent.GatewayRequestID {
		t.Fatal("fixture drift")
	}
	if base.EventID != withAgent.EventID {
		t.Error("AgentID changed the idempotency key; a retry from a subagent context would look like a new event")
	}
}

// TestUnusableUpstreamRequestIDFallsBack. The upstream id becomes part of
// activity_id, which core stores and dedupes on, so an oversized or non-
// printable value must not reach it verbatim.
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
			if len(evs) != 2 {
				t.Fatalf("spooled %d events, want the pair", len(evs))
			}
			for _, ev := range evs {
				if ev.GatewayRequestID == bad {
					t.Errorf("%s: unusable upstream id was used verbatim: %q", ev.EventType, bad)
				}
				if !strings.HasPrefix(ev.GatewayRequestID, "gw-") {
					t.Errorf("%s: no fallback id was minted: %q", ev.EventType, ev.GatewayRequestID)
				}
			}
			// One minted id for the call, not one per half, or the two halves land in
			// different activities and neither is ever paired.
			if evs[0].GatewayRequestID != evs[1].GatewayRequestID {
				t.Errorf("the halves got different fallback ids (%q, %q)", evs[0].GatewayRequestID, evs[1].GatewayRequestID)
			}
		})
	}
}

// TestUsableUpstreamRequestIDIsPreferred keeps the bound from throwing away
// the real id; the provider's own id is what makes a stored span joinable to a
// support ticket.
func TestUsableUpstreamRequestIDIsPreferred(t *testing.T) {
	em, spool, _ := newTestEmitter(t)
	c := capturedWithSession("sess-good-id")
	c.ResponseHeaders = map[string]string{"Request-Id": "req_011CSabcDEF123"}
	em.Emit(context.Background(), c)

	evs := spooledEvents(t, spool, "sess-good-id")
	if len(evs) != 2 {
		t.Fatalf("spooled %d events, want the pair", len(evs))
	}
	for _, ev := range evs {
		if ev.GatewayRequestID != "req_011CSabcDEF123" {
			t.Errorf("%s: provider request id not used: %+v", ev.EventType, ev)
		}
	}
}

// TestTheWarningReturnsAfterTheInterval.
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
		t.Errorf("warned %d times after the interval elapsed, want 2; a standing fault went silent", n)
	}
}

// TestDIDIsResolvedLazilySoInitTakesEffectWithoutARestart.
func TestDIDIsResolvedLazilySoInitTakesEffectWithoutARestart(t *testing.T) {
	em, spool, warnings := newTestEmitter(t)
	did := ""
	em.DID = func() string { return did }

	em.Emit(context.Background(), capturedWithSession("sess-1"))
	if spool.any() {
		t.Fatal("delivered an event with no DID; nothing could have attributed it")
	}
	// `init --provider`, not `auth`: identity is per tool and auth no longer
	// writes one, so the old remedy would send a reader somewhere that cannot
	// fix a lane recording nothing.
	if !strings.Contains(warnings.String(), "openbox init --provider") {
		t.Errorf("the warning does not name the remedy: %q", warnings.String())
	}

	did = testDID
	em.Emit(context.Background(), capturedWithSession("sess-1"))
	evs := spooledEvents(t, spool, "sess-1")
	if len(evs) != 2 {
		t.Fatalf("spooled %d events after the DID appeared, want the pair; a restart should not be required", len(evs))
	}
	for _, ev := range evs {
		if ev.DeveloperDID != testDID {
			t.Errorf("%s: DeveloperDID = %q", ev.EventType, ev.DeveloperDID)
		}
	}
}

// TestDIDIsCachedOnceResolved keeps the lazy read from becoming a per-call
// file read at ~52 model calls per turn.
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

// TestUnusableSessionHeaderIsRefusedAndReported. Only the far less load-
// bearing upstream request id was bounded.
func TestUnusableSessionHeaderIsRefusedAndReported(t *testing.T) {
	for name, id := range map[string]string{
		"over the length bound": strings.Repeat("s", maxSessionIDLen+1),
		"control character":     "sess\x00id",
		"newline":               "sess\nid",
		"path traversal":        "../../escape",
		"bare parent":           "..",
		"path separator":        "a/b",
	} {
		t.Run(name, func(t *testing.T) {
			delivery := newFakeDelivery()
			var warned int
			e := &Emitter{
				Lane:    LaneGateway,
				Deliver: delivery.Deliver,
				DID:     func() string { return "did:aip:x" },
				Warn:    func(string, ...any) { warned++ },
				Now:     func() time.Time { return time.Unix(0, 0).UTC() },
			}
			e.Emit(context.Background(), gateway.Captured{
				HTTPMethod:     "POST",
				RequestHeaders: map[string]string{sessionHeader: id},
			})

			if delivery.any() {
				t.Error("an unusable session id produced a delivered event")
			}
			if warned == 0 {
				t.Error("the drop was silent; a governance gap nobody is told about is indistinguishable from a working gateway")
			}
		})
	}
}

// TestAUsableSessionHeaderStillSpools keeps the bound from becoming a blanket
// refusal: a real Claude Code session id is a UUID and must pass.
func TestAUsableSessionHeaderStillSpools(t *testing.T) {
	delivery := newFakeDelivery()
	e := &Emitter{
		Lane:    LaneGateway,
		Deliver: delivery.Deliver,
		DID:     func() string { return "did:aip:x" },
		Warn:    func(string, ...any) {},
		Now:     func() time.Time { return time.Unix(0, 0).UTC() },
		Elected: func() bool { return true },
	}
	e.Emit(context.Background(), gateway.Captured{
		HTTPMethod:     "POST",
		RequestHeaders: map[string]string{sessionHeader: "3f1c9a6e-4b2d-4c8a-9e10-7d5b2a8c4f61"},
	})
	if !delivery.any() {
		t.Error("a valid UUID session id was refused; the bound is too tight to be useful")
	}
}

// TestOnlyModelCallsWarnAboutAMissingSession. A non-model call must still
// produce no event.
func TestOnlyModelCallsWarnAboutAMissingSession(t *testing.T) {
	noSession := func(method, url string) gateway.Captured {
		c := sampleCaptured()
		c.HTTPMethod = method
		c.HTTPURL = url
		c.RequestHeaders = map[string]string{"Anthropic-Version": "2023-06-01"}
		return c
	}

	t.Run("a health check is silent", func(t *testing.T) {
		em, spool, warnings := newTestEmitter(t)
		em.Emit(context.Background(), noSession(http.MethodHead, "https://api.anthropic.com/api/hello"))
		if got := warnings.String(); got != "" {
			t.Errorf("a health check produced a governance warning: %q", got)
		}
		if spool.any() {
			t.Error("a health check was delivered as a governance event")
		}
	})

	t.Run("a model listing is silent", func(t *testing.T) {
		em, _, warnings := newTestEmitter(t)
		em.Emit(context.Background(), noSession(http.MethodGet, "https://api.anthropic.com/v1/models"))
		if got := warnings.String(); got != "" {
			t.Errorf("a model listing produced a governance warning: %q", got)
		}
	})

	t.Run("a model call still warns", func(t *testing.T) {
		em, _, warnings := newTestEmitter(t)
		em.Emit(context.Background(), noSession(http.MethodPost, "https://api.anthropic.com/v1/messages"))
		if !strings.Contains(strings.ToLower(warnings.String()), "x-claude-code-session-id") {
			t.Errorf("a POSTed model call with no session id did not warn: %q", warnings.String())
		}
	})

	t.Run("an unknown POST path still warns", func(t *testing.T) {
		em, _, warnings := newTestEmitter(t)
		em.Emit(context.Background(), noSession(http.MethodPost, "https://api.anthropic.com/v1/something-new"))
		if warnings.String() == "" {
			t.Error("an unrecognised POST was silently ignored; the predicate must err toward warning")
		}
	})
}

// TestAgentIDIsBounded pins the third caller-supplied id to the same rule as
// the other two.
func TestAgentIDIsBounded(t *testing.T) {
	if got := usableAgentID(strings.Repeat("a", maxRequestIDLen+1)); got != "" {
		t.Errorf("an over-long agent id was accepted (%d chars kept); it must be dropped", len(got))
	}
	if got := usableAgentID("agent\nid"); got != "" {
		t.Error("an agent id containing a newline was accepted")
	}
	if got := usableAgentID("agent-a1b2"); got != "agent-a1b2" {
		t.Errorf("a normal agent id was dropped: %q", got)
	}
	if got := usableAgentID(""); got != "" {
		t.Errorf("an absent agent id must stay absent, got %q", got)
	}
}
