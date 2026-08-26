package gatewayemit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/gateway"
)

// sessionHeader is how a relayed request names the session it belongs to.
//
// Canonical spelling, because the capture side runs every key through
// textproto.CanonicalMIMEHeaderKey. A lookup for the lowercase wire spelling
// misses on every request, and the daemon would report "no session id" forever
// while the header was sitting right there.
const sessionHeader = "X-Claude-Code-Session-Id"

// agentHeader scopes a call to a subagent. Unlike the session header it is
// CONDITIONAL — Claude Code emits it only when an agent context exists — so its
// absence is normal and must never be treated as a fault.
const agentHeader = "X-Claude-Code-Agent-Id"

// warnInterval bounds how often one recurring fault is reported. Once per process
// lifetime is not detection: a daemon runs for weeks, so a single line scrolls out
// of a log nobody tails and the gap goes silent again. Once per hour keeps a
// standing fault standing without drowning a log at ~52 model calls per turn.
const warnInterval = time.Hour

// upstreamRequestHeader is the provider's own id for the call. Preferred as the
// gateway request id because it is the same id the provider's logs use, which
// makes a stored span joinable to a support ticket.
const upstreamRequestHeader = "Request-Id"

// Emitter turns each relayed model call into a spooled governance event.
//
// It writes to the SAME spool the Claude Code hooks use and files events under
// the session id the request carried, so the hook path's existing flushers drain
// them with no new delivery mechanism. That has a second consequence worth
// stating: this process never touches the signing key or the obx_ credential —
// the flusher does. The relay daemon does zero secret I/O.
type Emitter struct {
	Spool hookflow.Spool

	// DID resolves the developer identity, and it is a FUNCTION because this is a
	// long-lived daemon. Reading it once at construction meant a gateway started
	// before `openbox auth` finished stayed capture-less for its whole life —
	// which is the ordinary order of operations, not an edge case, and the hook
	// path never had the problem because each hook is a fresh process. Resolved
	// lazily and cached on the first non-empty answer, so the steady state is
	// still one read.
	DID func() string

	// Warn reports a dropped event. Required in practice: every path that
	// declines to emit is a governance gap, and a gap nobody is told about is
	// indistinguishable from a working gateway.
	Warn func(format string, args ...any)

	// Flush nudges delivery for a session; nil disables the nudge, which is what
	// tests want (the real one spawns a detached process). Without it the events
	// still ship on the session's next hook-driven flush.
	Flush func(sessionID string)

	// Now is injectable so a test can pin a timestamp; nil ⇒ time.Now.
	Now func() time.Time

	mu                sync.Mutex
	lastNoSessionWarn time.Time
	lastNoDIDWarn     time.Time
	cachedDID         string
	fallbackSeq       uint64
}

// developerDID resolves the DID, caching the first non-empty answer. Re-reading
// until one appears is what lets `openbox auth` take effect without a restart.
func (e *Emitter) developerDID() string {
	e.mu.Lock()
	if e.cachedDID != "" {
		did := e.cachedDID
		e.mu.Unlock()
		return did
	}
	e.mu.Unlock()

	if e.DID == nil {
		return ""
	}
	did := e.DID()
	if did == "" {
		return ""
	}
	e.mu.Lock()
	e.cachedDID = did
	e.mu.Unlock()
	return did
}

