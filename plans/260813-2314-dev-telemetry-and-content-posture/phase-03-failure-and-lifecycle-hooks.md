# Phase 03 — failure + lifecycle hooks (structural only)

## Context links

- Plan: [plan.md](plan.md) · Depends: [phase 02](phase-02-status-on-tool-results.md) (Status field
  + v1.2 exist; step-1 evidence decides this phase's wiring)
- Design owner: superseded plan 2241 phase-02
- Hook fields: research report §2 (verified against code.claude.com/docs/en/hooks, 2026-08-13)
- Signal precedent: `client/payload.go` wireEventType — `PromptSubmitted`→`SignalReceived("prompt_submitted")`,
  `CommitCreated`→`"commit_created"`

## Overview

- **Date:** 2026-08-13 · **Priority:** P1 · **Status:** pending · **Effort:** 5h
- Wire four hooks, **structural fields ONLY** (free-text `denial_reason`/`error_message` deferred
to that decision phases): `PostToolUseFailure` → failed `ToolResult`
(`status:"failed"`); `SubagentStart`/`PermissionDenied`/`StopFailure` → new DevEvent
types riding stock `SignalReceived` (INV-8, no core change). Plus Task `subagent_type`
extraction.

## Key Insights

1. New DevEvent types map onto the existing accept-listed wire types — no new endpoint/table
   (repo rule: reuse, don't rebuild).
2. `PostToolUseFailure` carries the common tool fields (tool_name, tool_use_id, tool_input) →
   the existing `mapTool` path works; the duration stash `TakeStart` keys on the same pairing
   key, so failed calls get real `duration_ms`.
3. Phase 02 step 1 verified the firing semantics. Expected: failure hook fires INSTEAD of
   `PostToolUse`. If both fire: two completed-side rows (distinct `event_id`, same `activity_id`)
   would **corrupt SUCCESS%** (success + failed both count against one `.total`) — that is the
   stop-and-replan branch, not "safe over-report".
4. Unknown hook keys in settings are never invoked by older Claude Code versions — degrade =
   absence of new events, fail-open. Verify against installed 2.1.229 + one older docs snapshot.
5. `StopFailure` docs: "output and exit codes are ignored" — stdout-forbidden discipline still
   applies (INV-3).
6. New hooks must be **registered by the installer** (settings template) or they never fire;
   existing installs need re-init — an upgrade note phase 05 documents.

## Requirements

- R1: `HookPostToolUseFailure` → `EventToolResult`, `Status:"failed"`, `EndedAt`, `mapTool`,
  toolMetadata, duration pairing. Structural extras only if the payload provides an enum (verify
  live) — nothing free-text.
- R2: `HookSubagentStart` → `EventSubagentStarted` → `SignalReceived("subagent_started")`;
  metadata `agent_id`/`agent_type` (capStr).
- R3: `HookPermissionDenied` → `EventPermissionDenied` → `SignalReceived("permission_denied")`;
  metadata: tool identity, `tool_use_id`, `classifier_verdict` (tri-state → bind as `*bool`).
  **NO `denial_reason`** in this plan.
- R4: `HookStopFailure` → `EventAPIError` → `SignalReceived("api_error")`; metadata `error_type`
  via `enumOr` allowlist {rate_limit, overloaded, authentication_failed, oauth_org_not_allowed,
  billing_error, invalid_request, model_not_found, server_error, max_output_tokens, unknown}.
  **NO `error_message`** in this plan.
- R5: Task tool calls (`classifyTool` catch-all): extract `subagent_type` from `tool_input`
(identifier, capStr) → ToolCall metadata. No `prompt`/`description` egress.
- R6: Installer registers the new hook keys at both scopes it manages; absence on older versions
  degrades fail-open; INV-3 preserved (no stdout on any new hook).
- R7: Schema: new DevEvent types added to the enum the guard pins
  (`contracts/dev-event/conformance/schema_guard_test.go:27`) — still v1.2 (same bump as phase 02);
  conformance fixtures for each new event.
- R8: `activity_id`/`event_id` derivation untouched; pin tests green unedited; 11 modules `-race`
  + both cross-compiles.

## Architecture

```
PostToolUseFailure ─▶ mapTool path ─▶ EventToolResult(Status:"failed", duration paired) ─▶ ActivityCompleted
SubagentStart      ─▶ EventSubagentStarted  ─▶ SignalReceived("subagent_started")
PermissionDenied   ─▶ EventPermissionDenied ─▶ SignalReceived("permission_denied")
StopFailure        ─▶ EventAPIError         ─▶ SignalReceived("api_error")
Task tool_input.subagent_type ─▶ ToolCall metadata (existing event, one more structural key)
```

No engine (hookflow) change; adapter + installer + client accept-list only.

## Related code files

| Path | Change |
|---|---|
| `adapters/claude-code/hookevent.go` | new hook consts; structural fields only (`classifier_verdict *bool`, `error_type` string→enumOr at map time) |
| `adapters/claude-code/mapper.go` | four new cases + Task `subagent_type` extraction in `classifyTool` path |
| `adapters/claude-code/installer.go` (settings template) | register the four hook keys |
| `client/event.go` | new DevEvent type consts |
| `client/payload.go` | wireEventType mapping to `SignalReceived(<name>)`; ToolResult failed path already handled by phase 02's `statusFor` |
| `contracts/dev-event/schema/dev-event.schema.json` | event-type enum additions (v1.2) |
| `contracts/dev-event/conformance/testdata/valid/` | one fixture per new event |
| `client/testdata/golden/` | new golden per new signal event + `activity_tool_failed.json` |
| `adapters/claude-code/enforce_conformance_test.go` | `C22 a failed tool call reports status "failed"` + signal-event cases |
| `adapters/claude-code/mapper_test.go`, duration-stash tests | pairing + tri-state + enumOr tests |

Do **not** touch: `adapters/codex/**` behaviour (no failure hook — documented in phase 02),
pin tests, `usage.go`, `go.mod`.

## Implementation Steps

1. Re-check phase 02 step-1 evidence for the extra hooks (`SubagentStart`, `PermissionDenied`,
   `StopFailure` stdin shapes) against the live docs + one empirical capture each in the scratch
   project (permission denial is producible by denying a permission prompt; StopFailure needs a
   forced API error — if not producible, wire from the documented shape and mark it
   docs-only-verified).
2. Hook consts + structural bindings in `hookevent.go` (bools/ints/identifier strings only; every
free-text field left unbound with a comment).
3. Mapper cases + Task `subagent_type` extraction; `enumOr` for `error_type`; `*bool` tri-state
   for `classifier_verdict`.
4. Client: DevEvent consts + `SignalReceived` name mapping; golden fixtures.
5. Installer template: register hooks; verify `openbox init` idempotence (re-init upgrades an
   existing install without duplicating entries).
6. Schema enum + conformance fixtures; C22 + one conformance case per signal event asserting the
   wire `signal_name` and the absence of free-text keys.
7. Older-version degrade check: settings with unknown hook keys against an older binary/doc —
   record the evidence.
8. Test sweep: 11 modules `-race`; both cross-compiles; leakscan unchanged.
9. Commits: `feat(claude-code): wire tool-failure and lifecycle hooks` +
   `feat(client): carry failed tool results and lifecycle signals`.

## Todo list

- [ ] Hook payload shapes captured/verified per hook
- [ ] hookevent consts + structural bindings (free-text deliberately unbound)
- [ ] Mapper cases + Task subagent_type + enumOr/error_type + *bool tri-state
- [ ] DevEvent consts + SignalReceived mapping + goldens
- [ ] Installer registration + re-init idempotence
- [ ] Schema enum + fixtures + C22 + signal conformance cases
- [ ] Older-version degrade evidence recorded
- [ ] 11 modules `-race` + cross-compiles green; pin tests untouched

## Success Criteria

- A failed tool call produces `ActivityCompleted` with `"status":"failed"` and a real
  `duration_ms` on the outbound bytes (C22).
- Each lifecycle hook produces its `SignalReceived` with the expected `signal_name` and **no**
  free-text field anywhere in the outbound bytes (asserted per event).
- `openbox init` on an already-governed project adds the new hooks exactly once.
- Older Claude Code with the new settings keys: session runs clean, new events simply absent.

## Risk Assessment

| Risk | L×I | Mitigation / pre-decided response |
|---|---|---|
| Both PostToolUse and PostToolUseFailure fire per failed call ⇒ SUCCESS% corrupt | L×H | Phase 02 step 1 checked. Signal: two completed-side events share an `activity_id` in the spool. Response: stop-and-replan (spool-local suppression keyed on `tool_use_id` is the candidate) |
| StopFailure not empirically producible | M×L | Wire from documented shape, mark docs-only-verified; phase 06 lists it under "not covered" |
| Hook payload field names drift from docs | M×M | Empirical capture in step 1 is the authority; docs are the map, the spool is the territory |
| New settings keys break older versions | L×H | Degrade check in step 7; unknown keys are documented as never-invoked (fail-open) |
| `classifier_verdict` bound as bool loses the null case | M×L | `*bool` tri-state, mirrored from the `Enforce *bool` lesson |

## Security Considerations

- Structural-only is the line: `denial_reason` and `error_message` are free text a model or user
wrote — they egress only under that decision's phases with redaction, not
here.
- INV-3: none of the new hooks may write stdout (StopFailure's is ignored by the provider, but
  discipline is uniform).
- The permission-denied event reveals that a policy denied something (metadata), not what the
  content was — keep it that way in fixtures and docs.

## Next steps

Phase 04 — the assistant-turn span (the egress change).
