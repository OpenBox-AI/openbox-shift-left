# Mapping — normalized dev event → base-SDK unified wire model (openbox-core `/evaluate`)

**Contract:** [`schema/dev-event.schema.json`](schema/dev-event.schema.json) v1.0 (the tool-agnostic **adapter-facing** shape; unchanged).
**Wire model (as built, E7):** the **base SDK's** `EventType` set — `WorkflowStarted / WorkflowCompleted / SignalReceived / ActivityStarted`+`hook_trigger` — serialized by `client/payload.go` onto `POST /api/v1/governance/evaluate` (openbox-core). Ratified in [`ADR-0004`](../../.fab7/sdlc/design/adr/ADR-0004-unify-dev-events-onto-base-wire-model.md); built by **E7-S3** (span builder), **E7-S4** (tool→hook), **E7-S5** (lifecycle→Workflow*/Signal).

> **What changed vs SL-1 (read this first).** SL-1 kept a *parallel* developer vocabulary (`SessionStarted`/`ToolCall`/…) and passed each `event_type` **verbatim** to core, which required patching core's accept-list (SL-13 EXT-core). **ADR-0004 reversed that:** shift-left now re-expresses the *same* normalized dev events onto the base SDK's blessed wire types, so (a) telemetry conforms to `assert_hook_wire_shape` (mirrored as `client.AssertHookWireShape`, E7-S1), (b) the EXT-core accept-list is **retired** (E7-S2), and (c) dev tool calls pair on the shared dashboard timeline. The normalized contract (this schema) is **unchanged** — adapters still emit `ToolCall`/`SessionStarted`/…; only the **client→core wire serialization** moved.

**Two layers.** The *adapter-facing* contract ([schema](schema/dev-event.schema.json)) is what a provider adapter produces via SPI `emit()` — its `event_type` enum is still the 7 dev-runtime lifecycle names. The *wire* layer below is what the shared `client/` translates that into. Adding a provider never touches either layer (PRD FR-4, architecture §1b).

---

## 1. Envelope field mapping (every event)

Source-cited to `client/payload.go` (`governanceEventPayload`, the marshaled body) and the base contract `openbox-sdk-python/openbox_core/contracts/events.py` (READ-ONLY reference).

| Normalized dev event | → wire `governanceEventPayload` field | Notes |
|---|---|---|
| — (constant) | `source` = `"developer-runtime"` | Free-form in core; distinguishes dev traffic from the SDK's `"workflow-telemetry"`. |
| `event_type` | `event_type` | **Re-mapped**, not passed through — see §2. Resolves to a base wire type (`WorkflowStarted`/`WorkflowCompleted`/`SignalReceived`/`ActivityStarted`). |
| `openbox_session_id` | `run_id` | Session keyed by `(workflow_id, run_id, workflow_type)`. |
| `developer_did` (or workspace/repo id) | `workflow_id` | Stable per-workspace identity so `(workflow_id, run_id)` is unique per session. |
| — (constant) | `workflow_type` = `"developer-session"` | **Required** by the base contract on **both** `Workflow*` **and** `SignalReceived` events (`event_rules.py` `_REQUIRED_WORKFLOW_FIELDS`; core reads it into a dedicated column, `storage_event.go`). The *constant* value keeps a session's `WorkflowStarted` + its `SignalReceived`s + `WorkflowCompleted` on one `(workflow_id, run_id, workflow_type)` identity so core resolves them to **one** session row. Omitted (`omitempty`) on the `ActivityStarted` hook path (which builds its own envelope). |
| — (per signal) | `signal_name` | Set **only** on `SignalReceived` (`prompt_submitted`/`commit_created`/`deploy`); required there (`event_rules.py` raises `ENVELOPE_MISSING_FIELDS` otherwise). |
| `timestamp` | `timestamp` | Core field is a **string** (RFC3339) — pass through verbatim. |
| `span` (tool events only) | `spans[0]` + `span_count` = len(`spans`) | Flat base `SpanData`; see §3. Lifecycle/signal events are **span-less** (the base contract rejects span-bearing non-hook events). |
| `metadata` | `metadata` (`json.RawMessage`) | Merged per-type keys below; JSON object. Carries commit/deploy lineage (§2). |
| `tokens`, `cost` | `metadata.tokens`, `metadata.cost` | No first-class payload fields; carried in `metadata` (finops, SL-16). |
| `developer_did` | — | Identity is via the signed AIP headers + Bearer key, **not** a body field. `from_agent_did`/`multi_agent_session_id` stay empty (Handoff-only). |
| `content.*`, `span.request_body/response_body` | `spans[].request_body` / `response_body` **only when content-capture enabled**, **capped to 65536 chars** before egress (`capBody`, G_SEC SEC-1) | Stripped at the client when disabled (INV-2). Never a first-class payload field, never on the local enforce `DecisionRequest`. |

