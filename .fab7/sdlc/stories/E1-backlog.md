# Epic E1 — Provider-agnostic governance core: story backlog

**Author:** Sol (Planning Lead) — 2026-07-07 (re-prioritized 2026-07-07: shift-left-first)
**Epic:** E1 (PRD §8) — OpenBox registers a developer session as an agent and ingests/stores/normalizes/reads its events through the existing governance pipeline, tool-agnostically.
**Covers:** FR-1, FR-4, FR-7, FR-8 (+ enabling ingestion infra). **Scope basis:** path-prefix (deterministic floor) + code-graph blast-radius check (no cross-scope collisions surfaced).
**Note:** this is the story *list* (specs to be written by `draft-story`, ledger by `init-ledger`) — not contracts, not live status.

## ⚑ Priority policy — SHIFT-LEFT FIRST (sponsor directive 2026-07-07)

Build **shift-left-repo-owned** work first; changes to **external OpenBox components (openbox-backend, openbox-core)** are **lowest priority and deferred**. Classification of E1 stories by owning codebase:

| Story | Owning codebase | Priority |
|---|---|---|
| **E1-S1** event contract | **shift-left repo** (`contracts/`) | **PRIORITY (build now)** |
| E1-S2 developer-agent registration | openbox-backend (external) | **DEFERRED** |
| E1-S3 core event types | openbox-core (external) | **DEFERRED** |
| E1-S4 core persistence | openbox-core (external) | **DEFERRED** |
| E1-S5 lineage read API | openbox-backend (external) | **DEFERRED** |

**Consequence:** within E1, only **E1-S1** is shift-left-owned. The near-term shift-left build path is **E1-S1 (contract) + E2 (the `openbox` CLI + Claude Code adapter) + E3 (git action)**, all in this repo.

**OD14 — DECIDED (2026-07-07, sponsor):** *Assume the external OpenBox component changes are made* — the developer event types, `kind=developer` registration, persistence, and lineage API (E1-S2/S3/S4/S5) are owned elsewhere and treated as **assumed-satisfied dependencies**. Shift-left stories are written against the **full intended API surface** (developer events are first-class at `/api/v1/governance/evaluate`; `agent/create` accepts `kind=developer`; the lineage read API exists). The external stories keep their specs and stay **deferred/lowest-priority** for the shift-left team; they are not this team's build path but are presumed available at integration time. → See the dedicated **shift-left build backlog**: `.fab7/sdlc/stories/shift-left-backlog.md`.

The five stories below remain the correct decomposition; their **priority/sequence is now governed by the table above** — the deferred (external) stories keep their specs so they're ready when the external work is scheduled, but they are **not** in the first build path.

Persistence facts grounding the scopes (code-graph): openbox-core owns the **write** path — `api/governance.go` `EvaluateGovernanceEvent` → `services/governance_workflow.go` (SessionLifecycle/StoreHookSpan activities) → `datastore/*_pgx.go` (session_pgx, governance_event_pgx, span_pgx, session_merkle_leaf_pgx). Event-type vocabulary = `content/governance.go` constants, gated by `isValidGovernanceEventType` (api/governance.go). openbox-backend owns **registration** (`modules/agent/`, `didFor` naming.ts, `AgentEntity.createHash`, trust schema) and **read/serve** (`modules/governance-event/` `GovernanceEventService.getSessionLogs`/`loadSpansForEvents`, mapping the same tables via TypeORM).

---

## Stories

### E1-S1 — Normalized developer-runtime event contract + conformance test
- **Goal / value:** a versioned, tool-agnostic event schema — the single interface every adapter and both server sides implement (FR-4). Nothing else in E1 is safe to freeze without it.
- **Source:** PRD FR-4; architecture §1b (D3/D4 normalized event contract), INV-8.
- **Acceptance criteria (observable):**
  - A versioned schema (`schema_version`) defines the developer event types `SessionStarted|PromptSubmitted|ToolCall|ToolResult|SessionEnded|CommitCreated|Deploy` with fields: `openbox_session_id`, `developer_did`, `event_type`, `tool{name,kind:shell|file|mcp,mcp_server?}`, timestamps, `tokens?`, `cost?`, `content?` (optional, absent by default).
  - A conformance test suite validates a sample event of each type and rejects malformed/content-when-disabled payloads.
  - Schema is language-neutral (consumable by Go core, TS backend, and adapters).
