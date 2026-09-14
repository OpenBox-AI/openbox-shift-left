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

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
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

	// Elected stops two lanes describing the same turn. Resolved PER RECORD (see
	// electedFn); nil is a WIRING DEFECT, not a setting, and is reported as one.
	Elected func() bool

	// ElectionProblem reports why the election could not be RESOLVED, or "".
	ElectionProblem func() string

	// RunStore resolves this session's run identity (RunID/RunGeneration on
	// Identity), so this lane names the same run a hook event would (R7). The
	// zero value resolves to runs/ under the resolved DefaultSessionDir() --
	// what a production daemon gets from its unit's OPENBOX_SESSION_DIR
	// (insight 7, a daemon has no $HOME) -- and a test points Dir at a temp
	// directory so it never touches the developer's real registry.
	RunStore obgit.RunStore

	// ElectedName is WHICH lane the election named, "" when it named none.
	//
	// Elected() being false has two causes and one is healthy: another lane won,
	// and is emitting. If it named NOBODY -- what a settings rewrite leaves --
	// then no lane emits at all while this relay is holding the call. Without the
	// name those are indistinguishable, and the second was skipped quietly under a
	// log line asserting a producer that does not exist. Nil ⇒ unknown.
	ElectedName func() string

	mu                 sync.Mutex
	lastBadSessionWarn time.Time
	lastNoSessionWarn  time.Time
	// lastNoSessionOtherWarn throttles every class EXCEPT a completion separately, so
	// probe noise cannot silence the one case that is a real defect.
	lastNoSessionOtherWarn time.Time
	lastNoDIDWarn          time.Time
	lastNoElectionWarn     time.Time
	lastUndecidedWarn      time.Time
	lastNoRoutedLaneWarn   time.Time
	cachedDID              string
	fallbackSeq            uint64
	// probesSkipped counts token-count probes classified and deliberately not
	// spooled. It is the local audit fact that survives the dropped emission:
	// identifiers and counts only, reported through vlog, never egressed.
	probesSkipped uint64
}

func (e *Emitter) electionProblem() string {
	if e.ElectionProblem == nil {
		return ""
	}
	return e.ElectionProblem()
}

// electedName reports the lane the election named. The bool is whether the
// question could be ANSWERED, which is not the same as the answer being empty.
func (e *Emitter) electedName() (string, bool) {
	if e.ElectedName == nil {
		return "", false
	}
	return e.ElectedName(), true
}

