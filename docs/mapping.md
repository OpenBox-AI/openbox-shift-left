# Mapping: normalized dev event → openbox-core wire payload

Who this is for: someone integrating with or operating OpenBox who needs to
know how a field this program emits lands in core's tables. Read
[architecture.md](architecture.md) and [dev-event-contract.md](dev-event-contract.md)
first; nothing here is needed to set a machine up.

**Contract:** [`api/dev-event.schema.json`](../api/dev-event.schema.json) is the
adapter-facing shape every provider adapter (`internal/adapters/*`) produces.
**Wire model:** `internal/client/payload.go`'s `buildPayload` re-expresses that
shape onto the base SDK's five `event_type`s —
`WorkflowStarted` / `WorkflowCompleted` / `SignalReceived` / `ActivityStarted` /
`ActivityCompleted` — and POSTs it to `/api/v3/governance/evaluate`. A
developer event never carries `spans[]`: core parses that array for in-request
decisions and then discards it (persistence needs a `hook_trigger` this client
never sets), so nothing sent there was ever stored, and this client stopped
sending it.

**Contract versions:** `schema_version` (currently `"1.10"`) is a `const` in
the schema and is the authority on the adapter-facing shape; see the schema's
own `x-changelog` for what each version added. This document describes the
current wire shape only.

---

## 1. Envelope field mapping (every event)

Cited to `governanceEventPayload` in `internal/client/payload.go`.

| Dev event field | Wire field | Note |
|---|---|---|
| *(constant)* | `source` = `"developer-runtime"` | distinguishes dev traffic from the base SDK's own `"workflow-telemetry"` |
| `event_type` | `event_type` | re-mapped, not passed through, to one of the five wire types — see §2 |
| `openbox_session_id` | `run_id` (generation 0) | the session key is `(workflow_id, run_id, workflow_type)` |
| `run_id` (adapter-set, only on a `--resume`-continued run) | `run_id` (overrides the session id) | `runIDFor` prefers this field; absent at generation 0, where the session id doubles as the run id |
| `run_generation` | `run_generation` | 0 for the original run; an ordering hint, never a key |
| `continued_from_run_id` | `continued_from_run_id` | set only on the `WorkflowStarted` of a continued run |
| `developer_did` (or workspace id) | `workflow_id` | stable per-workspace identity; unchanged across a continued run |
| *(constant)* | `workflow_type` = `"developer-session"` | required on `Workflow*`/`SignalReceived`; keeps a session's whole event tree on one identity |
| *(per signal)* | `signal_name` | set, and required, only on `SignalReceived` |
| *(activity events)* | `activity_id` | pairs a tool call's or turn's two halves onto one row; for a tool call it also doubles as the approval key — see "Operation vs invocation identity" in §2 |
| `tool.name` | `activity_type` | the dashboard's Activity column; a lifecycle event carries its `event_type` string instead |
| `tool.*`, `span.*` (started half) | `activity_input` | structural locators by default; content only under content capture — see §3 |
| `span.*` (completed half) | `activity_output` | counts by default; content only under content capture — see §3 |
| `started_at` → `ended_at` | `duration_ms` | client-computed float milliseconds; omitted, never zero, when unknown |
| `timestamp` | `timestamp` | RFC3339 string, passed through verbatim |
| `metadata` | `metadata` | merged with per-type keys — see §2 |
| `status` | `status` | `ToolResult` only, `completed`\|`failed`; the only input to core's per-tool success metric |
| `tokens`, `cost`, `model` | `metadata.tokens` / `.cost` / `.model` | no first-class fields; a turn's completed half also carries model + counts in `activity_output` |
| `developer_did` | *(not a body field)* | a workload agent authenticates with a bearer token (§5), not a signed identity header; `workflow_id` is derived in memory and never carried as a credential |
| `span` | *(not serialized)* | the carrier the client reads locators, counts and bodies out of; never itself emitted, on any event |
| `content.output` (turn) | `activity_output.content`, gated | the assistant's reply text |
| `content.prompt` | `signal_args.prompt`, gated, capped | stripped when content capture is off |
| `content.tool_input` | `activity_input.command` / `.arguments` / `.content`, gated | key chosen by tool kind (`contentKeyFor`); an `Agent`/`ToolSearch` spawn always lands under `.arguments`, never `.command`, because `command` is what shell-matching policy rules read |
| `content.tool_output` | `activity_output.output`, gated | what the tool produced, or its own error text on a failed call |
| `content.thinking` (turn) | `activity_output.thinking`, gated | kept apart from `.content` so a policy never scores chain-of-thought as the reply |
| `content.signal_detail` | `metadata.<per-class key>` and `signal_args.<same key>`, gated | key chosen per event type (`signalDetailKeyFor`) |
| `span.request_body` / `.response_body` (in-path lanes only) | `activity_input.content` / `activity_output.content`, gated | see §3 |
| `span.request_headers` / `.response_headers` | *(nowhere)* | never egresses; the developer's live provider credential rides every model request, so these are redacted by key name at capture regardless |
| `span.credential_fingerprint` | `metadata.credential_fingerprint`, ungated | one-way derived account-binding evidence, not privacy content |

