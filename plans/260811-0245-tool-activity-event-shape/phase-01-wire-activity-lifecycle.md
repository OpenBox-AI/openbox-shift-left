# Phase 01 — Wire: tool events onto the activity lifecycle

## Context links

- Parent: [plan.md](plan.md)
- Evidence: [synthesis-findings.md](research/synthesis-findings.md), [scout-01](scout/scout-01-shift-left-current-shape.md)
- Supersedes the tool rows of `docs/adr/ADR-0004-base-wire-unification.md` (recorded in Phase 03)
- Dependencies: none. Blocks Phase 02, 03, 04.

## Overview

- **Date:** 2026-08-11
- **Description:** Re-serialize `ToolCall`/`ToolResult` from the paired hook-span
  envelope onto `ActivityStarted`/`ActivityCompleted` lifecycle events — hook-less,
  span-less, each independently evaluated by core.
- **Priority:** P1
- **Implementation status:** complete
- **Review status:** not started

## Key insights

1. **Core needs no change.** `ActivityStarted` and `ActivityCompleted` are both on the
   accept-list (`internal/api/governance.go:273-286`), both are guardrails-eligible
   (`governance_workflow.go:429-431`), and both take the NORMAL path — one
   `governance_events` row each, full OPA + Guardrails + AGE. Core already *decodes*
   `activity_id`, `activity_type`, `activity_input`, `activity_output`, `attempt`,
   `start_time`, `end_time`, `duration_ms` and `error`
   (`internal/content/governance.go:186-236`) — which is why no core change is needed.
   Of those, this phase *sends* everything except `start_time`/`end_time` (insight 4).
2. **The adapter-facing contract does not move.** `DevEvent`, `Span`, the schema, the
   mappers, the spool and the duration stash are untouched — only the client→core
   serialization changes. Same separation ADR-0004 established.
3. **`activity_id` and `workflow_id` must not change, and `ApprovalKeyFor` shares their
   derivation.** `activity_id` is operation-derived (`hookActivityID:472` ←
   `Span.OperationID`), it is the approval key, and core scopes its bypass grants by it.
   `client/approval.go:52-57` builds the approval poll key from `hookWorkflowID:463` +
   `hookActivityID` — deliberately, so a poll can never address a different row than the
   escalation created. Both functions survive this phase, renamed and shared
   (validation decision 1). `hookWorkflowID` is byte-identical to
   `buildLifecyclePayload`'s own derivation (`payload.go:159-162` vs `:463-468`), so
   collapsing the two paths changes no id — and removes a duplicate.
4. **Timing becomes the client's job — `duration_ms` only.** Today core derives the row's
   `duration_ms` from the stored span; with no span, the `ActivityCompleted` payload must
   carry it or the dashboard row shows no duration (`workflow-tree-view.tsx:643,703`
   reads `event.duration_ms` directly). `ev.StartedAt` is already threaded onto the
   `ToolResult` by the duration stash (`hookflow/engine.go:72-83`), so the data is
   present. `start_time`/`end_time` are **not** sent: `*float64` of unverified unit with
   no known consumer (validation decision 2).
5. **The tool path gains `workflow_type` for the first time.** Routing tool events
   through the typed struct fixes the divergence from the base SDK's
   `ActivityContext.to_payload_fields()` at no extra cost.

## Requirements

- `ToolCall` → `event_type=ActivityStarted`; `ToolResult` → `event_type=ActivityCompleted`.
- Neither carries `spans`, `span_count` or `hook_trigger`.
- Both carry `source`, `workflow_id`, `run_id`, `workflow_type="developer-session"`,
  `activity_id`, `activity_type` (tool name), `timestamp`, `metadata`.
- `ActivityStarted` carries `activity_input` (structural + content-gated escalation
  context, exactly as today).
- `ActivityCompleted` carries `activity_output`, `duration_ms`, and `error` when the tool
  failed. Not `start_time`/`end_time`.
- `ApprovalKeyFor` produces byte-identical output before and after the change.
- `Emit` stays 1 DevEvent → 1 POST → 1 verdict. `EventID` stays the idempotency key.
- Content posture unchanged (INV-2): no command text, file body or tool output on the
  observe path; the Tier-2 escalation's `Content.ToolInput` keeps riding
  `activity_input` content-gated.

## Architecture

