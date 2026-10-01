package telemetryemit

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// Emitter is the production caller the mapper was missing.
type Emitter struct {
	Mapper *Mapper

	// Deliver is how this emitter actually gets an event to core: the
	// telemetry daemon hands it a bounded pool's Submit, in-process, never a
	// spool write. Required; nil is a wiring
	// defect. It
	// returns false when the record was NOT accepted (a saturated pool),
	// which stops the pair's loop: Completed is never sent after a dropped
	// Started.
	Deliver func(ctx context.Context, ev client.DevEvent) bool

	// DID is a resolver, not a value. The hook path never had the problem because
	// every hook is a fresh process.
	DID func() string

	// Warn reports trouble.
	Warn    func(format string, args ...any)
	Verbose func(format string, args ...any)

	// Enrich, when set, lets a provider attach a model call's request and
	// response bodies to the pair before it ships (see Enricher). Nil leaves
	// the lane exactly as it was.
	Enrich *Enricher
	// Lifetime bounds the enrichments pending when the daemon stops. It is the
	// daemon's context, not the export request's, which ends before an
	// enrichment does. Nil means never cancelled.
	Lifetime context.Context

	mu      sync.Mutex
	semOnce sync.Once
	sem     chan struct{}
	wg      sync.WaitGroup
	// closed (under mu) refuses new enrichments: see Close.
	closed   bool
	drops    map[string]int
	emitted  int
	lastWarn time.Time
}

// dropWarnInterval unthrottled, the signal that something is wrong would
// itself be the thing that makes the log unreadable; and this daemon's stdio
// is the only place a silently-not-recording lane can be noticed at all.
const dropWarnInterval = 30 * time.Second

var _ telemetry.Emitter = (*Emitter)(nil)

// Emit maps one record and spools whatever it produced. It returns nil on
// every path, deliberately.
func (e *Emitter) Emit(ctx context.Context, rec telemetry.Record) error {
	if e == nil {
		return nil
	}
	turn, outcome := e.Mapper.TurnFor(rec)
	events := turn.Events
	if outcome != Emitted || len(events) == 0 {
		e.record(outcome, rec)
		e.traceOutcome(traceOutcomeName(outcome), reasonName(outcome), rec)
		return nil
	}

	did := events[0].DeveloperDID
	if e.DID != nil {
		did = e.DID()
	}
	if did == "" {
		e.record(dropNoDID, rec)
		e.traceOutcome("dropped", reasonName(dropNoDID), rec)
		return nil
	}

	if e.Deliver == nil {
		e.record(dropSpoolFailed, rec)
		e.warnThrottled("openbox telemetry: this emitter has no Deliver seam configured, so a model-call "+
			"turn (%s) for activity %s cannot reach core. This is a wiring defect, not a setting.",
			events[0].EventType, events[0].OtelRequestID)
		e.traceOutcome("dropped", "no_deliver_seam", rec)
		return nil
	}

	for i := range events {
		events[i].DeveloperDID = did
	}
	if e.Enrich.active() {
		e.emitEnriched(ctx, turn, rec)
		return nil
	}
	e.deliverPair(ctx, events, rec)
	return nil
}

// deliverPair submits the pair. Order matters twice: Started must be SUBMITTED
// first, and the loop STOPS when a submission is refused (the pool is
// saturated), because submitting Completed after Started was dropped files the
// single-sided activity this pairing eliminates. Delivery itself happens off
// this goroutine; accepted here means queued for one attempt, not confirmed on
// the wire.
func (e *Emitter) deliverPair(ctx context.Context, events []client.DevEvent, rec telemetry.Record) {
	accepted := 0
	for i := range events {
		if ok := e.Deliver(ctx, events[i]); !ok {
			e.record(dropSpoolFailed, rec)
			e.warnThrottled("openbox telemetry: dropped a model-call turn (%s) for activity %s: "+
				"the delivery pool is saturated. %s", events[i].EventType, events[i].OtelRequestID, abandonNote(accepted))
			e.traceOutcome("dropped", "delivery_pool_saturated", rec)
			break
		}
		accepted++
		e.mu.Lock()
		e.emitted++
		e.mu.Unlock()
	}
	if accepted < len(events) {
		return
	}

	if e.Verbose != nil {
		e.Verbose("  telemetry: recorded %s turn %s as %d event(s)",
			rec.EventName, events[0].OtelRequestID, len(events))
	}
	e.traceOutcome("recorded", "", rec)
}

// traceOutcomeName folds a Mapper Outcome into the three-way capture.outcome
// vocabulary trace.Read/openbox trace group on: SkipNotElected is the one
// healthy non-emission (another lane owns this session), IsDrop is a real
// loss, and everything else this switch does not name is unreachable today
// but still falls to "dropped" rather than silently miscounting as recorded.
func traceOutcomeName(o Outcome) string {
	switch {
	case o == Emitted:
		return "recorded"
	case o == SkipNotElected || o == SkipIncompleteCall:
		return "skipped"
	default:
		return "dropped"
	}
}

// traceOutcome is telemetryemit's own capture.outcome record -- the OTLP
// lane's twin of gatewayemit's, with the raw record body this receiver
// already has (it is raw at the receiver by construction: OTLP is not
// redacted client-side the way a relayed HTTP body is). Best-effort like
// every trace.Emit.
func (e *Emitter) traceOutcome(outcome, reason string, rec telemetry.Record) {
	detail := map[string]any{
		"signal":     string(rec.Signal),
		"event_name": rec.EventName,
		"attrs":      trace.JSON(rec.Attrs),
	}
	if reason != "" {
		detail["reason"] = reason
	}
	trace.Emit(trace.Record{
		Stage:   trace.StageCapture,
		Lane:    "telemetry",
		Outcome: outcome,
		Detail:  detail,
	})
}

const (
	dropNoDID       = Outcome(-1)
	dropSpoolFailed = Outcome(-2) // name kept for the counter key's stability; means "delivery refused"
)

func reasonName(o Outcome) string {
	switch o {
	case dropNoDID:
		return "no-developer-did"
	case dropSpoolFailed:
		return "delivery-refused"
	}
	return o.String()
}

// record skips are counted for the verbose view but never warned about: most
// records are legitimately uninteresting, and warning on them would train a
// reader to ignore the log.
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
		return fmt.Sprintf("%d event(s) recorded", emitted)
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
	return fmt.Sprintf("%d event(s) recorded, other outcomes: %s", emitted, strings.Join(parts, " "))
}

// abandonNote says what a failed append cost; see gatewayemit, which states the
// same reasoning rather than sharing an import for one string.
func abandonNote(appended int) string {
	if appended == 0 {
		return "the whole activity is abandoned, so no half-record is stored"
	}
	return "its opening half is already spooled and will store UNPAIRED"
}
