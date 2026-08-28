package telemetryemit

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/telemetry"
)

// Emitter is the production caller the mapper was missing.
//
// Until this existed, package telemetry tested its receiver against a stub
// Emitter and this package tested its mapper against hand-built records, with
// nothing joining them — the exact shape that let the gateway relay perfectly
// and discard every capture it made. The control test drives the real command
// into the real spool with no fake at either end.
//
// It writes into the Claude Code adapter's spool, under the session id the
// record carried, so the hook path's existing flushers deliver it. Same choice
// the gateway's emitter makes and for the same reason: no second delivery
// mechanism, and both producers' events for one session travel together.
type Emitter struct {
	Spool  hookflow.Spool
	Mapper *Mapper

	// DID is a RESOLVER, not a value. This daemon outlives the moment it starts,
	// and resolving once meant a daemon started before `openbox auth` finished
	// recorded nothing for its whole life — the ordinary order of operations, not
	// an edge case. The hook path never had the problem because every hook is a
	// fresh process. Learned by the gateway's emitter; not re-learned here.
	DID func() string

	// Warn reports trouble. Flush nudges the realtime delivery for a session.
	// Verbose, when set, reports the outcome of EVERY record.
	Warn    func(format string, args ...any)
	Flush   func(sessionID string)
	Verbose func(format string, args ...any)

	mu       sync.Mutex
	drops    map[string]int
	emitted  int
	lastWarn time.Time
}

// dropWarnInterval throttles the drop warning.
//
// A malformed export produces one drop PER RECORD, and Claude Code exports
// thousands per session. Unthrottled, the signal that something is wrong would
// itself be the thing that makes the log unreadable — and this daemon's stdio is
// the only place a silently-not-recording lane can be noticed at all.
const dropWarnInterval = 30 * time.Second

var _ telemetry.Emitter = (*Emitter)(nil)

// Emit maps one record and spools whatever it produced.
//
// It returns nil on every path, deliberately. The interface's own contract says
// an error is logged and dropped rather than returned to the exporter, because a
// rejected export is retried and eventually surfaced by the governed tool — so a
// failing sink would degrade the very session it exists to observe. This lane is
// additive by construction; OD4's silence-is-a-finding is the compensating
// control, and the counters below are what make that finding possible.
func (e *Emitter) Emit(_ context.Context, rec telemetry.Record) error {
	if e == nil {
		return nil
	}
	ev, outcome := e.Mapper.EventFor(rec)
	if outcome != Emitted {
		e.record(outcome, rec)
		return nil
	}

	if e.DID != nil {
		ev.DeveloperDID = e.DID()
	}
	if ev.DeveloperDID == "" {
		// No DID means nothing can be attributed or signed. Counted like a drop
		// because that is what it is, and named separately because the remedy is
		// completely different: run `openbox auth`, not investigate the export.
		e.record(dropNoDID, rec)
		return nil
	}

	if err := e.Spool.Append(ev); err != nil {
		e.record(dropSpoolFailed, rec)
		e.warnThrottled("openbox telemetry: cannot spool a model-call turn: %v", err)
		return nil
	}

	e.mu.Lock()
	e.emitted++
	e.mu.Unlock()

	if e.Verbose != nil {
		e.Verbose("  telemetry: recorded %s turn %s", rec.EventName, ev.OtelRequestID)
	}
	if e.Flush != nil {
		e.Flush(ev.SessionID)
	}
	return nil
}

// Two drop reasons that belong to the emitter rather than the mapper: the mapper
// knows nothing about credentials or the spool.
const (
	dropNoDID       = Outcome(-1)
	dropSpoolFailed = Outcome(-2)
)

func reasonName(o Outcome) string {
	switch o {
	case dropNoDID:
		return "no-developer-did"
	case dropSpoolFailed:
		return "spool-write-failed"
	}
	return o.String()
}

// record counts an outcome, and warns when records are being LOST.
//
// Skips are counted for the verbose view but never warned about: most records
// are legitimately uninteresting, and warning on them would train a reader to
// ignore the log. Drops warn, because a lane that goes quiet because every
// record now fails validation must not look identical to a quiet session — the
// distinction phase 10's report named as this daemon's inherited requirement.
func (e *Emitter) record(o Outcome, rec telemetry.Record) {
	name := reasonName(o)

	e.mu.Lock()
	if e.drops == nil {
		e.drops = map[string]int{}
	}
	e.drops[name]++
	n := e.drops[name]
	e.mu.Unlock()

	if e.Verbose != nil {
		e.Verbose("  telemetry: %s (%s)", name, rec.EventName)
	}
	if o.IsDrop() || o < 0 {
		e.warnThrottled("openbox telemetry: dropped a record (%s); %d so far. The lane is receiving but not recording.", name, n)
	}
}

func (e *Emitter) warnThrottled(format string, args ...any) {
	if e.Warn == nil {
		return
	}
	e.mu.Lock()
	if time.Since(e.lastWarn) < dropWarnInterval {
		e.mu.Unlock()
		return
	}
	e.lastWarn = time.Now()
	e.mu.Unlock()
	e.Warn(format, args...)
}

// Stats renders the counters for the doctor's recording line and for shutdown.
//
// "Reachable" and "recording" are different facts, and every gap this lane
// exists to expose lives between them — a receiver can accept every export and
// record none of it. This is the only place that difference is observable.
func (e *Emitter) Stats() (emitted int, drops map[string]int) {
	if e == nil {
		return 0, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]int, len(e.drops))
	for k, v := range e.drops {
		out[k] = v
	}
	return e.emitted, out
}

// String is the one-line summary for the daemon's log on shutdown.
func (e *Emitter) String() string {
	emitted, drops := e.Stats()
	if len(drops) == 0 {
		return fmt.Sprintf("%d turns recorded", emitted)
	}
	keys := make([]string, 0, len(drops))
	for k := range drops {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable output: this is read by humans comparing runs
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, drops[k]))
	}
	return fmt.Sprintf("%d turns recorded, other outcomes: %s", emitted, strings.Join(parts, " "))
}
