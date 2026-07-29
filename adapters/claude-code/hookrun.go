package claudecode

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
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
	// content-capture opt-in (default off = metadata-only, INV-2). This is
	// the same flag the flush client's Emit uses to strip content, so
	// capture and egress agree. Resolved once (cheap config+env, no secret
	// I/O).
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

	// On SessionEnd, behind the off-by-default finops opt-in, read the
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
			staleness = checkPolicyStaleness(logger, ev.SessionID, stdout)
		}
		posture := effectivePosture(staleness)
		ad.Mapper.Posture = &posture
	}

	if _, err := ad.Observe(hook, ev); err != nil {
		logger.Printf("spool %s event: %v", hook, err)
		// fall through — SessionEnd still tries to flush what is already spooled
	}

	// In enforce mode the PreToolUse hook is a synchronous pre-execution
	// gate — obtain the governance decision from the in-process decider
	// before the tool runs (INV-3b: evaluated in-memory, no network/IPC,
	// fail-open on any fault). Default off (observe): with enforce off
	// ResolveEnforce is false, the decider is never invoked, and this
	// block is inert — so the observe/advisory path is byte-identical to
	// observe-only. Only PreToolUse gates (the pre-execution concept);
	// other hooks keep observing.
	//
	// EnforceDecision obtains the decision (bounded, fail-open); the apply
	// path applies it — mapping the verdict onto a Claude Code `deny`/`ask`
	// permissionDecision on stdout, the moment WouldBlock() becomes a real
	// block. applyDecision emits only deny/ask (tighten-only); a
	// CONSTRAIN/ALLOW/UNKNOWN(fail-open) verdict writes nothing and the
	// tool proceeds via Claude Code's own permission flow —
	// byte-identical to observe mode. The durable enforcement record runs
	// after the stdout decision, off the blocking path, best-effort
	// (never blocks — INV-3).
	if hook == HookPreToolUse && ResolveEnforce() {
		// The whole enforce gate (T1 + a possible T2) is bounded by one
		// wall clock so the sequential per-tier budgets can never jointly
		// exceed CC's 5s hook timeout (see maxEnforceHookBudget /
		// tier2Budget).
		enforceStart := time.Now()
		// The fail-closed session-start staleness block is realized here
		// (CC has no SessionStart "deny session" primitive). If this
		// session was marked stale under fail-closed, deny every tool call
		// — reusing the unchanged apply cascade (a synthesized HALT →
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
		// (default off) is on. Resolved once here (cheap config+env, no
		// secret I/O). The body + redaction are local-only (never
		// egressed — INV-2); the observe Mapper egress path stays
		// metadata-only unless content capture is on. With both off,
		// Content stays nil and no redaction is emitted — byte-identical
		// to the baseline.
		localRedaction := ResolveSecretDetection() || ResolveContentCapture()
		dec := EnforceDecision(context.Background(), newDecider(), id, ev, localRedaction)
		// Apply the per-org failure policy (fail-open default / opt-in
		// fail-closed). On an evaluation outage (dec.FailOpen) under
		// fail-closed this synthesizes a HALT so the unchanged apply
		// cascade denies; otherwise the decision passes through untouched
		// (fail-open → proceed; a real verdict is never overridden). The
		// mapVerdict/applyDecision cascade stays policy-agnostic.
		policy := resolveFailurePolicy()
		dec = applyFailurePolicy(dec, policy)
		// For high-risk classes (Bash / MCP execution), when Tier-2 is
		// enabled and T1 did not already tighten (deny/ask), escalate to
		// the authoritative full server verdict via a synchronous
		// /evaluate before the tool runs — closing the policy-only floor
		// exactly where arbitrary execution is dangerous, and nowhere
		// else (frequent edits stay T1-only, no /evaluate latency). The T2
		// budget is owned in-binary (ResolveTier2Timeout, clamped under
		// CC's 5s hook timeout, which fails open), and the T2 decision
		// runs the same failure policy (fail-open proceed / fail-closed
		// deny on a T2 outage) + apply cascade. Default off: with T2 off
		// this is inert and the T1-only path is byte-identical to the
		// T1-only baseline.
		if ResolveTier2() && isHighRiskClass(ev.ToolName) && !decisionTightens(dec) {
			t2 := escalateTier2(context.Background(), logger, ad.Mapper, ev, tier2Budget(enforceStart))
			// Carry the local T1 secret redaction onto the T2 decision so
			// a redact-and-continue still applies on the T2 proceed path.
			// (High-risk classes are non-file today, so T1 carries no
			// RedactedContent — this is defensive: it keeps the local
			// redaction orthogonal to the T2 server verdict if the class
			// sets ever overlap. Redaction is applied on proceed only.)
			t2.RedactedContent = dec.RedactedContent
			t2.RedactionCategories = dec.RedactionCategories
			dec = applyFailurePolicy(t2, policy)
		}
		logEnforceDecision(logger, ev, dec, policy)
		// Apply (deny/ask) + updatedInput redaction on the proceed path,
		// gated on localRedaction. origInput = the raw tool_input,
		// reconstructed with only the content field redacted (never
		// egressed).
		applied, _ := applyDecision(stdout, dec, localRedaction, ev.ToolInput)
		recordEnforcement(logger, ev, dec, applied)
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
			surfaceFindings(hook, stdout, logger)
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
		n, err = ad.Flush(ctx, sessionID, cl)
	}
	if err != nil {
		logger.Printf("flush ended early after %d event(s): %v", n, err)
	}
}
