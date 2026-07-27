package codex

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"
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
// hook, and observe mode, write nothing to stdout (except the Tier-3
// findings additionalContext/systemMessage).
//
// `sub` is the hook name (SessionStart/UserPromptSubmit/PreToolUse/PostToolUse/
// SessionEnd) or "flush"; `stdin` carries the hook payload JSON; `stdout` is where
// the enforce apply writes the permissionDecision (unused in observe mode).
func RunHook(sub string, stdin io.Reader, stdout io.Writer, logger *log.Logger) {
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
		runFlush(logger, "")
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
	// Pin the Mapper clock to one instant for this hook invocation. In
	// enforce+Tier-2 mode RunHook maps the PreToolUse event twice — once
	// here via Observe (the spooled copy, flushed on SessionEnd) and once
	// inside escalateTier2 → runTier2 (the synchronous /evaluate copy). A
	// fresh time.Now() on each Map would fold a different RFC3339Nano
	// timestamp into deriveID and yield different event_ids, so the two
	// would not collapse under one Idempotency-Key server-side
	// (double-count). Pinning makes the T2 send a true idempotency-ready
	// re-send of the spooled event. The offset (hook entry vs mid-Map) is
	// microseconds and semantically irrelevant.
	pinnedNow := time.Now()
	ad.Mapper.Now = func() time.Time { return pinnedNow }

	// On SessionEnd, behind the off-by-default finops opt-in, read the
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
	if hook == HookSessionEnd && ResolveFinops() {
		if tokens, cost, err := readRolloutUsage(ev.TranscriptPath); err != nil {
			logger.Printf("finops: rollout usage skipped: %v", err)
		} else if tokens != nil || cost != nil {
			ad.Mapper.Finops = &FinopsUsage{Tokens: tokens, Cost: cost}
		}
	}

	if _, err := ad.Observe(hook, ev); err != nil {
		logger.Printf("spool %s event: %v", hook, err)
		// fall through — SessionEnd still tries to flush what is already spooled
	}

	// In enforce mode the PreToolUse hook is a synchronous pre-execution
	// gate — obtain the governance decision from the in-process decider
	// before the tool runs (INV-3b: in-memory, no network/IPC, fail-open
	// on any fault). Default off (observe): with enforce off
	// ResolveEnforce is false, the decider is never invoked, and this
	// block is inert — so the observe/advisory path is byte-identical.
	// Only PreToolUse gates.
	if hook == HookPreToolUse && ResolveEnforce() {
		// The whole enforce gate (T1 + a possible T2) is bounded by one
		// wall clock so the sequential per-tier budgets can never jointly
		// push the hook past Codex's kill (Codex fails open on a
		// hook-timeout — see maxEnforceHookBudget / tier2Budget).
		enforceStart := time.Now()
		// The fail-closed session-start staleness block is realized here
		// (Codex, like CC, has no SessionStart "deny session" primitive).
		// A stale-marked fail-closed session denies every tool call —
		// reusing the unchanged apply cascade (a synthesized HALT →
		// mapVerdict deny) — until `openbox dev sync` clears the marker.
		// The check is a local file stat (network-free — INV-3b).
		if dec, blocked := staleGateDecision(ev.SessionID); blocked {
			logEnforceDecision(logger, ev, dec, resolveFailurePolicy())
			applied, _ := applyDecision(stdout, dec, false, ev.ToolInput)
			recordEnforcement(logger, ev, dec, applied)
			return
		}
		// Local redaction gate: hand the tool body to the local decider
		// and apply any content-only redaction it returns. Enabled when
		// either Tier-1 secret detection (default on) or content capture
		// (default on) is on. The body + redaction are local-only (never
		// egressed — INV-2); the observe Mapper egress path stays
		// metadata-only unless content capture is on. With both off,
		// Content stays nil and no redaction is emitted.
		localRedaction := ResolveSecretDetection() || ResolveContentCapture()
		dec := EnforceDecision(context.Background(), newDecider(), id, ev, localRedaction)
		// Apply the per-org failure policy (fail-open default / opt-in
		// fail-closed). On an evaluation outage (dec.FailOpen) under
		// fail-closed this synthesizes a HALT so the unchanged apply
		// cascade denies; otherwise the decision passes through
		// untouched.
		policy := resolveFailurePolicy()
		dec = applyFailurePolicy(dec, policy)
		// For high-risk classes (Bash / MCP execution), when Tier-2 is
		// enabled and T1 did not already tighten, escalate to the
		// authoritative full server verdict via a synchronous /evaluate
		// before the tool runs. The T2 budget is owned in-binary
		// (clamped under Codex's hook kill), and the T2 decision runs the
		// same failure policy + apply cascade. Default off: with T2 off
		// this is inert and the T1-only path is byte-identical.
		if ResolveTier2() && isHighRiskClass(ev.ToolName) && !decisionTightens(dec) {
			t2 := escalateTier2(context.Background(), logger, ad.Mapper, ev, tier2Budget(enforceStart))
			// Carry the local T1 secret redaction onto the T2 decision so
			// a redact-and-continue still applies on the T2 proceed path.
			// (High-risk classes are non-file today, so T1 carries no
			// RedactedContent — defensive.)
			t2.RedactedContent = dec.RedactedContent
			t2.RedactionCategories = dec.RedactionCategories
			dec = applyFailurePolicy(t2, policy)
		}
		logEnforceDecision(logger, ev, dec, policy)
		// Apply (deny) + allow+updatedInput redaction on the proceed
		// path, gated on localRedaction. origInput = the raw tool_input,
		// reconstructed with only the "command" field redacted (never
		// egressed).
		applied, _ := applyDecision(stdout, dec, localRedaction, ev.ToolInput)
		recordEnforcement(logger, ev, dec, applied)
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
			surfaceFindings(hook, stdout, logger)
		}
	}

	// Opt-in ambient install: make commit attribution ambient by
	// installing the commit-trailer hook into the repo this session
	// opened.
	if hook == HookSessionStart {
		// Best-effort session-start policy staleness check.
		// Enforce-gated so the observe path stays byte-identical (no
		// network, no SessionStart stdout when enforce is off). Off the
		// tool hot path; fully fail-safe — it warns (fail-open) or marks
		// the session stale (fail-closed) but never blocks the session.
		if ResolveEnforce() {
			checkPolicyStaleness(logger, ev.SessionID, stdout)
		}
		maybeInstallGitHook(logger, ev.Cwd)
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
// from CODEX_THREAD_ID (highest precedence — see adapters/common/git).
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
		n, err = ad.Flush(ctx, sessionID, cl)
	}
	if err != nil {
		logger.Printf("flush ended early after %d event(s): %v", n, err)
	}
}
