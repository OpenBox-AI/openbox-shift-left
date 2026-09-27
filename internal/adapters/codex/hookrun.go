package codex

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"time"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// flushBudget bounds the detached `flush` subcommand: the realtime trigger's
// spawned child, the periodic sweeper, and `openbox uninstall`'s own flush
// all inherit it. 60s so a pass can start DeliveryAttemptTimeout-bounded
// (30s) attempts with slack; SessionEnd itself no longer drains inline
// against this budget -- see sessionEndInlineWindow.
const flushBudget = 60 * time.Second

// sessionStartInlineWindow bounds SessionStart's own inline delivery attempt:
// Codex's SessionStart timeout is hotHookTimeoutSec (5s); this leaves 1s of
// slack for process teardown, and is comfortably under the window so more
// than one queued line can still be attempted if a backlog is waiting.
const sessionStartInlineWindow = time.Duration(hotHookTimeoutSec-1) * time.Second

// sessionStartAttemptTimeout bounds each individual attempt inside
// sessionStartInlineWindow, strictly less than it: DrainSession never starts
// an attempt it cannot finish, so an attemptTimeout equal to the whole window
// would let even a fast attempt's few milliseconds of real elapsed time push
// the NEXT line's remaining budget below the bound and defer it needlessly.
const sessionStartAttemptTimeout = sessionStartInlineWindow - time.Second

// sessionEndInlineWindow bounds SessionEnd's own inline delivery attempt.
// Codex clamps SessionEnd's own hook timeout to 3s and logs that it did
// ("clamping SessionEnd hook timeout to 3s in <hooks.json>", measured on
// codex-cli 0.150.0-alpha.8; phase 00 probe P0.4); 2s leaves roughly a second
// for process start, ResolveCredentials and client.New.
const sessionEndInlineWindow = 2 * time.Second

// sessionEndAttemptTimeout is sessionEndInlineWindow's per-attempt bound, for
// the same reason sessionStartAttemptTimeout is not equal to its window.
const sessionEndAttemptTimeout = time.Second

