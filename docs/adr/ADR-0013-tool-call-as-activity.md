# ADR-0013 — A tool call is an Activity; retire the hook-span layer

Status: Accepted — 2026-08-11.
Implements: `client/payload.go` (`buildPayload`, `wireTypeFor`,
`structuralActivityOutput`, `durationMs`), `client/testdata/golden/activity_*.json`.
Supersedes: ADR-0004's `ToolCall`/`ToolResult` wire rows and its §Amendment
mirror obligation. ADR-0004 otherwise stands.
Amended by: **ADR-0018** — "developer sessions are span-less" now holds for tool
events only. One minimal span rides `TurnCompleted` under the content gate, to
feed a core reader that accepts no other shape. `client/hookspan.go`,
`client/spanbuilder.go`, the family root tuples and `AssertHookWireShape` stay
deleted; everything below about TOOL events is unchanged.

## Context

ADR-0004 unified developer telemetry onto openbox-core's stock wire types so a
stock core would accept it. For tool calls it landed on the base SDK's hook
shape: both `ToolCall` and `ToolResult` serialize as `event_type =
ActivityStarted` with `hook_trigger: true`, carrying a flat OTel `SpanData` whose
`stage` field ("started"/"completed") is the only thing on the wire that
distinguishes the two halves.

That followed the base SDK correctly. `wire_event_type()` forces
`ActivityStarted` for any `hook_trigger` event regardless of stage, and
`assert_hook_wire_shape` asserts `ActivityStarted` unconditionally. An earlier
draft had mapped `ToolResult` → `ActivityCompleted`; ADR-0004 deliberately
reversed it, and MAPPING.md §2 argued the point at length.

Three things about that shape became hard to defend as shift-left matured.

**The span was fabricated.** The base SDK's hook path exists for runtimes with
in-process OpenTelemetry: a hook fires mid-activity and has a real span to
attach. A Claude Code or Codex hook is a short-lived separate process with no
OTel at all. `client/spanbuilder.go` hand-built a span — deterministic 16-hex
`span_id`, 32-hex `trace_id` derived from the session id, `duration_ns` computed
from a cross-process timestamp stash — purely so the payload would satisfy a
shape. Nothing measured anything.

**It put the wrong header on the data — and core deduped on that header.** A
completed tool call arrived as `event_type: "ActivityStarted"` carrying
`spans[0].stage: "completed"`. No `ActivityCompleted` was ever emitted for a dev
session, so any consumer reading event types rather than span internals saw a
session of nothing but starts.

Worse, core's idempotency check keys on
`(agent_id, workflow_id, run_id, activity_id, event_type)`
(`activities/governance/validation.go:96`). Under the hook shape a tool call's two
halves matched on **all five** — same `activity_id` by design, same
`event_type` — so the `ToolResult` POST landed on the existing-event branch
(`governance_workflow.go:228-231`) rather than creating a row. The shared
`span_id` that ADR-0004 chose deliberately as the pairing mechanism was also what
made the span-dedup check see nothing new. The second POST of every tool call was
substantially a no-op: no new row, no independent evaluation.

That is not a bug in core. It is core's idempotency working exactly as specified
against a payload that told it two different facts were the same event.

**The mirror was a standing unverifiable obligation.** ADR-0004 §Amendment
recorded it as the known weak point: `client/hookspan.go` is a hand-maintained Go
copy of the base SDK's hook contract, and "nothing mechanically compares it
against upstream, so it guards local edits only." Closing it needed a
Python-generated corpus or push access to `openbox-sdk-python`; neither
materialized.

## Decision

**A tool execution is an Activity, not a hook on one.**

- `ToolCall` → `ActivityStarted`
- `ToolResult` → `ActivityCompleted`
- Both hook-less and span-less. Both fully evaluated by core.
- The span layer is retired: `client/hookspan.go` and `client/spanbuilder.go` are
  deleted, along with `AssertHookWireShape`, `HookType`, the family root tuples
  and the hex id derivations.

