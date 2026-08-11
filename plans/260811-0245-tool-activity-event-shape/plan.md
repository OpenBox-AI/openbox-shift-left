---
title: "Tool executions become Activities on the core wire"
description: "Re-map ToolCall/ToolResult from paired hook-span events onto the ActivityStarted/ActivityCompleted lifecycle, and retire the hook-span machinery."
status: implemented — live verification pending
priority: P1
effort: 9h
branch: feat/realtime-event-delivery
tags: [wire-contract, telemetry, adr, breaking-observability]
created: 2026-08-11
---

# Tool executions become Activities

## Why

For Claude Code and Codex every unit of work is a tool execution, and there is no
OpenTelemetry in-process to produce spans — `client/spanbuilder.go` fabricates one by
hand. Today both stages of a tool call ride `ActivityStarted`+`hook_trigger`,
distinguished only by `spans[0].stage`, which puts a `stage:"completed"` span under an
`ActivityStarted` header and never emits an `ActivityCompleted` at all.

**Decision (OD, 2026-08-11):** a tool execution *is* an Activity. `ToolCall` →
`ActivityStarted`, `ToolResult` → `ActivityCompleted`, both hook-less and span-less,
both fully evaluated by core. The span layer is retired.

Evidence and the rejected alternatives: [synthesis-findings.md](research/synthesis-findings.md).

## Accepted trade-off

Dev sessions will produce **zero `spans` rows**. Lost: `semantic_type` classification,
span-level Merkle leaves, and the span as a field carrier. Gained: a wire that matches
the model, `workflow_type` present on tool events for the first time, and the retirement
of `client/hookspan.go` — the hand-maintained mirror of the base hook contract that
ADR-0004 flags as a standing unverifiable obligation.

Event volume is unchanged (2 POSTs per tool call, as today).

## Non-goals

- The adapter-facing contract (`contracts/dev-event/schema/dev-event.schema.json` v1.0)
  is **unchanged** — no `schema_version` bump. Adapters, mappers, the spool, the
  duration stash and the enforce path are untouched.
- No change to openbox-core, openbox-sdk-python, openbox-temporal-sdk-python or
  openbox-fe. The shape uses accept-listed event types and payload fields core already
  decodes, and the dashboard already renders both types first-class.
- Local enforcement (INV-3b) and the approval loop keep their current behavior.

## Phases

| # | Phase | Effort | Status |
|---|---|---|---|
| 01 | [Wire: tool events onto the activity lifecycle](phase-01-wire-activity-lifecycle.md) | 3h | complete |
| 02 | [Retire the hook-span machinery](phase-02-retire-hook-span-machinery.md) | 1.5h | complete |
| 03 | [Contracts, ADR and docs](phase-03-contracts-adr-docs.md) | 2h | complete |
| 04 | [Prove it on a live stack](phase-04-testbed-verification.md) | 2.5h | scripts done · live run pending |

Dependencies: 01 → 02 → 03; 04 depends on 01+02 and closes 03's claims.

## Acceptance criteria

1. One tool call produces exactly two `governance_events` rows for one `activity_id`:
   `ActivityStarted` then `ActivityCompleted`, each independently evaluated.
2. `activity_type` is the tool name; `activity_id` is unchanged from today (still
   operation-derived, still the approval key, still stable across a retry).
3. `ActivityCompleted` carries `activity_output`, `duration_ms` and, on failure, `error`
   — so the dashboard row shows a real duration without a span to derive it from.
   `start_time`/`end_time` are deliberately not sent (validation decision 2).
4. `workflow_type="developer-session"` is present on tool events.
5. No payload contains `spans`, `span_count` or `hook_trigger`.
6. The E9 approval loop still holds, escalates, and consumes a granted approval on
   rewake, proven live.
7. `testbed/run-all.sh` passes against a real local stack.

## Outcome (2026-08-11)

Phases 01–03 complete; phase 04's script changes complete, its **live run not
performed** — the seven-container local stack was not available and the user
elected to run it themselves. All 11 workspace modules build, vet and test clean.

**Acceptance criteria:** 2, 4, 5 met and verified; 3 met except `error` (see
below); 1, 6, 7 need the live run.

Two deviations, both recorded in the phase files:

1. **`error` is not sent**, so acceptance criterion 3 is partially unmet by
   design. It has no source (the frozen adapter contract has no failure field)
   **and** no consumer (core reads `payload.Error` only for `WorkflowFailed`,
   `storage_event.go:281-286`). A failed tool call is therefore indistinguishable
   from a successful one on the wire — a real gap, recorded in ADR-0013 rather
   than papered over with a field that does nothing. Closing it needs a failure
   signal in the adapter schema, which is its own decision.
2. **`attempt` is not sent** — permanently null in a stateless hook process.

## Found in review, not in the plan

Core's approval-status poll resolves `(workflow_id, run_id, activity_id)` with no
`event_type` filter and no ordering (`governance_event_pgx.go:74-87`). That key
was unique per tool call before this change; two rows now share it. The primary
hold is unaffected (it polls pre-execution), but a **retry after a completed
attempt** — the path `operation_id` exists to support — may resolve the completed
row and read a real grant as undecided. Fix is core-side and out of this plan's
scope; recorded in ADR-0013 Consequences and MAPPING.md §7 item 6.

## Open questions

