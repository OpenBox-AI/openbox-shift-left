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

// flushBudget bounds the SessionEnd drain. It must stay strictly under the
// timeout Codex actually enforces, which is 3 s -- not the 15 the installer used
// to register. Codex clamps and says so: "clamping SessionEnd hook timeout to 3s
// in <hooks.json>" (measured on codex-cli 0.150.0-alpha.8; phase 00 probe P0.4).
// The old 12 s budget was therefore never reachable -- the drain was SIGKILLed
// mid-flight every time. 2 s leaves roughly a second for process start,
// ResolveCredentials and client.New, so the flush now finishes and retires
// cleanly instead of dying partway.
const flushBudget = 2 * time.Second

// RunHook executes the path for one Codex hook invocation; the engine behind
// the unified `openbox hook codex <event>` subcommand.
//   - In observe mode (the default) it writes nothing to stdout.
//   - It never returns a blocking signal in observe mode: any failure (bad
//     payload, missing identity, unreachable OpenBox, even a panic) is logged
//     and swallowed.
func RunHook(sub string, stdin io.Reader, stdout io.Writer, logger *log.Logger) {
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
	ev, err := ParseHookEvent(stdin)
	if err != nil {
		logger.Printf("dropping %s event: %v", hook, err)
		return
	}

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
	gated := ResolveEnforce() && (hook == HookPreToolUse || hook == HookUserPromptSubmit ||
		hook == HookPermissionRequest)

	if gated {
		// The latch is keyed by session id because the gate below sets no RunID,
		// so EnforceGate.runID falls through to t.SessionID(). Reader and writer
		// must move together: adding a RunID to the gate literal without changing
		// this call would write the latch under one key and read it under another,
		// silently un-halting a terminated session.
		if info, halted := hookflow.SessionHalted(ev.SessionID); halted {
			if _, err := ad.Observe(hook, ev); err != nil {
				logger.Printf("spool %s event: %v", hook, err)
			}
			nudgeFlush()
			c, toolName, toolKind := haltReplayTriple(hook, ev)
			// The latch IS the decided state: no /evaluate round trip.
			hookflow.ReplaySessionHalt(logger, stdout, info, ev.SessionID, toolName, toolKind, c)
			return
		}

		var spoolObserve func()
		if devEv, ok := ad.Mapper.Map(hook, ev); ok {
			appendObserve := ad.RecordDeferred(devEv)
			spoolObserve = func() {
				if err := appendObserve(); err != nil {
					logger.Printf("spool %s event: %v", hook, err)
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
			Contract:     c,
			Evaluator:    evaluator,
			Record:       func(dec decision.Decision, res hookflow.ApplyResult) { record(logger, ev, dec, res) },
			SpoolObserve: spoolObserve,
			// RunID deliberately unset: see the latch-key note above.
		}
		res := g.Run(context.Background(), logger, stdout, target)
		// A gate that already wrote owns stdout; appending findings after it
		// would put a second JSON document on the same stream.
		if hook == HookUserPromptSubmit && !res.Emitted && ResolveFindings() {
			hookflow.SurfaceFindings(provider, string(hook), stdout, logger)
		}
		return
	}

	// Default off: with enforce off the decider is never invoked and the gate is
	// inert, so the observe path stays byte-identical to observe-only.
	if _, err := ad.Observe(hook, ev); err != nil {
		logger.Printf("spool %s event: %v", hook, err)
	}
	if hook != HookSessionEnd {
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
		runFlush(logger, ev.SessionID)
	}
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
