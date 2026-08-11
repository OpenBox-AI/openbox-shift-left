# Debug: activity_id appearing in >2 events

Date: 2026-08-11 15:41 (+07)
Reporter: ak:debug
Evidence: 130 core event rows, session `6a66bfba-5ce4-481d-8ef4-f0144cf642b1`,
agent `ad97a67f-b9ba-447c-bf5d-f894d6919feb` (two OpenBox API dumps supplied by user)
Code ref: `origin/main` @ 56411f2 (PR #3, feat/realtime-event-delivery merged)

## Executive summary

User expectation is correct: one `activity_id` = one tool execution = exactly
2 events (ActivityStarted + ActivityCompleted). Confirmed broken.

**Root cause: the Tier-2 escalation path stores the ActivityStarted twice.**
In enforce+Tier-2 mode `RunHook` maps the same PreToolUse payload twice — the
observe/spool copy and the synchronous `/evaluate` escalation copy. Both derive
an identical `event_id` *deliberately*, so they would collapse under one
`Idempotency-Key`. openbox-core does not dedupe developer events, so both are
stored, and both are committed as distinct Merkle leaves.

Client-side half of the idempotency contract shipped. Server-side half never did.
The code says so itself — `adapters/claude-code/mapper.go:524`.

Not caused by the operation-derived `activity_id` (see H1 below): latent, did not
fire in this session.

## Measured shape

126 events carry an `activity_id`; 60 distinct ids.

| Shape | Count | Meaning |
|---|---|---|
| `SC` | 52 | correct |
| `SSC` | 7 | **duplicated ActivityStarted** |
| `S` | 1 | unpaired (secondary finding) |

Started=67, Completed=59, excess=8 = 7 duplicates + 1 unpaired.

Per-tool: Bash 27 activity_ids / **7 duplicated**; Read 28 / 0; Edit 3 / 0;
Agent 2 / 0. Only the shell class — the approval-capable one — escalates.

## Proof it is the Tier-2 double-map

All 7 `SSC` groups, without exception:

- both ActivityStarted share one `event_id` (e.g. `cc-314660445c091c8acbd7c…`)
- identical `created_at` to the millisecond — the pinned Mapper clock
  (`adapters/claude-code/hookrun.go:104`)
- identical `metadata`, incl. `trust_tier: 2` and one `tool_use_id`
- `input` DIFFERS — the discriminator:
  - observe copy: `{"kind":"shell","tool_name":"Bash"}` (structural only)
  - escalation copy: `{"kind":"shell","command":"grep -rn …"}` (carries the
    command — `Content.ToolInput` is set *only* on a Tier-2 escalation,
    `client/event.go`)
- **adjacent `merkle_leaf_index`** pairs: [2,3] [8,9] [12,13] [105,106]
  [116,117] [123,124] [126,127]

Two rows, one real tool call, two audit leaves, different detail levels.

## Mechanism

```
PreToolUse (Bash, policy escalates)
├─ Observe  → Mapper.Map() → spool → flush   → ActivityStarted  (structural input)
└─ escalateTier2 → Mapper.Map() → /evaluate  → ActivityStarted  (input + command)
                                  same event_id, no server dedupe → BOTH STORED
PostToolUse                                  → ActivityCompleted
```

Timing: the two POSTs land ~90ms apart (`updated_at` 03:14:46.067 vs .157;
`api_response_ms` 540 / 533), so the realtime flush (ff8edbd) delivers the spool
copy almost immediately. Realtime delivery did not *create* the duplicate — it
existed whenever both paths ran — it collapsed the gap and made it obvious.

## Impact

- Inflated event and activity counts on every escalated shell call.
- Duplicate Merkle leaves: the audit log double-counts one tool execution.
  Material for a governance product whose value proposition is the evidence.
- Timeline shows the same call twice with unequal content, so a reviewer cannot
  tell duplication from two real attempts.
- Approval accounting: both rows are addressable by the same
  (workflow_id, run_id, activity_id) triple.

## Fix options

1. **Server-side dedupe on `Idempotency-Key`/`metadata.event_id` in core** —
   the "completing half" the client already anticipates. Correct and general;
   fixes the lost-200 retry case too. Different repo (openbox-backend /
   openbox-core).
2. **Suppress the redundant client emit** in `hookrun.go`: skip the spool copy
   when the synchronous T2 `/evaluate` confirms delivery. Constraint: the T2 send
   is fail-open, so the spool copy must survive when T2 fails/times out —
   suppress on success only, never unconditionally.
3. Both. (2) stops the bleeding in this repo; (1) is the durable invariant.

Not implemented — diagnosis only.

## Secondary findings

- **1 unpaired ActivityStarted**: `Read` on
  `/private/tmp/poc-temporal-refund/tests` — a *directory* path with
  `file_operation` set. Likely the Read errored and PostToolUse never produced a
  result. Separate from the duplication; worth its own look.
- **`RealtimeTrigger` doc overstates its guarantee**
  (`adapters/common/hookflow/realtime.go:48`): claims redundant flushes "can
  never double-count" partly because "the server deduplicates on the
  Idempotency-Key". That dedupe does not exist. Atomic spool rotation is the
  only real protection, and it does not cover the Tier-2 path. Doc should be
  corrected regardless of which fix is chosen — a governance product overstating
  its own assurance is the failure mode CLAUDE.md warns about.
- **H1, latent**: `activity_id` is operation-derived, not invocation-derived
  (`client/payload.go:211`, `client/operation.go:14`) — deliberate, so an
  approval survives a retry. Consequence: two *deliberate* identical operations
  in one session (same file read twice, same command twice) collapse onto one
  `activity_id` and would produce `SCSC`. Zero groups of 4+ here, so it did not
  fire, but it will. Temporal disambiguates with `attempt`, which this payload
  omits by design (`client/payload.go:19`).

## Unresolved questions

1. Fix at core (dedupe) or at the adapter (suppress redundant emit), or both?
2. Is duplicate-Merkle-leaf backfill/repair needed for already-stored sessions,
   or is forward-only correctness acceptable?
3. H1: is operation-collapse acceptable as specified, or should `activity_id`
   regain per-execution uniqueness with the approval key tracked separately?
   Reversing it naively re-breaks the approval loop this design fixed.
4. The unpaired `Read` on a directory path — expected failure mode, or a
   PostToolUse gap?
