# ADR-0020 — The prompt gate, and HALT ends the session

Status: Accepted. Date: 2026-08-18.

Extends ADR-0017 (every gated surface is `/evaluate`'s to decide) to a second
hook, and gives HALT a session-level meaning the developer runtime previously
did not have. No new table, endpoint or service.

## Context

Two gaps, both observed live (diagnosis:
`plans/reports/debug-260818-1656-halt-block-does-not-stop-loop.md`):

1. **Only tool calls gated.** `PromptSubmitted` — the event that carries the
   developer's intent, the thing every later alignment check scores against —
   was observe-only. A policy that wanted to stop work at the prompt could only
   watch it happen post-hoc in the dashboard.
2. **HALT ≈ BLOCK.** The reference SDK terminates the workflow on HALT
   (`GovernanceHaltError`); the developer runtime mapped both HALT and BLOCK to
   one per-call `permissionDecision:"deny"`. Claude Code treats a deny as "this
   call refused, keep going", so the model routed around the strongest verdict
   governance can issue. Nothing emitted Claude Code's documented session-stop
   lever (top-level `continue:false` + `stopReason`, which overrides per-event
   decisions).

## Decision (owner, 2026-08-18)

1. **`UserPromptSubmit` is a gate.** In enforce mode the `PromptSubmitted`
   event is evaluated inline before the prompt is processed, through the same
   shared `EnforceGate` as tool calls: same escalation, failure policy,
   approval hold (held, then blocked if unanswered), spool-dedupe. HALT/BLOCK
   render as the hook's native top-level `{"decision":"block","reason":…}` —
   the prompt is refused and erased. Registration raises the hook timeout to
   the gating ceiling (30s); **existing installs gain the gate on the next
   `openbox init`**, exactly like the ADR-0018 hooks.
2. **A HALT the control plane returns ends the session.** The response carries
   `continue:false` + `stopReason` (the turn stops immediately), and a local
   latch (`<config>/openbox/halted-sessions/<session>.json`) refuses every
   later prompt and tool call in that session locally — no re-evaluation,
   `--resume` included. The interactive REPL process itself stays open but
   inert: the hook protocol has no process-exit response (verified against the
   hooks reference), and in headless runs the turn stop ends the process.
3. **Only the server's own HALT qualifies.** The discriminator is
   `Verdict==HALT && Source==evaluate && FailOpen==false`, marked by the gate
   AFTER the approval hold resolves. Synthesized HALTs keep per-call deny
   semantics: the fail-closed outage answer (`FailOpen=true`), an approver
   reject (`Source=approval:decided`), an unanswered hold — which is re-sourced
   `approval:undecided` as part of this change, because it previously kept
   `Source=evaluate` and was indistinguishable from a real server HALT in both
   the audit and this discriminator.
4. **Every server HALT is trusted, authored or not.** The known core defect
   that expresses a session-precondition failure as an unauthored HALT
   (`debug-260814-1231`) will therefore now END sessions instead of denying
   calls until the record clears. The owner chose that consequence over
   client-side verdict discrimination, which plan 260814-2235 already rejected:
   truthfulness is the decider's obligation, and the fix is core-side.

## Consequences

- HALT and BLOCK finally mean different things on the developer runtime:
  BLOCK refuses one call/prompt, HALT ends the session. Policy authors get the
  same escalation ladder the agent runtime has.
- Codex has no session-stop lever and no prompt hook; a HALT there renders as
  its strongest per-call deny and writes no latch (the latch is keyed on the
  session stop actually being EXPRESSED). Codex session-stop semantics are a
  follow-up.
- The latch is local state under the developer's control, like every other
  client-side control here (ADR-0015 threat model): deleting the file un-halts
  the machine's view, and the server-side record still shows every verdict.
  The latch is a faithful-client mechanism, not tamper-proof enforcement.
- A fail-closed org's outage behavior is unchanged: deny per call while
  unreachable, never a permanent session kill from a transient outage.
- Prompt approvals are addressable only at session granularity
  (`activityPairKey` for signal events carries no per-invocation span, and
  `activity_id` derivation is byte-pinned event identity — not changed here).
  Every failure mode of the hold lands on block: over-ask, never over-grant.
- Conformance C27–C31 pin the semantics on real RunHook stdout bytes; the
  session-halt discriminator's regression case (a hold timeout terminating the
  session) is pinned in `TestApprovalUndecided_DeniesWithTheReference` and
  `TestApplyDecisionSessionHaltSplit`.
