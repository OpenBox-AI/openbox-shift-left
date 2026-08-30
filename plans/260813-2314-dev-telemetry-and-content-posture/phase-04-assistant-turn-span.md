# Phase 04 — the assistant-turn span + the filed core ask

## Context links

- Plan: [plan.md](plan.md) · Depends: [phase 01](phase-01-dev-turn-content-carrier.md)
  (authority), [phase 02](phase-02-status-on-tool-results.md) + [phase 03](phase-03-failure-and-lifecycle-hooks.md)
  (same struct, same golden dir)
- Design owner: superseded plan 2200 phase-03 (full detail there; this file is the executable
  merge — deltas: core ask filed, thinking/stop_reason wording)
- Core targets (read-only): `openbox-core internal/services/goal_alignment_session.go:64-88`,
  `internal/services/age.go:109-172`, `internal/services/governance_workflow.go:302-304`,
  `internal/content/session.go:236-256,326-333,451-476`

## Overview

- **Date:** 2026-08-13 · **Priority:** P2 · **Status:** pending · **Effort:** 4h
- Attach ONE minimal wire span to `TurnCompleted` carrying the assistant turn's text as
  `response_body` in the exact shape core's AGE demands — content_capture-gated, secret-redacted
  **before** attach, `capBody`-capped. Unblocks Goal Alignment Trend + Recent Drift. Claude Code
  only; Codex deferred. **Also files the openbox-core ask that retires this stopgap** (merge
  decision 3).

## Key Insights

1. **The carrier is dictated three times over.** AGE reads ONLY `payload.Spans`, only the LAST
   span, only `Stage=="completed" && SemanticType==llm_completion && ResponseBody!=nil`
   (`goal_alignment_session.go:64-72`), unmarshalled as
   `{"choices":[{"message":{"content":"…"}}]}` (`:73-88`). Match exactly.
2. **`semantic_type` cannot be asserted** — core recomputes it (`governance_workflow.go:302-304`);
   only `isLLMCall` (attributes `http.method=="POST"` + LLM-domain `http.url`,
   `session.go:451-476`) or the root-field twin (`session.go:236-256`) yield `llm_completion`.
   Hence synthesized classification keys (OD-0018-1, accepted in phase 01) +
   `openbox.span_synthetic:true`.
3. **Span ids must be deterministic.** Core dedupes on `(span_id, stage)` per session
   (`internal/content/governance.go:255-262`); the crash path re-reports turns
   (`hookrun.go:286-291`). Hash-derive so a re-report is byte-identical:
   `SpanID = hex(sha256("turnspan\x1f"+activity_id))[:16]`,
   `TraceID = hex(sha256("turntrace\x1f"+session_id))[:32]`.
4. **`hook_trigger` stays unset** — with it true + spans present the payload enters the
   approval-bypass fingerprint path (`governance_workflow.go:310-330`).
5. **Text source = `Stop`/`SubagentStop.last_assistant_message`** (docs-recommended; transcript
lags). Leaves `usage.go`'s transcript allowlist and sentinel `TestFinops_NoContentOnWire`
(`usage_test.go:485`) untouched. `stop_reason` stays unbound — **deferred to that decision**,
not "unneeded". Thinking: not in this carrier — that decision owns it (owner posture).
6. **Gates compose both directions.** Turn events exist only under `ResolveFinops()`
   (`hookrun.go:174-183`) + window-has-usage (`mapper.go:262-264`); the span only when `Content`
   survives capture (`stripContent`). Core's goal session is CREATED by `SignalReceived` with
   non-empty `signal_args` (`age.go:113-137`) — sent only under capture; the append Lua returns 0
   with no session (`goal_alignment_session.go:33-41`). Capture off ⇒ nothing anywhere.
7. **Redaction is structural, not remembered:** injected on the `Mapper` collaborator (idiom:
   `Now`, `NewID`, `Finops`, `Posture` — `mapper.go:37-77`), so every `MapTurn` output is
   redacted by construction.

## Requirements

- R1: Exactly one span, `TurnCompleted` only, only when text non-empty after gating+redaction.
- R2: `stage:"completed"`; attributes = classification keys + synthetic marker; `response_body` =
  OpenAI-chat wrapper; `semantic_type` may ship for readability, never relied on.
- R3: `hook_trigger` absent; `span_count` = 1.
- R4: Ids deterministic and stable across re-report.
- R5: Redact **before** attach (local secret detection), then `capBody` 64KB applied to the TEXT
  before JSON-wrapping (`client/payload.go:694-715`).
