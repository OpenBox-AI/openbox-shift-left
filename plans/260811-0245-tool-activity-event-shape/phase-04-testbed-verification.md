# Phase 04 — Prove it on a live stack

## Context links

- Parent: [plan.md](plan.md)
- Depends on: [Phase 01](phase-01-wire-activity-lifecycle.md), [Phase 02](phase-02-retire-hook-span-machinery.md)
- Closes the unverified claims in [Phase 03](phase-03-contracts-and-docs.md)
- Suite docs: `docs/testbed/e2e.md`; entry point `testbed/run-all.sh`

## Overview

- **Date:** 2026-08-11
- **Description:** Drive real headless Claude Code and Codex sessions against a real local
  OpenBox stack and assert what actually arrived — two activity rows per tool call, a
  rendered duration, a still-working approval loop, and no span rows.
- **Priority:** P1 — the repo's rule is that unit tests are not evidence that a hook works.
- **Implementation status:** script changes complete; **live run NOT performed** (no local stack available)
- **Review status:** not started

## Key insights

1. **This phase is the only thing that can validate the change.** Every load-bearing fact
   about core's ingest was established by *reading* core, including the central assumption
   that an `ActivityCompleted` for an existing `activity_id` creates its own row rather
   than being merged, rejected, or deduped. Reading is not running.
2. **Two existing scripts assert the old shape and must change.**
   `testbed/20-capture.sh:63` asserts `>=4` `ActivityStarted` rows and `:65-77` asserts
   span rows by class (`file_`, `shell_command`, `file_write`, MCP). `testbed/25-realtime.sh`
   counts `ActivityStarted` as its progress signal (`:71`, `:90`). Both become
   activity-pair assertions.
3. **One script prints spans as diagnostics, not assertions** (`testbed/30-enforce.sh:84`
   dumps `row_to_json` from `spans`). It will print nothing; that is expected, but the
   diagnostic should be re-pointed at `governance_events` so a failing enforce run still
   shows something useful.
4. **The realtime flusher on this branch is the delivery path under test.**
   `TestHookRealtimeDelivery` covers the binary against a mock core, and
   `testbed/25-realtime.sh` has never run against a live stack. This phase is the first
   time both the new shape and the realtime trigger meet a real core — keep the two
   failure modes distinguishable when something breaks.
5. **The approval loop is the highest-stakes regression surface.** It is shipped and
   verified today. `activity_id` is unchanged by design, and the approval record is keyed
   on `(workflow_id, run_id, activity_id)` with no span dependency — but the span-based
   bypass at `governance_workflow.go:316` can no longer fire, so the grant must be
   consumed through the path `testbed/40-approvals.sh` and `70-approver-auto.sh` exercise.

## Requirements

- One tool call ⇒ exactly one `ActivityStarted` row and one `ActivityCompleted` row sharing
  one `activity_id`, in a real session.
- `governance_events.activity_type` is the tool name for both rows.
- `duration_ms` is present and plausible on the `ActivityCompleted` row; `start_time` and
  `end_time` are absent by design (validation decision 2).
- `ApprovalKeyFor`'s output is unchanged from before the refactor — the Phase 01 pin test
  is green *and* the live approval loop consumes a grant.
- `select count(*) from spans where session_id=…` is **0**, asserted deliberately rather
  than left implicit.
- Merkle: event leaves present for both rows; no span leaves.
- `40-approvals.sh` and `70-approver-auto.sh` pass unchanged — hold, escalate, grant,
  rewake, consume.
- `50-lineage.sh` and `60-visibility.sh` pass unchanged.
- The dashboard shows an `ActivityStarted` row followed by an `ActivityCompleted` row per
  tool call, with a duration on the completed one — checked by eye, recorded in the run
  artifact.
- Both providers verified (Claude Code and Codex), since both mappers feed one serializer.

## Architecture

The suite's shape is unchanged: `00-preflight` → `10-onboard` → capture → realtime →
enforce → approvals → lineage → visibility → approver → teardown, with `tb_*` helpers
asserting against the real Postgres. Only the assertions inside `20-capture.sh` and
`25-realtime.sh` move from the span table to the event table.

## Related code files

| File | Change |
|---|---|
| `testbed/20-capture.sh` | replace span-class assertions with activity-pair assertions; assert `spans` count is 0 |
| `testbed/25-realtime.sh` | `mid_activity()` and the final assertion count both activity types |
| `testbed/30-enforce.sh` | re-point the span diagnostic at `governance_events` |
| `testbed/40-approvals.sh`, `70-approver-auto.sh` | run unchanged; do not soften on failure |
| `testbed/50-lineage.sh`, `60-visibility.sh` | run unchanged |
| `docs/testbed/e2e.md` | describe what capture now proves |

