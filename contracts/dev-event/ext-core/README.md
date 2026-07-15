# EXT-core — RETIRED (ADR-0004 / E7-S2)

> **Status: retired 2026-07-15.** This directory previously held the SL-13
> "external dependency" that patched openbox-core's `isValidGovernanceEventType`
> accept-list to admit shift-left's 7 developer-runtime `event_type` strings
> (`SessionStarted`/`PromptSubmitted`/`ToolCall`/`ToolResult`/`SessionEnded`/`CommitCreated`/`Deploy`).

**Why it's gone.** [ADR-0004](../../../.fab7/sdlc/design/adr/ADR-0004-unify-dev-events-onto-base-wire-model.md)
unified shift-left telemetry onto the base SDK's **stock** wire vocabulary, and
epic **E7** built it (E7-S3/S4/S5). The OpenBox client no longer emits a
developer-specific `event_type`; it now maps every dev event onto a wire type
openbox-core **already** accept-lists:

| Dev event | Base wire `event_type` |
|---|---|
| `SessionStarted` / `SessionEnded` | `WorkflowStarted` / `WorkflowCompleted` |
| `PromptSubmitted` / `CommitCreated` / `Deploy` | `SignalReceived` (`signal_name`) |
| `ToolCall` / `ToolResult` | `ActivityStarted` + `hook_trigger` (span stage started/completed) |

Because these are stock types, **no accept-list patch is needed** — E7-S0 (spike
S8) confirmed every one returns HTTP 200 on stock core, and **E7-S2** removed the
accept-list additions + `SessionStarted`/`SessionEnded` lifecycle special-cases
from openbox-core (`internal/api/governance.go`, `internal/content/governance.go`,
`internal/services/activities/governance/storage_session.go`). The SL-13 drift
guard (`../conformance/extcore_drift_test.go`) and patch artifacts
(`openbox-core-dev-event-types.patch`, `dev-event-types.json`, `apply.sh`) were
deleted with this retirement.

The one *additive* openbox-core change that E7 keeps is the semantic classifier:
`ComputeSemanticTypeFromSpan` now first-classes `shell`→`shell_command` and
`mcp`→`mcp_tool_call` (E7-S2). That is a classification enrichment, not an
accept-list — it needs no external patch dependency.

See [`../MAPPING.md`](../MAPPING.md) for the live wire mapping.