`schema_version` and `event_id` are contract/idempotency fields; `event_id` is
the client's own dedupe key and is never a core payload field.

**Run identity.** Only `--resume` continues a run: same session id, a fresh
run id, `continued_from_run_id` naming the sealed run it continues. `/clear`,
startup, compact and fork do not continue a run — `/clear` and fork mint a new
session id outright. Every `SessionEnd` seals its run the same way regardless
of why the session ended; there is no "suspended" state. See
[coverage.md §1a](coverage.md#1a-clear-and---resume-are-different-and-only-one-continues-a-run)
for the consequence: goal alignment resets on `/clear` but carries forward on
`--resume`.

---

## 2. Per-type mapping (dev event → base wire event)

Built by `wireTypeFor` in `internal/client/payload.go`.

| Dev `event_type` | Wire `event_type` | `signal_name` / `activity_type` | Notes |
|---|---|---|---|
| `SessionStarted` | `WorkflowStarted` | — | creates the session `(workflow_id, run_id, workflow_type)` |
| `SessionEnded` | `WorkflowCompleted` | — | closes it; `metadata.total_tokens`/`.total_cost`/`.duration_ms` |
| `PromptSubmitted` | `SignalReceived` | `prompt_submitted` | its `signal_args` **is** the session's goal — see "Signal payload" below |
| `CommitCreated` | `SignalReceived` | `commit_created` | first producer is the `git post-commit` hook, only where the git hook is installed, agent commits only; `metadata.commit_sha`/`.tree_sha`/`.parent_shas`/`.repo`/`.branch`/`.patch_id`/`.openbox_session_id` |
| `Deploy` | `SignalReceived` | `deploy` | `metadata.deploy_id`/`.commit_sha`/`.repo`/`.environment`/`.deploy_did` |
| `ToolCall` | `ActivityStarted` | — | opens a tool call; a pre-execution decision |
| `ToolResult` | `ActivityCompleted` | — | closes it, sharing `activity_id`; independently evaluated; `status` drives the tool's success metric |
| `TurnStarted` | `ActivityStarted` | `activity_type: llm_completion` | opens a model turn — see "Model turns" below |
| `TurnCompleted` | `ActivityCompleted` | `activity_type: llm_completion` | carries token counts, and under content capture the model's reply |
| `ModelCallRequested` | `ActivityStarted` | `activity_type: model_call_gate` | opens a model-call gate, evaluated through policy before the call is sent — see "Model-call gate" below |
| `ModelCallFinished` | `ActivityCompleted` | `activity_type: model_call_gate` | closes it, sharing `activity_id`; metadata only, never usage |
| `SubagentStarted` | `SignalReceived` | `subagent_started` | `metadata.agent_id`/`.agent_type` |
| `PermissionDenied` | `SignalReceived` | `permission_denied` | `denial_reason` is gated free text |
| `APIError` | `SignalReceived` | `api_error` | `error_type` is a closed provider enum; `error_details` is gated free text |
| 21 observe-only lifecycle signals (`Setup`, `InstructionsLoaded`, `UserPromptExpansion`, `MessageDisplay`, `PermissionRequest`, `PostToolBatch`, `Notification`, `TaskCreated`, `TaskCompleted`, `TeammateIdle`, `ConfigChange`, `CwdChanged`, `DirectoryAdded`, `FileChanged`, `WorktreeRemove`, `PreCompact`, `PostCompact`, `PreModelSwitch`, `PostModelSwitch`, `Elicitation`, `ElicitationResult`) | `SignalReceived` | `signal_name` = the type's own snake_case | none is paired as an activity; several carry one gated content key — see `signalDetailKeyFor` in `internal/client/payload.go` for the exact key per type |

### Muse Code hooks

Muse's hook stdin is Claude Code-shaped; `internal/adapters/muse/mapper.go`
maps it onto the same contract. The shapes are doc-derived and unverified on a
Muse binary (see [coverage.md](coverage.md)).

| Muse hook | Dev `event_type` | Gated | Note |
|---|---|---|---|
| `SessionStart` | `SessionStarted` | no | `source` `resume` or `clear` opens a new run |
| `UserPromptSubmit` | `PromptSubmitted` | yes | refusal is `decision: block` |
| `PreToolUse` | `ToolCall` | yes | `mcp__<server>__<tool>` names are MCP tools; paired by `tool_use_id` |
| `PostToolUse` / `PostToolUseFailure` | `ToolResult` | no | `completed` / `failed` |
| `PermissionRequest` | `PermissionRequest` | yes | evaluated to refuse only; no `tool_use_id` |
| `SubagentStart` | `SubagentStarted` | no | |
| `StopFailure` | `APIError` | no | no error text bound |
| `SessionEnd` | `SessionEnded` | no | |
| `PreLLMCall` | `ModelCallRequested` | yes | a model-call gate, see below |
| `PostLLMCall` | `ModelCallFinished` | no | metadata only |
| `Stop` | none | no | runs the session-log reconciler; no turn, no usage |
| `SubagentStop`, `PreCompact`, `PostCompact`, `Notification`, `PostToolBatch`, `Interrupt` | none | no | not installed |

Muse sends no `TurnStarted`/`TurnCompleted`: no hook payload's usage reaches
the wire, so a Muse session has no `llm_completion` rows.

### Correlation metadata keys

`metadata` is a free-form object; every key below is optional, and a provider
that has none simply omits it. No governance engine reads `metadata` directly
— not OPA, not Guardrails, not the alignment judge — but the correlation keys
also ride `signal_args` on a `SignalReceived` row, which those engines do
read. Never write one of core's own audit/verdict metadata keys (`event_type`,
`workflow_id`, `trust_tier`, `post_attestation`, `policy_fallback_used`, and
similar) into `metadata`: core merges the client's blob first and its own
keys second, skip-if-present, so a client key of the same name wins in the
stored row.