- The openbox-core approval-poll fix above.
- Whether openbox-backend's read side needs any change (carried from
  [synthesis-findings.md](research/synthesis-findings.md) §"Unresolved").
- Whether the deployed core is ahead of the local checkout on semantic-type
  classification — now **moot for dev sessions**, which send no spans; MAPPING.md's
  E7-S2 claim was deleted as unowned rather than restated.

## Validation Log

### Session 1 — 2026-08-11

**Questions asked:** 4 · **Tier:** Standard (4 phases → Fact Checker + Contract Verifier)

#### Verification Results

- **Claims checked:** 46
- **Verified:** 43 | **Failed:** 1 | **Unverified:** 0 (2 flagged, both resolved as
  false positives)

##### Failures

1. [Contract Verifier] `hookWorkflowID` — Phase 01 step 8 marked it for deletion, but
   `client/approval.go:52-57` (`ApprovalKeyFor`, production) calls it *and*
   `hookActivityID`. `ApprovalKeyFor` has 13 references including production adapter code
   (`adapters/claude-code/rewake.go`, `adapters/common/hookflow/gate.go`). Deleting it
   breaks the approval poll key. Resolved by decision 1 below.

##### Resolved false positives

- `adapters/common/hookflow/duration.go:118` and
  `contracts/dev-event/conformance/schema_guard_test.go:66` name `buildPayload` in
  **comments** across module boundaries — not callers (unexported, different module).

##### Spot-checked and verified

`client/`: `buildMetadata:550`, `rfc3339Nanos:726`, `firstNonEmpty:717`, `capBody:706`,
`stripContent:680`, `structuralActivityInput:581`, `hookActivityID:472`,
`hookWorkflowID:463`, `activityLabel:497`, `lifecycleWireType:201`,
`buildHookPayload:227`, `buildLifecyclePayload:158`, `TraceContextFrom` +
`newHexID` (spanbuilder.go), `sessionTraceID:406`.
`openbox-core`: `isValidGovernanceEventType` governance.go:273, `guardrailsEligible`
governance_workflow.go:429, `GovernanceEventPayload` content/governance.go:186,
`FindByWorkflowRunActivityID` governance_event_pgx.go:57, spans FK
spans.bob.go:33-34, `SpanCount` write storage_event.go:140, Merkle `IsNewEvent`
merkle.go:34, `storeSpanToTable` storage_spans.go:21 with its single production caller
storage_event.go:69.
Docs/tests: `docs/architecture.md:117` "Assurance — what the evidence proves",
`docs/testbed/e2e.md`, `TestHookRealtimeDelivery` cli/cmd/openbox/main_test.go:479.

Correction: `activityLabel` is at `payload.go:497`, not `:498` as
[scout-01](scout/scout-01-shift-left-current-shape.md) states.

#### Confirmed decisions

1. **Approval-key ownership:** one shared derivation. Rename `hookWorkflowID` →
   `workflowIDFor` and `hookActivityID` → `activityIDFor`, keep both, and have the
   collapsed serializer *and* `ApprovalKeyFor` call them. Removes the duplicate
   derivation in `buildLifecyclePayload`. Pin the current output values in a test before
   touching anything.
2. **Timing:** `duration_ms` only. `start_time`/`end_time` are not sent — unknown unit,
   no known consumer, and `duration_ms` is the field the dashboard reads
   (`workflow-tree-view.tsx:643,703`). The unit-verification step is dropped.
3. **Phase 04 fallback pre-authorized:** if the live run shows core merging or rejecting
   the second row, implementation proceeds directly to the 3-POST fallback
   (`ActivityStarted` → `ActivityCompleted` → `ActivityCompleted`+`hook_trigger` with the
   span) without a new decision. It needs its own ADR entry, since it diverges from the
   base SDK's hook rule.
4. **Frozen schema:** `contracts/dev-event/schema/` is untouched, no `schema_version`
   bump. `Span.Stage` becomes an unread field and is retained deliberately; MAPPING.md
   §3's field-home table is the authority on what the serializer actually reads.

#### Action items

- [x] Propagate decisions 1-4 to phase files
- [ ] Phase 01: pin `activity_id` + `workflow_id` values in a test *before* refactoring
- [ ] Phase 03: the 3-POST fallback needs an ADR entry if Phase 04 triggers it

### Whole-Plan Consistency Sweep

- **Files reread:** plan.md, phase-01, phase-02, phase-03, phase-04
- **Decision deltas checked:** 4
- **Reconciled stale references:** 7 claim families across 5 files —
  (1) timing fields as things we *send*, in plan.md acceptance criteria + phase-01 ×5 +
  phase-03 + phase-04; (2) `hookWorkflowID` marked for deletion, phase-01 steps + related-code
  table; (3) phase-02's "no production caller" claim, now naming `client/approval.go`;
  (4) phase-04's unit-verification risk row, replaced by an arithmetic row;
  (5) the fallback response, "stop and replan" → pre-authorized, in phase-01 + phase-04;
  (6) `Span.Stage` / frozen-schema treatment, phase-01 + phase-02 + phase-03;
  (7) phase-01 insight 1's ambiguity between what core *decodes* and what we *send*.
- **Unresolved contradictions:** 0

Verified post-sweep: all four phase files carry the 12 required sections in order and
exactly one `Validation Session 1` marker; no file still describes `start_time`/`end_time`
as sent, `hookWorkflowID` as deleted, or the fallback as needing a new decision.
