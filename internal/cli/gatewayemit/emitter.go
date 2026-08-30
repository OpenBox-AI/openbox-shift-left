package gatewayemit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

const sessionHeader = "X-Claude-Code-Session-Id"

// agentHeader unlike the session header it is conditional; Claude Code emits
// it only when an agent context exists; so its absence is normal and must
// never be treated as a fault.
const agentHeader = "X-Claude-Code-Agent-Id"

const warnInterval = time.Hour

const upstreamRequestHeader = "Request-Id"

// Emitter turns each relayed model call into a spooled governance event. That
// has a second consequence worth stating: this process never touches the
// signing key or the obx_ credential; the flusher does.
type Emitter struct {
	// Lane names the producer this emitter speaks for. Required; there is no
	// default, deliberately.
	Lane Lane

	Spool hookflow.Spool

	// DID resolves the developer identity, and it is a function because this is a
	// long-lived daemon.
	DID func() string

	// Warn reports a dropped event.
	Warn func(format string, args ...any)

	// Flush nudges delivery for a session; nil disables the nudge, which is what
	// tests want (the real one spawns a detached process).
	Flush func(sessionID string)

	// Verbose reports the capture outcome of every call, unthrottled. Nil ⇒
	// silent. It prints identifiers and counts only; never a header, a body, or
	// the credential fingerprint.
	Verbose func(format string, args ...any)

	// Now is injectable so a test can pin a timestamp; nil ⇒ time.Now.
	Now func() time.Time

	mu                 sync.Mutex
	lastBadSessionWarn time.Time
	lastNoSessionWarn  time.Time
	lastNoDIDWarn      time.Time
	cachedDID          string
	fallbackSeq        uint64
}

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
func (e *Emitter) Emit(ctx context.Context, c gateway.Captured) {
	defer func() {
		if r := recover(); r != nil {
			e.warn("openbox gateway: dropped a captured call after a panic: %v", r)
		}
	}()

	// Latent today: WithGate has no production caller, so nothing is gated and
	// this path cannot fire.

	if !e.Lane.valid() {
		e.vlog("  capture: DROPPED — this emitter has no lane configured")
		e.warn("openbox: a model-call emitter was constructed with no lane, so captured calls " +
			"cannot be attributed to a producer and are being DROPPED. This is a wiring defect, not a setting.")
		return
	}

	did := e.developerDID()
	if did == "" {
		e.vlog("  capture: SKIPPED — no developer DID configured (run `openbox auth`)")
		e.warnThrottled(&e.lastNoDIDWarn, "openbox gateway: no developer DID configured, so relayed model calls are NOT being recorded. Run `openbox auth`; no restart is needed.")
		return
	}

	sessionID := c.RequestHeaders[sessionHeader]
	if sessionID != "" && !usableSessionID(sessionID) {
		e.vlog("  capture: SKIPPED — %s is not a usable session id", sessionHeader)
		e.warnThrottled(&e.lastBadSessionWarn, "openbox gateway: relayed calls carry a %s header that is not a usable session id "+
			"(too long, or not printable ASCII), so nothing can be attributed to a session. The model calls themselves are unaffected.", sessionHeader)
		return
	}
	if sessionID == "" {
		e.vlog("  capture: SKIPPED — the call carries no %s header, so it cannot be attributed to a session", sessionHeader)
		if isModelCall(c) {
			e.warnThrottled(&e.lastNoSessionWarn, "openbox gateway: relayed model calls carry no %s header, so nothing can be attributed to a session and no governance events are being sent. "+
				"The model calls themselves are unaffected.", sessionHeader)
		}
		return
	}

	id := Identity{
		SessionID:    sessionID,
		DeveloperDID: did,
		AgentID:      usableAgentID(c.RequestHeaders[agentHeader]),
	}
	ev, err := EventFor(e.Lane, id, e.requestID(c), e.now(), c)
	if err != nil {
		e.vlog("  capture: DROPPED — %v", err)
		e.warn("openbox: dropped a captured call: %v", err)
		return
	}
	if err := e.Spool.Append(ev); err != nil {
		e.vlog("  capture: DROPPED — %v", err)
		e.warn("openbox gateway: dropped event %s: %v", ev.EventID, err)
		return
	}
	e.vlog("  capture: recorded session=%s activity=%s:%s:%s", sessionID, sessionID, e.Lane.Name, requestIDOf(ev))
	if e.Flush != nil {
		e.Flush(sessionID)
	}
}

// isModelCall deliberately permissive in the other direction; any POST counts,
// including one to a path this code has never heard of; because the failure
// directions are not symmetric.
func isModelCall(c gateway.Captured) bool {
	return strings.EqualFold(c.HTTPMethod, http.MethodPost)
}

func (e *Emitter) requestID(c gateway.Captured) string {
	if id := c.ResponseHeaders[upstreamRequestHeader]; usableRequestID(id) {
		return id
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A process-local counter cannot collide with itself, which is the property
		// actually needed.
		return GatewayIDPrefix + "seq-" + strconv.FormatUint(atomic.AddUint64(&e.fallbackSeq, 1), 36) +
			"-" + strconv.FormatInt(e.now().UTC().UnixNano(), 36)
	}
	return e.Lane.IDPrefix + hex.EncodeToString(b[:])
}

func requestIDOf(ev client.DevEvent) string {
	switch {
	case ev.ProxyRequestID != "":
		return ev.ProxyRequestID
	case ev.GatewayRequestID != "":
		return ev.GatewayRequestID
	default:
		return ""
	}
}

func (e *Emitter) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Emitter) vlog(format string, args ...any) {
	if e.Verbose != nil {
		e.Verbose(format, args...)
	}
}

func (e *Emitter) warn(format string, args ...any) {
	if e.Warn != nil {
		e.Warn(format, args...)
	}
}

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

const maxRequestIDLen = 128

const maxSessionIDLen = 128

func usableSessionID(id string) bool {
	if !printableASCII(id, maxSessionIDLen) {
		return false
	}
	return !strings.ContainsAny(id, `/\`) && id != "." && id != ".."
}

func usableAgentID(id string) string {
	if printableASCII(id, maxRequestIDLen) {
		return id
	}
	return ""
}

func usableRequestID(id string) bool {
	return printableASCII(id, maxRequestIDLen)
}

func printableASCII(s string, n int) bool {
	if s == "" || len(s) > n {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}
