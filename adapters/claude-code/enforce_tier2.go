package claudecode

import (
	"context"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Tier-2 synchronous /evaluate escalation (STORY-E6-S10, Phase-2, design §7).
//
// The enforce gate is TIERED by risk class (design §7, ratified brian 2026-07-14):
//
//   - Tier 1 — the LOCAL decider (E6-S1..S9): in-process, ~50 ms, the only tier
//     allowed to block frequent low-risk edits. It enforces ONLY the policy/OPA
//     dimension (the §2a "fidelity floor": Guardrail PII/NSFW + LLamaFirewall drift
//     are model-backed and server-side, so a local verdict is a FLOOR — it can be
//     more permissive than core).
//   - Tier 2 — a SYNCHRONOUS /evaluate call (this file): for HIGH-RISK classes only
//     (Bash / MCP execution), after T1 allows, obtain the AUTHORITATIVE full server
//     verdict (policy + guardrail classifiers; drift advisory) BEFORE the tool runs.
//     This closes the floor exactly where arbitrary execution makes it dangerous —
//     and nowhere else, so frequent edits never pay the ~1.6 s /evaluate latency
//     (OD-SYNC-9: accept the full latency for rare high-risk calls).
//
// WHY THIS TIERS WHEN THE REFERENCE SDK DOES NOT: the temporal SDK runs the full
// synchronous evaluate_event on EVERY activity (activity_interceptor.py:227-256,
// no risk-class tiering — cross-repo recon). A dev-runtime gate is different: it
// runs on every matched TOOL call, and a ~1.6 s wait on every Edit would make the
// developer remove the hook — and every gate upstream of CI evaporates (design §7
// "why this is a design rule, not a tuning knob"). So risk-class tiering is a
// deliberate shift-left addition, the developer-runtime analog of the SDK's
// coarse skip lists. It only ever makes the gate MORE thorough (T2 can upgrade a
// T1 allow to a deny); it never loosens a T1 block.
//
// TIMEOUT IS OWNED IN-BINARY (OD-SYNC-8, a CORRECTNESS bound). Claude Code fails
// OPEN when a PreToolUse hook exceeds its configured timeout (verified empirically,
// CC v2.1.210) — a CC-layer fail-open that would silently defeat a fail-CLOSED org.
// So the T2 wait is bounded by OUR budget (ResolveTier2Timeout, clamped to
// maxTier2Timeout) which stays strictly UNDER the PreToolUse hook timeout with
// margin: our own fail-open/closed verdict (via applyFailurePolicy) is emitted
// before CC's timeout can fire. This is the same discipline as the T1 clamp
// (maxEnforceTimeout), scaled up for the network round-trip.
//
// FAILURE SEMANTICS reuse E6-S3 verbatim. A T2 call that yields no real verdict
// (transport failure, timeout, an empty/unmapped response — all folded by Emit's
// fail-open into a VerdictUnknown Evaluation) becomes a FailOpen decision.Decision,
// so the UNCHANGED applyFailurePolicy proceeds (fail-open, OD9) or synthesizes a
// HALT (fail-closed) exactly as it does for a T1 outage. This mirrors the SDK's
// _handle_api_error (fail_open → None → proceed; fail_closed → synthetic HALT),
// recon-confirmed at client.py:204-208.

const (
	// bashToolName is the Claude Code tool for arbitrary shell execution. It is the
	// one shell-kind tool that is genuinely high-risk (WebFetch/Task/TodoWrite also
	// classify as ToolShell but do not execute arbitrary commands), so it is matched
	// by exact name rather than by the coarse kind.
	bashToolName = "Bash"

	// sourceTier2 marks a decision obtained from the Tier-2 sync /evaluate (a REAL
	// server verdict). sourceTier2FailOpen marks a T2 call that yielded no real
	// verdict (outage/unknown) — the failure-policy input, analogous to the local
	// Client's sourceFailOpenClient. Both are content-free and appear only in the
	// stderr diagnostic + the content-free enforcement audit (INV-1/INV-2).
	sourceTier2         = "tier2:evaluate"
	sourceTier2FailOpen = "tier2:fail-open"
)

// defaultTier2Timeout is the default in-binary budget for one synchronous
// /evaluate escalation. S2 measured /evaluate at ~0.8–1.6 s (the full Temporal
// pipeline), so 3.5 s comfortably covers one round-trip plus the client's bounded
// retries, while staying under maxTier2Timeout.
const defaultTier2Timeout = 3500 * time.Millisecond

// maxTier2Timeout caps the configurable T2 budget. Like maxEnforceTimeout for T1
// this is a CORRECTNESS bound, not a nicety: the whole PreToolUse hook (config read
// + T1 gate + T2 round-trip + spool) MUST return before Claude Code's PreToolUse
// hook timeout (plugin/hooks/hooks.json, 5 s), because CC fails OPEN on a hook
// timeout and would silently let a fail-closed deny slip. 4 s leaves ~1 s of margin
// under the shipped 5 s hook timeout for the T1 gate + apply + audit that bracket
// the T2 call. An org running a slower core that needs a larger budget must ALSO
// raise the PreToolUse hook timeout in hooks.json (it owns that value — OD-SYNC-8)
// and keep budget < hook timeout; lifting this hard-coded ceiling in lock-step is a
// noted follow-on.
const maxTier2Timeout = 4 * time.Second

// maxEnforceHookBudget caps the WHOLE enforce PreToolUse hook's wall clock (T1
// decider + T2 /evaluate, which run SEQUENTIALLY) so their independently-clamped
// budgets can never JOINTLY exceed CC's 5 s hook timeout (G3 MINOR-2). Without this,
// an org that raises OPENBOX_ENFORCE_TIMEOUT_MS toward the 2 s T1 clamp AND enables
// T2 could see T1(≤2 s) + T2(≤4 s) ≈ 6 s > 5 s → CC fails OPEN → a fail-closed
// org's high-risk call runs silently ungoverned. 4 s leaves ~1 s margin under the
// 5 s hook for the config reads + apply + audit that bracket the two gates.
const maxEnforceHookBudget = 4 * time.Second

// tier2Budget is the effective budget for the T2 escalation: the configured T2
// budget, but never more than the time REMAINING in the whole-hook wall-clock cap
// (maxEnforceHookBudget) after the T1 gate already ran. enforceStart is the instant
// the enforce block began. A non-positive remainder (T1 already consumed the cap —
// only reachable with a very slow decider under a raised T1 clamp) yields a
// non-positive budget, so escalateTier2 fail-opens IMMEDIATELY (fail-closed denies,
// fail-open proceeds) rather than push the hook past the CC timeout — the safe
// direction, by construction.
func tier2Budget(enforceStart time.Time) time.Duration {
	budget := ResolveTier2Timeout()
	if rem := maxEnforceHookBudget - time.Since(enforceStart); rem < budget {
		budget = rem
	}
	return budget
}

// isHighRiskClass reports whether a tool is a Tier-2 high-risk class — arbitrary
// EXECUTION — that warrants escalating a T1 allow to the authoritative server
// verdict (design §7): the Bash tool (arbitrary shell) or any MCP tool call
// (executes on an MCP server). Frequent low-risk classes (Edit/Write/Read) and the
// other shell-catch-all tools (WebFetch/Task/TodoWrite) are T1-only, so they never
// pay the /evaluate latency (OD-SYNC-9). Widening this set (e.g. to WebFetch) is a
// config/policy question, deliberately out of v1 scope.
func isHighRiskClass(toolName string) bool {
	if toolName == bashToolName {
		return true
	}
	kind, _, _, _, _ := classifyTool(toolName)
	return kind == client.ToolMCP
}

// decisionTightens reports whether a decision already yields a Claude Code
// deny/ask (via the unchanged mapVerdict cascade). When T1 already tightens — a
// real BLOCK/HALT/guardrail-fail/REQUIRE_APPROVAL, or a fail-closed synthesized
// HALT — there is nothing for T2 to add (governance only tightens; T2 cannot make
// a block MORE restrictive), so the escalation is skipped and the T1 decision
// stands. T2 fires ONLY when T1 would otherwise PROCEED.
func decisionTightens(dec decision.Decision) bool {
	d, _ := mapVerdict(dec.Evaluation)
	return d != ""
}

// tier2FailOpen builds the "no real T2 verdict" decision — the failure-policy
// input for a T2 outage (transport failure, timeout, missing credentials, or an
// empty/unmapped /evaluate response). FailOpen=true so the UNCHANGED
// applyFailurePolicy proceeds (fail-open, OD9) or denies (fail-closed) exactly as
// for a T1 outage. cause is a fixed, content-free diagnostic string (INV-2), never
// tool content — it feeds failClosedReason on the fail-closed path.
func tier2FailOpen(cause string) decision.Decision {
	return decision.Decision{
		Evaluation: client.Evaluation{Verdict: client.VerdictUnknown, Reason: cause},
		FailOpen:   true,
		Source:     sourceTier2FailOpen,
	}
}

// tier2Decision wraps a /evaluate Evaluation into a decision.Decision the E6-S3
// failure policy + E6-S2 apply cascade consume unchanged. A VerdictUnknown means
// no real server verdict was obtained (Emit folds every transport failure/timeout/
// empty response into VerdictUnknown, fail-open) → a fail-open decision; any real
// verdict (ALLOW/CONSTRAIN/REQUIRE_APPROVAL/BLOCK/HALT) is carried through
// FailOpen=false, so a reachable-core ALLOW is never overridden by fail-closed
// (the E6-S3 crux: fail-closed engages on no-verdict only). This mirrors the local
// Client's isRealVerdictSource discipline: an unknown verdict is "OpenBox did not
// govern this call", not a real allow.
func tier2Decision(eval client.Evaluation) decision.Decision {
	if eval.Verdict == client.VerdictUnknown {
		return tier2FailOpen("tier-2 /evaluate returned no verdict")
	}
	return decision.Decision{Evaluation: eval, Source: sourceTier2}
}

// escalateTier2 performs one synchronous /evaluate escalation for a high-risk tool
// and returns the resulting decision (a real verdict, or a fail-open decision on
// any fault). It NEVER blocks beyond the budget and NEVER surfaces an error — every
// fault degrades to tier2FailOpen so the caller's failure policy decides (fail-open
// proceeds; fail-closed denies).
//
// THE WHOLE escalation — credential resolution INCLUDED — is bounded by the budget
// via a goroutine + select (G_SEC MEDIUM). Credential resolution does the first
// hot-path secret-store I/O in enforce mode, and secretLookup is NOT context-aware
// (an OS keychain that prompts / locks, or a network-backed store, can hang), so
// bounding only the network Emit would leave a hole: a hung keychain could push the
// whole PreToolUse hook past Claude Code's 5 s hook timeout, at which point CC fails
// OPEN — a fail-closed org's high-risk call would then run SILENTLY UNGOVERNED, the
// exact INV-3b failure class this tier exists to prevent. Bounding the entire body
// closes it: on budget expiry we return a fail-open decision (→ fail-open proceeds,
// fail-closed DENIES) and abandon the in-flight goroutine (the hook process exits
// shortly after, reaping it; the result channel is buffered so it never blocks on
// send). The SAFE direction — a hang becomes a bounded deny (fail-closed) or a
// bounded proceed (fail-open), never a CC-timeout fail-open.
func escalateTier2(ctx context.Context, logger *log.Logger, m Mapper, ev *HookEvent, budget time.Duration) decision.Decision {
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// Buffered so an abandoned goroutine (budget-expired path) never blocks on send.
	resultCh := make(chan decision.Decision, 1)
	go func() { resultCh <- runTier2(cctx, logger, m, ev) }()

	select {
	case dec := <-resultCh:
		return dec
	case <-cctx.Done():
		// Budget exhausted before a verdict — INCLUDING a hung, non-cancelable
		// secret-store lookup. Degrade fail-open; the failure policy decides.
		logger.Printf("tier-2 escalation degrading (budget %v exceeded)", budget)
		return tier2FailOpen("tier-2 budget exceeded")
	}
}

// runTier2 is the bounded body: build the event, resolve creds, build the client,
// and Emit under cctx. It reuses the observe Mapper to build the SAME normalized
// tool-call event the hot-path spool already produced (identical deterministic
// event_id → an idempotency-ready re-send, SL-14), so the T2 egress posture is
// IDENTICAL to observe: metadata-only unless content capture is on (m.CaptureContent)
// — Tier-2 introduces NO new egress surface (the /evaluate client strips content
// when capture is off, exactly as on flush). The obx_ key + Ed25519 seed live only
// inside the client (INV-1); the returned Decision carries the verdict alone.
func runTier2(cctx context.Context, logger *log.Logger, m Mapper, ev *HookEvent) decision.Decision {
	devEv, ok := m.Map(HookPreToolUse, ev)
	if !ok {
		// Same drop condition as the observe path (missing session/DID). Nothing to
		// evaluate → degrade; the failure policy proceeds (fail-open) or denies
		// (fail-closed) rather than silently skipping the high-risk gate.
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
		// Emit reserves its error for a caller PRECONDITION (an unbuildable event),
		// never a transport failure (that is fail-open INSIDE Emit → a VerdictUnknown
		// Evaluation). Either way, degrade.
		logger.Printf("tier-2 escalation degrading (emit): %v", err)
		return tier2FailOpen("tier-2 event build failed")
	}
	return tier2Decision(eval)
}
