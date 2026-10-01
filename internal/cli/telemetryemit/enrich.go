package telemetryemit

import (
	"context"
	"fmt"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// DefaultEnrichWait is how long, counted from the daemon receiving the record,
// an enrichment may take before the row ships without the part it is missing.
// It is measured from receipt and not from the tool's own timestamps: the
// tool's journal clock says nothing about when the daemon heard of the call.
const DefaultEnrichWait = 2 * time.Second

// DefaultMaxInFlight bounds the enrichments pending at once. Past it a record
// ships metadata-only at once, so a burst never holds a goroutine and a reader
// per record.
const DefaultMaxInFlight = 16

// Call names one model call whose bodies a provider may be able to supply.
type Call struct {
	// Session is the session id as the record named it, BEFORE the parent fold
	// (Turn.Session). A child's content is filed under the child.
	Session string
	// RequestID is the provider's id for the call, the same one that becomes
	// the activity id.
	RequestID string
	// At is the record's own time, and Tokens the pair's counts: what a provider
	// whose log carries no id for the call joins on. Neither is content.
	At     time.Time
	Tokens *client.Tokens
	// Deadline is receipt time + Enricher.Wait. The enricher polls until it or
	// ctx ends, then returns whatever it has.
	Deadline time.Time
}

// Miss records one body the enricher could not supply. Part is "request" or
// "response"; Reason is the provider's own vocabulary (stash_absent,
// log_absent, unverified, timeout, ...). It never carries content.
type Miss struct {
	Part   string
	Reason string
}

// Result is what an enricher found. Each half is independent: either may be
// empty while the other is set, and an empty one ships metadata-only. Bodies
// must already be redacted and bounded; the emitter attaches them as they are.
type Result struct {
	Request  string
	Response string
	Misses   []Miss
}

// Enricher is the provider-neutral enrichment seam. A provider supplies Enrich
// (and usually Enabled); the emitter owns the asynchrony, the concurrency
// bound, the deadline, ordering and the trace findings. A nil *Enricher keeps
// the emitter byte-identical to a lane without one.
type Enricher struct {
	// Outcome is the trace capture.outcome label findings are filed under,
	// e.g. "muse.content".
	Outcome string
	// Enabled, when non-nil, is consulted per record; false delivers the pair
	// at once and never calls Enrich. This is where a provider's content
	// posture goes, so a posture of off reads nothing.
	Enabled func() bool
	// Wait overrides DefaultEnrichWait; MaxInFlight overrides DefaultMaxInFlight.
	Wait        time.Duration
	MaxInFlight int
	// Enrich runs off the receiver's goroutine. ctx is the emitter's lifetime
	// context, cancelled at shutdown. It must return by Call.Deadline.
	Enrich func(ctx context.Context, c Call) Result
}

func (en *Enricher) active() bool {
	if en == nil || en.Enrich == nil {
		return false
	}
	return en.Enabled == nil || en.Enabled()
}

func (en *Enricher) wait() time.Duration {
	if en.Wait > 0 {
		return en.Wait
	}
	return DefaultEnrichWait
}

// Close stops accepting enrichments and waits for the pending ones. A record
// emitted afterwards is delivered at once, metadata-only, with reason shutdown.
// The flag and wg.Add share one lock, so an Emit can never Add while Close is
// already waiting. Idempotent.
func (e *Emitter) Close() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	e.wg.Wait()
}

// Wait blocks until every pending enrichment has delivered its row. For
// shutdown and tests.
func (e *Emitter) Wait() {
	if e != nil {
		e.wg.Wait()
	}
}

func (e *Emitter) lifetime() context.Context {
	if e.Lifetime != nil {
		return e.Lifetime
	}
	return context.Background()
}

// slots lazily builds the in-flight semaphore.
func (e *Emitter) slots() chan struct{} {
	e.semOnce.Do(func() {
		n := e.Enrich.MaxInFlight
		if n <= 0 {
			n = DefaultMaxInFlight
		}
		e.sem = make(chan struct{}, n)
	})
	return e.sem
}

// emitEnriched delivers a pair after asking the enricher for its bodies,
// without holding the receiver's goroutine. Started and Completed go out from
// the one goroutine, in order. Saturation and shutdown deliver at once.
func (e *Emitter) emitEnriched(ctx context.Context, t Turn, rec telemetry.Record) {
	call := Call{Session: t.Session, RequestID: t.Events[0].OtelRequestID, At: rec.Timestamp}
	for _, ev := range t.Events {
		if ev.EventType == client.EventTurnCompleted {
			call.Tokens = ev.Tokens
		}
	}
	call.Deadline = time.Now().Add(e.Enrich.wait())
	life := e.lifetime()

	e.mu.Lock()
	closed := e.closed
	if !closed {
		e.wg.Add(1)
	}
	e.mu.Unlock()
	if closed || life.Err() != nil {
		if !closed {
			e.wg.Done()
		}
		e.finding(t, Miss{Part: "both", Reason: "shutdown"})
		e.deliverPair(context.WithoutCancel(life), t.Events, rec)
		return
	}
	select {
	case e.slots() <- struct{}{}:
	default:
		e.wg.Done()
		e.finding(t, Miss{Part: "both", Reason: "enrich_saturated"})
		e.deliverPair(ctx, t.Events, rec)
		return
	}

	go func() {
		defer e.wg.Done()
		defer func() { <-e.sem }()
		res := e.runEnrich(life, call)
		for _, m := range res.Misses {
			e.finding(t, m)
		}
		for i := range t.Events {
			sp := t.Events[i].Span
			if sp == nil {
				continue
			}
			switch t.Events[i].EventType {
			case client.EventTurnStarted:
				sp.RequestBody = res.Request
			case client.EventTurnCompleted:
				sp.ResponseBody = res.Response
			}
		}
		// The request's context ended when the export call returned; the pair
		// must still be delivered, and a shutdown must not turn a finished
		// enrichment into a refused submission.
		e.deliverPair(context.WithoutCancel(life), t.Events, rec)
	}()
}

// runEnrich shields the daemon from a provider enricher's panic: the row ships
// metadata-only and says why.
func (e *Emitter) runEnrich(ctx context.Context, c Call) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			e.warnThrottled("openbox telemetry: the content enricher panicked (%v); the row ships metadata-only", r)
			res = Result{Misses: []Miss{{Part: "both", Reason: "enrich_panic"}}}
		}
	}()
	return e.Enrich.Enrich(ctx, c)
}

// finding files one content-free capture.outcome record.
func (e *Emitter) finding(t Turn, m Miss) {
	outcome := e.Enrich.Outcome
	if outcome == "" {
		outcome = "enrich.content"
	}
	detail := map[string]any{"part": m.Part, "reason": m.Reason, "request_id": t.Events[0].OtelRequestID}
	trace.Emit(trace.Record{
		Lane:      "telemetry",
		SessionID: t.Events[0].SessionID,
		Stage:     trace.StageCapture,
		Outcome:   outcome,
		Detail:    detail,
	})
	if e.Verbose != nil {
		e.Verbose("  telemetry: %s %s", outcome, fmt.Sprintf("%s=%s", m.Part, m.Reason))
	}
}
