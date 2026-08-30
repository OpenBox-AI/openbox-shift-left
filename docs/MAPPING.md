# Mapping; normalized dev event → base-SDK unified wire model (openbox-core `/evaluate`)

**Contract:** [`schema/dev-event.schema.json`](../api/dev-event.schema.json);
the version is `x-schema-version` in that file, which is the authority; this
document tracks it. **Wire model:** the **base SDK's** `EventType` set,
`WorkflowStarted / WorkflowCompleted / SignalReceived / ActivityStarted /
ActivityCompleted`, serialized by `client/payload.go` (`buildPayload`) onto
`POST /api/v1/governance/evaluate` (openbox-core). Every payload is hook-less.
It is also span-less, with two exceptions and both ride a `TurnCompleted`: the
ONE content-gated span of a hook-observed turn (section 2), and the span
of a gateway-observed model call (section 3).

## Contract versions

The version is `x-schema-version` in the schema file, which is the authority.
Every change below is additive except the one marked otherwise.

| Version | What it added |
|---|---|
| v1.1 | The model-turn pair (`TurnStarted`/`TurnCompleted` on the activity wire types, `activity_type: llm_completion`) and the fields it needs. **Not additive:** `tokens.input` is redefined as pure input, where v1.0's rollup folded both cache counts into it |
| v1.2 | A top-level `status` on tool results, three failure and lifecycle signal types, and one span on a content-capturing `TurnCompleted` |
| v1.3 | Tool content: tool output, observe-path tool input, and the free-text failure detail |
| v1.4 | The turn's extended thinking, in `activity_output.thinking` |
| v1.5 | A second producer rather than a second event: a local relay observes the model call and emits its own `TurnCompleted`, keyed by `gateway_request_id` in a disjoint `:gateway:` namespace |
| v1.6 | Two more producers, `otel_request_id`/`:otel:` and `proxy_request_id`/`:proxy:`, plus the election that keeps exactly one of them emitting per session. Also a repair: `session_rollup` was never a declared property of an `additionalProperties:false` object, and `TurnStarted` required `turn_index` unconditionally, so one adapter's rollup pair had been failing its own contract since v1.1 |

Two reshapes preceded those versions and left the adapter-facing contract
untouched; only the client-to-core payload changed. The first retired a parallel
developer vocabulary that required an accept-list patch in the control plane. The
second moved tool calls off the hook-span envelope onto `ActivityStarted` and
`ActivityCompleted`, both span-less, because a hook process has no in-process
tracing and the span it used to send was fabricated to satisfy a shape rather
than to record a measurement.

The cost of that second reshape still stands: dev sessions produce no `spans`
rows for tool calls, so there are no span-level Merkle leaves and no server-side
`semantic_type` for them. v1.2 added exactly one span back, on a
content-capturing turn, to feed a reader that accepts no other shape.

**Two layers.** The *adapter-facing* contract
([schema](../api/dev-event.schema.json)) is what a provider adapter produces via
SPI `emit`; its `event_type` enum is the 12 dev-runtime lifecycle names. The
*wire* layer below is what the shared `client/` translates that into. Adding a
provider never touches either layer (PRD FR-4, architecture §1b). That the span
retirement required **zero** edits under the contract is the split working as
designed; and that the turn pair DID require edits here is the same split
saying, correctly, that this one is a contract change.

---

## 1. Envelope field mapping (every event)

Source-cited to `client/payload.go` (`governanceEventPayload`, the marshaled
body) and the base contract
`openbox-sdk-python/openbox_core/contracts/events.py` (read-only reference).

