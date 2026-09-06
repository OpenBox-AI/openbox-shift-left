package claudecode

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

const flushBudget = 12 * time.Second

// RunHook executes the observe-only path for one Claude Code hook invocation.
// It is the single engine behind `openbox hook claude-code <event>`, which is
// the only way in: the standalone alias binary it once also served was never
// released and is gone.
func RunHook(sub string, stdin io.Reader, stdout io.Writer, logger *log.Logger) {
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
	ev, err := ParseHookEvent(stdin)
	if err != nil {
		logger.Printf("dropping %s event: %v", hook, err)
		return
	}

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
		} else if err := obgit.WriteSessionRecord(regDir, ev.SessionID, ev.Cwd, time.Now()); err != nil {
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
			Undelivered: ad.Spool.UndeliveredCount(),
			Discarded:   ad.Spool.DiscardedCount(),
		}
	}

	if hook == HookSessionStart {
		posture := effectivePosture()
		ad.Mapper.Posture = &posture
	}

	nudgeFlush := func() {
		hookflow.RealtimeTrigger{Spool: ad.Spool, Provider: "claude-code"}.Maybe(logger, ev.SessionID)
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
	// through enumOr): the vendor documents blocking decisions on it as
	// ignored, and gating it anyway would file an enforcement audit line
	// claiming a block that never happened.
	gated := ResolveEnforce() && (hook == HookPreToolUse || hook == HookUserPromptSubmit ||
		(hook == HookConfigChange && ev.Source != policySettingsSource))

	if gated {
		if info, halted := hookflow.SessionHalted(runID); halted {
			if _, err := ad.Observe(hook, ev); err != nil {
				logger.Printf("spool %s event: %v", hook, err)
			}
			nudgeFlush()
			var c hookflow.OutputContract = promptContract
			toolName, toolKind := promptToolKind, promptToolKind
			switch hook {
			case HookPreToolUse:
				kind, _, _, _, _ := classifyTool(ev.ToolName)
				c, toolName, toolKind = contract, ev.ToolName, string(kind)
			case HookConfigChange:
				c, toolName, toolKind = configContract, configToolKind, configToolKind
			}
			hookflow.ReplaySessionHalt(logger, stdout, info, ev.SessionID, toolName, toolKind, c)
			return
		}
	}

	var spoolObserve func()
	if gated {
		if devEv, ok := ad.Mapper.Map(hook, ev); ok {
			appendObserve := ad.RecordDeferred(devEv)
			spoolObserve = func() {
				if err := appendObserve(); err != nil {
					logger.Printf("spool %s event: %v", hook, err)
				}
				nudgeFlush()
			}
		}
	} else {
		if _, err := ad.Observe(hook, ev); err != nil {
			logger.Printf("spool %s event: %v", hook, err)
		}
		if hook != HookSessionEnd {
			nudgeFlush()
		}
	}

	// With enforce off the gate is never invoked and the observe path stays byte-
	// identical to observe-only.
	if gated {
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
			Contract:     c,
			Evaluator:    evaluator,
			Record:       func(dec decision.Decision, res hookflow.ApplyResult) { record(logger, ev, dec, res) },
			SpoolObserve: spoolObserve,
			RunID:        runID,
		}
		res := g.Run(context.Background(), logger, stdout, target)
		if hook == HookUserPromptSubmit && !res.Emitted && ResolveFindings() {
			hookflow.SurfaceFindings("claude-code", string(hook), stdout, logger)
		}
		return
	}

	if hook == HookPostToolUse || hook == HookUserPromptSubmit {
		if ResolveFindings() {
			hookflow.SurfaceFindings("claude-code", string(hook), stdout, logger)
		}
	}

	if hook == HookSessionStart {
		maybeInstallGitHook(logger, ev.Cwd)
	}

	if hook == HookSessionEnd {
		runFlush(logger, ev.SessionID)
	}
}

// isBumpSource reports whether SessionStart's source enum opens a new run
// generation (R4, resume-only re-scope): ONLY resume does. clear joins
// startup, compact and fork as non-bumping -- a `/clear` was measured live
// (Claude Code 2.1.263) to mint a brand-new session id, ~46ms after the
// SessionEnd that sealed the old one, so the SessionStart it fires next has
// no prior run under ITS id to continue; `--resume` is the one source that
// reuses the id, which is the only case where "continue the run this id
// already has" is a coherent question. enumOr guards against an
// unrecognized value reading as a bump.
func isBumpSource(source string) bool {
	return enumOr(source, sourceValues) == "resume"
}

// anchorsTurnCursor reports whether a SessionStart opens onto a transcript that
// already holds turns this runtime has counted. `resume` reopens the SAME file
// the previous sitting wrote; `fork` opens a fresh session id over a COPY of the
// parent's history. Both start reading at a point where the prose above the
// cursor is old news. `startup` has no history, `compact` keeps its cursor, and
// `clear` mints a fresh id whose transcript is empty.
func anchorsTurnCursor(source string) bool {
	switch enumOr(source, sourceValues) {
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

func maybeInstallGitHook(logger *log.Logger, cwd string) {
	if !ResolveInstallGitHook() {
		return
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		return
	}
	hooksDir, err := obgit.Git{Dir: cwd}.HooksDirDefault()
	if err != nil {
		return // not a git repo / detached worktree; nothing to install into
	}
	cfg := obgit.HookConfig{Command: self, Args: []string{"hook", "git", "prepare-commit-msg"}}
	if err := obgit.InstallPostCommitHook(hooksDir, cfg); err != nil {
		logger.Printf("post-commit hook not installed (trailer still works): %v", err)
	}
	if err := obgit.InstallHook(hooksDir, cfg); err != nil {
		logger.Printf("git-hook install skipped: %v", err)
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
