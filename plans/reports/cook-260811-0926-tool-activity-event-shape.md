# Cook report — tool executions become Activities

Plan: [260811-0245-tool-activity-event-shape](../260811-0245-tool-activity-event-shape/plan.md)
Branch: `feat/realtime-event-delivery` · 2026-08-11 · uncommitted

## Result

Phases 01–03 complete. Phase 04's script changes complete; **its live run was not
performed** — the seven-container local stack was unavailable and the user elected
to run `testbed/run-all.sh` themselves.

All 11 `go.work` modules: build, vet, test clean. `gofmt` clean. All testbed
scripts parse.

## The wire, before and after

```
before  {"activity_id":"cc-act-210c…","activity_type":"Write","event_type":"ActivityStarted",
         "hook_trigger":true,"span_count":1,"spans":[{…18 fields…,"stage":"completed"}],…}

after   {"source":"developer-runtime","event_type":"ActivityCompleted","activity_type":"Write",
         "activity_id":"cc-act-210c…","workflow_id":"…","run_id":"…",
         "workflow_type":"developer-session","activity_output":{"bytes_written":128},
         "duration_ms":2500,"timestamp":"…","metadata":{…}}
```

`activity_id` is byte-identical — checked against the pre-change fixtures in git,
not merely asserted.

## Acceptance criteria

| # | Criterion | Status |
|---|---|---|
| 1 | Two rows per tool call, one `activity_id`, each evaluated | needs live run |
| 2 | `activity_type` = tool name; `activity_id` unchanged | **met, verified** |
| 3 | `activity_output`, `duration_ms`, and on failure `error` | **partially met** — `error` deliberately omitted, below |
| 4 | `workflow_type` on tool events | **met** |
| 5 | No `spans` / `span_count` / `hook_trigger` | **met** |
| 6 | Approval loop holds, escalates, consumes on rewake | needs live run |
| 7 | `testbed/run-all.sh` passes | needs live run |

## Two deliberate deviations

**`error` is not sent.** No source — the frozen adapter contract
(`dev-event.schema.json` v1.0) has no failure field — *and* no consumer: core
reads `payload.Error` only for `WorkflowFailed` (`storage_event.go:281-286`), so
an `ActivityCompleted.error` is decoded and discarded. Core's `Error` is also
`*ErrorInfo`, a struct, not a string. **Consequence: a failed tool call is
indistinguishable from a successful one on the wire.** rather than shipping a
field with neither producer nor consumer. Closing it needs a failure signal in
the adapter schema — its own decision.

**`attempt` is not sent.** Permanently null in a stateless hook process.

## What the change turned out to be

Core's dedupe key is `(agent_id, workflow_id, run_id, activity_id, event_type)`
(`activities/governance/validation.go:96`). Under the hook shape a tool call's two
halves matched on **all five** — same `activity_id` by design, both
`ActivityStarted` — so the `ToolResult` POST hit the existing-event branch
(`governance_workflow.go:228-231`) and never created a row. The shared `span_id`
that decision chose as the pairing mechanism was also what made the span-dedup
check see nothing new.

So the completed half of every tool call was substantially a no-op. Because
`event_type` is in the key, the new shape recovers it — an independently evaluated
row with its own OPA pass and Guardrails stage 1 over `activity_output`. This
reframes the change from cosmetic to a recovery of governance coverage, and is now
the lead item in that decision's Context and Consequences.

Still source-reading, not running. Phase 04 is what settles it.

## Found in code review — needs a core-side fix

`FindByWorkflowRunActivity` (`internal/datastore/governance_event_pgx.go:74-87`,
via `services/governance.go:291`) backs the approval-status poll. It filters on
`(workflow_id, run_id, activity_id)` with **no `event_type` and no `ORDER BY`**,
then `.One()`. That key was unique per tool call before; two rows now share it.

The primary hold is safe — escalation and polling are pre-execution, so the
completed row does not exist yet. The exposure is the **retry after a completed
attempt**, exactly the path `operation_id` exists to support: the poll may resolve
the completed row, whose `approval_expiration_time` is NULL, so `Decided()` reads
false and a real grant goes unconsumed.

Out of this plan's scope (non-goals forbid openbox-core changes).
Consequences and MAPPING.md §7 item 6, and spun off as a task.

## Files

**Deleted** `client/hookspan.go`, `client/spanbuilder.go` + tests,
`client/payload_hook_test.go`, 4 `hook_*.json` fixtures. **New**
`client/approval_key_pin_test.go`, 6 `activity_*.json` fixtures,.
**Rewritten** `client/payload.go` (one serializer, was two),
`contracts/dev-event/MAPPING.md` §1/§2/§3/§5/§7. **Corrected**
`client/README.md`, `CLAUDE.md`, root `README.md`, `docs/architecture.md`,
`docs/data-and-privacy.md`, `docs/testbed/e2e.md`,
`contracts/dev-event/{README,COVERAGE}.md`, `ext-core/README.md`, `{that
decision,README}.md`, three adapter comment-only fixes. **Testbed**
`20-capture.sh`, `25-realtime.sh`, `30-enforce.sh`.

`contracts/dev-event/schema/` untouched — no `schema_version` bump.

## Verified rather than assumed

- `activity_id` byte-identical, checked against `git show HEAD:` fixtures.
- The span's `request_body`/`response_body` carried **nothing**: no adapter has
  ever set either, and both mappers assert they stay empty
  (`claude-code/mapper_test.go:169`, `codex/mapper_test.go:207`). Lets
  `data-and-privacy.md` state the narrowing as measured, not hoped.
- `contracts/dev-event/conformance` passes with zero edits — the two-layer split
  working as designed.
- `client/leakscan_test.go` needed no extension: it scans the whole payload for
  both tool events, so `activity_input`/`activity_output` are covered by
  construction.
- `capBody`'s size cap lost its only coverage when `payload_hook_test.go` went
  (it was pinned solely through the span's `request_body`); re-homed onto the two
  surviving content fields.

## To finish

```bash
./testbed/env.sh mint
./testbed/run-all.sh
```

Then write the artifact to `plans/260811-0245-tool-activity-event-shape/reports/`
and release the gated claims: `MAPPING.md` §7's "NOT YET RUN" header,
`CLAUDE.md`'s current-state paragraph, that decision's "Not yet proven".

## Unresolved

1. Should `error` be plumbed anyway (metadata-sourced, absent in practice), or is
   the recorded gap the right answer until the schema gains a failure field?
2. The openbox-core approval-poll fix — who owns it, and does it block shipping?
3. Does openbox-backend's read side need any change? (Carried from the plan.)
