package hookflow

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// HaltOnDeliveryFailure is the one writer for "core did not accept this
// event", shared by every drainer (wired as the Engine's default
// Spool.OnFailure -- see NewEngine, which also records the caller's own
// discard-ledger line alongside this) and a gate's own escalation, for an
// explicit, proven non-acceptance (Evaluator.run): a write-if-absent halt
// latch on the event's own run (runIDFor), naming the event and the failure
// class (client.FailureClass), content-free (INV-2). Every later gated call,
// prompt and relayed model call of that run is refused with this reason until
// a new session starts.
//
// Write-if-absent (WriteSessionHaltIfAbsent): an existing latch, whichever
// cause reached it first -- a live HALT verdict, or an earlier delivery
// failure -- is left exactly as it is. A crash-orphan reclaim (a drainer that
// died mid-delivery, its outcome unprovable) reaches this the same way,
// classified "orphaned" rather than misreported as a network fault.
//
// It never touches a spool's own discard ledger: a caller holding a spooled
// copy of ev records that separately (its own Spool.defaultOnFailure), since
// only it knows where its ledger lives; a gate's own escalation has nothing
// spooled to discard in the first place.
func HaltOnDeliveryFailure(logger *log.Logger, ev client.DevEvent, err error) {
	class := client.FailureClass(err)
	if errors.Is(err, errOrphanedDrain) {
		class = "orphaned"
	}
	reason := fmt.Sprintf(
		"OpenBox could not record %s for this session (%s); the run is halted so nothing it does goes unrecorded. Start a new session to continue.",
		ev.EventType, class,
	)
	WriteSessionHaltIfAbsent(logger, runIDFor(ev), SessionHaltInfo{
		Reason:    reason,
		Cause:     class,
		EventType: string(ev.EventType),
		TS:        time.Now().UTC().Format(time.RFC3339Nano),
	})
}
