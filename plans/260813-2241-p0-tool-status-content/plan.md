--- title: "P0 telemetry enrichment + content-posture decision record" description: "Send tool success/failure status (fixes Tool Health 0%),
wire failure/lifecycle hooks, then draft that decision full-content-capture" status: superseded priority: P1 effort: 2d branch: main tags:
[telemetry, claude-code-adapter, contract, decision record, privacy-posture] created: 2026-08-13 ---

# P0 telemetry enrichment + content-posture decision record

> **Superseded 2026-08-13** by
> [260813-2314-dev-telemetry-and-content-posture](../260813-2314-dev-telemetry-and-content-posture/plan.md)
> (merged with 260813-2200; this plan's status/failure-hook design carries over as phases 02–03,
> the posture decision record renumbered to that decision as phase 07).

Decision basis: `plans/reports/research-260813-2215-session-content-capture-gaps.md`
(§Decision update — FULL CAPTURE posture, 2026-08-13). P0 = structural-only, **no new
content egress, no decision record needed**; that decision (drafted here, phase 04)
authorizes the content phases P1–P3 which are **out of scope** for this plan's
implementation.

## Outcome

1. Every `ActivityCompleted` tool event carries `status: completed|failed` → core's
   `ExtractToolMetric` computes real success → Tool Health Matrix stops showing 0%.
2. Failures, subagent spawns, permission denials, API errors become visible events.
3.  drafted (Proposed) covering P1–P3.

## Constraints / non-goals

- No content field added in phases 01–03 (INV-2 intact; SL3-SEC-3 untouched until that decision lands).
- `activity_id` / `event_id` derivation byte-identical (approval_key_pin_test, goldens for identity).
- No new table/endpoint/service; new event types ride stock `SignalReceived` (INV-8).
- Do NOT implement P1–P3 (tool output / assistant text / thinking) — decision record draft only.

## Phases

| # | Phase | Status | Effort | Depends |
|---|---|---|---|---|
| 01 | [Status on tool results](phase-01-status-on-tool-results.md) | pending | 3h | — |
| 02 | [Failure + lifecycle hooks](phase-02-failure-and-lifecycle-hooks.md) | pending | 5h | 01 |
| 03 | [Verification + docs](phase-03-verification-and-docs.md) | pending | 4h | 01, 02 |
| 04 | [that decision content-posture draft](phase-04-content-posture.md) | pending | 3h | — (sequenced last per request) |

## Acceptance criteria

- [ ] `ActivityCompleted` (tool) wire bytes carry `"status":"completed"`; failure path carries `"failed"` — asserted on outbound bytes in conformance.
- [ ] `PostToolUseFailure`, `SubagentStart`, `PermissionDenied`, `StopFailure` wired end-to-end (hook → event → spool/flush), fail-open preserved (INV-3).
- [ ] Task tool calls carry `subagent_type` metadata; no prompt/description egress in P0.
- [ ] All 11 modules green under `-race`; both cross-compiles pass; goldens updated deliberately with review note.
- [ ] Known side effect verified & documented: core `storage_event.go:416` copies `status` → `workflow_status` column on activity rows (accept or file core ask).
- [ ] `that decision` drafted, self-consistent with research report + that decision/0015/0017 conventions; `README.md` indexed.
- [ ] Testbed assertions added for status + failure events (run deferred — no local stack; noted per repo convention).

## Key references

- Research + decision: `plans/reports/research-260813-2215-session-content-capture-gaps.md`
- Widget root causes: `plans/260813-2200-dashboard-widget-telemetry-gaps/scout/scout-02-write-side-core-sdk-shiftleft.md`
- Contract: `contracts/dev-event/MAPPING.md` (§3 field map; `status` currently listed "Retired with the span layer" — un-retire deliberately)
- Core read side (verified 2026-08-13): `openbox-core/internal/services/activities/observability/errors.go:301-337` (IsSuccess), `internal/services/activities/governance/storage_event.go:416-417` (Status→WorkflowStatus copy)

## Unresolved questions

1. Does `PostToolUse` also fire when a tool fails, or only `PostToolUseFailure`? (Assumed: only failure hook. Verify against real binary in phase 03; if both fire, accept double completed-row until core dedupe ask lands — over-report, never lose.)
2. Minimum Claude Code version for `PostToolUseFailure`/`SubagentStart`/`StopFailure` hook names; behavior of older versions on unknown hook keys in settings (verify in phase 02; degrade = hook never fires, fail-open).
3. `workflow_status` populated on activity rows — cosmetic or harmful downstream? (Phase 03 verify; likely file small core ask to scope the copy to Workflow* events.)
4. Codex parity: Codex adapter has no failure-hook equivalent → its tools stay success-unknown. Documented gap in; separate pass later.