- R6: `content_capture:false` ⇒ no `spans`, no `span_count`, no assistant text on the wire at all.
- R7: `activity_id`/`event_id`/approval keys byte-unchanged (`deriveID` excludes `Content`).
- R8: Codex unchanged; non-support documented in `capabilities.go`.
- R9: 11 modules `-race` green; both cross-compiles.
- R10: **openbox-core issue filed** (gh, OpenBox-AI/openbox-core): AGE reads assistant content
from the `llm_completion` activity_output for span-less dev sessions — named as the change that
deletes the synthesized keys; link it in that decision and phase 05 docs.

## Architecture

```
Stop/SubagentStop ─▶ hookrun emitTurn (finops-gated, window has usage)
  └─ Mapper.MapTurn: CaptureContent? ─ no ▶ no Content, no span (R6)
                     yes ▶ completed.Content = {Output: m.RedactContent(e.LastAssistantMessage)}
  └─ spool ─▶ flush ─▶ Emit: !contentOn ⇒ stripContent nils Content (second, independent gate)
       └─ buildPayload EventTurnCompleted ⇒ p.Spans = turnAssistantSpan(ev); p.SpanCount = 1
```

New `client/turnspan.go`: `wireSpan` struct (span_id, trace_id, name `"llm_completion"` — must
not uppercase-contain EMBED/TOOL (`session.go:327-332`), kind, stage, start/end epoch **nanos**
via `rfc3339Nanos`, attributes `{"http.method":"POST","http.url":"https://api.anthropic.com/v1/messages","openbox.span_synthetic":true}`,
response_body via `json.Marshal` for correct escaping, semantic_type).
`governanceEventPayload` gains `Spans []wireSpan` + `SpanCount int` appended after `Status`.

Redaction seam: `decision.Redactor` gains exported
`RedactText(s string) (string, []string, bool)` wrapping the same scanner (no second detector);
`adapters/common/hookflow.RedactText` wraps it bounded by `MaxRedactBody` (oversized returned
unchanged — fail-open, same skip-not-truncate rule as the file-body path, `enforce.go:125-134`).
`Mapper.RedactContent func(string) string` (nil ⇒ identity), wired in `hookrun.go:109` from
`ResolveSecretDetection()` (`devconfig.go:367`).

**Codex deferral (record in `capabilities.go`):** no assistant-text field; `Stop` deliberately
unwired (`adapters/codex/capabilities.go:15`); SessionEnd rollup is one message in the same flush
as `WorkflowCompleted`, which DELETES the goal session (`age.go:158-160`) — wrong granularity and
racy ordering. Sourcing from rollout JSONL would widen a numbers-only projection.

## Related code files

| Path | Change |
|---|---|
| `client/turnspan.go` | **new** — `wireSpan`, `turnAssistantSpan(ev)`, id derivations |
| `client/payload.go:28-78,154-160,319-370` | `Spans`/`SpanCount` fields; `EventTurnCompleted` arm; amend `turnActivityOutput`'s INV-2 doc block (turn MAY carry gated text — in the span, not here) |
| `decision/redact.go:60-90` | exported `RedactText` |
| `adapters/common/hookflow/enforce.go:125-155` | bounded `RedactText` helper |
| `adapters/claude-code/hookevent.go:119-136` | bind `LastAssistantMessage`; rewrite the "absence is the safeguard" comment honestly (what changed; `stop_reason` unbound →; thinking →) |
| `adapters/claude-code/mapper.go:37-77,262-318` | `RedactContent` field; `MapTurn` attaches gated+redacted `Content.Output` to the completed half only |
| `adapters/claude-code/hookrun.go:109` | wire redactor from `ResolveSecretDetection()` |
| `adapters/codex/capabilities.go` | alignment-feed non-support entry |
| `client/testdata/golden/` | regenerate turn fixtures; add `activity_turn_completed_content.json` |
| `client/golden_test.go`, `client/payload_test.go`, `client/leakscan_test.go` | content-on case; span shape/determinism/cap/absence tests; turn canary case |
| `adapters/claude-code/enforce_conformance_test.go` | C23/C24/C25 |
| `adapters/claude-code/usage_test.go:485` | EXTEND `TestFinops_NoContentOnWire` (never weaken) |

`MapTurn` callers stay compiling (injected-field design, no signature change):
`hookrun.go:324`, `usage_test.go:530,619`.
Do **not** touch: `usage.go`, `client/hookspan.go`/`spanbuilder.go` (stay deleted), pin tests,
`go.mod`.

## Implementation Steps

1. Reference-only recovery of the retired span shape: `git show 01166bd^:client/spanbuilder.go`
   (id formats, flat layout). Do NOT restore files/family tuples/`AssertHookWireShape`.
2. `decision.RedactText` + tests (same corpus as `Decide`; `changed=false` on untouched input).
3. `hookflow.RedactText` (bounded, fail-open, documented).
4. `hookevent.go`: bind `LastAssistantMessage`; honest comment rewrite.
5. `mapper.go`: `RedactContent`; `MapTurn` completed-half attach
   (`if m.CaptureContent && e.LastAssistantMessage != ""`). Started half, metadata,
   `Model`/`Tokens` untouched.