`schema_version` and `event_id` are contract/idempotency fields — `event_id` is the client's idempotency key (INV-5), used client-side for dedupe; neither is a core payload field.

---

## 2. Per-type mapping (dev event → base wire event)

Built by `lifecycleWireType` (lifecycle/signal, `client/payload.go`) and `buildPayload`'s tool dispatch (E7-S4).

| Dev `event_type` | Base wire `event_type` | `signal_name` | Span | Key `metadata` | Core effect |
|---|---|---|---|---|---|
| `SessionStarted` | `WorkflowStarted` | — | — | `provider`, `tool_version`, `repo`, `cwd` | **create** session `(workflow_id, run_id, workflow_type)` (`storage_session.go`) |
| `SessionEnded` | `WorkflowCompleted` | — | — | `total_tokens`, `total_cost`, `duration_ms` | **terminal** — closes the session |
| `PromptSubmitted` | `SignalReceived` | `prompt_submitted` | — | `tokens`, `cost`, `model` | mid-session signal |
| `CommitCreated` | `SignalReceived` | `commit_created` | — | `commit_sha`, `repo`, `branch` (FR-5) | mid-session signal; commit lineage |
| `Deploy` | `SignalReceived` | `deploy` | — | `deploy_id`, `commit_sha`, `repo`, `environment`, `deploy_did` (FR-6/7) | signal; deploy lineage |
| `ToolCall` | `ActivityStarted`+`hook_trigger` | — | stage=`started` | `tool_name` | pre-exec decision (OPA runs; the enforce point) |
| `ToolResult` | `ActivityStarted`+`hook_trigger` | — | stage=`completed` | `tool_name`, `exit_code`? | **same** hook envelope; carries `bytes_*`/`lines_count` |

### The `ToolCall`/`ToolResult` pair — key correction (E7-S4)

Both stages serialize as **`event_type = ActivityStarted`** with `hook_trigger = true`. A `ToolResult` is **not** `ActivityCompleted`:

- The base SDK's `wire_event_type()` forces `ActivityStarted` for **any** `hook_trigger` event, regardless of stage (`events.py` `hook(...)` → `EventKind.HOOK` serializes as `ActivityStarted`).
- `ActivityCompleted` is a **hook-less lifecycle** type that must **not** carry spans (`assert_hook_wire_shape` asserts `ActivityStarted` unconditionally and accepts span `stage ∈ {started, completed}`). Emitting `ActivityCompleted` would **fail our own** `client.AssertHookWireShape`.
- The two stages are **paired by a shared `span_id` (+ deterministic `activity_id`, `trace_id`)**, not by parent linkage. `client/spanbuilder.go` derives these from `session/tool/locator` with **no** stage or timestamp input, so the two separate hook processes (and the SL-4 spool) mint the **same** ids without threading state. Core/the dashboard pair them onto one timeline row.

> **Supersedes** ADR-0004's baseline table (row `ToolResult → ActivityCompleted`) and Consequences bullet: the accurate, self-consistent mechanism is *both stages = `ActivityStarted`+hook, paired by span_id*. See E7-S4 result artifact for the cross-repo derivation. The dashboard-pairing behavior this produces is re-verified live in §7.

### `semantic_type` is computed server-side

