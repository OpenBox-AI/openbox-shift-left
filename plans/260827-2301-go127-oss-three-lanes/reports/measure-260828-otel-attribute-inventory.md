# Phase 10 step 1 — the OTel attribute inventory, from a real desktop corpus

**Date:** 2026-08-28 · **Corpus:** openbox-logger run
`20260827T063932Z-225cac`, `otel/` only · **Client:** `claude-code-desktop`
**1.37937.3**, `claude.deployment_mode=1p`, darwin/arm64 · **Volume:** 4366 log
lines, 1823 trace lines, 482 metric lines (52 MB / 30 MB / 5 MB)

## Why this is admissible evidence, and what it is not

Phase 10 step 1 says "enumerate the exact attribute set per event type from the
phase-09 corpus", and phase 09's receiver has never listened — so there is no
corpus from *our* receiver. There is a corpus from *the target*: the sibling
lab's collector captured a real desktop session's real export, which is the
surface this lane exists to reach (the gateway lane's dead end). Read for schema
only; **no values were copied into this repo** (see Privacy below).

The unmeasured hop is the one between them: pdata flattening inside
`telemetry/consume.go`, which is unit-tested but has never seen a live export.
That hop is bounded by construction — `Record.Attrs` flattens every OTLP value
type through `AsString()`, and an unbound attribute is ignored rather than an
error (`telemetry/record.go`) — so attribute drift degrades to missing fields
plus an OD4 finding, never a lane outage. Also unobserved: the **CLI** terminal
client's export (assumed congruent, same engine; doctor's recording line and OD4
catch divergence). Version-pin per OD3: re-verify at phase 13 replay and at the
first live run.

## Event inventory — 19 types