6. `hookrun.go:109`: wire redactor (nil when secret detection off — honest degradation).
7. `client/turnspan.go` + payload arm: build span when `ev.Content != nil && Content.Output != ""`;
   cap BEFORE wrap; both keys omitted otherwise.
8. Tests — unit: span present/absent per gate; wrapper parses back to the text; ids 16/32 hex,
   identical across two builds; cap at 64K; `SpanCount==1`; no `hook_trigger` key; name untainted.
   Conformance: `C23 a turn span carries the assistant text only with content capture on`;
   `C24 content_capture:false emits no span and no span_count`;
   `C25 a secret in the assistant text never reaches /evaluate` (assert on outbound bytes, C18
   discipline). Extend `TestFinops_NoContentOnWire`; if it passes trivially, that is a defect
   (CLAUDE.md).
9. Regenerate goldens; READ the diff (turn fixtures gain spans/span_count in the content-on case
   only; tool fixtures unchanged; ids unchanged anywhere).
10. `go test ./... -race` per module; both cross-compiles.
11. **File the core ask**: gh issue on openbox-core — "AGE: read assistant content from
    `llm_completion` activity_output for span-less dev sessions" citing
    `goal_alignment_session.go:64-88` +; link it back into that decision's OD-0018-1 section.
12. Commits: `feat(client): carry the assistant turn text on one wire span`,
    `feat(claude-code): bind the assistant turn message under the content gate`,
    `feat(decision): expose text redaction for content bodies`.

## Todo list

- [ ] `decision.RedactText` + `hookflow.RedactText` + tests
- [ ] `LastAssistantMessage` bound + honest comment (stop_reason/thinking →)
- [ ] `Mapper.RedactContent` + gated `MapTurn` attach + hookrun wiring
- [ ] `client/turnspan.go` + payload arm (cap-before-wrap, deterministic ids)
- [ ] Codex non-support entry
- [ ] Unit + C23/C24/C25 + leakscan turn case + extended sentinel
- [ ] Goldens regenerated, diff read; 11 modules `-race` + cross-compiles
- [ ] Core ask filed and cross-linked

## Success Criteria

- Capture ON: exactly one span, `stage=="completed"`, attributes satisfy `isLLMCall`,
  `response_body` → `choices[0].message.content == <text>`. Capture OFF: no spans/span_count/text
  anywhere in the bytes (C24 + leakscan). Secret never in the bytes (C25).
- Byte-identical ids across two builds of the same turn.
- Pin tests green with zero edits; sentinel strictly stronger (diff shows added assertions only).
- No `spans` key on tool/signal/lifecycle payloads; Codex goldens unchanged.
- Core issue exists with citations; that decision links it.

## Risk Assessment

| Risk | L×I | Mitigation / pre-decided response |
|---|---|---|
| `last_assistant_message` absent on `SubagentStop` | M×M | Gate is `!= ""` ⇒ no span, no error. Signal: phase 6 shows main-thread-only alignment. Response: document subagent turns as unfed; do NOT reach into the transcript |
| Field absent on BOTH hooks (provider changed) | L×H | Signal: step 8 test against a real captured payload binds nothing. Response: stop-and-replan onto transcript source — needs that decision amendment first |
| Core classifies ≠ `llm_completion` ⇒ AGE silent | M×H | Path traced (`ComputeSemanticTypeFromSpan`→`classifyLLMType`). Signal: `spans.span_type != 'llm_completion'` live. Response: add the root `http_method`/`http_url` twin (`session.go:236-256`) |
| Synthesized `http.url` trips an org policy on HTTP egress | M×M |; turns never gate (`Stop` writes no stdout) so nothing blocks. Response: switch to root-field variant |
| Re-reported turn stores a second span row | M×M | Deterministic ids + core `(span_id,stage)` dedupe; lost-200 window stays the documented server-side ask |
| Redaction forgotten on a future path | M×H | Structural (Mapper collaborator) + C25 on outbound bytes |
| Alignment still silent (LlamaFirewall unset) | M×M | Precondition, phase 06 P0 — not a defect of this phase |

## Security Considerations

- **This is the egress change.** Assistant text leaves the machine under capture: redacted
  (better than the prompt today — known limit, unchanged here), 64KB-capped, absent when off.
- Redact-before-attach asserted on outbound bytes (C25), never on intermediates.
- Exactly ONE new string on `HookEvent`. `stop_reason`, `tool_response`, transcript fields: each
is its own that decision decision.
- Server-side the text becomes Guardrails/OPA-visible and lands in `spans` + Merkle leaves — a
real retention increase, out of shift-left's control.

## Next steps

Phase 05 — reconcile every doc/contract surface with what actually shipped.
