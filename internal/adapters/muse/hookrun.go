package muse

import (
	"context"
	"io"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// inlineWindow bounds SessionStart's and SessionEnd's own inline delivery
// attempt: both run under the 5s ceiling of a non-gated handler, and this
// leaves 1s for process start, credential resolution and teardown.
var inlineWindow = Engine{}.HookCeilings().Other - time.Second

// inlineAttemptTimeout bounds each attempt inside inlineWindow, strictly less
// than it: a drain never starts an attempt it cannot finish, so an equal bound
// would defer even a fast attempt needlessly.
var inlineAttemptTimeout = inlineWindow - time.Second

// nowFn pins the clock every event of one hook invocation is stamped with. A
// seam, so a test can make the run fail somewhere only a fault would.
var nowFn = time.Now

// RunHook executes one Muse hook invocation; the engine behind the unified
// `openbox hook muse <event>` subcommand.
//
// Muse fails OPEN on a hook that crashes or answers invalidly, so a gated event
// must end in a schema-valid answer or a non-zero exit, never a silent exit 0.
// Two rules follow. A payload that cannot be read answers with a refusal. And a
// panic on a gated event is not recovered here (see Engine.RunHook): it
// reaches the caller, which exits with FaultExitCode. A panic on any other event
// gates nothing, so it is logged and swallowed.
func RunHook(sub string, stdin io.Reader, stdout io.Writer, logger *log.Logger) {
	hookStart := time.Now()
	defer devconfig.Pin()()
	if !HookName(sub).Gated() {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("recovered: %v", r)
			}
		}()
	}

	if hookflow.RunSubcommand(logger, provider, DefaultSpoolDir(), sub) {
		return
	}

	hook, err := ParseHookName(sub)
	if err != nil {
		logger.Printf("%v", err)
		return
	}
	if !hook.Observed() {
		// A handler for an event with no contract type. Drain stdin so Muse is
		// not left writing into a pipe nobody reads, and report nothing.
		_, _ = io.Copy(io.Discard, io.LimitReader(stdin, maxHookPayload))
		return
	}

	id, err := ResolveIdentity()
	if err != nil {
		// No identity is governance inactive, not a fault: `openbox init` has
		// not run, or the store predates the current one. The caller prints the
		// fix once; denying every call of an unconfigured machine would make
		// Muse unusable rather than governed.
		logger.Printf("no identity, dropping %s event: %v", hook, err)
		return
	}

	ev, err := hookflow.ReadHookEvent(provider, string(hook), stdin, func(e *HookEvent) string { return e.SessionID })
	if err != nil {
		logger.Printf("dropping %s event: %v", hook, err)
		if hook.Gated() {
			refuseUnreadable(hook, stdout, logger)
		}
		return
	}

	stdout, traceOut := hookflow.TraceHookOutput(provider, string(hook), stdout, hookStart, func() string { return ev.SessionID })
	defer traceOut()

	ad := New(id, DefaultSpoolDir())
	ad.Mapper.CaptureContent = devconfig.ResolveContentCapture()
	ad.Mapper.RedactContent = hookflow.ContentRedactor(devconfig.ResolveSecretDetection())
	pinnedNow := nowFn()
	ad.Mapper.Now = func() time.Time { return pinnedNow }

	lc := lifecycle{
		Dir:  lifecycleDir(DefaultSpoolDir()),
		Runs: obgit.RunStore{Dir: obgit.RunDir(obgit.DefaultSessionDir())},
		Now:  func() time.Time { return pinnedNow },
		Log:  logger,
	}
	// A subagent's event moves into its parent's session before anything reads
	// the session id: the spool, the run, the latch and the gate ledger are all
	// the parent's from here on, as a Claude Code subagent's are.
	lc.foldSubagent(hook, ev, sessionLogRoot())

	if hook.ends() && !ev.folded() {
		ad.Mapper.Evidence = &EvidenceState{
			Undelivered: ad.Spool.UndeliveredCountFor(ev.SessionID),
			Discarded:   ad.Spool.DiscardedCount(),
		}
	}
	if hook == HookSessionStart || (hook == HookSubagentStart && !ev.folded()) {
		posture := effectivePosture(lc.Dir)
		ad.Mapper.Posture = &posture
	}

	// Run identity, resolved before anything is mapped or spooled, so the run
	// id this hook's event(s) are stamped with and the latch this hook consults
	// agree. A resume continues the session as a new run (continue-as-new),
	// which is what leaves it unlatched, and Muse sends no SessionStart for one:
	// the lifecycle opens the run, and spools its SessionStarted ahead of this
	// hook's own event, when the session's first event is not a SessionStart.
	// Absent, unreadable or corrupt records fall back to generation 0 with one
	// stderr line: a run-identity lookup must never block a call or drop an
	// event.
	var (
		run  RunIdentity
		tr   transition
		lock func()
	)
	if hook == HookStop {
		// Stop reports a turn, never a run boundary: it neither opens nor
		// closes a run.
		run = lc.current(ev.SessionID)
	} else {
		lock = lc.lock(ev.SessionID)
		run, tr = lc.advance(hook, ev)
		ad.Mapper.Run = &run
		if tr.Open {
			if ad.Mapper.Posture == nil {
				posture := effectivePosture(lc.Dir)
				ad.Mapper.Posture = &posture
			}
			if start, ok := ad.Mapper.openEvent(hook, ev, tr.Resumed); ok {
				if err := ad.Record(start); err != nil {
					logger.Printf("spool the opening SessionStarted: %v", err)
				}
			}
		}
		lock()
	}
	ad.Mapper.Run = &run
	// The same selection client.runIDFor makes: the minted run id when one
	// exists, else the bare session id.
	runID := ev.SessionID
	if run.RunID != "" {
		runID = run.RunID
	}

	nudgeFlush := func() {
		hookflow.RealtimeTrigger{Spool: ad.Spool, Provider: provider}.Maybe(logger, ev.SessionID)
	}

	if hook.Gated() {
		runGated(ad, id, hook, ev, runID, stdout, logger, nudgeFlush)
		return
	}

	// Every hook that reaches here is never gated: the observe-only path.
	devEv, mapped := ad.Mapper.Map(hook, ev)
	if tr.Drop {
		mapped = false
	}
	if mapped {
		threadModelCallDuration(ad.Durations, &devEv)
		if err := ad.Record(devEv); err != nil {
			logger.Printf("spool %s event: %v", hook, err)
		}
	}
	if hook == HookPostLLMCall {
		if mapped {
			traceModelCall(hook, ev, devEv)
		}
		if !tr.Drop {
			stashModelCall(ad, logger, ev)
		}
	}
	switch hook {
	case HookStop:
		// The turn's pair, then the pass over the session's own journal. A turn
		// is reported only into a run that is open: Stop never opens one.
		if st, have := lc.load(ev.SessionID); have && !st.Sealed {
			emitTurn(ad, logger, ev)
			nudgeFlush()
		}
		reconcileOnHook(hook, ev.SessionID, runID, hookStart, pinnedNow, logger)
	case HookSessionStart:
		// The session's own WorkflowStarted event is drained inline, under this
		// session's own stripe, right after the append above, so no detached
		// flusher's debounce window can let anything else of this run overtake it.
		inlineAttempt(ad, logger, ev.SessionID, hookStart)
	case HookSessionEnd:
		if !ev.folded() {
			clearTurnIndex(ad.Spool.Dir, ev.SessionID)
		}
		// Muse kills a SessionEnd hook as the session exits, whatever its
		// timeout, so an inline attempt here dies mid-delivery and leaves a
		// half-sent file the next drain can only discard. Delivery goes to the
		// detached flusher (its own session, so it outlives this process) at
		// once; the journal pass after it is best-effort.
		forceFlusher(logger, ev.SessionID)
		reconcileOnHook(hook, ev.SessionID, runID, hookStart, pinnedNow, logger)
	case HookSubagentStart, HookSubagentStop:
		// Muse tears a subagent's hooks down as soon as the subagent is done,
		// the same race as SessionEnd, so a subagent's SessionStarted and
		// SessionEnded go to the detached flusher too; a folded subagent's
		// events sit in its parent's spool, which is the one flushed. A gated
		// call drains its session's backlog first, so a run's WorkflowStarted
		// still reaches core ahead of anything else of it. A subagent's own
		// journal is not reconciled: the join looks for the parent's actions.
		if hook == HookSubagentStop && ev.folded() {
			if st, have := lc.load(ev.SessionID); have && !st.Sealed {
				emitTurn(ad, logger, ev)
			}
		}
		forceFlusher(logger, ev.SessionID)
	default:
		nudgeFlush()
	}
	if hook == HookPostToolUse && devconfig.ResolveFindings() {
		surfaceFindings(hook, stdout, logger)
	}
}

