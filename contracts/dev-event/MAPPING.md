# Mapping — normalized dev event → base-SDK unified wire model (openbox-core `/evaluate`)

**Contract:** [`schema/dev-event.schema.json`](schema/dev-event.schema.json) **v1.1** (the tool-agnostic **adapter-facing** shape).
**Wire model:** the **base SDK's** `EventType` set — `WorkflowStarted / WorkflowCompleted / SignalReceived / ActivityStarted / ActivityCompleted` — serialized by `client/payload.go` (`buildPayload`) onto `POST /api/v1/governance/evaluate` (openbox-core). Every payload is span-less and hook-less.

> **What changed, read this first.** Two reshapes brought the wire here.
>
> **ADR-0004** retired SL-1's parallel developer vocabulary (`SessionStarted`/`ToolCall`/… passed verbatim, requiring a core accept-list patch) and re-expressed the same normalized dev events onto the base SDK's blessed wire types.
>
> **[ADR-0013](../../docs/adr/ADR-0013-tool-call-as-activity.md)** then moved tool calls off the hook-span envelope: `ToolCall` → `ActivityStarted`, `ToolResult` → `ActivityCompleted`, both span-less. A hook process has no in-process OTel, so the span shift-left used to send was fabricated by hand to satisfy a shape rather than to record a measurement. Retiring it also dissolved ADR-0004's standing obligation to hand-maintain a Go mirror of the base hook contract. **Cost, stated plainly:** dev sessions produce zero `spans` rows, so there are no span-level Merkle leaves and no server-side `semantic_type` for them.
>
> The normalized contract (this schema) was **unchanged** through both — adapters still emit `ToolCall`/`SessionStarted`/…; only the **client→core wire serialization** moved.
>
> **[ADR-0014](../../docs/adr/ADR-0014-turn-as-activity-and-identifier-allowlist.md)** is the first change that DID touch this contract, which is why it is v1.1: it adds the model-turn pair (`TurnStarted`/`TurnCompleted`, riding the same two activity wire types with `activity_type: llm_completion`) and the fields it needs (`model`, `turn_index`, `agent_id`), and it **re-defines `tokens.input` as pure input** — v1.0's Claude Code rollup folded both cache counts into it. Everything else is additive; that one semantic is what makes it a version bump rather than a silent edit. **Cost, stated plainly:** the transcript projection's INV-2 guarantee is now a curated allowlist enforced by a test, not a structural impossibility, because the model id is a bound string.

**Two layers.** The *adapter-facing* contract ([schema](schema/dev-event.schema.json)) is what a provider adapter produces via SPI `emit()` — its `event_type` enum is the 9 dev-runtime lifecycle names. The *wire* layer below is what the shared `client/` translates that into. Adding a provider never touches either layer (PRD FR-4, architecture §1b). That the span retirement required **zero** edits under `contracts/dev-event/` is the split working as designed — and that the turn pair DID require edits here is the same split saying, correctly, that this one is a contract change.

---

## 1. Envelope field mapping (every event)

Source-cited to `client/payload.go` (`governanceEventPayload`, the marshaled body) and the base contract `openbox-sdk-python/openbox_core/contracts/events.py` (READ-ONLY reference).