| Normalized dev event | → wire `governanceEventPayload` field | Notes |
|---|---|---|
|; (constant) | `source` = `"developer-runtime"` | Free-form in core; distinguishes dev traffic from the SDK's `"workflow-telemetry"`. |
| `event_type` | `event_type` | **Re-mapped**, not passed through; see §2. Resolves to one of the five base wire types. |
| `openbox_session_id` | `run_id` | Session keyed by `(workflow_id, run_id, workflow_type)`. |
| `developer_did` (or workspace/repo id) | `workflow_id` | Stable per-workspace identity so `(workflow_id, run_id)` is unique per session. One derivation, `workflowIDFor`; shared with `ApprovalKeyFor` (§2). |
|; (constant) | `workflow_type` = `"developer-session"` | **Required** by the base contract on `Workflow*` and `SignalReceived` events (`event_rules.py` `_REQUIRED_WORKFLOW_FIELDS`; core reads it into a dedicated column, `storage_event.go`). The *constant* value keeps a session's whole tree on one `(workflow_id, run_id, workflow_type)` identity so core resolves it to **one** session row. Now present on **tool events too**; the old hook envelope omitted it, diverging from the base SDK's `ActivityContext.to_payload_fields`; routing tool events through the same struct fixed that at no cost. |
|; (per signal) | `signal_name` | Set **only** on `SignalReceived` (`prompt_submitted`/`commit_created`/`deploy`/`subagent_started`/`permission_denied`/`api_error`); required there (`event_rules.py` raises `ENVELOPE_MISSING_FIELDS` otherwise). |
|; (activity events) | `activity_id` | Set on **both** halves of a tool call and of a turn. Pairs them onto one row; for a tool call it is additionally the approval key; see §2 "Operation vs invocation identity" and "The turn pair". |
| `gateway_request_id` |; (feeds `activity_id` and the span id) | **v1.5.** A gateway-observed turn's discriminator: the provider's own `Request-Id`, or a locally minted `gw-` id. Its ONLY job is to keep the turn producers in disjoint `activity_id` namespaces: they describe the same turn, and a shared namespace would let core's dedupe absorb one as a duplicate of the other, losing half the evidence with no error anywhere. Bounded and charset-checked by `gatewayemit.usableRequestID` because it originates upstream and reaches a stored key verbatim; and, since v1.6, declared in the schema too, so all three producer ids sit at one depth. The retrofit was initially declined as a contract break and that reasoning was wrong: this field has ONE production assignment path, already gated at the identical rule, so declaring the bound rejects nothing. `gatewayemit.TestGatewayIDBoundMatchesTheContract` holds the two statements of the rule together. |
| `session_rollup` |; (feeds `activity_id`) | **v1.1 on the wire, DECLARED in v1.6.** Marks a turn activity covering a whole session; Codex's granularity, since its per-turn hook is deliberately unwired. Between v1.1 and v1.6 the client emitted this field and the schema did not declare it, so with `additionalProperties:false` every Codex rollup pair failed its own contract; nothing noticed because no fixture carried one. |
| `otel_request_id` |; (feeds `activity_id`) | **v1.6.** A TELEMETRY-observed turn's discriminator (`:otel:`), from the local OTLP receiver. Bound is **declared in the schema** (128 chars, printable ASCII) rather than left to the producer, so a lane added later inherits it. Structural identifier (INV-2), never derived from prompt or body text. |
| `proxy_request_id` |; (feeds `activity_id`) | **v1.6.** A TRANSPORT-observed turn's discriminator (`:proxy:`), from the local in-path TLS relay. Same shape and bound as `otel_request_id`; the lanes differ in vantage point, not in contract. In-path, so it outranks the client-asserted lanes in the producer election. |
| **exactly one of the five above** |; | `$defs.turnProducer` is a five-branch `oneOf` `$ref`'d from BOTH turn branches. Five producers describe the same kind of turn; `turnActivityIDFor` branches on which field is PRESENT. None ⇒ no `activity_id` and the pair never correlates. Two ⇒ the turn is attributed to a producer that did not observe it. The rule lives in one place because stating it per branch is exactly how `TurnStarted` and `TurnCompleted` drifted apart. |
| `tool.name` | `activity_type` | The dashboard's "Activity" column. Lifecycle events carry their `event_type` string instead, so the column is never empty. |
| `tool.*`, `span.*` (started) | `activity_input` | Structural locators only; see §3. Core stores it as the row's `input` and runs Guardrails **stage 0** over it (`internal/services/guardrail.go:180`). |
| `span.*` (completed) | `activity_output` | Counts and an exit code only; see §3. Core stores it as the row's `output` and runs Guardrails **stage 1** over it (`guardrail.go:192`). |
| `started_at` → `ended_at` | `duration_ms` | **Client-computed**, in float milliseconds. Core used to derive the row's duration from the stored span; with no span, the client is the only thing that can. Core copies it onto the row verbatim (`storage_event.go:292-294`) and the dashboard reads `event.duration_ms` directly. **Omitted, never zero**, when unknown; see §3. |
| `timestamp` | `timestamp` | Core field is a **string** (RFC3339); pass through verbatim. |
| `metadata` | `metadata` (`json.RawMessage`) | Merged per-type keys below; JSON object. Carries commit/deploy lineage (§2). |
| `status` | `status` | **`ToolResult` only**, enum `completed`\|`failed`. The field core's per-tool success metric reads, and the only one: `IsSuccess = payload.Status != nil && *payload.Status == "completed"` (`openbox-core.../observability/errors.go:333`). **Not content-gated**; derived from which provider hook fired, so it ships identically with `content_capture:false`. Never on a turn/lifecycle/signal event: `payload.status` also writes the row's `workflow_status` column for **any** event type (`storage_event.go:417`), where it means something else. `client.statusFor` enforces both the vocabulary and the scope; C20–C22 assert it on the outbound bytes. |
| `tokens`, `cost`, `model` | `metadata.tokens`, `metadata.cost`, `metadata.model` | No first-class payload fields; carried in `metadata`. On a turn's `ActivityCompleted` the same model + counts ALSO ride `activity_output`, so they are policy-visible; see §2 "The turn pair". |
| `developer_did` |; | Identity is via the signed AIP headers + Bearer key, **not** a body field. `from_agent_did`/`multi_agent_session_id` stay empty (Handoff-only). |
| `span` |; | **Not serialized.** The adapter-facing `span` object is the carrier the client reads locators and counts *out of* (§3); it is never itself emitted, on any event. Do not confuse it with the wire span below, which shares no code and no fields. |
| `content.output` (turn) | `spans[0].response_body` + `span_count` | **`TurnCompleted` only, content-gated**. ONE span, carrying the assistant turn's text wrapped as `{"choices":[{"message":{"content":…}}]}`; the exact shape core's goal-alignment extractor unmarshals (`goal_alignment_session.go:64-88`), which reads `payload.Spans` and nothing else. Secret-redacted **before** attachment, then `capBody`-capped at 64KB. Both keys **absent** with capture off. `hook_trigger` is still never sent, on any event: true alongside spans routes the payload into core's approval-bypass fingerprint path (`governance_workflow.go:310-330`). See `client/turnspan.go`. |
| `content.prompt` | `signal_args.prompt` **only when content-capture enabled**, capped to 65536 chars (`capBody`) | Stripped at the client when disabled (INV-2). |
| `content.tool_input` | `activity_input.command` / `.arguments` / `.content` **only when content-capture enabled**, capped | Key named per tool class (`contentKeyFor`), so a reader is never shown a file body labelled `command`. **v1.3: also on the OBSERVE path**, not gated calls only; the "never the observe path" half of an owner decision is retired. The gated copy overwrites the observe extract with the bytes the tool rewrite produced, so the server judges exactly what the tool was rewritten to. |
| `content.tool_output` | `activity_output.output` **only when content-capture enabled**, capped | **v1.3.** `ToolResult` only. What the tool produced; or, on a failed call, its own free-text error; `status` says which. Core stores it as the row's `output` and runs Guardrails stage "1" over it. Secret-redacted **before** attachment (conformance C34). |
| `content.thinking` (turn) | `activity_output.thinking` **only when content-capture enabled**, capped | **v1.4.** `TurnCompleted` only. The turn's extended-thinking blocks, concatenated in file order from the transcript window; the only source, since no hook carries thinking and the provider's own OTel export redacts it unconditionally. Deliberately **not** the span in the row above: that span is read as the assistant's REPLY by core's alignment extractor, so chain-of-thought there would score every later turn's drift against the model's reasoning. Secret-redacted **before** attachment, `capBody`-capped, absent with capture off (conformance C40/C41, sentinel `TestFinops_NoContentOnWire`). This is the field that AMENDS v1.1's transcript allowlist; the first free-form content string that projection binds. |
| `content.signal_detail` | `metadata.denial_reason` / `metadata.error_details` **only when content-capture enabled**, capped | **v1.3.** Per event type (`signalDetailKeyFor`); dropped on every other type. Deliberately **not** `signal_args`; core reads a `SignalReceived` with non-empty `signal_args` as a NEW USER GOAL (`age.go:112-137`). Conformance C38 asserts both halves. **No reader renders these yet**; the Verify tab reads `signal_args`, which this deliberately avoids; so they are stored-and-queryable rather than displayed. Same posture as `metadata.event_id`. |
| `span.request_body/response_body` (adapter-facing) | `spans[].request_body` / `.response_body`, **gateway only** | **Not an egress channel for any HOOK adapter**, and that is asserted rather than assumed: neither mapper populates either and both pin them empty (`internal/adapters/claude-code/mapper_test.go:169`, `internal/adapters/codex/mapper_test.go:207`). **v1.5 opened it for exactly one producer**: `gatewayObservedSpan` reads both off this same adapter-facing object when the event carries a `gateway_request_id`, because a relay observes a real request and response. Content-gated (`stripContent` clears them) and `capBody`-capped. The assistant turn span's `response_body` in the row above is yet another path; same wire field, built from `content.output` instead. |