// RunHook executes the path for one Codex hook invocation; the engine behind
// the unified `openbox hook codex <event>` subcommand.
//   - In observe mode (the default) it writes nothing to stdout.
//   - It never returns a blocking signal in observe mode: any failure (bad
//     payload, missing identity, unreachable OpenBox, even a panic) is logged
//     and swallowed.
func RunHook(sub string, stdin io.Reader, stdout io.Writer, logger *log.Logger) {
	hookStart := time.Now()
	defer devconfig.Pin()()
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("recovered: %v", r)
		}
	}()

	if sub == "" {
		logger.Printf("usage: openbox hook codex <HookName|flush>")
		return
	}
	if sub == "flush" {
		runFlush(logger, os.Getenv(hookflow.EnvFlushSession))
		return
	}

	hook, err := ParseHookName(sub)
	if err != nil {
		logger.Printf("%v", err)
		return
	}

	id, err := ResolveIdentity()
	if err != nil {
		logger.Printf("no identity, dropping %s event: %v", hook, err)
		return
	}

	// Read the whole payload BEFORE it is parsed, bounded the same way
	// ParseHookEvent's own decoder is (maxHookPayload): the trace records raw
	// stdin even when parsing fails, and a stream decoder consumed on a
	// failed parse cannot be replayed to build that record afterward.
	rawStdin, _ := io.ReadAll(io.LimitReader(stdin, maxHookPayload))
	ev, err := ParseHookEvent(bytes.NewReader(rawStdin))
	sessionID := ""
	if ev != nil {
		sessionID = ev.SessionID
	}
	hookflow.TraceHookIn(provider, string(hook), sessionID, rawStdin, err)
	if err != nil {
		logger.Printf("dropping %s event: %v", hook, err)
		return
	}

	var hookOut hookflow.HookOutputBuffer
	stdout = io.MultiWriter(stdout, &hookOut)
	defer func() {
		hookflow.TraceHookOut(provider, string(hook), ev.SessionID, hookStart, &hookOut)
	}()

	ad := New(id, DefaultSpoolDir())
	ad.Mapper.CaptureContent = ResolveContentCapture()
	if ResolveSecretDetection() {
		redactor := decision.NewRedactor()
		ad.Mapper.RedactContent = func(s string) string { return hookflow.RedactText(redactor, s) }
	}
	ad.Mapper.ThreadID = os.Getenv(obgit.EnvCodexThreadID)
	pinnedNow := time.Now()
	ad.Mapper.Now = func() time.Time { return pinnedNow }

	// This is the only place transcript_path is opened, and only when
	// ResolveFinops() is set; with finops off it is never dereferenced.
	if hook == HookSessionEnd && ResolveFinops() {
		if tokens, model, err := readRolloutUsage(ev.TranscriptPath); err != nil {
			logger.Printf("finops: rollout usage skipped: %v", err)
		} else if tokens != nil {
			ad.Mapper.Finops = &FinopsUsage{Tokens: tokens, Model: model}
		}
	}

	// Read the turn cursor BEFORE anything spools a SessionEnded event: the
	// engine's own SessionEnded handler calls Turns.ClearSession, so by the time
	// the rollup decision is made below the cursor is already gone. Capturing it
	// here is the difference between "this session emitted turns" and "the cursor
	// happens to be empty right now".
	sessionHadTurns := hook == HookSessionEnd && turnsEmitted(ad, ev.SessionID)

	if hook == HookSessionEnd {
		ad.Mapper.Evidence = &EvidenceState{
			Undelivered: ad.Spool.UndeliveredCountFor(ev.SessionID),
			Discarded:   ad.Spool.DiscardedCount(),
		}
	}

	if hook == HookSessionStart {
		posture := effectivePosture()
		ad.Mapper.Posture = &posture
	}

	nudgeFlush := func() {
		hookflow.RealtimeTrigger{Spool: ad.Spool, Provider: provider}.Maybe(logger, ev.SessionID)
	}

	// Stop and SubagentStop are turn boundaries, not signals and not gated calls,
	// so they short-circuit before the gate. Stop output is ALWAYS empty here:
	// `decision:"block"` on Stop injects a continuation prompt built from our
	// reason text, which would be OpenBox driving the agent rather than governing
	// it. That is a non-goal, permanently.
	if hook == HookStop || hook == HookSubagentStop {
		if ResolveFinops() {
			emitTurn(ad, logger, hook, ev)
			nudgeFlush()
		}
		return
	}

	// The two must agree: deciding to defer the observe copy here and then not
	// running the gate would drop the event.
	//
	// Enforcement is always on now (ResolveEnforce always reports true): every
	// gated hook always runs the gate below and reads the latch. There is no
	// longer an observe-only branch for these three hook classes.
	//
	// hook.Gated() (installer.go) is the one source of truth for this set,
	// shared with the installer's own raised-ceiling install: an independent
	// boolean expression here could drift from what got installed.
	gated := hook.Gated()

	if gated {
		// The latch is keyed by session id because the gate below sets no RunID,
		// so EnforceGate.runID falls through to t.SessionID(). Reader and writer
		// must move together: adding a RunID to the gate literal without changing
		// this call would write the latch under one key and read it under another,
		// silently un-halting a terminated session.
		//
		// onHalted renders a halted run's answer to this gated call: the same
		// steps hookflow.ReplaySessionHalt takes (no server round-trip; the
		// latch is the decided state), reimplemented here only because that
		// helper does not report back the ApplyResult the gate's own
		// post-drain latch re-read needs to return. Used both for the
		// PRE-gate check right below (a run already halted before this call
		// even started) and wired onto the gate itself (EnforceGate.OnHalted)
		// for a halt this call's OWN drain step discovers -- one
		// implementation of "how a halted run answers," not two.
		onHalted := func(info hookflow.SessionHaltInfo) hookflow.ApplyResult {
			if _, err := ad.Observe(hook, ev); err != nil {
				logger.Printf("spool %s event: %v", hook, err)
			}
			nudgeFlush()
			c, toolName, toolKind := haltReplayTriple(hook, ev)
			dec, res := hookflow.SessionHaltReplay(logger, stdout, info, false, nil, toolName, c)
			hookflow.RecordEnforcement(logger, ev.SessionID, toolKind, dec, res)
			return res
		}

		if info, halted := hookflow.SessionHalted(ev.SessionID); halted {
			onHalted(info)
			return
		}

		var spoolObserve, spoolObserveHead func()
		if devEv, ok := ad.Mapper.Map(hook, ev); ok {
			// Threaded once here (not through RecordDeferred): whichever of
			// the two closures the gate actually invokes needs the SAME
			// already-threaded event, and threading it twice would double-
			// put its duration-pairing entry.
			ad.ThreadDuration(&devEv)
			spoolObserve = func() {
				if err := ad.Spool.Append(devEv); err != nil {
					logger.Printf("spool %s event: %v", hook, err)
				}
				nudgeFlush()
			}
			spoolObserveHead = func() {
				if err := ad.Spool.SpoolObserveHead(devEv); err != nil {
					logger.Printf("spool %s event to the session head: %v", hook, err)
				}
				nudgeFlush()
			}
		}

		var (
			c      hookflow.OutputContract = promptContract
			target hookflow.EnforceTarget  = promptTarget{id: id, mapper: ad.Mapper, ev: ev}
			record                         = recordPromptEnforcement
		)
		switch hook {
		case HookPreToolUse:
			c, target, record = contract, enforceTarget{id: id, mapper: ad.Mapper, ev: ev}, recordEnforcement
		case HookPermissionRequest:
			c, target, record = permissionContract, permissionTarget{id: id, mapper: ad.Mapper, ev: ev}, recordPermissionEnforcement
		}
		g := hookflow.EnforceGate{
			Contract:         c,
			Evaluator:        evaluator,
			Record:           func(dec decision.Decision, res hookflow.ApplyResult) { record(logger, ev, dec, res) },
			SpoolObserve:     spoolObserve,
			SpoolObserveHead: spoolObserveHead,
			// RunID deliberately unset: see the latch-key note above.
			Queue:      ad.Engine,
			OnHalted:   onHalted,
			ForceFlush: func() { forceFlusher(logger, ev.SessionID) },
		}
		res := g.Run(context.Background(), logger, stdout, target)
		// A gate that already wrote owns stdout; appending findings after it
		// would put a second JSON document on the same stream.
		if hook == HookUserPromptSubmit && !res.Emitted && ResolveFindings() {
			hookflow.SurfaceFindings(provider, string(hook), stdout, logger)
		}
		return
	}

	// Every hook class that reaches here is never gated (Stop/SubagentStop
	// already returned above, and PreToolUse/UserPromptSubmit/PermissionRequest
	// always take the branch above): this is the observe-only path,
	// byte-identical to today regardless of enforcement.
	if _, err := ad.Observe(hook, ev); err != nil {
		logger.Printf("spool %s event: %v", hook, err)
	}
	switch hook {
	case HookSessionStart:
		// The session's own WorkflowStarted event is drained inline, under
		// this session's own stripe, right after the append above -- so no
		// detached flusher's debounce window can let anything else of this
		// run overtake it.
		inlineAttempt(ad, logger, ev.SessionID, hookStart, sessionStartInlineWindow, sessionStartAttemptTimeout)
	case HookSessionEnd:
		// Handled after the finops rollup below, not nudged here.
	default:
		nudgeFlush()
	}

	// Never a blocking field (INV-3); categories/counts only (INV-2); PostToolUse
	// is stat-guarded.
	if hook == HookPostToolUse || hook == HookUserPromptSubmit {
		if ResolveFindings() {
			hookflow.SurfaceFindings(provider, string(hook), stdout, logger)
		}
	}

	if hook == HookSessionStart && ResolveInstallGitHook() {
		obgit.InstallAmbient(ev.Cwd, logger.Printf)
	}

	if hook == HookSessionEnd {
		// Owner ruling (2026-09-17): the session rollup fires ONLY when no turn
		// pair was emitted for this session. Per-turn is the primary record; the
		// rollup survives for sessions where Stop never fired at all -- a crash, a
		// kill, or a surface that does not run the hook. Those are not rare (the
		// 3 s SessionEnd ceiling and the vendor's idle rule both produce them), so
		// dropping the rollup outright would lose real data, while keeping both
		// unconditionally would ship the same tokens twice under two activity ids.
		if sessionHadTurns {
			logger.Printf("finops: per-turn pairs were emitted; skipping the session rollup to avoid double-counting")
		} else if started, completed, ok := ad.Mapper.MapUsageRollup(ev); ok {
			for _, usageEv := range []client.DevEvent{started, completed} {
				if err := ad.Record(usageEv); err != nil {
					logger.Printf("finops: spool %s event: %v", usageEv.EventType, err)
					break
				}
			}
		}
	}

	if hook == HookSessionEnd {
		// The same inline attempt, within SessionEnd's own (smaller) budget,
		// falling back to the detached flusher when the window runs out.
		inlineAttempt(ad, logger, ev.SessionID, hookStart, sessionEndInlineWindow, sessionEndAttemptTimeout)
	}
}