| Normalized dev event | → wire `governanceEventPayload` field | Notes |
|---|---|---|
| — (constant) | `source` = `"developer-runtime"` | Free-form in core; distinguishes dev traffic from the SDK's `"workflow-telemetry"`. |
| `event_type` | `event_type` | **Re-mapped**, not passed through — see §2. Resolves to one of the five base wire types. |
| `openbox_session_id` | `run_id` | Session keyed by `(workflow_id, run_id, workflow_type)`. |
| `developer_did` (or workspace/repo id) | `workflow_id` | Stable per-workspace identity so `(workflow_id, run_id)` is unique per session. One derivation, `workflowIDFor` — shared with `ApprovalKeyFor` (§2). |
| — (constant) | `workflow_type` = `"developer-session"` | **Required** by the base contract on `Workflow*` and `SignalReceived` events (`event_rules.py` `_REQUIRED_WORKFLOW_FIELDS`; core reads it into a dedicated column, `storage_event.go`). The *constant* value keeps a session's whole tree on one `(workflow_id, run_id, workflow_type)` identity so core resolves it to **one** session row. Now present on **tool events too** — the old hook envelope omitted it, diverging from the base SDK's `ActivityContext.to_payload_fields()`; routing tool events through the same struct fixed that at no cost. |
| — (per signal) | `signal_name` | Set **only** on `SignalReceived` (`prompt_submitted`/`commit_created`/`deploy`); required there (`event_rules.py` raises `ENVELOPE_MISSING_FIELDS` otherwise). |
| — (activity events) | `activity_id` | Set on **both** halves of a tool call and of a turn. Pairs them onto one row; for a tool call it is additionally the approval key — see §2 "Operation vs invocation identity" and "The turn pair". |
| `tool.name` | `activity_type` | The dashboard's "Activity" column. Lifecycle events carry their `event_type` string instead, so the column is never empty. |
| `tool.*`, `span.*` (started) | `activity_input` | Structural locators only; see §3. Core stores it as the row's `input` and runs Guardrails **stage 0** over it (`internal/services/guardrail.go:180`). |
| `span.*` (completed) | `activity_output` | Counts and an exit code only; see §3. Core stores it as the row's `output` and runs Guardrails **stage 1** over it (`guardrail.go:192`). |
| `started_at` → `ended_at` | `duration_ms` | **Client-computed**, in float milliseconds. Core used to derive the row's duration from the stored span; with no span, the client is the only thing that can. Core copies it onto the row verbatim (`storage_event.go:292-294`) and the dashboard reads `event.duration_ms` directly. **Omitted, never zero**, when unknown — see §3. |
| `timestamp` | `timestamp` | Core field is a **string** (RFC3339) — pass through verbatim. |
| `metadata` | `metadata` (`json.RawMessage`) | Merged per-type keys below; JSON object. Carries commit/deploy lineage (§2). |
| `tokens`, `cost`, `model` | `metadata.tokens`, `metadata.cost`, `metadata.model` | No first-class payload fields; carried in `metadata`. On a turn's `ActivityCompleted` the same model + counts ALSO ride `activity_output`, so they are policy-visible — see §2 "The turn pair". |
| `developer_did` | — | Identity is via the signed AIP headers + Bearer key, **not** a body field. `from_agent_did`/`multi_agent_session_id` stay empty (Handoff-only). |
| `span` | — | **No longer serialized.** No payload carries `spans`, `span_count` or `hook_trigger` (ADR-0013). The struct survives in the frozen adapter contract as the carrier the client reads locators and counts *from* — see §3. |
| `content.prompt` | `signal_args.prompt` **only when content-capture enabled**, capped to 65536 chars (`capBody`) | Stripped at the client when disabled (INV-2). |
| `content.tool_input` | `activity_input.command` / `.arguments` **only when content-capture enabled**, capped | Tier-2 **escalation only**, never the observe path (OD-E9-7). |
| `span.request_body/response_body` | — | **No longer an egress channel.** They rode the span; with no span the serializer does not read them at all, so they cannot egress even with capture on. This removed nothing that worked: no adapter has ever populated either, and both mappers assert they stay empty (`adapters/claude-code/mapper_test.go:169`, `adapters/codex/mapper_test.go:207`). |

`schema_version` and `event_id` are contract/idempotency fields — `event_id` is the client's idempotency key (INV-5), used client-side for dedupe; neither is a core payload field.

---

## 2. Per-type mapping (dev event → base wire event)

Built by `wireTypeFor` in `client/payload.go` — one table for every event type, feeding one serializer.

