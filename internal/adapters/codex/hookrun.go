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

// flushBudget bounds SessionEnd/flush delivery so session teardown is never
// held up. Kept under the 15 s SessionEnd hook timeout the installer writes
// (Codex's own default is 600 s, but it clamps SessionEnd hook timeouts —
// addendum #8 — so we pin an explicit small one and stay under it, mirroring
// the CC adapter's 12-under-15 discipline).
const flushBudget = 12 * time.Second

// RunHook executes the path for one Codex hook invocation — the engine
// behind the unified `openbox hook codex <event>` subcommand.
//
// Safety contract (INV-3 — observe-only, never block; the default
// whole-product posture):
//   - In observe mode (the default) it writes nothing to stdout. Codex
//     parses an exit-0 hook's stdout as hook output JSON
//     (hookSpecificOutput/decision/…), so any stray byte could inject
//     context or trip the output parser — all diagnostics go to `logger`
//     (stderr) only, terse and secret-free (INV-1).
//   - It never returns a blocking signal in observe mode: any failure (bad
//     payload, missing identity, unreachable OpenBox, even a panic) is
//     logged and swallowed. The caller must exit 0 regardless.
//
// Enforce mode (opt-in via ResolveEnforce, default off) is the sole
// exception to "nothing to stdout": on a PreToolUse hook it may write a
// Codex permissionDecision (deny, or allow+updatedInput for a redaction)
// to `stdout` — the INV-3b carve-out. It still only ever tightens (deny /
// content-stripping rewrite, never a grant) and still exits 0, so a
// non-blocking verdict is byte-identical to observe mode. Every other
// hook, and observe mode, write nothing to stdout (except the
// findings additionalContext/systemMessage).
//
// `sub` is the hook name (SessionStart/UserPromptSubmit/PreToolUse/PostToolUse/
// SessionEnd) or "flush"; `stdin` carries the hook payload JSON; `stdout` is where
// the enforce apply writes the permissionDecision (unused in observe mode).
func RunHook(sub string, stdin io.Reader, stdout io.Writer, logger *log.Logger) {
	// Freeze config reads for this hook run: everything the gate decides —
	// enforce, the failure policy — must come from one version of
	// dev.json, not one read per flag.
	defer devconfig.Pin()()
	// Guarantee a panic never escapes into the caller's exit path.
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

	// Deliberate delta from the CC adapter: there is no session liveness
	// registry here. Codex injects CODEX_THREAD_ID into every tool/shell
	// exec environment, so a Codex-run `git commit` is attributed directly
	// by the prepare-commit-msg hook's env read — no registry needed for
	// agent-made commits. Commits typed in the user's own terminal are a
	// deferred non-goal.

	ad := New(id, DefaultSpoolDir())
	// Authorize prompt capture on the observe/egress path only under the
	// content-capture posture (on by default, opt-out honored) — the same
	// flag the flush client's Emit uses to strip content, so capture and
	// egress agree. Resolved once (cheap config+env, no secret I/O).
	ad.Mapper.CaptureContent = ResolveContentCapture()
	// The current thread's id, which the hook payload does not carry. Recorded
	// only when it differs from the payload's session id, i.e. under a forked
	// thread (E8-S4) — the Mapper decides, this just supplies the ambient
	// value so Map stays I/O-free.
	ad.Mapper.ThreadID = os.Getenv(obgit.EnvCodexThreadID)
	// Pin the Mapper clock to one instant for this hook invocation. In
	// on a gated call RunHook maps the PreToolUse event twice — once
	// here via Observe (the spooled copy, flushed on SessionEnd) and once
	// inside escalateEvaluation → runTier2 (the synchronous /evaluate copy). A
	// fresh time.Now() on each Map would fold a different RFC3339Nano
	// timestamp into deriveID and yield different event_ids, so the two
	// would not collapse under one Idempotency-Key server-side
	// (double-count). Pinning makes the T2 send a true idempotency-ready
	// re-send of the spooled event. The offset (hook entry vs mid-Map) is
	// microseconds and semantically irrelevant.
	pinnedNow := time.Now()
	ad.Mapper.Now = func() time.Time { return pinnedNow }

	// On SessionEnd, behind the finops gate (default ON, opt-out), read the
	// session's rollout JSONL for usage numbers only and hand them to the
	// Mapper, which attaches them to the SessionEnded event. This is the
	// only place transcript_path is opened, and only when
	// ResolveFinops() is set — with finops off it is never dereferenced.
	// SessionEnd is teardown (off the Pre/PostToolUse hot path), and
	// Codex flushes the transcript before the SessionEnd hook runs, so
	// the counts are complete. Best-effort (INV-3): any error —
	// missing/null/oversized/malformed rollout — is logged to stderr and
	// skipped; it never fails the flush, blocks, or writes stdout. Only
	// the projection-only parser (usage.go) touches the file, so no
	// content can enter the event (INV-2). When transcript_path is
	// absent/null the read simply errors and is skipped — the adapter
	// does not reconstruct a ~/.codex/sessions path (a real SessionEnd
	// always carries transcript_path — session_end.rs @ rust-v0.145.0 —
	// and a HOME-derived scan would fight the read-only/hermeticity
	// posture).
	//
	// Since ADR-0014 the same read also feeds the session-rollup `llm_completion`
	// activity pair (emitted after the SessionEnded event is spooled, below) and
	// binds the model id from `turn_context.payload.model`. Cost is not read at
	// all: Codex's token path carries no cost field, and cost is never derived
	// from a pricing table here.
	if hook == HookSessionEnd && ResolveFinops() {
		if tokens, model, err := readRolloutUsage(ev.TranscriptPath); err != nil {
			logger.Printf("finops: rollout usage skipped: %v", err)
		} else if tokens != nil {
			ad.Mapper.Finops = &FinopsUsage{Tokens: tokens, Model: model}
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
		posture := effectivePosture()
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
		hookflow.RealtimeTrigger{Spool: ad.Spool, Provider: "codex"}.Maybe(logger, ev.SessionID)
	}

	// Resolved ONCE and reused by the gate below. The two must agree: deciding
	// to defer the observe copy here and then not running the gate would drop
	// the event.
	gated := hook == HookPreToolUse && ResolveEnforce()

	// The observe copy. On a gated hook it is DEFERRED into the gate below,
	// which spools it only if the inline evaluation did not already deliver the
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
	// budget (the evaluation).
	if gated {
		g := hookflow.EnforceGate{
			Contract:     contract,
			Evaluator:    evaluator,
			Record:       func(dec decision.Decision, res hookflow.ApplyResult) { recordEnforcement(logger, ev, dec, res) },
			SpoolObserve: spoolObserve,
		}
		g.Run(context.Background(), logger, stdout, enforceTarget{id: id, mapper: ad.Mapper, ev: ev})
		return
	}

	// Surface governance findings recorded on the flush path (the
	// advisories.jsonl sink) back into the session as a content-free
	// summary — on UserPromptSubmit (turn-start) and PostToolUse
	// (near-real-time), via additionalContext (→ model) + systemMessage
	// (→ user); Codex accepts additionalContext on both events. Gated
	// behind ResolveFindings (default off): with it off this is inert and
	// both hooks write nothing. Never a blocking field (INV-3);
	// categories/counts only (INV-2); PostToolUse is stat-guarded.
	// Orthogonal to enforce.
	if hook == HookPostToolUse || hook == HookUserPromptSubmit {
		if ResolveFindings() {
			hookflow.SurfaceFindings("codex", string(hook), stdout, logger)
		}
	}

	// Opt-in ambient install: make commit attribution ambient by
	// installing the commit-trailer hook into the repo this session
	// opened.
	if hook == HookSessionStart {
		// The staleness check itself now runs in the prelude above (its
		// outcome is posture evidence); only the git-hook install remains here.
		maybeInstallGitHook(logger, ev.Cwd)
	}

	// SessionEnd additionally emits the session-rollup `llm_completion` activity
	// pair (ADR-0014) — Codex's granularity for the model+usage signal, since its
	// per-turn Stop hook is deliberately unwired. Spooled BEFORE the flush below,
	// so the pair rides the same drain as the SessionEnded event rather than
	// waiting for a later session's flush. Best-effort: a spool failure is logged
	// and the flush proceeds with whatever is there.
	//
	// It rides the same Finops read that populated the SessionEnded rollup, so the
	// two agree by construction; nothing is read twice.
	if hook == HookSessionEnd {
		if started, completed, ok := ad.Mapper.MapUsageRollup(ev); ok {
			for _, usageEv := range []client.DevEvent{started, completed} {
				if err := ad.Record(usageEv); err != nil {
					logger.Printf("finops: spool %s event: %v", usageEv.EventType, err)
					break
				}
			}
		}
	}

	// SessionEnd delivers the session's spooled events off the hot path.
	// Codex flushes the transcript before running SessionEnd hooks, so
	// teardown ordering is safe.
	if hook == HookSessionEnd {
		runFlush(logger, ev.SessionID)
	}
}

// maybeInstallGitHook installs the prepare-commit-msg hook into the repo
// at cwd, pointing it back at this unified engine as `openbox hook git
// prepare-commit-msg` (the OD17 fold — no separate git-hook binary).
// Gated behind the install_git_hook posture (default off), a no-op
// outside a git repo, idempotent, and it refuses to overwrite a foreign
// hook (obgit.InstallHook). The installed hook stamps `OpenBox-Session:`
// from CODEX_THREAD_ID (highest precedence — see internal/adapters/common/git).
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
	// core.hooksPath. The explicit `openbox hook git install` honors it.
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
	// Advisory-tier recording (CC parity): route the per-record stderr
	// summary through the hook's logger. Diagnostics only — stderr, never
	// stdout.
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
