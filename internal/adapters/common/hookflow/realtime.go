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

	self := t.Self
	if self == "" {
		exe, err := os.Executable()
		if err != nil || exe == "" {
			logger.Printf("realtime flush: cannot resolve own binary: %v", err)
			return
		}
		if strings.HasSuffix(strings.TrimSuffix(exe, ".exe"), ".test") {
			return
		}
		self = exe
	}

	window := t.Window
	if window <= 0 {
		window = DefaultRealtimeWindow
	}
	lock := t.Spool.FlushLockPath(sessionID)

	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	switch {
	case err == nil:
		f.Close()
	case os.IsExist(err):
		info, statErr := os.Stat(lock)
		if statErr != nil {
			return // lock vanished mid-race: a flusher just finished; next hook retries
		}
		if time.Since(info.ModTime()) < window {
			return // debounced: a flush is pending or running
		}
		now := time.Now()
		if chErr := os.Chtimes(lock, now, now); chErr != nil {
			logger.Printf("realtime flush: stale-lock takeover skipped: %v", chErr)
			return
		}
	default:
		logger.Printf("realtime flush: lock claim skipped: %v", err)
		return
	}

	cmd := exec.Command(self, "hook", t.Provider, "flush")
	cmd.Env = append(os.Environ(), EnvFlushSession+"="+sessionID)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = detachAttr()
	start := t.Start
	if start == nil {
		start = (*exec.Cmd).Start
	}
	if err := start(cmd); err != nil {
		logger.Printf("realtime flush: spawn failed (events wait for SessionEnd): %v", err)
		_ = os.Remove(lock)
		return
	}
	if cmd.Process != nil {
		go func() { _ = cmd.Wait() }()
	}
}
