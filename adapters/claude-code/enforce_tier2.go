package claudecode

import (
	"context"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Tier-2 synchronous /evaluate escalation.
//
// The enforce gate is tiered by risk class:
//
//   - Tier 1 — the local decider: in-process, ~50ms, the only tier allowed
//     to block frequent low-risk edits. It enforces only the policy/OPA
//     dimension (the "fidelity floor": Guardrail PII/NSFW + LLamaFirewall
//     drift are model-backed and server-side, so a local verdict is a
//     floor — it can be more permissive than core).
//   - Tier 2 — a synchronous /evaluate call (this file): for high-risk
//     classes only (Bash / MCP execution), after T1 allows, obtain the
//     authoritative full server verdict (policy + guardrail classifiers;
//     drift advisory) before the tool runs. This closes the floor exactly
//     where arbitrary execution makes it dangerous — and nowhere else, so
//     frequent edits never pay the ~1.6s /evaluate latency (accept the
//     full latency for rare high-risk calls).
//
// Why this tiers when the reference SDK does not: the temporal SDK runs
// the full synchronous evaluate_event on every activity, no risk-class
// tiering. A dev-runtime gate is different: it runs on every matched tool
// call, and a ~1.6s wait on every Edit would make the developer remove the
// hook — and every gate upstream of CI evaporates. So risk-class tiering
// is a deliberate shift-left addition, the developer-runtime analog of the
// SDK's coarse skip lists. It only ever makes the gate more thorough (T2
// can upgrade a T1 allow to a deny); it never loosens a T1 block.
//
// Timeout is owned in-binary — a correctness bound. Claude Code fails open
// when a PreToolUse hook exceeds its configured timeout — a CC-layer
// fail-open that would silently defeat a fail-closed org. So the T2 wait
// is bounded by our budget (ResolveTier2Timeout, clamped to
// maxTier2Timeout) which stays strictly under the PreToolUse hook timeout
// with margin: our own fail-open/closed verdict (via applyFailurePolicy)
// is emitted before CC's timeout can fire. This is the same discipline as
// the T1 clamp (maxEnforceTimeout), scaled up for the network round-trip.
//
// Failure semantics reuse the T1 failure policy verbatim. A T2 call that
// yields no real verdict (transport failure, timeout, an empty/unmapped
// response — all folded by Emit's fail-open into a VerdictUnknown
// Evaluation) becomes a FailOpen decision.Decision, so the unchanged
// applyFailurePolicy proceeds (fail-open) or synthesizes a HALT
// (fail-closed) exactly as it does for a T1 outage. This mirrors the SDK's
// _handle_api_error (fail_open → None → proceed; fail_closed → synthetic
// HALT).

const (
	// bashToolName is the Claude Code tool for arbitrary shell execution.
	// It is the one shell-kind tool that is genuinely high-risk
	// (WebFetch/Task/TodoWrite also classify as ToolShell but do not
	// execute arbitrary commands), so it is matched by exact name rather
	// than by the coarse kind.
	bashToolName = "Bash"

	// sourceTier2 marks a decision obtained from the Tier-2 sync /evaluate
	// (a real server verdict). sourceTier2FailOpen marks a T2 call that
	// yielded no real verdict (outage/unknown) — the failure-policy
	// input, analogous to the local Client's sourceFailOpenClient. Both
	// are content-free and appear only in the stderr diagnostic + the
	// content-free enforcement audit (INV-1/INV-2).
	sourceTier2         = "tier2:evaluate"
	sourceTier2FailOpen = "tier2:fail-open"
)

// defaultTier2Timeout is the default in-binary budget for one synchronous
// /evaluate escalation. /evaluate measures at ~0.8-1.6s (the full Temporal
// pipeline), so 3.5s comfortably covers one round-trip plus the client's
// bounded retries, while staying under maxTier2Timeout.
const defaultTier2Timeout = 3500 * time.Millisecond

// maxTier2Timeout caps the configurable T2 budget. Like maxEnforceTimeout
// for T1 this is a correctness bound, not a nicety: the whole PreToolUse
// hook (config read + T1 gate + T2 round-trip + spool) must return before
// Claude Code's PreToolUse hook timeout (5s), because CC fails open on a
// hook timeout and would silently let a fail-closed deny slip. 4s leaves
// ~1s of margin under the shipped 5s hook timeout for the T1 gate + apply
// + audit that bracket the T2 call. An org running a slower core that
// needs a larger budget must also raise the PreToolUse hook timeout in
// hooks.json and keep budget < hook timeout; lifting this hard-coded
// ceiling in lock-step is a noted follow-on.
const maxTier2Timeout = 4 * time.Second

// maxEnforceHookBudget caps the whole enforce PreToolUse hook's wall clock
// (T1 decider + T2 /evaluate, which run sequentially) so their
// independently-clamped budgets can never jointly exceed CC's 5s hook
// timeout. Without this, an org that raises OPENBOX_ENFORCE_TIMEOUT_MS
// toward the 2s T1 clamp and enables T2 could see T1(≤2s) + T2(≤4s) ≈ 6s
// > 5s → CC fails open → a fail-closed org's high-risk call runs silently
// ungoverned. 4s leaves ~1s margin under the 5s hook for the config reads
// + apply + audit that bracket the two gates.
const maxEnforceHookBudget = 4 * time.Second

// tier2Budget is the effective budget for the T2 escalation: the
// configured T2 budget, but never more than the time remaining in the
// whole-hook wall-clock cap (maxEnforceHookBudget) after the T1 gate
// already ran. enforceStart is the instant the enforce block began. A
// non-positive remainder (T1 already consumed the cap — only reachable
// with a very slow decider under a raised T1 clamp) yields a non-positive
// budget, so escalateTier2 fail-opens immediately (fail-closed denies,
// fail-open proceeds) rather than push the hook past the CC timeout — the
// safe direction, by construction.
func tier2Budget(enforceStart time.Time) time.Duration {
	budget := ResolveTier2Timeout()
	if rem := maxEnforceHookBudget - time.Since(enforceStart); rem < budget {
		budget = rem
	}
	return budget
}

// isHighRiskClass reports whether a tool is a Tier-2 high-risk class —
// arbitrary execution — that warrants escalating a T1 allow to the
// authoritative server verdict: the Bash tool (arbitrary shell) or any MCP
// tool call (executes on an MCP server). Frequent low-risk classes
// (Edit/Write/Read) and the other shell-catch-all tools
// (WebFetch/Task/TodoWrite) are T1-only, so they never pay the /evaluate
// latency. Widening this set (e.g. to WebFetch) is a config/policy
// question, deliberately out of scope here.
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

// tier2FailOpen builds the "no real T2 verdict" decision — the
// failure-policy input for a T2 outage (transport failure, timeout,
// missing credentials, or an empty/unmapped /evaluate response).
// FailOpen=true so the unchanged applyFailurePolicy proceeds (fail-open)
// or denies (fail-closed) exactly as for a T1 outage. cause is a fixed,
// content-free diagnostic string (INV-2), never tool content — it feeds
// failClosedReason on the fail-closed path.
func tier2FailOpen(cause string) decision.Decision {
	return decision.Decision{
		Evaluation: client.Evaluation{Verdict: client.VerdictUnknown, Reason: cause},
		FailOpen:   true,
		Source:     sourceTier2FailOpen,
	}
}

// tier2Decision wraps a /evaluate Evaluation into a decision.Decision the
// failure policy + apply cascade consume unchanged. A VerdictUnknown means
// no real server verdict was obtained (Emit folds every transport
// failure/timeout/empty response into VerdictUnknown, fail-open) → a
// fail-open decision; any real verdict
// (ALLOW/CONSTRAIN/REQUIRE_APPROVAL/BLOCK/HALT) is carried through
// FailOpen=false, so a reachable-core ALLOW is never overridden by
// fail-closed (fail-closed engages on no-verdict only). This mirrors the
// local Client's isRealVerdictSource discipline: an unknown verdict is
// "OpenBox did not govern this call", not a real allow.
func tier2Decision(eval client.Evaluation) decision.Decision {
	if eval.Verdict == client.VerdictUnknown {
		return tier2FailOpen("tier-2 /evaluate returned no verdict")
	}
	return decision.Decision{Evaluation: eval, Source: sourceTier2}
}

// escalateTier2 performs one synchronous /evaluate escalation for a
// high-risk tool and returns the resulting decision (a real verdict, or a
// fail-open decision on any fault). It never blocks beyond the budget and
// never surfaces an error — every fault degrades to tier2FailOpen so the
// caller's failure policy decides (fail-open proceeds; fail-closed
// denies).
//
// The whole escalation — credential resolution included — is bounded by
// the budget via a goroutine + select. Credential resolution does the
// first hot-path secret-store I/O in enforce mode, and secretLookup is not
// context-aware (an OS keychain that prompts / locks, or a
// network-backed store, can hang), so bounding only the network Emit
// would leave a hole: a hung keychain could push the whole PreToolUse hook
// past Claude Code's 5s hook timeout, at which point CC fails open — a
// fail-closed org's high-risk call would then run silently ungoverned,
// the exact INV-3b failure class this tier exists to prevent. Bounding the
// entire body closes it: on budget expiry we return a fail-open decision
// (→ fail-open proceeds, fail-closed denies) and abandon the in-flight
// goroutine (the hook process exits shortly after, reaping it; the result
// channel is buffered so it never blocks on send). The safe direction — a
// hang becomes a bounded deny (fail-closed) or a bounded proceed
// (fail-open), never a CC-timeout fail-open.
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

// runTier2 is the bounded body: build the event, resolve creds, build the
// client, and Emit under cctx. It reuses the observe Mapper to build the
// same normalized tool-call event the hot-path spool already produced
// (identical deterministic event_id → an idempotency-ready re-send), so
// the T2 egress posture is identical to observe: metadata-only unless
// content capture is on (m.CaptureContent) — Tier-2 introduces no new
// egress surface (the /evaluate client strips content when capture is
// off, exactly as on flush). The obx_ key + Ed25519 seed live only inside
// the client (INV-1); the returned Decision carries the verdict alone.
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