| Key | Providers | Meaning |
|---|---|---|
| `tool_use_id` | Claude Code, Codex, Muse | per-invocation id for a `ToolCall`/`ToolResult` pair |
| `pair_recovered` | Claude Code, Codex | `true` when the two hook processes reported different local ids for the same call and the client reconciled them onto one `activity_id`; absent, never `false`, otherwise |
| `prompt_id` | Claude Code (all lanes) | the active prompt's id, once known |
| `previous_request_id` | Claude Code (gateway/proxy lanes) | the prior call's upstream request id |
| `is_subagent` | Claude Code (gateway/proxy lanes) | present and `true` only when confirmed; never a stored `false` |
| `entrypoint` | Claude Code (gateway/proxy lanes) | `cli` / `sdk` / `desktop` |
| `query_source` | Claude Code (telemetry lane) | the provider's own call-type discriminator; vendor-owned vocabulary, absent means unclassified |
| `task_id` | Claude Code | correlates `TaskCreated`/`TaskCompleted`; never an activity pair |
| `elicitation_id` | Claude Code | correlates `Elicitation`/`ElicitationResult` |
| `turn_id`, `message_id` | Claude Code | `MessageDisplay`'s own correlation pair |
| `agent_id`, `agent_type` | Claude Code | identifies the subagent an event occurred inside |
| `turn_id` | Codex | per-turn correlation id |
| `thread_id`, `root_session_id` | Codex | set only when a forked thread's id differs from the session id it continues |

