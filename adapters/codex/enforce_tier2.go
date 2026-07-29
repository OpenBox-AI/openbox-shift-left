package codex

import (
	"context"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Tier-2 synchronous /evaluate escalation for the Codex adapter. Byte-for-
// byte the same tiering discipline as Claude Code — only the timeout
// derivation is Codex-shaped.
//
//   - Tier 1 — the local in-process decider (enforce.go): microseconds,
//     the only tier allowed to block frequent low-risk edits; a
//     policy/OPA floor (Guardrail PII/NSFW + drift are model-backed and
//     server-side, so a local verdict can be more permissive than core,
//     never stricter than a real server BLOCK).
//   - Tier 2 — a synchronous /evaluate call (this file): for high-risk
//     classes only (Bash / MCP execution), after T1 allows, obtain the
//     authoritative full server verdict before the tool runs. Frequent
//     edits (apply_patch) never pay the /evaluate latency. T2 can only
//     tighten a T1 allow.
//
// Failure semantics reuse the T1 failure policy verbatim: a T2 call
// yielding no real verdict (transport failure, timeout, empty/unmapped
// response) becomes a FailOpen decision, so the unchanged
// applyFailurePolicy proceeds (fail-open) or synthesizes a HALT
// (fail-closed) exactly as for a T1 outage.
//
// Timeout is owned in-binary. Codex fails open when a PreToolUse hook
// overruns its `timeout`, so a T2 wait that pushed the hook past the
// installed timeout would silently defeat a fail-closed org — the exact
// hazard this tier exists to prevent. The T2 budget is therefore clamped
// strictly under maxEnforceHookBudget (enforce.go — itself derived from
// the installed hot-hook timeout, not copied from Claude Code's 5s
// constant). Codex's 600s default hook timeout gives far more headroom
// than Claude Code, but the default budget stays conservative until
// there's reason to raise it.

const (
	// bashToolName is Codex's hook identity for shell-like execution
	// (classifyTool grounding: HookToolName::bash() is serialized for
	// shell_command/unified_exec/exec paths). The genuinely high-risk
	// shell tool, matched by exact name.
	bashToolName = "Bash"

	// sourceTier2 marks a decision from the Tier-2 sync /evaluate (a real
	// server verdict); sourceTier2FailOpen marks a T2 call that yielded
	// no real verdict (outage/unknown) — the failure-policy input. Both
	// content-free (INV-1/INV-2).
	sourceTier2         = "tier2:evaluate"
	sourceTier2FailOpen = "tier2:fail-open"
)

// defaultTier2Timeout is the default in-binary budget for one synchronous
// /evaluate escalation. /evaluate measures at ~0.8-1.6s, so 3.5s covers
// one round-trip plus the client's bounded retries while staying under
// maxTier2Timeout. Kept at the Claude Code value ("T2 ≤ the CC value"
// until there's reason for more) — Codex's larger hook budget is
// available headroom, not a reason to widen the default.
const defaultTier2Timeout = 3500 * time.Millisecond

// maxTier2Timeout caps the configurable T2 budget. Derived (not copied):
// it is the whole-hook wall-clock ceiling (maxEnforceHookBudget,
// enforce.go — installed hot-hook timeout minus margin), because the
// entire PreToolUse hook (config reads + T1 gate + T2 round-trip + apply
// + audit) must return before Codex kills the hook and fails open. If an
// org raises the installed hot-hook timeout, this rises with it
// automatically.
const maxTier2Timeout = maxEnforceHookBudget

// tier2Budget is the effective budget for the T2 escalation: the configured budget,
// never more than the time REMAINING in maxEnforceHookBudget after the T1 gate ran.
// enforceStart is the instant the enforce block began. A non-positive remainder
// yields a non-positive budget, so escalateTier2 fail-opens IMMEDIATELY (the safe
// direction) rather than push the hook past Codex's kill.
func tier2Budget(enforceStart time.Time) time.Duration {
	budget := ResolveTier2Timeout()
	if rem := maxEnforceHookBudget - time.Since(enforceStart); rem < budget {
		budget = rem
	}
	return budget
}

// isHighRiskClass reports whether a tool is a Tier-2 high-risk class —
// arbitrary execution — that warrants escalating a T1 allow to the
// authoritative server verdict: the Bash tool (arbitrary shell) or any
// MCP tool call. Frequent low-risk classes (apply_patch file edits) and
// the other shell-catch-all tools (web_search/update_plan/…) are T1-only,
// so they never pay the /evaluate latency.
func isHighRiskClass(toolName string) bool {
	if toolName == bashToolName {
		return true
	}
	kind, _, _, _, _ := classifyTool(toolName)
	return kind == client.ToolMCP
}

// decisionTightens reports whether a decision already yields a Codex deny (via the
// unchanged mapVerdict cascade). When T1 already tightens — a real BLOCK/HALT/
// guardrail-fail/REQUIRE_APPROVAL, or a fail-closed synthesized HALT — there is
// nothing for T2 to add, so the escalation is skipped and the T1 decision stands.
// T2 fires ONLY when T1 would otherwise PROCEED.
func decisionTightens(dec decision.Decision) bool {
	d, _ := mapVerdict(dec.Evaluation)
	return d != ""
}

// tier2FailOpen builds the "no real T2 verdict" decision — the
// failure-policy input for a T2 outage. FailOpen=true so the unchanged
// applyFailurePolicy proceeds (fail-open) or denies (fail-closed) exactly
// as for a T1 outage. cause is a fixed, content-free diagnostic string
// (INV-2).
func tier2FailOpen(cause string) decision.Decision {
	return decision.Decision{
		Evaluation: client.Evaluation{Verdict: client.VerdictUnknown, Reason: cause},
		FailOpen:   true,
		Source:     sourceTier2FailOpen,
	}
}

// tier2Decision wraps a /evaluate Evaluation into a decision the failure
// policy + apply cascade consume unchanged. A VerdictUnknown means no real
// server verdict (Emit folds every transport failure/timeout/empty
// response into VerdictUnknown, fail-open) → a fail-open decision; any
// real verdict is carried through FailOpen=false, so a reachable-core
// ALLOW is never overridden by fail-closed.
func tier2Decision(eval client.Evaluation) decision.Decision {
	if eval.Verdict == client.VerdictUnknown {
		return tier2FailOpen("tier-2 /evaluate returned no verdict")
	}
	return decision.Decision{Evaluation: eval, Source: sourceTier2}
}

// escalateTier2 performs one synchronous /evaluate escalation for a
// high-risk tool and returns the resulting decision (a real verdict, or a
// fail-open decision on any fault). It never blocks beyond the budget and
// never surfaces an error.
//
// The whole escalation — credential resolution included — is bounded by
// the budget via a goroutine + select. Credential resolution does the
// first hot-path secret-store I/O in enforce mode and secretLookup is not
// context-aware (an OS keychain that prompts/locks can hang), so bounding
// only the network Emit would leave a hole: a hung keychain could push
// the whole hook past Codex's kill → fail-open → a fail-closed org's
// high-risk call runs silently ungoverned. Bounding the entire body closes
// it: on budget expiry we return a fail-open decision (→ fail-open
// proceeds, fail-closed denies) and abandon the in-flight goroutine (the
// hook process exits shortly after; the result channel is buffered so the
// goroutine never blocks on send).
func escalateTier2(ctx context.Context, logger *log.Logger, m Mapper, ev *HookEvent, budget time.Duration) decision.Decision {
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	resultCh := make(chan decision.Decision, 1)
	go func() { resultCh <- runTier2(cctx, logger, m, ev) }()

	select {
	case dec := <-resultCh:
		return dec
	case <-cctx.Done():
		logger.Printf("tier-2 escalation degrading (budget %v exceeded)", budget)
		return tier2FailOpen("tier-2 budget exceeded")
	}
}

// runTier2 is the bounded body: build the event, resolve creds, build the
// client, and Emit under cctx. It reuses the observe Mapper to build the
// same normalized tool-call event the hot-path spool produced (identical
// deterministic event_id → idempotency-ready re-send), so the T2 egress
// posture is identical to observe: metadata-only unless content capture
// is on (the /evaluate client strips content when capture is off). The
// obx_ key + Ed25519 seed live only inside the client (INV-1); the
// returned Decision carries the verdict alone.
func runTier2(cctx context.Context, logger *log.Logger, m Mapper, ev *HookEvent) decision.Decision {
	devEv, ok := m.Map(HookPreToolUse, ev)
	if !ok {
		return tier2FailOpen("tier-2 event not mappable")
	}
	creds, err := ResolveCredentials()
	if err != nil {
		logger.Printf("tier-2 escalation degrading (credentials): %v", err)
		return tier2FailOpen("tier-2 credentials unavailable")
	}
	cl, err := creds.NewClient(logger)
	if err != nil {
		logger.Printf("tier-2 escalation degrading (client init): %v", err)
		return tier2FailOpen("tier-2 client unavailable")
	}
	eval, err := cl.Emit(cctx, devEv)
	if err != nil {
		// Covers both an unbuildable event and a delivery failure
		// (client.ErrDelivery). A synchronous escalation has nothing to retry
		// from — the spooled copy is the durable one — so either way this
		// degrades fail-open rather than guessing a verdict.
		logger.Printf("tier-2 escalation degrading (emit): %v", err)
		return tier2FailOpen("tier-2 escalation undelivered")
	}
	return tier2Decision(eval)
}
