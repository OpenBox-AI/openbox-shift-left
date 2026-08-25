# Mapping — normalized dev event → base-SDK unified wire model (openbox-core `/evaluate`)

**Contract:** [`schema/dev-event.schema.json`](schema/dev-event.schema.json) **v1.2** (the tool-agnostic **adapter-facing** shape).
**Wire model:** the **base SDK's** `EventType` set — `WorkflowStarted / WorkflowCompleted / SignalReceived / ActivityStarted / ActivityCompleted` — serialized by `client/payload.go` (`buildPayload`) onto `POST /api/v1/governance/evaluate` (openbox-core). Every payload is hook-less, and span-less except for the ONE content-gated span a `TurnCompleted` carries (ADR-0018, §2).

> **What changed, read this first.** Two reshapes brought the wire here.
>
> **ADR-0004** retired SL-1's parallel developer vocabulary (`SessionStarted`/`ToolCall`/… passed verbatim, requiring a core accept-list patch) and re-expressed the same normalized dev events onto the base SDK's blessed wire types.
>
> **[ADR-0013](../../docs/adr/ADR-0013-tool-call-as-activity.md)** then moved tool calls off the hook-span envelope: `ToolCall` → `ActivityStarted`, `ToolResult` → `ActivityCompleted`, both span-less. A hook process has no in-process OTel, so the span shift-left used to send was fabricated by hand to satisfy a shape rather than to record a measurement. Retiring it also dissolved ADR-0004's standing obligation to hand-maintain a Go mirror of the base hook contract. **Cost, stated plainly:** dev sessions produce zero `spans` rows for tool calls, so there are no span-level Merkle leaves and no server-side `semantic_type` for them. (**[ADR-0018](../../docs/adr/ADR-0018-dev-turn-content-carrier.md)** later added exactly one span, on a content-capturing `TurnCompleted`, to feed a core reader that accepts no other shape — see §2. Tool calls stay span-less.)
>
> The normalized contract (this schema) was **unchanged** through both — adapters still emit `ToolCall`/`SessionStarted`/…; only the **client→core wire serialization** moved.
>
> **[ADR-0018](../../docs/adr/ADR-0018-dev-turn-content-carrier.md)** is v1.2, and it is purely additive: a top-level `status` on tool results (the field core's success metric reads and no producer had ever written — Tool Health showed 0.0% for every session because of it), three failure/lifecycle signal types, and the one turn span above. **Cost, stated plainly:** the assistant's reply text now egresses under content capture, redacted and capped; and to be classified as an LLM call by core's own recompute, that span must carry synthesized `http.*` attributes describing a request the client never made — marked `openbox.span_synthetic:true` and retired by [openbox-core#130](https://github.com/OpenBox-AI/openbox-core/issues/130).
>
> **[ADR-0014](../../docs/adr/ADR-0014-turn-as-activity-and-identifier-allowlist.md)** is the first change that DID touch this contract, which is why it is v1.1: it adds the model-turn pair (`TurnStarted`/`TurnCompleted`, riding the same two activity wire types with `activity_type: llm_completion`) and the fields it needs (`model`, `turn_index`, `agent_id`), and it **re-defines `tokens.input` as pure input** — v1.0's Claude Code rollup folded both cache counts into it. Everything else is additive; that one semantic is what makes it a version bump rather than a silent edit. **Cost, stated plainly:** the transcript projection's INV-2 guarantee is now a curated allowlist enforced by a test, not a structural impossibility, because the model id is a bound string.

**Two layers.** The *adapter-facing* contract ([schema](schema/dev-event.schema.json)) is what a provider adapter produces via SPI `emit()` — its `event_type` enum is the 12 dev-runtime lifecycle names. The *wire* layer below is what the shared `client/` translates that into. Adding a provider never touches either layer (PRD FR-4, architecture §1b). That the span retirement required **zero** edits under `contracts/dev-event/` is the split working as designed — and that the turn pair DID require edits here is the same split saying, correctly, that this one is a contract change.

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
| — (per signal) | `signal_name` | Set **only** on `SignalReceived` (`prompt_submitted`/`commit_created`/`deploy`/`subagent_started`/`permission_denied`/`api_error`); required there (`event_rules.py` raises `ENVELOPE_MISSING_FIELDS` otherwise). |
| — (activity events) | `activity_id` | Set on **both** halves of a tool call and of a turn. Pairs them onto one row; for a tool call it is additionally the approval key — see §2 "Operation vs invocation identity" and "The turn pair". |
| `tool.name` | `activity_type` | The dashboard's "Activity" column. Lifecycle events carry their `event_type` string instead, so the column is never empty. |
| `tool.*`, `span.*` (started) | `activity_input` | Structural locators only; see §3. Core stores it as the row's `input` and runs Guardrails **stage 0** over it (`internal/services/guardrail.go:180`). |
| `span.*` (completed) | `activity_output` | Counts and an exit code only; see §3. Core stores it as the row's `output` and runs Guardrails **stage 1** over it (`guardrail.go:192`). |
| `started_at` → `ended_at` | `duration_ms` | **Client-computed**, in float milliseconds. Core used to derive the row's duration from the stored span; with no span, the client is the only thing that can. Core copies it onto the row verbatim (`storage_event.go:292-294`) and the dashboard reads `event.duration_ms` directly. **Omitted, never zero**, when unknown — see §3. |
| `timestamp` | `timestamp` | Core field is a **string** (RFC3339) — pass through verbatim. |
| `metadata` | `metadata` (`json.RawMessage`) | Merged per-type keys below; JSON object. Carries commit/deploy lineage (§2). |
| `status` | `status` | **`ToolResult` only**, enum `completed`\|`failed` (ADR-0018). The field core's per-tool success metric reads, and the only one: `IsSuccess = payload.Status != nil && *payload.Status == "completed"` (`openbox-core .../observability/errors.go:333`). **Not content-gated** — derived from which provider hook fired, so it ships identically with `content_capture:false`. Never on a turn/lifecycle/signal event: `payload.status` also writes the row's `workflow_status` column for **any** event type (`storage_event.go:417`), where it means something else. `client.statusFor` enforces both the vocabulary and the scope; C20–C22 assert it on the outbound bytes. |
| `tokens`, `cost`, `model` | `metadata.tokens`, `metadata.cost`, `metadata.model` | No first-class payload fields; carried in `metadata`. On a turn's `ActivityCompleted` the same model + counts ALSO ride `activity_output`, so they are policy-visible — see §2 "The turn pair". |
| `developer_did` | — | Identity is via the signed AIP headers + Bearer key, **not** a body field. `from_agent_did`/`multi_agent_session_id` stay empty (Handoff-only). |
| `span` | — | **Not serialized.** The adapter-facing `span` object is the carrier the client reads locators and counts *out of* (§3); it is never itself emitted, on any event (ADR-0013). Do not confuse it with the wire span below, which shares no code and no fields. |
| `content.output` (turn) | `spans[0].response_body` + `span_count` | **`TurnCompleted` only, content-gated** (ADR-0018). ONE span, carrying the assistant turn's text wrapped as `{"choices":[{"message":{"content":…}}]}` — the exact shape core's goal-alignment extractor unmarshals (`goal_alignment_session.go:64-88`), which reads `payload.Spans` and nothing else. Secret-redacted **before** attachment, then `capBody`-capped at 64KB. Both keys **absent** with capture off. `hook_trigger` is still never sent, on any event: true alongside spans routes the payload into core's approval-bypass fingerprint path (`governance_workflow.go:310-330`). See `client/turnspan.go`. |
| `content.prompt` | `signal_args.prompt` **only when content-capture enabled**, capped to 65536 chars (`capBody`) | Stripped at the client when disabled (INV-2). |
| `content.tool_input` | `activity_input.command` / `.arguments` / `.content` **only when content-capture enabled**, capped | Key named per tool class (`contentKeyFor`), so a reader is never shown a file body labelled `command`. **v1.3 (ADR-0019 P1): also on the OBSERVE path**, not gated calls only — the "never the observe path" half of OD-E9-7 is retired. The gated copy overwrites the observe extract with the bytes the tool rewrite produced, so the server judges exactly what the tool was rewritten to. |
| `content.tool_output` | `activity_output.output` **only when content-capture enabled**, capped | **v1.3 (ADR-0019 P1).** `ToolResult` only. What the tool produced — or, on a failed call, its own free-text error; `status` says which. Core stores it as the row's `output` and runs Guardrails stage "1" over it. Secret-redacted **before** attachment (conformance C34). |
| `content.signal_detail` | `metadata.denial_reason` / `metadata.error_details` **only when content-capture enabled**, capped | **v1.3 (ADR-0019 P1).** Per event type (`signalDetailKeyFor`); dropped on every other type. Deliberately **not** `signal_args` — core reads a `SignalReceived` with non-empty `signal_args` as a NEW USER GOAL (`age.go:112-137`). Conformance C38 asserts both halves. **No reader renders these yet** — the Verify tab reads `signal_args`, which this deliberately avoids — so they are stored-and-queryable rather than displayed. Same posture as `metadata.event_id`. |
| `span.request_body/response_body` (adapter-facing) | — | **Not an egress channel.** These are fields of the frozen ADAPTER-FACING `span` object, which the serializer does not read: no adapter has ever populated either, and both mappers assert they stay empty (`adapters/claude-code/mapper_test.go:169`, `adapters/codex/mapper_test.go:207`). The wire span's `response_body` in the row above is a different field on a different struct (`client.wireSpan`), built only from `content.output`. |

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
| `ToolResult` | `ActivityCompleted` | — | `activity_id`, `activity_type`, `activity_output`, `duration_ms`, **`status`** | `tool_name`, `exit_code`?, `tool_use_id`?, `agent_id`?, `agent_type`?, `is_interrupt`?, `subagent_type`? | its **own** row, sharing the `activity_id`; independently evaluated (OPA + Guardrails stage 1). `status` drives `tool.<name>.success` and is copied to the row's `workflow_status` |
| `TurnStarted` | `ActivityStarted` | — | `activity_id`, `activity_type` (`llm_completion`) | `turn_index`, `agent_id`?, `agent_type`? | one row opening the turn; no `activity_input` (a turn's input is the prompt, which rides the `prompt_submitted` signal under the content gate) |
| `TurnCompleted` | `ActivityCompleted` | — | `activity_id`, `activity_type` (`llm_completion`), `activity_output`, `duration_ms`, `spans`+`span_count` (gated) | `tokens`, `model`, `turn_index`, `agent_id`?, `agent_type`? | its **own** row, sharing the `activity_id`; carries the turn's model + four token counts, and under content capture ONE span carrying the assistant text (§"The turn pair") |
| `SubagentStarted` | `SignalReceived` | `subagent_started` | — | `agent_id`, `agent_type` | mid-session signal; a subagent that spawns and does nothing is now visible |
| `PermissionDenied` | `SignalReceived` | `permission_denied` | — | `tool_name`, `tool_use_id`, `permission_mode`?, `denial_reason`? | mid-session signal: THAT a call was refused, which tool it was about, and — **v1.3, content-gated** — why. The provider's `reason` is free text, so it rides `content.signal_detail` → `metadata.denial_reason` and never `signal_args` |
| `APIError` | `SignalReceived` | `api_error` | — | `error_type` (closed provider enum), `error_details`? | mid-session signal; a turn that ended in a provider error rather than an answer. `error_details` is the provider's free-text elaboration — **v1.3, content-gated**, beside the enum |

Two POSTs per tool call, as before — the count did not change, only the shape.
Two more per model turn, when usage capture is on.

**The three ADR-0018 signals carry NO `signal_args`, and that is a correctness
constraint.** Core's alignment engine treats *any* `SignalReceived` with
non-empty `signal_args` as a new user goal: it scores the assistant messages
accumulated so far against the previous goal and then OVERWRITES the session's
goal with the stringified args (`openbox-core internal/services/age.go:112-137`).
Putting the denied tool's name there — the field the Verify tab renders as
"Input", so an obvious next step — would replace the developer's actual prompt as
the thing every later turn is scored against. `prompt_submitted` is the one
signal that must keep its args, because it is what creates that session.
`client/lifecycle_signals_test.go` and the signal golden fixtures hold both
halves.

Availability note: these four hooks are registered by the installer, so an
**existing install does not emit them until `openbox init` is re-run**. A Claude
Code version that does not know the hook keys never invokes them (verified) — the
events are simply absent, fail-open.

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

#### And, under content capture, ONE span (ADR-0018)

A `TurnCompleted` additionally carries `spans` (exactly one entry) and
`span_count: 1` when the org has content capture on and the hook supplied an
assistant message. That span exists for one reader: core's goal-alignment
extractor takes assistant text from `payload.Spans` and from no other field
(`goal_alignment_session.go:64-88`), so a span-less session can never feed Goal
Alignment Trend or Recent Drift, however much metadata it sends.

Every field of it is dictated by that reader, and each one fails **silently** if
it drifts — core logs and returns `""`, which looks exactly like the empty widget
this is meant to fill:

| Wire field | Value | Why it must be this |
|---|---|---|
| `stage` | `"completed"` | the extractor rejects anything else |
| `semantic_type` | `"llm_completion"` | sent for readability only — core **recomputes** it (`governance_workflow.go:303`) |
| `name` | `"llm_completion"` | must not contain `EMBED`/`TOOL` uppercase, or it classifies as embedding/tool-call (`session.go:323-334`) |
| `attributes` | `http.method=POST`, `http.url=…api.anthropic.com…`, `openbox.span_synthetic=true` | the only inputs that make core's recompute yield `llm_completion` (`isLLMCall`, `session.go:451-476`). **They describe an HTTP request the client never made** — the marker says so on every span |
| `response_body` | `{"choices":[{"message":{"content":…}}]}` | the exact shape the extractor unmarshals |
| `span_id`/`trace_id` | sha256-derived, 16/32 hex | core dedupes on `(span_id, stage)` and the turn cursor re-reads a window after a crash; random ids would store the text twice |

The synthesized attributes are **OD-0018-1**, accepted with a named retirement
condition: [openbox-core#130](https://github.com/OpenBox-AI/openbox-core/issues/130)
moves assistant content onto the `llm_completion` `activity_output`, at which
point the span and its attributes are deleted. Removing them before that lands
kills the feature silently.

This is the ONLY span any developer event carries. Tool events stay span-less,
and `client/hookspan.go`/`spanbuilder.go` stay deleted.

### `semantic_type`: computed for the turn span, absent for everything else

Core derives `SpanData.semantic_type` from a span's source fields
(`ComputeSemanticTypeFromSpan`, `internal/content/session.go:204`), per span,
overwriting whatever was sent.

- **Tool and lifecycle events** send no span, so no `semantic_type` is computed
  for them. That is a real loss, recorded in ADR-0013's consequences —
  `tool.kind` and the `activity_input` locators are what a consumer classifies on
  instead.
- **A content-capturing `TurnCompleted`** does send one, so core classifies it —
  and the client must *feed* that classifier rather than assert a value, which is
  exactly why the synthesized `http.*` attributes exist (above). The client's own
  `semantic_type` on the wire is ignored.

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
| `content.tool_input` | `activity_input.command` / `.arguments` / `.content` | started | content-gated, `capBody`-capped. **v1.3: observe path too**, not gated calls only |
| `content.tool_output` | `activity_output.output` | completed | **v1.3**; content-gated, redacted before attach, `capBody`-capped. A failed call carries its error text here |
| `span.invocation_id` | — (local) | — | feeds the duration-stash key; never a wire field |
| `span.operation_id` | — (local) | — | feeds `activity_id`; never a wire field |
| `span.semantic_type` | — | — | the client has never sent this field; the wire span's own `semantic_type` is built independently and is recomputed by core anyway (§2) |
| `span.stage` | — | — | **retained, read by nothing.** Kept deliberately: the adapter contract is frozen, adapters still set it, and a future span-bearing shape would need it back without an adapter change. (The wire span's `stage` is a separate, hard-coded `"completed"` — it does not come from here) |
| `span.module` | — | — | never had a wire home |
| `span.request_body`, `span.response_body` | — | — | **dropped as an egress channel** (§1). Measured, not assumed: no adapter has ever set either |

Retired with the span layer: `parent_span_id`, `hook_type`, `duration_ns`,
`events`, the family root tuples (`file_mode`, `shell_command`,
`shell_exit_code`, `mcp_method`, …) and the per-span `status` object.
`client/hookspan.go` and `client/spanbuilder.go` — along with
`AssertHookWireShape`, the hand-maintained mirror ADR-0004 flagged as a standing
unverifiable obligation — are deleted, and stay deleted.

**Three names on that list came back with ADR-0018, and one is a different
field entirely.** Stated precisely so this table is not read as either more or
less than it is:

- `span_id`, `trace_id`, `kind`, `attributes` and the 16-/32-hex id derivations
  exist again — on `client.wireSpan`, for the ONE content-gated turn span (§2),
  hash-derived rather than minted at random. Nothing else emits them.
- **`status` on that retired list is the per-span OTel status OBJECT**
  (`{code, description}`), and it is still gone. The top-level `status` in §1 is
  an unrelated new envelope field: a two-literal enum on tool results, ungated.
  Same word, different field, different purpose.

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
| Spans table | **One row per content-capturing model turn, and nothing else** (ADR-0018). Tool and lifecycle events remain span-less — ADR-0013's trade-off (no span-level Merkle leaves, no `semantic_type`) still holds for them. For the turn span, span-level Merkle leaves and server-side classification come back, and the assistant's text is stored server-side: a real retention increase, outside this repo's control. With `content_capture:false` there are no span rows at all. |
| Goal alignment (`age.go`, `goal_alignment_session.go`) | `prompt_submitted`'s `signal_args` CREATE the session's goal; any *other* `SignalReceived` with non-empty `signal_args` OVERWRITES it (`age.go:112-137`), which is why the ADR-0018 signals carry none. Assistant text is appended from `payload.Spans` only. Requires `LlamaFirewallHost` set (`llama_firewall.go:31-34`) and Redis up — **without either, both widgets stay empty with a perfect client**. |
| Tool metrics (`observability/errors.go:301-333`) | `status == "completed"` on an `ActivityCompleted` is the only input to `IsSuccess`; `.total` increments on the started half. `llm_completion` is excluded from tool metrics (`IsLLMCompletionActivity`), so turn events do not pollute them. |
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
3. **Span rows: zero for a tool call; exactly one per captured turn.** With
   `content_capture:false`, `select count(*) from spans where session_id=…` is 0
   — asserted deliberately so a future reader does not "fix" it. With capture on,
   the only rows are `span_type='llm_completion'`, one per turn (ADR-0018).
4. **Merkle.** Event leaves for both rows; span leaves only for the turn spans.
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
12. **`llm_completion` is ABSENT from tool metrics.** Core shipped the exclusion:
    `ExtractToolMetric` returns nil for an `IsLLMCompletionActivity` payload
    (`observability/errors.go:320-322`, read at `develop` 68f0398 — PR #125
    merged as `0643ad3`). This check was previously "pollution is present and
    expected"; it is now an absence assertion.
13. **The narrowed INV-2, end to end.** Sentinel strings seeded into the
    transcript's `content`, `thinking`, `tool_input` and `tool_result` are absent
    from the stored rows, while `model` is present. Unit-level absence is
    necessary, not sufficient — this is the assertion a privacy reviewer should
    be pointed at, because ADR-0014 replaced a structural impossibility with an
    allowlist.
14. **The documented opt-out is real.** With usage capture disabled: zero
    `llm_completion` rows and no model anywhere beyond `SessionStarted`.

### Additionally, for ADR-0018 (status, failure signals, the turn span)

15. **`tool.<name>.success` is non-zero.** On a FRESH agent id — an existing
    agent carries accumulated `.failed` from before this shipped, so it shows
    partial recovery rather than 100%. Also confirm `governance_events.
    workflow_status` holds the same literal on the tool row, which is what
    distinguishes "the client did not send it" from "core did not read it".
16. **A failed tool call is stored as a failure**, with a real `duration_ms`:
    the duration stash has to pair across two DIFFERENT hooks (`PreToolUse` →
    `PostToolUseFailure`), and if it does not, every failure silently drops out
    of the latency percentiles.
17. **Exactly one span row per captured turn**, `span_type='llm_completion'`,
    `stage` completed — and its `response_body` round-trips to the assistant's
    text. If the row exists but `span_type` is anything else, the synthesized
    attributes stopped satisfying `isLLMCall` and alignment is silently dead.
18. **`age_evaluations` has a row** (`span_id IS NULL`) and
    `observability_metrics` carries `metric_type='goal_alignment'`. The honest
    criterion is that an evaluation HAPPENED — a drift verdict is not producible
    on demand. **Check the preconditions first**: `LlamaFirewallHost` set, Redis
    up. Without them both widgets stay empty and the client is not at fault.
19. **The three signal names appear** in `governance_events`
    (`subagent_started`, `permission_denied`, `api_error`) — and each row's
    `signal_args` is NULL. A non-null one means the alignment goal is being
    overwritten by telemetry.
20. **No regression in the tools widget's span CTE**, which selects
    `span_type='mcp_tool_call'` only: the new `llm_completion` spans must not
    appear there.
21. **Capture off ⇒ nothing new.** Re-run with `OPENBOX_CONTENT_CAPTURE=0`: no
    span rows, no assistant text anywhere, and `status` still present on tool
    rows. This single check validates the whole gate design.

_When the run happens, record the artifact under
`plans/260811-0245-tool-activity-event-shape/reports/` (ADR-0013 claims) and
`plans/260811-1640-coding-agent-token-usage/reports/` (ADR-0014 claims), and
replace this status line with what was observed._