### Operation vs invocation identity

A tool call has two identities:

| Field | Means | Feeds | Stable across a retry? |
|---|---|---|---|
| `span.invocation_id` | this attempt | a local pairing key only | no |
| `span.operation_id` | what is being done | `activity_id` | yes — this is what an approval is filed against |

`activity_id` must survive a retry, or an approved request can never be
consumed. A shell call hashes its command (`OperationForCommand`); an MCP call
hashes its canonicalized arguments (`OperationForArgs`); any other class falls
back to the invocation id and can never hold an approval
(`internal/client/operation.go`).

### Model turns

A model turn rides the same `ActivityStarted`/`ActivityCompleted` pair a tool
call does, tagged `activity_type: llm_completion`; both halves fire from one
observation, so the pair is always complete. `activity_id` is
`<session_id>:turn:<index>` (or `<session_id>:agent:<agent_id>:turn:<index>`
for a subagent), except for the lane producers, which key
on their own request id instead (`turnActivityIDFor` in
`internal/client/payload.go`).

Up to three producers can observe a turn, in disjoint `activity_id`
namespaces so core's dedupe never absorbs one producer's evidence as a
duplicate of another's: the hook lane (Claude Code/Codex's own `Stop`), and either the local
telemetry receiver (`otel_request_id`, `:otel:`) or the transport relay
(`proxy_request_id`, `:proxy:`). The older gateway relay
(`gateway_request_id`, `:gateway:`) keeps its own namespace but is no longer
installed. An in-path lane classifies the call by its
captured path, not its HTTP method — method alone would file a token-count
probe as a real completion:

| Path | `activity_type` | Carries bodies |
|---|---|---|
| `POST /v1/messages` | `llm_completion` | yes |
| `POST /v1/responses`, `POST /v1/chat/completions` (any host) | `llm_completion` | yes |
| `POST /backend-api/codex/responses` on `chatgpt.com` only (path unverified against a live capture) | `llm_completion` | yes |
| `POST /v1/messages/count_tokens` | `token_count` | no, and not emitted at all |
| the tool's own telemetry call to its vendor | `tool_telemetry` | no |
| anything else on the intercepted host | `provider_request` | yes |
| a provider's pre-send hook, evaluated through policy (no path: not a relayed call) | `model_call_gate` | no, metadata only |

A relayed call is attributed to a provider by its host and then its carrier
header (`internal/cli/sessionkey/proxy.go`, `AttributeProxy`). A host one
provider reaches needs no disambiguation. On a host several providers reach
(`api.meta.ai` is in the claude-code, muse and codex rows), exactly one
provider's carrier must resolve:

| Carrier present | Outcome |
|---|---|
| `X-Claude-Code-Session-Id` only | claude-code, session id is the header |
| `X-Client-Request-Id` plus a Codex-only `Originator` header (`codex…`; unverified against a live capture) | codex, session id is the thread id |
| `X-Client-Request-Id` alone on a shared host | skipped, `no_provider_carrier` (another tool may send the same header) |
| none | skipped, `no_provider_carrier` |
| more than one provider's | skipped, `ambiguous_carrier` |

