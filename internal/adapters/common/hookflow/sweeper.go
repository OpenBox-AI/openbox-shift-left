package hookflow

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const DefaultSweepInterval = 15 * time.Minute

const DefaultSweepDelay = 90 * time.Second

// Sweeper is the completeness net: one abandoned file accumulated 1,048 events
// over eighteen days. It spawns the flush subcommand rather than delivering,
// because a lane daemon holds no signing key.
type Sweeper struct {
	Spool    Spool
	Provider string
	// Self is the binary to spawn; empty ⇒ os.Executable(), never PATH.
	Self     string
	Interval time.Duration
	Delay    time.Duration
	Start    func(*exec.Cmd) error
}

func (s Sweeper) Run(ctx context.Context, logger *log.Logger) {
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	delay := s.Delay
	if delay == 0 {
		delay = DefaultSweepDelay
	}

	if delay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
	for {
		s.sweepOnce(logger, interval)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// sweepLockName is DOTTED on purpose: sanitizeSessionID emits only
// [A-Za-z0-9_-], so no real session's flush lock can ever collide with it, and
// none of the spool's file predicates (IsBacklogFile, IsRecoveryFile,
// RetireStale's `.jsonl` test) match it either.
const sweepLockName = ".sweep.flushlock"

// claimSweep is the machine-wide debounce: one sweep per window across every
// lane daemon sharing the spool directory. It reports whether this process won.
//
// Nothing releases the lock, unlike a session's: a sweep has no session end to
// release it at, so the next sweep takes it over once it has gone stale. The
// window is the sweep interval, which makes that exactly one sweep per interval.
func (s Sweeper) claimSweep(logger *log.Logger, window time.Duration) bool {
	if err := os.MkdirAll(s.Spool.Dir, 0o700); err != nil {
		logger.Printf("spool sweep: cannot reach the spool directory: %v", err)
		return false
	}
	lock := filepath.Join(s.Spool.Dir, sweepLockName)
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	switch {
	case err == nil:
		f.Close()
		return true
	case os.IsExist(err):
		info, statErr := os.Stat(lock)
		if statErr != nil {
			return false // vanished mid-race; the next interval retries
		}
		if time.Since(info.ModTime()) < window {
			return false // another lane's sweeper already owns this interval
		}
		now := time.Now()
		if chErr := os.Chtimes(lock, now, now); chErr != nil {
			logger.Printf("spool sweep: stale-lock takeover skipped: %v", chErr)
			return false
		}
		return true
	default:
		logger.Printf("spool sweep: lock claim skipped: %v", err)
		return false
	}
}

func (s Sweeper) sweepOnce(logger *log.Logger, interval time.Duration) {
	self := s.Self
	if self == "" {
		exe, err := os.Executable()
		if err != nil || exe == "" {
			logger.Printf("spool sweep: cannot resolve own binary: %v", err)
			return
		}
		// Under `go test` the executable is the test binary: a fork bomb, not a sweep.
		if strings.HasSuffix(strings.TrimSuffix(exe, ".exe"), ".test") {
			return
		}
		self = exe
	}

	pending := s.Spool.BacklogCount()
	if pending == 0 {
		return // an empty queue is the healthy steady state
	}

	// Debounced machine-wide, as Maybe debounces per session, and for a sharper
	// reason: all three lane daemons sweep the ONE directory on the same
	// un-jittered timers, so an ungated sweepOnce spawns two or three flushers
	// milliseconds apart. Two overlapping FlushAll walks are the precondition for
	// the orphan-reclaim double drain, so this is correctness, not courtesy.
	if !s.claimSweep(logger, interval) {
		return
	}

	cmd := exec.Command(self, "hook", s.Provider, "flush")
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	if lf := s.Spool.openFlusherLog(); lf != nil {
		defer lf.Close()
		cmd.Stdout, cmd.Stderr = lf, lf
	} else {
		cmd.Stdout, cmd.Stderr = nil, nil
	}
	cmd.SysProcAttr = detachAttr()

	start := s.Start
	if start == nil {
		start = (*exec.Cmd).Start
	}
	if err := start(cmd); err != nil {
		logger.Printf("spool sweep: spawn failed (%d event(s) stay spooled): %v", pending, err)
		return
	}
	logger.Printf("spool sweep: %d spooled event(s) had no session to trigger their delivery; "+
		"spawned a catch-up flush (see %s)", pending, s.Spool.FlusherLogPath())
	if cmd.Process != nil {
		go func() { _ = cmd.Wait() }()
	}
}