One serialization path replaces two. `buildPayload`'s tool/lifecycle split collapses:
every `DevEvent` becomes a `governanceEventPayload` struct, and `lifecycleWireType`
becomes the single event-type mapping table.

```
DevEvent ─► lifecycleWireType ─► governanceEventPayload ─► json.Marshal ─► sign ─► POST
             SessionStarted   → WorkflowStarted
             SessionEnded     → WorkflowCompleted
             PromptSubmitted  → SignalReceived(prompt_submitted)
             CommitCreated    → SignalReceived(commit_created)
             Deploy           → SignalReceived(deploy)
             ToolCall         → ActivityStarted     ← new
             ToolResult       → ActivityCompleted   ← new
```

The map-based hook envelope (`buildHookPayload`) and its field-name constants disappear,
so the two marshalling regimes and their divergent key orders collapse to one — which
also removes the reason the golden fixtures had to pin two orders.

## Related code files

| File | Change |
|---|---|
| `client/payload.go` | `buildPayload` (split removed), `lifecycleWireType` (+2 rows), `governanceEventPayload` (+ activity/duration/error fields), `structuralActivityInput` (reused for ActivityStarted), new `structuralActivityOutput`, rename `hookWorkflowID`→`workflowIDFor` / `hookActivityID`→`activityIDFor` (**keep both**), delete `buildHookPayload`/`buildHookSpan`/`hookSpanID`/`sessionTraceID`/`spanData`/hook field constants |
| `client/approval.go` | `ApprovalKeyFor` call sites updated to the renamed shared derivations; **behavior must not change** |
| `client/event.go` | no change (adapter-facing contract frozen). `Span.Stage` becomes an unread field, retained deliberately — validation decision 4 |
| `client/payload_test.go`, `payload_lifecycle_test.go`, `payload_hook_test.go` | hook-path tests replaced by activity-path tests |
| `client/golden_test.go`, `client/testdata/golden/` | 4 `hook_*.json` fixtures replaced by `activity_started_*.json` / `activity_completed_*.json` |
| `client/acceptancetest/acceptance_test.go` | wire-shape expectations |
| `adapters/*`, `adapters/common/hookflow/*`, `contracts/dev-event/schema/` | **no change** — assert this, do not edit |

## Implementation steps

1. **First, before any refactor:** add a test that pins the current output of
   `hookWorkflowID`, `hookActivityID` and `ApprovalKeyFor` for a fixed `DevEvent`. These
   are the approval key. The test must survive the rename and keep passing — it is the
   only thing standing between this refactor and unaddressable approvals.
2. Extend `governanceEventPayload` with the fields core already decodes: `ActivityID
   *string`, `ActivityInput`/`ActivityOutput` (`json.RawMessage`), `Attempt *int`,
   `DurationMs *float64`, `Error`. All `omitempty`. **No `StartTime`/`EndTime`** —
   validation decision 2.
3. Add `EventToolCall → wireActivityStarted` and `EventToolResult →
   wireActivityCompleted` to `lifecycleWireType`; add the `wireActivityCompleted`
   constant. Keep the error return for an unmapped type.
4. Rename `hookWorkflowID` → `workflowIDFor` and `hookActivityID` → `activityIDFor`,
   keeping both. Point `ApprovalKeyFor` (`client/approval.go:52-57`) at the new names and
   update its doc comment, which currently cites `buildHookPayload`.
5. Delete the `buildPayload` split so every event flows through the struct path. Use
   `workflowIDFor(ev)` for `WorkflowID` on **every** event — this replaces
   `buildLifecyclePayload`'s duplicate inline derivation (`payload.go:159-162`). Set
   `ActivityID` from `activityIDFor(ev)` and `ActivityType` from `activityLabel(ev)` for
   tool events only.
6. On `ActivityStarted`, attach `structuralActivityInput(ev)` (unchanged behavior).
7. Add `structuralActivityOutput(ev)` for `ActivityCompleted`: `bytes_read`,
   `bytes_written`, `lines_count` from `ev.Span`, and `exit_code` from the same source
   `buildMetadata` reads. Structural identifiers only — no output text (INV-2).
8. On `ActivityCompleted`, set `DurationMs` from `ev.StartedAt`→`ev.EndedAt` (falling
   back to `ev.Timestamp` for either, as `buildHookSpan` did at `payload.go:302-303`),
   in float milliseconds. Set `Error` when the result reports failure.
