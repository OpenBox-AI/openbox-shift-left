package claudecode

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"
)

// flushBudget bounds SessionEnd/flush delivery so session teardown is never held
// up (kept under the SessionEnd hook timeout in plugin/hooks.json).
const flushBudget = 12 * time.Second

// RunHook executes the observe-only path for ONE Claude Code hook invocation.
// It is the single engine shared by the unified `openbox hook claude-code
// <event>` subcommand (STORY-SL4-WIRE-2) and the retired-to-alias
// openbox-cc-hook binary.
//
// SAFETY CONTRACT (INV-3 / D7 — observe-only, never block):
//   - In OBSERVE mode (the default) it writes NOTHING to stdout. On SessionStart/
//     UserPromptSubmit, stdout from an exit-0 hook is injected into the model's
//     context — so all diagnostics go to `logger` (stderr) only, terse and
//     secret-free (INV-1).
//   - It NEVER returns a blocking signal in observe mode: any failure (bad
//     payload, missing identity, unreachable OpenBox, even a panic) is logged and
//     swallowed. The CALLER must exit 0 regardless.
//
// ENFORCE mode (E6, opt-in via ResolveEnforce, default OFF) is the sole exception
// to "nothing to stdout": on a PreToolUse hook it may write a Claude Code
// permissionDecision (deny/ask) to `stdout` — the INV-3b carve-out. It still only
// ever TIGHTENS (deny/ask, never allow) and still exits 0, so a non-blocking
// verdict is byte-identical to observe mode. Every other hook, and observe mode,
// write nothing to stdout.
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

	// STORY-SL-5: maintain this session's liveness record so the git
	// prepare-commit-msg hook can attribute a commit to the session that made it
	// (parallel-safe, worktree-scoped). Structural fields only (session_id +
	// cwd), never content (INV-2). Best-effort: a failure is logged, never
	// surfaced.
	regDir := obgit.DefaultSessionDir()
	if hook == HookSessionEnd {
		if err := obgit.RemoveSessionRecord(regDir, ev.SessionID); err != nil {
			logger.Printf("session registry cleanup: %v", err)
		}
	} else if err := obgit.WriteSessionRecord(regDir, ev.SessionID, ev.Cwd, time.Now()); err != nil {
		logger.Printf("session registry touch: %v", err)
	}

	ad := New(id, DefaultSpoolDir())

	// STORY-SL-16 (OD-FINOPS): on SessionEnd, behind the off-by-default finops
	// opt-in, read the session transcript for usage NUMBERS ONLY and hand them to
	// the Mapper, which attaches them to the SessionEnded event. This is the ONLY
	// place transcript_path is opened, and ONLY when ResolveFinops() is set — with
	// finops off it is never dereferenced (byte-identical to pre-SL-16 output).
	// SessionEnd is teardown (off the Pre/PostToolUse hot path, NFR-2). Best-effort
	// (INV-3): any error — missing/oversized/malformed transcript — is logged to
	// stderr and skipped; it never fails the flush, blocks, or writes stdout. Only
	// the projection-only parser (usage.go) touches the file, so no content can
	// enter the event (INV-2).
	if hook == HookSessionEnd && ResolveFinops() {
		if tokens, cost, err := readTranscriptUsage(ev.TranscriptPath); err != nil {
			logger.Printf("finops: transcript usage skipped: %v", err)
		} else if tokens != nil || cost != nil {
			ad.Mapper.Finops = &FinopsUsage{Tokens: tokens, Cost: cost}
		}
	}

	if _, err := ad.Observe(hook, ev); err != nil {
		logger.Printf("spool %s event: %v", hook, err)
		// fall through — SessionEnd still tries to flush what is already spooled
	}

	// STORY-E6-S1 (Phase-2 enforcement): in ENFORCE mode the PreToolUse hook is a
	// SYNCHRONOUS pre-execution gate — obtain the governance decision from the
	// LOCAL sidecar BEFORE the tool runs (INV-3b: bounded by the Client's hard
	// ~50ms timeout, fail-open on any fault). Default OFF (observe): with enforce
	// off ResolveEnforce is false, the sidecar is NEVER dialed, and this block is
	// inert — so the observe/advisory path is byte-identical to Phase-1 (AC-4).
	// Only PreToolUse gates (the pre-execution concept); other hooks keep observing.
	//
	// E6-S1 OBTAINS the decision (bounded, fail-open); E6-S2 APPLIES it — mapping
	// the verdict onto a Claude Code `deny`/`ask` permissionDecision on stdout, the
	// moment WouldBlock() becomes a real block. applyDecision emits ONLY deny/ask
	// (tighten-only); a CONSTRAIN/ALLOW/UNKNOWN(fail-open) verdict writes nothing
	// and the tool proceeds via Claude Code's own permission flow — byte-identical
	// to observe mode. The durable enforcement record runs AFTER the stdout
	// decision, off the blocking path, best-effort (never blocks — INV-3).
	if hook == HookPreToolUse && ResolveEnforce() {
		dec := EnforceDecision(context.Background(), newSidecarClient(), id, ev)
		// STORY-E6-S3: apply the per-org FAILURE POLICY (fail-open default / opt-in
		// fail-closed, OD9). On an evaluation outage (dec.FailOpen) under fail-closed
		// this synthesizes a HALT so the unchanged apply cascade denies; otherwise the
		// decision passes through untouched (fail-open → proceed; a real verdict is
		// never overridden). The mapVerdict/applyDecision cascade stays policy-agnostic.
		policy := resolveFailurePolicy()
		dec = applyFailurePolicy(dec, policy)
		logEnforceDecision(logger, ev, dec, policy)
		applied, _ := applyDecision(stdout, dec)
		recordEnforcement(logger, ev, dec, applied)
	}

	// Opt-in ambient install (STORY-SL-5 wiring): make governance ambient by
	// installing the commit-trailer hook into the repo this session opened.
	if hook == HookSessionStart {
		maybeInstallGitHook(logger, ev.Cwd)
	}

	// SessionEnd delivers the session's spooled events off the hot path.
	if hook == HookSessionEnd {
		runFlush(logger, ev.SessionID)
	}
}

