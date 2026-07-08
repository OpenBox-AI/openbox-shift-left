# OpenBox client — AIP-signed `/evaluate` transport (STORY-SL-3)

The shared, reusable data-plane client every developer-runtime adapter
(SL-4 Claude Code, SL-7 Codex, SL-8 Cursor) and the git action (SL-6) use to
emit a normalized developer event to OpenBox.

```
normalized DevEvent ─▶ strip content (INV-2) ─▶ build GovernanceEventPayload
                     ─▶ AIP Ed25519 sign ─▶ POST /api/v1/governance/evaluate
                     ─▶ parse verdict (observe: ignored in Phase 1)
```

It is deliberately **not** the control-plane client. Onboarding/registration
(`POST /agent/create`, human/org credential) lives in `cli/internal/backend`
(STORY-SL-2). This package is the **agent's own** runtime transport: auth is the
agent's `obx_` runtime key + an Ed25519 AIP signature.

## Usage

```go
c, err := client.New(client.Config{
    BaseURL: "https://core.openbox.ai",
    APIKey:  obxRuntimeKey,   // obx_(live|test)_… — from the secret store (INV-1)
    DID:     agentDID,        // did:aip:…
    SeedB64: ed25519SeedB64,  // base64 raw 32-byte seed — from the secret store
    // ContentCaptureEnabled: false  // default: metadata-only (INV-2/OD4)
    Logger:  myLogger,        // optional; fail-open drops are logged here
})
if err != nil { /* unusable identity — construction fault */ }

verdict, err := c.Emit(ctx, client.DevEvent{
    SchemaVersion: client.SchemaVersion,
    EventID:       uuid,               // idempotency key (INV-5)
    EventType:     client.EventToolCall,
    SessionID:     openboxSessionID,   // → core run_id
    DeveloperDID:  agentDID,
    Timestamp:     time.Now().UTC().Format(time.RFC3339),
    Tool:          client.Tool{Name: "Edit", Kind: client.ToolFile},
    Span:          &client.Span{SemanticType: "file_write", Stage: "started", FilePath: p},
})
// Phase-1 observe: err is only ever a caller precondition fault (e.g. no
// EventID); transport failures are fail-open (verdict == VerdictUnknown, nil).
// Callers IGNORE the verdict in Phase 1 (INV-3).
```

## Invariants enforced here

| Invariant | Where |
|---|---|
| **INV-1** obx_ key + Ed25519 seed never logged/leaked | `signing.go` (seed stays in `signer`); `client.go` logs only ids/types/errors; plaintext `http://` to a non-loopback host is refused (`checkBaseURL`) so the bearer key can't travel in the clear |
| **INV-2** strip content when content-capture disabled | `payload.go:stripContent`, gated in `client.go:Emit` (default off) |
| **INV-3** fail-open; never block the caller | `client.go:Emit` returns `(VerdictUnknown, nil)` on any transport error |
| **INV-5** client event id for idempotent ingestion | `DevEvent.EventID` required; carried in `metadata.event_id` (core has no first-class field); retries reuse the identical body. **End-to-end dedupe needs an EXT-core change** — see below |

## semantic_type is set indirectly (verified core behavior)

core **recomputes** every span's `semantic_type` at ingest
(`governance_workflow.go:309` → `ComputeSemanticTypeFromSpan`) from the span
**`name`** + an **`attributes`** map, and **ignores** the inbound `semantic_type`,
`hook_type`, `file_operation`, and `function`. So the client sets the fields core
actually reads (`payload.go:classificationHints`): file ops get a
`name` of `file.write`/`file.read`/… plus a non-nil `file_path`; MCP calls get
`attributes["mcp.method"]="callTool"`. The real tool name is preserved in
`metadata.tool_name`. (This corrects contracts/dev-event/MAPPING.md §3, which
described the mechanism as reading `file_operation`/`function`/`hook_type` —
tracked as a contract doc fix.)

## Idempotency (INV-5) is best-effort until EXT-core

`event_id` is transmitted in `metadata`, but core does **not** currently dedupe
the developer event types (its dedupe paths need `activity_id` / a span unique
constraint, which dev events don't have). So a retry after an ambiguous success
(committed, but the 200 was lost) can double-count. This is telemetry skew in
Phase-1 observe, not a safety issue, and closes when EXT-core keys dedupe on
`metadata.event_id`.

## Cross-repo alignment (verified via Explore, 2026-07-08)

- **Signing** matches `openbox-temporal-sdk-python/openbox/request_signing.py`
  byte-for-byte: canonical `UPPER(METHOD)\nPATH\nTIMESTAMP\nNONCE\nBODY_SHA256`,
  Ed25519 over raw UTF-8 (no pre-hash), std-base64 signature, `token_urlsafe(24)`
  nonce, lowercase-hex body SHA-256, `+00:00`-offset timestamp — and against
  openbox-core's server-side verifier `BuildAgentIdentityCanonicalRequest`
  (`services/agent.go:93`). The `client_test.go` mock **re-verifies every request
  exactly as core does** (`verifyLikeCore` mirrors `agent.go:165-184`).
- **Payload** mirrors core `GovernanceEventPayload` / `SpanData`
  (`internal/content/governance.go:186/266`): `source="developer-runtime"`,
  `run_id`=session, `workflow_id`=workspace/DID, span times are **int64 epoch
  nanoseconds**, `metadata` is `json.RawMessage`. See `contracts/dev-event/MAPPING.md`.
- **Response** is core's public `GovernanceVerdictPublicResponse`: `verdict` is a
  plain lowercase string (`allow|constrain|require_approval|block|halt`) with a
  legacy `action` fallback — parsed in `verdict.go`.

## [EXT-core] dependency

The 7 developer lifecycle `event_type` strings are **not yet** in core's
`isValidGovernanceEventType` accept-list (`internal/api/governance.go:273`), so a
real POST today returns HTTP 400 → a fail-open drop (logged). Integration tests
therefore run against the in-process core-mirror server. Once EXT-core's 3
additive edits land (S6 §3), the same client emits end-to-end unchanged.

## Test / validate

```bash
cd client && go build ./... && go test ./...
```