- **Write scope:** `contracts/dev-event/` (new, this repo or shared package). `scope_basis: path-prefix`.
- **Dependencies:** none. **First.**
- **Gates:** G2 (scope), G3 (build/test).
- **Invariants:** INV-2 (content optional, default-absent), INV-8 (additive/compatible).
- **Validation:** schema lints + conformance test suite passes (exact runner TBD with impl language — confirm in project memory `validation-commands`).
- **Stop:** if the schema would require a non-additive change to existing governance event shape → HALT, route to architecture.

### E1-S2 — Developer-agent registration (`kind=developer`) + session child
- **Goal / value:** a developer/tool-install can register as an OpenBox agent and open sessions, reusing the existing registry — the identity substrate (FR-1, FR-8).
- **Source:** PRD FR-1/FR-8/NFR-6; architecture D5 (new agent kind, session child), INV-6/-7, OD11 (hybrid).
- **Acceptance criteria (observable):**
  - `POST agent/create` (or its service path) accepts `kind=developer` and issues `obx_` key + `didFor()` DID + initial trust record, reusing the existing flow (`agent.service.ts:652`).
  - A session opens as a child `SessionEntity` under the developer agent's DID; credential persisted only as hash (`AgentEntity.createHash`), never cleartext (NFR-6/INV-1).
  - Records are `organization_id`-scoped (INV-4); developer agents share the runtime DID namespace (INV-7).
- **Write scope:** `openbox-backend/src/modules/agent/`, `openbox-backend/src/modules/session/`. `scope_basis: path-prefix`.
- **Dependencies:** none (parallel with E1-S1). 
- **Gates:** G2, G3, **security review (Sam)** — touches identity/credential/authz.
- **Invariants:** INV-1, INV-4, INV-6-adjacent, INV-7.
- **Validation:** backend build + unit tests for the agent module (confirm exact `npm` scripts in project memory `validation-commands`).
- **Stop:** if a new `kind` requires schema-breaking changes to `AgentEntity` beyond additive → flag.

### E1-S3 — Core developer event-type vocabulary + evaluate acceptance
- **Goal / value:** openbox-core accepts developer-runtime event types at the existing evaluate endpoint without breaking Temporal consumers (FR-4).
- **Source:** PRD FR-4; architecture D4; INV-8; grounds: `content/governance.go:13-19`, `isValidGovernanceEventType` (api/governance.go:273).
- **Acceptance criteria (observable):**
  - New event-type constants added to `content/governance.go` matching the S1 contract; `isValidGovernanceEventType` accepts them.
  - `EvaluateGovernanceEvent` accepts a well-formed developer event (200) and rejects unknown/malformed types.
  - **INV-8 conformance test:** existing Temporal governance event types and their consumers (`governance.evaluated`) behave unchanged.
- **Write scope:** `openbox-core/internal/content/`, `openbox-core/internal/api/governance.go`. `scope_basis: path-prefix`.
- **Dependencies:** **E1-S1** (contract defines the types).
- **Gates:** G2, G3.
- **Invariants:** INV-8.
- **Validation:** `cd openbox-core && go build ./... && go test ./internal/content/... ./internal/api/...`.
- **Stop:** if a consumer switches on event type in a way the additive change breaks → HALT (INV-8 violated), route to architecture.

### E1-S4 — Core developer-event persistence & session lifecycle
- **Goal / value:** developer events persist as Session/GovernanceEvent/Span through the existing activities, idempotently and privacy-safely (enabling FR-2/FR-3 ingestion; FR-7 data).
- **Source:** architecture D3 (reuse SessionEntity/GovernanceEventEntity/SpanEntity), INV-2/-4/-5; grounds: `services/governance_workflow.go` (SessionLifecycleActivity, StoreHookSpanActivity), `datastore/{session,governance_event,span,session_merkle_leaf}_pgx.go`.
- **Acceptance criteria (observable):**
  - A developer `SessionStarted`/`SessionEnded` drives the session lifecycle; per-tool/prompt events store as governance events + spans linked to the session and Merkle leaf.
  - Ingestion is idempotent by client event id — retries/buffered flush do not double-count tokens/cost or duplicate rows (INV-5).
  - Content fields are dropped when content-capture is disabled; metadata-only by default (INV-2). All rows `organization_id`-scoped (INV-4).
