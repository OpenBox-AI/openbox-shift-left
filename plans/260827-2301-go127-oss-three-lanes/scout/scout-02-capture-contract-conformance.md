# Scout 02 — capture → contract → conformance path

Read-only map of what a new model-call producer (`:otel:`, `:proxy:`) must extend.
All refs verified by scout against source, 2026-08-27.

## A. Verdict + payload

- `client/verdict.go` — Verdict literals ALLOW/CONSTRAIN/REQUIRE_APPROVAL/BLOCK/HALT;
  `Evaluation` struct (113–126); `WouldBlock()` (135–137); `ApprovalRef()` (150–155);
  GuardrailResult / DriftResult.
- `client/event.go` — `DevEvent` (369–475), EventType enum (53–130), Tool/Tokens/Span/
  Content structs, `TurnIndex` / `AgentID` / `SessionRollup` / `GatewayRequestID` fields.
- `client/payload.go:143–202` — `buildPayload` routes DevEvent → wire payload:
  ToolCall→ActivityStarted, ToolResult→ActivityCompleted, TurnStarted/TurnCompleted→
  Activity(`llm_completion`), SessionStarted/Ended→Workflow*, PromptSubmitted/Deploy→
  SignalReceived.

**signal_args trap:** `client/payload.go:576–593` `signalDetailKeyFor()` routes
PermissionDenied.reason → `metadata.denial_reason`, APIError.error →
`metadata.error_details`. NEVER `signal_args` (core reads it as a NEW USER GOAL).
Rationale doc: `client/event.go:88–94`, `323–342`.

## B. Gateway capture span (the template for `:proxy:`)

- `client/gatewayspan.go` — `gatewayObservedSpan()` (155–199) builds the wireSpan.
- **`attrs["http_status_code"]`** at line 52 — `_code` suffix; the short spelling is
  silently dropped by core. `gatewaySpanAttributes()` (43–65) builds `http.method`,
  `http.url`, `http_status_code`, `openbox.credential_fingerprint`.
- Span fields (176–197): HTTPMethod, HTTPURL, HTTPStatus, CredentialFingerprint.
- `gatewaySpanID` (147) derives a stable id from SessionID + GatewayRequestID
  (re-emit dedupe).
- `capHeaders` (69–140) bounds header maps; `capHeaderValue` caps per BYTE then
  truncates at a rune boundary (131–140).

## C. activity_id namespacing — the collision rule

`client/payload.go:350–373` `turnActivityIDFor()`:

| producer | activity_id shape |
|---|---|
| tool calls | `cc-act-<32 hex>` (no colons) |
| hook turns | `<session>:turn:<index>` |
| gateway turns | `<session>:gateway:<gateway_request_id>` (355–357) |
| Codex rollup | `<session>:usage:rollup` |

Byte pins: `client/approval_key_pin_test.go:50` (`cc-act-e490dad4315c494b702ce1978a4e114b`),
`client/turn_key_pin_test.go:48–51` (`sess-pin-0001:turn:3`,
`sess-pin-0001:agent:agt-77:turn:3`).
`TestTurnActivityIDCannotCollideWithToolCallID` (turn_key_pin_test.go:91–107) holds
the separator invariant.

Schema `oneOf` (dev-event.schema.json, the TurnCompleted branch) requires **exactly
one** activity-id discriminator; scout reports branches for `gateway_request_id`,
`turn_index`, and `session_rollup`. **Verify the exact branch count against the file
before editing** — phase 01 adds branches here.

## D. Contract + conformance

- `contracts/dev-event/schema/dev-event.schema.json` — v1.5, `x-schema-version` const
  at line 28; `x-changelog` is a version→prose map.
- `contracts/dev-event/conformance/conformance.go` — `ValidateDevEvent(raw []byte,
  contentCaptureEnabled bool)` (21–60).
- `adapters/claude-code/conformance_test.go:16–44` — adapter→wire check.
- `adapters/claude-code/enforce_conformance_test.go` — the real C-case harness:
  C1–C7 matrix (93–205), C18–C26 (270+); **`serveCapturing()` (244–259)** captures the
  actual POSTed bytes. **A C-case asserts outbound bytes, never decision logic.**
- Golden fixtures: `contracts/dev-event/conformance/testdata/valid/*.json`
  (session_started, tool_call, tool_result, turn_completed, …).

## E. INV-2 allowlist — the boundary NOT to cross

`adapters/claude-code/usage.go`: projection bound by struct tags only.
`usageNumbers` (86–91) numeric-only. `turnLine` (99–143): timestamp (parsed then
discarded), isSidechain (bool), Message{Model, Content `json.RawMessage`, Usage}.
Every other transcript field unbound.
`maxThinkingBytes = 4 × 65536` (145–156); `thinkingFrom()` (205–222).
Sentinel: `usage_test.go:270–278` `TestFinops_NoContentOnWire`.

**Boundary:** transcript parsing is UNGATED (`readTurnUsage`, 308); the gate is at
`Mapper.MapTurn` attachment; `Mapper.RedactContent` redacts before attach; `capBody`
caps at egress. → Telemetry mappers bind at their OWN edge and must not touch this file.

## F. Redaction + content backstop

- `client/payload.go:541–574` — `contentMetadataKeys`: message, prompt, output,
  content, file_text, diff, patch, body, stdout, stderr, command, input_text,
  denial_reason, error_details, arguments, thinking.
- Line 598 `buildMetadata()` drops those keys when `contentStripped=true`.
- `turnActivityOutput()` (417–461) — capped via `capBody` (451); model+usage+thinking.
- Order (usage.go:56–58): adapter `RedactContent` → client `stripContent` → `capBody`.
- `capBody` = 65,536 **runes**; `maxThinkingBytes` = 4×65,536 **bytes** (collection
  bound must stay larger than the wire bound).

## Consequences for this plan

1. New producers add branches to `turnActivityIDFor` + the schema `oneOf` → contract
   **v1.6** (phase 01).
2. Any new content key (bodies, tool_decision text) MUST join `contentMetadataKeys`
   or it routes around the gate.
3. `http_status_code` spelling is load-bearing for the transport span too.
4. New C-cases follow the `serveCapturing` pattern — assert bytes.
5. `usage.go` untouched; its sentinel must stay green unchanged.

## Unresolved

- Exact count/shape of the TurnCompleted `oneOf` branches — confirm in-file at
phase-08 (contract) execution time
before editing.
