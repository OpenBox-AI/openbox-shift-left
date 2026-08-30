package gatewayemit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

func newTestEmitter(t *testing.T) (*Emitter, hookflow.Spool, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	spool := hookflow.Spool{Dir: filepath.Join(dir, "cc-spool")}
	var warnings bytes.Buffer
	em := &Emitter{
		Lane:  LaneGateway,
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

// TestUnusableSessionHeaderIsRefusedAndReported.
//
// The session id comes off a request header, on a loopback listener the package's
// own doc says "performs no caller authentication" — and it is used as a spool
// FILENAME, as the per-session flush-debounce key, and as core's run_id. Only the
// far less load-bearing upstream request id was bounded.
//
// Each case is a distinct concrete failure: an over-long id becomes an
// ENAMETOOLONG filename on every call; a control character lands in a stored key;
// a path separator escapes the spool directory. All three must be refused with a
// throttled warning rather than spooled.
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
			spool := hookflow.Spool{Dir: t.TempDir()}
			var warned int
			e := &Emitter{
				Lane:  LaneGateway,
				Spool: spool,
				DID:   func() string { return "did:aip:x" },
				Warn:  func(string, ...any) { warned++ },
				Now:   func() time.Time { return time.Unix(0, 0).UTC() },
			}
			e.Emit(context.Background(), gateway.Captured{
				HTTPMethod:     "POST",
				RequestHeaders: map[string]string{sessionHeader: id},
			})

			files, _ := os.ReadDir(spool.Dir)
			if len(files) != 0 {
				t.Errorf("an unusable session id produced %d spool file(s): %v", len(files), files)
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
	spool := hookflow.Spool{Dir: t.TempDir()}
	e := &Emitter{
		Lane:  LaneGateway,
		Spool: spool,
		DID:   func() string { return "did:aip:x" },
		Warn:  func(string, ...any) {},
		Now:   func() time.Time { return time.Unix(0, 0).UTC() },
	}
	e.Emit(context.Background(), gateway.Captured{
		HTTPMethod:     "POST",
		RequestHeaders: map[string]string{sessionHeader: "3f1c9a6e-4b2d-4c8a-9e10-7d5b2a8c4f61"},
	})
	files, _ := os.ReadDir(spool.Dir)
	if len(files) == 0 {
		t.Error("a valid UUID session id was refused; the bound is too tight to be useful")
	}
}

// TestOnlyModelCallsWarnAboutAMissingSession. A healthy install relays calls that
// legitimately have no session id — Claude Code sends `HEAD /api/hello` on
// startup — and warning about those announced "no governance events are being
// sent" on a gateway that was working perfectly. A recurring false alarm in the
// one channel that reports real gaps is worse than no channel.
//
// The SKIP is unconditional; only the WARNING is gated. A non-model call must
// still produce no event.
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
		if entries, _ := os.ReadDir(spool.Dir); len(entries) != 0 {
			t.Error("a health check was spooled as a governance event")
		}
	})

	t.Run("a model listing is silent", func(t *testing.T) {
		em, _, warnings := newTestEmitter(t)
		em.Emit(context.Background(), noSession(http.MethodGet, "https://api.anthropic.com/v1/models"))
		if got := warnings.String(); got != "" {
			t.Errorf("a model listing produced a governance warning: %q", got)
		}
	})

	// The half that must NOT be suppressed: a real inference request with no
	// session id is a genuine governance gap and has to stay loud.
	t.Run("a model call still warns", func(t *testing.T) {
		em, _, warnings := newTestEmitter(t)
		em.Emit(context.Background(), noSession(http.MethodPost, "https://api.anthropic.com/v1/messages"))
		if !strings.Contains(strings.ToLower(warnings.String()), "x-claude-code-session-id") {
			t.Errorf("a POSTed model call with no session id did not warn: %q", warnings.String())
		}
	})

	// Permissive in the safe direction: an unrecognised POST path still warns,
	// because a missed warning hides a real gap while a spurious one is only noise.
	t.Run("an unknown POST path still warns", func(t *testing.T) {
		em, _, warnings := newTestEmitter(t)
		em.Emit(context.Background(), noSession(http.MethodPost, "https://api.anthropic.com/v1/something-new"))
		if warnings.String() == "" {
			t.Error("an unrecognised POST was silently ignored; the predicate must err toward warning")
		}
	})
}

// TestAgentIDIsBounded pins the third caller-supplied id to the same rule as the
// other two. It is the one that reaches a SIGNED payload's metadata, so an
// unbounded value does not merely mis-attribute the call — the event grows past
// what core accepts and the whole record of that model call is lost.
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
