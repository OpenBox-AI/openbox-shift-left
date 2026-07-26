package codex

import (
	"context"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Tier-2 synchronous /evaluate escalation for the Codex adapter (STORY-SL7-B, the
// port of E6-S10; design §7). Byte-for-byte the same tiering discipline as Claude
// Code — only the timeout derivation is Codex-shaped.
//
//   - Tier 1 — the LOCAL in-process decider (enforce.go): microseconds, the only
//     tier allowed to block frequent low-risk edits; a policy/OPA FLOOR (Guardrail
//     PII/NSFW + drift are model-backed and server-side, so a local verdict can be
//     more permissive than core, never stricter than a real server BLOCK).
//   - Tier 2 — a SYNCHRONOUS /evaluate call (this file): for HIGH-RISK classes only
//     (Bash / MCP execution), after T1 allows, obtain the AUTHORITATIVE full server
//     verdict BEFORE the tool runs. Frequent edits (apply_patch) never pay the
//     /evaluate latency (OD-SYNC-9). T2 can only TIGHTEN a T1 allow.
//
// FAILURE SEMANTICS reuse the E6-S3 policy verbatim: a T2 call yielding no real
// verdict (transport failure, timeout, empty/unmapped response) becomes a FailOpen
// decision, so the UNCHANGED applyFailurePolicy proceeds (fail-open) or synthesizes
// a HALT (fail-closed) exactly as for a T1 outage.
//
// TIMEOUT IS OWNED IN-BINARY (OD-SL7-T2-TIMEOUT). Probe P1 proved Codex FAILS OPEN
// when a PreToolUse hook overruns its `timeout`, so a T2 wait that pushed the hook
// past the installed timeout would silently defeat a fail-closed org — the exact
// hazard this tier exists to prevent. The T2 budget is therefore clamped strictly
// under maxEnforceHookBudget (enforce.go — itself DERIVED from the installed
// hot-hook timeout, not copied from Claude Code's 5 s constant). Codex's 600 s
// default hook timeout gives far more headroom than Claude Code, but the default
// budget stays conservative until a probe justifies raising it (packet ruling).

const (
	// bashToolName is Codex's hook identity for shell-like execution (classifyTool
	// grounding: HookToolName::bash() is serialized for shell_command/unified_exec/
	// exec paths). The genuinely high-risk shell tool, matched by exact name.
	bashToolName = "Bash"

	// sourceTier2 marks a decision from the Tier-2 sync /evaluate (a REAL server
	// verdict); sourceTier2FailOpen marks a T2 call that yielded no real verdict
	// (outage/unknown) — the failure-policy input. Both content-free (INV-1/INV-2).
	sourceTier2         = "tier2:evaluate"
	sourceTier2FailOpen = "tier2:fail-open"
)

// defaultTier2Timeout is the default in-binary budget for one synchronous
// /evaluate escalation. S2 measured /evaluate at ~0.8–1.6 s, so 3.5 s covers one
// round-trip plus the client's bounded retries while staying under maxTier2Timeout.
// Kept at the Claude Code value per OD-SL7-T2-TIMEOUT ("T2 ≤ the CC value" until a
// probe justifies more) — Codex's larger hook budget is available headroom, not a
// reason to widen the default.
const defaultTier2Timeout = 3500 * time.Millisecond

// maxTier2Timeout caps the configurable T2 budget. DERIVED (not copied): it is the
// whole-hook wall-clock ceiling (maxEnforceHookBudget, enforce.go — installed
// hot-hook timeout minus margin), because the entire PreToolUse hook (config reads
// + T1 gate + T2 round-trip + apply + audit) MUST return before Codex kills the
// hook and fails OPEN (probe P1). If an org raises the installed hot-hook timeout,
// this rises with it automatically.
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

// isHighRiskClass reports whether a tool is a Tier-2 high-risk class — arbitrary
// EXECUTION — that warrants escalating a T1 allow to the authoritative server
// verdict: the Bash tool (arbitrary shell) or any MCP tool call. Frequent low-risk
// classes (apply_patch file edits) and the other shell-catch-all tools
// (web_search/update_plan/…) are T1-only, so they never pay the /evaluate latency
// (OD-SYNC-9).
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

// tier2FailOpen builds the "no real T2 verdict" decision — the failure-policy input
// for a T2 outage. FailOpen=true so the UNCHANGED applyFailurePolicy proceeds
// (fail-open) or denies (fail-closed) exactly as for a T1 outage. cause is a fixed,
// content-free diagnostic string (INV-2).
func tier2FailOpen(cause string) decision.Decision {
	return decision.Decision{
		Evaluation: client.Evaluation{Verdict: client.VerdictUnknown, Reason: cause},
		FailOpen:   true,
		Source:     sourceTier2FailOpen,
	}
}

// tier2Decision wraps a /evaluate Evaluation into a decision the E6-S3 failure
// policy + apply cascade consume unchanged. A VerdictUnknown means no real server
// verdict (Emit folds every transport failure/timeout/empty response into
// VerdictUnknown, fail-open) → a fail-open decision; any real verdict is carried
// through FailOpen=false, so a reachable-core ALLOW is never overridden by
// fail-closed.
func tier2Decision(eval client.Evaluation) decision.Decision {
	if eval.Verdict == client.VerdictUnknown {
		return tier2FailOpen("tier-2 /evaluate returned no verdict")
	}
	return decision.Decision{Evaluation: eval, Source: sourceTier2}
}

// escalateTier2 performs one synchronous /evaluate escalation for a high-risk tool
// and returns the resulting decision (a real verdict, or a fail-open decision on
// any fault). It NEVER blocks beyond the budget and NEVER surfaces an error.
//
// THE WHOLE escalation — credential resolution INCLUDED — is bounded by the budget
// via a goroutine + select. Credential resolution does the first hot-path
// secret-store I/O in enforce mode and secretLookup is NOT context-aware (an OS
// keychain that prompts/locks can hang), so bounding only the network Emit would
// leave a hole: a hung keychain could push the whole hook past Codex's kill →
// fail-open → a fail-closed org's high-risk call runs SILENTLY UNGOVERNED. Bounding
// the entire body closes it: on budget expiry we return a fail-open decision (→
// fail-open proceeds, fail-closed DENIES) and abandon the in-flight goroutine (the
// hook process exits shortly after; the result channel is buffered so the goroutine
// never blocks on send).
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

// runTier2 is the bounded body: build the event, resolve creds, build the client,
// and Emit under cctx. It reuses the observe Mapper to build the SAME normalized
// tool-call event the hot-path spool produced (identical deterministic event_id →
// idempotency-ready re-send), so the T2 egress posture is IDENTICAL to observe:
// metadata-only unless content capture is on (the /evaluate client strips content
// when capture is off). The obx_ key + Ed25519 seed live only inside the client
// (INV-1); the returned Decision carries the verdict alone.
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
		logger.Printf("tier-2 escalation degrading (emit): %v", err)
		return tier2FailOpen("tier-2 event build failed")
	}
	return tier2Decision(eval)
}