## Implementation steps

1. Boot the local stack per `docs/testbed/e2e.md`; confirm `00-preflight` and `10-onboard`
   are green before changing any assertion.
2. Rewrite `20-capture.sh`'s tool section: for the session's `run_id`, assert
   `ActivityStarted` count == `ActivityCompleted` count == the number of tool calls the
   scripted session makes, and assert every `ActivityCompleted.activity_id` has a matching
   `ActivityStarted.activity_id`.
3. Add the explicit `spans` count-is-zero assertion with a comment naming it as the
   accepted trade-off, so a future reader does not "fix" it.
4. Assert `activity_type` is non-null and equals the expected tool name for a known call,
   and that `duration_ms` on the completed row is > 0.
5. Assert Merkle: event leaves for both rows, zero span leaves for the session.
6. Update `25-realtime.sh` counting so the debounced-flush progress signal still works when
   each tool call contributes two rows of two different types.
7. Re-point `30-enforce.sh:84`'s diagnostic.
8. Run `./testbed/run-all.sh` for Claude Code; then the Codex path. Record both.
9. Open the dashboard on the captured `run_id` and confirm the paired rows and the
   duration. Screenshot into the run artifact.
10. Write the run artifact to `plans/260811-0245-tool-activity-event-shape/reports/`, then
    release Phase 03's gated claims (`CLAUDE.md` current state, MAPPING.md §5/§7).

## Todo list

- [ ] **Stack booted; preflight + onboard green pre-change — BLOCKED, see below**
- [x] `20-capture.sh` activity-pair assertions (counts equal + no unpaired completed)
- [x] `20-capture.sh` spans-are-zero assertion with rationale comment
- [x] `activity_type` + `duration_ms` assertions
- [x] Merkle event-leaf / no-span-leaf assertion
- [x] `25-realtime.sh` counting fixed
- [x] `30-enforce.sh` diagnostic re-pointed
- [ ] `run-all.sh` green for Claude Code — **not run**
- [ ] `run-all.sh` green for Codex — **not run**
- [ ] Dashboard confirmed by eye + screenshot — **not done**
- [ ] Run artifact written; Phase 03 claims released — **not done**

## Why the live run did not happen

`testbed/00-preflight.sh` requires seven running containers — `postgres`,
`openbox-core`, `openbox-backend`, `opa`, `governance-worker`,
`attestation-worker`, `openbox-fe` — from `local-stack/docker-compose.local.yml`
in a sibling repo, plus a minted control token (`./testbed/env.sh mint`). None
were running. The user's decision was to land the script changes and run the
suite themselves.

**Nothing downstream of the run was faked.** Phase 03's gated claims are NOT
released: `MAPPING.md` §7 opens with "NOT YET RUN", `CLAUDE.md`'s current-state
paragraph says implemented-and-unit-verified rather than verified end to end, and
that decision's consequences carry the load-bearing assumption as unproven.

## To run it

```bash
./testbed/env.sh mint      # once, after the stack is up
./testbed/run-all.sh
```

Then write the artifact to `reports/` here and release the gated claims.

## What the assertions now check

| Script | Assertion added or changed |
|---|---|
| `20-capture.sh` | `ActivityStarted` count == `ActivityCompleted` count == tool calls; **zero unpaired** completed rows (SQL `not exists` on `activity_id`); `activity_type` non-null; at least one completed row with `duration_ms > 0`; at least one with `activity_output`; tool classes asserted through `input->>'kind'` / `file_path` / `mcp_server`+`mcp_tool` instead of span types; **`spans` count is 0**, with a comment naming it as the accepted trade-off; Merkle event leaves >= 2, span leaves == 0, and at least one leaf joined to an `ActivityCompleted` |
| `25-realtime.sh` | `mid_activity()` counts `event_type like 'Activity%'` — the completed half is often what the debounced flush delivers, so counting only starts under-reports the progress signal; final assertions add a completed-half count and an equality check that no half was double-sent across the realtime and teardown drains |
| `30-enforce.sh` | secret-egress scan now asserts the combined query is non-empty before asserting the secret is absent — otherwise an empty result would pass vacuously once the spans half returns nothing |

### Found in code review, before any run — an approval-poll ambiguity

Core's approval-status endpoint resolves a poll through
`FindByWorkflowRunActivity` (`internal/datastore/governance_event_pgx.go:74-87`,
called from `internal/services/governance.go:291`), which filters on
`(workflow_id, run_id, activity_id)` with **no `event_type` filter and no
`ORDER BY`**, then takes `.One()`. That key was unique per tool call before this
change, because a `ToolResult` updated the existing row rather than creating one.
Two activity rows now share it.