func (e *Emitter) noSessionWarnClock(class PathClass) *time.Time {
	if class == ClassCompletion {
		return &e.lastNoSessionWarn
	}
	return &e.lastNoSessionOtherWarn
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
		e.vlog("  capture: DROPPED; this emitter has no lane configured")
		e.warn("openbox: a model-call emitter was constructed with no lane, so captured calls " +
			"cannot be attributed to a producer and are being DROPPED. This is a wiring defect, not a setting.")
		return
	}

	if e.Elected == nil {
		e.vlog("  capture: DROPPED; this emitter has no election gate configured")
		e.warnThrottled(&e.lastNoElectionWarn, "openbox: a model-call emitter was constructed with no election gate, "+
			"so it cannot know whether it is this machine's producer and captured calls are being DROPPED. "+
			"This is a wiring defect, not a setting.")
		return
	}
	if !e.Elected() {
		if problem := e.electionProblem(); problem != "" {
			e.vlog("  capture: DROPPED; the election cannot be resolved: %s", problem)
			e.warnThrottled(&e.lastUndecidedWarn, "openbox: %s. Until that is readable this lane "+
				"cannot know whether it is this machine's producer, so captured model calls are "+
				"being DROPPED -- which is NOT the same as another lane winning. Re-run `openbox "+
				"init` so the unit carries --settings, or pass --elected. The model calls "+
				"themselves are unaffected.", problem)
			return
		}
		if name, known := e.electedName(); known && name == "" {
			// Resolved cleanly, and named nobody, while this relay is holding a call:
			// a routing gap, not another lane's turn. The old code took the skip
			// below and logged another lane as the producer, the one thing that
			// cannot be true here.
			e.vlog("  capture: DROPPED; the election names no producer, yet this relay observed the call")
			e.warnThrottled(&e.lastNoRoutedLaneWarn, "openbox: this relay is observing model calls but "+
				"the tool's settings route NO lane, so no lane is this machine's elected producer and "+
				"captured model calls are being DROPPED by every lane. Something rewrote the settings "+
				"file after install; a running tool keeps the environment it started with, so this is "+
				"invisible from inside the session. `openbox doctor` names the missing keys and "+
				"`openbox init --provider claude-code` rewrites them. The model calls "+
				"themselves are unaffected.")
			return
		}
		e.vlog("  capture: SKIPPED; another lane is this machine's elected model-call producer")
		return
	}

	did := e.developerDID()
	if did == "" {
		e.vlog("  capture: SKIPPED; no developer DID configured (run `openbox init --provider claude-code`)")
		e.warnThrottled(&e.lastNoDIDWarn, "openbox gateway: no developer DID configured, so relayed model calls are NOT being recorded. Run `openbox init --provider claude-code`; no restart is needed.")
		return
	}

	sessionID := c.RequestHeaders[sessionHeader]
	if sessionID != "" && !usableSessionID(sessionID) {
		e.vlog("  capture: SKIPPED; %s is not a usable session id", sessionHeader)
		e.warnThrottled(&e.lastBadSessionWarn, "openbox gateway: relayed calls carry a %s header that is not a usable session id "+
			"(too long, or not printable ASCII), so nothing can be attributed to a session. The model calls themselves are unaffected.", sessionHeader)
		return
	}
	if sessionID == "" {
		e.vlog("  capture: SKIPPED; the call carries no %s header, so it cannot be attributed to a session", sessionHeader)
		// isModelCall stays permissive for the WARN decision; the class decides care.
		if class := classifyPath(c.HTTPURL); isModelCall(c) && class.WarnsOnMissingSession() {
			e.warnThrottled(e.noSessionWarnClock(class), "openbox gateway: no %s header on %s, so nothing can be attributed to a session and no governance event is being sent. "+
				"The model calls themselves are unaffected.", sessionHeader, class.Subject(c.HTTPURL))
		}
		return
	}

	if class := classifyPath(c.HTTPURL); !class.Emits() {
		n := atomic.AddUint64(&e.probesSkipped, 1)
		e.vlog("  capture: SKIPPED; %s is a token-count probe, which is classified but never emits a governance event (%d seen so far)",
			class.Subject(c.HTTPURL), n)
		return
	}

	runID, runGen := e.resolveRun(sessionID, c)
	id := Identity{
		SessionID:     sessionID,
		DeveloperDID:  did,
		AgentID:       usableAgentID(c.RequestHeaders[agentHeader]),
		RunID:         runID,
		RunGeneration: runGen,
	}
	requestID := e.requestID(c)
	events, err := EventsFor(e.Lane, id, requestID, e.now(), c)
	if err != nil {
		e.vlog("  capture: DROPPED; %v", err)
		e.warn("openbox: dropped a captured call: %v", err)
		return
	}
	// Order matters twice: the spool delivers in append order, so Started lands
	// first; and the loop STOPS on a failure, because appending Completed after
	// Started failed files the single-sided activity this pairing eliminates.
	appended := 0
	for _, ev := range events {
		if err := e.Spool.Append(ev); err != nil {
			e.vlog("  capture: DROPPED; %v", err)
			e.warn("openbox gateway: dropped event %s (%s) for activity %s: %v. %s",
				ev.EventID, ev.EventType, requestID, err, abandonNote(appended))
			break
		}
		appended++
	}
	if appended < len(events) {
		return
	}
	e.vlog("  capture: recorded session=%s activity=%s:%s:%s as %d event(s) in %s",
		sessionID, sessionID, e.Lane.Name, requestID, len(events), c.Elapsed().Round(time.Millisecond))
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

// resolveRun reads the shared run record ONCE per captured pair and applies
// R12: a call whose observed start precedes the record's own bump timestamp
// belongs to the run the bump sealed, not the one it opened. Read failure
// (absent/unreadable/corrupt/inconsistent record) fails open to generation 0
// (INV-3) -- this lane never blocks or drops a captured call over it.
func (e *Emitter) resolveRun(sessionID string, c gateway.Captured) (runID string, runGen int) {
	rec, err := e.RunStore.Read(sessionID)
	if err != nil || rec.Generation == 0 {
		return "", 0
	}
	started, _ := boundsOf(c, e.now())
	if started.UnixNano() < rec.UpdatedAt {
		return rec.PreviousRunID, rec.Generation - 1
	}
	return rec.RunID, rec.Generation
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

// abandonNote says what a failed append cost, which differs by which half failed.
func abandonNote(appended int) string {
	if appended == 0 {
		return "the whole activity is abandoned, so no half-record is stored"
	}
	return "its opening half is already spooled and will store UNPAIRED"
}
