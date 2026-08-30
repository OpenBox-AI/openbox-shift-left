# Mapping — normalized dev event → base-SDK unified wire model (openbox-core `/evaluate`)

**Contract:** [`schema/dev-event.schema.json`](../api/dev-event.schema.json) — the version is `x-schema-version` in that file, which is the authority; this document tracks it. **Wire model:** the **base SDK's** `EventType` set — `WorkflowStarted / WorkflowCompleted / SignalReceived / ActivityStarted / ActivityCompleted` — serialized by `client/payload.go` (`buildPayload`) onto `POST /api/v1/governance/evaluate` (openbox-core). Every payload is hook-less. It is also span-less, with
two exceptions and both ride a `TurnCompleted`: the ONE content-gated span of a hook-observed turn (that decision, §2), and the span of a gateway-observed model call (that decision, §3).

> **What changed, read this first.** Two reshapes brought the wire here.
>
> **that decision** retired SL-1's parallel developer vocabulary (`SessionStarted`/`ToolCall`/… passed verbatim, requiring a core accept-list patch) and re-expressed the same normalized dev events onto the base SDK's blessed wire types.
>
> **that decision** then moved tool calls off the hook-span envelope: `ToolCall` → `ActivityStarted`, `ToolResult` → `ActivityCompleted`, both span-less. A hook process has no in-process OTel, so the span shift-left used to send was fabricated by hand to satisfy a shape rather than to record a measurement. Retiring it also dissolved that decision's standing obligation to hand-maintain a Go mirror of the base hook contract. **Cost, stated plainly:** dev sessions produce zero `spans` rows for tool calls, so there are no span-level Merkle leaves and no server-side `semantic_type` for them. (**that decision** later added exactly one span, on a content-capturing `TurnCompleted`, to feed a core reader that accepts no other shape — see §2. Tool calls stay span-less.)
>
> The normalized contract (this schema) was **unchanged** through both — adapters still emit `ToolCall`/`SessionStarted`/…; only the **client→core wire serialization** moved.
>
> **that decision** is v1.2, and it is purely additive: a top-level `status` on tool results (the field core's success metric reads and no producer had ever written — Tool Health showed 0.0% for every session because of it), three failure/lifecycle signal types, and the one turn span above. **Cost, stated plainly:** the assistant's reply text now egresses under content capture, redacted and capped; and to be classified as an LLM call by core's own recompute, that span must carry synthesized `http.*` attributes describing a request the client never made — marked `openbox.span_synthetic:true` and retired by [openbox-core#130](https://github.com/OpenBox-AI/openbox-core/issues/130).
>
> **that decision** is v1.5, purely additive, and it introduces a **second producer** rather than a second event: a local relay observes the model call itself and emits a `TurnCompleted` alongside the hook path's. Both describe a turn, so the one thing that must never collide is `activity_id` — hence `gateway_request_id` (§1) and its disjoint `:gateway:` namespace. Its span is the first this client sends that is **measured rather than synthesized**, so it carries no `openbox.span_synthetic` marker and its `http_*` fields are observations. **Cost, stated plainly:** the largest content class this contract carries (a whole model request, system prompt included) and the first span fields that ship with content capture OFF — `http_method`, `http_url`, `http_status` and `credential_fingerprint` are structural account-binding evidence, deliberately outside the privacy switch.
>
> **that decision** is v1.6 and generalizes what that decision started: not a second producer but a **fifth**. A local OTLP receiver (`otel_request_id`, `:otel:`) and a local in-path TLS relay (`proxy_request_id`, `:proxy:`) each observe model calls the gateway lane cannot reach — the desktop app and subscription-OAuth sessions, measured. Disjoint namespaces keep their `activity_id`s apart; a separate **producer election** (exactly one lane emits per session) keeps the COUNT right, and neither substitutes for the other. It also **repairs** the contract: `session_rollup` was never a declared property while the object is `additionalProperties:false`, and `TurnStarted` required `turn_index` unconditionally — so Codex's rollup pair had been failing its own contract since v1.1 — both halves, every session. The gateway was NOT affected: it emits no `TurnStarted` at all (`gatewayemit.EventFor` is `TurnCompleted`-only, deliberately), so v1.5's half-repair cost it nothing; what the unconditional `turn_index` would have broken is any LATER lane emitting a pair, which is what phases 09–11 intend. The exactly-one rule now lives once, in `$defs.turnProducer`, `$ref`'d by both halves. **Cost, stated plainly:** the transport lane puts a CA on the developer's machine (that decision reversed, OD2); the telemetry lane's claim is the weakest in the product, since the tool being governed is the reporter; and ~95% of model-call bodies exceed the 64KB cap, so their tails exist nowhere org-side (OD1(c)).
>
> **that decision** is the first change that DID touch this contract, which is why it is v1.1: it adds the model-turn pair (`TurnStarted`/`TurnCompleted`, riding the same two activity wire types with `activity_type: llm_completion`) and the fields it needs (`model`, `turn_index`, `agent_id`), and it **re-defines `tokens.input` as pure input** — v1.0's Claude Code rollup folded both cache counts into it. Everything else is additive; that one semantic is what makes it a version bump rather than a silent edit. **Cost, stated plainly:** the transcript projection's INV-2 guarantee is now a curated allowlist enforced by a test, not a structural impossibility, because the model id is a bound string.

**Two layers.** The *adapter-facing* contract ([schema](../api/dev-event.schema.json)) is what a provider adapter produces via SPI `emit()` — its `event_type` enum is the 12 dev-runtime lifecycle names. The *wire* layer below is what the shared `client/` translates that into. Adding a provider never touches either layer (PRD FR-4, architecture §1b). That the span retirement required **zero** edits under the contract is the split working as designed — and that the turn pair DID require edits here is the same split saying, correctly, that this one is a contract change.

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
| `gateway_request_id` | — (feeds `activity_id` and the span id) | **v1.5.** A gateway-observed turn's discriminator: the provider's own `Request-Id`, or a locally minted `gw-` id. Its ONLY job is to keep the turn producers in disjoint `activity_id` namespaces: they describe the same turn, and a shared namespace would let core's dedupe absorb one as a duplicate of the other, losing half the evidence with no error anywhere. Bounded and charset-checked by `gatewayemit.usableRequestID` because it originates upstream and reaches a stored key verbatim — and, since v1.6, declared in the schema too, so all three producer ids sit at one depth. The retrofit was initially declined as a contract break and that reasoning was wrong: this field has ONE production assignment path, already gated at the identical rule, so declaring the bound rejects nothing. `gatewayemit.TestGatewayIDBoundMatchesTheContract` holds the two statements of the rule together. |
| `session_rollup` | — (feeds `activity_id`) | **v1.1 on the wire, DECLARED in v1.6.** Marks a turn activity covering a whole session — Codex's granularity, since its per-turn hook is deliberately unwired. Between v1.1 and v1.6 the client emitted this field and the schema did not declare it, so with `additionalProperties:false` every Codex rollup pair failed its own contract; nothing noticed because no fixture carried one. |
| `otel_request_id` | — (feeds `activity_id`) | **v1.6.** A TELEMETRY-observed turn's discriminator (`:otel:`), from the local OTLP receiver. Bound is **declared in the schema** (128 chars, printable ASCII) rather than left to the producer, so a lane added later inherits it. Structural identifier (INV-2), never derived from prompt or body text. |
| `proxy_request_id` | — (feeds `activity_id`) | **v1.6.** A TRANSPORT-observed turn's discriminator (`:proxy:`), from the local in-path TLS relay. Same shape and bound as `otel_request_id`; the lanes differ in vantage point, not in contract. In-path, so it outranks the client-asserted lanes in the producer election. |
| **exactly one of the five above** | — | `$defs.turnProducer` is a five-branch `oneOf` `$ref`'d from BOTH turn branches. Five producers describe the same kind of turn; `turnActivityIDFor` branches on which field is PRESENT. None ⇒ no `activity_id` and the pair never correlates. Two ⇒ the turn is attributed to a producer that did not observe it. The rule lives in one place because stating it per branch is exactly how `TurnStarted` and `TurnCompleted` drifted apart. |
| `tool.name` | `activity_type` | The dashboard's "Activity" column. Lifecycle events carry their `event_type` string instead, so the column is never empty. |
| `tool.*`, `span.*` (started) | `activity_input` | Structural locators only; see §3. Core stores it as the row's `input` and runs Guardrails **stage 0** over it (`internal/services/guardrail.go:180`). |
| `span.*` (completed) | `activity_output` | Counts and an exit code only; see §3. Core stores it as the row's `output` and runs Guardrails **stage 1** over it (`guardrail.go:192`). |
| `started_at` → `ended_at` | `duration_ms` | **Client-computed**, in float milliseconds. Core used to derive the row's duration from the stored span; with no span, the client is the only thing that can. Core copies it onto the row verbatim (`storage_event.go:292-294`) and the dashboard reads `event.duration_ms` directly. **Omitted, never zero**, when unknown — see §3. |
| `timestamp` | `timestamp` | Core field is a **string** (RFC3339) — pass through verbatim. |
| `metadata` | `metadata` (`json.RawMessage`) | Merged per-type keys below; JSON object. Carries commit/deploy lineage (§2). |
| `status` | `status` | **`ToolResult` only**, enum `completed`\|`failed`. The field core's per-tool success metric reads, and the only one: `IsSuccess = payload.Status != nil && *payload.Status == "completed"` (`openbox-core .../observability/errors.go:333`). **Not content-gated** — derived from which provider hook fired, so it ships identically with `content_capture:false`. Never on a turn/lifecycle/signal event: `payload.status` also writes the row's `workflow_status` column for **any** event type (`storage_event.go:417`), where it means something else. `client.statusFor` enforces both the vocabulary and the scope; C20–C22 assert it on the outbound bytes. |
| `tokens`, `cost`, `model` | `metadata.tokens`, `metadata.cost`, `metadata.model` | No first-class payload fields; carried in `metadata`. On a turn's `ActivityCompleted` the same model + counts ALSO ride `activity_output`, so they are policy-visible — see §2 "The turn pair". |
| `developer_did` | — | Identity is via the signed AIP headers + Bearer key, **not** a body field. `from_agent_did`/`multi_agent_session_id` stay empty (Handoff-only). |
| `span` | — | **Not serialized.** The adapter-facing `span` object is the carrier the client reads locators and counts *out of* (§3); it is never itself emitted, on any event. Do not confuse it with the wire span below, which shares no code and no fields. |
| `content.output` (turn) | `spans[0].response_body` + `span_count` | **`TurnCompleted` only, content-gated**. ONE span, carrying the assistant turn's text wrapped as `{"choices":[{"message":{"content":…}}]}` — the exact shape core's goal-alignment extractor unmarshals (`goal_alignment_session.go:64-88`), which reads `payload.Spans` and nothing else. Secret-redacted **before** attachment, then `capBody`-capped at 64KB. Both keys **absent** with capture off. `hook_trigger` is still never sent, on any event: true alongside spans routes the payload into core's approval-bypass fingerprint path (`governance_workflow.go:310-330`). See `client/turnspan.go`. |
| `content.prompt` | `signal_args.prompt` **only when content-capture enabled**, capped to 65536 chars (`capBody`) | Stripped at the client when disabled (INV-2). |
| `content.tool_input` | `activity_input.command` / `.arguments` / `.content` **only when content-capture enabled**, capped | Key named per tool class (`contentKeyFor`), so a reader is never shown a file body labelled `command`. **v1.3 : also on the OBSERVE path**, not gated calls only — the "never the observe path" half of OD-E9-7 is retired. The gated copy overwrites the observe extract with the bytes the tool rewrite produced, so the server judges exactly what the tool was rewritten to. |
| `content.tool_output` | `activity_output.output` **only when content-capture enabled**, capped | **v1.3.** `ToolResult` only. What the tool produced — or, on a failed call, its own free-text error; `status` says which. Core stores it as the row's `output` and runs Guardrails stage "1" over it. Secret-redacted **before** attachment (conformance C34). |
| `content.thinking` (turn) | `activity_output.thinking` **only when content-capture enabled**, capped | **v1.4.** `TurnCompleted` only. The turn's extended-thinking blocks, concatenated in file order from the transcript window — the only source, since no hook carries thinking and the provider's own OTel export redacts it unconditionally. Deliberately **not** the span in the row above: that span is read as the assistant's REPLY by core's alignment extractor, so chain-of-thought there would score every later turn's drift against the model's reasoning. Secret-redacted **before** attachment, `capBody`-capped, absent with capture off (conformance C40/C41, sentinel `TestFinops_NoContentOnWire`). This is the field that AMENDS that decision's transcript allowlist — the first free-form content string that projection binds. |
| `content.signal_detail` | `metadata.denial_reason` / `metadata.error_details` **only when content-capture enabled**, capped | **v1.3.** Per event type (`signalDetailKeyFor`); dropped on every other type. Deliberately **not** `signal_args` — core reads a `SignalReceived` with non-empty `signal_args` as a NEW USER GOAL (`age.go:112-137`). Conformance C38 asserts both halves. **No reader renders these yet** — the Verify tab reads `signal_args`, which this deliberately avoids — so they are stored-and-queryable rather than displayed. Same posture as `metadata.event_id`. |
| `span.request_body/response_body` (adapter-facing) | `spans[].request_body` / `.response_body`, **gateway only** | **Not an egress channel for any HOOK adapter**, and that is asserted rather than assumed: neither mapper populates either and both pin them empty (`internal/adapters/claude-code/mapper_test.go:169`, `internal/adapters/codex/mapper_test.go:207`). **v1.5 opened it for exactly one producer**: `gatewayObservedSpan` reads both off this same adapter-facing object when the event carries a `gateway_request_id`, because a relay observes a real request and response. Content-gated (`stripContent` clears them) and `capBody`-capped. The assistant turn span's `response_body` in the row above is yet another path — same wire field, built from `content.output` instead. |

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

**The three that decision signals carry NO `signal_args`, and that is a
correctness constraint.** Core's alignment engine treats *any* `SignalReceived`
with non-empty `signal_args` as a new user goal: it scores the assistant messages
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

### Why a `ToolResult` is an `ActivityCompleted`

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

> **Supersedes** That decision's `ToolCall`/`ToolResult` rows and E7-S4's correction
> above. That decision remains the true record of why the first answer was chosen;
> that decision carries the change
> in premise and the full trade-off.

### The turn pair: `llm_completion` as an `activity_type`

A model turn is the unit a coding agent spends tokens in, and it rides the same
activity carrier a tool call does — `ActivityStarted`/`ActivityCompleted`, both
already accept-listed, so INV-8 needs nothing new.

The shape the AI-Agent runtime uses for this signal is an `llm_completion`
**span** whose `response_body` is `{model, usage{…}}`. Dev sessions write no
spans, and core's span-based usage aggregation is gated on `detectLLMProvider`
reading `span.GetHTTPURL()` — which a hook process has none of. So the turn
takes the activity shape and mirrors the span's `response_body` inside
`activity_output`:

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
for them. That is a real loss's consequences — `tool.kind` and the
`activity_input` locators are what a consumer classifies on instead.
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
| `content.thinking` | `activity_output.thinking` | completed | **v1.4**, turns only; content-gated, redacted before attach, `capBody`-capped. Sourced from the transcript window, not a hook field |
| `span.invocation_id` | — (local) | — | feeds the duration-stash key; never a wire field |
| `span.operation_id` | — (local) | — | feeds `activity_id`; never a wire field |
| `span.semantic_type` | — | — | the client has never sent this field; the wire span's own `semantic_type` is built independently and is recomputed by core anyway (§2) |
| `span.stage` | — | — | **retained, read by nothing.** Kept deliberately: the adapter contract is frozen, adapters still set it, and a future span-bearing shape would need it back without an adapter change. (The wire span's `stage` is a separate, hard-coded `"completed"` — it does not come from here) |
| `span.module` | — | — | never had a wire home |
| `span.request_body`, `span.response_body` | `spans[].request_body` / `.response_body` | completed | **Dropped for every producer except one.** No hook adapter sets either, and none may — that is what §1's "span-less" means. The GATEWAY sets both (v1.5): they are the observed model request and response, content-gated by `stripContent`, `capBody`-capped. The assistant turn span also writes `response_body`, with the reply text |

**Gateway-only span fields** (v1.5). All `omitempty` and declared last on `wireSpan`, so the assistant turn span's bytes — and its golden fixture — are untouched. `gatewayObservedSpan` is the only producer; a hook event carries none of them.

| `DevEvent` field | Wire home | Gated | Notes |
|---|---|---|---|
| `span.request_headers` | `spans[].request_headers` | **yes** | Credential headers are already replaced by KEY NAME at capture, before the gate is consulted; the key survives so an authenticated call stays distinguishable from an anonymous one. Bounded per value and per map (`capHeaders`) because an upstream header block is bounded only by the Transport's 10 MiB default, and an oversized event is rejected whole |
| `span.response_headers` | `spans[].response_headers` | **yes** | same treatment |
| `span.http_method` | `spans[].http_method` | no | structural. Core RECOMPUTES `semantic_type` per span and `isLLMCall` is the only path to `llm_completion`, reading this plus an LLM domain in the URL — so a span missing the pair still stores, classifies as something else, and every `llm_completion` reader goes quiet with no error |
| `span.http_url` | `spans[].http_url` | no | structural, **query dropped** — so a provider that ever accepted content or a token as a query parameter cannot turn this ungated field into a content leak |
| `span.http_status` | `spans[].**http_status_code**` | no | structural. The rename is load-bearing: core's `SpanData` spells it `http_status_code` and `encoding/json` drops an unrecognized key silently, so the short spelling reached core's parser and vanished before policy or storage saw it. Absent (`omitempty`) when no response was observed at all — a relayed call whose transport failed after the request went out |
| `span.credential_fingerprint` | `spans[].credential_fingerprint` **and** `spans[].attributes["openbox.credential_fingerprint"]` | no | Both, deliberately. Core has no field for the top-level key so it is dropped on ingest today; `attributes` is a real `SpanData` field that survives and is stored, so it is the copy account binding can match on. Ungated because a privacy switch must not let an org opt out of being identified |

Retired with the span layer: `parent_span_id`, `hook_type`, `duration_ns`,
`events`, the family root tuples (`file_mode`, `shell_command`,
`shell_exit_code`, `mcp_method`, …) and the per-span `status` object.
`client/hookspan.go` and `client/spanbuilder.go` — along with
`AssertHookWireShape`, the hand-maintained mirror that decision flagged as a
standing unverifiable obligation — are deleted, and stay deleted.

**Three names on that list came back with that decision, and one is a
different field entirely.** Stated precisely so this table is not read as
either more or less than it is:

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
| Session store (`storage_session.go`) | `WorkflowStarted`→create, `WorkflowCompleted`→terminal — **native**, no EXT-core lifecycle edit. Unchanged by. |
| Accept-list (`internal/api/governance.go:273-286`) | All five types we emit are accept-listed, `ActivityCompleted` included — no core patch. |
| Idempotency / dedupe (`activities/governance/validation.go:96`) | Keyed on `(agent_id, workflow_id, run_id, activity_id, event_type)`. Because `event_type` is in the key, a tool call's two halves are now **distinct** events. Under the hook shape they matched on all five — same `activity_id`, both `ActivityStarted` — so the `ToolResult` POST hit the existing-event branch (`governance_workflow.go:228-231`) and was substantially a no-op. A **retry** of the same half still dedupes correctly, which is the behavior you want. |
| OPA policy eval (`opa.go`) | Bypassed (auto-allow) **only** for `Workflow*` (latency). `ActivityStarted`, **`ActivityCompleted`** and `SignalReceived` all go through **real** OPA — so the completed half is now independently evaluated, where the dedupe collision above meant it previously returned the started half's cached verdict. |
| Guardrails eligibility (`governance_workflow.go:429-431`) | Both activity types are guardrails-**eligible**: stage 0 reads `activity_input` (`guardrail.go:180`), stage 1 reads `activity_output` (`guardrail.go:192`). Structural fields only by default (INV-2). |
| Row fields (`storage_event.go:258-294`) | `activity_id`/`activity_type`/`attempt`/`activity_input`→`input`/`activity_output`→`output`/`duration_ms` are set **event-type-agnostically**, so the completed half's duration and output land with no core change. Note `payload.Error` is read **only** for `WorkflowFailed`, which is why the client sends none — see §3. |
| `signal_name` / `workflow_type` | Stored in dedicated columns; commit/deploy lineage rides `metadata` (core has no `commit_sha`/`deploy_id` columns) and **survives** the Signal mapping. |
| Spans table | **One row per content-capturing model turn, and nothing else**. Tool and lifecycle events remain span-less — that decision's trade-off (no span-level Merkle leaves, no `semantic_type`) still holds for them. For the turn span, span-level Merkle leaves and server-side classification come back, and the assistant's text is stored server-side: a real retention increase, outside this repo's control. With `content_capture:false` there are no span rows at all. |
| Goal alignment (`age.go`, `goal_alignment_session.go`) | `prompt_submitted`'s `signal_args` CREATE the session's goal; any *other* `SignalReceived` with non-empty `signal_args` OVERWRITES it (`age.go:112-137`), which is why that decision signals carry none. Assistant text is appended from `payload.Spans` only. Requires `LlamaFirewallHost` set (`llama_firewall.go:31-34`) and Redis up — **without either, both widgets stay empty with a perfect client**. |
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

**Status: NOT YET RUN for that decision shape, nor for that decision turn pair.**
Everything above about core's ingest was established by reading openbox-core, and
reading is not running. The claims below are what `test/run-all.sh` must confirm
against a live local stack before any of them is asserted as fact.

The suite's assertions are in place (`test/20-capture.sh`, `25-realtime.sh`,
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
`content_capture:false`, `select count(*) from spans where session_id=…` is 0 —
asserted deliberately so a future reader does not "fix" it. With capture on, the
only rows are `span_type='llm_completion'`, one per turn.
4. **Merkle.** Event leaves for both rows; span leaves only for the turn spans.
5. **The approval loop is intact.** Hold → escalate → grant → rewake → consume,
   with `40-approvals.sh` and `70-approver-auto.sh` unmodified. `activity_id` is
   unchanged by design (pinned in `client/approval_key_pin_test.go`), but core's
   span-based approval bypass can no longer fire, so the grant must be consumed
   through the poll path.
6. **The retry-after-completion path, specifically.** Core's approval-status
query filters on `(workflow_id, run_id, activity_id)` with no `event_type` and no
ordering (`datastore/governance_event_pgx.go:74-87`, via
`services/governance.go:291`). That key used to be unique per tool call; two
activity rows now share it. Drive an operation to completion, then retry the
**same** operation so it escalates again, and confirm the poll still resolves the
started row and consumes the grant. If it resolves the completed row instead, its
NULL `approval_expiration_time` reads as undecided and the hold waits out its
budget — a core-side fix (scope by `event_type`, or order by `approval_expired_at
IS NOT NULL DESC, created_at DESC`), not a wire change. Found in review;
Consequences.
7. **INV re-check:** INV-2 (no command text, file body or tool output on the
   observe path), INV-1 (no secrets), INV-3/3b (enforce local, observe never
   blocks), INV-8 (stock core, no accept-list patch).

### Additionally, for that decision turn pair

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
13. **The amended INV-2, end to end.** Sentinel strings seeded into the
    transcript's `content`, `tool_input` and `tool_result` are absent from the
    stored rows, while `model` is present. Unit-level absence is necessary, not
    sufficient — this is the assertion a privacy reviewer should be pointed at,
    because that decision replaced a structural impossibility with an allowlist.
    **v1.4 makes this two-directional:** the `thinking` sentinel must be absent
    from stored rows with capture off and PRESENT (redacted, capped) with it on.
    "The marker is nowhere" and "the runtime stored nothing" are the same
    observation; only the positive half separates them.
14. **The documented opt-out is real.** With usage capture disabled: zero
    `llm_completion` rows and no model anywhere beyond `SessionStarted`.

### Additionally, for that decision (status, failure signals, the turn span)

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
    span rows, no assistant text anywhere, no `thinking` key, and `status` still
    present on tool rows. This single check validates the whole gate design.

### Additionally, for that decision (thinking)

22. **`activity_output.thinking` survives ingest**, as its own key on the turn's
    `ActivityCompleted` row. Core's merged `ExtractModelMetricsFromActivity`
    unmarshals `{model, usage}` from this object, so a sibling key should be
    ignored by the Go decoder and the whole object stored as the row's output.
    Both halves are **read, not run** — the test is what settles whether the
    key is stored and whether the extra field perturbs usage extraction.
23. **Thinking is NOT in the turn's span**, on the same rows: the span's
    `response_body` carries the reply and only the reply. If thinking appears
    there, goal-alignment scoring is comparing later turns against
    chain-of-thought, which fails silently and looks like drift.
24. **Volume, measured not assumed.** Thinking at ≤64KB per turn through the
    realtime flusher is the same open question phase 01 left for tool bodies —
    record bytes/session, not just presence.

### Additionally, for that decision (the gateway span)

Driven by `test/45-gateway.sh`, which is written and has never run.

25. **A gateway span is stored as its own `spans` row**, with the observed
    `http_method` / `http_url` / `http_status_code`. The rename matters more than
    presence: the short spelling `http_status` was silently dropped by core's
    decoder, so assert the value arrived, not that the client sent it.
26. **`attributes["openbox.credential_fingerprint"]` survives ingest.** It is the
    only copy account binding can match on — core has no top-level field, so that
    key is dropped. If the attribute does not survive either, that decision has no
    evidence to decide on and the account rule cannot fire.
27. **The two producers' `activity_id`s are disjoint** on one session that has both.
    A collision means core's dedupe absorbed one turn as a duplicate of the other
    and half the evidence is gone with no error — case 45.B asserts the intersection
    is empty.
28. **`semantic_type` recomputes to `llm_completion`.** Core recomputes it per span
    and `isLLMCall` reads method + URL; if it classifies as anything else, every
    reader that filters on `llm_completion` goes quiet with no error anywhere.
29. **A capture-off gateway span still STORES**, carrying only its structural
    fields. The outbound bytes are already pinned both ways
    (`TestGatewaySpanContentGatedOffTheWire`); what a live run adds is whether core
    keeps a span row whose bodies and header maps are absent, since this is the one
    span where the gate removes some fields and deliberately keeps others.
30. **Volume.** A full model request and response is a larger increment than
    thinking or tool bodies — record bytes/session at tool-call cadence before any
    cap is widened.

### Additionally, for that decision (the telemetry and transport lanes)

Contract-level only — items 31–33 are what **v1.6 alone** puts on the wire, and
they can be confirmed the moment either lane emits. The lanes' own behavioural
claims belong to phases 09–13 and are listed in their plan, not here.

31. **A `session_rollup` turn pair STORES as one row.** This is the repair, and it
    is the only item here that concerns a shape the client has emitted for months:
    Codex's session usage has been failing its own contract since v1.1, so a live
    run is the first evidence that core accepts `<session>:usage:rollup` as an
    `activity_id` and pairs the two halves on it. If it does not, the defect was
    never only in the schema.
32. **`activity_id`s stay disjoint across every lane present on one session.**
    Asserted locally across all five shapes
    (`TestEveryTurnProducerNamespaceIsDisjoint`); what a live run adds is that core
    stores them as distinct rows rather than deduping any pair together. The
    intersection must be empty — a collision means half the evidence is gone with
    no error, the same failure case 45.B checks for the gateway.
33. **Exactly one model-call producer emitted per session.** Distinct from item 32:
    disjoint ids make core store BOTH, so a failed election shows up as a doubled
    token count on every dashboard rather than as missing rows. Count the
    `llm_completion` activities for a session against the turns actually taken.

### Additionally, for the lanes themselves (that decision, phases 09–13)

The paragraph above used to say the lanes' behavioural claims "belong to phases
09–13 and are listed in their plan, not here." Both lanes now exist and emit, so
their live-stack items belong with the rest. Items 34–39 are held by the dormant
`test/46-otel-lane.sh` and `47-transport.sh`.

Everything below is unproven for one shared reason: both lanes are verified by
**replay** — real recorded traffic through the shipped code path on a host that
cannot bind — so no socket, no supervisor and no control plane has been in the
path of one of these events.

34. **The OTLP HTTP intake accepts what the CLIENT actually exports.** The intake
    itself is not unrun: `TestTelemetryCommandActuallyRecords` POSTs a real
    OTLP/JSON export to a real receiver on a real port and reads the governance
    event back off disk, and it **passed on a bind-capable host** (phase 09). What
    it has never seen is a real Claude Code export — its own protobuf encoding,
    its own resource attributes, its own batching. The replay path adds nothing
    here, because it enters one layer BELOW the HTTP server
    (`Receiver.ConsumeLogsJSON`).
35. **The 13 telemetry env keys are the ones Claude Code reads.** This is the one
    claim the repo cannot test about itself: every test asserts JSON we wrote, and
    the client silently ignores a name it does not recognise, so a rename yields a
    green suite and a receiver that never gets a record. The set is copied verbatim
    from the sibling lab run that produced the corpus and pinned as a literal list
    (`internal/cli/activation/keys.go`). Confirm by observing a record arrive, not
    by reading the file back.
36. **Core stores an `:otel:` `TurnCompleted` as its own row**, and its synthetic
    span (`openbox.span_synthetic`) classifies as `llm_completion` after ingest.
    Same failure mode as item 28: misclassification is silent.
37. **Core stores a `:proxy:` `TurnCompleted` as its own row.** Its span carries
    the provider's raw response body, so — exactly as for the gateway lane — it
    contributes nothing to goal alignment. Confirm the row, not the alignment.
38. **The election holds across a real session, not just per record.** Item 33
    counts producers; this adds the ordering that broke once already — install
    telemetry first and transport second, then confirm telemetry falls silent from
    the next record rather than from the next daemon restart.
39. **Volume at the transport lane's real cadence.** Measured locally at **70,080
    bytes of spool per model call** (~334 MB per 5,000-call session, run
    `20260827T063932Z-225cac`). What a live run adds is whether the flusher and the
    control plane absorb that rate; it is a larger increment than item 30's.

_When the run happens, record the artifact under
`plans/260811-0245-tool-activity-event-shape/reports/` (that decision
claims) and `plans/260811-1640-coding-agent-token-usage/reports/` (that
decision claims), and replace this status line with what was observed._
