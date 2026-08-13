package hookflow

import (
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
)

// EnvFlushSession carries the session id from the realtime trigger to the
// spawned flusher process (`openbox hook <provider> flush`). Env rather than a
// positional arg so the flush subcommand's argv contract is unchanged; the
// value is a session id only — structural, never content (INV-2).
const EnvFlushSession = "OPENBOX_FLUSH_SESSION"

// DefaultRealtimeWindow is the debounce window: at most one flusher is spawned
// per session per window, and a burst of tool calls inside it is delivered by
// one drain. 2s keeps delivery near-real-time without a spawn per hook.
const DefaultRealtimeWindow = 2 * time.Second

// RealtimeTrigger spawns a short-lived detached flusher for a session after an
// event is spooled, so telemetry reaches core mid-session instead of waiting
// for SessionEnd (batch-at-end remains the completeness safety net).
//
// The hot path stays INV-3 clean: Maybe performs no network I/O — its worst
// case is one lockfile stat/create plus, at most once per Window, a Start of
// our own binary. Every fault is logged and swallowed; events then simply wait
// for SessionEnd exactly as before the trigger existed.
//
// Debounce is a per-session lockfile in the spool dir (FlushLockPath): hooks
// are separate short-lived processes, so the filesystem is the only shared
// state. The spawned flusher refreshes the lock's mtime when its drain starts
// and removes it when the drain finishes (TouchFlushLock/ReleaseFlushLock), so
// the window covers the spawn plus the drain's first Window. A drain slower
// than that (a slow network, a long backlog) looks stale to a later hook,
// which takes the lock over and spawns a second flusher alongside it —
// wasteful under exactly the conditions you would rather not add work, but
// never incorrect (see the delivery guarantees below). The same staleness is
// what recovers a lock whose flusher was killed mid-drain. Tail caveat: an
// event spooled in the instant between a flusher's final drain and its lock
// release waits for the next hook (or SessionEnd) — bounded staleness, never
// loss.
//
// Redundant flushes are harmless because spool rotation is an atomic rename: a
// losing drain claims nothing and delivers zero events, so two flushers can
// never both send one spooled event.
//
// That rename is the WHOLE guarantee, and it only covers events that go through
// the spool. Server-side dedupe on the Idempotency-Key is NOT a second line of
// defence: core does not dedupe developer events on their id today (E8-S7 is the
// client half only — a stable, unique id so that eventual dedupe is trivially
// correct). Anything that reaches core outside the spool therefore has to avoid
// duplicating itself on its own. The inline evaluation is exactly that case, and
// assuming a dedupe that did not exist is how it came to store every escalated
// ActivityStarted twice — see EnforceGate.SpoolObserve.
type RealtimeTrigger struct {
	Spool    Spool
	Provider string // adapter name as the hook subcommand spells it, e.g. "claude-code"
	// Self is the binary to spawn; empty ⇒ os.Executable() (never a PATH lookup).
	Self string
	// Window is the debounce window; zero ⇒ DefaultRealtimeWindow.
	Window time.Duration
	// Enabled gates the trigger; nil ⇒ devconfig.ResolveRealtime. When it
	// reports false, Maybe returns before any filesystem I/O — byte-identical
	// to the pre-realtime behavior.
	Enabled func() bool
	// Start launches the prepared command; nil ⇒ (*exec.Cmd).Start. Injectable
	// so unit tests can observe the spawn without executing anything.
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

	// Resolve the binary to spawn BEFORE touching the filesystem, so a run
	// that cannot spawn anyway (no resolvable self, or a test binary) leaves
	// the spool dir byte-identical to pre-realtime behavior.
	self := t.Self
	if self == "" {
		exe, err := os.Executable()
		if err != nil || exe == "" {
			logger.Printf("realtime flush: cannot resolve own binary: %v", err)
			return
		}
		// The trigger may only ever spawn the openbox engine. Under `go test`
		// the executable is the TEST binary (`*.test`), and spawning it with
		// hook args would re-run the suite — recursively, since those tests
		// reach this code again (a fork bomb, not a flusher). Tests that
		// exercise the spawn itself inject Self explicitly.
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

	// Claim the debounce lock. O_EXCL create is the atomic happy path; on
	// EEXIST the mtime decides between "flush in flight / just ran" (skip)
	// and "stale claim from a dead spawner" (take over by refreshing the
	// mtime). The takeover is deliberately best-effort rather than atomic:
	// two racing hooks can both take over a stale lock and spawn twice, which
	// costs one redundant drain and can never double-count (see type doc).
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
		// Spool dir may not exist yet (Maybe runs after Append, which creates
		// it — but stay defensive) or the create failed some other way. Log
		// and fall back to SessionEnd delivery.
		logger.Printf("realtime flush: lock claim skipped: %v", err)
		return
	}

	cmd := exec.Command(self, "hook", t.Provider, "flush")
	cmd.Env = append(os.Environ(), EnvFlushSession+"="+sessionID)
	// The flusher is fire-and-forget: no pipes back to the hook process, so
	// the hook can exit immediately and the provider never waits on the child.
	// Its stderr diagnostics are discarded — acceptable, because the flush
	// path is already fail-open and SessionEnd re-drains whatever it missed.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = detachAttr()
	start := t.Start
	if start == nil {
		start = (*exec.Cmd).Start
	}
	if err := start(cmd); err != nil {
		logger.Printf("realtime flush: spawn failed (events wait for SessionEnd): %v", err)
		// Leave no claim behind: with the lock held, every later hook in this
		// window would skip too, and if spawning is persistently broken the
		// stale takeover would keep failing the same way.
		_ = os.Remove(lock)
		return
	}
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
}