9. Delete `buildHookPayload`, `buildHookSpan`, `hookSpanID`, `sessionTraceID`, the
   `spanData` struct and the `fieldSource`/`fieldEventType`/… map-key constants. **Do not
   delete `workflowIDFor`/`activityIDFor`** (the renamed derivations).
10. Replace the hook golden fixtures with activity fixtures; keep one per tool kind
    (shell/file/mcp) per stage so the field mapping stays pinned.
11. `cd client && go test ./...`, then `go build ./cli/...` and `go test ./...` in each
    module listed by `go.work`.

## Todo list

- [x] Pin `hookWorkflowID` / `hookActivityID` / `ApprovalKeyFor` output in a test (step 1, do this first) — `client/approval_key_pin_test.go`, green before any refactor
- [x] `governanceEventPayload` fields (no `start_time`/`end_time`) — see deviations below for `attempt`/`error`
- [x] `wireTypeFor` (renamed from `lifecycleWireType`) + `wireActivityCompleted`
- [x] Rename to `workflowIDFor` / `activityIDFor`; repoint `ApprovalKeyFor` + its doc comment
- [x] Collapse `buildPayload`; single `workflowIDFor` for every event; wire `activity_id`/`activity_type`
- [x] `structuralActivityOutput` + `duration_ms` on ActivityCompleted (`error` not sent — deviation 2)
- [x] Delete hook payload builders, `hookSpanID`, `sessionTraceID`, `spanData`, `hookTypeFor`, `hookSpanShape`, `fileSpanName`, `spanPairKey`, field constants, stage constants
- [x] Regenerate golden fixtures — 6 activity fixtures (shell/file/mcp × started/completed)
- [x] Assert zero diff under `adapters/` and `contracts/` — `git diff --stat` empty for both
- [x] Per-module `go test`; approval-key pin test still green

### Deviations from the written phase, and why

1. **`attempt` omitted.** Listed in step 2 as a field core decodes. It has no
   source: the frozen adapter contract carries no attempt counter and a hook
   process is stateless, so it would serialize as permanently nil. The struct's
   own documented convention is to carry only what the client sets.
2. **`error` omitted.** Two independent reasons, both verified in the core
   checkout: the frozen `DevEvent`/`Span` contract has no failure or exit-code
   field, and core reads `payload.Error` **only** for `WorkflowFailed`
   (`storage_event.go:281-286`) — an `ActivityCompleted.error` is decoded and
   discarded. Core's `Error` is also `*ErrorInfo` (a struct with required
   `type`/`message`), not a string. Emitting a field with no producer and no
   consumer would be the overstatement `CLAUDE.md` forbids. Recorded as a limit
   for Phase 03's MAPPING.md field-home table; **this leaves acceptance criterion
   3's "on failure, `error`" unmet by design** — flagged for the user.
3. **`exit_code` kept**, promoted from `ev.Metadata` as step 7 specifies. Unlike
   `error` it has a real consumer (core stores `activity_output` as the row's
   `output` and runs Guardrails over it), so the promotion is live the moment an
   adapter supplies one. No adapter does today.
4. **`lifecycleWireType` renamed to `wireTypeFor`.** It now maps every event type,
   including the two activity ones; the old name asserts the opposite. Only
   `MAPPING.md:37` referenced it, and Phase 03 rewrites that line.
5. **`payload_hook_test.go` deleted here, not in Phase 02.** Every case in it
   asserted the retired shape, so Phase 01 could not end green while it existed.
   Its one non-hook-specific case — the `capBody` size cap, which was pinned only
   through the span's `request_body` — was re-homed to
   `payload_enrich_test.go` against the two surviving content fields
   (`activity_input.command`, `signal_args.prompt`), so the cap keeps its
   coverage.

### Verified, not assumed

- `activity_id` is byte-identical across the change, checked against the
  pre-change fixtures in git: `cc-act-210c9b44…` (file) and `cc-act-15a86e08…`
  (mcp) are the same before and after.
- The span's `request_body`/`response_body` carried **nothing** on the observe
  path. No adapter has ever populated either, and both mappers have tests
  asserting they stay empty (`adapters/claude-code/mapper_test.go:169`,
  `adapters/codex/mapper_test.go:207`). Phase 01's security item and Phase 03's
  "appears to have been unused" can be stated as measured.
