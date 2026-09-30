package muse

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// flushBudget bounds the detached `flush` subcommand: the realtime trigger's
// spawned child and the periodic sweeper both inherit it. 60s so a pass can
// start attempt-timeout-bounded (30s) deliveries with slack.
const flushBudget = 60 * time.Second

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

	if sub == "" {
		logger.Printf("usage: openbox hook muse <HookName|flush>")
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

	// Read the whole payload BEFORE it is parsed, bounded the same way
	// ParseHookEvent's own decoder is: the trace records raw stdin even when
	// parsing fails, and a stream decoder consumed on a failed parse cannot be
	// replayed to build that record afterward.
	rawStdin, _ := io.ReadAll(io.LimitReader(stdin, maxHookPayload))
	ev, err := ParseHookEvent(bytes.NewReader(rawStdin))
	sessionID := ""
	if ev != nil {
		sessionID = ev.SessionID
	}
	hookflow.TraceHookIn(provider, string(hook), sessionID, rawStdin, err)
	if err != nil {
		logger.Printf("dropping %s event: %v", hook, err)
		if hook.Gated() {
			refuseUnreadable(hook, stdout, logger)
		}
		return
	}

	var hookOut hookflow.HookOutputBuffer
	stdout = io.MultiWriter(stdout, &hookOut)
	defer func() {
		hookflow.TraceHookOut(provider, string(hook), ev.SessionID, hookStart, &hookOut)
	}()

	// Run identity: resolved before New()/Record(), so the run id this hook's
	// event(s) are stamped with and the latch this hook consults agree. A resume
	// or a clear opens a new run of the session (continue-as-new), which is what
	// leaves it unlatched. Absent, unreadable or corrupt records fall back to
	// generation 0 with one stderr line: a run-identity lookup must never block
	// a call or drop an event.
	runStore := obgit.RunStore{Dir: obgit.RunDir(obgit.DefaultSessionDir())}
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
	// exists, else the bare session id.
	runID := ev.SessionID
	if run.RunID != "" {
		runID = run.RunID
	}

	ad := New(id, DefaultSpoolDir())
	ad.Mapper.Run = &run
	ad.Mapper.CaptureContent = devconfig.ResolveContentCapture()
	if devconfig.ResolveSecretDetection() {
		redactor := decision.NewRedactor()
		ad.Mapper.RedactContent = func(s string) string { return hookflow.RedactText(redactor, s) }
	}
	pinnedNow := nowFn()
	ad.Mapper.Now = func() time.Time { return pinnedNow }

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

	if hook.Gated() {
		runGated(ad, id, hook, ev, runID, stdout, logger, nudgeFlush)
		return
	}

	// Every hook that reaches here is never gated: the observe-only path.
	devEv, mapped := ad.Mapper.Map(hook, ev)
	if mapped {
		if err := ad.Record(devEv); err != nil {
			logger.Printf("spool %s event: %v", hook, err)
		}
	}
	if hook == HookPostLLMCall && mapped {
		traceModelCall(hook, ev, devEv)
	}
	switch hook {
	case HookStop:
		// Nothing to report; the pass over the session's own journal is the
		// whole of this handler.
		reconcileOnHook(hook, ev.SessionID, runID, hookStart, pinnedNow, logger)
	case HookSessionStart:
		// The session's own WorkflowStarted event is drained inline, under this
		// session's own stripe, right after the append above, so no detached
		// flusher's debounce window can let anything else of this run overtake it.
		inlineAttempt(ad, logger, ev.SessionID, hookStart)
	case HookSessionEnd:
		// The journal pass comes first, on a budget far below the handler's
		// ceiling, so it cannot eat the delivery window it precedes.
		reconcileOnHook(hook, ev.SessionID, runID, hookStart, pinnedNow, logger)
		// The same inline attempt, within SessionEnd's own budget, falling back
		// to the detached flusher when the window runs out.
		inlineAttempt(ad, logger, ev.SessionID, hookStart)
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

	if hook == HookPreToolUse {
		// Before anything can end the call: whatever the verdict, a call whose
		// hook started is one the journal join must find.
		if err := RecordGateCall(DefaultSpoolDir(), ev.SessionID, ev.ToolName, ev.ToolUseID); err != nil {
			logger.Printf("gate ledger: %v", err)
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
		if hook == HookPreLLMCall {
			traceModelCall(hook, ev, devEv)
		}
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
		target, record = promptTarget{id: id, mapper: ad.Mapper, ev: ev}, recordPromptEnforcement
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

// inlineAttempt drives one single-attempt, own-session drain within the inline
// window (measured from hookStart, the hook's own process start, so credential
// resolution and everything RunHook already did count against the SAME budget
// the hook's own ceiling is), so a SessionStart or SessionEnd hook's newest
// event does not wait on the detached flusher's debounce window to reach core.
// It owns the stripe lock for its own session: the append this hook just made is
// drained under the SAME lock, so nothing can overtake it. An attempt whose own
// ctx expires before core answers is requeued, never scored as a failure.
// Whatever the window could not start, or get an answer for, falls back to the
// detached flusher; a re-send is deduped by core's idempotency key.
func inlineAttempt(ad *Adapter, logger *log.Logger, sessionID string, hookStart time.Time) {
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

	ctx, cancel := context.WithDeadline(context.Background(), hookStart.Add(inlineWindow))
	defer cancel()
	opts := hookflow.DrainOptions{Mode: hookflow.Block, AttemptTimeout: inlineAttemptTimeout, RequeueUnanswered: true}
	if _, err := ad.DrainSession(ctx, sessionID, cl, opts); err != nil {
		logger.Printf("inline delivery ended early: %v", err)
	}
	if ad.Spool.PendingCount(sessionID) > 0 {
		forceFlusher(logger, sessionID)
	}
}

// forceFlusher spawns the detached flusher regardless of the realtime_flush
// toggle: once an inline attempt could not finish, waiting out the ordinary
// debounce is not the fallback's job.
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
	ad.Log = logger.Printf
	ad.Advisory.Log = logger
	n, err := ad.FlushOrSweep(ctx, sessionID, cl)
	if err != nil {
		logger.Printf("flush ended early after %d event(s): %v", n, err)
	}
}
