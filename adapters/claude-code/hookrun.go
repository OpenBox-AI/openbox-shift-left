package claudecode

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// flushBudget bounds SessionEnd/flush delivery so session teardown is never held
// up (kept under the SessionEnd hook timeout in plugin/hooks.json).
const flushBudget = 12 * time.Second

// RunHook executes the observe-only path for one Claude Code hook
// invocation. It is the single engine shared by the unified `openbox hook
// claude-code <event>` subcommand and the retired-to-alias
// openbox-cc-hook binary.
//
// Safety contract (INV-3 — observe-only, never block):
//   - In observe mode (the default) it writes nothing to stdout. On
//     SessionStart/UserPromptSubmit, stdout from an exit-0 hook is
//     injected into the model's context — so all diagnostics go to
//     `logger` (stderr) only, terse and secret-free (INV-1).
//   - It never returns a blocking signal in observe mode: any failure (bad
//     payload, missing identity, unreachable OpenBox, even a panic) is
//     logged and swallowed. The caller must exit 0 regardless.
//
// Enforce mode (opt-in via ResolveEnforce, default off) is the sole
// exception to "nothing to stdout": on a PreToolUse hook it may write a
// Claude Code permissionDecision (deny/ask) to `stdout` — the INV-3b
// carve-out. It still only ever tightens (deny/ask, never allow) and
// still exits 0, so a non-blocking verdict is byte-identical to observe
// mode. Every other hook, and observe mode, write nothing to stdout.
//
// `sub` is the hook name (SessionStart/UserPromptSubmit/PreToolUse/PostToolUse/
// SessionEnd) or "flush"; `stdin` carries the hook payload JSON; `stdout` is where
// the enforce apply writes the permissionDecision (unused in observe mode).
func RunHook(sub string, stdin io.Reader, stdout io.Writer, logger *log.Logger) {
	// Freeze config reads for this hook run: everything the gate decides —
	// enforce, the failure policy, tier-2 — must come from one version of
	// dev.json, not one read per flag.
	defer devconfig.Pin()()
	// Guarantee a panic never escapes into the caller's exit path.
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
		// A realtime-trigger spawn scopes the drain to its session via
		// OPENBOX_FLUSH_SESSION; the manual `flush` subcommand leaves it unset
		// and drains everything, exactly as before.
		runFlush(logger, os.Getenv(hookflow.EnvFlushSession))
		return
	}

	hook, err := ParseHookName(sub)
	if err != nil {
		logger.Printf("%v", err)
		return
	}

	// Hot path: parse + spool. Only the DID is resolved here (no secret I/O).
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

	// Maintain this session's liveness record so the git prepare-commit-msg
	// hook can attribute a commit to the session that made it
	// (parallel-safe, worktree-scoped). Structural fields only (session_id
	// + cwd), never content (INV-2). Best-effort: a failure is logged,
	// never surfaced.
	regDir := obgit.DefaultSessionDir()
	if hook == HookSessionEnd {
		if err := obgit.RemoveSessionRecord(regDir, ev.SessionID); err != nil {
			logger.Printf("session registry cleanup: %v", err)
		}
	} else if err := obgit.WriteSessionRecord(regDir, ev.SessionID, ev.Cwd, time.Now()); err != nil {
		logger.Printf("session registry touch: %v", err)
	}

	ad := New(id, DefaultSpoolDir())
	// Authorize prompt capture on the observe/egress path only under the
	// content-capture posture (on by default since 2026-07-15, opt-out
	// honored — this comment said "default off = metadata-only" long after
	// that stopped being true). This is the same flag the flush client's
	// Emit uses to strip content, so capture and egress agree. Resolved once
	// (cheap config+env, no secret I/O).
	ad.Mapper.CaptureContent = ResolveContentCapture()
	// Pin the Mapper clock to one instant for this hook invocation. RunHook
	// maps the PreToolUse event twice in enforce+Tier-2 mode — once here
	// via Observe (the spool copy, flushed on SessionEnd) and once inside
	// escalateTier2 (the synchronous /evaluate copy). A fresh time.Now() on
	// each Map would fold a different RFC3339Nano timestamp into deriveID
	// and yield different event_ids, so the two would not collapse under
	// one Idempotency-Key even after server-side dedupe lands. Pinning
	// makes the T2 send a true idempotency-ready re-send of the spooled
	// event. The offset (hook entry vs mid-Map) is microseconds and
	// semantically irrelevant.
	pinnedNow := time.Now()
	ad.Mapper.Now = func() time.Time { return pinnedNow }

	// On SessionEnd, behind the finops gate (default ON, opt-out), read the
	// session transcript for usage numbers only and hand them to the
	// Mapper, which attaches them to the SessionEnded event. This is the
	// only place transcript_path is opened, and only when ResolveFinops()
	// is set — with finops off it is never dereferenced. SessionEnd is
	// teardown (off the Pre/PostToolUse hot path). Best-effort (INV-3):
	// any error — missing/oversized/malformed transcript — is logged to
	// stderr and skipped; it never fails the flush, blocks, or writes
	// stdout. Only the projection-only parser (usage.go) touches the
	// file, so no content can enter the event (INV-2).
	if hook == HookSessionEnd && ResolveFinops() {
		if tokens, cost, err := readTranscriptUsage(ev.TranscriptPath); err != nil {
			logger.Printf("finops: transcript usage skipped: %v", err)
		} else if tokens != nil || cost != nil {
			ad.Mapper.Finops = &FinopsUsage{Tokens: tokens, Cost: cost}
		}
	}

	// SessionEnd prelude: record how much of this session's telemetry is still
	// sitting in carry-over files, before the final flush runs (E8-S7). Counted
	// here so the SessionEnded event carries it; RunHook does the I/O so Map
	// stays pure.
	if hook == HookSessionEnd {
		ad.Mapper.Evidence = &EvidenceState{Undelivered: ad.Spool.UndeliveredCount()}
	}

	// SessionStart prelude: run the freshness check and resolve the effective
	// posture BEFORE mapping, so both ride the SessionStarted event as evidence
	// (E8-S5). The check is enforce-gated exactly as before — with enforce off
	// it makes no network call and the posture honestly records "not_checked".
	// It is the only SessionStart stdout writer, so running it earlier does not
	// reorder anything the provider sees.
	if hook == HookSessionStart {
		staleness := devconfig.StalenessNotChecked
		if ResolveEnforce() {
			staleness = hookflow.CheckPolicyStaleness(logger, ev.SessionID, stdout)
		}
		posture := effectivePosture(staleness)
		ad.Mapper.Posture = &posture
	}

	// Near-real-time delivery: with the event spooled, nudge a detached,
	// debounced flusher for this session so telemetry reaches core mid-session
	// instead of waiting for SessionEnd (which stays the completeness safety
	// net, so this runs on every other hook). Gated inside Maybe
	// (ResolveRealtime, default on). It costs a lockfile check and, at most
	// once per debounce window, a fork+exec — no network I/O in this process
	// and no wait on the child, so the delay it can add to the PreToolUse gate
	// below is local and bounded (INV-3/INV-3b).
	nudgeFlush := func() {
		hookflow.RealtimeTrigger{Spool: ad.Spool, Provider: "claude-code"}.Maybe(logger, ev.SessionID)
	}

	// Turn boundary (ADR-0014): Stop closes a main-thread turn, SubagentStop a
	// subagent's. Both emit the TurnStarted/TurnCompleted pair for the transcript
	// window that has appeared since this window's cursor last advanced, then
	// return — neither hook observes a lifecycle event of its own (Map returns
	// false for them by design) and neither may ever write stdout, because both
	// can block a session via `decision: "block"`.
	if hook == HookStop || hook == HookSubagentStop {
		if ResolveFinops() {
			emitTurn(ad, logger, hook, ev)
			nudgeFlush()
		}
		return
	}

	// Resolved ONCE and reused by the gate below. The two must agree: deciding
	// to defer the observe copy here and then not running the gate would drop
	// the event.
	gated := hook == HookPreToolUse && ResolveEnforce()

	// The observe copy. On a gated hook it is DEFERRED into the gate below,
	// which spools it only if the Tier-2 escalation did not already deliver the
	// identical event — see EnforceGate.SpoolObserve for why writing it here
	// stored every escalated ActivityStarted twice. The duration stash is still
	// threaded now (RecordDeferred): it has to be written before the tool runs,
	// and suppressing a redundant spool copy must not cost the call its
	// duration_ms.
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
			// fall through — SessionEnd still tries to flush what is already spooled
		}
		if hook != HookSessionEnd {
			nudgeFlush()
		}
	}

	// In enforce mode the PreToolUse hook is a synchronous pre-execution gate:
	// obtain the governance decision before the tool runs (INV-3b — evaluated
	// in-memory, no network or IPC, fail-open on any fault). Default off: with
	// enforce off the decider is never invoked and this is inert, so the observe
	// path stays byte-identical to observe-only. Only PreToolUse gates; every
	// other hook keeps observing.
	//
	// The gate's steps and their order live in hookflow.EnforceGate, shared with
	// every provider. What this adapter supplies is how to read its own hook
	// event (enforceTarget), how it spells a response (contract), and its hook
	// budget (tier2).
	if gated {
		g := hookflow.EnforceGate{
			Contract:     contract,
			Tier2:        tier2,
			Record:       func(dec decision.Decision, res hookflow.ApplyResult) { recordEnforcement(logger, ev, dec, res) },
			SpoolObserve: spoolObserve,
		}
		g.Run(context.Background(), logger, stdout, enforceTarget{id: id, mapper: ad.Mapper, ev: ev})
		return
	}

	// Surface governance findings recorded on the flush path (the
	// advisories.jsonl sink) back into the session as a content-free
	// summary — on UserPromptSubmit (turn-start, since CC discards
	// SessionEnd output) and PostToolUse (near-real-time). Gated behind
	// ResolveFindings (default off): with it off this is inert and both
	// hooks write nothing. It emits only additionalContext (→ model) +
	// systemMessage (→ user), never a blocking field, so it can never
	// block a tool call (INV-3); it surfaces only categories/counts,
	// never content (INV-2); and PostToolUse is stat-guarded. Orthogonal
	// to enforce — findings are advisory feedback in both observe and
	// enforce sessions.
	if hook == HookPostToolUse || hook == HookUserPromptSubmit {
		if ResolveFindings() {
			hookflow.SurfaceFindings("claude-code", string(hook), stdout, logger)
		}
	}

	// Opt-in ambient install: make governance ambient by installing the
	// commit-trailer hook into the repo this session opened.
	if hook == HookSessionStart {
		// The staleness check itself now runs in the prelude above (its
		// outcome is posture evidence); only the git-hook install remains here.
		maybeInstallGitHook(logger, ev.Cwd)
	}

	// SessionEnd delivers the session's spooled events off the hot path.
	if hook == HookSessionEnd {
		runFlush(logger, ev.SessionID)
	}
}

