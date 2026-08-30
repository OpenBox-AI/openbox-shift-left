# Phase 02 — Core: developer sessions stop being rejected and stop latching

> **Planned here, EXECUTED in `../openbox-core`.** One PR, conventional commit with a PROD ticket
> prefix per core convention (precedent `c7a93f3` "PROD-314 fix(governance): …"). Nothing in
> openbox-shift-left changes in this phase.

## Context links

- Plan: [plan.md](plan.md) · Blocks on: — · Parallel-safe with phases 01, 03
- Surface + test conventions: [researcher-01](research/researcher-01-core-governance-surface.md)
- Discriminator chain (VERIFIED): [scout-01](research/scout-01-agent-type-chain.md)
- Mechanism: [debug-260814-1231](../reports/debug-260814-1231-session-no-longer-active-halt.md)

## Overview

- **Date:** 2026-08-14 · **Priority:** P1 · **Effort:** 5h
- **Description:** Four changes in `internal/services/governance_workflow.go` — Block 1 dev-skip,
  Block 2 halted-half dev-skip, Block 2 attested-half store-and-mark-late, latch dev-skip — plus
  three invariant pins and the first-ever Block-2 tests. Agent-runtime behavior unchanged.
- **Implementation status:** pending · **Review status:** not reviewed

## Key Insights

1. **The discriminator costs nothing.** `agent *content.Agent` is fetched at
   `governance_workflow.go:153-157`, before Block 1 (`:239`) and Block 2 (`:275`); `AgentType
   *string` at `internal/content/agent.go:21`, populated by an unprojected `FindByToken`
   (`internal/datastore/agent_pgx.go:33-46`, mapped `bob_pgx.go:86,96`). shift-left sends
   `"developer"` at registration (`cli/internal/devinit/devinit.go:43`); backend persists it
   (`src/modules/agent/entities/agent.entity.ts:42`). No new query, no schema change.
2. **The attested half is NOT optional for this bug.** A dev `SessionEnded` is a terminal event, and
   terminal events are what trigger signing (`attestation_workflow.go:174-176`) — so the same
   late-event scenario that trips Block 1's status half trips Block 2's `IsAttested` half once
   attestation completes. Fixing only the status half leaves a sealed dev session blocked with a
   different reason string.
3. **Branch on activity RESULTS; do not skip activities.** Blocks 1 and 2 keep
   `CheckSessionStatusActivity` (`:234`) and `SessionLifecycleActivity` (`:257-270`) unconditional,
   so their history is unchanged and in-flight replays stay deterministic. Only two changes remove
   an activity from history — the latch (`:742`) and, for a post-attestation dev event, the
   attestation start (`:778`, `:886`). Core has **no `workflow.GetVersion` convention** (grep under
   `internal/`: 0 hits) ⇒ accepted with a break signal (R-B), not versioned.
4. **Storing a late event is already decoupled from the Merkle tree.** Leaves are appended only by
   `AttestationWorkflow` step 3 (`attestation_workflow.go:159-171`), started via
   `StartAttestationWorkflowActivity` (`:778` hook path, `:886` main path).
   `FinalizeSessionActivity` recomputes the root over **all** leaves
   (`activities/attestation/finalize.go:19-20`); `SessionAttestation` pins `EventCount` at signing
   (`internal/content/session.go:41-49`). An appended leaf is therefore the only thing that could
   invalidate a signed root — skipping the attestation start is what makes store-and-mark safe.
5. **Block 2 has zero tests today** (researcher-01 Q5: `IsAttested` in the workflow test → 0 hits).
6. **The client is not touched** — truthfulness is the decider's obligation.

## Requirements

- R1: developer session ⇒ Block 1 never rejects; event proceeds to lifecycle, OPA, guardrails, AGE,
  storage.
- R2: developer session ⇒ Block 2's halted-status half never rejects.
- R3: developer session + `IsAttested` ⇒ **store-and-mark-late**: no rejection; event stores;
  carries a marker that it is outside the sealed set; attestation start skipped. Never silent-drop.
- R4: developer session ⇒ `UpdateSessionHaltedActivity` never invoked.
- R5: `AgentType` nil or any other value ⇒ all three behaviors identical to today.
- R6: invariant pin — a policy-authored HALT carries a non-empty `policy_id` **and** stores a
  governance event.
- R7: no new table, endpoint or service (repo rule); no migration.
- R8: conventional commit + PROD ticket; no plan/phase labels in code, tests or message.

## Architecture

Helper next to the blocks: `isDeveloperSession(agent) → agent != nil && agent.AgentType != nil &&
*agent.AgentType == "developer"`.

- **Block 1** (`:239`): guard the condition with `!isDeveloperSession(agent)`; the activity still runs.
- **Block 2** (`:275`): split the disjunction into two branches. Attested — runtime rejects as today;
  developer sets a local `skipAttestation` flag plus a metadata marker and falls through. Halted —
  runtime rejects as today; developer falls through.
- **Latch** (`:741`): add `&& !isDeveloperSession(agent)` — only invocation site of
  `UpdateSessionHaltedActivity` (researcher-01 Q3). **Attestation start** (`:778`, `:886`): guarded
  by `skipAttestation`.
- **Marker:** a `post_attestation` key on the event payload metadata
  (`content.GovernanceEventPayload.Metadata json.RawMessage`, `internal/content/governance.go:194-245`)
  — a JSON key, not a column. Confirm it persists before relying on it (R-E).

## Related code files (all in `../openbox-core`)