Muse has no known session carrier, so its row never attributes a call. A
skipped call is counted in the local capture trace and never guessed. None of
these headers is a credential, so none is redacted: the halt latch resolves a
session off them.

### Model-call gate

`ModelCallRequested`/`ModelCallFinished` ride the same `ActivityStarted`/
`ActivityCompleted` pair, with `activity_type: model_call_gate` on both halves
(v1.10). `activity_id` is `<session_id>:llmgate:<model_call_request_id>`
(`modelCallGateActivityID` in `internal/client/modelcallgate.go`), a namespace
disjoint from every producer above. It is never `llm_completion`: neither half
carries usage, a turn index or a producer request id, and no `hook_trigger` or
`spans[]` is sent. `ModelCallRequested`'s `activity_input` holds `model`,
`provider`, `message_count`, `tool_count`, `tool_names` and, under content
capture, `message_previews`; `ModelCallFinished`'s `activity_output` holds
`status`, `finish_reason`, `response_id`, `error_class`, `tool_call_count`,
`model` and `provider`. A denied gate leaves the started row only. Core must
evaluate the started half through policy and count it as neither a completion
nor a turn; see the 1.10 entry in [dev-event-contract.md](dev-event-contract.md).

### Signal payload: `signal_args`

Every `SignalReceived` except `prompt_submitted` carries its structural (and,
under content capture, its one content) key in `signal_args` as well as
`metadata`. `prompt_submitted`'s `signal_args` **is** the session's goal text.
Core's goal-alignment gate is what keeps the two apart: it only treats
`signal_args` as a new goal when `signal_name == prompt_submitted`. A
machine-injected `prompt_submitted` (a task notification arriving as a prompt
rather than typed input) carries no `signal_args` at all —
`metadata.prompt_source` records the classification, and the text that would
otherwise have become the goal is withheld rather than emptied.

### Unpaired activity halves

A tool call interrupted mid-flight (for example, an Esc during an
auto-approved shell command) can leave an `ActivityStarted` with no matching
`ActivityCompleted`. Count pairs per `activity_id` and tolerate a
started-only row; never synthesize the missing half.

---

## 3. Field homes: where every `DevEvent.Span` field goes

The adapter-facing `span` object is stable across contract versions and
adapters still populate it; `internal/client/payload.go` reads locators, counts and (gated)
bodies out of it onto `activity_input`/`activity_output` instead of
serializing it. No event carries a `span` or `spans[]` field on the wire.
Golden fixtures under `internal/client/testdata/golden/` pin this table
byte-exactly, one per tool kind per stage.

| `DevEvent` field | Wire home | Read on | Note |
|---|---|---|---|
| `tool.name` | `activity_input.tool_name`, `activity_type`, `metadata.tool_name` | started (+ both) | |
| `tool.kind` | `activity_input.kind` | started | `shell` / `file` / `mcp` |
| `tool.mcp_server`, `span.mcp_server` | `activity_input.mcp_server` | started | mcp only |
| `span.file_path`, `.file_operation` | `activity_input.file_path` / `.file_operation` | started | |
| `span.function` | `activity_input.mcp_tool` | started | mcp only |
| `span.bytes_read` / `.bytes_written` / `.lines_count` | `activity_output.<same>` | completed | |
| `metadata.exit_code` | `activity_output.exit_code` | completed | promoted from the free-form blob; no adapter supplies one today |
| `started_at`, `ended_at` | `duration_ms` | completed | omitted, never zero, when unknown |
| `content.tool_input` | `activity_input.command` / `.arguments` / `.content` | started | gated, capped; key chosen by tool kind |
| `content.tool_output` | `activity_output.output` | completed | gated, capped |
| `content.thinking` | `activity_output.thinking` | completed | gated, capped, turns only |
| `span.invocation_id`, `.operation_id` | *(local only)* | — | feed pairing / `activity_id`; never wire fields |
| `span.semantic_type`, `.stage`, `.module` | *(no wire home)* | — | adapters still set them; nothing on the wire reads them |
| `span.request_body` | `activity_input.content` | started | in-path lanes only — see "Request window" below |
| `span.response_body` | `activity_output.content` | completed | in-path lanes only, verbatim, gated and capped |
| `span.request_headers` / `.response_headers` | *(nowhere)* | — | redacted by key name at capture; never egresses |
| `span.http_status` | `metadata.http_status` | completed | absent when no response was observed at all |
| `span.credential_fingerprint` | `metadata.credential_fingerprint` | either | ungated |
| `span.prompt_id`, `.previous_request_id`, `.is_subagent`, `.entrypoint` | `metadata.<same>` | either | in-path lanes only, ungated |
| `span.response_truncated`, `.response_bytes_seen` | `activity_output.openbox_capture` | completed | `{truncated, original_bytes}`; `truncated:false` is emitted, never omitted |

