package claudecode

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
// (30s) attempts with slack.
const flushBudget = 60 * time.Second

// sessionStartInlineWindow bounds SessionStart's own inline delivery attempt:
// the CC SessionStart hook is otherHookTimeoutSec (5s); this leaves 1s of
// slack for the rest of the hook and process teardown, and is comfortably
// under the window so more than one queued line can still be attempted if a
// backlog is waiting.
const sessionStartInlineWindow = time.Duration(otherHookTimeoutSec-1) * time.Second

// sessionStartAttemptTimeout bounds each individual attempt inside
// sessionStartInlineWindow, strictly less than it: DrainSession never starts
// an attempt it cannot finish, so an attemptTimeout equal to the whole window
// would let even a fast attempt's few milliseconds of real elapsed time push
// the NEXT line's remaining budget below the bound and defer it needlessly.
const sessionStartAttemptTimeout = sessionStartInlineWindow - time.Second

// sessionEndInlineWindow bounds SessionEnd's own inline delivery attempt: the
// CC SessionEnd hook is 15s; this leaves 3s for the git-registry cleanup and
// finops rollup already done earlier in this hook, plus process teardown.
const sessionEndInlineWindow = 12 * time.Second

// sessionEndAttemptTimeout is sessionEndInlineWindow's per-attempt bound, for
// the same reason sessionStartAttemptTimeout is not equal to its window.
const sessionEndAttemptTimeout = 10 * time.Second

