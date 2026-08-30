# ADR-0004 — Unify dev telemetry onto the base SDK wire model

> **Superseded in part by [ADR-0013](ADR-0013-tool-call-as-activity.md)
> (2026-08-11):** the `ToolCall`/`ToolResult` rows below and the §Amendment
> mirror obligation. Tool calls are now `ActivityStarted`/`ActivityCompleted`,
> span-less, and `client/hookspan.go` is deleted. Everything else here stands,
> and the reasoning below is left intact — it is the true record of why the
> hook shape was the right answer under the premises that then held.

Status: Accepted — **reconstructed 2026-07-31**, amended, and superseded in part
(see the note above).
Reconstructed from: `contracts/dev-event/MAPPING.md`, `client/payload.go`,
`client/hookspan.go`, `client/acceptancetest/acceptance_test.go`.

## Context

Shift-left originally emitted its own developer-runtime `event_type` strings.
openbox-core accept-lists event types, so those required an EXT-core patch — a
fork of the data plane that every deployment had to carry.

## Decision

Keep the normalized dev-event vocabulary as the adapter-facing contract, and map
it onto base wire types openbox-core already accepts. The dev event stays the
thing adapters produce; only the wire representation changes.

The mapping (MAPPING.md §2):

- a session is a workflow: `SessionStarted` → `WorkflowStarted`,
  `SessionEnded` → `WorkflowCompleted`;
- `PromptSubmitted` / `CommitCreated` / `Deploy` → `SignalReceived` with a
  `signal_name`;
- `ToolCall` / `ToolResult` → `ActivityStarted` with `hook_trigger`, paired by a
  shared span id, distinguished only by the span's `stage`.

The last one reverses an earlier draft that mapped `ToolResult` to
`ActivityCompleted`: both stages are `ActivityStarted`, because that is what the
base SDK does.

This retires the EXT-core accept-list patch. A stock core accepts every event.

## Amendment (E7-S1)

The original plan upstreamed the `shell`/`mcp`/`tool` hook types to
`openbox-sdk-python` so both SDKs shared one definition. Push access was not
available, so shift-left carries a **Go mirror** of the base hook contract in
`client/hookspan.go`, clearly marked as such, with an assertion function
mirroring the Python conformance check.

The mirror is the known weak point: nothing mechanically compares it against
upstream, so it guards local edits only. Closing that needs a corpus generated
by the Python SDK, or push access to retire the mirror.

## Consequences

- No EXT-core patch; the acceptance suite proves a stock core accepts everything.
- Wire bytes are a hard contract — RF-H2 pinned them with golden fixtures.
- The mirror is a standing obligation until upstreaming is possible.
