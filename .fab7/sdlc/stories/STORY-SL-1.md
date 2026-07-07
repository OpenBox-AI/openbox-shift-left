# STORY-SL-1 — Normalized developer event contract + conformance test

**Risk:** medium (keystone interface every adapter + the client depend on)

## Source
- **PRD:** `.fab7/sdlc/design/prd.md` FR-4 (tool-agnostic normalized event contract).
- **Architecture:** `architecture.md` §1b (Provider Adapter Contract — the normalized event is the SPI), D3/D4 (S6-corrected).
- **Discovery:** spike **S6 §2/§3/§7** (core `GovernanceEventPayload` shape; the two event-type axes; verdict vocab), **S1 A1** (Claude Code hook events), **S5 §1** (Codex events), **S3** (`CommitCreated`/`Deploy`).
- **Decision:** OD4 (metadata-only default), OD17 (Go — conformance harness).

## User Value
A single versioned, tool-agnostic event schema that any coding-tool adapter maps onto and the client emits verbatim — so adding a provider never changes the core or the wire format.

## Inlined context (so the builder need not re-read the design)
- **Target wire shape (openbox-core, verified S6 §0/§2):** events are POSTed to `POST /api/v1/governance/evaluate` as a `GovernanceEventPayload` — common fields `source`, `event_type`, `workflow_id`, `run_id`, `timestamp`, plus `span_count`, `spans []SpanData`, `metadata` (JSON), `multi_agent_session_id`, `from_agent_did`. A "session" is keyed by `(workflow_id, run_id)` (both required). `SpanData` already has first-class fields for file ops (`file_path`, `file_operation`, `bytes_read/written`, `lines_count`), function/tool calls (`function`, `module`, `args`, `result`), and semantic types `file_read/write`, `mcp_tool_call`, `llm_tool_call`, `llm_completion`.
- **Two axes (S6 §7):** developer events are *lifecycle* types (this contract) that carry *semantic span* types (already in core). The contract maps each lifecycle type to (a) which openbox-core `event_type` it becomes and (b) which span semantic type(s) it carries.
- **Verdict vocab (canonical, S6 §7):** `HALT > BLOCK > REQUIRE_APPROVAL > ALLOW`.
- **Privacy (OD4/INV-2):** `content` fields are optional and **absent by default**.

## Acceptance Criteria
- A versioned schema (`schema_version` present) defines the 7 lifecycle event types: `SessionStarted`, `PromptSubmitted`, `ToolCall`, `ToolResult`, `SessionEnded`, `CommitCreated`, `Deploy`.
- For each type the schema documents: the target openbox-core `event_type` it maps to, the carried span semantic type(s), and which fields land in `metadata` (e.g. git `commit_sha`/`repo`/`deploy_id` for `CommitCreated`/`Deploy`).
- Event fields include: `openbox_session_id` (→ how it maps to `(workflow_id, run_id)`), `developer_did`, `tool { name, kind: shell|file|mcp, mcp_server? }`, `timestamps`, `tokens?`, `cost?`, `content?` (optional, absent by default).
- The canonical verdict enum `HALT|BLOCK|REQUIRE_APPROVAL|ALLOW` is defined for parsing responses.
- A **conformance test** (Go) validates one well-formed sample of each event type, and **rejects**: (a) a malformed/unknown type, (b) any event carrying `content` when content-capture is disabled.
- A documented mapping table (schema → `GovernanceEventPayload`) exists so SL-3 can build payloads without guessing.

## Nonfunctional Requirements
- **security:** schema forbids credentials/secret material as first-class fields; `content` gated (INV-2).
- **compatibility:** additive to core's accepted types — must not require changing existing Temporal event semantics (INV-8).
- **portability:** schema is language-neutral (JSON Schema); only the conformance harness is Go.

## Write Scope
- `contracts/dev-event/` (schema files, mapping doc, and Go conformance harness under `contracts/dev-event/conformance/`).

## Dependencies
- None. **First-batch (parallel with STORY-SL-2).**
- **External (assumed-satisfied, not built here):** EXT-core — openbox-core's `isValidGovernanceEventType` must accept these lifecycle types for events to be *accepted* end-to-end (3 additive edits, S6 §3). The contract is authored independently of that change.

## Invariants
- **INV-2:** content optional and absent by default; schema rejects content-when-disabled.
- **INV-8:** additive/compatible with core's existing accepted event types; no change to Temporal event semantics.

## Human Gates
| Gate | Question | Owner | Evidence Needed | Allowed Outcomes |
|---|---|---|---|---|
| G1_READY | Contract home: `contracts/` in this repo (published for core/backend/adapters to consume)? | brian (tech) | confirmed location + publish plan | confirm / relocate |
| G3_REVIEW | Does the schema map cleanly onto core's `GovernanceEventPayload` without a core wire change? | brian (architecture) | mapping table reviewed vs S6 §2 | approve / revise |

## Validation
```bash
# conformance harness (Go, OD17)
cd contracts/dev-event/conformance && go build ./... && go test ./...
# schema well-formedness (any JSON Schema validator; e.g.)
# npx ajv compile -s ../schema/*.json   # or a Go-based schema check in the harness
```

## Stop conditions
- If mapping a lifecycle type onto `GovernanceEventPayload` would require a **non-additive** change to the core wire shape or to existing Temporal event semantics → HALT (INV-8), route to architecture.
- If a required field cannot be represented without storing content by default → HALT (INV-2), route to OD4 owner.
