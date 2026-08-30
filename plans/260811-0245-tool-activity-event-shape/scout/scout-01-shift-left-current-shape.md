# Scout 01 — what shift-left emits today (first-hand, this repo)

Scope: the wire shape for a tool execution, and where the code decides it.
Every claim cited to `path:line` in `openbox-shift-left`.

## 1. Emission topology

`Emit` is strictly **1 DevEvent → 1 POST → 1 verdict**, keyed by `EventID` as the
idempotency key ([client/client.go:168](../../../client/client.go)). Any change that makes one tool
execution produce two POSTs must mint a second distinct idempotency key
(`client/client.go:195` passes `ev.EventID` verbatim to `post`).

Spool → flush path: adapter `emit()` → `Engine.Record` (spool append, no I/O) →
`Engine.Flush` → `Emitter` → `Client.Emit`
([adapters/common/hookflow/engine.go:57,97](../../../adapters/common/hookflow/engine.go)).

Tool events originate at exactly 4 non-test sites:
- `adapters/claude-code/mapper.go:191` (PreToolUse → `ToolCall`), `:197` (PostToolUse → `ToolResult`)
- `adapters/codex/mapper.go:186`, `:192` (same)
- plus `adapters/{claude-code,codex}/enforce.go` build a `ToolCall` for the *local* decision (never egressed as its own event).

## 2. The wire split

`buildPayload` splits by class ([client/payload.go:130](../../../client/payload.go)):
- `ToolCall`/`ToolResult` → `buildHookPayload` (`payload.go:227`)
- everything else → `buildLifecyclePayload` (`payload.go:158`), via
  `lifecycleWireType` (`payload.go:201`).

Declared wire types (`payload.go:73-78`): `WorkflowStarted`, `WorkflowCompleted`,
`SignalReceived`, `ActivityStarted`. **`ActivityCompleted` does not exist anywhere
in this repo** — not as a constant, not in `lifecycleWireType`.

`BuildHookEvent` hardcodes the envelope ([client/spanbuilder.go:202-220](../../../client/spanbuilder.go)):
`event_type:"ActivityStarted"`, `hook_trigger:true`, `span_count`, `spans`,
`activity_id`, `activity_type` — **stage-independent**. The span's `stage` field is
the only started-vs-completed distinguisher on the wire.

## 3. Confirmed by the pinned golden fixtures

`client/testdata/golden/hook_shell_started.json` and `hook_file_completed.json`
both carry `"event_type":"ActivityStarted"`, `"hook_trigger":true`,
`"span_count":1`; they differ only in `spans[0].stage` (`started` vs `completed`),
`end_time`, `duration_ns`. This is byte-identical to the user's screenshot
(`ActivityStarted` header, one nested `stage:"completed"` span with `duration_ns`).

So the reported symptom is **the designed behavior**, not a regression.

## 4. Why it was designed that way (and where that is written down)

- `contracts/dev-event/MAPPING.md:90-98` — "The `ToolCall`/`ToolResult` pair — key
  correction (E7-S4)": both stages are `ActivityStarted`+`hook_trigger`; "A
  `ToolResult` is **not** `ActivityCompleted`". Rationale given: the base SDK's
  `wire_event_type()` forces `ActivityStarted` for any `hook_trigger` event, and
  `ActivityCompleted` must not carry spans.
- `:25-30` — Accepted; explicitly
  *reverses* an earlier draft that mapped `ToolResult`→`ActivityCompleted`.
- `client/hookspan.go:102-126` — `AssertHookWireShape` **fails** any hook payload
  whose `event_type != "ActivityStarted"`. Shift-left's own conformance gate
  would reject the shape the user is asking for.

Base-SDK corroboration (read-only reference `openbox-sdk-python`):
- `openbox_core/contracts/events.py:146-151` `wire_event_type()` → `ActivityStarted`
  for any `hook_trigger`.
- `openbox_core/validation/event_rules.py:79-85` raises
  `ACTIVITY_COMPLETED_WITH_SPANS` — `ActivityCompleted` must not carry spans.
- `openbox_core/validation/event_rules.py:112-118` raises `HOOK_WRONG_WIRE_TYPE`
  for any hook event that is not `ActivityStarted`.

## 5. The gap that decision did *not* consider

The base SDK's hook events are **attachments to an activity that something else
declares**. `activity_started()`/`activity_completed()` exist as first-class
factories (`events.py:246,277`) and are *not called anywhere inside
`openbox_core`* — the agent runtime calls them. Shift-left calls **neither**.

Consequence: for a developer session, core/the dashboard receive hook spans bound
to an `activity_id` for which **no activity lifecycle pair was ever emitted**. The
tool execution is treated as an activity by `activity_id` alone. That is the
substance of the user's objection, and it is a real modeling gap distinct from the
"which wire type carries the completed span" question that decision settled.

## 6. Second, independent divergence found: `workflow_type` missing on the hook path

The base SDK merges `ActivityContext.to_payload_fields()` into every hook body,
and that map **includes `workflow_type`**
(`openbox-sdk-python/openbox_core/contracts/context.py:45-58`).

`buildHookPayload` sets only `source`, `workflow_id`, `run_id`, `timestamp`,
`metadata` (`client/payload.go:242-247`) — **no `workflow_type`**. The struct path
carries `workflow_type:"developer-session"` (`payload.go:142,175`); the hook path
is a separate map envelope and omits it. `payload.go:31-36` documents the omission
as deliberate ("Absent on the ActivityStarted hook path").

Both golden hook fixtures confirm the absence.

Why it may matter: MAPPING.md:20-22 states core keys a session by
`(workflow_id, run_id, workflow_type)`. If that is true of the ingest path, tool
events resolve to a *different* identity than the session's `WorkflowStarted`.
**Needs core-side confirmation** — see researcher-01 Q1/Q4.

## Unresolved questions

1. Does core persist `spans` when `event_type == "ActivityCompleted"`, or is span
   storage gated on `hook_trigger`/`ActivityStarted`? (researcher-01 Q3)
2. Does core upsert spans by `span_id` (so the two stages collapse into one span
   row), or insert one row per POST? (researcher-01 Q3/Q4)
3. Does the dashboard pair rows by `activity_id`/`span_id` or by the
   `Started`/`Completed` type suffix? (researcher-02 Q2) — MAPPING.md:178 flagged
   this exact risk as unverified.
4. Does the missing `workflow_type` on the hook path actually split session
   identity at core? (researcher-01 Q1/Q4)
