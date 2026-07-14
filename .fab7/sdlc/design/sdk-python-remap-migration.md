# Migration design — unify shift-left telemetry onto the base SDK wire model (ADR-0004 option B)

**Companion to:** `.fab7/sdlc/design/sdk-python-remap.md` (§4) and `adr/ADR-0004-unify-dev-events-onto-base-wire-model.md`. **Status:** design (pending ADR-0004 sub-decisions #1/#2 + G_ADR). Source-cited to `openbox-sdk-python/openbox_core/`, `openbox-core/internal/content/`, and this repo.

This is the concrete "how" for re-expressing the 7 developer events onto the base SDK's `Workflow/Activity/Signal` + flat `SpanData` wire shape. It is deliberately implementation-level so the epic can be sliced without re-deriving the wire facts.

## 1. Target wire shapes (ground truth)

**Lifecycle event body** (flat top-level; `events.py:111-131`, `to_payload_dict`): `{source, event_type, ...flat identity fields, timestamp}`. Required workflow fields: `workflow_id, run_id, workflow_type` (`gate` validation raises `ContractError` / `ENVELOPE_MISSING_FIELDS` otherwise). `source` is `"workflow-telemetry"` for the base; shift-left may keep `"developer-runtime"` (free-form, unvalidated — `MAPPING.md §6`).

**Hook (tool-call) event** (`events.py:365-401`): serializes as `event_type="ActivityStarted"`, `hook_trigger=true`, `activity_id`, `activity_type`, non-empty `spans:[flat SpanData]`, `span_count=len(spans)`.

**Flat `SpanData` common root fields** (`conformance/fake_core.py:21-36`, present even when null): `span_id, trace_id, parent_span_id, name, kind, stage, start_time, end_time, duration_ns, attributes, status, events, hook_type, error`. Ids: `span_id`=16 lowercase hex, `trace_id`=32, `parent_span_id`=16-when-present. Started-stage: `end_time:null` + `duration_ns:null`. Family root fields per `hook_type` (`fake_core.py:39-66`): `file_operation` → `file_path, file_mode, file_operation, bytes_read, bytes_written`; `function_call` → `function, module, args, result`; (`http_request`, `db_query` also defined).

**Core semantic-type derivation** (authoritative, `openbox-core internal/content/session.go:204-296`): file → root `FilePath` set + `span.Name ∈ {file.read,file.write,file.open,file.delete}`; mcp → `attributes["mcp.method"]=="callTool"`; shell/unknown → `internal`. So shift-left sets the **source fields** and Core computes `semantic_type` (do not send it).

## 2. Event-by-event mapping (baseline; shell/mcp/commit/deploy per ADR sub-decisions)

| # | Dev event (`client/event.go`) | → Base wire | Fields to set |
|---|---|---|---|
| 1 | `SessionStarted` | `WorkflowStarted` | `workflow_id`=stable session/workspace id, `run_id`=`openbox_session_id`, `workflow_type`="developer-session" (or provider `claude-code`), `timestamp`. metadata: provider, tool_version, repo, cwd. |
| 2 | `SessionEnded` | `WorkflowCompleted` | same ids; metadata: total_tokens, total_cost, duration_ms. |
| 3 | `ToolCall` **file** | `ActivityStarted`+hook | `activity_id`, `activity_type`=tool name; span: `hook_type="file_operation"`, `name`="file.write"\|"file.read"\|"file.open"\|"file.delete" (from `file_operation`), root `file_path`, `file_operation`, `stage="started"`, `end_time:null`,`duration_ns:null`. |
| 4 | `ToolCall` **mcp** | `ActivityStarted`+hook | span: NEW `hook_type="mcp"` (B-core) + `attributes={"mcp.method":"callTool","mcp.tool":<fn>,"mcp.server":<srv>}`, `name`=mcp function. Core's extended classifier computes `mcp_tool_call`. |
| 5 | `ToolCall` **shell** | `ActivityStarted`+hook | span: NEW `hook_type="shell"` (B-core), `name`=tool name (`Bash`), command in gated `request_body` + attributes; Core's extended classifier gives shell a first-class semantic type (no longer `internal`). |
| 6 | `ToolResult` | `ActivityCompleted` | same `activity_id`; completed-stage span (real `end_time`, `duration_ns`, `bytes_read`/`bytes_written`, `lines_count`; `status`). Pairs with #3–5 → dashboard timeline fix. |
| 7 | `PromptSubmitted` | `SignalReceived` | `signal_name="prompt_submitted"`; metadata: tokens, cost, model. |
| 8 | `CommitCreated` | `SignalReceived` | `signal_name="commit_created"`; metadata: commit_sha, repo, branch (FR-5). |
| 9 | `Deploy` | `SignalReceived` | `signal_name="deploy"`; metadata: deploy_id, commit_sha, repo, environment, deploy_did (FR-6/7). |

**B-core hook-type additions (upstream, critical-path):** add `shell` / `mcp` / (optionally a generic `tool`) members to `openbox_core.contracts.otel_spans.HookType` (`otel_spans.py:36-43`) + their family root fields in `_ROOT_FIELDS_BY_HOOK_TYPE`/`fake_core._FAMILY_ROOT_FIELDS`, and extend Core's `ComputeSemanticTypeFromSpan` (`openbox-core internal/content/session.go:204`) to classify a `shell` span (e.g. `shell_command`) and an `mcp` span (`mcp_tool_call`) from their root/attribute fields. shift-left then emits those hook types.

## 3. Flat `SpanData` construction in Go (no OTel dependency)

shift-left does **not** need OpenTelemetry — the wire is flat `SpanData`, not OTel. Build the dict directly:
- `span_id`: 8 random bytes → 16 lowercase hex. `trace_id`: 16 random bytes → 32 hex. Per session, keep a `trace_id` stable and set `parent_span_id` to link ToolResult→ToolCall (or the activity's started span).
- `start_time`: int nanoseconds since epoch. Started stage: `end_time=nil`, `duration_ns=nil`. Completed: real `end_time`, `duration_ns=end-start`.
- `kind`: "INTERNAL" for file/shell/function; "CLIENT" for mcp/http (mirror `_DEFAULT_KIND_BY_HOOK`, `otel_spans.py:94-100`).
- Include all common root fields (null when absent) + the family root fields for the chosen `hook_type` — so shift-left passes `assert_hook_wire_shape` shape checks.
- `attributes` carries mcp.* and any non-first-class hints (Core reads `mcp.method` from here).
- This replaces the current hand-built `client/event.go Span` (`event.go:76-92`) — the new builder emits the flat Core `SpanData` directly.

## 4. Client / payload changes (`client/`)
- `client/event.go`: replace the 7-type `EventType` + `Span`/`DevEvent` with the base envelope + flat `SpanData` builders (or an internal normalized form that serializes to the base shape). `source` stays `"developer-runtime"` (or flip to `"workflow-telemetry"` — free-form).
- `client/payload.go`: build the base `to_payload_dict`-equivalent (`workflow_started`/`activity_started`+hook/`signal_received` factories in Go). Preserve compact-JSON-signed-once (`payload.go:90`). Add `span_count`.
- `client/verdict.go`: **unchanged** (response parsing is stable; §2c of the remap doc). Optionally parse `fallback_used`.
- Idempotency (`event_id`/Idempotency-Key, SL-14) and signing (`client/signing.go`) **unchanged**.

## 5. Sidecar / enforce impact (small)
The local `DecisionRequest`/`DecisionResponse` (`sidecar/protocol.go`) is **shift-left-owned and local** — it need not change for wire unification. But for consistency, the local decision axes (tool kind/file/command) already mirror what the `ActivityStarted` hook carries, so `buildDecisionRequest` (`adapters/claude-code/enforce.go:172-217`) can stay as-is. The enforce cascade (`mapVerdict`/`applyDecision`) is untouched. **Net: the enforce path is orthogonal to this migration** — this is an observe/egress-shape change.

## 6. EXT-core retirement — FULL (no residual)
Under B-core + Signal, every dev event maps to a wire type **already in Core's accept-list** (`isValidGovernanceEventType`): `WorkflowStarted/Completed`, `ActivityStarted/Completed`, `SignalReceived`. Core already creates a session row on `WorkflowStarted` and closes on `WorkflowCompleted/Failed` (`storage_session.go:41`), so session lifecycle works with stock Core (retires EXT-core edit #3). Because `CommitCreated`/`Deploy` become `SignalReceived` (sub-decision #2), there is **no residual dev-type accept-list** — the entire SL-13 3-edit EXT-core patch is retired. NOTE: the B-core work *adds* to Core's **semantic classifier** (new `shell`/`mcp` hook-type handling in `ComputeSemanticTypeFromSpan`) — a different, additive Core change from the retired accept-list patch.

## 7. Conformance alignment
- shift-left's E6-S7 suite (`enforce_conformance_test.go`, C1–C9) already covers the **behavioral** parity matrix (`instructions.md:320-332`): BLOCK-not-run, HALT, fail-open/closed, observe-never-blocks, staleness. Keep it; add a mapping table E6-S7 C1–C9 ↔ base `tests/conformance/test_required_cases.py`.
- New: a **wire-shape** conformance test asserting shift-left's emitted `ActivityStarted`+hook payload satisfies the base `assert_hook_wire_shape` invariants (common+family root fields, hex ids, started nulls, no nested `otel`/`openbox`/`data`). This is the test that only passes once §3 lands.

## 8. Phased plan (epic E7 — B-core critical path)
1. **E7-S0 (spike)** — verify against live Core (memory `local-openbox-run`): dev `ActivityStarted`/hook spans yield the intended `semantic_type` (file today; shell/mcp after the classifier extension); `Workflow*` OPA-bypass doesn't disable enforcement (enforcement is on `Activity` tool calls); flat-span attribute space holds shell/mcp; `SignalReceived` carries commit/deploy lineage keys usably. Produces the evidence the upstream contract additions need. **Blocks the rest.**
2. **E7-S1 (UPSTREAM, base-SDK) — critical path.** In `openbox-sdk-python`: add `shell`/`mcp`/`tool` members to `HookType` + their family root fields (`otel_spans.py`, `conformance/fake_core.py`); extend `assert_hook_wire_shape` to accept them. Local commit per the sibling-repo-changes-stay-local convention; push/PR only when brian asks.
3. **E7-S2 (UPSTREAM, Core) — critical path.** In `openbox-core`: extend `ComputeSemanticTypeFromSpan` (`session.go:204`) to classify `shell` (e.g. `shell_command`) and `mcp` spans first-class. Local commit; retire the SL-13 EXT-core accept-list patch (§6). Live-verify the classifier.
4. **E7-S3 (shift-left)** — Go flat-`SpanData` builder + 16/32-hex id + trace generation + started/completed staging (§3), using the new hook types; unit + wire-shape conformance test mirroring `assert_hook_wire_shape`.
5. **E7-S4 (shift-left)** — event→wire mapping for `ToolCall`/`ToolResult` (file/mcp/shell) (§2 rows 3–6); enforce path stays green (§5).
6. **E7-S5 (shift-left)** — session lifecycle `SessionStarted/Ended`→`Workflow*`; `PromptSubmitted`/`CommitCreated`/`Deploy`→`SignalReceived` (§2 rows 1,2,7,8,9).
7. **E7-S6 (shift-left)** — update `MAPPING.md` + the dev-event JSON schema to the unified shape; adopt the base conformance parity matrix; live E2E in the shared dashboard (verify the timeline pairing fix / `dashboard-devruntime-display-gaps`).

**Critical path:** E7-S0 → E7-S1 (base-SDK) → E7-S2 (Core) must land before E7-S3+ (shift-left mirrors the extended contract). E7-S1 and E7-S2 are cross-repo; they gate the shift-left work.

## 9. Open questions carried to the spike (E7-S0)
- Does Core's `Workflow*` OPA-bypass (`opa.go:205`) matter for a developer session, given enforcement is on `ToolCall`=`Activity`? (expected: no — but verify.)
- Does `assert_hook_wire_shape` reject a null/unknown `hook_type` for shell/mcp, or only enforce family fields when `hook_type` matches a family? (determines whether B-shiftleft can pass wire conformance without B-core hook types.)
- Content-capture (INV-2): does routing tool content through the gated span `request_body` on an `ActivityStarted` change Core's guardrails-eligibility in a way that affects the content gate? (`MAPPING.md §5` line 95 says Activity is guardrails-eligible — confirm the gate still holds.)
