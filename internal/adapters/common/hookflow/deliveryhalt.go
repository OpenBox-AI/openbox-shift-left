package hookflow

import (
	"errors"
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// RecordDeliveryFailure is the one writer for "core did not accept this
// event", shared by every drainer (wired as the Engine's default
// Spool.OnFailure -- see NewEngine, which also records the caller's own
// discard-ledger line alongside this) and a gate's own escalation, for an
// explicit, proven non-acceptance (Evaluator.runWith): a content-free finding
// (INV-2), naming the event and the failure class (client.FailureClass), both
// logged and traced (trace.StageDeliveryFailed).
//
// It deliberately writes NO latch: the event that failed to deliver is
// denied on its own account (the caller's own fail-closed
// EvaluationFailOpen already does that), but the run itself is never
// stopped -- the next gated call, and the next hook event, get their own
// fresh attempt. Only a REAL HALT verdict from core (WriteSessionHalt,
// through Deliver) still stops a run.
//
// It never touches a spool's own discard ledger: a caller holding a spooled
// copy of ev records that separately (its own Spool.defaultOnFailure), since
// only it knows where its ledger lives; a gate's own escalation has nothing
// spooled to discard in the first place.
func RecordDeliveryFailure(logger *log.Logger, ev client.DevEvent, err error) {
	class := client.FailureClass(err)
	if errors.Is(err, errOrphanedDrain) {
		class = "orphaned"
	}
	if logger != nil {
		logger.Printf("openbox: %s for %s not accepted by core (%s); the run continues, only this event is lost",
			ev.EventType, ev.SessionID, class)
	}
	trace.Emit(trace.Record{
		SessionID: ev.SessionID,
		RunID:     ev.RunID,
		EventID:   ev.EventID,
		EventType: string(ev.EventType),
		Stage:     trace.StageDeliveryFailed,
		Outcome:   "not_accepted",
		ErrClass:  class,
		Detail: map[string]any{
			"class":      class,
			"event_type": string(ev.EventType),
		},
	})
}