The base SDK's "hooks are always `ActivityStarted`" rule is not violated, because
**shift-left no longer emits hook events**. The rule binds runtimes that have a
span to attach; without OTel there is no hook-on-an-activity to model, and the
tool call simply *is* the activity.

What holds the two halves together is `activity_id`, unchanged from the previous
shape: derived from `session/tool/locator/operation` with no stage, timestamp or
attempt input, so two separate hook processes — and a rehydrated spool flush —
mint the same id with no shared state. It is also the approval key, which is why
its derivation is pinned byte-for-byte in `client/approval_key_pin_test.go`
rather than left to a refactor's good intentions.

`ActivityCompleted` additionally carries `activity_output` (byte/line counts, and
an exit code if an adapter ever supplies one) and `duration_ms`. The client
computes the duration because nothing else can any more: core derived it from the
stored span.

### What is deliberately not sent

- **`start_time` / `end_time`** — `*float64` of unverified unit with no known
  consumer. `duration_ms` is the field the dashboard reads.
- **`attempt`** — no source. A hook process is stateless and the frozen
  adapter contract has no attempt counter, so it would be permanently null.
- **`error`** — no source *and* no consumer. The frozen `DevEvent`/`Span`
  contract has no failure or exit-code field, and core reads `payload.Error` only
  for `WorkflowFailed` (`storage_event.go:281-286`); an `ActivityCompleted.error`
  is decoded and discarded. Emitting a field with neither producer nor consumer
  would be exactly the overstatement this repo exists to prevent. **A failed tool
  call is therefore indistinguishable from a successful one on the wire** —
  a real gap, recorded rather than papered over. Closing it needs a failure
  signal in the adapter contract, which is a schema change and its own decision.

### Scope held constant

The adapter-facing contract (`contracts/dev-event/schema/dev-event.schema.json`
v1.0) is **unchanged** — no `schema_version` bump. Adapters, mappers, the spool,
the duration stash, local enforcement (INV-3b) and the approval loop are
untouched. `contracts/dev-event/conformance` passed with zero edits, which is the
two-layer split ADR-0004 established working as designed. Event volume is
unchanged: two POSTs per tool call, as before.

## Consequences

**Gained**

- The wire matches the model: a completed call is an `ActivityCompleted`.
- **The completed half becomes a real event again.** Because `event_type` is part
  of core's dedupe key, `(activity_id, ActivityCompleted)` no longer collides with
  `(activity_id, ActivityStarted)`, so the second POST creates its own row and
  gets its own evaluation instead of returning the first one's cached verdict.
  `ActivityCompleted` is accept-listed (`internal/api/governance.go:273-286`),
  goes through real OPA, and is Guardrails-eligible at stage 1 over
  `activity_output` (`internal/services/guardrail.go:192`). This recovers
  governance coverage the hook shape was silently discarding — the strongest
  source-level argument for the change, and the thing Phase 04 must confirm live.
- Retries stay idempotent, and now demonstrably so: a re-POSTed
  `ActivityStarted` matches the full dedupe key and returns the cached verdict
  rather than double-counting.
- `workflow_type="developer-session"` now rides tool events, fixing a silent
  divergence from the base SDK's `ActivityContext.to_payload_fields()`.
- ADR-0004's mirror obligation is dissolved outright — no upstreaming, no
  corpus, no push access needed.
- One serializer instead of two. The map-based hook envelope and its divergent
  key order are gone, and `workflow_id`/`activity_id` now have exactly one
  derivation each, shared with `ApprovalKeyFor`.
- The span's `request_body`/`response_body` are no longer an egress channel at
  all. This removed nothing that worked — no adapter has ever populated either,
  and both mappers assert they stay empty — but it does mean the channel cannot
  be re-opened by an adapter mistake plus a content-capture opt-in.

**Lost — the accepted trade-off**