### The request window is a selection, not a truncation

A model-call request body routinely exceeds the 64 KiB cap, and the newest
turn sits at the end of its `messages` array, not the start — a plain
byte-offset cut would keep old, unchanging boilerplate and drop the turn a
reader actually wants. `internal/gateway` therefore builds a synthetic
document instead: `model` verbatim, a capped `system`, `tools` dropped
entirely, and `messages` kept from the end backward until the budget. A
claude.ai body has no `messages`; its turn is a top-level `prompt`, which is
kept instead. `metadata.openbox_capture.truncated_paths` (or, on a response,
`activity_output.openbox_capture.truncated`) names what was cut; its absence
means nothing was.

A streamed Anthropic-format reply is stored as the one message it assembles
into (text, tool input and citations), not as its pings and per-token deltas,
which would spend the cap on framing. Any other response format is stored
verbatim.

---

## 4. Verdict (parsing the response)

The `/evaluate` response carries a `verdict` field, and a legacy `action`
field for older wire producers. `internal/client/verdict.go`'s
`parseEvaluation` prefers `verdict` and falls back to `action`; anything
unrecognized resolves to an empty, non-blocking verdict.

| Canonical | Wire `verdict` | Legacy `action` |
|---|---|---|
| `ALLOW` | `allow` | `continue` |
| `CONSTRAIN` | `constrain` | `continue` |
| `REQUIRE_APPROVAL` | `require_approval` | `require-approval` |
| `BLOCK` | `block` | `stop` |
| `HALT` | `halt` | `stop` |

For a gated call the parsed verdict is enforced, tighten-only (INV-3 in
[dev-event-contract.md](dev-event-contract.md#invariants)): it can deny,
hold or halt a call but never turns a provider's own deny into an allow.

`parseEvaluation` also carries, when core sends them: `risk_score`,
`alignment_score`, `trust_tier`, `behavioral_violations`, `constraints`,
`approval_id`, `governance_event_id`, a parsed `guardrails_result`
(`Passed` + structured `Reasons`, never raw content) and a parsed `age_result`
(`GoalDrifted` / `GoalAlignmentChecked` / `ViolationsCount`, a count only,
never the violation strings). None of these change the verdict; they are
carried for advisory recording only.

---

## 5. Client transport notes

A workload agent authenticates once per cold cache, not per request: a
bootstrap document (API-key only) plus an RS256 client assertion exchanged at
Keycloak for a short-lived bearer (see [architecture.md](architecture.md)'s
auth-flow section). Every `/api/v3/governance/evaluate` request then carries:

- `Authorization` — bearer-prefixed `obx_` API key
- `X-OpenBox-Workload-Token` — the exchanged bearer
- `X-OpenBox-SDK-Version`
- `Idempotency-Key` — the event id, when the caller supplies one

The body is compact JSON, unsigned; there is no per-request signature, nonce
or body-hash header. `workflow_id` (§1) is derived in memory from `agent_id`
and never carried on a header, signed or otherwise.
