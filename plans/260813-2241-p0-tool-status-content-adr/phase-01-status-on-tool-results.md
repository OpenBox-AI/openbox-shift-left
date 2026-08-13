# Phase 01 — Status on tool results

## Context links

- Parent: [plan.md](plan.md) · Depends: none
- Root cause: `plans/260813-2200-dashboard-widget-telemetry-gaps/scout/scout-02-write-side-core-sdk-shiftleft.md` §Widget 3
- Contract: `contracts/dev-event/MAPPING.md:229,239-241` (`exit_code` promotion live; `status` "Retired with the span layer")

## Overview

- Date: 2026-08-13 · Priority: P1 · Status: pending · Review: n/a
- Emit top-level `status:"completed"` on every tool `ActivityCompleted`. Core already reads it
  (`errors.go:332-334`: `IsSuccess = *payload.Status == "completed"`), so this client-only change
  makes Tool Health SUCCESS% real for new sessions. Failure value (`"failed"`) arrives in phase 02.

## Key Insights

- Core payload field exists: `content.GovernanceEventPayload.Status *string` (governance.go:206). Documented
  for Workflow events, but `ExtractToolMetric` explicitly reads it on `ActivityCompleted` — reuse, don't invent.
- `status` is a lifecycle enum, not content → INV-2-clean, no gate, no ADR.
- MAPPING.md lists `status` retired with the span layer (was per-span OTel status). Re-introducing it is a
  deliberate un-retire at payload level — document why in the MAPPING row, not a silent add.
- Goldens pin wire bytes: adding a key changes fixtures on purpose. `deriveID` inputs unchanged → event ids stable.

## Requirements

1. `client.DevEvent` gains `Status string` (json `status,omitempty`) — survives spool round-trip (additive JSON).
2. `buildPayload` maps it to payload `Status *string` only when non-empty.
3. Mapper sets `ev.Status = "completed"` in `case HookPostToolUse` only (never on started, never on turn/session events).
4. Value set is closed: `completed|failed` — enforce via test, mirror `enumOr` discipline.

## Architecture

Hook stdin → `Mapper.Map(HookPostToolUse)` sets `Status` → spool → flush `buildPayload` → wire `"status"` →
core `storage_event` row + `ExtractToolMetric.IsSuccess`. No new files; no engine (hookflow) change.

## Related code files

- `client/event.go` — DevEvent struct (+Status; doc comment: lifecycle enum, INV-2-clean)
- `client/payload.go` — payload struct `Status *string` + buildPayload mapping
- `adapters/claude-code/mapper.go` — `case HookPostToolUse`
- `client/testdata/golden/activity_*_completed.json` — regenerate (started fixtures untouched)
- `client/approval_key_pin_test.go` — must pass UNCHANGED (identity untouched)
- `contracts/dev-event/schema/` — add `status` enum to event schema (completed half)
- `contracts/dev-event/MAPPING.md` — §3 row: `status` → payload `status`, completed only + un-retire note

## Implementation Steps

1. Add `Status` to DevEvent + payload struct + buildPayload mapping (empty ⇒ absent, matching omitempty posture).
2. Mapper: set on PostToolUse. Add mapper unit test: started carries none; completed carries `completed`; turn/session/signal events never carry one.
3. Update schema + regenerate goldens; eyeball diff = exactly one added key per completed fixture.
4. Conformance case: real hook binary → stub `/evaluate`; assert `"status":"completed"` present on outbound ActivityCompleted bytes and ABSENT on ActivityStarted.
5. MAPPING.md row + un-retire rationale.

## Todo list

- [ ] DevEvent.Status + payload wiring
- [ ] Mapper PostToolUse
- [ ] Unit + conformance cases
- [ ] Schema + goldens
- [ ] MAPPING.md row

## Success Criteria

- Outbound ActivityCompleted bytes carry `"status":"completed"` (conformance-asserted); event ids byte-identical; `go test -race ./...` green in touched modules.

## Risk Assessment

- **workflow_status side effect** (core copies Status → row's workflow_status unconditionally, storage_event.go:416) — accepted for P0, verified in phase 03; rollback = stop setting the field (additive, no reader breaks).
- Golden churn masking an accidental identity change → mitigated by approval_key_pin_test + reviewing diff scope.

## Security Considerations

- Enum only; no content, no gate interaction; `stripContent` untouched. No new egress class.

## Next steps

Phase 02 adds the `failed` value + its producer hook.
