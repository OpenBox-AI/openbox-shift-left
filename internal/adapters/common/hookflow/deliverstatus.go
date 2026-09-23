package hookflow

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"time"
)

// DeliverStatus is what a lane daemon persists about its DeliverPool for
// `doctor` -- a separate process with no channel back into a running
// daemon's memory -- to read. No IPC, no port: a small JSON file under the
// OpenBox home the daemon already resolves for every other sink it owns
// (the spool, the advisory record, the CA), the same way those are read
// across a process boundary.
type DeliverStatus struct {
	// Dropped is DeliverPool.Dropped() at the last persisted moment: records
	// this lane could not accept (saturation) or could not finish delivering
	// before a shutdown drain gave up on them (Close's deadline), added
	// together, since Dropped() itself does not distinguish them.
	Dropped uint64 `json:"dropped"`
	// Since is when this persister -- and so this daemon's counting -- started.
	Since time.Time `json:"since"`
}

// DeliverStatusPath names one lane's status file under homeDir (the OpenBox
// home directory, devconfig.Home()). lane is a short, filesystem-safe name
// ("telemetry", "transport") so the two daemons never share one file.
func DeliverStatusPath(homeDir, lane string) string {
	return filepath.Join(homeDir, lane+"-delivery-status.json")
}

// StatusPersister rate-limits writing a DeliverPool's Dropped() count to
// disk. Report is cheap enough to call often (a ticker tick, every drop) but
// only actually writes when the value has changed AND at most once per
// interval; Flush always writes regardless of the interval, for the one
// call site that matters most: the final count once a daemon has drained its
// pool and dropped cannot change again for this process's life.
//
// A nil *StatusPersister and one built with an empty path both make every
// method a no-op: persistence here is best-effort disclosure for `doctor`,
// never load-bearing for governance, and a caller that could not resolve the
// OpenBox home degrades to no status file rather than an error.
type StatusPersister struct {
	path     string
	since    time.Time
	interval time.Duration
	// now is injectable so a test can control the rate limit deterministically
	// instead of racing real wall-clock sleeps; nil ⇒ time.Now.
	now func() time.Time

	mu        sync.Mutex
	lastSaved time.Time
	lastValue uint64
	wrote     bool
}

// NewStatusPersister builds a persister writing to path, timestamped from
// now. interval<=0 falls back to a sane default; an empty path disables
// persistence entirely.
func NewStatusPersister(path string, interval time.Duration) *StatusPersister {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &StatusPersister{path: path, since: time.Now(), interval: interval}
}

func (s *StatusPersister) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Report persists dropped when it differs from the last persisted value and
// at least interval has passed since the last write. Errors from the write
// itself are swallowed (see the type doc): a doctor row that is stale or
// absent is the correct failure shape for a status file that could not land,
// never a reason to disrupt delivery.
func (s *StatusPersister) Report(dropped uint64) {
	if s == nil || s.path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wrote && dropped == s.lastValue {
		return
	}
	now := s.clock()
	if s.wrote && now.Sub(s.lastSaved) < s.interval {
		return
	}
	s.commit(now, dropped)
}

// Flush unconditionally persists dropped, ignoring both the rate limit and
// the change check -- for the final count at shutdown, which is exactly the
// write a rate limit could otherwise eat.
func (s *StatusPersister) Flush(dropped uint64) {
	if s == nil || s.path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commit(s.clock(), dropped)
}

// commit assumes s.mu is held.
func (s *StatusPersister) commit(now time.Time, dropped uint64) {
	s.lastSaved = now
	s.lastValue = dropped
	s.wrote = true
	raw, err := json.Marshal(DeliverStatus{Dropped: dropped, Since: s.since})
	if err != nil {
		return
	}
	// atomicWriteFile, not internal/cli/atomicfile directly: adapter code may
	// depend on adapters/common, never on internal/cli (see
	// atomicwriteexport.go), and this file lives in that adapter-common
	// package.
	_ = atomicWriteFile(s.path, raw, 0o600)
}
