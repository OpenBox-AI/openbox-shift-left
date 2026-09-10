package hookflow

import (
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// EnvFlushSession carries the session id from the realtime trigger to the
// spawned flusher process (`openbox hook <provider> flush`). Env rather than a
// positional arg so the flush subcommand's argv contract is unchanged; the
// value is a session id only; structural, never content (INV-2).
const EnvFlushSession = "OPENBOX_FLUSH_SESSION"

// DefaultRealtimeWindow is the debounce window: at most one flusher is spawned
// per session per window, and a burst of tool calls inside it is delivered by
// one drain. 2s keeps delivery near-real-time without a spawn per hook.
const DefaultRealtimeWindow = 2 * time.Second

// RealtimeTrigger spawns a short-lived detached flusher for a session after an
// event is spooled, so telemetry reaches core mid-session instead of waiting
// for SessionEnd (batch-at-end remains the completeness safety net).
type RealtimeTrigger struct {
	Spool    Spool
	Provider string // adapter name as the hook subcommand spells it, e.g. "claude-code"
	// Self is the binary to spawn; empty ⇒ os.Executable() (never a PATH lookup).
	Self string
	// Window is the debounce window; zero ⇒ DefaultRealtimeWindow.
	Window time.Duration
	// Enabled gates the trigger; nil ⇒ devconfig.ResolveRealtime.
	Enabled func() bool
	// Start launches the prepared command; nil ⇒ (*exec.Cmd).Start.
	Start func(*exec.Cmd) error
}

// Maybe spawns one detached flusher for sessionID unless a flush is already
// pending or running inside the debounce window. It never blocks, never writes
// stdout, and never returns an error to the hook path (INV-3): every fault is
// a logger line and a return.
func (t RealtimeTrigger) Maybe(logger *log.Logger, sessionID string) {
	enabled := t.Enabled
	if enabled == nil {
		enabled = devconfig.ResolveRealtime
	}
	if !enabled() || sessionID == "" {
		return
	}

	self, ok := selfBinary(logger, t.Self, "realtime flush")
	if !ok {
		return
	}

	window := t.Window
	if window <= 0 {
		window = DefaultRealtimeWindow
	}
	lock := t.Spool.FlushLockPath(sessionID)

	if !claimWindowLock(logger, lock, window, "realtime flush") {
		return
	}

	if err := t.Spool.spawnFlusher(self, t.Provider,
		append(os.Environ(), EnvFlushSession+"="+sessionID), t.Start); err != nil {
		logger.Printf("realtime flush: spawn failed (events wait for SessionEnd): %v", err)
		_ = os.Remove(lock)
	}
}

// selfBinary resolves the binary to spawn: the injected override, else
// os.Executable() (never a PATH lookup). It reports !ok when that cannot be
// resolved, or when it is the `go test` binary -- spawning that with hook args
// would re-run the suite recursively, since those tests reach this code again:
// a fork bomb, not a flusher. what prefixes the diagnostic.
func selfBinary(logger *log.Logger, override, what string) (string, bool) {
	if override != "" {
		return override, true
	}
	exe, err := os.Executable()
	if err != nil || exe == "" {
		logger.Printf("%s: cannot resolve own binary: %v", what, err)
		return "", false
	}
	if strings.HasSuffix(strings.TrimSuffix(exe, ".exe"), ".test") {
		return "", false
	}
	return exe, true
}

// claimWindowLock takes an mtime-debounced lockfile and reports whether this
// process owns the window. A lock that vanished mid-race, or one still inside
// the window, yields false; a stale one is taken over by refreshing its mtime.
// Nothing releases such a lock -- the next claimant takes it over once it has
// gone stale, which is what makes the window exactly one holder wide.
func claimWindowLock(logger *log.Logger, lock string, window time.Duration, what string) bool {
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	switch {
	case err == nil:
		f.Close()
		return true
	case os.IsExist(err):
		info, statErr := os.Stat(lock)
		if statErr != nil {
			return false
		}
		if time.Since(info.ModTime()) < window {
			return false
		}
		now := time.Now()
		if chErr := os.Chtimes(lock, now, now); chErr != nil {
			logger.Printf("%s: stale-lock takeover skipped: %v", what, chErr)
			return false
		}
		return true
	default:
		logger.Printf("%s: lock claim skipped: %v", what, err)
		return false
	}
}

// spawnFlusher starts the detached `hook <provider> flush` child. The flusher
// gets a voice: a file, not the parent's descriptors, because this child
// outlives the hook process and a hook's stdout injects what appears on it.
func (s Spool) spawnFlusher(self, provider string, env []string, start func(*exec.Cmd) error) error {
	cmd := exec.Command(self, "hook", provider, "flush")
	cmd.Env = env
	cmd.Stdin = nil
	if lf := s.openFlusherLog(); lf != nil {
		defer lf.Close()
		cmd.Stdout, cmd.Stderr = lf, lf
	} else {
		cmd.Stdout, cmd.Stderr = nil, nil
	}
	cmd.SysProcAttr = detachAttr()
	if start == nil {
		start = (*exec.Cmd).Start
	}
	if err := start(cmd); err != nil {
		return err
	}
	if cmd.Process != nil {
		// Reap the child, or a long-lived caller accumulates zombies until the
		// per-uid process limit is reached and nothing on the machine can fork.
		// Wait in a goroutine rather than Release: the child is already detached
		// and there are no pipes to drain, so this blocks only on its exit.
		go func() { _ = cmd.Wait() }()
	}
	return nil
}
