package codex

import (
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
// codex-cli 0.150.0-alpha.8); 2s leaves roughly a second
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

	if hookflow.RunSubcommand(logger, provider, DefaultSpoolDir(), sub) {
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

	ev, err := hookflow.ReadHookEvent(provider, string(hook), stdin, func(e *HookEvent) string { return e.SessionID })
	if err != nil {
		logger.Printf("dropping %s event: %v", hook, err)
		return
	}

	stdout, traceOut := hookflow.TraceHookOutput(provider, string(hook), stdout, hookStart, func() string { return ev.SessionID })
	defer traceOut()

	ad := New(id, DefaultSpoolDir())
	ad.Mapper.CaptureContent = ResolveContentCapture()
	ad.Mapper.RedactContent = hookflow.ContentRedactor(ResolveSecretDetection())
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
		// onHalted renders a halted run's answer to this gated call
		// (hookflow.Adapter.HaltReplayer): no server round trip, the latch is the
		// decided state. Used both for the PRE-gate check and wired onto the gate for
		// a halt this call's OWN drain step discovers: one implementation of "how a
		// halted run answers", not two.
		onHalted := ad.HaltReplayer(logger, stdout, hook, ev, ev.SessionID, nudgeFlush, func() (hookflow.OutputContract, string, string) { return haltReplayTriple(hook, ev) })

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
			spoolObserve, spoolObserveHead = ad.GateSpoolers(logger, string(hook), devEv, nudgeFlush)
		}

		var (
			c      hookflow.OutputContract = promptContract
			target hookflow.EnforceTarget  = newPromptTarget(id, ad.Mapper, ev)
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
		// The session rollup fires ONLY when no turn
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

// inlineAttempt drains this session's newest events within window
// (hookflow.Engine.InlineAttempt), falling back to the detached flusher.
func inlineAttempt(ad *Adapter, logger *log.Logger, sessionID string, hookStart time.Time, window, attemptTimeout time.Duration) {
	ad.InlineAttempt(logger, sessionID, hookStart, window, attemptTimeout, func() { forceFlusher(logger, sessionID) })
}

// forceFlusher spawns the detached flusher regardless of the realtime_flush
// toggle (hookflow.ForceFlusher).
func forceFlusher(logger *log.Logger, sessionID string) {
	hookflow.ForceFlusher(logger, DefaultSpoolDir(), provider, sessionID)
}
