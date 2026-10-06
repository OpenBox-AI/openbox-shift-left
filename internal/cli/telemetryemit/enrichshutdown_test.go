package telemetryemit

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// A record arriving after the daemon's lifetime ended is delivered at once,
// metadata-only, and on a context that is not the dead one: the export
// request's context is cancelled by then, and a delivery handed it would be
// refused instead of sent.
func TestEnrichAfterShutdownDeliversOnALiveContext(t *testing.T) {
	life, stop := context.WithCancel(context.Background())
	stop()
	exportCtx, exportStop := context.WithCancel(context.Background())
	exportStop()

	var dead, delivered atomic.Int32
	e := &Emitter{
		Mapper: codexMapper(), DID: func() string { return testDID }, Lifetime: life,
		Deliver: func(ctx context.Context, _ client.DevEvent) bool {
			if ctx.Err() != nil {
				dead.Add(1)
				return false
			}
			delivered.Add(1)
			return true
		},
		Enrich: &Enricher{Outcome: "codex.content", Enrich: func(context.Context, Call) Result {
			t.Error("the enricher ran after shutdown")
			return Result{}
		}},
	}
	if err := e.Emit(exportCtx, codexSSE(nil)); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if dead.Load() != 0 || delivered.Load() != 2 {
		t.Errorf("delivered %d, refused on a dead context %d; want 2 and 0", delivered.Load(), dead.Load())
	}
}
