---
title: Tool executions became Activities on the core wire
date: 2026-08-11
summary: "Reshaped ToolCall/ToolResult onto ActivityStarted/ActivityCompleted, retired the fabricated hook-span layer, and found that core's dedupe key had been silently swallowing the completed half of every tool call."
---

# Tool executions became Activities on the core wire

## What happened

Executed the four-phase plan `260811-0245-tool-activity-event-shape`. Tool events
moved off the base SDK's hook-span envelope onto the activity lifecycle:
`ToolCall` → `ActivityStarted`, `ToolResult` → `ActivityCompleted`, both span-less
and hook-less. `client/hookspan.go` and `client/spanbuilder.go` deleted, and with
them ADR-0004's standing obligation to hand-maintain a Go mirror of a Python
contract nothing could mechanically check.

The premise that made it defensible: the base SDK's "hooks are always
`ActivityStarted`" rule binds runtimes that have in-process OpenTelemetry and a
real span to attach. A Claude Code or Codex hook is a short-lived separate
process with no OTel at all, so the span was hand-fabricated — deterministic
16-hex `span_id`, 32-hex `trace_id` from the session id — purely to satisfy a
shape. Nothing was measuring anything.

## The discovery that reframed the change

Mid-implementation, while checking whether core would store the second row:
core's idempotency check keys on
`(agent_id, workflow_id, run_id, activity_id, event_type)`
(`activities/governance/validation.go:96`).

Under the old shape a tool call's two halves matched on **all five** — same
`activity_id` by design, both `ActivityStarted`. So the `ToolResult` POST hit the
existing-event branch (`governance_workflow.go:228-231`) and never created a row.
The shared `span_id` that ADR-0004 chose deliberately as the pairing mechanism
was also what made the span-dedup check see nothing new.

The completed half of every tool call was substantially a no-op: no row, no
independent evaluation. This wasn't a bug in core — it was core's idempotency
working exactly as specified against a payload that told it two different facts
were the same event. It turned the change from "the header is wrong" into "we
have been silently discarding half our governance coverage."

## Decisions

**`error` omitted, and criterion 3 left partially unmet.** Two independent
blockers: the frozen adapter schema has no failure field, and core reads
`payload.Error` only for `WorkflowFailed` (`storage_event.go:281-286`) — an
`ActivityCompleted.error` is decoded and discarded. Shipping it would have been a
field with neither producer nor consumer. The honest consequence — a failed tool
call is indistinguishable from a successful one on the wire — is recorded in
ADR-0013 instead of being papered over. `attempt` omitted for the same class of
reason: permanently null in a stateless hook process.

**MAPPING.md's E7-S2 `semantic_type` claim deleted, not restated.** It had no
owner, was already contradicted by observed data, and is now moot — no span means
nothing to classify. An unowned claim in a governance product is worse than an
acknowledged gap.

**Renamed `lifecycleWireType` → `wireTypeFor`.** It maps every event type now;
the old name asserted the opposite.

## Found in code review

`FindByWorkflowRunActivity` (openbox-core
`internal/datastore/governance_event_pgx.go:74-87`, via
`services/governance.go:291`) backs the approval-status poll and filters on
`(workflow_id, run_id, activity_id)` with no `event_type` and no `ORDER BY`, then
`.One()`. That key was unique per tool call before; two rows now share it.

The primary hold is safe — escalation and polling are pre-execution, so the
completed row doesn't exist yet. The exposure is the retry after a completed
attempt, which is exactly the path `operation_id` exists to support: the poll may
resolve the completed row, whose `approval_expiration_time` is NULL, so
`Decided()` reads false and a real grant goes unconsumed. Out of scope here (the
plan forbids openbox-core changes); recorded in ADR-0013 and spun off.

## What went right

Writing the approval-key pin test **first**, before touching anything, was the
single best decision. `activity_id` is the approval key and every other test in
the package builds its expectation from the same function it exercises — a
derivation that changed consistently would have passed them all silently. The pin
test holds a literal hash and proved the id is byte-identical across the refactor.

Verifying rather than assuming also paid: the span's `request_body`/`response_body`
turned out to have carried **nothing** — no adapter ever set them, and both
mappers have tests asserting they stay empty. That let `data-and-privacy.md`
describe the narrowing as measured instead of hoped.

## Next steps

Phase 04's live run did not happen — the seven-container local stack wasn't
available. All gated claims are explicitly unreleased: MAPPING.md §7 opens with
"NOT YET RUN", CLAUDE.md says implemented-and-unit-verified, ADR-0013 carries the
load-bearing assumption as unproven.

```bash
./testbed/env.sh mint && ./testbed/run-all.sh
```

Then release those claims, and decide who owns the openbox-core approval-poll fix.

> Historical work record — not durable authority. Prefer docs/specs/ADRs for current decisions.