// emitTurn reads the transcript window this turn-boundary hook delimits and
// spools the TurnStarted/TurnCompleted pair for it (ADR-0014).
//
// The step order is the correctness argument, not a style choice:
//
//  1. read the cursor for THIS window — (session, agent), so a subagent's
//     window and the main thread's never consume each other's bytes;
//  2. read the transcript from that offset, taking this side of the sidechain
//     partition only;
//  3. spool both halves;
//  4. advance the cursor — LAST.
//
// A crash between 3 and 4 re-reads one window on the next firing, which re-mints
// the same `<session>:turn:<n>` activity_id; core's dedupe key includes
// activity_id and event_type, so the server returns the cached verdict instead of
// storing a second row. The reverse order would lose a turn's tokens with nothing
// to recover them from. Over-report into a server that deduplicates; never
// under-report into nothing.
//
// Best-effort throughout (INV-3): every fault is logged to stderr and swallowed.
// It writes nothing to stdout on any path — Stop and SubagentStop can block a
// session, so a stray write is a defect, not a degradation.
func emitTurn(ad *Adapter, logger *log.Logger, hook HookName, ev *HookEvent) {
	// Subagent turns are keyed and partitioned by agent. A SubagentStop without
	// an agent_id (older Claude Code, or a payload that omitted it) would
	// otherwise share the main thread's cursor and consume its window: skip it
	// rather than corrupt the main thread's accounting.
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
		// A firing with no new usage is not a turn. Still advance the cursor over
		// the bytes that were consumed, so the same lines are not re-scanned on
		// every subsequent firing for the rest of the session.
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
	// Spool both halves before the cursor moves. If the second append fails the
	// cursor stays put and the whole window is re-read next time — the pair is
	// re-derived with the same ids and the server absorbs the duplicate.
	for _, turnEv := range []client.DevEvent{started, completed} {
		if err := ad.Record(turnEv); err != nil {
			logger.Printf("finops: spool %s event: %v", turnEv.EventType, err)
			return
		}
	}

	next.Index = pos.Index + 1
	if err := ad.Turns.Write(ev.SessionID, agentID, next); err != nil {
		// The events are spooled; only the cursor is behind. The next firing
		// re-reads this window and re-mints the same ids, which core dedupes.
		logger.Printf("finops: turn cursor write failed (window may be re-read): %v", err)
	}
}

