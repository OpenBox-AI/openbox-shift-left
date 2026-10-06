package client

import (
	"fmt"
	"sync/atomic"
	"time"
)

// TokenAcquisition is one workload-token acquisition as the local trace
// sees it: whether the cache answered, how long it took, and why it failed.
// It never carries the token.
type TokenAcquisition struct {
	FromCache bool
	Duration  time.Duration
	Err       error
}

func (a TokenAcquisition) String() string {
	return fmt.Sprintf("from_cache=%v duration=%s err=%v", a.FromCache, a.Duration, a.Err)
}

// tokenObserver is process-wide rather than a Config field: every client in
// a process (each hook's, each lane daemon's per-tool one) reports to the
// same trace, and main is the one place that knows the trace exists. This
// package cannot import internal/trace itself -- it is imported by the
// guarded internal/gateway and internal/decision, which must not reach a
// file writer.
var tokenObserver atomic.Pointer[func(TokenAcquisition)]

// SetTokenObserver installs observe for every client in the process and
// returns a function restoring the previous one. nil stops observing.
func SetTokenObserver(observe func(TokenAcquisition)) (restore func()) {
	var next *func(TokenAcquisition)
	if observe != nil {
		next = &observe
	}
	prev := tokenObserver.Swap(next)
	return func() { tokenObserver.Store(prev) }
}

func observeToken(fromCache bool, start time.Time, err error) {
	if observe := tokenObserver.Load(); observe != nil {
		(*observe)(TokenAcquisition{FromCache: fromCache, Duration: time.Since(start), Err: err})
	}
}