`schema_version` and `event_id` are contract/idempotency fields; `event_id` is
the client's idempotency key (INV-5), used client-side for dedupe; neither is a
core payload field.

---

## 2. Per-type mapping (dev event → base wire event)

Built by `wireTypeFor` in `client/payload.go`; one table for every event type,
feeding one serializer.

| Dev `event_type` | Base wire `event_type` | `signal_name` | Activity fields | Key `metadata` | Core effect |
|---|---|---|---|---|---|
| `SessionStarted` | `WorkflowStarted` |; |; | `provider`, `tool_version`, `repo`, `cwd` | **create** session `(workflow_id, run_id, workflow_type)` (`storage_session.go`) |
| `SessionEnded` | `WorkflowCompleted` |; |; | `total_tokens`, `total_cost`, `duration_ms` | **terminal**; closes the session |
| `PromptSubmitted` | `SignalReceived` | `prompt_submitted` |; | `tokens`, `cost`, `model` | mid-session signal |
| `CommitCreated` | `SignalReceived` | `commit_created` |; | `commit_sha`, `repo`, `branch` (FR-5) | mid-session signal; commit lineage |
| `Deploy` | `SignalReceived` | `deploy` |; | `deploy_id`, `commit_sha`, `repo`, `environment`, `deploy_did` (FR-6/7) | signal; deploy lineage |
| `ToolCall` | `ActivityStarted` |; | `activity_id`, `activity_type`, `activity_input` | `tool_name`, `tool_use_id`?, `agent_id`?, `agent_type`? | one `governance_events` row; pre-exec decision (OPA + Guardrails stage 0) |
| `ToolResult` | `ActivityCompleted` |; | `activity_id`, `activity_type`, `activity_output`, `duration_ms`, **`status`** | `tool_name`, `exit_code`?, `tool_use_id`?, `agent_id`?, `agent_type`?, `is_interrupt`?, `subagent_type`? | its **own** row, sharing the `activity_id`; independently evaluated (OPA + Guardrails stage 1). `status` drives `tool.<name>.success` and is copied to the row's `workflow_status` |
| `TurnStarted` | `ActivityStarted` |; | `activity_id`, `activity_type` (`llm_completion`) | `turn_index`, `agent_id`?, `agent_type`? | one row opening the turn; no `activity_input` (a turn's input is the prompt, which rides the `prompt_submitted` signal under the content gate) |
| `TurnCompleted` | `ActivityCompleted` |; | `activity_id`, `activity_type` (`llm_completion`), `activity_output`, `duration_ms`, `spans`+`span_count` (gated) | `tokens`, `model`, `turn_index`, `agent_id`?, `agent_type`? | its **own** row, sharing the `activity_id`; carries the turn's model + four token counts, and under content capture ONE span carrying the assistant text (§"The turn pair") |
| `SubagentStarted` | `SignalReceived` | `subagent_started` |; | `agent_id`, `agent_type` | mid-session signal; a subagent that spawns and does nothing is now visible |
| `PermissionDenied` | `SignalReceived` | `permission_denied` |; | `tool_name`, `tool_use_id`, `permission_mode`?, `denial_reason`? | mid-session signal: THAT a call was refused, which tool it was about, and; **v1.3, content-gated**; why. The provider's `reason` is free text, so it rides `content.signal_detail` → `metadata.denial_reason` and never `signal_args` |
| `APIError` | `SignalReceived` | `api_error` |; | `error_type` (closed provider enum), `error_details`? | mid-session signal; a turn that ended in a provider error rather than an answer. `error_details` is the provider's free-text elaboration; **v1.3, content-gated**, beside the enum |

Two POSTs per tool call, as before; the count did not change, only the shape.
Two more per model turn, when usage capture is on.

**The three v1.2 signals carry no `signal_args`, and that is a
correctness constraint.** Core's alignment engine treats *any* `SignalReceived`
with non-empty `signal_args` as a new user goal: it scores the assistant
messages accumulated so far against the previous goal and then overwrites the
session's goal with the stringified args (`openbox-core
internal/services/age.go:112-137`). Putting the denied tool's name there, the
field the Verify tab renders as "Input", so an obvious next step, would replace
the developer's actual prompt as the thing every later turn is scored against.
`prompt_submitted` is the one signal that must keep its args, because it is what
creates that session. `client/lifecycle_signals_test.go` and the signal golden
fixtures hold both halves.

Availability note: these four hooks are registered by the installer, so an
**existing install does not emit them until `openbox init` is re-run**. A Claude
Code version that does not know the hook keys never invokes them (verified); the
events are simply absent, fail-open.

### Correlation metadata keys

`metadata` is deliberately a free-form object, so these are **well-known keys
rather than schema fields**; no `schema_version` bump, because the normalized
shape is unchanged and the version `const` marks breaking changes only. All are
structural identifiers (INV-2 permits them; INV-1 still forbids secrets) and all
are optional; a provider that does not expose one simply omits it.

| Key | Providers | Meaning |
|---|---|---|
| `tool_use_id` | Claude Code, Codex | Per-invocation id for a `ToolCall`/`ToolResult` pair. It rides `span.invocation_id`, a *local* field (spooled, never emitted) that keys the cross-process duration stash. The wire pairing itself is `activity_id`. `span.function` is the MCP function name only. |

### Operation vs invocation identity

A tool call has two identities, and the normalized event keeps them apart:

| Field | Means | Derives | Stable across a retry? |
|---|---|---|---|
| `span.invocation_id` | THIS attempt (`tool_use_id`) | the duration-stash key, and `event_id`'s per-call distinguisher | No; by design |
| `span.operation_id` | WHAT is being done | `activity_id` | **Yes; load-bearing** |

`activity_id` is the approval key (`POST /governance/approval`) and the scope of
both of core's bypass grants, so it must survive a retry or an approved request
can never be consumed. Both fields are local: they exist to feed those opaque
hashes and are never emitted as span fields on the core wire payload.

`operation_id` is per class, matching what core's own
`ComputeApprovalFingerprint` keys on: **shell** hashes the command (approving
`ls` must not grant `rm -rf /`), **MCP** hashes the canonicalized argument shape
beside the real function name (core: "same tool with different arguments must
require fresh approval"), and any other class falls back to the invocation;
those expose no structural discriminator, are never escalated, and so can never
hold an approval. The hashes are correlation ids folded into an already-opaque
id, never content fields (INV-2).

> Conflating the two shipped and was found in a live session: every retry became
> a different activity, so an approver's decision could not be consumed, each
> retry filed a fresh request, and the rewake's "re-run to proceed" looped.
| `agent_id`, `agent_type` | Claude Code | Identify the subagent an event occurred inside. Present on *every* payload fired within a subagent, so the subagent tree is reconstructable from tool events alone; which is why the `SubagentStart`/`SubagentStop` boundary markers need no lifecycle type of their own (COVERAGE.md §3.2). |
| `turn_id` | Codex | Per-turn correlation id. |
| `thread_id`, `root_session_id` | Codex | Emitted only when a forked thread's id differs from the session id it continues. |

### Why a `ToolResult` is an `ActivityCompleted`

This reverses what an earlier decision concluded, so the reasoning is worth stating rather
than just the table row.

an earlier decision was right about the base SDK: `wire_event_type` forces `ActivityStarted`
for **any** `hook_trigger` event regardless of stage, and
`assert_hook_wire_shape` asserts `ActivityStarted` unconditionally. Emitting
`ActivityCompleted` *with a hook envelope* would violate that contract.

What changed is the premise, not the rule. **That rule binds hook events, and
shift-left no longer emits any.** The base SDK's hook path exists for runtimes
with in-process OpenTelemetry, where a hook fires mid-activity and has a real
span to attach. A Claude Code or Codex hook is a short-lived separate process
with no OTel at all: the span was hand-fabricated by `client/spanbuilder.go` to
satisfy a shape. Once you stop fabricating it, the tool call is not a hook on an
activity; it *is* the activity, and it takes the ordinary hook-less lifecycle
types.

The two halves pair on **`activity_id` alone** now (there is no `span_id`). It
is derived from `session/tool/locator/operation` with no stage, timestamp or
attempt input, so the two separate hook processes, and a rehydrated spool flush,
mint the same id without threading any state.

> **Supersedes** That decision's `ToolCall`/`ToolResult` rows and an earlier decision's correction
> above. That decision remains the true record of why the first answer was chosen;
> The change is carried
> in premise and the full trade-off.

### The turn pair: `llm_completion` as an `activity_type`

A model turn is the unit a coding agent spends tokens in, and it rides the same
activity carrier a tool call does; `ActivityStarted`/`ActivityCompleted`, both
already accept-listed, so INV-8 needs nothing new.

The shape the AI-Agent runtime uses for this signal is an `llm_completion`
**span** whose `response_body` is `{model, usage{…}}`. Dev sessions write no
spans, and core's span-based usage aggregation is gated on `detectLLMProvider`
reading `span.GetHTTPURL`; which a hook process has none of. So the turn takes
the activity shape and mirrors the span's `response_body` inside
`activity_output`:

```json
{"model": "claude-opus-4-8",
 "usage": {"input_tokens": 1204, "output_tokens": 318,
           "cache_creation_input_tokens": 4096, "cache_read_input_tokens": 58210}}
```

Four numbers and one bounded identifier; nothing else may enter this object.
Core runs Guardrails stage 1 and OPA over `activity_output`, so token spend is
policy-visible (intended), and the schema staying numbers-plus-one-identifier is
what keeps that safe.

`activity_type` is the literal `llm_completion` on **both** halves. The name is
core's own (`internal/content/session.go:105` `SemanticTypeLLMCompletion`), so
one vocabulary spans both runtimes and the core-side extractor keys on a name
core already knows.

**`activity_id` is `<session_id>:turn:<index>`**, or
`<session_id>:agent:<agent_id>:turn:<index>` for a subagent's turn. Not a hash,
unlike the tool-call id: a turn has no operation to key on, is never approved
(so the id is not an approval key), and a readable id is worth having in stored
rows. It cannot collide with `cc-act-<32 hex>` by construction, and
`client/turn_key_pin_test.go` pins the bytes and the separation.

**Both halves are emitted from one hook firing** (Claude Code's `Stop`), so the
pair is atomic; no orphan half, no cross-hook turn index to race, and queued
prompts fold into one turn. The Started half's timestamp is the locally-parsed
turn-open time, used only to compute `duration_ms`; it is consumer-invisible
either way, since core derives duration from `duration_ms` on the Completed half
alone. The timestamp *string* never reaches the wire.

Codex reaches the same signal at session granularity: one pair at SessionEnd
with `activity_id <session_id>:usage:rollup`. Its `Stop` hook exists but is
deliberately unwired; scope, not impossibility.

>
> carries the full trade-off, including the one that matters: the transcript
> projection's INV-2 guarantee is now a **curated allowlist enforced by a test**
> rather than a structural impossibility, because `message.model` is bound.

#### And, under content capture, ONE span

A `TurnCompleted` additionally carries `spans` (exactly one entry) and
`span_count: 1` when the org has content capture on and the hook supplied an
assistant message. That span exists for one reader: core's goal-alignment
extractor takes assistant text from `payload.Spans` and from no other field
(`goal_alignment_session.go:64-88`), so a span-less session can never feed Goal
Alignment Trend or Recent Drift, however much metadata it sends.

Every field of it is dictated by that reader, and each one fails **silently** if
it drifts; core logs and returns `""`, which looks exactly like the empty widget
this is meant to fill:

| Wire field | Value | Why it must be this |
|---|---|---|
| `stage` | `"completed"` | the extractor rejects anything else |
| `semantic_type` | `"llm_completion"` | sent for readability only; core **recomputes** it (`governance_workflow.go:303`) |
| `name` | `"llm_completion"` | must not contain `EMBED`/`TOOL` uppercase, or it classifies as embedding/tool-call (`session.go:323-334`) |
| `attributes` | `http.method=POST`, `http.url=…api.anthropic.com…`, `openbox.span_synthetic=true` | the only inputs that make core's recompute yield `llm_completion` (`isLLMCall`, `session.go:451-476`). **They describe an HTTP request the client never made**; the marker says so on every span |
| `response_body` | `{"choices":[{"message":{"content":…}}]}` | the exact shape the extractor unmarshals |
| `span_id`/`trace_id` | sha256-derived, 16/32 hex | core dedupes on `(span_id, stage)` and the turn cursor re-reads a window after a crash; random ids would store the text twice |

The synthesized attributes are **an owner decision**, accepted with a named retirement
condition: when the control plane moves assistant content onto the
`llm_completion` `activity_output`, the span and its attributes are deleted.
Removing them before that lands kills the feature silently.

This is the only span any developer event carries. Tool events stay span-less,
and `client/hookspan.go`/`spanbuilder.go` stay deleted.

### `semantic_type`: computed for the turn span, absent for everything else

Core derives `SpanData.semantic_type` from a span's source fields
(`ComputeSemanticTypeFromSpan`, `internal/content/session.go:204`), per span,
overwriting whatever was sent.

- **Tool and lifecycle events** send no span, so no `semantic_type` is computed
  for them. That is a real loss's consequences; `tool.kind` and the
  `activity_input` locators are what a consumer classifies on instead.
- **A content-capturing `TurnCompleted`** does send one, so core classifies it;
  and the client must *feed* that classifier rather than assert a value, which
  is exactly why the synthesized `http.*` attributes exist (above). The client's
  own `semantic_type` on the wire is ignored.

MAPPING.md previously carried an "an earlier decision dependency (server-side, pending)" claim
that `shell`→`shell_command` and `mcp`→`mcp_tool_call` classification was
awaiting a core edit. **That claim is deleted, not restated.** It was already
contradicted by observed data (a live span carried `semantic_type:
"shell_command"` while the openbox-core checkout defines `mcp_tool_call`
(`session.go:111`) and no `shell_command` constant), it named no owner, and it
is now moot: with no span there is nothing to classify. An unowned claim in a
governance product is worse than an acknowledged gap.

---

## 3. Field homes; where every `DevEvent.Span` field goes

**This table is the authority on what the serializer reads.** The adapter-facing
`span` object is frozen at schema v1.0 and adapters still populate it; what
changed is that `client/payload.go` now reads locators and counts *out* of it
into `activity_input`/`activity_output` instead of serializing it as a span.

| `DevEvent` field | Wire home | Read on | Notes |
|---|---|---|---|
| `tool.name` | `activity_input.tool_name`, `activity_type`, `metadata.tool_name` | started (+`activity_type` on both) | |
| `tool.kind` | `activity_input.kind` | started | `shell`/`file`/`mcp`; the classification that replaces `semantic_type` |
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
| `content.thinking` | `activity_output.thinking` | completed | **v1.4**, turns only; content-gated, redacted before attach, `capBody`-capped. Sourced from the transcript window, not a hook field |
| `span.invocation_id` |; (local) |; | feeds the duration-stash key; never a wire field |
| `span.operation_id` |; (local) |; | feeds `activity_id`; never a wire field |
| `span.semantic_type` |; |; | the client has never sent this field; the wire span's own `semantic_type` is built independently and is recomputed by core anyway (§2) |
| `span.stage` |; |; | **retained, read by nothing.** Kept deliberately: the adapter contract is frozen, adapters still set it, and a future span-bearing shape would need it back without an adapter change. (The wire span's `stage` is a separate, hard-coded `"completed"`; it does not come from here) |
| `span.module` |; |; | never had a wire home |
| `span.request_body`, `span.response_body` | `spans[].request_body` / `.response_body` | completed | **Dropped for every producer except one.** No hook adapter sets either, and none may; that is what §1's "span-less" means. The GATEWAY sets both (v1.5): they are the observed model request and response, content-gated by `stripContent`, `capBody`-capped. The assistant turn span also writes `response_body`, with the reply text |

**Gateway-only span fields** (v1.5). All `omitempty` and declared last on
`wireSpan`, so the assistant turn span's bytes, and its golden fixture, are
untouched. `gatewayObservedSpan` is the only producer; a hook event carries none
of them.

| `DevEvent` field | Wire home | Gated | Notes |
|---|---|---|---|
| `span.request_headers` | `spans[].request_headers` | **yes** | Credential headers are already replaced by KEY NAME at capture, before the gate is consulted; the key survives so an authenticated call stays distinguishable from an anonymous one. Bounded per value and per map (`capHeaders`) because an upstream header block is bounded only by the Transport's 10 MiB default, and an oversized event is rejected whole |
| `span.response_headers` | `spans[].response_headers` | **yes** | same treatment |
| `span.http_method` | `spans[].http_method` | no | structural. Core RECOMPUTES `semantic_type` per span and `isLLMCall` is the only path to `llm_completion`, reading this plus an LLM domain in the URL; so a span missing the pair still stores, classifies as something else, and every `llm_completion` reader goes quiet with no error |
| `span.http_url` | `spans[].http_url` | no | structural, **query dropped**; so a provider that ever accepted content or a token as a query parameter cannot turn this ungated field into a content leak |
| `span.http_status` | `spans[].**http_status_code**` | no | structural. The rename is load-bearing: core's `SpanData` spells it `http_status_code` and `encoding/json` drops an unrecognized key silently, so the short spelling reached core's parser and vanished before policy or storage saw it. Absent (`omitempty`) when no response was observed at all; a relayed call whose transport failed after the request went out |
| `span.credential_fingerprint` | `spans[].credential_fingerprint` **and** `spans[].attributes["openbox.credential_fingerprint"]` | no | Both, deliberately. Core has no field for the top-level key so it is dropped on ingest today; `attributes` is a real `SpanData` field that survives and is stored, so it is the copy account binding can match on. Ungated because a privacy switch must not let an org opt out of being identified |

Retired with the span layer: `parent_span_id`, `hook_type`, `duration_ns`,
`events`, the family root tuples (`file_mode`, `shell_command`,
`shell_exit_code`, `mcp_method`, …) and the per-span `status` object.
`client/hookspan.go` and `client/spanbuilder.go`, along with
`AssertHookWireShape`, the hand-maintained mirror flagged as a
standing unverifiable obligation, are deleted, and stay deleted.

**Three names on that list came back with v1.2, and one is a different
field entirely.** Stated precisely so this table is not read as either more or
less than it is:

- `span_id`, `trace_id`, `kind`, `attributes` and the 16-/32-hex id derivations
  exist again; on `client.wireSpan`, for the ONE content-gated turn span (§2),
  hash-derived rather than minted at random. Nothing else emits them.
- **`status` on that retired list is the per-span OTel status object** (`{code,
  description}`), and it is still gone. The top-level `status` in §1 is an
  unrelated new envelope field: a two-literal enum on tool results, ungated.
  Same word, different field, different purpose.

The golden fixtures in `client/testdata/golden/activity_*.json` pin this table
byte-exactly, one per tool kind per stage. If a fixture carries a field this
table does not list, one of the two is wrong.

---

## 4. Verdict (parsing the response)

The `/evaluate` response is core's `EvaluationResult`; `verdict` + legacy
`action` + `fallback_used` (confirmed live spike). The canonical enum is
`HALT > BLOCK > REQUIRE_APPROVAL > CONSTRAIN > ALLOW`; the wire is
**lowercase**:

| Canonical | Wire `verdict` | Legacy `action` |
|---|---|---|
| `ALLOW` | `allow` | `continue` |
| `CONSTRAIN` | `constrain` | `continue` |
| `REQUIRE_APPROVAL` | `require_approval` | `require-approval` |
| `BLOCK` | `block` | `stop` |
| `HALT` | `halt` | `stop` |

`fallback_used=true` marks a fail-open verdict (core's OPA/Guardrail unreachable
→ default ALLOW). Phase-1 **observe** (D7/INV-3) treats every async-egress
verdict as advisory and never blocks the tool call. Enforcement (Phase-2, E6) is
a **separate, local** decision on the sidecar `DecisionRequest`; it does **not**
read this response (INV-3b; enforce path untouched by the E7 wire reshape).

---

## 5. INV-8 / conformance statement

The wire model is now the base SDK's **stock** vocabulary; no dev-specific
`event_type` strings, so **no core accept-list patch is needed** (an earlier decision: all
base types → HTTP 200 on stock core). This is a **net simplification** vs the event contract's
EXT-core patch, which an earlier decision retires.

**Downstream-consumer behavior on the unified shape** (verified an earlier decision live +
cross-repo Explore):

| Consumer | Behavior |
|---|---|
| Session store (`storage_session.go`) | `WorkflowStarted`→create, `WorkflowCompleted`→terminal; **native**, no EXT-core lifecycle edit. Unchanged by. |
| Accept-list (`internal/api/governance.go:273-286`) | All five types we emit are accept-listed, `ActivityCompleted` included; no core patch. |
| Idempotency / dedupe (`activities/governance/validation.go:96`) | Keyed on `(agent_id, workflow_id, run_id, activity_id, event_type)`. Because `event_type` is in the key, a tool call's two halves are now **distinct** events. Under the hook shape they matched on all five; same `activity_id`, both `ActivityStarted`; so the `ToolResult` POST hit the existing-event branch (`governance_workflow.go:228-231`) and was substantially a no-op. A **retry** of the same half still dedupes correctly, which is the behavior you want. |
| OPA policy eval (`opa.go`) | Bypassed (auto-allow) **only** for `Workflow*` (latency). `ActivityStarted`, **`ActivityCompleted`** and `SignalReceived` all go through **real** OPA; so the completed half is now independently evaluated, where the dedupe collision above meant it previously returned the started half's cached verdict. |
| Guardrails eligibility (`governance_workflow.go:429-431`) | Both activity types are guardrails-**eligible**: stage 0 reads `activity_input` (`guardrail.go:180`), stage 1 reads `activity_output` (`guardrail.go:192`). Structural fields only by default (INV-2). |
| Row fields (`storage_event.go:258-294`) | `activity_id`/`activity_type`/`attempt`/`activity_input`→`input`/`activity_output`→`output`/`duration_ms` are set **event-type-agnostically**, so the completed half's duration and output land with no core change. Note `payload.Error` is read **only** for `WorkflowFailed`, which is why the client sends none; see §3. |
| `signal_name` / `workflow_type` | Stored in dedicated columns; commit/deploy lineage rides `metadata` (core has no `commit_sha`/`deploy_id` columns) and **survives** the Signal mapping. |
| Spans table | **One row per content-capturing model turn, and nothing else**. Tool and lifecycle events remain span-less; the trade-off (no span-level Merkle leaves, no `semantic_type`) still holds for them. For the turn span, span-level Merkle leaves and server-side classification come back, and the assistant's text is stored server-side: a real retention increase, outside this repo's control. With `content_capture:false` there are no span rows at all. |
| Goal alignment (`age.go`, `goal_alignment_session.go`) | `prompt_submitted`'s `signal_args` CREATE the session's goal; any *other* `SignalReceived` with non-empty `signal_args` OVERWRITES it (`age.go:112-137`), which is why the v1.2 signals carry none. Assistant text is appended from `payload.Spans` only. Requires `LlamaFirewallHost` set (`llama_firewall.go:31-34`) and Redis up; **without either, both widgets stay empty with a perfect client**. |
| Tool metrics (`observability/errors.go:301-333`) | `status == "completed"` on an `ActivityCompleted` is the only input to `IsSuccess`; `.total` increments on the started half. `llm_completion` is excluded from tool metrics (`IsLLMCompletionActivity`), so turn events do not pollute them. |
| Dashboard activity timeline (`run.provider.ts`) | Pairs `ActivityStarted`/`ActivityCompleted`, which is now literally what a tool call emits. §7 records whether that renders as expected; **not yet run live**. |

If any future lifecycle type cannot map without a non-additive wire change →
**HALT** and route to architecture.

---

## 6. Client transport notes

Verified against the SDK's `request_signing.py`; the client matches core
exactly:
- **Body:** compact JSON; the **signed bytes must equal the transmitted bytes**
  (serialize once, send raw). `capBody` truncation happens **before** marshal =
  before signing.
- **AIP signature (Ed25519):** canonical string
  `UPPER(METHOD)\nPATH\nTIMESTAMP\nNONCE\nBODY_SHA256_HEX`; headers
  `X-OpenBox-Agent-DID/Timestamp/Nonce/Signature`, `X-OpenBox-Body-SHA256`,
  `Authorization: Bearer <obx_>`, `X-OpenBox-SDK-Version`.
- **`sdk_version`:** set server-side from the header; not in the body.

---

## 7. What a live run must confirm

Everything above about how the control plane ingests these events was
established by reading its source, and reading is not running. `test/run-all.sh`
carries the assertions; the suite has not been run against a live stack, so the
row behaviour in section 5 is derived rather than observed.

Until it runs, none of the following is asserted as fact.

### Activity rows and identity

| # | What a run must confirm |
|---|---|
| 1 | One tool call stores exactly one `ActivityStarted` and one `ActivityCompleted` sharing an `activity_id`, rather than merging, deduping or rejecting the second |
| 2 | `duration_ms` is present and plausible on the completed row, and renders |
| 4 | Event Merkle leaves exist for both rows; span leaves only for turn spans |
| 8 | T turns store T pairs, counted rather than merely present, with contiguous indexes from 0 |
| 9 | A colon-shaped `activity_id` survives ingest unaltered |
| 27, 32 | The producers' `activity_id`s are disjoint on a session carrying more than one |
| 31 | A `session_rollup` turn pair stores as one row |
| 33 | Exactly one model-call producer emits per session |
| 36, 37 | An `:otel:` and a `:proxy:` `TurnCompleted` each store as their own row |

### Spans

| # | What a run must confirm |
|---|---|
| 3, 17 | Zero span rows for a tool call; exactly one `span_type='llm_completion'` per captured turn |
| 23 | Thinking is absent from the turn's span while present on the activity |
| 25 | A gateway span stores as its own row with the observed request and response |
| 26 | `attributes["openbox.credential_fingerprint"]` survives ingest |
| 28 | `semantic_type` recomputes to `llm_completion` from the synthesized `http.*` attributes |
| 29 | A capture-off gateway span still stores, carrying only structural evidence |

### Usage, content and posture

| # | What a run must confirm |
|---|---|
| 10 | Per-turn token counts sum to the SessionEnd rollup, field by field |
| 11 | Subagent tokens are counted exactly once |
| 12 | `llm_completion` is absent from tool metrics |
| 13, 7 | The invariants hold end to end: no secrets, no ungated content, enforce local, observe never blocking |
| 14, 21 | Each documented opt-out is real: capture off stores nothing new |
| 22 | `activity_output.thinking` survives ingest as its own key |

### Enforcement and approvals

| # | What a run must confirm |
|---|---|
| 5 | Hold, escalate, grant, rewake, consume, with the approval scripts unmodified |
| 6 | A retry after completion still resolves the started row and consumes the grant. Core's approval-status query filters on `(workflow_id, run_id, activity_id)` with no `event_type` and no ordering, and two activity rows now share that key. If it resolves the completed row instead, the null expiration reads as undecided and the hold waits out its budget |
| 15, 16 | `tool.<name>.success` is non-zero, and a failed call stores as a failure with a real duration |
| 18, 19, 20 | Alignment has a row, the three signal names appear, and the tools widget is unaffected |

### Lane intake and volume

| # | What a run must confirm |
|---|---|
| 34 | The OTLP intake accepts what the client actually exports, over protobuf |
| 35 | The 13 telemetry environment keys are the ones the tool reads. This is the one claim this repository cannot verify about itself |
| 38 | The election holds across a real session, not only per record |
| 24, 30, 39 | Volume at each lane's real cadence: thinking at up to 64KB per turn, a full request and response per gateway call, and 69,179 bytes of spool per transport call |