Core derives `SpanData.semantic_type` from the span's **source** fields via `ComputeSemanticTypeFromSpan` (`internal/content/session.go`). The client sets the source fields + `hook_type`; it does **not** send `semantic_type` (the wire assertion forbids it).

| `tool.kind` | Span shape the client sets (`hookSpanShape`, E7-S4) | Core-computed `semantic_type` |
|---|---|---|
| `file` | `hook_type=file_operation`; name `file.read`/`file.write`/`file.open`/`file.delete`; root `file_path`, `file_operation`, `bytes_*` | `file_read`/`file_write`/`file_open`/`file_delete` (`session.go` file classifier) |
| `mcp` | `hook_type=mcp` (kind=CLIENT); `attributes["mcp.method"]="callTool"` + `mcp_server`/`mcp_tool`; `mcp_*` family roots | `mcp_tool_call` — **first-class via E7-S2** (`session.go` mcp classifier) |
| `shell` | `hook_type=shell`; `shell_command` present-but-**null** on egress (INV-2: read only for LOCAL enforce) | `shell_command` — **first-class via E7-S2** (was `internal` fallback pre-E7-S2) |

> **E7-S2 dependency (server-side, pending).** Making `shell`→`shell_command` and `mcp`→`mcp_tool_call` first-class is the openbox-core classifier edit **E7-S2** (extend `ComputeSemanticTypeFromSpan`; retire the SL-13 EXT-core accept-list). Until E7-S2 lands, `shell` resolves to core's `internal` fallback and `mcp` to `mcp_tool_call` (already recognized pre-E7-S2 per `session.go` mcp attribute path). The wire shape does not depend on E7-S2 — E7-S0 confirmed stock core accepts every base type at HTTP 200.

---

## 3. Building a flat `SpanData` (tool events)

Built by `client/spanbuilder.go` (`BuildHookSpan`), the no-OTel Go port of the base SDK's `from_otel_span`. Every span is a **flat** dict (no nested `otel`/`openbox` envelope, no `data` blob, no `semantic_type`) with all 14 common root fields present + the family tuple:

| Field group | Fields | Notes |
|---|---|---|
| Common roots (always present) | `span_id`, `trace_id`, `parent_span_id`, `name`, `kind`, `stage`, `start_time`, `end_time`, `duration_ns`, `attributes`, `status`, `events`, `hook_type`, `error` | `span_id` = 16-hex, `trace_id` = 32-hex (regex-checked by the assertion). `stage="started"` ⇒ `end_time=null`, `duration_ns=null`. |
| `file_operation` family | `file_path`, `file_mode`, `file_operation`, `bytes_read`, `bytes_written` | Present-but-null when not applicable. |
| `mcp` family | `mcp_server`, `mcp_tool`, `mcp_method` | Inert until E7-S2 recognizes them server-side. |
| `shell` family | `shell_command`, `shell_exit_code` | `shell_command` **never egressed** (INV-2). |
| `tool` family | `tool_name` | |
| Gated content | `request_body`, `response_body` | INV-2 gated + `capBody`-truncated (§1). |

The started+completed pair reuse the **same** `span_id` (base pairing mechanism; `HookSpan.SpanID` reuse). `trace_id` derives from the authenticated `SessionID`. See `client/hookspan.go` for the mirrored contract and `AssertHookWireShape` for the conformance assertion.

---

## 4. Verdict (parsing the response)

The `/evaluate` response is core's `EvaluationResult` — `verdict` + legacy `action` + `fallback_used` (confirmed live, E7-S0 spike). The canonical enum is `HALT > BLOCK > REQUIRE_APPROVAL > CONSTRAIN > ALLOW`; the wire is **lowercase**:

| Canonical | Wire `verdict` | Legacy `action` |
|---|---|---|
| `ALLOW` | `allow` | `continue` |
| `CONSTRAIN` | `constrain` | `continue` |
| `REQUIRE_APPROVAL` | `require_approval` | `require-approval` |
| `BLOCK` | `block` | `stop` |
| `HALT` | `halt` | `stop` |

