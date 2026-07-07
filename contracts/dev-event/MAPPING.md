# Mapping — normalized dev event → openbox-core `GovernanceEventPayload`

**Story:** STORY-SL-1 · **Contract:** [`schema/dev-event.schema.json`](schema/dev-event.schema.json) v1.0
**Target (verified, spike S6 + live code):** `POST /api/v1/governance/evaluate` on **openbox-core**, body `GovernanceEventPayload` (`internal/content/governance.go:186`), spans `SpanData` (`:266`), semantic-type constants (`internal/content/session.go:95`), accept-list `isValidGovernanceEventType` (`internal/api/governance.go:273`).

This doc lets **SL-3** (the OpenBox client) build a `GovernanceEventPayload` from a normalized dev event **without guessing**. The mapping is additive to core (INV-8): it introduces new `event_type` strings and reuses the existing span/metadata shape — **no core wire change**.

---

## 1. Envelope field mapping (every event)

| Normalized dev event | → `GovernanceEventPayload` field | Notes |
|---|---|---|
| — (constant) | `source` = `"developer-runtime"` | Analogue of the SDK's `"workflow-telemetry"`. |
| `event_type` | `event_type` | **Identity.** The 7 lifecycle names below are added to core's accept-list by EXT-core (S6 §3). |
| `openbox_session_id` | `run_id` | Session keyed by `(workflow_id, run_id)`, both NOT-NULL + jointly unique (S6 §4). |
| `developer_did` (or workspace/repo id) | `workflow_id` | Stable per-workspace identity so `(workflow_id, run_id)` is unique per session. |
| `timestamp` | `timestamp` | Core field is a **string** (RFC3339) — pass through verbatim. |
| `span` (when present) | `spans[0]`, and set `span_count` = len(`spans`) | See §3. |
| `metadata` | `metadata` (`json.RawMessage`) | Merge the per-type keys below; JSON object. |
| `tokens`, `cost` | `metadata.tokens`, `metadata.cost` | No first-class payload fields; carried in `metadata` (finops). |
| `developer_did` | — | Auth/identity is via the signed AIP headers + Bearer key, **not** a body field. `from_agent_did`/`multi_agent_session_id` stay empty (Handoff-only). |
| `content.*`, `span.request_body/response_body` | `spans[].request_body` / `response_body` **only when content-capture enabled** | Stripped at the client when disabled (INV-2). Never a first-class payload field. |

`schema_version` and `event_id` are contract/idempotency fields — `event_id` is the client's idempotency key (INV-5); it is not a core payload field and is used client-side for dedupe.

---

## 2. Per-lifecycle-type mapping

| Dev `event_type` | core `event_type` | Carried span `semantic_type` | Key `metadata` fields | Core lifecycle effect (S6 §3, `storage_session.go:40`) |
|---|---|---|---|---|
| `SessionStarted` | `SessionStarted` | *(none)* | `provider` (claude-code\|codex\|cursor), `tool_version`, `repo`, `cwd` | **create** session `(workflow_id, run_id)` |
| `PromptSubmitted` | `PromptSubmitted` | `llm_completion` *(optional; only if turn tokens known)* | `tokens`, `cost`, `model` | mid-session lookup |
| `ToolCall` | `ToolCall` | by `tool.kind`: file→`file_read`/`file_write`/`file_open`/`file_delete`; mcp→`mcp_tool_call`; shell→`internal` | `tool_name` | mid-session; span `stage="started"` |
| `ToolResult` | `ToolResult` | same span type as its `ToolCall` | `tool_name`, `exit_code`? | mid-session; span `stage="completed"` (carries `bytes_*`, `lines_count`) |
| `SessionEnded` | `SessionEnded` | *(none)* | `total_tokens`, `total_cost`, `duration_ms` | **terminal** — closes the session |
| `CommitCreated` | `CommitCreated` | *(none)* | `commit_sha`, `repo`, `branch` | mid-session (or standalone); commit→session binding (FR-5) |
| `Deploy` | `Deploy` | *(none)* | `deploy_id`, `commit_sha`, `repo`, `environment`, `deploy_did` (= git hash + timestamp, FR-6) | standalone; deploy lineage (FR-6/FR-7) |

**Two axes (S6 §7).** The rows above are the **lifecycle** axis (this contract, new to core). The `semantic_type` column is the **semantic-span** axis, which **already exists** in core — the contract carries it, it does not define it.

**`semantic_type` is computed server-side.** Core derives `SpanData.semantic_type` from the span's source fields (`file_path`/`file_operation`, `function`/`module`, `hook_type`) via `ComputeSemanticTypeFromSpan` (`session.go:204`). The adapter/client should set those **source** fields; the `semantic_type` in this contract is the intended target/hint. To land the target reliably, also set `SpanData.hook_type` (`"file_operation"` | `"function_call"`). Shell/command tool calls (`tool.kind=shell`) have **no** shell semantic type in core — they resolve to `internal` (core's fallback); command detail rides in `tool.name` + `metadata` (+ gated `request_body`).

> **Session lifecycle — EXT-core dependency (verified live, G3_REVIEW).** Core creates a session row **only** on `WorkflowStarted` and closes it only on `WorkflowCompleted`/`WorkflowFailed` (`internal/services/activities/governance/storage_session.go:41`); every other `event_type` falls through to `handleSessionLookup` (benign — logs a warn if not found, `Action:"none"`, no error). Therefore `SessionStarted→create` / `SessionEnded→terminal` **requires EXT-core's 3rd additive edit**: extend `handleSessionCreate`/the lifecycle switch to map these two (this is edit #3 of the 3 S6 §3 scoped — constants, accept-list, lifecycle switch). **Do NOT** shortcut by mapping developer sessions onto the existing `WorkflowStarted`/`WorkflowCompleted` strings: those trigger the `Workflow*` OPA-bypass (`opa.go:205`) and the guardrails-ineligible path (`governance_workflow.go:429`), and conflate developer sessions with Temporal workflows — a semantic break, not additive. Distinct types + edit #3 is the correct, INV-8-preserving choice.