- Core runs Guardrails over `activity_input` at stage "0" and `activity_output`
  at stage "1" (`internal/services/guardrail.go:180,192`), and
  `setOptionalPayloadFields` persists activity_id/type/input/output/duration_ms
  event-type-agnostically (`storage_event.go:258-294`). `duration_ms` reaches the
  row directly.
- `contracts/dev-event/conformance` passes with no edits, confirming the
  two-layer split is real (Phase 02 insight 3).

## Success criteria

1. A `ToolCall` payload is `ActivityStarted` with `activity_id`, `activity_type`,
   `activity_input`, `workflow_type`, and **no** `spans`/`span_count`/`hook_trigger`.
2. A `ToolResult` payload is `ActivityCompleted` with the same `activity_id`, plus
   `activity_output` and `duration_ms` — and no `start_time`/`end_time`.
3. `activity_id`, `workflow_id` and `ApprovalKeyFor`'s output are byte-identical to what
   today's code produces, proven by the step-1 pin test.
4. Exactly one derivation each for `workflow_id` and `activity_id` in the package; the
   serializer and `ApprovalKeyFor` both call it.
5. Golden fixtures cover shell/file/mcp × started/completed.
6. No file under `adapters/` or `contracts/dev-event/schema/` is modified.

## Risk assessment

| Risk | Mitigation | Signal it broke | Pre-decided response |
|---|---|---|---|
| `activity_id` or `workflow_id` derivation changes during the rename, orphaning approvals — `ApprovalKeyFor` is consumed by production adapter code (`adapters/claude-code/rewake.go`, `adapters/common/hookflow/gate.go`) | Step 1 pins all three outputs in a test **before** any refactor; the rename is mechanical only | The pin test fails, or the approval poll returns not-found after a grant, or rewake loops | Stop, restore the derivation, re-run 40-approvals |
| `duration_ms` computed wrong (sign, unit, or missing `StartedAt`) | Float milliseconds from the stash-threaded `StartedAt`; `buildHookSpan:302-303` is the precedent for the fallback chain | Dashboard shows blank, zero, or a negative duration | Fix the arithmetic; the field has one consumer and a visible failure mode, so this surfaces immediately |
| Core returns a cached verdict on a rewake retry (`governance_workflow.go:231`) because the same `(activity_id, ActivityStarted)` is re-POSTed, and with no span the span-based approval bypass at `:316` cannot fire | Enforcement is local (INV-3b) and the approval verdict is read from `/governance/approval`, not `/evaluate` — so this is advisory bookkeeping | Advisory sink records a stale `require_approval` after a grant | Accept as advisory noise; if it misleads operators, file a core-side follow-up rather than reshaping the wire |
| A dev-event field silently loses its home when the span goes away | Steps 5-6 enumerate every `Span` field and its destination; golden fixtures pin the result | A field present in today's goldens has no counterpart | Re-home to `metadata` (free-form, already the carrier for tokens/cost/lineage) |

**Assumption that may break:** that core stores an `ActivityCompleted` for an
`activity_id` whose `ActivityStarted` exists without merging or rejecting it. Verified
by reading, not by running. Signal: Phase 04 finds one row instead of two. Response:
proceed directly to the 3-POST fallback — **pre-authorized, validation decision 3** — and
add its ADR entry in Phase 03. No new decision needed.

## Security considerations

- INV-1: no key or seed touches the payload. Unchanged.
- INV-2: `activity_output` carries counts and an exit code, never output text;
  `activity_input` keeps its existing content gate for the escalation context only.
  The span's `request_body`/`response_body` channel disappears — **confirm it carried
  nothing on the observe path** (per `CLAUDE.md`, tool commands and file bodies never
  egress on observe events) so its removal is not a silent behavior change.
- Tier-1 local secret detection and redaction run on the enforce path, which this phase
  does not touch.
- The removal of span rows shrinks the egress surface; record that in Phase 03 rather
  than claiming a privacy improvement here without measuring it.

## Next steps

Phase 02 deletes the now-unreferenced hook-span mirror and its parity tests.

<!-- Updated: Validation Session 1 - duration_ms only (no start_time/end_time); hookWorkflowID/hookActivityID renamed and KEPT as shared derivations for ApprovalKeyFor; approval-key pin test is now step 1; 3-POST fallback pre-authorized; Span.Stage retained unread -->