- **Write scope:** `openbox-core/internal/services/governance_workflow.go`, `openbox-core/internal/datastore/` (governance_event_pgx.go, session_pgx.go, span_pgx.go). `scope_basis: path-prefix`.
- **Dependencies:** **E1-S3** (needs accepted types).
- **Gates:** G2, G3, **security review (Sam)** — persists session data / privacy boundary.
- **Invariants:** INV-2, INV-4, INV-5.
- **Validation:** `cd openbox-core && go test ./internal/services/... ./internal/datastore/...` (DB-backed tests).
- **Stop:** if idempotency needs a schema change to the shared tables beyond additive → flag.

### E1-S5 — Lineage read surface (session → commit → deploy + finops)
- **Goal / value:** for a commit or deploy DID, return the originating session(s), tools/MCP used, prompt count, and total tokens/cost (FR-7) — the observable payoff of E1.
- **Source:** PRD FR-7; architecture INV-6; grounds: `GovernanceEventService.getSessionLogs`/`loadSpansForEvents` (governance-event.service.ts:250,218).
- **Acceptance criteria (observable):**
  - A read API/query returns, for a given commit hash or deploy DID, the linked session set, the tools/MCP names used, prompt count, and summed tokens/cost.
  - Reuses `getSessionLogs`/span loading; `organization_id`-scoped (INV-4); returns `unattributed` cleanly when no session links exist (INV-6, pre-E3 state).
  - Finops totals reconcile with the per-span token/cost metadata.
- **Write scope:** `openbox-backend/src/modules/governance-event/`. `scope_basis: path-prefix`.
- **Dependencies:** **E1-S2** (developer sessions exist to read); consumes data from **E1-S4** for end-to-end verification (deploy leg completes after E3).
- **Gates:** G2, G3.
- **Invariants:** INV-4, INV-6.
- **Validation:** backend build + unit/integration tests for the lineage query (confirm exact scripts).
- **Stop:** if FR-7 needs a query the existing schema can't answer without a new table → flag (challenges the reuse assumption; route to architecture).

---

## Dependencies & sequencing (no cycles)

```
E1-S1 (contract) ─┬─► E1-S3 (core types) ──► E1-S4 (core persistence) ──┐
                  │                                                     ├─► E1-S5 (lineage read)
E1-S2 (registration + session child) ───────────────────────────────────┘
```

- E1-S1 and E1-S2 have no dependencies → **parallelizable** (disjoint scopes: new `contracts/` vs `openbox-backend/src/modules/agent+session/`).
- E1-S3 depends on S1; E1-S4 depends on S3 (serialized within core → their scopes overlap-free anyway).
- E1-S5 depends on S2 (sessions) and, for full end-to-end verification, S4 (data). Backend scopes S2 (`agent/`,`session/`) and S5 (`governance-event/`) are path-prefix disjoint; blast-radius check surfaced no collision, and S5-after-S2 ordering removes any residual risk.

## Recommended first build batch — REVISED (shift-left-first)
**E1-S1 only** (the event contract — the sole shift-left-owned E1 story), then proceed into **E2** (`openbox` CLI + Claude Code adapter) as the real near-term shift-left work. **E1-S2/S3/S4/S5 are deferred** (external components, lowest priority) — draft them as ready specs but do not schedule them in the first build path.

*(Superseded prior recommendation: "E1-S1 + E1-S2 in parallel" — E1-S2 is external and now deferred.)*

## Deferred / out of E1
- Adapter emission (FR-2/FR-3/FR-5) = **E2**; Deploy-DID linkage (FR-6) = **E3** — the deploy leg of FR-7 completes when E3 lands.
- Enforcement = E6 (Phase 2, blocked on S2 latency spike).

## Flags for draft-story / planning
- **OD14 (blocks E2 drafting):** with external changes deferred, how do developer session events reach OpenBox in Phase-1 observe? The existing `/api/v1/governance/evaluate` rejects new developer event types until E1-S3 (deferred). Options: (a) **OTel-first** — telemetry via Claude Code native OTel → existing OTLP/span path (no external change); (b) **map onto existing event types** through the evaluate endpoint (semantic debt, no external change); (c) **accept E1-S3 as the single un-deferred external change**. Owner: brian.
- **Exact validation commands** for openbox-backend (npm scripts) and openbox-core (Go test targets, DB fixtures) — needed only when the deferred external stories are scheduled; not blocking for E1-S1/E2.
- **Security review (Sam)** required on E1-S2 (identity/credential) and E1-S4 (session-data persistence / privacy boundary) — when those deferred stories are scheduled.
- **S1 contract home:** in this repo (`contracts/`) vs a shared package consumed by core/backend/adapters — decide at draft-story; non-blocking.