// Emit records one relayed call. It never returns an error and never panics.
//
// This runs inside the relay's request goroutine (gateway/proxy.go, after the
// response has finished streaming). A sensor that panicked or blocked here would
// break the developer's model call, which is strictly worse than losing one
// event — the same fail-open rule the hook path holds (INV-3).
func (e *Emitter) Emit(ctx context.Context, c gateway.Captured) {
	defer func() {
		if r := recover(); r != nil {
			e.warn("openbox gateway: dropped a captured call after a panic: %v", r)
		}
	}()

	// A REFUSED call arrives here too (gateway/proxy.go emits one so a stopped
	// call is not invisible in stored data), and this mapping would report it as a
	// COMPLETED turn — the refusal status is the only hint, and 403 is also a real
	// upstream answer. Latent today: WithGate has no production caller, so nothing
	// is gated and this path cannot fire. It must be settled before the gate is
	// wired, and how a refusal is represented on the wire is phase 06's decision,
	// not something to invent here — `status` is tool-only by client.statusFor.
	// Recorded in phase 06 rather than left to be rediscovered.

	did := e.developerDID()
	if did == "" {
		e.warnThrottled(&e.lastNoDIDWarn, "openbox gateway: no developer DID configured, so relayed model calls are NOT being recorded. Run `openbox auth`; no restart is needed.")
		return
	}

	sessionID := c.RequestHeaders[sessionHeader]
	if sessionID == "" {
		// Deliberately no synthesized id, and the reason is NOT that such events
		// would go undelivered — they would: the flush nudge below takes whatever
		// session id it is handed. The reasons are that a made-up id fabricates a
		// session core never saw start, that anything can reach a loopback port
		// (curl, health checks, probes) and none of it deserves a session record,
		// and that inventing an attribution key is precisely the mis-attribution
		// this product exists to prevent.
		//
		// The header itself is statically evidenced in Claude Code 2.1.229 — it
		// sits in the unconditional default header map beside x-app and
		// User-Agent — but that is code-path evidence, not observed traffic, so
		// this path stays live and must stay loud.
		e.warnThrottled(&e.lastNoSessionWarn, "openbox gateway: relayed calls carry no %s header, so nothing can be attributed to a session and no governance events are being sent. "+
			"The model calls themselves are unaffected.", sessionHeader)
		return
	}

	id := Identity{
		SessionID:    sessionID,
		DeveloperDID: did,
		AgentID:      c.RequestHeaders[agentHeader],
	}
	ev := EventFor(id, e.requestID(c), e.now(), c)
	if err := e.Spool.Append(ev); err != nil {
		e.warn("openbox gateway: dropped event %s: %v", ev.EventID, err)
		return
	}
	if e.Flush != nil {
		e.Flush(sessionID)
	}
}

// requestID picks the id this call is known by. The provider's own id when it
// sent one; otherwise a fresh random one, which only has to be unique — the
// event carries its own idempotency key derived from it, and the built event is
// serialized to the spool once, so nothing later needs to recompute this.
//
// The upstream value is BOUNDED before it is trusted. It becomes part of
// activity_id ("<session>:gateway:<id>"), which is an ungated structural field
// core stores and dedupes on, so an unbounded upstream-controlled string would
// reach that field verbatim. The provider is not an attacker in the threat model,
// but a compromised or merely broken upstream is exactly what a governance sensor
// should not forward blindly — and a fallback that is always available costs
// nothing.
func (e *Emitter) requestID(c gateway.Captured) string {
	if id := c.ResponseHeaders[upstreamRequestHeader]; usableRequestID(id) {
		return id
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A bare timestamp here would NOT be unique: two concurrent calls in the
		// same session can read the same clock tick, and the id feeds the
		// idempotency hash — so core would absorb the second as a duplicate and
		// that call's evidence would vanish with no error anywhere. A process-local
		// counter cannot collide with itself, which is the property actually
		// needed.
		return GatewayIDPrefix + "seq-" + strconv.FormatUint(atomic.AddUint64(&e.fallbackSeq, 1), 36) +
			"-" + strconv.FormatInt(e.now().UTC().UnixNano(), 36)
	}
	return GatewayIDPrefix + hex.EncodeToString(b[:])
}

func (e *Emitter) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Emitter) warn(format string, args ...any) {
	if e.Warn != nil {
		e.Warn(format, args...)
	}
}

// warnThrottled reports a recurring fault at most once per warnInterval.
func (e *Emitter) warnThrottled(last *time.Time, format string, args ...any) {
	e.mu.Lock()
	now := e.now()
	if !last.IsZero() && now.Sub(*last) < warnInterval {
		e.mu.Unlock()
		return
	}
	*last = now
	e.mu.Unlock()
	e.warn(format, args...)
}

// maxRequestIDLen bounds the upstream id. Generous next to a real one
// ("req_011CS..." is ~30 chars) and far below anything that could bloat the
// activity_id column.
const maxRequestIDLen = 128

// usableRequestID reports whether an upstream id may be used verbatim. Printable
// ASCII only: a control character or a newline in an id that becomes part of a
// stored key is a shape nobody downstream is expecting.
func usableRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for _, r := range id {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}
