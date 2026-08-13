---
title: "Populate dev-session widget telemetry (tool success + goal alignment)"
description: "Emit ActivityCompleted status and a content-gated assistant-turn span so Tool Health, Goal Alignment and Drift widgets populate for developer sessions"
status: superseded
priority: P2
effort: 12h
branch: main
tags: [telemetry, contracts, privacy, adr]
created: 2026-08-13
---

# Populate dev-session widget telemetry

> **Superseded 2026-08-13** by
> [260813-2314-dev-telemetry-and-content-posture](../260813-2314-dev-telemetry-and-content-posture/plan.md)
> (merged with 260813-2241, validation interview recorded there). The `scout/` and research
> evidence in this directory remains the source of record.

Three Monitor-tab widgets are empty/wrong for a `kind=developer` agent because dev sessions never
emit the telemetry they derive from. Scope: **openbox-shift-left only** (user decision) — core /
backend / FE unchanged; their defects become documented follow-ups.

| Widget | Root cause | Fix |
|---|---|---|
| Tool Health SUCCESS 0.0% | core counts success only on top-level `status=="completed"` (`openbox-core .../observability/errors.go:333`); `client/payload.go:28-78` has no `status` key → `.failed` increments every completion | phase 2: `status` on tool `ActivityCompleted`, derived structurally |
| Goal Alignment Trend + Recent Drift | AGE accumulates assistant text ONLY from `payload.Spans` needing `Stage=="completed" && SemanticType==llm_completion && ResponseBody!=nil` (`openbox-core .../goal_alignment_session.go:64-88`); dev sessions are span-less (ADR-0013) | phase 3: one minimal wire span on `TurnCompleted`, content-gated, redacted |

Phase 3 partially reverses ADR-0013 (spans return for exactly one carrier) and widens egress → **ADR-0018 lands first**.

## Phases

Strictly sequential: phases 2 and 3 both edit `client/payload.go`, `client/testdata/golden/` and the schema (file ownership).

| # | Phase | Effort | Status | Blocks on |
|---|---|---|---|---|
| 1 | [ADR-0018 — dev-turn content carrier](phase-01-adr-0018-dev-turn-content-carrier.md) | 1.5h | pending | — |
| 2 | [ActivityCompleted `status` end-to-end](phase-02-activity-status-field.md) | 3h | pending | 1 |
| 3 | [Assistant-turn span end-to-end](phase-03-assistant-turn-span.md) | 4h | pending | 1, 2 |
| 4 | [Docs + contract + CLAUDE.md reconciliation](phase-04-docs-and-contract-reconciliation.md) | 2h | pending | 2, 3 |
| 5 | [Manual verification guide](phase-05-manual-verification-guide.md) | 1.5h | pending | 4 |

## Decisions locked here (rationale in the phase files)

1. **Assistant text source = `Stop`/`SubagentStop.last_assistant_message`** (hook payload,
   content-gated like `Prompt`), NOT the transcript projection. Deviates from the brief on
   evidence: the provider's docs call it the recommended source (the transcript lags) and it
   leaves `usage.go`'s INV-2 allowlist and its load-bearing sentinel
   `TestFinops_NoContentOnWire` (`adapters/claude-code/usage_test.go:485`) **untouched**
   (`plans/reports/research-260813-2215-session-content-capture-gaps.md:78-80,92-93`).
   Fallback pre-decided in phase 3 if the field is absent.
2. **`semantic_type` cannot be asserted on the wire.** Core recomputes it for every span
   (`openbox-core .../governance_workflow.go:302-304`) and the only paths to `llm_completion`
   require `http.method:POST` + an LLM-domain `http.url` (`.../content/session.go:326-333,451-476`).
   The span therefore carries **synthesized classification keys** — named in ADR-0018 as OD-0018-1.
3. **Codex deferred** for the span; Claude Code first-class. It binds no assistant-text field, its
   `Stop` is deliberately unwired (`adapters/codex/capabilities.go:15`), and its only turn activity
   is the SessionEnd rollup — one message, wrong granularity for AGE.
4. **`status` on tool `ActivityCompleted` only** — never turns (core excludes `llm_completion`
   from tool metrics), never lifecycle (`payload.Status` also writes
   `governance_events.workflow_status`, `openbox-core .../storage_event.go:416-418`).
5. Adapter-facing schema → **v1.2** (additive, ADR-0014 precedent). Verification is **manual**
   per user decision; unit + conformance stay required gates.

## Scout evidence

- [scout-01 — read side: openbox-fe + openbox-backend](scout/scout-01-read-side-fe-backend.md)
- [scout-02 — write side: core + sdk-python + shift-left](scout/scout-02-write-side-core-sdk-shiftleft.md)
- [research — session content-capture gaps](../reports/research-260813-2215-session-content-capture-gaps.md)

## Unresolved questions

1. **OD-0018-1 (needs a human):** accept synthesized `http.method`/`http.url` on the turn span
   as classification keys, or file the ~3-line openbox-core ask (honour wire `semantic_type`, or
   read assistant text from the `llm_completion` activity) and leave both widgets empty until it
   lands? Plan assumes accept-and-document; reversing it deletes phase 3 only.
2. Does Claude Code's `PostToolUse` fire for FAILED tool calls, and what structural marker does
   it carry (`tool_response.is_error`? `exit_code`? a separate `PostToolUseFailure` hook)? Both
   branches pre-decided in phase 2 step 1; a third branch (fires on failure, no structural
   marker) is stop-and-replan.
3. Alignment needs **finops ON too** — `emitTurn` is finops-gated
   (`adapters/claude-code/hookrun.go:174`) and skipped when the window has no usage. Accept as a
   documented limit, or decouple the turn pair from the finops gate (separate decision)?
4. Out of scope, worth filing: core's success derivation is dead for **every** current producer
   (the Python SDK's `activity_completed()` has no `status` param); AGE needs LlamaFirewall
   reachable (`LlamaFirewallHost` empty ⇒ `performTraceCheck` returns nil ⇒ never checked).
