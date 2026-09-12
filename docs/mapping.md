# Mapping; normalized dev event → base-SDK unified wire model (openbox-core `/evaluate`)

**Contract:** [`schema/dev-event.schema.json`](../api/dev-event.schema.json);
the version is `x-schema-version` in that file, which is the authority; this
document tracks it. **Wire model:** the **base SDK's** `EventType` set,
`WorkflowStarted / WorkflowCompleted / SignalReceived / ActivityStarted /
ActivityCompleted`, serialized by `client/payload.go` (`buildPayload`) onto
`POST /api/v1/governance/evaluate` (openbox-core). Every payload is hook-less.
It is also **entirely span-less, with no exceptions** as of v1.7. Both carriers
that used to exist -- the content-gated span of a hook-observed turn and the span
of a relay-observed model call -- are gone, because the control plane parses
`spans[]` and then discards it. Section 2 carries the reasoning.

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
| v1.7 | `activity_type`, which names an event's class from a closed vocabulary, and **the removal of the span carrier**. Classifying a relayed call by HTTP method alone had filed every POST as `llm_completion`, and ~40% of those rows were token-count probes. The spans went because the control plane parses `spans[]` and discards it, so nothing sent there was ever stored: model-call content moved to `activity_input`/`activity_output`, and the credential fingerprint to `metadata`. The in-path and telemetry lanes also began emitting BOTH halves of their activity, which every `activity_id` requires and none of them did |
| v1.8 | 21 observe-only lifecycle signal classes (setup, instructions, prompt expansion, message display, permission requests, tool batches, notifications, tasks, teammates, config changes, cwd/directory/file/worktree changes, compaction, model switching, elicitation), all riding stock `SignalReceived`; plus three additive run-identity fields (`run_id` declared for the first time, `run_generation`, `continued_from_run_id`) describing which run of a session produced an event; plus a subagent spawn's whole `tool_input` landing under `activity_input.arguments`. A `token_count` probe now egresses nothing at all, not even the classification it used to skip bodies on. All additive; a generation-0, non-spawn v1.7 payload is unchanged |

Two reshapes preceded those versions and left the adapter-facing contract
untouched; only the client-to-core payload changed. The first retired a parallel
developer vocabulary that required an accept-list patch in the control plane. The
second moved tool calls off the hook-span envelope onto `ActivityStarted` and
`ActivityCompleted`, both span-less, because a hook process has no in-process
tracing and the span it used to send was fabricated to satisfy a shape rather
than to record a measurement.