| File:line | Change |
|---|---|
| `internal/services/governance_workflow.go:153-157` | read-only — agent already in scope |
| `internal/services/governance_workflow.go:232-254` | Block 1 dev-skip |
| `internal/services/governance_workflow.go:273-299` | Block 2 split: halted half + attested half |
| `internal/services/governance_workflow.go:740-746` | latch dev-skip |
| `internal/services/governance_workflow.go:768-778`, `:886` | attestation start guarded |
| `internal/content/agent.go:21` | read-only — `AgentType *string` |
| `internal/services/governance_workflow_test.go:109` | `registerTestActivities` helper |
| `internal/services/governance_workflow_test.go:1347-1403`, `:1406-1460` | keep as runtime cases; add dev siblings |

## Implementation Steps

1. Confirm in code, before writing anything: (a) the leaf write happens only inside
   `AttestationWorkflow` (`:159-171`) and never from the event storage path; (b) the metadata marker
   persists. If (a) fails → R-A.
2. Apply the four code changes exactly as specified in Architecture: helper, Block 1 dev-skip
   (`:239`), Block 2 split with `skipAttestation` + marker on the attested branch, both
   `StartAttestationWorkflowActivity` calls guarded (`:778`, `:886`), latch dev-skip (`:741`).
3. Tests, `testsuite.TestWorkflowEnvironment` + `env.OnActivity` per house style:
   - `_HaltedSession` / `_CompletedSession`: make the non-developer agent explicit, assertions
     byte-identical; add developer siblings asserting the workflow proceeds (downstream activities
     called, verdict not HALT).
   - Block-2 halted half: runtime rejects, developer proceeds. Attested half: runtime rejects;
     developer stores, marker present, `StartAttestationWorkflowActivity` **not** called
     (`env.AssertNotCalled`).
   - Latch: developer HALT ⇒ `UpdateSessionHaltedActivity` not called; runtime HALT ⇒ still called
     (existing OPA-STOP `:470-476`, guardrail-STOP `:988-1000` mocks stay green).
   - R6 pin: policy-authored HALT ⇒ non-empty `policy_id` and the governance-event store called.
4. `go test ./...` in core; conventional commit with the PROD ticket; PR.

## Todo list

- [ ] Structural confirmation (leaf-write decoupling + metadata persistence)
- [ ] Four code changes applied (helper, Block 1, Block 2 split + attestation guard, latch)
- [ ] Runtime tests updated (assertions unchanged) + dev siblings
- [ ] Block-2 tests, both halves, first coverage · 3 invariant pins green
- [ ] Core suite green; PR opened with PROD ticket

## Success Criteria

1. R1/R2 asserted: developer session on a non-pending status row evaluates and stores; the verdict
   is whatever policy says, never status-derived (Block 1 and Block 2's halted half).
2. R3 asserted: attested session ⇒ event stores, marker present, attestation start not called.
3. R4 asserted: developer HALT ⇒ `UpdateSessionHaltedActivity` not called.
4. R5 asserted: non-developer (nil and other values) behaves as before; the two existing rejection
   tests keep their assertions unchanged.
5. R6 asserted: policy-authored HALT carries `policy_id` and stores a governance event.
6. `go test ./...` green; no migration in the diff; no new table/endpoint/service.

## Risk Assessment

- **R-A — storing a late event on an attested session breaks Merkle/attestation verification.**
  *Break signal:* step 1 shows the leaf write is not decoupled from event storage, or an attestation
  test fails with a root/signature mismatch after a late dev event stores. *Response:* **STOP that
  sub-step, surface to owner** (user decision). Fallback: post-attestation dev events return a
  truthful non-verdict **error**, not a HALT — the client's `ApplyFailurePolicy` then governs it
  (default `fail_open` ⇒ proceed). The other three changes ship independently of this one.
- **R-B — non-determinism for workflows in flight across the rollout** (two activities leave
  history). *Break signal:* `NonDeterministicError` for `GovernanceEventWorkflow` in worker logs
  after deploy. *Response:* adjust in-plan — executions are seconds long and an affected one fails
  into the client's failure policy (default proceeds). If errors persist >5 min past rollout, stop
  and add `workflow.GetVersion` around exactly those two call sites.
- **R-C — `agent_type` NULL for dev agents registered before `openbox auth`.** *Break signal:*
  phase-04 replay still shows a status-derived HALT. *Response:* adjust in-plan — re-register the
  replay machine's agent, confirm the column, re-run. Existing-agent remedy stays out of scope.
- **R-D — a runaway dev session can no longer be stopped by the halt latch.** *Break signal:* owner
  objects, or a policy is found relying on session-level halt for dev sessions. *Response:* none —
  accepted by user decision. Every gated call still gets a fresh server verdict, so a policy that
  halts keeps halting each call; only an unauthored or stale status loses its power to brick the
  session. Do not re-litigate in review.
- **R-E — the `post_attestation` marker does not persist.** *Break signal:* the stored event's
  metadata lacks the key in the phase-04 replay. *Response:* adjust in-plan — carry it on an
  existing persisted metadata surface, or fall back to a logged marker plus an explicit note in the
  phase-04 report. Never silent-drop the event to avoid the marker problem.

## Security Considerations

The relaxation is bounded by `agent_type == "developer"` — **server-derived at registration and
backend-persisted**, not client-asserted per event (scout-01) — so a governed agent cannot talk its
way out of runtime governance, and nil/other keeps status-quo semantics. Attested lineage stays
honest: a late dev event is excluded from the signed set and **explicitly marked**, so the gap is
named rather than hidden — the reason silent-drop was rejected. No new egress, no new credential
surface, no policy semantics change: a policy HALT still halts the call it was authored for.

## Next steps

Phase 04 deploys and replays this against a live stack. Phase 05 cites this PR's merge commit.