| event.name | count | what phase 10 wants from it |
|---|---:|---|
| `hook_execution_start` | 5255 | engine health |
| `hook_execution_complete` | 5255 | engine health (continuous duplicate-engine detection) |
| `tool_decision` | 2534 | decision metadata |
| `tool_result` | 2519 | tool outcome where hooks are silent |
| `api_request_body` | 2249 | request body → observed span |
| `api_response_body` | 2229 | response body → observed span |
| `api_request` | 2229 | **the turn**: model, 4 token counts, cost, duration, ids |
| `hook_registered` | 1458 | engine health |
| `assistant_response` | 718 | (not in phase 10's scope — hook lane already carries reply text) |
| `plugin_loaded` | 72 | — |
| `user_prompt` | 34 | (hook lane already carries the prompt) |
| `mcp_server_connection` | 34 | — |
| `subagent_completed` | 26 | — |
| `skill_activated` | 15 | — |
| `retention_sweep` | 11 | **relevant**: the client deletes its own artifacts (see body_ref) |
| `at_mention` | 4 | — |
| `api_refusal` | 2 | ** below** |
| `api_error` | 2 | failure signal |
| `compaction` | 1 | — |

Every event phase 10 requirement 1 names is present. Five types the phase did
not anticipate exist (`assistant_response`, `api_error`, `api_refusal`,
`subagent_completed`, `compaction`).

## Per-event attributes

Ubiquitous on every log record and elided from the table below:
`event.name`, `event.sequence`, `event.timestamp`, `session.id`, `prompt.id`,
`organization.id`, `user.email`, `user.id`, `user.account_id`,
`user.account_uuid`, `service.name`, `service.version`, `claude.deployment_mode`,
`host.arch`, `os.type`, `os.version`, `process.owner`, `terminal.type`.

| event | distinctive attributes |
|---|---|
| `api_request` | `model` `input_tokens` `output_tokens` `cache_read_tokens` `cache_creation_tokens` `cost_usd` `cost_usd_micros` `duration_ms` `request_id` `client_request_id` `query_source` `effort` `speed` `agent.name` `skill.name` `mcp_server.name` `mcp_tool.name` |
| `api_request_body` | `body_ref` `body_length` `model` `query_source` |
| `api_response_body` | `body_ref` `body_length` `model` `query_source` **`request_id`** |
| `api_error` | `status_code` `error` `attempt` `model` `duration_ms` `request_id` `client_request_id` `query_source` `effort` `speed` |
| `api_refusal` | `attempt` `model` `request_id` `server_fallback_hop` `query_source` `effort` `speed` `skill.name` |
| `tool_decision` | `decision` `source` `tool_name` `tool_parameters` `tool_source` `tool_use_id` |
| `tool_result` | `success` `duration_ms` `error` `error_type` `tool_name` `tool_use_id` `tool_input` `tool_input_size_bytes` `tool_result_size_bytes` `tool_parameters` `mcp_server_scope` |
| `hook_registered` | `hook_event` `hook_matcher` `hook_source` `hook_type` `safe_mode` |
| `hook_execution_start` | `hook_event` `hook_name` `hook_source` `num_hooks` `managed_only` `safe_mode` |
| `hook_execution_complete` | + `num_blocking` `num_cancelled` `num_success` `num_non_blocking_error` `total_duration_ms` |
| `assistant_response` | `response` `response_length` `model` `request_id` `message.uuid` `query_source` |
| `user_prompt` | `prompt` `prompt_length` `command_name` `command_source` `message.uuid` |
| `subagent_completed` | `agent_type` `agent.source` `model` `final_model` `model_swapped` `total_tokens` `total_tool_uses` `duration_ms` `is_async` `is_built_in` |
| `compaction` | `pre_tokens` `post_tokens` `trigger` `duration_ms` `success` |
| `mcp_server_connection` | `server_name` `server_scope` `transport_type` `status` `is_plugin` `duration_ms` |
| `retention_sweep` | `artifacts_deleted` `transcripts_deleted` `session_files_deleted` `files_past_cutoff` `files_retained_fresh` `period_days` `result` `error_count` `used_default` |

Traces (1823 lines): `claude_code.llm_request` (2234 spans),
`claude_code.tool` / `.execution` / `.blocked_on_user` (~2530 each),
`claude_code.interaction` (33). Span attributes include the **OTel GenAI
convention** (`gen_ai.request.model`, `gen_ai.response.id`, `gen_ai.system`,
`gen_ai.response.finish_reasons`), plus `stop_reason`, `full_command`,
`file_path`, `agent_id`/`parent_agent_id`/`subagent_type`, `status_code`.
Metrics (482 lines): `claude_code.token.usage`, `cost.usage`,
`code_edit_tool.decision`, `lines_of_code.count`, `commit.count`,
`session.count`, `active_time.total`.

## Six findings that change the implementation

### 1. Model bodies arrive as FILE PATHS, and that is a local-file-read oracle

`api_request_body` / `api_response_body` carry no body. They carry
`body_ref` — an **absolute filesystem path** into the raw-bodies sink
(`…/data/api-bodies/<stem>.request.json`) — plus `body_length` (observed up to
251 KB).

The receiver is an **unauthenticated loopback listener by construction**. So any
local process can POST a well-formed log record naming
`body_ref=/Users/…/.openbox/.env` or `~/.ssh/id_ed25519`, and a mapper that
simply opens the path would read it, run it through the redactor, sign it, and
egress it to the control plane as a model-call body. **This is the single
highest-severity item in phase 10** and the plan does not mention it.

Required containment, and it must be structural rather than a prefix check:
confine reads to the configured raw-bodies directory with `os.Root` (available at
the go 1.27 floor and immune to the symlink TOCTOU that `EvalSymlinks` + prefix
comparison is not), bound the read with `io.LimitReader`, never trust
`body_length`, and treat a missing file as normal — `retention_sweep` (11
occurrences here) shows the client deletes its own artifacts. The test should be
named after the escape attempt.

### 2. The response-body join is exact; the request-body join is not

Verified: `api_response_body`'s `body_ref` filename stem **equals** its
`request_id` (`req_011CeSoFqW2HfEh9jxCds86Y` in both). So response bodies attach
to their `api_request` exactly.

`api_request_body` carries **no request id at all** — only `session.id`,
`prompt.id`, `model`, `query_source` and `event.sequence` adjacency. So request-body
attachment is a nearest-preceding heuristic that can **misjoin under concurrent
calls in one session**. Attach the request body only when unambiguous; a misjoin
would file one call's prompt under another call's turn.

### 3. The same attribute name has DIFFERENT OTLP value types per event

Measured: `duration_ms` is `intValue` on `api_request` but **`stringValue`** on
`tool_result`. `success` is `stringValue`. Token counts are `intValue`;
`cost_usd` is `doubleValue`; `cost_usd_micros` is `intValue`.

A mapper that assumed `intValue` for `duration_ms` would read zero on every
`tool_result` — silently. This is already handled, and by accident of a good
decision: `consume.go` flattens every value through `AsString()`
(`consume.go:107,143`), so the mapper parses from text and the inconsistency
cannot bite. **Do not "optimize" that to typed extraction.**

### 4. Identity is record-level, not resource-level — and the plan says otherwise

Phase 10's architecture section says identity comes from *resource* attributes
(`organization.id`, `user.email`, `session.id`, `service.version`). Measured: the
resource carries only `service.name`, `service.version`,
`claude.deployment_mode`, `host.arch`, `os.type`, `os.version`, `process.owner`.
Everything else — including `session.id`, the correlation key — is **record**
level. `consume.go`'s merge order (resource, scope, record; record wins) already
handles both, so this is a documentation correction, not a code change.

It stays **client-asserted** either way: usable to bind sessions for detection,
never as proof for a refusal.

### 5. The lane is per-API-CALL, not per-turn — so its numbers will never match the hook lane

`query_source` distinguishes main-loop calls from side calls (title-generation
haiku calls appear inside sessions). The hook lane sums a `Stop`-window per turn;
this lane sees individual API calls. **They will not numerically agree, and that
is not a defect** — it is an argument for the election (phase 12), not for
averaging. Record the discrepancy; do not reconcile it.

Also: `claude_code.llm_request` **traces** describe the same calls as
`api_request` **logs**. Binding both would double-count *within* this lane, which
no namespace or election protects against. Traces and metrics stay
accept-and-count (phase 09's decision), now with a measured reason.

### 6. `api_refusal` exists — direct evidence toward

Two occurrences, carrying `attempt` and `server_fallback_hop`. §9 — what refusal
shape Claude Code does not retry around — is the **only** thing still keeping
that decision in DRAFT, and it has been treated as unanswerable without a probe.
These records are observational evidence about the client's own retry behaviour.

Recorded, not acted on: two samples is not an answer, and phase 13 owns the
probe-A instrument. But this is a cheaper path to §9 than the plan assumes.

## Privacy

The corpus contains real prompts, real tool inputs, real file bodies, the user's
real email, and real org UUIDs. **Only schema was read**: attribute KEYS, event
NAMES, value TYPES, and counts. The one value class printed during analysis was
`body_ref` paths and `request_id`s, to verify the join in finding 2. No prompt,
response, tool input or body content was read, and none is reproduced here.

Fixtures for phase 13 replay must be **sanitized at generation**, not filtered
later; raw corpus files must not be copied into this repo. The 2.2 GB `proxy/`
capture in the same run was not opened at all.

## Unresolved questions

1. **`assistant_response` and `user_prompt` carry full content inline** and are
   in the corpus but outside phase 10 requirement 1. The hook lane already
   egresses both, so binding them here would duplicate content under a second
   producer. Recommend leaving them unbound and stating it in `COVERAGE.md`;
   confirm that is the intent.
2. **Which `body_ref` directory** the lane configures is phase 09's env-key
   decision, still unmade (its probe never ran). The confinement root follows
   that config, so finding 1's control cannot be finished until it is.
3. `cost_usd` vs `cost_usd_micros` — both present. `turnActivityOutput`'s comment
   rules cost out of `activity_output` deliberately (the server derives it), so
   telemetry cost goes through the existing finops metadata path or nowhere. Do
   not create a second cost authority.