// RunHook executes the observe-only path for one Claude Code hook invocation.
// It is the single engine behind `openbox hook claude-code <event>`, which is
// the only way in: the standalone alias binary it once also served was never
// released and is gone.
func RunHook(sub string, stdin io.Reader, stdout io.Writer, logger *log.Logger) {
	hookStart := time.Now()
	defer devconfig.Pin()()
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("recovered: %v", r)
		}
	}()

	if sub == "" {
		logger.Printf("usage: openbox hook claude-code <HookName|flush>")
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

	// Structural fields only (session_id + cwd), never content (INV-2).
	// Restricted to the eleven hooks that already carry a session -> cwd
	// mapping the git trailer needs; the 21 new hook classes add nothing to
	// that map (MessageDisplay/InstructionsLoaded alone would turn this into a
	// per-message file write for information those events do not carry).
	regDir := obgit.DefaultSessionDir()
	if touchesSessionRegistry(hook) {
		if hook == HookSessionEnd {
			if err := obgit.RemoveSessionRecord(regDir, ev.SessionID); err != nil {
				logger.Printf("session registry cleanup: %v", err)
			}
		} else if err := obgit.WriteSessionRecord(regDir, ev.SessionID, ev.Cwd, provider, time.Now()); err != nil {
			logger.Printf("session registry touch: %v", err)
		}
	}

	// Run identity (phase 08, RESUME-ONLY re-scope): resolved before
	// New()/Record(), so the run id this hook's event(s) are stamped with and
	// the latch this hook consults just below agree. Only
	// SessionStart(source=resume) bumps -- NOT clear: measured live against
	// installed Claude Code 2.1.263, a `/clear` mints a DIFFERENT session id
	// 46ms apart (SessionEnd reason=clear, then SessionStart source=clear on a
	// new id core has never seen), so there is no prior run for it to
	// continue; bumping it would file the new session's run under an id that
	// belongs to a DIFFERENT (fresh) session's future record. Every other hook
	// -- and every other SessionStart source -- reads the existing record.
	// Absent/unreadable/corrupt/inconsistent ⇒ generation 0 plus one stderr
	// line (INV-3, R6): a run-identity lookup must never block a tool call or
	// drop an event.
	runStore := obgit.RunStore{Dir: obgit.RunDir(regDir)}
	var run RunIdentity
	if hook == HookSessionStart && isBumpSource(ev.Source) {
		if rec, err := runStore.Bump(ev.SessionID); err != nil {
			logger.Printf("run identity: bump failed, continuing at generation 0: %v", err)
		} else {
			run = RunIdentity{Generation: rec.Generation, RunID: rec.RunID, ContinuedFrom: rec.PreviousRunID}
		}
	} else if rec, err := runStore.Read(ev.SessionID); err != nil {
		logger.Printf("run identity: record unreadable, continuing at generation 0: %v", err)
	} else {
		run = RunIdentity{Generation: rec.Generation, RunID: rec.RunID}
	}
	// The same selection client.runIDFor makes: the minted run id when one
	// exists, else the bare session id (generation 0).
	runID := ev.SessionID
	if run.RunID != "" {
		runID = run.RunID
	}

	ad := New(id, DefaultSpoolDir())
	ad.Mapper.Run = &run
	ad.Mapper.CaptureContent = ResolveContentCapture()
	if ResolveSecretDetection() {
		redactor := decision.NewRedactor()
		ad.Mapper.RedactContent = func(s string) string { return hookflow.RedactText(redactor, s) }
	}
	pinnedNow := time.Now()
	ad.Mapper.Now = func() time.Time { return pinnedNow }

	// Before anything reads the cursor: a resumed or forked sitting inherits a
	// transcript whose earlier turns are already counted. See anchorTurnCursor.
	if hook == HookSessionStart && anchorsTurnCursor(ev.Source) {
		anchorTurnCursor(ad, logger, ev)
	}

	// This is the only place transcript_path is opened, and only when
	// ResolveFinops() is set; with finops off it is never dereferenced.
	if hook == HookSessionEnd && ResolveFinops() {
		if tokens, cost, err := readTranscriptUsage(ev.TranscriptPath); err != nil {
			logger.Printf("finops: transcript usage skipped: %v", err)
		} else if tokens != nil || cost != nil {
			ad.Mapper.Finops = &FinopsUsage{Tokens: tokens, Cost: cost}
		}
	}

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
	// ConfigChange's policy_settings source is never gated (raw compare, not
	// through EnumOr): the vendor documents blocking decisions on it as
	// ignored, and gating it anyway would file an enforcement audit line
	// claiming a block that never happened.
	//
	// Enforcement is always on now (ResolveEnforce always reports true): every
	// gated hook always runs the gate below and reads the latch. There is no
	// longer an observe-only branch for these three hook classes.
	gated := hook == HookPreToolUse || hook == HookUserPromptSubmit ||
		(hook == HookConfigChange && ev.Source != policySettingsSource)

	if gated {
		// haltReplayTriple picks the contract and the labels a halted session
		// replays with, per hook: the same choice the gate's own escalation
		// switch below makes.
		haltReplayTriple := func() (hookflow.OutputContract, string, string) {
			var c hookflow.OutputContract = promptContract
			toolName, toolKind := promptToolKind, promptToolKind
			switch hook {
			case HookPreToolUse:
				kind, _, _, _, _ := classifyTool(ev.ToolName)
				c, toolName, toolKind = contract, ev.ToolName, string(kind)
			case HookConfigChange:
				c, toolName, toolKind = configContract, configToolKind, configToolKind
			}
			return c, toolName, toolKind
		}
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
			c, toolName, toolKind := haltReplayTriple()
			dec, res := hookflow.SessionHaltReplay(logger, stdout, info, false, nil, toolName, c)
			hookflow.RecordEnforcement(logger, ev.SessionID, toolKind, dec, res)
			return res
		}

		if info, halted := hookflow.SessionHalted(runID); halted {
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
		case HookConfigChange:
			c, target, record = configContract, configSubject{id: id, mapper: ad.Mapper, ev: ev}, recordConfigEnforcement
		}
		g := hookflow.EnforceGate{
			Contract:         c,
			Evaluator:        evaluator,
			Record:           func(dec decision.Decision, res hookflow.ApplyResult) { record(logger, ev, dec, res) },
			SpoolObserve:     spoolObserve,
			SpoolObserveHead: spoolObserveHead,
			RunID:            runID,
			Queue:            ad.Engine,
			OnHalted:         onHalted,
			ForceFlush:       func() { forceFlusher(logger, ev.SessionID) },
		}
		res := g.Run(context.Background(), logger, stdout, target)
		if hook == HookUserPromptSubmit && !res.Emitted && ResolveFindings() {
			hookflow.SurfaceFindings(provider, string(hook), stdout, logger)
		}
		return
	}

	// Every hook class that reaches here is never gated (Stop/SubagentStop
	// already returned above, and PreToolUse/UserPromptSubmit/gated
	// ConfigChange always take the branch above): this is the observe-only
	// path, byte-identical to today regardless of enforcement.
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
		// Handled after the finops/git-registry work below, not nudged here.
	default:
		nudgeFlush()
	}

	if hook == HookPostToolUse || hook == HookUserPromptSubmit {
		if ResolveFindings() {
			hookflow.SurfaceFindings(provider, string(hook), stdout, logger)
		}
	}

	if hook == HookSessionStart && ResolveInstallGitHook() {
		obgit.InstallAmbient(ev.Cwd, logger.Printf)
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

// isBumpSource reports whether SessionStart's source enum opens a new run
// generation (R4, resume-only re-scope): ONLY resume does. clear joins
// startup, compact and fork as non-bumping -- a `/clear` was measured live
// (Claude Code 2.1.263) to mint a brand-new session id, ~46ms after the
// SessionEnd that sealed the old one, so the SessionStart it fires next has
// no prior run under ITS id to continue; `--resume` is the one source that
// reuses the id, which is the only case where "continue the run this id
// already has" is a coherent question. EnumOr guards against an
// unrecognized value reading as a bump.
func isBumpSource(source string) bool {
	return hookflow.EnumOr(source, sourceValues) == "resume"
}

// anchorsTurnCursor reports whether a SessionStart opens onto a transcript that
// already holds turns this runtime has counted. `resume` reopens the SAME file
// the previous sitting wrote; `fork` opens a fresh session id over a COPY of the
// parent's history. Both start reading at a point where the prose above the
// cursor is old news. `startup` has no history, `compact` keeps its cursor, and
// `clear` mints a fresh id whose transcript is empty.
func anchorsTurnCursor(source string) bool {
	switch hookflow.EnumOr(source, sourceValues) {
	case "resume", "fork":
		return true
	}
	return false
}

// anchorTurnCursor pins the main-thread turn cursor to the current end of the
// transcript, so the first turn of a resumed or forked sitting reports only what
// that sitting actually spent.
//
// Without it: SessionEnd clears the cursor (Engine.Record -> Turns.ClearSession),
// so a later resume reads from offset 0 and readTurnUsage -- which sums a single
// window from the cursor to EOF -- attributes EVERY earlier turn's tokens to the
// first Stop after the resume. That has always been the behaviour; it stayed
// invisible because the over-counted pair reused activity id `<session>:turn:0`
// and the control plane discarded it as a duplicate of the previous run's first
// turn. Run identity gives the continued run its own run_id, which is part of
// that dedupe key, so the duplicate would now be stored and the whole prior
// sitting billed a second time. `fork` never had the accidental protection --
// a fresh session id collides with nothing -- so it has been over-counting
// outright, and this closes that too.
//
// Best-effort by design (INV-3): a transcript that cannot be stat'ed leaves the
// cursor alone and costs one stderr line. Over-reporting a turn is the failure
// this prevents; failing to start a session would be worse.
func anchorTurnCursor(ad *Adapter, logger *log.Logger, ev *HookEvent) {
	if ev.TranscriptPath == "" {
		return
	}
	fi, err := os.Stat(ev.TranscriptPath)
	if err != nil {
		logger.Printf("finops: turn cursor not anchored for source=%s: %v", ev.Source, err)
		return
	}
	// The main-thread cursor, which is the one Stop reads; a sidechain carries
	// its own agent id and its own file.
	if err := ad.Turns.Write(ev.SessionID, "", hookflow.TurnPos{Offset: fi.Size(), Index: 0}); err != nil {
		logger.Printf("finops: turn cursor not anchored for source=%s: %v", ev.Source, err)
	}
}

// touchesSessionRegistry reports whether hook writes the session -> cwd
// registry the git trailer reads: the existing eleven only. CwdChanged is
// deliberately NOT here -- it is the one new hook that could carry cwd
// information, but whether the common `cwd` field on that payload is pre- or
// post-change is undocumented, and a registry keyed on the wrong one is worse
// than one not touched.
func touchesSessionRegistry(hook HookName) bool {
	switch hook {
	case HookSessionStart, HookUserPromptSubmit, HookPreToolUse, HookPostToolUse,
		HookPostToolUseFailure, HookSessionEnd, HookStop, HookSubagentStop,
		HookSubagentStart, HookPermissionDenied, HookStopFailure:
		return true
	}
	return false
}

// emitTurn reads the transcript from the cursor's offset, taking this side of
// the sidechain partition only, spools both halves of the pair, and advances the
// cursor last: a crash then over-reports into the control plane's dedupe rather
// than losing a turn.
func emitTurn(ad *Adapter, logger *log.Logger, hook HookName, ev *HookEvent) {
	agentID := ev.AgentID
	sidechain := hook == HookSubagentStop
	if sidechain && agentID == "" {
		logger.Printf("finops: SubagentStop without agent_id, skipping turn (would share the main-thread cursor)")
		return
	}

	pos := ad.Turns.Read(ev.SessionID, agentID)
	window, next, err := readTurnUsage(ev.TranscriptPath, pos, sidechain)
	if err != nil {
		logger.Printf("finops: turn usage skipped: %v", err)
		return
	}
	if !window.HasUsage {
		if next != pos {
			if err := ad.Turns.Write(ev.SessionID, agentID, next); err != nil {
				logger.Printf("finops: turn cursor write failed: %v", err)
			}
		}
		return
	}

	started, completed, ok := ad.Mapper.MapTurn(ev, window, pos.Index)
	if !ok {
		return
	}
	for _, turnEv := range []client.DevEvent{started, completed} {
		if err := ad.Record(turnEv); err != nil {
			logger.Printf("finops: spool %s event: %v", turnEv.EventType, err)
			return
		}
	}

	next.Index = pos.Index + 1
	if err := ad.Turns.Write(ev.SessionID, agentID, next); err != nil {
		logger.Printf("finops: turn cursor write failed (window may be re-read): %v", err)
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
	// Diagnostics only; stderr, never stdout, so a SessionStart/UserPromptSubmit
	// exit-0 hook still injects nothing (INV-3).
	ad.Advisory.Log = logger
	// One engine call: the lock, the drain loop and the retire ordering are its.
	n, err := ad.FlushOrSweep(ctx, sessionID, cl)
	if err != nil {
		logger.Printf("flush ended early after %d event(s): %v", n, err)
	}
}
