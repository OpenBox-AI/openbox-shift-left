# Phase 04 — Deploy to staging + live replay verification

> **Executed against `../openbox-core`'s deployment and a live stack.** No shift-left code changes.

## Context links

- Plan: [plan.md](plan.md) · Blocks on: [phase 02](phase-02-core-dev-session-fix.md)
- Mechanism being replayed: [debug-260814-1231](../reports/debug-260814-1231-session-no-longer-active-halt.md)
- Reads better if [phase 03](phase-03-enforcement-record-ts-reason.md) is merged first (one-file
  correlation instead of two) — not a dependency.

## Overview

- **Date:** 2026-08-14 · **Priority:** P1 · **Effort:** 2h
- **Description:** Confirm the RUNNING core carries the fix, then deliberately reproduce the
  incident — force a terminal session row mid-session — and assert the session keeps working, the
  event stores, and no unauthored HALT appears. Includes the runtime-agent regression check.
- **Implementation status:** pending
- **Review status:** not reviewed

## Key Insights

1. **Manifest repos are stale — image bumps happen out of band.** Verify the fix from the RUNNING
   service (deployed image digest / a version the service reports), never from a manifest file or
   from git. A green PR is not evidence about what is answering `/evaluate`.
2. **Unit tests cannot reach the claim.** The workflow tests mock `CheckSessionStatusActivity` and
   `SessionLifecycleActivity`; only a real session against a real stack proves that a real terminal
   row no longer blocks real tool calls. Repo discipline: reading is not evidence, and neither is a
   mock.
3. **The negative assertion is the load-bearing one.** "The call proceeded" is weak on its own —
   it is also what an unreached gate looks like. Pair it with: the event IS stored server-side, and
   `advisories.jsonl` shows a real verdict for that call with a policy id or a non-HALT verdict, and
   **zero HALTs with an empty policy id**.
4. **Regression check needs a non-developer agent.** Otherwise this phase proves the relaxation and
   says nothing about the thing it must not have loosened.
5. **A blocked stack is an acceptable outcome, stated honestly.** Precedent: several shipped
   features in this repo are "implemented, unit-verified, testbed NOT run". Silence about it is the
   failure mode; parking with a named status is not.

## Requirements

- R1: the running core's image/version is confirmed to be built from the phase-02 merge commit,
  read from the running service.
- R2: replay — a live dev session, a terminal session row forced mid-session, then a gated tool call
  that must proceed.
- R3: the gated call's event is stored server-side (a governance event exists).
- R4: `advisories.jsonl` and `enforcements.jsonl` for the replay contain zero HALTs with an empty
  policy id.
- R5: the session row is NOT flipped to `halted` by any verdict during the replay.
- R6: attested-half check — if the forced terminal event also seals the session, the later dev event
  still stores, carries the `post_attestation` marker, and adds no Merkle leaf (leaf count for that
  session unchanged after signing).
- R7: regression — a non-developer agent on a non-pending session still receives HALT.
- R8: findings written to `../reports/`, split by evidence strength (repo convention).

## Architecture

Replay shape: start a governed Claude Code session in an `openbox init`-ed directory (project scope,
enforce default-on). Mid-session, force the session row terminal by the least invasive means
available — preferably by delivering a terminal `SessionEnded` for that `(workflow_id, run_id)`,
which reproduces the observed trigger exactly; a direct staging-DB status update is the fallback if
event injection is not available. Then issue a gated tool call (any tool — the gate is `*`) and
observe.

## Related code files (read-only, for interpreting results)

- `~/Library/Application Support/openbox/advisories.jsonl`, `enforcements.jsonl` — client-side truth
- `adapters/claude-code/hookrun.go:197` — enforcement gates PreToolUse only, so the gated call must
  be a tool call, not a prompt or lifecycle event
- `adapters/common/hookflow/enforce.go:481-491` — `GovReason`; a missing `(policy: …)` suffix is the
  empty-policy-id tell
- `openbox-core internal/services/activities/attestation/finalize.go:19-20` — leaf-count check for R6

## Implementation Steps

