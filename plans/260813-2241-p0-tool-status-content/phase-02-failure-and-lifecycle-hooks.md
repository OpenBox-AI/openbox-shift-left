# Phase 02 — Failure + lifecycle hooks

## Context links

- Parent: [plan.md](plan.md) · Depends: phase 01 (Status field exists)
- Hook fields: research report §2 (verified against code.claude.com/docs/en/hooks, 2026-08-13)
- Signal precedent: `client/payload.go` wireEventType — PromptSubmitted→`SignalReceived("prompt_submitted")`, CommitCreated→`"commit_created"`

## Overview

- Date: 2026-08-13 · Priority: P1 · Status: pending · Review: n/a
- Wire four new hook events, structural fields ONLY (free-text `denial_reason`/`error_message` deferred to
That decision content phases): `PostToolUseFailure` → failed ToolResult; `SubagentStart`/`PermissionDenied`/
`StopFailure` → new DevEvent types riding stock `SignalReceived` (INV-8). Plus Task `subagent_type` extraction.

## Key Insights

- New DevEvent types map onto the existing 5 accept-listed wire types — no core change, no new endpoint (repo rule).
- `PostToolUseFailure` carries the common tool fields (tool_name, tool_use_id, tool_input) → same `mapTool`
  path works; duration stash `TakeStart` keys on the same pairing key, so failed calls get real `duration_ms`.
- Assumption (verify phase 03): PostToolUse fires only on success; failure fires PostToolUseFailure INSTEAD.
  If both fire: two completed rows (distinct event_ids, same activity_id) — over-report direction, safe.
- Older Claude Code versions may not know the new hook names — an unknown hook key in settings/plugin is
  simply never invoked (degrade = absence of new events, fail-open); verify against installed 2.1.229 + one older doc.
- `StopFailure` doc: "output and exit codes are ignored" — still stdout-forbidden discipline applies (INV-3).

## Requirements

1. `HookPostToolUseFailure`: → `EventToolResult`, `Status:"failed"`, EndedAt, mapTool, toolMetadata, duration pairing.
   Optional structural extras if payload provides an enum (verify live) — nothing free-text.
2. `HookSubagentStart` → new `EventSubagentStarted` → `SignalReceived("subagent_started")`; metadata `agent_id`/`agent_type` (capStr).
3. `HookPermissionDenied` → `EventPermissionDenied` → `SignalReceived("permission_denied")`; metadata: tool identity,
   `tool_use_id`, `classifier_verdict` (bool|null tri-state — bind as *bool). NO `denial_reason` in P0.
4. `HookStopFailure` → `EventAPIError` → `SignalReceived("api_error")`; metadata `error_type` via `enumOr` allowlist
   {rate_limit, overloaded, authentication_failed, oauth_org_not_allowed, billing_error, invalid_request,
   model_not_found, server_error, max_output_tokens, unknown}. NO `error_message` in P0.
5. Task tool calls (`classifyTool` catch-all): extract `subagent_type` from tool_input (identifier, capStr) → ToolCall metadata.
6. Wiring surfaces updated: `hookNames` map, plugin `hooks/hooks.json`, `localhooks.go` writer, installer help text,
   `capabilities.go` entries, `contracts/dev-event/schema` event-type additions, COVERAGE.md.

## Architecture

Hook argv subcommand → `ParseHookName` → `RunHook` observe path (none of the new hooks gate; only PreToolUse
enforces) → `Mapper.Map` new cases → spool/flush. `deriveID` distinguishes via EventType+timestamp; signal
events carry no Span (same as PromptSubmitted).

## Related code files

- `adapters/claude-code/hookevent.go` — hook consts, `hookNames`, bind `classifier_verdict *bool`; NOTE: keep the
  "Stop binds nothing" doc-comment honest — StopFailure binds `error_type` only, extend comment accordingly
- `adapters/claude-code/mapper.go` — 4 new cases + Task subagent_type in `toolMetadata`/`mapTool` path
- `adapters/claude-code/hookrun.go` — routing (observe path; confirm Stop/SubagentStop special-casing untouched)
- `client/event.go` — new EventType consts; `client/payload.go` — wireEventType rows
- `adapters/claude-code/plugin/` hooks manifest + `localhooks.go` + `installer.go` + `capabilities.go`
- `contracts/dev-event/schema/`, `contracts/dev-event/conformance/`, `COVERAGE.md`
- `adapters/common/hookflow/duration.go` — no change expected (key reuse); verify TakeStart call site covers failure hook

## Implementation Steps

1. Hook consts + hookNames + ParseHookName tests.
2. Mapper cases + unit tests (incl. failed duration pairing across processes: PutStart on Pre, TakeStart on Failure).
3. EventType consts + wireEventType mapping + payload tests + schema + conformance cases (assert signal_name,
   metadata enums, and ABSENCE of reason/message strings on outbound bytes).
4. Task subagent_type extraction (tool_input decode alongside existing filePath/command accessors — structural only).
5. Wiring: plugin hooks manifest, localhooks writer, installer text, capabilities entries. Verify `claude` 2.1.229
   accepts the hook names (docs + dry session).
6. COVERAGE.md + MAPPING.md rows.

## Todo list

- [ ] Hook consts/parse/bind
- [ ] Mapper 4 cases + Task subagent_type
- [ ] EventTypes + wire mapping + schema
- [ ] Conformance (bytes-level, absence assertions)
- [ ] Wiring surfaces (plugin/local/installer/capabilities)
- [ ] COVERAGE/MAPPING rows

## Success Criteria

- Simulated stdin payloads for all 4 hooks produce correct wire bytes (conformance); unknown-hook argv still fails
  closed at parse with fail-open exit (existing behavior); all touched modules `-race` green.

## Risk Assessment

- Both-hooks-fire world → duplicate completed rows: safe direction (over-report), server dedupe ask already filed; document.
- Hook-name drift across CC versions → events silently absent; mitigation: capabilities.go documents min version; absence-of-events≠absence-of-work already a documented product limit.
- Conformance parity with Codex (`conformance_parity_test.go`): new cases are claude-code-specific — confirm parity harness allows provider-local cases before authoring; if shared-only, place under adapter tests instead.

## Security Considerations

- All new fields identifier/enum class, capStr/enumOr-bounded at the untrusted boundary; `classifier_verdict` is a bool.
- Free-text reason/message deliberately unbound — same "no field to land in" construction as tool_response today.

## Next steps

Phase 03 verifies end-to-end + updates docs.
