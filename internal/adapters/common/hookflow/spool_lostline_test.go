package hookflow

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// TestSpoolNeverLosesALineToAConcurrentDrain.
//
// Append opens the session path and writes through that descriptor. drainFile
// renames the same path aside, reads the renamed file, delivers what it found
// and removes it. Nothing sequences the two, so an Append whose open resolved
// before the rename and whose write landed after the read puts its line into a
// file that is then unlinked: the event is never delivered, never written to a
// recovery file, and nothing anywhere records that it existed. A tool call that
// happened has no evidence, and the audit trail under-reports in silence.
//
// This is the defect the spool lock is for. Append's comment used to claim
// instead that "a single write of a small line is atomic under O_APPEND
// (posix), so concurrent hook processes for the same session never interleave"
// -- true about interleaving, and beside the point: these lines are not torn,
// they are gone.
func TestSpoolNeverLosesALineToAConcurrentDrain(t *testing.T) {
	const (
		writers   = 8
		perWriter = 400
		appended  = writers * perWriter
	)

	dir := t.TempDir()
	s := Spool{Dir: dir}

	var delivered atomic.Int64
	count := FlushFunc(func(_ context.Context, _ client.DevEvent) error {
		delivered.Add(1)
		return nil
	})

	stop := make(chan struct{})
	var drainer sync.WaitGroup
	drainer.Add(1)
	go func() {
		defer drainer.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = s.FlushSession(context.Background(), "sess-1", count)
		}
	}()

	var writing sync.WaitGroup
	for w := range writers {
		writing.Add(1)
		go func() {
			defer writing.Done()
			for i := range perWriter {
				if err := s.Append(ev("sess-1", fmt.Sprintf("w%d-e%d", w, i))); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}
	writing.Wait()
	close(stop)
	drainer.Wait()

	// Whatever the racing drainer left behind, plus any recovery files.
	for {
		n, err := s.FlushSession(context.Background(), "sess-1", count)
		if err != nil {
			t.Fatalf("final drain: %v", err)
		}
		if n == 0 {
			break
		}
	}

	if got := delivered.Load(); got != appended {
		t.Errorf("delivered %d of %d appended events; %d lines were written into a file the drain had "+
			"already renamed aside and then removed, so they reached neither the control plane nor a "+
			"recovery file", got, appended, int64(appended)-got)
	}
}