1. Merge phase 02; note the merge commit sha.
2. Deploy to staging. Read the running service's image digest / reported version and tie it to that
   sha. If they cannot be tied, stop — R-A.
3. Confirm the replay machine's agent has `agent_type = "developer"` (re-register via
   `openbox auth` if not) — otherwise the replay tests nothing (phase 02 R-C).
4. Start a governed dev session; make one normal gated tool call to establish a working baseline.
5. Force the session row terminal (preferred: deliver a terminal `SessionEnded`; fallback: staging
   DB status update).
6. Make the next gated tool call. Record: did it proceed, what did stdout say, what landed in both
   local sinks.
7. Server-side: confirm a governance event exists for that call; confirm the session row status;
   confirm the leaf count for the session (R6) and the marker if attested.
8. Regression: drive a non-developer agent event against a non-pending session; assert HALT.
9. Write the verification report to `../reports/`, splitting proven / partially proven / unproven.

## Todo list

- [ ] Phase 02 merged; sha recorded
- [ ] Staging deployed; running image tied to the sha (from the running service)
- [ ] Replay agent confirmed `agent_type = "developer"`
- [ ] Baseline gated call works
- [ ] Terminal session row forced mid-session
- [ ] Next gated call proceeds; event stored
- [ ] Zero empty-policy HALTs in the replay window
- [ ] Session not flipped to `halted`
- [ ] Attested-half checks (marker + leaf count) if reached
- [ ] Runtime-agent regression HALT confirmed
- [ ] Verification report written, evidence-strength split

## Success Criteria

1. The running core is provably the fixed build, evidenced from the running service.
2. Post-terminal-row gated call: **proceeds**, and its governance event exists server-side.
3. Zero HALT records with an empty policy id across the replay window.
4. Session status is not `halted` at the end of the replay.
5. If the attested path is exercised: event stored, marked, session leaf count unchanged.
6. Non-developer agent on a non-pending session: still HALT.
7. Report published with claims split by evidence strength.

## Risk Assessment

- **R-A — no reachable stack, or the running image cannot be tied to the sha.** *Break signal:* no
  staging endpoint answers, or the deployment exposes no digest/version that maps to the build.
  *Pre-decided response:* park the phase as **"implemented, unverified"**, say so in the plan status
  and in phase 05's docs decision, and do NOT claim the fix is verified anywhere. Phase 05 then
  waits, because its whole content is a claim about a deployed core.
- **R-B — the terminal row cannot be forced without a destructive write.** *Break signal:* no event
  injection path and no safe staging DB access. *Pre-decided response:* adjust in-plan — reproduce
  by natural means (end a session and continue emitting on the same session id from a resumed
  session); if that also fails, park exactly this assertion as unproven and keep the rest.
- **R-C — the call proceeds because the gate never ran.** *Break signal:* no advisory/enforcement
  record at all for the replay call. *Pre-decided response:* adjust in-plan — the run is void, not
  a pass. Re-check `openbox doctor`, project scope, and enforce posture, then re-run.
- **R-D — replay reveals the trigger is still firing** (something keeps writing terminal rows
  mid-session). *Break signal:* terminal rows appear without a deliberate injection. *Pre-decided
  response:* not a failure of this fix — the fix's job is that it no longer blocks work. Feed the
  observation to phase 01 as fresh evidence.
- **R-E — staging behaves differently from the environment that produced the incident.** *Break
  signal:* the pre-fix behavior cannot be reproduced on the same build. *Pre-decided response:*
  stop-and-replan the verification: without a reproducible baseline, a passing replay proves
  nothing. Reproduce on the pre-fix build first, then re-deploy the fix.

## Security Considerations

Staging only unless the owner explicitly authorizes production. Forcing a terminal session row is a
governance-record mutation — do it in staging, and never on a session belonging to another
developer. Do not disable enforcement to make the replay easier; that would void the result. No
credentials, policy bodies or prompt text in the report — identifiers and verdict shapes only.

## Next steps

On success, hand the merge commit sha to phase 05. On a parked outcome, phase 05 stays blocked and
the plan status records "implemented, unverified".
