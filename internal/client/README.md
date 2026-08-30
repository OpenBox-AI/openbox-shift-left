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
| **INV-5** client event id for idempotent ingestion | `DevEvent.EventID` required (deterministic + collision-safe — adapter `deriveID`); carried in `metadata.event_id` (core has no first-class field) **and** the `Idempotency-Key` header; retries reuse the identical key/body. **Server-side dedupe is partial** — see below |

## No spans, so no semantic_type

The client sends **no spans** ([ADR-0013](../docs/adr/ADR-0013-tool-call-as-activity.md)).
A tool call is two activity events — `ToolCall` → `ActivityStarted`, `ToolResult`
→ `ActivityCompleted` — sharing an `activity_id`, and neither carries `spans`,
`span_count` or `hook_trigger`.

core computes `semantic_type` from a span (`ComputeSemanticTypeFromSpan`), so for
developer sessions it computes none. `tool.kind` and the `activity_input`
locators carry that distinction instead. The `DevEvent.Span` struct still exists
— the adapter contract is frozen at schema v1.0 and adapters still populate it —
but the client now reads locators and counts *out* of it into
`activity_input`/`activity_output` rather than serializing it.
`contracts/dev-event/MAPPING.md` §3 is the authority on which fields it reads.

## Idempotency (INV-5): the client half is guaranteed

The client owns **half** the idempotency contract and guarantees it (STORY-SL-14):

- **Stable + unique key.** The CC `event_id` is derived deterministically from the
  event's own structural fields (adapters/claude-code `deriveID`), so the same
  logical event always hashes to the same id and two distinct events never
  collide — robust even if the id is ever recomputed from the spooled record.
- **On the wire twice.** It rides in `metadata.event_id` **and** in a standard
  `Idempotency-Key` request header (`== EventID`), constant across every retry.
  The header is inert until core consumes it and is not part of the AIP canonical
  string, so it never perturbs signature verification (verified against
  openbox-core `BuildAgentIdentityCanonicalRequest`).
- **Client at-most-once.** A retry re-sends the identical key (never a fresh one);
  the spool never re-sends an acked event across rotate/flush/recovery.

The **server-side half** is partial and not built here. core dedupes on
`(agent_id, workflow_id, run_id, activity_id, event_type)`
(`activities/governance/validation.go:96`), so a retried **tool** event does match
an existing row and returns its cached verdict — tool events carry an
`activity_id`. Lifecycle and signal events carry none, and
`CheckExistingEventActivity` skips the duplicate check entirely without one
(`validation.go:86-89`), so a retry of a `SessionStarted`/`SignalReceived` after
an ambiguous success (stored, but the 200 was lost) can still be counted twice.
That is telemetry skew in observe, not a safety issue, and closes when core keys
dedupe on the `event_id` / `Idempotency-Key` value.

## Cross-repo alignment (verified via Explore, 2026-07-08)

- **Signing** matches `openbox-temporal-sdk-python/openbox/request_signing.py`
  byte-for-byte: canonical `UPPER(METHOD)\nPATH\nTIMESTAMP\nNONCE\nBODY_SHA256`,
  Ed25519 over raw UTF-8 (no pre-hash), std-base64 signature, `token_urlsafe(24)`
  nonce, lowercase-hex body SHA-256, `+00:00`-offset timestamp — and against
  openbox-core's server-side verifier `BuildAgentIdentityCanonicalRequest`
  (`services/agent.go:93`). The `client_test.go` mock **re-verifies every request
  exactly as core does** (`verifyLikeCore` mirrors `agent.go:165-184`).
- **Payload** mirrors the subset of core's `GovernanceEventPayload`
  (`internal/content/governance.go:186`) the client sets:
  `source="developer-runtime"`, `run_id`=session, `workflow_id`=workspace/DID,
  `duration_ms` as float milliseconds, `metadata` as `json.RawMessage`. Fields
  core populates for Temporal events (`task_queue`, `parent_workflow_id`,
  `attempt`) are deliberately omitted. See `contracts/dev-event/MAPPING.md`.
- **Response** is core's public `GovernanceVerdictPublicResponse`: `verdict` is a
  plain lowercase string (`allow|constrain|require_approval|block|halt`) with a
  legacy `action` fallback — parsed in `verdict.go`.

## No core accept-list patch (INV-8)

The client never sends a developer `event_type` string. Every event maps onto one
of five stock base wire types — `WorkflowStarted`, `WorkflowCompleted`,
`SignalReceived`, `ActivityStarted`, `ActivityCompleted` — all of which are on
core's accept-list (`internal/api/governance.go:273-286`), so a stock core
accepts everything with no patch. `wireTypeFor` returns an error rather than
falling back to the dev string, because emitting a non-accept-listed type
produced a 400 that the fail-open path then swallowed: a new event type would
have gone silently undelivered.

## Test / validate

```bash
cd client && go build ./... && go test ./...
```
