# EXT-core — RETIRED (ADR-0004 / E7-S2)

> **Status: retired 2026-07-15.** This directory previously held the SL-13
> "external dependency" that patched openbox-core's `isValidGovernanceEventType`
> accept-list to admit shift-left's 7 developer-runtime `event_type` strings
> (`SessionStarted`/`PromptSubmitted`/`ToolCall`/`ToolResult`/`SessionEnded`/`CommitCreated`/`Deploy`).

**Why it's gone.** **ADR-0004** (unify dev events onto the base wire model)
unified shift-left telemetry onto the base SDK's **stock** wire vocabulary, and
epic **E7** built it (E7-S3/S4/S5). The OpenBox client no longer emits a
developer-specific `event_type`; it now maps every dev event onto a wire type
openbox-core **already** accept-lists:

| Dev event | Base wire `event_type` |
|---|---|
| `SessionStarted` / `SessionEnded` | `WorkflowStarted` / `WorkflowCompleted` |
| `PromptSubmitted` / `CommitCreated` / `Deploy` | `SignalReceived` (`signal_name`) |
| `ToolCall` / `ToolResult` | `ActivityStarted` / `ActivityCompleted`, span-less (ADR-0013; was `ActivityStarted`+`hook_trigger` with a span) |

Because these are stock types, **no accept-list patch is needed** — E7-S0 (spike
S8) confirmed every one returns HTTP 200 on stock core, and **E7-S2** removed the
accept-list additions + `SessionStarted`/`SessionEnded` lifecycle special-cases
from openbox-core (`internal/api/governance.go`, `internal/content/governance.go`,
`internal/services/activities/governance/storage_session.go`). The SL-13 drift
guard (`../conformance/extcore_drift_test.go`) and patch artifacts
(`openbox-core-dev-event-types.patch`, `dev-event-types.json`, `apply.sh`) were
deleted with this retirement.

No openbox-core change of any kind is required today. E7 once tracked an
additive classifier enrichment here (`ComputeSemanticTypeFromSpan` first-classing
`shell`→`shell_command`); that claim named no owner, could not be verified
against the openbox-core checkout, and is moot since
[ADR-0013](../../../docs/adr/ADR-0013-tool-call-as-activity.md) — developer
sessions send no spans for tool calls, so nothing classifies them. The one span
[ADR-0018](../../../docs/adr/ADR-0018-dev-turn-content-carrier.md) added IS
classified server-side, which is precisely why it has to carry synthesized
`http.*` attributes: the client cannot assert a `semantic_type`, only feed the
classifier that computes one.

See [`../MAPPING.md`](../MAPPING.md) for the live wire mapping.