| Dev `event_type` | Base wire `event_type` | `signal_name` | Activity fields | Key `metadata` | Core effect |
|---|---|---|---|---|---|
| `SessionStarted` | `WorkflowStarted` | — | — | `provider`, `tool_version`, `repo`, `cwd` | **create** session `(workflow_id, run_id, workflow_type)` (`storage_session.go`) |
| `SessionEnded` | `WorkflowCompleted` | — | — | `total_tokens`, `total_cost`, `duration_ms` | **terminal** — closes the session |
| `PromptSubmitted` | `SignalReceived` | `prompt_submitted` | — | `tokens`, `cost`, `model` | mid-session signal |
| `CommitCreated` | `SignalReceived` | `commit_created` | — | `commit_sha`, `repo`, `branch` (FR-5) | mid-session signal; commit lineage |
| `Deploy` | `SignalReceived` | `deploy` | — | `deploy_id`, `commit_sha`, `repo`, `environment`, `deploy_did` (FR-6/7) | signal; deploy lineage |
| `ToolCall` | `ActivityStarted` | — | `activity_id`, `activity_type`, `activity_input` | `tool_name`, `tool_use_id`?, `agent_id`?, `agent_type`? | one `governance_events` row; pre-exec decision (OPA + Guardrails stage 0) |
| `ToolResult` | `ActivityCompleted` | — | `activity_id`, `activity_type`, `activity_output`, `duration_ms` | `tool_name`, `exit_code`?, `tool_use_id`?, `agent_id`?, `agent_type`? | its **own** row, sharing the `activity_id`; independently evaluated (OPA + Guardrails stage 1) |
| `TurnStarted` | `ActivityStarted` | — | `activity_id`, `activity_type` (`llm_completion`) | `turn_index`, `agent_id`?, `agent_type`? | one row opening the turn; no `activity_input` (a turn's input is the prompt, which rides the `prompt_submitted` signal under the content gate) |
| `TurnCompleted` | `ActivityCompleted` | — | `activity_id`, `activity_type` (`llm_completion`), `activity_output`, `duration_ms` | `tokens`, `model`, `turn_index`, `agent_id`?, `agent_type`? | its **own** row, sharing the `activity_id`; carries the turn's model + four token counts |

Two POSTs per tool call, as before — the count did not change, only the shape.
Two more per model turn, when usage capture is on.

### Correlation metadata keys (E8-S3/S4)

`metadata` is deliberately a free-form object, so these are **well-known keys rather than schema
fields** — no `schema_version` bump, because the normalized shape is unchanged and the version
`const` marks breaking changes only. All are structural identifiers (INV-2 permits them; INV-1
still forbids secrets) and all are optional — a provider that does not expose one simply omits it.

| Key | Providers | Meaning |
|---|---|---|
| `tool_use_id` | Claude Code, Codex | Per-invocation id for a `ToolCall`/`ToolResult` pair. It rides `span.invocation_id`, a *local* field (spooled, never emitted) that keys the cross-process duration stash. The wire pairing itself is `activity_id`. `span.function` is the MCP function name only. |

### Operation vs invocation identity

A tool call has two identities, and the normalized event keeps them apart:

| Field | Means | Derives | Stable across a retry? |
|---|---|---|---|
| `span.invocation_id` | THIS attempt (`tool_use_id`) | the duration-stash key, and `event_id`'s per-call distinguisher | No — by design |
| `span.operation_id` | WHAT is being done | `activity_id` | **Yes — load-bearing** |

`activity_id` is the approval key (`POST /governance/approval`) and the scope of
both of core's bypass grants, so it must survive a retry or an approved request
can never be consumed. Both fields are local: they exist to feed those opaque
hashes and are never emitted as span fields on the core wire payload.

`operation_id` is per class, matching what core's own
`ComputeApprovalFingerprint` keys on: **shell** hashes the command (approving
`ls` must not grant `rm -rf /`), **MCP** hashes the canonicalized argument shape
beside the real function name (core: "same tool with different arguments must
require fresh approval"), and any other class falls back to the invocation —
those expose no structural discriminator, are never escalated, and so can never
hold an approval. The hashes are correlation ids folded into an already-opaque
id, never content fields (INV-2).

> Conflating the two shipped and was found in a live session: every retry became
> a different activity, so an approver's decision could not be consumed, each
> retry filed a fresh request, and the rewake's "re-run to proceed" looped.
| `agent_id`, `agent_type` | Claude Code | Identify the subagent an event occurred inside. Present on *every* payload fired within a subagent, so the subagent tree is reconstructable from tool events alone — which is why the `SubagentStart`/`SubagentStop` boundary markers need no lifecycle type of their own (COVERAGE.md §3.2). |
| `turn_id` | Codex | Per-turn correlation id. |
| `thread_id`, `root_session_id` | Codex | Emitted only when a forked thread's id differs from the session id it continues (E8-S4). |

### Why a `ToolResult` is an `ActivityCompleted` (ADR-0013)

This reverses what E7-S4 concluded, so the reasoning is worth stating rather than
just the table row.

E7-S4 was right about the base SDK: `wire_event_type()` forces `ActivityStarted`
for **any** `hook_trigger` event regardless of stage, and `assert_hook_wire_shape`
asserts `ActivityStarted` unconditionally. Emitting `ActivityCompleted` *with a
hook envelope* would violate that contract.

What changed is the premise, not the rule. **That rule binds hook events, and
shift-left no longer emits any.** The base SDK's hook path exists for runtimes
with in-process OpenTelemetry, where a hook fires mid-activity and has a real
span to attach. A Claude Code or Codex hook is a short-lived separate process
with no OTel at all: the span was hand-fabricated by `client/spanbuilder.go` to
satisfy a shape. Once you stop fabricating it, the tool call is not a hook on an
activity — it *is* the activity, and it takes the ordinary hook-less lifecycle
types.

The two halves pair on **`activity_id` alone** now (there is no `span_id`). It is
derived from `session/tool/locator/operation` with no stage, timestamp or
attempt input, so the two separate hook processes — and a rehydrated spool flush
— mint the same id without threading any state.

> **Supersedes** ADR-0004's `ToolCall`/`ToolResult` rows and E7-S4's correction
> above. ADR-0004 remains the true record of why the first answer was chosen;
> [ADR-0013](../../docs/adr/ADR-0013-tool-call-as-activity.md) carries the change
> in premise and the full trade-off.

### The turn pair: `llm_completion` as an `activity_type` (ADR-0014)

A model turn is the unit a coding agent spends tokens in, and it rides the same
activity carrier a tool call does — `ActivityStarted`/`ActivityCompleted`, both
already accept-listed, so INV-8 needs nothing new.

The shape the AI-Agent runtime uses for this signal is an `llm_completion`
**span** whose `response_body` is `{model, usage{…}}`. Dev sessions write no
spans (ADR-0013), and core's span-based usage aggregation is gated on
`detectLLMProvider` reading `span.GetHTTPURL()` — which a hook process has none
of. So the turn takes the activity shape and mirrors the span's `response_body`
inside `activity_output`:

```json
{"model": "claude-opus-4-8",
 "usage": {"input_tokens": 1204, "output_tokens": 318,
           "cache_creation_input_tokens": 4096, "cache_read_input_tokens": 58210}}
```

Four numbers and one bounded identifier — nothing else may enter this object.
Core runs Guardrails stage 1 and OPA over `activity_output`, so token spend is
policy-visible (intended), and the schema staying numbers-plus-one-identifier is
what keeps that safe.

`activity_type` is the literal `llm_completion` on **both** halves. The name is
core's own (`internal/content/session.go:105` `SemanticTypeLLMCompletion`), so one
vocabulary spans both runtimes and the core-side extractor keys on a name core
already knows.

**`activity_id` is `<session_id>:turn:<index>`**, or
`<session_id>:agent:<agent_id>:turn:<index>` for a subagent's turn. Not a hash,
unlike the tool-call id: a turn has no operation to key on, is never approved (so
the id is not an approval key), and a readable id is worth having in stored rows.
It cannot collide with `cc-act-<32 hex>` by construction, and
`client/turn_key_pin_test.go` pins the bytes and the separation.

**Both halves are emitted from one hook firing** (Claude Code's `Stop`), so the
pair is atomic — no orphan half, no cross-hook turn index to race, and queued
prompts fold into one turn. The Started half's timestamp is the locally-parsed
turn-open time, used only to compute `duration_ms`; it is consumer-invisible
either way, since core derives duration from `duration_ms` on the Completed half
alone. The timestamp *string* never reaches the wire.

Codex reaches the same signal at session granularity: one pair at SessionEnd with
`activity_id <session_id>:usage:rollup`. Its `Stop` hook exists but is
deliberately unwired — scope, not impossibility.

> [ADR-0014](../../docs/adr/ADR-0014-turn-as-activity-and-identifier-allowlist.md)
> carries the full trade-off, including the one that matters: the transcript
> projection's INV-2 guarantee is now a **curated allowlist enforced by a test**
> rather than a structural impossibility, because `message.model` is bound.

### `semantic_type`: none, for dev sessions

Core derives `SpanData.semantic_type` from a span's source fields
(`ComputeSemanticTypeFromSpan`, `internal/content/session.go:204`). **Dev sessions
send no spans, so no `semantic_type` is computed for them.** This is a real loss
and is recorded as such in ADR-0013's consequences — `tool.kind` and the
`activity_input` locators are what a consumer classifies on instead.

MAPPING.md previously carried an "E7-S2 dependency (server-side, pending)" claim
that `shell`→`shell_command` and `mcp`→`mcp_tool_call` classification was
awaiting a core edit. **That claim is deleted, not restated.** It was already
contradicted by observed data (a live span carried `semantic_type:
"shell_command"` while the openbox-core checkout defines `mcp_tool_call`
(`session.go:111`) and no `shell_command` constant), it named no owner, and it is
now moot: with no span there is nothing to classify. An unowned claim in a
governance product is worse than an acknowledged gap.

---

## 3. Field homes — where every `DevEvent.Span` field goes

**This table is the authority on what the serializer reads.** The adapter-facing
`span` object is frozen at schema v1.0 and adapters still populate it; what
changed is that `client/payload.go` now reads locators and counts *out* of it
into `activity_input`/`activity_output` instead of serializing it as a span.

| `DevEvent` field | Wire home | Read on | Notes |
|---|---|---|---|
| `tool.name` | `activity_input.tool_name`, `activity_type`, `metadata.tool_name` | started (+`activity_type` on both) | |
| `tool.kind` | `activity_input.kind` | started | `shell`/`file`/`mcp` — the classification that replaces `semantic_type` |
| `tool.mcp_server` | `activity_input.mcp_server` | started | falls back from `span.mcp_server` |
| `span.file_path` | `activity_input.file_path` | started | |
| `span.file_operation` | `activity_input.file_operation` | started | |
| `span.mcp_server` | `activity_input.mcp_server` | started | mcp kind only |
| `span.function` | `activity_input.mcp_tool` | started | mcp kind only; for other kinds it is a local pairing input, never wire data |
| `span.bytes_read` | `activity_output.bytes_read` | completed | |
| `span.bytes_written` | `activity_output.bytes_written` | completed | |
| `span.lines_count` | `activity_output.lines_count` | completed | |
| `metadata.exit_code` | `activity_output.exit_code` | completed | promoted from the free-form blob; **no adapter supplies one today**, so it is absent in practice. Kept because the promotion is live the moment one does |
| `started_at`, `ended_at` | `duration_ms` | completed | float ms; **omitted, not zero**, when the stash missed, a timestamp does not parse, or the result is not positive. Zero would claim the call took no time |
| `content.tool_input` | `activity_input.command` / `.arguments` | started | Tier-2 escalation only, content-gated, `capBody`-capped |
| `span.invocation_id` | — (local) | — | feeds the duration-stash key; never a wire field |
| `span.operation_id` | — (local) | — | feeds `activity_id`; never a wire field |
| `span.semantic_type` | — | — | the client has never sent it; core computed it from the span, and there is no span |
| `span.stage` | — | — | **retained, read by nothing.** Kept deliberately: the adapter contract is frozen, adapters still set it, and a future span-bearing shape would need it back without an adapter change |
| `span.module` | — | — | never had a wire home |
| `span.request_body`, `span.response_body` | — | — | **dropped as an egress channel** (§1). Measured, not assumed: no adapter has ever set either |

Retired with the span layer: `span_id`, `trace_id`, `parent_span_id`, `kind`,
`hook_type`, `duration_ns`, `attributes`, `status`, `events`, the family root
tuples (`file_mode`, `shell_command`, `shell_exit_code`, `mcp_method`, …) and the
16-/32-hex id derivations. `client/hookspan.go` and `client/spanbuilder.go` —
along with `AssertHookWireShape`, the hand-maintained mirror ADR-0004 flagged as
a standing unverifiable obligation — are deleted.

The golden fixtures in `client/testdata/golden/activity_*.json` pin this table
byte-exactly, one per tool kind per stage. If a fixture carries a field this
table does not list, one of the two is wrong.

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
| Session store (`storage_session.go`) | `WorkflowStarted`→create, `WorkflowCompleted`→terminal — **native**, no EXT-core lifecycle edit. Unchanged by ADR-0013. |
| Accept-list (`internal/api/governance.go:273-286`) | All five types we emit are accept-listed, `ActivityCompleted` included — no core patch. |
| Idempotency / dedupe (`activities/governance/validation.go:96`) | Keyed on `(agent_id, workflow_id, run_id, activity_id, event_type)`. Because `event_type` is in the key, a tool call's two halves are now **distinct** events. Under the hook shape they matched on all five — same `activity_id`, both `ActivityStarted` — so the `ToolResult` POST hit the existing-event branch (`governance_workflow.go:228-231`) and was substantially a no-op. A **retry** of the same half still dedupes correctly, which is the behavior you want. |
| OPA policy eval (`opa.go`) | Bypassed (auto-allow) **only** for `Workflow*` (latency). `ActivityStarted`, **`ActivityCompleted`** and `SignalReceived` all go through **real** OPA — so the completed half is now independently evaluated, where the dedupe collision above meant it previously returned the started half's cached verdict. |
| Guardrails eligibility (`governance_workflow.go:429-431`) | Both activity types are guardrails-**eligible**: stage 0 reads `activity_input` (`guardrail.go:180`), stage 1 reads `activity_output` (`guardrail.go:192`). Structural fields only by default (INV-2). |
| Row fields (`storage_event.go:258-294`) | `activity_id`/`activity_type`/`attempt`/`activity_input`→`input`/`activity_output`→`output`/`duration_ms` are set **event-type-agnostically**, so the completed half's duration and output land with no core change. Note `payload.Error` is read **only** for `WorkflowFailed`, which is why the client sends none — see §3. |
| `signal_name` / `workflow_type` | Stored in dedicated columns; commit/deploy lineage rides `metadata` (core has no `commit_sha`/`deploy_id` columns) and **survives** the Signal mapping. |
| Spans table | **No rows for dev sessions.** The accepted trade-off (ADR-0013): no span-level Merkle leaves, no `semantic_type`. Event-level Merkle leaves are unaffected. |
| Dashboard activity timeline (`run.provider.ts`) | Pairs `ActivityStarted`/`ActivityCompleted`, which is now literally what a tool call emits. §7 records whether that renders as expected — **not yet run live**. |

If any future lifecycle type cannot map without a non-additive wire change → **HALT** and route to architecture.

---

## 6. Client transport notes

Verified against the SDK's `request_signing.py` — the client matches core exactly:
- **Body:** compact JSON; the **signed bytes must equal the transmitted bytes** (serialize once, send raw). `capBody` truncation happens **before** marshal = before signing.
- **AIP signature (Ed25519):** canonical string `UPPER(METHOD)\nPATH\nTIMESTAMP\nNONCE\nBODY_SHA256_HEX`; headers `X-OpenBox-Agent-DID/Timestamp/Nonce/Signature`, `X-OpenBox-Body-SHA256`, `Authorization: Bearer <obx_>`, `X-OpenBox-SDK-Version`.
- **`sdk_version`:** set server-side from the header — not in the body.

---

## 7. Live E2E verification

**Status: NOT YET RUN for the ADR-0013 shape, nor for the ADR-0014 turn pair.**
Everything above about core's ingest was established by reading openbox-core, and
reading is not running. The claims below are what `testbed/run-all.sh` must
confirm against a live local stack before any of them is asserted as fact.

The suite's assertions are in place (`testbed/20-capture.sh`, `25-realtime.sh`,
`28-usage.sh`); what is missing is a run. Until then, treat §5's row behavior as
*derived from core's source*, not as observed.

1. **Two rows, one activity.** One tool call ⇒ exactly one `ActivityStarted` and
   one `ActivityCompleted` sharing an `activity_id`. This is the load-bearing
   unverified assumption: that core stores the completed event as its own row
   rather than merging, deduping or rejecting it.
2. **Duration renders.** `duration_ms` present and plausible on the completed
   row, and visible on the dashboard — the field has exactly one consumer and a
   visible failure mode.
3. **Zero span rows.** `select count(*) from spans where session_id=…` is 0,
   asserted deliberately so a future reader does not "fix" it.
4. **Merkle.** Event leaves for both rows; no span leaves.
5. **The approval loop is intact.** Hold → escalate → grant → rewake → consume,
   with `40-approvals.sh` and `70-approver-auto.sh` unmodified. `activity_id` is
   unchanged by design (pinned in `client/approval_key_pin_test.go`), but core's
   span-based approval bypass can no longer fire, so the grant must be consumed
   through the poll path.
6. **The retry-after-completion path, specifically.** Core's approval-status
   query filters on `(workflow_id, run_id, activity_id)` with no `event_type` and
   no ordering (`datastore/governance_event_pgx.go:74-87`, via
   `services/governance.go:291`). That key used to be unique per tool call;
   two activity rows now share it. Drive an operation to completion, then retry
   the **same** operation so it escalates again, and confirm the poll still
   resolves the started row and consumes the grant. If it resolves the completed
   row instead, its NULL `approval_expiration_time` reads as undecided and the
   hold waits out its budget — a core-side fix (scope by `event_type`, or order
   by `approval_expired_at IS NOT NULL DESC, created_at DESC`), not a wire
   change. Found in review; see ADR-0013 Consequences.
7. **INV re-check:** INV-2 (no command text, file body or tool output on the
   observe path), INV-1 (no secrets), INV-3/3b (enforce local, observe never
   blocks), INV-8 (stock core, no accept-list patch).

### Additionally, for the ADR-0014 turn pair

8. **T turns ⇒ T pairs, counted not merely present.** Every
   `<session>:turn:<n>` id has exactly one `ActivityStarted` and one
   `ActivityCompleted`, and the indexes are contiguous from 0. An existence check
   is worthless here: the Tier-2 duplicate-`ActivityStarted` bug shipped because
   the only assertion that would have caught it was an existence check.
9. **A colon-shaped `activity_id` survives ingest.** Core treats the column as an
   opaque string (`activities/governance/validation.go:96`) and the wire field is
   free-form, but no dev event has ever carried a non-hex id. Confirm the row
   stores it verbatim.
10. **Σ per-turn == the SessionEnd rollup, field by field** — input, output,
    cache-creation, cache-read. Two independent derivations of one quantity, and
    they are only comparable now that both un-fold the cache counts (v1.1).
11. **Subagent tokens counted exactly once.** A session that spawns a subagent
    must show separate `:agent:<id>:turn:<n>` records, attributed by `agent_id`,
    with the global sum still equal to the rollup — the double-count this design's
    sidechain partition exists to prevent.
12. **Tool-metric pollution is present and expected.**
    `ExtractToolMetric` accepts any non-empty `activity_type`
    (`observability/errors.go:301-323`), so until openbox-core ships the
    exclusion, `llm_completion` appears under tool metrics with call counts and
    latency percentiles. Record it against the core-side issue; it is not a
    shift-left defect. Once core ships, flip the check to asserting absence.
13. **The narrowed INV-2, end to end.** Sentinel strings seeded into the
    transcript's `content`, `thinking`, `tool_input` and `tool_result` are absent
    from the stored rows, while `model` is present. Unit-level absence is
    necessary, not sufficient — this is the assertion a privacy reviewer should
    be pointed at, because ADR-0014 replaced a structural impossibility with an
    allowlist.
14. **The documented opt-out is real.** With usage capture disabled: zero
    `llm_completion` rows and no model anywhere beyond `SessionStarted`.

_When the run happens, record the artifact under
`plans/260811-0245-tool-activity-event-shape/reports/` (ADR-0013 claims) and
`plans/260811-1640-coding-agent-token-usage/reports/` (ADR-0014 claims), and
replace this status line with what was observed._