The cost of that second reshape still stands and is now total: dev sessions
produce **no `spans` rows at all**, so there are no span-level Merkle leaves and no
server-side `semantic_type` anywhere. v1.2 did add one span back, on a
content-capturing turn, to feed a reader that accepted no other shape; v1.7
removed it again, on the evidence that the control plane never stored it and that
the reader now feeds from `activity_input` instead (§2).

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
| `openbox_session_id` | `run_id` | Session keyed by `(workflow_id, run_id, workflow_type)`. **Generation 0 only**: the wire `run_id` equals this field exactly when no continued run exists yet. See "Run identity" below for generation ≥ 1. |
| `developer_did` (or workspace/repo id) | `workflow_id` | Stable per-workspace identity so `(workflow_id, run_id)` is unique per session. One derivation, `workflowIDFor`; shared with `ApprovalKeyFor` (§2). Unchanged by run identity: a continued run keeps the same `workflow_id`. |
| `run_id` (adapter-facing; v1.8) | `run_id` (wire; overrides `openbox_session_id` when present) | **v1.8.** The minted v4 UUID of a **continued** (`--resume`-opened) run, set by the adapter at hook time from the local run record. `runIDFor` prefers this over `openbox_session_id`; absent at generation 0, where the fallback is `openbox_session_id`, so every 1.7 payload is unchanged. The same value reaches `ApprovalKeyFor`, so an approval always addresses the run that asked. |
| `run_generation` (v1.8) | `run_generation` | **v1.8.** 0 for the original run; incremented once per `SessionStart(source=resume)`. Informational: a local counter that MAY restart at 1 if the run record is lost — an ordering hint, never a key. |
| `continued_from_run_id` (v1.8) | `continued_from_run_id` | **v1.8.** The sealed run this one continues from, set ONLY on the `WorkflowStarted` of a generation ≥ 1 run. May name a run this machine never itself sent (a session resumed elsewhere). |
|; (constant) | `workflow_type` = `"developer-session"` | **Required** by the base contract on `Workflow*` and `SignalReceived` events (`event_rules.py` `_REQUIRED_WORKFLOW_FIELDS`; core reads it into a dedicated column, `storage_event.go`). The *constant* value keeps a session's whole tree on one `(workflow_id, run_id, workflow_type)` identity so core resolves it to **one** session row. Now present on **tool events too**; the old hook envelope omitted it, diverging from the base SDK's `ActivityContext.to_payload_fields`; routing tool events through the same struct fixed that at no cost. |
|; (per signal) | `signal_name` | Set **only** on `SignalReceived` (`prompt_submitted`/`commit_created`/`deploy`/`subagent_started`/`permission_denied`/`api_error`); required there (`event_rules.py` raises `ENVELOPE_MISSING_FIELDS` otherwise). |
|; (activity events) | `activity_id` | Set on **both** halves of a tool call and of a turn. Pairs them onto one row; for a tool call it is additionally the approval key; see §2 "Operation vs invocation identity" and "The turn pair". **A pairing check must tolerate a missing Completed half** — see the interrupt note below. |
| `gateway_request_id` |; (feeds `activity_id` and the span id) | **v1.5.** A gateway-observed turn's discriminator: the provider's own `Request-Id`, or a locally minted `gw-` id. Its ONLY job is to keep the turn producers in disjoint `activity_id` namespaces: they describe the same turn, and a shared namespace would let core's dedupe absorb one as a duplicate of the other, losing half the evidence with no error anywhere. Bounded and charset-checked by `gatewayemit.usableRequestID` because it originates upstream and reaches a stored key verbatim; and, since v1.6, declared in the schema too, so all three producer ids sit at one depth. The retrofit was initially declined as a contract break and that reasoning was wrong: this field has ONE production assignment path, already gated at the identical rule, so declaring the bound rejects nothing. `gatewayemit.TestGatewayIDBoundMatchesTheContract` holds the two statements of the rule together. |
| `session_rollup` |; (feeds `activity_id`) | **v1.1 on the wire, DECLARED in v1.6.** Marks a turn activity covering a whole session; Codex's granularity, since its per-turn hook is deliberately unwired. Between v1.1 and v1.6 the client emitted this field and the schema did not declare it, so with `additionalProperties:false` every Codex rollup pair failed its own contract; nothing noticed because no fixture carried one. |
| `otel_request_id` |; (feeds `activity_id`) | **v1.6.** A TELEMETRY-observed turn's discriminator (`:otel:`), from the local OTLP receiver. Bound is **declared in the schema** (128 chars, printable ASCII) rather than left to the producer, so a lane added later inherits it. Structural identifier ([INV-2](dev-event-contract.md#invariants)), never derived from prompt or body text. |
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
| `span` |; | **Not serialized, and there is no longer a wire span to confuse it with.** The adapter-facing `span` object is the carrier the client reads locators, counts and bodies *out of* (§3); it is never itself emitted, on any event, and no event carries `spans[]` or `span_count` at all (§2). |
| `content.output` (turn) | `activity_output.content` **only when content-capture enabled**, capped | **`TurnCompleted` only.** The assistant turn's text. It rode `spans[0].response_body`, wrapped as `{"choices":[{"message":{"content":…}}]}`, for one reader -- and core parses `spans[]` on the normal path and then discards it, so it was never stored anywhere at all (§2). Verbatim now, with no OpenAI-chat wrapper, in a field that round-trips. Secret-redacted **before** attachment, capped, **absent** with capture off. `hook_trigger` is still never sent, on any event: true alongside spans routes the payload into core's approval-bypass fingerprint path (`governance_workflow.go:310-330`), and `internal/client/lifecyclepairing_test.go`'s sibling census in `modelcallcontent_test.go` is what holds that now. |
| `content.prompt` | `signal_args.prompt` **only when content-capture enabled**, capped to 65536 chars (`capBody`) | Stripped at the client when disabled ([INV-2](dev-event-contract.md#invariants)). |
| `content.tool_input` | `activity_input.command` / `.arguments` / `.content` **only when content-capture enabled**, capped | Key named per tool class (`contentKeyFor`), so a reader is never shown a file body labelled `command`. **v1.3: also on the OBSERVE path**, not gated calls only; the "never the observe path" half of an owner decision is retired. The gated copy overwrites the observe extract with the bytes the tool rewrite produced, so the server judges exactly what the tool was rewritten to. **v1.8 (C-03):** `arguments` gained a second producer — an `Agent`/`ToolSearch` spawn is `kind:shell` for local-enforce purposes but semantically `llm_tool_call`, so `contentKeyFor(kind, sem)` special-cases it to `arguments` **before** the shell branch, deliberately never `command`, because `command` is an enforcement key 71 of the seeded control pack's 104 conditions match on. |
| `content.tool_output` | `activity_output.output` **only when content-capture enabled**, capped | **v1.3.** `ToolResult` only. What the tool produced; or, on a failed call, its own free-text error; `status` says which. Core stores it as the row's `output` and runs Guardrails stage "1" over it. Secret-redacted **before** attachment (conformance C34). |
| `content.thinking` (turn) | `activity_output.thinking` **only when content-capture enabled**, capped | **v1.4.** `TurnCompleted` only. The turn's extended-thinking blocks, concatenated in file order from the transcript window; the only source, since no hook carries thinking and the provider's own OTel export redacts it unconditionally. Deliberately **not** merged into `activity_output.content`, which carries the assistant's REPLY: chain-of-thought there would score every later turn's drift against the model's reasoning. (It used to say "not the span in the row above"; that span is gone, the separation is not.) Secret-redacted **before** attachment, `capBody`-capped, absent with capture off (conformance C40/C41, sentinel `TestFinops_NoContentOnWire`). This is the field that AMENDS v1.1's transcript allowlist; the first free-form content string that projection binds. |
| `content.signal_detail` | `metadata.<per-class key>` **and** `signal_args.<same key>` **only when content-capture enabled**, capped | **v1.3; both destinations since v1.9.** Per event type (`signalDetailKeyFor`); dropped on every other type. It used to be deliberately **not** `signal_args`, because core read a `SignalReceived` with non-empty `signal_args` as a NEW USER GOAL (`HandleSessionLifecycle`, `goal_alignment.go`); core's goal gate now scopes that to `prompt_submitted`. Conformance C38 asserts both halves. **These are now shown in more places**: the Verify tab already rendered `metadata`, but `signal_args` is additionally rendered in session replay, event details, the monitor and stored guardrail-violation and compliance-evidence records — see [data-and-privacy.md](data-and-privacy.md). |
| `span.request_body` (adapter-facing) | `activity_input.content` on the **opening** half, **only when content-capture enabled**, capped | **In-path lanes only.** Not an egress channel for any HOOK adapter, and that is asserted rather than assumed: neither mapper populates it and both pin it empty (`internal/adapters/claude-code/mapper_test.go:169`, `internal/adapters/codex/mapper_test.go:207`). The key is `content` because Goal Alignment's per-operation cap orders keys by a fixed priority list and drops unlisted ones first, so `request_body` would have been the first casualty (§2). Capped keeping the **TAIL**: a `/v1/messages` body is a conversation whose newest turn is at the end. Content-gated; `stripContent` clears it. |
| `span.response_body` (adapter-facing) | `activity_output.content` on the **closing** half, **only when content-capture enabled**, capped | **In-path lanes only**, same gate. The relay's observed response, verbatim, SSE frames and all: what a relay saw is what an auditor wants, and reassembling it is a semantic transform belonging with the consumer. Capped keeping the **HEAD**, because a reply starts at its beginning. A content-encoded body is decompressed in the capture path first (§ below); before that, every response body OpenBox had ever captured was an 88-byte placeholder. |
| `span.request_headers` / `.response_headers` (adapter-facing) |; | **No longer emitted by any producer.** They reached core only inside `spans[]`, which is discarded, so nothing ever read them; and they were the highest-risk class this client carried, because the developer's live provider credential is on every model request. Still declared in the contract, and still redacted by key name at capture; they simply do not leave the machine. |
| `span.credential_fingerprint` (adapter-facing) | `metadata.credential_fingerprint`, **ungated** | Rehomed from the wire span's `attributes`, which never persisted for a developer session -- so this is a rehoming, not a regression, and account binding has a route into core for the first time. Deliberately **not** in `contentMetadataKeys`: it is one-way derived governance evidence, and letting a privacy setting remove it would let an org opt out of being identified. |

`schema_version` and `event_id` are contract/idempotency fields; `event_id` is
the client's idempotency key (INV-5), used client-side for dedupe; neither is a
core payload field.

### Run identity: what continues, and what does not (v1.8)

Full measurement in [coverage.md §1a](coverage.md#1a-clear-and---resume-are-different-and-only-one-continues-a-run); the derivation rule here. **`--resume` is the only source that continues a run**: same session id before and after, a fresh v4 UUID `run_id` minted for the new run, `continued_from_run_id` naming the sealed run it continues — Temporal's continue-as-new shape (<https://docs.temporal.io/workflow-execution/continue-as-new>). **`clear`, `startup`, `compact` and `fork` do not continue a run**; `clear` and `fork` additionally mint a brand-new session id, so there is nothing to link to, and `compact` fires no `SessionEnd` at all. **Every `SessionEnd` seals its run, for every reason, byte-identically** — no suspended-session state, no `session_suspended` wire value. Consequence: goal alignment resets on `/clear` (a new, unrelated session to core) but carries forward on `--resume` (core seeds the continued run from the previous run's latest prompt, [phase 13](../plans/260905-2345-unified-dev-event-plan/phase-13-core-goal-carry-forward.md)).

**The schema's own prose is currently stale here.** `api/dev-event.schema.json`'s `openbox_session_id` and `run_generation` descriptions still name `clear` alongside `resume` as a bump source — contradicting the resume-only decision above and that same file's own v1.8 `x-changelog` entry, which already states resume-only correctly. This document states the shipped, tested behavior (`isBumpSource`, `internal/adapters/claude-code/hookrun.go`); the two schema property descriptions need the same one-word fix a future pass should make.

---

## 2. Per-type mapping (dev event → base wire event)

Built by `wireTypeFor` in `client/payload.go`; one table for every event type,
feeding one serializer.

| Dev `event_type` | Base wire `event_type` | `signal_name` | Activity fields | Key `metadata` | Core effect |
|---|---|---|---|---|---|
| `SessionStarted` | `WorkflowStarted` |; |; | `provider`, `tool_version`, `repo`, `cwd` | **create** session `(workflow_id, run_id, workflow_type)` (`storage_session.go`) |
| `SessionEnded` | `WorkflowCompleted` |; |; | `total_tokens`, `total_cost`, `duration_ms` | **terminal**; closes the session |
| `PromptSubmitted` | `SignalReceived` | `prompt_submitted` |; | `tokens`, `cost`, `model`, `prompt_source`?, `prompt_source_inferred`? | mid-session signal; a machine-injected turn (a task notification arriving as a prompt) is labelled here and withholds its text as the goal -- see the rule below |
| `CommitCreated` | `SignalReceived` | `commit_created` |; | `commit_sha`, `repo`, `branch` (FR-5) | mid-session signal; commit lineage |
| `Deploy` | `SignalReceived` | `deploy` |; | `deploy_id`, `commit_sha`, `repo`, `environment`, `deploy_did` (FR-6/7) | signal; deploy lineage |
| `ToolCall` | `ActivityStarted` |; | `activity_id`, `activity_type`, `activity_input` | `tool_name`, `tool_use_id`?, `agent_id`?, `agent_type`? | one `governance_events` row; pre-exec decision (OPA + Guardrails stage 0) |
| `ToolResult` | `ActivityCompleted` |; | `activity_id`, `activity_type`, `activity_output`, `duration_ms`, **`status`** | `tool_name`, `exit_code`?, `tool_use_id`?, `agent_id`?, `agent_type`?, `is_interrupt`?, `subagent_type`? | its **own** row, sharing the `activity_id`; independently evaluated (OPA + Guardrails stage 1). `status` drives `tool.<name>.success` and is copied to the row's `workflow_status` |
| `TurnStarted` | `ActivityStarted` |; | `activity_id`, `activity_type` (`llm_completion`) | `turn_index`, `agent_id`?, `agent_type`? | one row opening the turn. A HOOK turn carries no `activity_input`: its input is the prompt, which rides the `prompt_submitted` signal under the content gate. An IN-PATH lane's turn carries `activity_input.content`, the observed request body (§3) |
| `TurnCompleted` | `ActivityCompleted` |; | `activity_id`, `activity_type`, `activity_output`, `duration_ms` | `tokens`, `model`, `turn_index`, `agent_id`?, `agent_type`? | its **own** row, sharing the `activity_id`; carries the turn's model + four token counts, and under content capture the model's reply in `activity_output.content` (§"The turn pair"). **No `spans`, no `span_count`** |
| `SubagentStarted` | `SignalReceived` | `subagent_started` |; | `agent_id`, `agent_type` | mid-session signal; a subagent that spawns and does nothing is now visible |
| `PermissionDenied` | `SignalReceived` | `permission_denied` |; | `tool_name`, `tool_use_id`, `permission_mode`?, `denial_reason`? | mid-session signal: THAT a call was refused, which tool it was about, and; **v1.3, content-gated**; why. The provider's `reason` is free text, so it rides `content.signal_detail` → `metadata.denial_reason`, and since v1.9 into `signal_args.denial_reason` as well, where a policy can actually match it |
| `APIError` | `SignalReceived` | `api_error` |; | `error_type` (closed provider enum), `error_details`? | mid-session signal; a turn that ended in a provider error rather than an answer. `error_details` is the provider's free-text elaboration; **v1.3, content-gated**, beside the enum |
| `Setup` *(v1.8)* | `SignalReceived` | `setup` |; | `trigger` | mid-session signal; session bootstrap fired |
| `InstructionsLoaded` *(v1.8)* | `SignalReceived` | `instructions_loaded` |; | `file_path`, `memory_type`, `load_reason`, `trigger_file_path`, `parent_file_path`, `globs[]`? | mid-session signal; which memory file loaded and why |
| `UserPromptExpansion` *(v1.8)* | `SignalReceived` | `user_prompt_expansion` |; | `expansion_type`, `command_name`, `command_source` | mid-session signal; **structural-only (D1)**, no content ever — the pre-expansion prompt would duplicate `prompt_submitted` under a second key |
| `MessageDisplay` *(v1.8)* | `SignalReceived` | `message_display` |; | `turn_id`, `message_id`, `index`?, `final`? | mid-session signal; **structural-only**, `message_id` is not a transcript join key |
| `PermissionRequest` *(v1.8)* | `SignalReceived` | `permission_request` |; | `tool_name`, `permission_mode`? | mid-session signal; **content-gated** (`requested_tool_input`) — for an `Agent` request this is the whole subagent prompt (C7) |
| `PostToolBatch` *(v1.8)* | `SignalReceived` | `post_tool_batch` |; | `batch_size`, `batch_tool_use_ids[]`? | mid-session signal; **structural-only (D1)**, no `tool_calls[]` content |
| `Notification` *(v1.8)* | `SignalReceived` | `notification` |; | `notification_type`? | mid-session signal; **content-gated** (`notification_message`) |
| `TaskCreated` *(v1.8)* | `SignalReceived` | `task_created` |; | `task_id`, `teammate_name`?, `team_name`? | mid-session signal; **content-gated** (`task_subject`; the description is never sent) |
| `TaskCompleted` *(v1.8)* | `SignalReceived` | `task_completed` |; | `task_id`, `teammate_name`?, `team_name`? | mid-session signal; shares `task_subject` with `TaskCreated`, never paired as an Activity |
| `TeammateIdle` *(v1.8)* | `SignalReceived` | `teammate_idle` |; | `teammate_name`?, `team_name`? | mid-session signal |
| `ConfigChange` *(v1.8)* | `SignalReceived` | `config_change` |; | `source`?, `file_path`? | mid-session signal, and the one new hook that is **gated** (coverage.md §4); `source:policy_settings` is never gated |
| `CwdChanged` *(v1.8)* | `SignalReceived` | `cwd_changed` |; | `old_cwd`?, `new_cwd`? | mid-session signal; structural, always sent |
| `DirectoryAdded` *(v1.8)* | `SignalReceived` | `directory_added` |; | `directory`?, `source`? | mid-session signal; structural, always sent |
| `FileChanged` *(v1.8)* | `SignalReceived` | `file_changed` |; | `file_path`?, `event`? | mid-session signal; bounded watch list (coverage.md §3), never the file body |
| `WorktreeRemove` *(v1.8)* | `SignalReceived` | `worktree_remove` |; | `worktree_path`? | mid-session signal; unpaired — `WorktreeCreate` is refused, not missing (coverage.md §3) |
| `PreCompact` *(v1.8)* | `SignalReceived` | `pre_compact` |; | `trigger`? | mid-session signal; **content-gated** (`compact_instructions`, the user's `/compact <instructions>`) |
| `PostCompact` *(v1.8)* | `SignalReceived` | `post_compact` |; | `trigger`? | mid-session signal; **content-gated** (`compact_summary`) |
| `PreModelSwitch` *(v1.8)* | `SignalReceived` | `pre_model_switch` |; | `from_model`?, `to_model`?, `requested_model`?, `source`?, `cache_ttl`?, `pricing`?, `context_tokens`?, `prompt_cache_warm`?, `estimated_cache_write_usd`? | mid-session signal; never sets `ev.Model`, so it cannot contaminate the token-rollup `metadata.model` key |
| `PostModelSwitch` *(v1.8)* | `SignalReceived` | `post_model_switch` |; | same shape as `PreModelSwitch` | mid-session signal; **not** a pair with `PreModelSwitch` in either direction — no shared id |
| `Elicitation` *(v1.8)* | `SignalReceived` | `elicitation` |; | `mcp_server_name`?, `mode`?, `url`?, `elicitation_id`? | mid-session signal; **content-gated** (`elicitation_message`, the MCP server's prompt) |
| `ElicitationResult` *(v1.8)* | `SignalReceived` | `elicitation_result` |; | `mcp_server_name`?, `action`?, `mode`?, `elicitation_id`? | mid-session signal; **content-gated** (`elicitation_response`) — your answer, form values included; residual risk in [data-and-privacy.md](data-and-privacy.md) |

Two POSTs per tool call, as before; the count did not change, only the shape.
Two more per model turn, when usage capture is on.

**Every signal but `prompt_submitted` carries its payload in `signal_args`
(v1.9), and `prompt_submitted`'s `signal_args` is the goal. Core's gate is what
keeps the two apart. A machine-injected `prompt_submitted` -- the mapper's
`machineInjectedPrompt` classifying a task notification that arrived as a
prompt instead of typed input -- carries no `signal_args` at all:
`metadata.prompt_source` records the classification, but the text that would
otherwise have become the goal is withheld, not merely emptied.**

This reverses the rule v1.8 documented here, so it is worth stating what the old
constraint was and what actually removed it rather than quietly swapping the
sentence.

*The constraint.* Core's alignment engine treated *any* `SignalReceived` with
non-empty `signal_args` as a new user goal: it scored the assistant messages
accumulated so far against the previous goal, then overwrote the session's goal
with the stringified args. Two pieces of core made that unconditional —
`HandleSessionLifecycle` branched on `EventType` alone and never read
`signal_name` (`openbox-core internal/services/goal_alignment.go`), and
`stringifySignalArgs` (`goal_alignment_session.go`) turned any non-empty object
into text, falling back to the raw JSON. So a `post_tool_batch` signal would
have set the goal to `{"batch_size":1,…}`, and a live session carried ~140 such
signals in under 17 minutes. Under that core, the rule here was correct.

*What removed it.* Not a change of mind about the hazard — a change to core. Its
goal gate now suppresses goal creation for a signal whose `source` is
`developer-runtime` and whose `signal_name` is not `prompt_submitted`. Both keys
are checked, so every non-developer source behaves exactly as before. **That gate
is a precondition, not a footnote:** a client speaking 1.9 against an ungated
core reproduces the original defect in full. See the ordered rollout in the plan
that landed this.

*What the rule is now.* `signal_args` is the only field a governance engine reads
on a signal — OPA matches `signal_name` + `signal_args`, Guardrails read
`signal_args` and nothing else — so a class that carries none is unenforceable,
which is what the 21 v1.8 classes were. The projection is uniform and has no
per-class knowledge: `commit_created` and `deploy` lost their hand-written
lineage allowlists too, because a deploy's lineage is exactly what a deploy
policy matches on. `prompt_submitted`'s branch is untouched and byte-identical.

*What holds it.* `internal/client/signalvocabulary_test.go`'s
`TestEverySignalProjectsItsPayload` (count exactly 26),
`TestSignalArgsProjectionCoversEveryClass` over the whole vocabulary, the signal
golden fixtures, and conformance C38/C52. `TestSignalArgsProjectionDoesNotUseCoreGoalKeys`
pins the residual hazard: core's `stringifySignalArgs` prefers
`prompt`/`message`/`input`/`text`/`content` in that order, so a future mapper key
with one of those names would be read as a goal by any ungated core.

Availability note: these four hooks, and the 21 v1.8 classes above with them,
are registered by the installer, so an
**existing install does not emit them until `openbox init` is re-run** (the
dark-install window, D8; nothing schedules that re-run automatically). A Claude
Code version that does not know the hook keys never invokes them (verified); the
events are simply absent, fail-open.

> **An interrupted tool legitimately leaves one row.** A 630-event live session
> paired 242 of 244 `activity_id`s. The two exceptions were both `Bash`, both in
> auto permission mode, each with an `ActivityStarted` and a goal-alignment
> verdict and no `ActivityCompleted`. This is not an adapter gap:
> `PostToolUseFailure` **is** registered by the installer
> (`internal/adapters/claude-code/localhooks.go`, matcher `*`), and when it fires
> the two halves share the operation identity the `activity_id` is derived from
> (`TestAnInterruptedToolStillPairs`). The cause appears to be upstream — Claude
> Code 2.1.263 dispatches that hook under the same abort signal Esc trips and
> cancels it, so on an interrupt nothing reaches the spool. That mechanism was
> **read from the provider binary, not reproduced by a deliberate replay**, and a
> single lost delivery would produce the same shape; treat the cause as the
> leading explanation rather than a settled one.
>
> The practical rule is the same under either explanation, and it is a rule about
> how you *check*: count per `activity_id` and tolerate a Started with no
> Completed. **Never synthesize the missing Completed** — it would need an end
> time nobody observed and a status nobody reported, and it would make a real
> delivery drop indistinguishable from a user pressing Esc.

### Correlation metadata keys

`metadata` is deliberately a free-form object, so these are **well-known keys
rather than schema fields**; no `schema_version` bump, because the normalized
shape is unchanged and the version `const` marks breaking changes only. All are
structural identifiers (see [the Invariants
glossary](dev-event-contract.md#invariants) for INV-1/INV-2) and all
are optional; a provider that does not expose one simply omits it.

> **No governance engine evaluates `metadata`.** Verified: not OPA policy input
> (`opa.go` never reads it), not Guardrails (`BuildGuardrailInput` forwards
> `activity_input` / `signal_args` / `activity_output` only), not the alignment
> judge. Since v1.9 the keys below also ride `signal_args` on a signal row,
> which OPA and Guardrails *do* read. The duplication is deliberate: a move
> would have broken the SQL correlation these keys exist for. It is merged into
> the stored row and is therefore queryable in SQL, which is exactly what
> `credential_fingerprint` and `http_status` were rehomed here for -- forensics
> a reviewer can run after the fact. That was the stated purpose and it is met.
>
> **It is read outside the engines, and the earlier claim that it had no reader
> at all was false.** Core materialises `deploy_session_links` from
> `metadata.sessions[]` and reads `metadata.source` during goal alignment; the
> backend's lineage dashboard filters on `metadata->>'repo'` / `'commit_sha'`
> and builds the deploy DTO from those keys; the Verify tab renders the whole
> object; and compliance evidence exports list `governance_events.metadata` as
> a source. A reader after the fact is exactly what this field is for.
>
> The reason to write it down is the corollary: **`metadata` cannot be an
> enforcement surface.** A key a policy, a guardrail or the judge must
> *evaluate* belongs on `activity_input` / `activity_output` (or `signal_args`
> on a signal), which map to dedicated columns and which the engines actually
> read. A key a reviewer, a dashboard or a transcript view reads *after the
> fact* — a correlation id, a status code, a truncation note — belongs here:
> `credential_fingerprint`, `http_status`, the correlation keys below and the
> deploy lineage keys are all that class. Stop growing `metadata` *for
> enforcement*; a key added here expecting a policy to evaluate it will simply
> sit unread.
>
> **Reserved key names.** Core seeds its own verdict metadata — `event_type`,
> `workflow_id`, `trust_tier`, `post_attestation`, `policy_fallback_used`, plus
> `profile_id` and `source` — and then merges the client's keys skip-if-exists.
> But storage merges the client blob **first and unconditionally**, so a client
> key carrying one of those names *wins* in the stored row: a client-written
> `trust_tier` silently poisons the backend's tier filter. Never emit one of
> those names from an adapter or from `payload.go`.
>
> **Why a signal's own fields ride `metadata` — and, since v1.9, `signal_args` too (insight 6).** A `SignalReceived` row still has no `activity_input`/`activity_output` of its own; those columns exist only on the Activity carrier a tool call or a turn uses. That half of the original claim is not merely still true, it is enforced structurally: `internal/client/payload.go`'s activity guard nils both fields whenever `activity_id` is empty, and no signal sets one. Giving a signal an `activity_id` to reach those columns would break "every activity that *ran* carries exactly two rows", which is what makes `SignalReceived` legitimately unpaired — so that route is closed, not merely unattractive.
>
> What changed is "the only home". `signal_args` is a **second** home, and the better one, because the engines read it and never read `metadata`. Signal fields now ride both: `metadata` for the SQL forensics path, `signal_args` for enforcement. "Stop growing `metadata`" above stands as written — a key added to `metadata` *alone* still reaches no engine.

| Key | Providers | Meaning |
|---|---|---|
| `tool_use_id` | Claude Code, Codex | Per-invocation id for a `ToolCall`/`ToolResult` pair. It rides `span.invocation_id`, a *local* field (spooled, never emitted) that keys the cross-process duration stash. The wire pairing itself is `activity_id`. `span.function` is the MCP function name only. |
| `pair_recovered` | Claude Code, Codex | Set `true` on a `ToolResult` when the duration stash's started-half operation id differed from this event's own mapped one and was adopted onto it, so the call's two halves still share one `activity_id` instead of tearing in two. An MCP call's arguments can differ between the two hook processes that map its Pre and Post, which is when this fires. **Absent, never `false`,** when the ids matched or the stash held no record -- the `is_subagent` convention. Structural and ungated: in neither `contentMetadataKeys` nor core's goal-text keys, so `content_capture: false` does not strip it. Degrades: a payload with no `tool_use_id` at all (a legacy Claude Code build, or Codex before 0.145.0) keys the stash on the structural fallback `ToolCallStartKey`, which itself includes `operation_id`, so such a build can still split. |
| `prompt_id` (v1.8) | Claude Code | The UUID of the prompt being processed, threaded onto **every** event via `commonMetadata` (not just `PromptSubmitted`). Absent until the first user input, and absent entirely on Claude Code versions before 2.1.196. The `:otel:` lane sources the same key independently, from the record's `prompt.id` attribute, bounded to 256 bytes, on both halves of a model-call pair -- so its presence no longer implies the hook lane produced the event. The vendor documents the two as the same id, which is what makes the cross-lane join sound. **Since v1.9 the `:gateway:`/`:proxy:` lanes source it too** (`span.prompt_id`), parsed from the request's `x-anthropic-billing-header` system-block text (`cc_prompt_id`) rather than from a header despite the name -- a third independent producer of the same well-known key. |
| `previous_request_id` (v1.9) | Claude Code (`:gateway:`/`:proxy:` lanes) | The prior call's upstream `Request-Id`, from the same attribution block's `cc_prev_req`. Equals this lane's own `activity_id` suffix, so an ordering chain resolves to activity ids with no new correlation scheme. |
| `is_subagent` (v1.9) | Claude Code (`:gateway:`/`:proxy:` lanes) | From the same block's `cc_is_subagent`. **Absent, never `false`,** when the token is missing: set only when present and exactly `true`, because a UI would read `false` as "confirmed not a subagent". Pairs with `agent_id` (already promoted from `X-Claude-Code-Agent-Id`) so a consumer can label a subagent's turns; these ship flat, not nested. |
| `entrypoint` (v1.9) | Claude Code (`:gateway:`/`:proxy:` lanes) | From the same block's `cc_entrypoint` (cli / sdk / desktop; vendor-owned, not enumerated here). |
| `query_source` | Claude Code (`:otel:` lane) | The provider's own call-type discriminator on an OTel `api_request` record (`sdk`, `repl_main_thread`, `agent:custom`, `prompt_suggestion`, ...). Vendor-owned vocabulary with no fixed table: never allowlisted, an unrecognized value rides verbatim, bounded to 256 bytes. **Absence means unclassified** -- never inferred as conversation, never as sidecar, never defaulted. A consumer that reads a missing `query_source` as "background" would render a real message as a chore, which is the defect this key exists to prevent. |
| `task_id` (v1.8) | Claude Code | Correlates a `TaskCreated`/`TaskCompleted` pair; never an Activity pair (see §2's note on why an orphan would depress success rates). |
| `elicitation_id` (v1.8) | Claude Code | Correlates an `Elicitation`/`ElicitationResult` pair. |
| `turn_id`, `message_id` (v1.8) | Claude Code | `MessageDisplay`'s own correlation pair, streamed-line-batch scoped. Distinct from Codex's `turn_id` below: this one is **not** the API's `msg_…` transcript id, so it cannot join back to a specific stored assistant message. |

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
id, never content fields ([INV-2](dev-event-contract.md#invariants)).

> Conflating the two shipped and was found in a live session: every retry became
> a different activity, so an approver's decision could not be consumed, each
> retry filed a fresh request, and the rewake's "re-run to proceed" looped.
| `agent_id`, `agent_type` | Claude Code | Identify the subagent an event occurred inside. Present on *every* payload fired within a subagent, so the subagent tree is reconstructable from tool events alone; which is why the `SubagentStart`/`SubagentStop` boundary markers need no lifecycle type of their own (coverage.md §3.2). |
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

For a turn nobody observed the bytes of, that is the whole object: four numbers
and one bounded identifier.

**That is no longer a ceiling.** This object previously read "nothing else may
enter", and that property is deliberately given up. A turn now also carries what
the model said, under `content`, and its extended thinking, under `thinking`:

```json
{"model": "claude-opus-4-8",
 "usage": {"input_tokens": 1204, "output_tokens": 318,
           "cache_creation_input_tokens": 4096, "cache_read_input_tokens": 58210},
 "content": "…the provider's response, as the relay observed it…",
 "thinking": "…the turn's extended-thinking text…"}
```

Say the consequence plainly, because the old sentence existed to prevent exactly
this: core runs Guardrails stage 1 and OPA over `activity_output`, so **token
spend and model content now share one policy-visible object**. Both are evaluated
synchronously, inside the request the caller blocks on, with no server-side size
limit. That is why the bodies here are bounded in **bytes** rather than
characters (`maxModelCallBodyBytes`) -- the cost being managed is volume, and 64Ki
runes of CJK is 192 KB on the wire.

The reason the property was traded rather than kept: `spans[]` was the only other
home, and core discards it (see below). A bounded, policy-visible object that
holds the evidence beats a pristine one that holds none.

`activity_type` is `llm_completion` on **both** halves of a real completion. The
name is core's own (`internal/content/session.go:105`
`SemanticTypeLLMCompletion`), so one vocabulary spans both runtimes and the
core-side extractor keys on a name core already knows.

**It is no longer the only value a model-call event can carry**, and that is the
correction rather than an extension. The in-path lanes classified a relayed call
by HTTP method alone, so every POST to the intercepted host became
`llm_completion` -- and roughly **40 %** of those rows were
`POST /v1/messages/count_tokens` probes, which carry no usage, no assistant text
and no completion (~7,400 probes to ~10,600 completions in a five-day corpus).
They distorted every `llm_completion` metric, the turn count, and anything
reasoning about how many model calls a session made.

Classification is now from the captured **path**, not the method:

| Path | `activity_type` | Carries bodies | Emitted |
|---|---|---|---|
| `POST /v1/messages` | `llm_completion` | yes | yes |
| `POST /v1/messages/count_tokens` | `token_count` | **no** | **no** |
| `POST /api/…` (the tool reporting on itself to its vendor) | `tool_telemetry` | no | yes |
| anything else on the intercepted host | `provider_request` | yes | yes |

Three notes a reader will otherwise trip on. **`activity_type` is genuinely
ours**: core recomputes `semantic_type` and ignores what the client sends, but
`activity_type` is a pass-through column the dashboard reads first, so this table
is the authority and `client.AllActivityTypes` is its code twin. **An unknown
path is still emitted**, under `provider_request`, because unknown traffic to a
provider is exactly what an auditor wants to see and the failure directions are
not symmetric. **A `token_count` event carries no bodies at all, and now no
event at all** -- it holds the same conversation a completion does, minus the
reply, and the client fires one on every keystroke-triggered recount, so
attaching bodies there would egress the whole conversation repeatedly for an
event that describes no work; core also drops it unread on the other end
(`isRelayedNonToolActivity`), so a probe is classified for the classifier's own
correctness (`PathClass.Emits()`, `internal/cli/gatewayemit/pathclass.go`) and
then never spooled. That asymmetry is privacy-relevant and is a decision, not
an oversight.

Existing dashboards counting `llm_completion` will show a **step change** when
probes stop being counted. That is the correction, but it looks like a regression
to anyone watching the number rather than the definition. Rows already stored keep
`llm_completion`; no migration is in scope.

**`activity_id` is `<session_id>:turn:<index>`**, or
`<session_id>:agent:<agent_id>:turn:<index>` for a subagent's turn. Not a hash,
unlike the tool-call id: a turn has no operation to key on, is never approved
(so the id is not an approval key), and a readable id is worth having in stored
rows. It cannot collide with `cc-act-<32 hex>` by construction, and
`client/turn_key_pin_test.go` pins the bytes and the separation.

**Both halves are emitted from one firing**, on every lane, so the pair is atomic;
no orphan half, no cross-hook turn index to race, and queued prompts fold into one
turn. For the hook lane that firing is Claude Code's `Stop`; for the in-path and
telemetry lanes it is the single `Emit` that follows the observed call.

That was not true of the model-call lanes until this repair. **Every in-path row
was an `ActivityCompleted` with no `ActivityStarted`, and `duration_ms` was
null** -- the relay measured the call and threw the number away, computing the
elapsed time only to write it into a verbose log line. What a reader saw as "one
Started, many Completed" on the dashboard timeline was that. It went uncaught
because the live suite's pairing assertion filters every query to *tool* activity
types, so the `llm_completion` rows sat outside the only guard there was;
`internal/client/lifecyclepairing_test.go` is the session-wide twin that now holds
it without a live stack.

Two consequences of emitting the pair from one firing, both deliberate:

- The Started half is **retroactive** on the in-path lanes: the relay only knows the call is over when it is over, so the opening row is emitted with the request timestamp after the fact. That is correct for pairing and for `duration_ms`, and it is safe because core pairs a completion to its start by looking the `activity_id` up at ingest (`inheritRefusalFromStart` → `FindByWorkflowRunActivityID`), never by arrival order. The spool delivers in append order, so the start still lands first.
- `duration_ms` on an in-path row is the **relayed call's** latency, measured to end-of-stream rather than to response headers. A streamed completion runs for seconds after its headers, and `api_response_ms` -- the control plane's own response time, 536–725 ms -- sits next to it and is easily mistaken for it.

The Started half's timestamp is used to compute `duration_ms`; core derives
duration from `duration_ms` on the Completed half alone.

Codex reaches the same signal at session granularity: one pair at SessionEnd
with `activity_id <session_id>:usage:rollup`. Its `Stop` hook exists but is
deliberately unwired; scope, not impossibility.

>
> carries the full trade-off, including the one that matters: the transcript
> projection's [INV-2](dev-event-contract.md#invariants) guarantee is now a **curated allowlist enforced by a test**
> rather than a structural impossibility, because `message.model` is bound.

#### No spans at all, and why the previous answer was worse than useless

**A developer event carries no `spans[]` and no `span_count`.** Not one, not
under content capture, not on any lane. The subsection this replaces described,
in detail, the exact shape of a span that core throws away.

Core parses `spans[]` on the normal path and uses it in memory for policy,
behaviour and goal checks. It then **discards it.** Persistence into the `spans`
table is gated on `hook_trigger` plus a pre-existing event row
(`governance_workflow.go:234`; `HasNewSpans` is set only under
`input.IsHookEvent && input.HookSpanIdentity != nil`, `validation.go:154-174`),
and this client deliberately never sets `hook_trigger` -- doing so would put a
model turn on core's approval-bypass fingerprint path, which a turn is not an
approvable operation for. Otherwise the normal path runs: "Create governance
event only / no spans" (`governance_workflow.go:842-856`). The legacy
`governance_events.spans` jsonb column has **no writer anywhere in core**; the
`span_count: 0` seen on read is the backend recomputing it as a relation count
over the `spans` table (`governance-event.service.ts:63-69`), whatever the client
sent.

So every body ever sent in `spans[]` was parsed, used, and dropped, and every
assertion that one "reached the wire" was true and pointless.

There is a second, independent reason, and it would apply even if the array
persisted: **a span describes work nested INSIDE an activity.** In the AI-agent
runtime a generic activity wrapped an OpenAI call, so `llm_completion` appeared
there as a *span*, alongside `parse_json_response` and 400 `file.write` spans --
the inner steps of that activity. Here `llm_completion` **is** the activity. It
occupies that same slot and has no inner steps, so nesting a span inside it would
represent one model call twice.

What replaced each part:

| Was, in the span | Is now | Note |
|---|---|---|
| `response_body`, wrapped as `{"choices":[{"message":{"content":…}}]}` | `activity_output.content`, verbatim | the OpenAI-chat wrapper existed for one reader; see below |
| `request_body` | `activity_input.content` on the opening half | the key is `content` because Goal Alignment's per-operation cap orders keys by a fixed priority list and drops unlisted ones first, so `request_body` would have been the first thing discarded |
| `attributes["openbox.credential_fingerprint"]` | `metadata.credential_fingerprint`, ungated | a **rehoming, not a regression**: span attributes never persisted for a developer session, so account binding has never had anything to match on |
| `request_headers`, `response_headers` | nowhere | nothing read them, and they were the highest-risk class this client carried: the developer's live provider credential is on every model request |
| the synthesized `openbox.span_synthetic` attribute | nowhere | it marked the pair below as fabricated for core's per-span recompute; with no span there is nothing to classify, and nothing fabricated reaches a stored field either (next row) |
| the synthesized `http.method` / `http.url` | nowhere, for THIS lane | the telemetry lane observes no HTTP exchange and synthesizes both. They ride `activity_input` for the IN-PATH lanes, which really observed them -- but `turnActivityInput` emits nothing without a request body, and this lane has none, so a fabricated pair is never stored as an observation |
| `span_id` / `trace_id`, and core's `(span_id, stage)` dedupe | nowhere | `activity_id` is the pairing key now, and it always was for the activity rows |

**The retirement condition is met, by a different route than the one recorded.**
The synthesized attributes were an owner decision with a named condition: delete
them when the control plane moves assistant content onto the `llm_completion`
`activity_output`. The client moved the content instead, which makes the span
moot rather than premature. The old warning -- "removing them before that lands
kills the feature silently" -- was correct about the mechanism and is now spent.

**Goal Alignment still scores, but not by reading a relayed request body.**
Superseded, kept for history — verified on `openbox-core` `develop`,
2026-09-01: this section said the primary path was an `ActivityStarted`
carrying non-empty `activity_input`, resolved to a judgeable *operation*
(`goal_alignment.go:268` → `buildGoalOperation`), the span-based extractor
surviving only as a **fallback** (`:298`), and concluded "the request body on
the opening half feeds alignment natively." True of a tool call; the sentence
did not distinguish a relayed model-call row, where it is false.

**Verified on `openbox-core` `develop`, 2026-09-12** (`resolveAction`,
`goal_alignment.go:456-458`): a relayed `ActivityStarted` whose `activity_type`
is one of `llm_completion`, `provider_request`, `token_count`,
`tool_telemetry` is dropped by `isRelayedNonToolActivity` (`:554-580`)
**before** `buildGoalOperation` ever runs, so a relayed row's
`activity_input.content` — the window described below — is never opened for
judging. A tool call is unaffected: its `ActivityStarted` is never
relayed-non-tool, so `buildGoalOperation` still reads its `activity_input`
exactly as this section originally said. The matching `ActivityCompleted` of a
model turn is judged only when `activity_output.reply_text` is present
(`replyTextFromActivityOutput`, `:591-602`), a key this client sets on the
**hook lane only** (`modelCallReplyKey`, `internal/client/payload.go:884-911`,
from `mapper.go`'s `MapTurn`). So an in-path (`:gateway:`/`:proxy:`) turn is
judged on **neither** half — matching
[architecture.md](architecture.md)'s "a lane-observed turn contributes nothing
to goal alignment. Alignment for those turns comes from the hook path or not
at all," which this section now agrees with instead of contradicting.

This is the settled disposition of §3.7's proxy-lane finding — filed and
withdrawn twice, 2026-09-10 and 2026-09-12: a relayed model call is never
goal-judged **by design**. Do not re-file it.

How much text the judge keeps, where it does resolve an operation — a tool
call's `activity_input`, or the hook lane's reassembled `reply_text` — is
still governed by the same core-side cap: roughly **390-444 bytes**, kept as
**head 3/5 + tail 2/5** with the middle elided (`elideMiddle`,
`goal_alignment_session.go:773-786`). The window is core-side and not ours to
set, and a relayed row's window is simply never opened. What *is* ours is
which bytes would land in it, which is why the stored request document puts
`messages` last — still load-bearing for OPA, Guardrails and any human or UI
reader of `activity_input.content` on an in-path row, per
[The request window is a SELECTION](#the-request-window-is-a-selection-and-what-it-contains).

What alignment gives up, stated rather than glossed: a **relayed** turn is not
judged at all, on either half. A **hook-lane** turn is judged on its
reassembled `reply_text` (v1.9) instead of the model's reply riding a span,
which is what "operations, not spans" used to mean here. The operation
signal — a tool call's `activity_input` — is the one path untouched by any of
this, since a tool call is never a relayed-non-tool activity.

### `semantic_type`: computed for nothing, because nothing sends a span

Core derives `SpanData.semantic_type` from a span's source fields
(`ComputeSemanticTypeFromSpan`, `internal/content/session.go:204`), per span,
overwriting whatever was sent. **No developer event sends a span, so core computes
this for nothing.** `tool.kind`, the `activity_input` locators and `activity_type`
are what a consumer classifies on instead.

`DevEvent.Span.semantic_type` remains part of the adapter-facing contract and
adapters still set it; it is a local field that never reaches the wire.

mapping.md previously carried an "an earlier decision dependency (server-side, pending)" claim
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
changed is that `client/payload.go` reads locators, counts **and bodies** *out* of
it into `activity_input`/`activity_output` instead of serializing it as a span.
No event carries `spans[]` at all -- see §2 for why the previous answer was worse
than useless.

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
| `content.tool_input` | `activity_input.command` / `.arguments` / `.content` | started | content-gated, `capBody`-capped. **v1.3: observe path too**, not gated calls only. **v1.8:** an `Agent`/`ToolSearch` spawn routes to `.arguments`, never `.command`, regardless of its `shell` kind (see §1) |
| `content.tool_output` | `activity_output.output` | completed | **v1.3**; content-gated, redacted before attach, `capBody`-capped. A failed call carries its error text here |
| `content.thinking` | `activity_output.thinking` | completed | **v1.4**, turns only; content-gated, redacted before attach, `capBody`-capped. Sourced from the transcript window, not a hook field |
| `span.invocation_id` |; (local) |; | feeds the duration-stash key; never a wire field |
| `span.operation_id` |; (local) |; | feeds `activity_id`; never a wire field |
| `span.semantic_type` |; |; | the client has never sent this field, and there is no wire span to recompute it from either (§2) |
| `span.stage` |; |; | **retained, read by nothing on the wire.** Kept deliberately: the adapter contract is frozen, adapters still set it, and it now says which half of a pair a local event is, which a reader of the spool wants. |
| `span.module` |; |; | never had a wire home |
| `span.request_body` | `activity_input.content` | started | **In-path lanes only.** No hook adapter sets it, and none may; that is what §1's "span-less" means. The observed model REQUEST, content-gated by `stripContent`. **SELECTED, not truncated** -- see the note below; `capModelCallRequest` (which keeps the TAIL) remains the net but the wired path no longer reaches its cut. The key is `content` and not `request_body` because the alignment judge's per-operation cap orders by a fixed priority list and drops unlisted keys first — still the reason for the name, though verified 2026-09-12 that a relayed row's `activity_input` never reaches that cap to be dropped from at all (`isRelayedNonToolActivity`, `goal_alignment.go:456-458`); the naming protects the tool-call operations that do |
| `span.response_body` | `activity_output.content` | completed | **In-path lanes only.** The observed model RESPONSE, verbatim SSE frames and all, content-gated and capped by `capModelCallBody` |
| `span.http_status` | `metadata.http_status` | completed | structural, ungated, **absent when no response was observed at all** -- a relayed call whose transport failed before one existed. Rehomed alongside `credential_fingerprint` for the same reason: without it a 5xx stores identically to a success whose reply was not captured. "completed" here is now enforced rather than described: it shipped on **both** halves until v1.8, and 116 of 116 live `ActivityStarted` rows asserted `200` on a request nothing had answered yet. `observesAResponse` bounds it, and it bounds the metadata KEY as well as the span field, so an adapter cannot reinstate the assertion by writing `http_status` itself |
| *(none -- client-synthesized from every wire value this payload knows to be incomplete on egress)* | `metadata.openbox_capture.truncated_paths` | every event | Sorted array of destination-qualified wire paths (e.g. `activity_output.output`, `metadata.denial_reason`, `signal_args.prompt`) naming every value known-incomplete on egress: every `capBodyInto` cut, every `capModelCallBody` cut on a model-call row's `content`/`reply_text` keys (since v1.9 -- these previously bypassed this summary entirely and left it silent about the single largest thing a relayed call's row cut), and every gateway-observed truncation (`span.response_truncated`, since v1.9 -- a gateway cut whose stored buffer lands at or under this client's own byte cap left `clientCut` false and the index silent even though the row's own `truncated: true` already admitted the loss). **Omitted entirely when nothing was cut** -- absence means "nothing truncated", never an empty array. The same value cut into two wire objects (the `eventMetadataForEgress` backstop runs once for `metadata` and once for `signal_args`) yields **two** entries; no dedupe. Structural: deliberately **not** in `contentMetadataKeys`, so the content gate never drops it and the dynamic backstop can never re-cap it (the object is appended after that loop has returned). The field is `truncated_paths`, not `truncated`, because `activity_output.openbox_capture.truncated` is a **bool** about one response body's completeness -- a different, now-answered question; see the row below. The gateway's request-side window selection (`openbox_selection`, in the request-window table below) is also a known-incomplete value, but is reported in-band inside `activity_input.content` itself rather than indexed here -- deliberate, not an oversight |
| `span.response_truncated` | `activity_output.openbox_capture` | completed | v1.9. `{truncated, original_bytes}`, the response-side twin of `openbox_selection` (below): **`truncated: false` is emitted, never omitted** -- that is the "this reply is complete" signal a UI needs. `truncated` ORs the gateway's own signal with this client's `capModelCallBody` cut (`len(span.response_body) > 65,536` bytes), so it is true if either side observed loss; `original_bytes` is `len(span.response_body)` -- the decoded body's own length. **Never** `span.response_bytes_seen`: that counter is the gateway sink's own, measured off the wire *before* decoding, and ~98% of real replies are brotli/gzip (`internal/gateway/decode.go`), so it is a compressed byte count and is not comparable to the decoded `response_body` it would claim to bound -- it is routinely SMALLER than the content beside it. `len(span.response_body)` is exact for a complete reply and an honest lower bound for one the gateway or this client's `capModelCallBody` cut, including a compressed body that failed to decode into a marker, where the marker's own length is reported because the true original size is unknowable. Present only alongside `activity_output.content` (the `span.response_body` arm) -- absent under `content_capture:false` along with the body it describes, and on a hook-sourced turn, which has no gateway sink to report from. Rides `activity_output`, so Guardrails stage 1 reads it and the alignment judge's per-operation cap drops it first (an unlisted key, same as `bytes_read`/`bytes_written` above) -- harmless, a bool and an int, but a reason NOT to "harmonise" it into `metadata` beside the sibling `truncated_paths` summary, or vice versa. See the in-path-lane-only table below for what feeds each Span field |

#### The request window is a SELECTION, and what it contains

Until v1.8 a stored model-call request body was **a constant**. Three caps
composed head/head/tail -- `capturableBody` head-cut 256 KiB, `capRunes` head-cut
65,536 **runes**, then `capModelCallRequest` tail-cut 65,536 **bytes** -- so the net
was the *tail of the head*: exactly 65,536 bytes on all 27 measured rows, dropping
only the first few hundred. Since the head is the part that does not change between
calls, 28 calls stored 4 distinct bodies and `"messages"` appeared in 0 of 27. The
truncation marker stated the opposite of what happened: it claimed the start was
dropped, when the END had gone two layers earlier. And for an all-ASCII body,
65,536 runes equals 65,536 bytes, so the byte cap returned its input unchanged and
**no marker appeared at all** -- a head window presented as a complete body.

No byte window could have fixed it. Measured over the recorded corpus: `messages`
is p50 **82.1%** of a request body, **95.3%** of bodies exceed 64 KiB, and key
order varies (65.5% put `messages` second with `tools` trailing, 27.1% put it
after `system`). So neither end reliably contains the newest turn.

`internal/gateway` therefore **selects**. It binds tolerantly -- one key name on a
top-level object, never a schema -- and stores a synthetic document:

| Field | What |
|---|---|
| `model` | verbatim |
| `system` | head window, ~4 KiB, re-encoded with an elision note (a truncated `RawMessage` would not parse) |
| `openbox_selection` | `{dropped_messages, skipped_trailing_non_turns, dropped_keys, original_bytes}`; absent when nothing was dropped |
| `messages` | **last**, the newest entries greedily from the end until the budget |

`tools` is dropped entirely -- it is the boilerplate that was being stored on every
call, and dropping it is the one change here that *reduces* egress.

Two properties are load-bearing and easy to undo by accident:

- **`messages` is last because the document is a Go struct, not a map.** `json.Marshal` sorts a map's keys, which would emit `messages` first, into the exact middle core's `elideMiddle` discards (it keeps head 3/5 + tail 2/5 of a ~390-444 byte window, `goal_alignment_session.go:773-786`) **on a row that reaches it**. Declaring it last still puts the newest turn in the tail — read by OPA, Guardrails and any human or UI viewer of `activity_input.content` on every in-path row; the alignment judge itself opens that tail only on the hook lane's own operations (verified 2026-09-12: a relayed row's `activity_input` is dropped before `buildGoalOperation`, `goal_alignment.go:456-458`, `:554-580`). The ordering is unchanged and still what every one of those readers needs.
- **The selection budget (48 KiB) sits deliberately below the 65,536-byte net.** Redaction runs *after* selection and can grow a body, and one byte of growth hands `capRunes` a head cut that removes the newest turn again. The headroom is pinned by a test against a secret-dense body, not left as arithmetic.
- **Trailing non-turns are dropped, so the judged tail carries a turn.** The agent runtime appends elements to `messages` that are not conversation -- most often a 51-132 byte `role:"system"` element holding `<total_tokens>N tokens left</total_tokens>`. Under the belief that a relayed row's tail fed the judge directly, that would have been the current goal on **67%** of measured calls; verified 2026-09-12 that no relayed row reaches the judge at all (`isRelayedNonToolActivity`, `goal_alignment.go:456-458`), so what actually reads the LAST element on those rows is OPA, Guardrails and any human or UI viewer of `activity_input.content` — the same readers the walk-back below still serves. Selection now walks back past trailing elements whose `role` is neither `user` nor `assistant` -- at most 4, never emptying the history -- and reports the count under its own `skipped_trailing_non_turns` key rather than folding it into `dropped_messages`: a budget drop and a shape problem are different losses. The predicate reads `role` and **never content**, because the same `<total_tokens>` marker also rides inside real turns (`assistant` 81, `user` 59 of 1,144 measured occurrences), so a content match destroys 140 genuine turns to catch the synthetic ones. A `role:"system"` element inside `messages` is a **client artifact** -- the Anthropic Messages API carries `system` as a top-level field -- and no contract this repo publishes defines it.

Anything that is not a conversation -- non-JSON, a missing or non-array `messages`,
a truncated body -- degrades to a tail window carrying a marker that names the
*selector*, so it can be told apart from a downstream truncation. It never errors:
this runs in the request path of the component that must not break the tool.

**A fallback window has two possible sources, and the marker names which.** Where
the selector could not bind at all -- non-JSON, no `messages`, a non-array
`messages`, or a budget it could not establish -- no document exists, so the window
is cut from the **raw body**. Where a document *was* built and then exceeded the
budget, the window is cut from **that document**, whose `tools` is already dropped
and whose `system` is already capped. That distinction is not cosmetic: windowing
the raw body there stored tool boilerplate and zero conversation on **26.5%** of
measured model calls, because 65.5% of bodies put `tools` after `messages`. The
two cases carry **different reason strings**, and rows stored before this change
keep the older wording, so an investigation can tell a window over boilerplate
from a window over conversation without dating the row.

The over-budget encode that produced those fallbacks is also gone. A message
larger than the whole budget is re-encoded as a marked JSON string, and that
re-encode is now **sized by measurement** rather than by a fixed allowance:
`json.Marshal` expands `"` and `\` 2x and a literal `<`, `>`, `&` or control byte
6x, so the previous 16-byte margin was outrun by any escaping-heavy message -- an
HTML paste is enough -- and the element came back larger than the room it had been
measured against. Sizing for the 6x worst case instead would divide the kept tail
by six on every ordinary body to serve the pathological one.

**In-path-lane-only span fields** (v1.5, rehomed in v1.7). Set by the gateway and
transport lanes; a hook event carries none of them. They used to ride
`spans[]`, which core discards, so none of them was ever stored -- v1.7 moved the
ones worth keeping onto fields that persist and dropped the rest.

| `DevEvent` field | Wire home | Gated | Notes |
|---|---|---|---|
| `span.request_headers` | ; | ; | **No longer emitted.** They reached core only inside `spans[]`, so nothing ever read them, and they were the highest-risk class this client carried: the developer's live provider credential is on every model request. Still declared on the adapter-facing contract and still redacted by key name at capture; they simply do not leave the machine |
| `span.response_headers` | ; | ; | same |
| `span.http_method` | `activity_input.http_method` | no | structural, and a COMPANION to the body rather than an operation of its own: `turnActivityInput` returns nothing at all when there is no `request_body`, because a non-empty `activity_input` is what makes core judge an operation, and a probe or a synthesized record would otherwise ship one describing no work |
| `span.http_url` | `activity_input.http_url` | no | structural, **query dropped**, and gated on a body for the same reason as `http_method` |
| `span.http_status` | `metadata.http_status` | no | see the row above; ungated because a status code is not content, and **completed-half only** |
| `span.credential_fingerprint` | `metadata.credential_fingerprint` | no | Rehomed in v1.7. Core has no span field for it and never stored the span anyway, so account binding has in fact never had anything to match on; `metadata` is merged into the stored row. Ungated because a privacy switch must not let an org opt out of being identified |
| `span.prompt_id` | `metadata.prompt_id` | no | v1.9. Parsed from the request's `x-anthropic-billing-header` system-block text (`cc_prompt_id`) -- a `system[]` TEXT ELEMENT, not a header despite the name (`internal/cli/gatewayemit/attribution.go`). Same wire key as the `:otel:` lane's `prompt.id` promotion, so both producers group a session under one name. Absent when the source token is absent -- unclassified, never inferred |
| `span.previous_request_id` | `metadata.previous_request_id` | no | v1.9. From the same block's `cc_prev_req`, which equals the prior call's upstream `Request-Id` -- this lane's own `activity_id` suffix -- so the chain resolves to activity ids with no new correlation scheme |
| `span.is_subagent` | `metadata.is_subagent` | no | v1.9. From the same block's `cc_is_subagent`. **Absent, never `false`,** when the token is missing. Ships flat, not nested: pairs with `metadata.agent_id` so a consumer can label a subagent's turns without the producer binding parent/child nesting |
| `span.entrypoint` | `metadata.entrypoint` | no | v1.9. From the same block's `cc_entrypoint` (cli / sdk / desktop; vendor-owned, not enumerated) |
| `span.response_bytes_seen` |; |; | v1.9. **Not projected to any wire field.** The gateway capture sink's own byte counter, incremented on every `Write` before its bound and clamp -- but counted off the wire *before* decoding, so for ~98% of real replies it is a compressed byte count and is not comparable to the decoded `response_body` that `original_bytes` describes (see the note row above). Retained because it is a true fact about the relay that a length check on the stored body cannot recover |
| `span.response_truncated` | `activity_output.openbox_capture.truncated` | no | v1.9. `true` when **any** of: the upstream connection broke before the reply finished (a read error on the relayed stream); the connection to the developer's own tool broke before the relay finished writing (a downstream write failure, carried as a flag on the capture sink rather than as an error, so it cannot be mistaken for a relay failure); the sink dropped bytes past its own bound; or the stored body already carries the capture path's cut mark (`decodeCapturable`'s compressed-body bound, or `capRunes`' rune bound) -- either means the gateway's own copy was incomplete before this client ever saw it. ORed with this client's own `capModelCallBody` cut before it reaches the wire. **`false` is emitted, never omitted** |

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
  came back with v1.2 on `client.wireSpan`, and went again with v1.7: that type
  and its one content-gated turn span are deleted, and no event carries them.
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
→ default ALLOW). Phase-1 **observe** (D7/[INV-3](dev-event-contract.md#invariants)) treats every async-egress
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
| Session store (`storage_session.go`) | `WorkflowStarted`→create, `WorkflowCompleted`→terminal; **native**, no EXT-core lifecycle edit. |
| Accept-list (`internal/api/governance.go:273-286`) | All five types we emit are accept-listed, `ActivityCompleted` included; no core patch. |
| Idempotency / dedupe (`activities/governance/validation.go:96`) | Keyed on `(agent_id, workflow_id, run_id, activity_id, event_type)`. Because `event_type` is in the key, a tool call's two halves are now **distinct** events. Under the hook shape they matched on all five; same `activity_id`, both `ActivityStarted`; so the `ToolResult` POST hit the existing-event branch (`governance_workflow.go:228-231`) and was substantially a no-op. A **retry** of the same half still dedupes correctly, which is the behavior you want. |
| OPA policy eval (`opa.go`) | Bypassed (auto-allow) **only** for `Workflow*` (latency). `ActivityStarted`, **`ActivityCompleted`** and `SignalReceived` all go through **real** OPA; so the completed half is now independently evaluated, where the dedupe collision above meant it previously returned the started half's cached verdict. |
| Guardrails eligibility (`governance_workflow.go:429-431`) | Both activity types are guardrails-**eligible**: stage 0 reads `activity_input` (`guardrail.go:180`), stage 1 reads `activity_output` (`guardrail.go:192`). Structural fields only by default ([INV-2](dev-event-contract.md#invariants)). |
| Row fields (`storage_event.go:258-294`) | `activity_id`/`activity_type`/`attempt`/`activity_input`→`input`/`activity_output`→`output`/`duration_ms` are set **event-type-agnostically**, so the completed half's duration and output land with no core change. Note `payload.Error` is read **only** for `WorkflowFailed`, which is why the client sends none; see §3. |
| `signal_name` / `workflow_type` | Stored in dedicated columns; commit/deploy lineage rides `metadata` (core has no `commit_sha`/`deploy_id` columns) and **survives** the Signal mapping. |
| Spans table | **One row per content-capturing model turn, and nothing else**. Tool and lifecycle events remain span-less; the trade-off (no span-level Merkle leaves, no `semantic_type`) still holds for them. For the turn span, span-level Merkle leaves and server-side classification come back, and the assistant's text is stored server-side: a real retention increase, outside this repo's control. With `content_capture:false` there are no span rows at all. |
| Goal alignment (`goal_alignment.go`, `goal_alignment_session.go`) | `prompt_submitted`'s `signal_args` CREATE the session's goal (`HandleSessionLifecycle`). Any *other* `SignalReceived` with non-empty `signal_args` used to OVERWRITE it — `stringifySignalArgs` stringifies anything and `CreateSession` was unconditional — which is why every signal but `prompt_submitted` carried none before v1.9. Core's goal gate now suppresses that for `source == developer-runtime` with a non-`prompt_submitted` name, and only then is the v1.9 projection safe. **The predecessor path `internal/services/age.go` no longer exists; do not cite it.** Assistant text no longer comes from `payload.Spans` at all — core parses `spans[]` and discards it, and this client sends none. A turn is judged from `activity_output.reply_text`, which core keys on the PRESENCE of with no fallback to `activity_output.content`; this client emits it as of v1.9, on the hook lane only, so before then no assistant turn had ever been judged. Requires `LlamaFirewallHost` set (`llama_firewall.go:31-34`) and Redis up; **without either, both widgets stay empty with a perfect client**. |
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

**One distinction is load-bearing in the tables below.** Some rows have moved out
of the pending list because reading `openbox-core` settled them; those are marked
**answered by reading source**, and that is not the same as observed. Nothing here
is marked observed without a live run.

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

### Spans; every row here is now ANSWERED, and most of the answers are "no"

These moved out of the pending list without a live run, because reading core's
source settles them and the answer is that the question was wrong. A row whose
answer is "no" is more valuable than one quietly deleted, so each is kept with
what closed it.

| # | Was pending | Answer |
|---|---|---|
| 3, 17 | Zero span rows for a tool call; exactly one `span_type='llm_completion'` per captured turn | **No -- zero either way.** Core parses `spans[]` on the normal path and discards it: persistence is gated on `hook_trigger` plus a pre-existing event row (`governance_workflow.go:234`, `validation.go:154-174`), which this client deliberately never sets. The client now sends no span at all (§2). |
| 23 | Thinking is absent from the turn's span while present on the activity | **Moot, and the property survives.** There is no span. Thinking and the reply text are separate keys under `activity_output` (`thinking`, `content`) for the original reason: a reader that conflates chain-of-thought with the answer is corrupted silently. |
| 25 | A gateway span stores as its own row with the observed request and response | **No.** Same gate as row 3. The bodies now ride `activity_input` / `activity_output`, which map to dedicated columns and round-trip. |
| 26 | `attributes["openbox.credential_fingerprint"]` survives ingest | **No, and it never could have.** Span attributes never persisted for a developer session, so account binding has never had anything to match on. Rehomed to `metadata.credential_fingerprint`, which core does merge into the stored row (`storage_event.go` `setMetadataField`). A live run must still confirm the fingerprint is readable **there**. |
| 28 | `semantic_type` recomputes to `llm_completion` from the synthesized `http.*` attributes | **Moot.** No span ships, so core computes nothing; the synthesized attributes are deleted and their named retirement condition is spent (§2). |
| 29 | A capture-off gateway span still stores, carrying only structural evidence | **No.** Nothing stores. The structural evidence that mattered -- the fingerprint, the method, the URL, the status -- is on the activity rows instead, and the fingerprint stays ungated. |

### Still pending on the model-call rows

| # | What a run must confirm |
|---|---|
| 40 | The provider **response body** is readable on a stored `ActivityCompleted`, decompressed and redacted, rather than the 88-byte placeholder every capture held before |
| 41 | The **request body** is readable on the paired `ActivityStarted`, under `content`, capped by the gateway's own selection budget — not by the alignment judge, which never opens a relayed row (verified 2026-09-12, see §"Goal Alignment still scores") |
| 42 | `aligned_goal_evaluations` still advances with no span anywhere: on the **hook lane**, alignment feeds from `activity_input` on a tool call's opening half (`buildGoalOperation`, `goal_alignment.go:460-464`) — a relayed model-call `ActivityStarted` is dropped before it gets there instead (`isRelayedNonToolActivity`, `:456-458`, `:554-580`; verified 2026-09-12) — and a field name that loses the cap fails **here and nowhere else** |
| 43 | `api_response_ms` stays inside budget with a 64 KB body attached. Baseline is 536–725 ms; both OPA and Guardrails read `activity_output` synchronously inside a 30 s envelope the caller blocks on, expanding every string leaf, with no server-side size limit |
| 44 | Session-wide lifecycle pairing holds on a real session: `select activity_id, count(*) … group by activity_id having count(*) <> 2` returns zero rows |
| 45 | No stored row has `activity_type: llm_completion` with a `count_tokens` URL, and no `token_count` row is stored at all for the session -- the probe is classified (`PathClass.Emits()` in `internal/cli/gatewayemit/pathclass.go`) but never spooled; the completion-to-probe ratio is unverifiable as an observable of this release since other landings in the same release add rows, so a run reports the net per-session volume range instead |

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

> **`activity_input` is the enforcement surface for a developer session.
> `input.spans` is agent-runtime-only.**
>
> Core's OPA input carries both: `addSpansToInput` fills `input.spans` from
> `payload.Spans` (`opa.go:610`) and `addActivityInputOutput` fills
> `input.activity_input` (`opa.go:744`). This client sends no `spans[]` at all --
> see §"No spans at all" -- so for a dev session `input.spans` is *absent*, not
> empty, and any rule matching `input.spans[_]` is permanently undefined. An
> undefined path is fail-safe in Rego, so such a rule raises nothing and blocks
> nothing; it is silently inert.
>
> That matters because all 30 seeded `sl-*` control templates were authored
> against `input.spans[_]` -- 104 conditions over exactly three paths
> (`attributes.command` 71, `file_path` 21, `file_operation` 12). Written down here
> because the failure is invisible from this side of the wire: nothing in this
> repository can detect it, and the three key names the re-pointed paths depend on
> are pinned by `internal/client/enforcementkeys_test.go` for exactly that reason.
> A rename there disarms up to 17 templates with every other test still green.
>
> Coverage consequence, stated rather than inferred: `command` is gated content,
> while `file_path` and `file_operation` are structural. An org that turns
> `content_capture` off therefore keeps the file-path controls and loses every
> command-matching control. See `docs/data-and-privacy.md`.

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
| 24, 30, 39 | **Partly measured, and the estimate held.** `TestTransportSoakMeasuresSpoolCostPerModelCall` reports **66,973 bytes of spool per model call**, projecting **~319 MB per session** at the corpus's ~5,000 calls, against the 69,179 estimated here. Two changes pushed in opposite directions and roughly cancelled: a response body that used to be an 88-byte placeholder is now up to 64 KB of real text, while a request body that is capped at the same size rides only ONE of the two rows a call now spools. This is throughput, not residency: delivery is self-triggering, so the spool is an empty queue in steady state and the number matters only when delivery fails. What a live run must still confirm is the **residency** at real cadence |