// inlineAttempt drives one single-attempt, own-session drain within window
// (measured from hookStart -- the hook's own process start, not from here --
// so credential resolution and everything RunHook already did are counted
// against the SAME budget the hook's own vendor timeout is), so a
// SessionStart or SessionEnd hook's own newest event does not wait on the
// detached flusher's debounce window to reach core. It reuses the caller's
// own Adapter (its spool, its advisory sink) rather than building a second
// one, and owns the stripe lock for its own session (hookflow.Block): the
// append this hook just made is drained under the SAME lock, so nothing can
// overtake it. An attempt whose own ctx expires before core answers is
// requeued, never scored as a failure (RequeueUnanswered): the short inline
// window proves nothing about whether core would have accepted the event.
// Whatever the window could not even start, or could not get an answer for,
// falls back to the detached flusher -- not a failure, and a re-send is
// deduped by core's idempotency key.
func inlineAttempt(ad *Adapter, logger *log.Logger, sessionID string, hookStart time.Time, window, attemptTimeout time.Duration) {
	creds, err := ResolveCredentials()
	if err != nil {
		logger.Printf("flush skipped (events remain spooled): %v", err)
		forceFlusher(logger, sessionID)
		return
	}
	cl, err := creds.NewClient(logger)
	if err != nil {
		logger.Printf("flush skipped (client init): %v", err)
		forceFlusher(logger, sessionID)
		return
	}

	ad.Log = logger.Printf
	ad.Advisory.Log = logger

	ctx, cancel := context.WithDeadline(context.Background(), hookStart.Add(window))
	defer cancel()
	opts := hookflow.DrainOptions{Mode: hookflow.Block, AttemptTimeout: attemptTimeout, RequeueUnanswered: true}
	if _, err := ad.DrainSession(ctx, sessionID, cl, opts); err != nil {
		logger.Printf("inline delivery ended early: %v", err)
	}
	if ad.Spool.PendingCount(sessionID) > 0 {
		forceFlusher(logger, sessionID)
	}
}