// maybeInstallGitHook installs the prepare-commit-msg hook into the repo
// at cwd, pointing it back at this unified engine as `openbox hook git
// prepare-commit-msg` (OD17 — the git hook is folded in, so no separate
// openbox-git-hook binary need be bundled). It is gated behind
// OPENBOX_INSTALL_GIT_HOOK (default off), a no-op outside a git repo,
// idempotent, and refuses to overwrite a foreign hook (InstallHook).
//
// Assumption: os.Executable() is the unified `openbox` engine (which
// handles the baked `hook git prepare-commit-msg` args). In production it
// is — the plugin wires SessionStart to `bin/openbox hook claude-code
// SessionStart`. If this ever runs under the legacy openbox-cc-hook alias
// (which cannot parse `hook git …`), the installed hook fail-opens to a
// no-op (commit proceeds, unstamped) rather than aborting the commit — an
// acceptable degradation for a deprecated path.
func maybeInstallGitHook(logger *log.Logger, cwd string) {
	if !ResolveInstallGitHook() {
		return
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		return
	}
	// The ambient (implicit) install is bounded to the repo's own
	// <git-common-dir>/hooks and does not follow a repo-controlled
	// core.hooksPath, which a malicious repo could point outside the
	// tree. The explicit `openbox hook git install` command honors
	// core.hooksPath.
	hooksDir, err := obgit.Git{Dir: cwd}.HooksDirDefault()
	if err != nil {
		return // not a git repo / detached worktree — nothing to install into
	}
	cfg := obgit.HookConfig{Command: self, Args: []string{"hook", "git", "prepare-commit-msg"}}
	// The post-commit hook carries the notes mirror and the signed attestation
	// (E8-S10). Additive and best-effort: the trailer works without it.
	if err := obgit.InstallPostCommitHook(hooksDir, cfg); err != nil {
		logger.Printf("post-commit hook not installed (trailer still works): %v", err)
	}
	if err := obgit.InstallHook(hooksDir, cfg); err != nil {
		logger.Printf("git-hook install skipped: %v", err)
	}
}

// runFlush drains the spool through the AIP-signed client. A missing/invalid
// full credential set leaves events spooled for a later `flush` (fail-open).
// sessionID=="" flushes every spooled session.
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
	// Advisory-tier recording: route the per-record stderr summary through
	// the hook's logger. Diagnostics only — stderr, never stdout, so a
	// SessionStart/UserPromptSubmit exit-0 hook still injects nothing
	// (INV-3).
	ad.Advisory.Log = logger
	var n int
	if sessionID == "" {
		n, err = ad.FlushAll(ctx, cl)
	} else {
		// Hold the session's realtime debounce lock for the whole drain and
		// release it after, so the next spooled event can trigger a fresh
		// flusher immediately. Also runs on SessionEnd, clearing the session's
		// last marker.
		ad.Spool.TouchFlushLock(sessionID)
		defer ad.Spool.ReleaseFlushLock(sessionID)
		n, err = ad.Flush(ctx, sessionID, cl)
	}
	if err != nil {
		logger.Printf("flush ended early after %d event(s): %v", n, err)
	}
}