---

## 3. Building a `SpanData` from `span`

| Contract `span.*` | → `SpanData` field (`governance.go:266`) |
|---|---|
| `semantic_type` | `semantic_type` (also drives `hook_type`; see above) |
| `stage` | `stage` (`"started"`/`"completed"`) |
| `file_path` | `file_path` |
| `file_operation` | `file_operation` |
| `bytes_read` / `bytes_written` | `bytes_read` / `bytes_written` |
| `lines_count` | `lines_count` |
| `function` | `function` (Go field `FuncName`) |
| `module` | `module` |
| `request_body` / `response_body` | `request_body` / `response_body` **(gated, INV-2)** |
| — (client-generated) | `span_id`, `trace_id`, `name`, `start_time`, `end_time` (int64 epoch) |

The client fills the required transport fields (`span_id`, `trace_id`, `name`, `start_time`, `end_time`) that are not part of the tool-agnostic contract.

---

## 4. Verdict (parsing the response)

The `/evaluate` response envelope is `GovernanceVerdictResponse` (`governance.go:334`). The canonical enum (`$defs.verdict`) is `HALT > BLOCK > REQUIRE_APPROVAL > CONSTRAIN > ALLOW`, but the wire is **lowercase**:

| Canonical (contract) | Wire `verdict` field | Legacy `action` field |
|---|---|---|
| `ALLOW` | `allow` | `continue` |
| `CONSTRAIN` | `constrain` | `continue` |
| `REQUIRE_APPROVAL` | `require_approval` | `require-approval` |
| `BLOCK` | `block` | `stop` |
| `HALT` | `halt` | `stop` |

Phase-1 observe (**D7/INV-3**) treats **every** verdict as allow and never blocks the tool call.

> **G3_REVIEW note.** The story's stated vocab is the 4-value `HALT|BLOCK|REQUIRE_APPROVAL|ALLOW`. Live core has a **5th tier `CONSTRAIN`** (Verdict=1, sandbox-enforcement, future) and serializes lowercase. The contract defines all 5 so a parser is forward-compatible; this is additive and requires **no** core change (INV-8 holds).

---

## 5. INV-8 compatibility statement

Every mapping above is **additive**: 7 new `event_type` strings (accept-listed by EXT-core's 3-edit change, S6 §3) reusing the **existing** `GovernanceEventPayload`/`SpanData`/`metadata` shape and the **existing** `/evaluate` route. No field is removed or repurposed; existing Temporal event semantics are untouched. If any future lifecycle type cannot map without a non-additive wire change → **HALT** and route to architecture (story stop condition).

**Verified downstream (G3_REVIEW, live openbox-core + openbox-backend).** Once `isValidGovernanceEventType` (`api/governance.go:273`) accepts the 7 types, **no** consumer errors, drops, or crashes — each defaults to a benign path. Behaviors to know (not blockers for Phase-1 observe):

| Consumer | Behavior on the new dev types |
|---|---|
| Metadata/attestation extractor (`governance_metadata.go`) | base metadata + `payload.Metadata` hashed; safe |
| Guardrails eligibility (`governance_workflow.go:429`) | **skipped** for dev types (not Activity/Signal). Fine in Phase 1 — content is redacted at-source (INV-2), not at core ingest. |
| OPA policy eval (`opa.go:205`) | **runs** for dev types (only `Workflow*` bypass). Harmless in Phase-1 observe (verdict ignored, INV-3); useful for Phase-2. |
| AGE / AIVSS goal-drift (`age.go`) | no per-type scoring for dev types; safe |
| Observability per-type counter (`invocation.go`) | generic counters fire; no per-type counter; safe |
| Backend store/read (`GovernanceEventService`, entity `varchar(50)`) | stores + reads fine; no enum guard. All 7 names < 50 chars. |
| Backend activity-timeline UI (`run.provider.ts:563`) | pairs only `ActivityStarted/Completed`, so dev events don't appear in **that** view. Developer session logs use the generic `getSessionLogs`. UI note, not a contract issue. |
| Webhooks (`webhook.go`) | key off the **verdict**, not input `event_type`; unaffected. |

## 6. Client transport notes (for SL-3)

Verified against the SDK's `request_signing.py` — the client must match core exactly:
- **Body:** compact JSON, `separators=(",",":")` equivalent; the **signed bytes must equal the transmitted bytes** (serialize once, send raw; never re-encode).
- **AIP signature (Ed25519):** canonical string = `UPPER(METHOD)\nPATH\nTIMESTAMP\nNONCE\nBODY_SHA256_HEX`; headers `X-OpenBox-Agent-DID`, `X-OpenBox-Agent-Timestamp`, `X-OpenBox-Agent-Nonce`, `X-OpenBox-Agent-Signature`, `X-OpenBox-Body-SHA256`, plus `Authorization: Bearer <obx_>` and `X-OpenBox-SDK-Version`.
- **`source`:** this contract uses `"developer-runtime"` (vs the SDK's `"workflow-telemetry"`). `source` is free-form/unvalidated in core — additive, distinguishes developer traffic.
- **`sdk_version`:** set **server-side** from the `X-OpenBox-SDK-Version` header — do not put it in the body.
