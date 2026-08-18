# Phase 01 — Trigger investigation: what wrote a terminal session row while the session was live

## Context links

- Plan: [plan.md](plan.md) · Blocks on: — · Parallel-safe with phases 02, 03
- Diagnosis: [debug-260814-1231](../reports/debug-260814-1231-session-no-longer-active-halt.md)
  (unresolved Q1 and Q5 are this phase's whole scope)
- Repo: DB read-only + openbox-shift-left client-side correlation. **No fix here.**

## Overview

- **Date:** 2026-08-14 · **Priority:** P1 · **Effort:** 1.5h
- **Description:** Establish what recorded a terminal `SessionEnded` for session `4459b8ed…` while
  it was still live, and whether the 00:46 `SessionStarted`-HALT on `37157843…` is the same defect
  or a second one. Output: evidence + a filed bug in the owning repo, or a stated blocker.
- **Implementation status:** pending
- **Review status:** not reviewed

## Key Insights

1. **This is the trigger, not the mechanism.** Phase 02 fixes the amplifier (reject + latch). It
   does NOT stop something from writing a terminal row mid-session — which, once dev sessions stop
   being blocked, only degrades lineage rather than work. Ordering is therefore independent.
2. **Two candidate owners, opposite fixes.** Client-originated (a late spool flush, a duplicate
   `SessionEnd` hook, a resume/compact cycle) ⇒ shift-left bug. Server-originated (out-of-order
   delivery, a second insert path) ⇒ core bug. The `sessions` + `governance_events` rows decide it.
3. **The 00:46 row is self-contradictory under the known model.** A `SessionStarted` HALT requires a
   non-pending row to already exist for a fresh UUID `run_id` — impossible under
   `handleSessionCreate`, which inserts `pending` on `WorkflowStarted`
   (`openbox-core internal/services/activities/governance/storage_session.go:58-66`). Either
   out-of-order delivery or a second insert path is real.
4. **The backend REST route is not reachable with what this machine holds** — the runtime `obx_`
   key is scoped to core's evaluate endpoint and the backend answers 401 (diagnosis :143-145). Plan
   for direct SQL or an admin-scoped token.
5. **`session_attestations` matters to phase 02.** If those sessions were sealed, the observed bug
   already had a second blocking half waiting (Block 2's `IsAttested`), which is exactly why phase
   02 treats the attested half as in scope rather than optional.

## Requirements

- R1: Recover the four full session ids and exact UTC instants from local sinks.
- R2: Read-only SQL on `sessions`, `governance_events`, `session_attestations` for those ids.
- R3: Classify the trigger as client-originated, server-originated, or inconclusive, with evidence.
- R4: If client-originated → file a bug in openbox-shift-left. If server-originated → file a core
  issue per its PROD ticket convention. If inconclusive → state exactly what would settle it.
- R5: No writes to any session row. No fix in this phase.

## Architecture

Session identity is `(workflow_id, run_id)` = (`workspaceID || developerDID`, the Claude Code
session id) — `client/payload.go:152-153`, `:288-300`. Status writers are exactly two
(`storage_session.go:170-205` on a terminal event; `activities/governance/validation.go:34-46`
`UpdateSessionHaltedActivity`); there is no stale-session sweeper. So a terminal row implies a
terminal EVENT arrived — the question is who sent it and when.

## Related code files (read-only)

- `~/Library/Application Support/openbox/advisories.jsonl` — carries `ts` (`adapters/common/hookflow/advisory.go:48`)
- `~/Library/Application Support/openbox/enforcements.jsonl` — no `ts` today (phase 03 fixes)
- `adapters/common/hookflow/` — spool + `RealtimeTrigger` debounced detached flusher (late-flush candidate)
- `adapters/claude-code/hookrun.go` — which hooks map to `SessionEnded`
- `openbox-core internal/services/activities/governance/storage_session.go:58-66`, `:170-205`

## Implementation Steps

1. `jq` the two local sinks for the four prefixes (`4459b8ed`, `37157843`, `e800efc2`, `c0e75ec0`);
   record full ids + UTC instants. Enforcement records have no `ts` — join via `advisories.jsonl`.
2. Obtain read-only DB access to the core Postgres that served these sessions (staging or prod as
   applicable). If unavailable after a bounded attempt, go to Risk R-B.
3. Query A — `sessions` by `run_id IN (…)` ordered by `created_at`: id, workflow_id, run_id,
   status, detail, started_at, completed_at, created_at, updated_at. Expect ≥2 rows for
   `4459b8ed` (the 12:31 recovery insert).
4. Query B — `governance_events` for those session ids ordered by `created_at`: event_type,
   created_at, workflow_status, verdict/action. Identify the row that carried the terminal event,
   its arrival instant, and whether a duplicate terminal event exists.
5. Query C — `session_attestations` for those session ids: was the session sealed, and when.
   Feed the answer to phase 02 (attested-half decision) regardless of the trigger verdict.
6. Correlate client-side: does the local spool/flush show a `SessionEnded` emitted at that instant?
   Check for a second `SessionEnd` hook firing (resume/compact) and for a delayed realtime flush.
7. Classify per R4 and file. Write the evidence report to `../reports/`.

## Todo list

- [ ] Full session ids + instants recovered from local sinks
- [ ] DB access obtained (or blocker recorded)
- [ ] Queries A, B, C run; rows captured verbatim in the report
- [ ] Client-side spool/flush correlation done
- [ ] Trigger classified with evidence
- [ ] Bug filed in the owning repo, or "inconclusive + what would settle it" recorded
- [ ] Attestation answer handed to phase 02

## Success Criteria

1. A named trigger with row-level evidence, or an explicit "inconclusive" with the exact missing
   datum.
2. If client-originated: a shift-left issue exists, citing the rows.
3. Phase 02 has a yes/no on whether those sessions were attested.
4. Zero writes performed against any session row (verified by using a read-only role or by review
   of the executed statements).

## Risk Assessment

- **R-A — rows aged out / retention window passed.** *Break signal:* Query A returns zero rows for
  those `run_id`s. *Pre-decided response:* adjust in-plan — switch to prospective capture: keep the
  queries and re-run them against the phase-04 replay, which reproduces the state deliberately.
- **R-B — no DB access, no admin token.** *Break signal:* no read-only credential after a bounded
  attempt (~30 min). *Pre-decided response:* stop and surface to owner; the phase parks as
  "blocked: no DB access" with the exact three queries recorded so anyone with access can settle it
  in minutes. Phases 02/03 are unaffected (no dependency).
- **R-C — evidence points at a third mechanism** (neither client nor a known core path).
  *Break signal:* the terminal `governance_events` row has an arrival instant inside the live
  window AND no matching client emission. *Pre-decided response:* stop-and-replan the ride-along —
  file it as an open core investigation rather than guessing an owner.

## Security Considerations

Read-only SQL exclusively; use a read-only role if one exists. Session ids, DIDs and workflow ids
are identifiers, safe for the report; do NOT paste org policy content, tokens, or `~/.openbox/.env`
material into it. The credentials file is plaintext and unprotected on Windows (ADR-0015) — treat
any secret encountered as already exposed and do not widen its reach by copying it.

## Next steps

Hand the attestation answer to phase 02. If client-originated, the filed bug becomes its own plan —
it is out of this plan's scope by user decision.