The primary hold is safe — escalation and polling are pre-execution, so the
`ActivityCompleted` row does not exist yet. The exposure is the **retry after a
completed attempt**, which is exactly the path `operation_id` exists to support:
a second attempt at an already-completed operation shares its `activity_id`, and
if the poll returns the completed row (NULL `approval_expiration_time`),
`ApprovalStatus.Decided()` reads false and a real grant goes unconsumed.

Added to MAPPING.md §7 item 6 as something the live run must exercise, and to that
decision's Consequences with the core-side fix. **Not reproduced — found by
reading.** It is out of scope here (the plan's non-goals forbid openbox-core
changes) but it is a direct consequence of this shape and should be fixed core-side
before this ships to anyone relying on retry-after-completion approvals.

### A judgment call worth flagging

`assert_eq` on started == completed is stricter than the phase's fallback
(`>=` plus the pairing invariant). The phase pre-authorized weakening it if counts
prove flaky across runs. Equality is the assertion that actually catches a merged
or dropped row, so it goes in first; weaken it only with a flaky run as evidence.

The `duration_ms` assertion requires at least one row with a positive duration,
not *all* rows: the client deliberately omits the field when the cross-process
start-time stash misses, so requiring it everywhere would fail on a real,
documented degradation rather than on a defect.

## Success criteria

1. `testbed/run-all.sh` passes for both providers against a live stack.
2. For a real session: `ActivityStarted` rows == `ActivityCompleted` rows == tool calls,
   paired on `activity_id`.
3. `spans` count for the session is 0, asserted.
4. Approval hold → grant → rewake → consume works, unmodified scripts.
5. The dashboard renders the pair with a duration; recorded with a screenshot.
6. A run artifact exists that a reader can use to re-derive every claim Phase 03 makes.

## Risk assessment

| Risk | Mitigation | Signal it broke | Pre-decided response |
|---|---|---|---|
| Core merges or rejects the `ActivityCompleted` instead of creating its own row (the phase's central unverified assumption) | This phase tests it first, before docs are released | One row per tool call, or a non-2xx on the completed POST | **Proceed to the 3-POST fallback — pre-authorized (validation decision 3), no new decision needed:** `ActivityStarted` → `ActivityCompleted` → `ActivityCompleted`+`hook_trigger` with the span, which core's event_type-agnostic span gate does support. Phase 03's record records that shape and its base-SDK divergence instead |
| The approval loop breaks because the span-based bypass no longer fires | `40-approvals.sh` / `70-approver-auto.sh` run unchanged and are not softened | Rewake loops, or a granted approval is never consumed | Stop. The approval loop is shipped behavior; reshaping the wire to keep it is preferable to shipping a broken gate |
| `duration_ms` is absent, zero or negative | It is the only timing field sent (validation decision 2), it has one consumer, and step 4 asserts it is > 0 | Assertion fails, or the dashboard row shows blank / a nonsense duration | Fix the arithmetic in Phase 01 step 8 and re-run — the failure is visible, not silent |
| A realtime-flush failure is mistaken for a shape failure, or vice versa | Run `20-capture.sh` (session-end flush) green before `25-realtime.sh` | Both fail together with the same symptom | Bisect by setting `OPENBOX_REALTIME=0` and re-running capture |
| A dashboard surface that read dev-session spans goes blank | Step 9 looks at the real UI, not just the DB | An empty panel or a broken span view | Record as a known limit in that decision; raise with the dashboard owner |

**Assumption that may break:** that the scripted testbed session's tool-call count is
stable enough to assert equality rather than `>=`. Signal: flaky counts across runs.
Response: assert the pairing invariant (every completed has a started with the same
`activity_id`) and a `>=` floor, rather than weakening the pairing check.

## Security considerations

- Confirm from the captured rows that no command text, file body or tool output reached
  core on the observe path — this is the run that turns Phase 03's "appears to have been
  unused" into a measured statement about the removed span content channel.
- Confirm the Tier-2 escalation still carries its content-gated `tool_input` and that it
  is absent when content capture is off.
- Confirm Tier-1 local secret redaction still runs on the enforce path (`30-enforce.sh`).
- Do not paste secrets, keys or raw session content into the run artifact — cite counts
  and column names.

## Next steps

Release Phase 03's gated claims, update `CLAUDE.md`'s current-state paragraph with what was
verified, and journal the outcome. Remaining open questions (openbox-backend read side,
deployed-core semantic classification) carry forward as their own work, not this plan's.

<!-- Updated: Validation Session 1 - 3-POST fallback pre-authorized (no stop-and-ask); duration_ms is the only timing field asserted; ApprovalKeyFor stability added to requirements -->
