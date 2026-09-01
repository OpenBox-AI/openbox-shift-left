package hookflow

import (
	"context"
	"log"
	"os"
	"os/exec"
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
		s.sweepOnce(logger)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (s Sweeper) sweepOnce(logger *log.Logger) {
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