`fallback_used=true` marks a fail-open verdict (core's OPA/Guardrail unreachable → default ALLOW). Phase-1 **observe** (D7/INV-3) treats every async-egress verdict as advisory and never blocks the tool call. Enforcement (Phase-2, E6) is a **separate, local** decision on the sidecar `DecisionRequest` — it does **not** read this response (INV-3b; enforce path untouched by the E7 wire reshape).

---

## 5. INV-8 / conformance statement

The wire model is now the base SDK's **stock** vocabulary — no dev-specific `event_type` strings, so **no core accept-list patch is needed** (E7-S0: all base types → HTTP 200 on stock core). This is a **net simplification** vs SL-1's EXT-core patch, which E7-S2 retires.

**Downstream-consumer behavior on the unified shape** (verified E7-S0 live + cross-repo Explore):

| Consumer | Behavior |
|---|---|
| Session store (`storage_session.go`) | `WorkflowStarted`→create, `WorkflowCompleted`→terminal — **native**, no EXT-core lifecycle edit. |
| OPA policy eval (`opa.go`) | Bypassed (auto-allow) **only** for `Workflow*` (latency); `ActivityStarted` (the tool call = enforce point) **and** `SignalReceived` go through **real** OPA. Session=workflow does **not** weaken enforcement (E7-S0 #2). |
| Guardrails eligibility | `ActivityStarted` is guardrails-**eligible** at core (unlike the old dev types) — useful for content-capture redaction (E6-S4). Metadata-only by default (INV-2). |
| `signal_name` / `workflow_type` | Stored in dedicated columns (`storage_event.go`); commit/deploy lineage rides `metadata` (core has no `commit_sha`/`deploy_id` columns) and **survives** the Signal mapping. |
| Dashboard activity timeline (`run.provider.ts`) | Pairs `ActivityStarted`/`Completed`. Dev tool calls now emit `ActivityStarted`+hook (both stages) → expected to appear (vs SL-1's `Unknown`). **Re-verified live in §7.** |

If any future lifecycle type cannot map without a non-additive wire change → **HALT** and route to architecture.

---

## 6. Client transport notes

Verified against the SDK's `request_signing.py` — the client matches core exactly:
- **Body:** compact JSON; the **signed bytes must equal the transmitted bytes** (serialize once, send raw). `capBody` truncation happens **before** marshal = before signing.
- **AIP signature (Ed25519):** canonical string `UPPER(METHOD)\nPATH\nTIMESTAMP\nNONCE\nBODY_SHA256_HEX`; headers `X-OpenBox-Agent-DID/Timestamp/Nonce/Signature`, `X-OpenBox-Body-SHA256`, `Authorization: Bearer <obx_>`, `X-OpenBox-SDK-Version`.
- **`sdk_version`:** set server-side from the header — not in the body.

---

## 7. Live E2E verification (dashboard pairing + INV re-check)

The unified shape's user-visible payoff is the dashboard fix. This section records the live E2E: boot the shared stack (RUNBOOK Path A: infra + Temporal + openbox-core + workers + openbox-fe dashboard), run a real Claude Code session (`SessionStarted` → file/shell/mcp `ToolCall`+`ToolResult` → `PromptSubmitted` → `SessionEnded`), and confirm on the dashboard timeline:

1. **Pairing:** a tool call renders as one paired started/completed row (not `Unknown`), fixing `dashboard-devruntime-display-gaps`. **Open risk to confirm:** the dashboard pairs by `event_type` — our shape emits **two `ActivityStarted`** events (paired by `span_id`), not `ActivityStarted`+`ActivityCompleted`; verify the timeline pairs on `span_id`/`activity_id` and not on the `Started`/`Completed` type suffix. If it pairs only by type suffix, that is a dashboard-side follow-up, not a wire defect.
2. **Semantic type:** file spans classify (`file_*`); shell/mcp classify first-class **once E7-S2 is live** (else shell→`internal`).
3. **INV re-check on the wire shape:** INV-2 (0 bodies stored, metadata-only), INV-1 (no secrets), INV-3/3b (enforce local, observe never blocks), INV-8 (stock core, no accept-list).

_Status: see the E7-S6 ledger entry / result artifact for the run evidence._