// maybeInstallGitHook installs the prepare-commit-msg hook into the repo at cwd,
// pointing it back at THIS unified engine as `openbox hook git prepare-commit-msg`
// (STORY-SL4-WIRE-2 / OD17 — the git hook is folded in, so no separate
// openbox-git-hook binary need be bundled). It is gated behind
// OPENBOX_INSTALL_GIT_HOOK (default off), a no-op outside a git repo, idempotent,
// and refuses to overwrite a foreign hook (InstallHook).
//
// ASSUMPTION: os.Executable() is the unified `openbox` engine (which handles the
// baked `hook git prepare-commit-msg` args). In production it is — the plugin
// wires SessionStart to `bin/openbox hook claude-code SessionStart`. If this ever
// runs under the legacy openbox-cc-hook alias (which cannot parse `hook git …`),
// the installed hook fail-opens to a no-op (commit proceeds, unstamped) rather
// than aborting the commit — an acceptable degradation for a deprecated path.
func maybeInstallGitHook(logger *log.Logger, cwd string) {
	if !ResolveInstallGitHook() {
		return
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		return
	}
	// SL5-SEC-2: the ambient (implicit) install is bounded to the repo's own
	// <git-common-dir>/hooks and does NOT follow a repo-controlled core.hooksPath,
	// which a malicious repo could point outside the tree. The explicit
	// `openbox hook git install` command honors core.hooksPath.
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
	// Advisory-tier recording (STORY-SL-9): route the per-record stderr summary
	// through the hook's logger. Diagnostics only — stderr, never stdout, so a
	// SessionStart/UserPromptSubmit exit-0 hook still injects nothing (INV-3).
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