// refuseUnreadable answers a gated event whose payload could not be parsed. The
// event is unknown, so nothing can be evaluated, and a silent exit 0 would be an
// allow: the answer is the refusal the event's own contract renders.
func refuseUnreadable(hook HookName, stdout io.Writer, logger *log.Logger) {
	line, _ := contractFor(hook).Render(hookflow.DecisionDeny, "OpenBox could not read this hook payload, so the call was not evaluated", nil)
	if len(line) == 0 {
		return
	}
	if _, err := stdout.Write(append(line, '\n')); err != nil {
		logger.Printf("refusal write failed: %v", err)
	}
}

// runGated runs the synchronous gate for one gated event: latch check, local
// redaction, /evaluate, failure policy after the evaluation, approval hold,
// latch on a real HALT, then the closed answer of the event's own contract.
func runGated(ad *Adapter, id Identity, hook HookName, ev *HookEvent, runID string, stdout io.Writer, logger *log.Logger, nudgeFlush func()) {
	// onHalted renders a halted run's answer to this gated call: no server round
	// trip, the latch is the decided state. Used both for the PRE-gate check and
	// wired onto the gate for a halt this call's OWN drain step discovers: one
	// implementation of "how a halted run answers", not two.
	onHalted := ad.HaltReplayer(logger, stdout, hook, ev, ev.SessionID, nudgeFlush, func() (hookflow.OutputContract, string, string) { return haltReplayTriple(hook, ev) })

	if hook == HookPreToolUse {
		// Before anything can end the call: whatever the verdict, a call whose
		// hook started is one the journal join must find.
		if err := RecordGateCall(DefaultSpoolDir(), ev.SessionID, ev.ToolName, ev.ToolUseID); err != nil {
			logger.Printf("gate ledger: %v", err)
		}
		// The shell this call runs carries the tool-use id and no session id, so
		// the commit hook finds the (parent) session through this entry. It is
		// written before the verdict, so a slow gate cannot reorder it.
		if err := PutToolUse(DefaultSpoolDir(), ev.ToolUseID, ev.SessionID); err != nil {
			logger.Printf("tool-use index: %v", err)
		}
	}

	if info, halted := hookflow.SessionHalted(runID); halted {
		onHalted(info)
		return
	}

	var spoolObserve, spoolObserveHead func()
	if devEv, ok := ad.Mapper.Map(hook, ev); ok {
		// Threaded once here: whichever of the two closures the gate invokes needs
		// the SAME already-threaded event, and threading it twice would double-put
		// its duration-pairing entry.
		ad.ThreadDuration(&devEv)
		threadModelCallDuration(ad.Durations, &devEv)
		if hook == HookPreLLMCall {
			traceModelCall(hook, ev, devEv)
		}
		spoolObserve, spoolObserveHead = ad.GateSpoolers(logger, string(hook), devEv, nudgeFlush)
	}

	var (
		target hookflow.EnforceTarget
		record func(*log.Logger, *HookEvent, decision.Decision, hookflow.ApplyResult)
	)
	switch hook {
	case HookPreToolUse:
		target, record = enforceTarget{id: id, mapper: ad.Mapper, ev: ev}, recordEnforcement
	case HookPermissionRequest:
		target, record = permissionTarget{id: id, mapper: ad.Mapper, ev: ev}, recordEnforcement
	case HookPreLLMCall:
		target, record = llmTarget{id: id, mapper: ad.Mapper, ev: ev}, recordModelCallEnforcement
	default:
		target, record = newPromptTarget(id, ad.Mapper, ev), recordPromptEnforcement
	}
	g := hookflow.EnforceGate{
		Contract:         contractFor(hook),
		Evaluator:        evaluator,
		Record:           func(dec decision.Decision, res hookflow.ApplyResult) { record(logger, ev, dec, res) },
		SpoolObserve:     spoolObserve,
		SpoolObserveHead: spoolObserveHead,
		// The latch is read and written under the run id: moving one without the
		// other would silently un-halt a terminated run.
		RunID:      runID,
		Queue:      ad.Engine,
		OnHalted:   onHalted,
		ForceFlush: func() { forceFlusher(logger, ev.SessionID) },
	}
	g.Run(context.Background(), logger, stdout, target)
}

// inlineAttempt drains this session's newest events within the inline window
// (hookflow.Engine.InlineAttempt), falling back to the detached flusher.
func inlineAttempt(ad *Adapter, logger *log.Logger, sessionID string, hookStart time.Time) {
	ad.InlineAttempt(logger, sessionID, hookStart, inlineWindow, inlineAttemptTimeout, func() { forceFlusher(logger, sessionID) })
}

// forceFlusher spawns the detached flusher regardless of the realtime_flush
// toggle (hookflow.ForceFlusher).
func forceFlusher(logger *log.Logger, sessionID string) {
	hookflow.ForceFlusher(logger, DefaultSpoolDir(), provider, sessionID)
}