// forceFlusher spawns the detached flusher regardless of the realtime_flush
// toggle: once an inline attempt could not finish, waiting out the ordinary
// debounce toggle is not the fallback's job. Reuses RealtimeTrigger.Maybe's
// own spawn/debounce plumbing rather than a second copy of it.
func forceFlusher(logger *log.Logger, sessionID string) {
	hookflow.RealtimeTrigger{
		Spool:    hookflow.Spool{Dir: DefaultSpoolDir()},
		Provider: provider,
		Enabled:  func() bool { return true },
	}.Maybe(logger, sessionID)
}

func runFlush(logger *log.Logger, sessionID string) {
	creds, err := ResolveCredentials()
	if err != nil {
		logger.Printf("flush skipped (events remain spooled): %v", err)
		return
	}
	cl, err := creds.NewClient(logger)
	if err != nil {
		logger.Printf("flush skipped (client init): %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), flushBudget)
	defer cancel()

	ad := New(creds.Identity(), DefaultSpoolDir())
	// The engine's own voice, on the same stderr the flusher's log now captures:
	ad.Log = logger.Printf
	// Diagnostics only; stderr, never stdout.
	ad.Advisory.Log = logger
	// One engine call: the lock, the drain loop and the retire ordering are its.
	n, err := ad.FlushOrSweep(ctx, sessionID, cl)
	if err != nil {
		logger.Printf("flush ended early after %d event(s): %v", n, err)
	}
}