- **Dev sessions produce zero `spans` rows.** Therefore:
  - **no span-level Merkle leaves.** An auditor reading the Merkle tree for a dev
    session sees event leaves only. Event-level attestation is unaffected;
    span-level attestation is gone. See
    `docs/architecture.md#assurance--what-the-evidence-proves`.
  - **no server-side `semantic_type`.** Core computes it from a span
    (`ComputeSemanticTypeFromSpan`), so `file_read`/`file_write`/`mcp_tool_call`
    classification no longer happens for dev sessions. Consumers classify on
    `tool.kind` and the `activity_input` locators instead.
  - the span is no longer available as a field carrier. Every field it used to
    hold has a documented destination — or an explicit "dropped, because" — in
    MAPPING.md §3, which is the authority on what the serializer reads.
- A failed tool call is not distinguishable from a successful one (above).
- **The approval-status poll becomes ambiguous, and needs a core-side fix.**
  `GetApprovalStatusByWorkflow` (`internal/services/governance.go:290-291`)
  resolves an approval through `FindByWorkflowRunActivity`
  (`internal/datastore/governance_event_pgx.go:74-87`), which filters on
  `(workflow_id, run_id, activity_id)` with **no `event_type` and no
  `ORDER BY`**, then takes `.One()`. Before this change that key was unique per
  tool call, because a `ToolResult` updated the existing row instead of creating
  one. Now two rows share it.

  The primary hold is unaffected: escalation and polling happen pre-execution
  (`adapters/common/hookflow/gate.go`, `approvalhold.go`), so the
  `ActivityCompleted` row does not exist yet. The exposure is the **retry after a
  completed attempt** — which is precisely the path `operation_id` exists to
  support. A second attempt at an operation that already completed once shares
  its `activity_id`, so the poll may return the completed row, whose
  `approval_expiration_time` is NULL. `ApprovalStatus.Decided()` requires a
  non-zero window, so it would read as undecided, the hold or rewake would wait
  out its budget, and a real grant would go unconsumed.

  Fix, core-side: scope the query to `event_type='ActivityStarted'`, or order by
  `approval_expired_at IS NOT NULL DESC, created_at DESC`. Out of scope here —
  this ADR changes no openbox-core code — and listed in MAPPING.md §7 as
  something the live run must exercise. Found in review, not yet observed
  running.

**Not yet proven.** Every claim about core's ingest was established by reading
openbox-core, not by running against it. The load-bearing assumption is that core
stores an `ActivityCompleted` for an existing `activity_id` as its own row rather
than merging, deduping or rejecting it. The dedupe key includes `event_type`
(`validation.go:96`), which says it should — but that is source-reading, and this
repo's own rule is that unit tests and code reading are not evidence that a hook
works. `testbed/run-all.sh` against a live local stack is what settles it; until
that run, MAPPING.md §7 carries the claims as underived.

## Alternatives rejected

**Keep the hook shape, add a real `ActivityCompleted` as a third POST.** Would
preserve span rows and `semantic_type` while fixing the event-type header, at the
cost of 50% more egress per tool call and two representations of one fact. Held
in reserve: if the live run shows core merging or rejecting the second row, the
pre-authorized fallback is `ActivityStarted` → `ActivityCompleted` →
`ActivityCompleted`+`hook_trigger` with the span, which core's event-type-agnostic
span gate does support. That shape would be a knowing divergence from the base
SDK's hook rule and would need its own entry here.

**Keep the span, just fix `event_type`.** Emitting `ActivityCompleted` *with* a
hook envelope and a span is precisely what the base contract forbids and what
ADR-0004 correctly rejected. It would have failed our own mirror assertion, and
kept the fabricated span and the mirror obligation with it.

**Upstream the hook types to `openbox-sdk-python` and keep the mirror honest.**
The original ADR-0004 plan. Still blocked on push access after months, and it
solves the wrong problem: a faithfully-mirrored contract for a span that should
not be fabricated in the first place is a well-maintained answer to a question
shift-left should not be asking.
