package hookflow

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// TestSpoolNeverLosesALineToAConcurrentDrain. Append writes through a
// descriptor it opened by path; drainFile renames that path aside, reads it and
// removes it. A write landing between the read and the remove goes into a file
// nothing reads again: never delivered, never written to a recovery file, and
// nowhere recorded. Append's old comment claimed O_APPEND made this safe -- true
// about interleaving, beside the point here. These lines are not torn, they are
// gone.
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
