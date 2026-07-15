# Architecture — Shift-Left Developer-Runtime Governance (Phase 1: Observe-First)

**Status:** draft
**Author:** John (Solution Architect) — 2026-07-07
**Sources:** `.fab7/sdlc/design/prd.md`, `.fab7/sdlc/discovery/brief.md`, `.fab7/sdlc/discovery/spikes/S1-dev-runtime-surfaces.md`; code-graph evidence over `openbox-backend`, `openbox-core`, `openbox-temporal-sdk-python`.
**Scope:** Phase 1 observe-first only. Enforcement (Phase 2) is designed-for, not built.

> **Guiding principle (from the sponsor):** *Maximal reuse of existing OpenBox components. Shift-left governs the developer's agentic CLI/IDE with the **same philosophy and the same machinery** the OpenBox SDK uses to govern an agent's runtime.* The shift-left adapter is to Claude Code/Cursor what `create_openbox_worker()` is to a Temporal worker. **No new governance pipeline is built — the developer runtime is onboarded onto the existing one.**

---

## 1. The mirror: agent runtime → developer runtime

| Concern | Agent runtime (exists today) | Developer runtime (shift-left) |
|---|---|---|
| Onboard | `pip install openbox` + `create_openbox_worker(openbox_api_key, agent_did)` (`worker.py:43`) | Install OpenBox adapter (Claude Code plugin / Cursor hooks bundle); configure `obx_` key + developer DID |
| Register identity | `POST agent/create` → `obx_` key + `didFor(agentId)` + trust (`agent.service.ts:652`) | **Same endpoint**, new `kind = developer`; session = child `SessionEntity` |
| Auth | `Authorization: Bearer <obx_key>` + `X-OpenBox-SDK-Version`, AIP Ed25519-signed via DID (`hook_governance.py:171`, `build_auth_headers`) | **Same headers, same AIP signing** (`_agent_did`, `_signer`) |
| Report/govern events | `POST /api/v1/governance/evaluate` on openbox-core (`hook_governance.py:381`; core `governance.go:28` `EvaluateGovernanceEvent`) | **Same endpoint**, new developer-runtime event types |
| Telemetry | events → `POST /api/v1/governance/evaluate` (`spans[]` in payload) | adapter **translates** tool telemetry → same `/evaluate` endpoint (NO OTLP intake exists — S6 §2) |
| Store | `SessionEntity` → `GovernanceEventEntity` (`governance_events`, FK+index on `session_id`) → `SpanEntity`; `SessionMerkleLeafEntity` for integrity | **Same tables** — developer sessions are Sessions; session events are GovernanceEvents+Spans |
| Read / dashboard | `GovernanceEventService.getSessionLogs / getLogs / getApprovals` | **Same service** — developer session logs for free |

The net new surface is small and mostly **client-side**: the per-tool adapters, one new agent `kind`, a set of developer-runtime event types, and the OpenBox git action's deploy event. The server pipeline is reused.

---

## 1b. Generic provider-adapter model & the interfaces of shift-left

Coding agents differ sharply in what they expose (S1: Claude Code has native OTel + rich hooks; Cursor has hooks + a poll-only Admin API and no OTel; Codex is a third profile, spike S5). The architecture must therefore be **provider-agnostic at the core and provider-specific only at the edge.** One normalized contract; one adapter per provider.

### The three interfaces of shift-left

1. **Developer onboarding interface (edge, per-provider).** The developer installs a thin OpenBox **adapter** for their tool and authenticates it with an `obx_` key + developer DID — exactly the `create_openbox_worker()` gesture, one layer out. Governance is then *ambient*: the developer keeps using their tool normally; there is no new day-to-day UI. **Front door (DECIDED, OD12 as revised by OD18): a hybrid — plugin vehicle + `openbox` CLI engine.** `openbox dev init --provider claude-code|cursor|codex` is the uniform front door; for Claude Code it **installs a native plugin that bundles `bin/openbox` (the Go engine, OD17) + the hook wiring** (distributed via a marketplace, force-enableable via managed `enabledPlugins`); for Codex/Cursor it lays down the config + managed-hooks bundle (`requirements.toml`/MDM; `hooks.json`/Team hooks). One Go engine + one contract underneath; the CLI also remains the CI/git-action path no plugin covers. Phase-1 plugin bundle = `bin/openbox` + hooks only (OD19; MCP server / skills deferred).
2. **Admin / governance interface (reused, provider-agnostic).** The **existing** OpenBox dashboard/backend: developer-agent registry, session logs (`GovernanceEventService.getSessionLogs`), lineage/finops views, and (Phase 2) policy. It is provider-agnostic **because events are normalized before they arrive** — the dashboard never branches on tool.
3. **Provider Adapter Contract (the SPI — the generic seam).** What every adapter MUST implement, regardless of tool:
   - `register()` — bind the session as an agent (`kind=developer`) via `POST agent/create`; obtain/attach `obx_` key + DID + Ed25519 signer (same as the SDK).
   - `emit(event)` — map the tool's native payload to the **normalized OpenBox governance event** (D3/D4 schema) and POST to `/api/v1/governance/evaluate` with the standard `Bearer` + AIP-signed headers.
   - `apply(verdict)` — Phase 2 only: enforce deny/ask/rewrite in the tool's terms; no-op (observe) in Phase 1.
   - `capabilities()` — declare the provider's capability profile (below).

### Capability model (how heterogeneity is absorbed)

An adapter declares which capabilities it supports; **OpenBox core is written against the normalized event contract and never assumes a capability.** Capabilities:

| Capability | Meaning | How it's satisfied |
|---|---|---|
| `identity.register` | bind session→agent | `POST agent/create` (all providers) |
| `telemetry.push` | native push of usage/cost/tool metrics | OTel → OTLP collector |
| `telemetry.poll` | server-side usage API polled by OpenBox | backend poller service |
| `telemetry.hook` | usage/tool events derived from lifecycle hooks | client hook → `emit()` |
| `session.lifecycle` | session start/end events | hook events |
| `tool.events` | pre/post tool + MCP-call events | hook events |
| `enforce.decision` | block/ask a tool call (Phase 2) | hook returns deny/ask |
| `enforce.rewrite` | redact tool input/output (Phase 2) | hook rewrites payload |
| `commit.binding` | stamp `OpenBox-Session` git trailer | **git-level, provider-independent** (S3) |
| `egress.proxy` | route model traffic via gateway | base-URL override |

### Capability matrix (evidence: S1 for Claude Code/Cursor, S5 for Codex)

| Capability | Claude Code | Cursor | Codex |
|---|---|---|---|
| `identity.register` | ✅ | ✅ | ✅ (obx_ key + DID; tool-independent) |
| `telemetry.push` (OTel) | ✅ native (S1 A4) | ❌ no OTel (S1 B4) | ✅ native `[otel]` OTLP, off-by-default (S5§4) |
| `telemetry.poll` | — (push instead) | ✅ Admin API ≤1/hr (S1 B4) | ✅ OpenAI Usage/Cost API (per-key/project, not per-session — S5§4) |
| `telemetry.hook` | ✅ (S1 A1) | ✅ (S1 B1) | ✅ hooks + rollout JSONL (S5§1,§4) |
| `session.lifecycle` | ✅ SessionStart/End | ⚠️ partial/beta (S1 A6) | ✅ SessionStart/Stop (S5§1) |
| `tool.events` incl. MCP | ✅ `mcp__*` matchers | ✅ `beforeMCPExecution` | ✅ PreToolUse over Bash/apply_patch/`mcp__*` (S5§1,§5) |
| `enforce.decision` | ✅ deny/ask (fail-controlled) | ⚠️ beta, **fail-open** (S1 B1) | ✅ deny via exit 2 / permissionDecision; **beta** (S5§1) |
| `enforce.rewrite` | ✅ updatedInput/Output | ✅ afterMCPExecution | ✅ `updatedInput` (PostToolUse can't undo — S5§1) |
| `commit.binding` | ✅ | ✅ | ✅ (git trailer, tool-independent — S3) |
| `egress.proxy` | ✅ ANTHROPIC_BASE_URL | ❌ Agent egress non-interceptable (S1 B5) | ✅ base-URL/model_provider, proxy-interceptable (S5§6) |
| *org mandate* | managed settings | Team hooks (Enterprise) | **requirements.toml + MDM, best-in-class** (S5§3) |

**Two capabilities are provider-independent and therefore always available:** `identity.register` (OpenBox registration) and `commit.binding` (git-level, S3). So even a provider with *no* usable in-tool surface still yields session→commit→deploy lineage and finops-by-registration — the floor of coverage never drops to zero.

**S5 finding — Codex is a STRONG governable provider** (roughly Claude-Code-parity, best-in-class org mandate via `requirements.toml`/MDM). Two Codex-specific gaps the adapter must handle: (a) no `CODEX_SESSION_ID` env var → capture session id inside a `SessionStart` hook to stamp the trailer; (b) Usage/Cost API attributes per-key/project, not per-session → provision per-developer keys/projects to bridge finops→session. Hooks are behind `features.hooks` (beta) — pin a Codex version.

**S6 correction — `telemetry.push` (OTel) is NOT an ingest differentiator.** OpenBox has no OTLP receiver (S6 §2), so *every* provider's adapter translates telemetry into `GovernanceEventPayload` and POSTs to `/evaluate` — the "native OTel" column above matters only for how the adapter *reads* local telemetry, not for a privileged path into OpenBox. The real differentiators across providers remain **hooks (enforcement, Phase 2)** and **org mandate**. Two capabilities are provider-independent and always available: `identity.register` (agent/create) and `commit.binding` (git trailer). Also verified by S6: developer sessions register via free-form `agent_type="developer"` (no `kind` enum, no migration), and the adapter must implement **AIP Ed25519 request signing** (`signing_required` defaults true) unless dev agents are registered signing-off (OD16).

### Derived governance level (shown honestly in the UI)

Per provider/session, OpenBox derives a coverage tier from the declared capabilities and displays it — no false sense of coverage (matches S1's "no silent caps"):
- **Observe** — `telemetry.*` + `session.lifecycle`/`tool.events` (metadata). Phase-1 target for every provider.
- **Advisory** — + guardrail/policy signals recorded but not blocking. ***ADOPTED** as an explicit Phase-1.5 increment (STORY-SL-9, OD-ADV — brian 2026-07-13): `client.Emit` returns a rich `Evaluation` (verdict + trust/risk/alignment scores, constraints, guardrail categories) and the Claude Code adapter + git action RECORD what would be enforced (a `would_block` label + guardrail hits) to a local `advisories.jsonl` sink off the hot path — record-only, metadata-only, never blocking (INV-2/INV-3). One flag short of Enforce.*
- **Enforce** — + `enforce.decision`/`enforce.rewrite` honored, fail-closed (OD9). Phase 2, only where the capability exists (Claude Code strong; Cursor caveated by fail-open beta; Codex per S5).

### Graceful degradation / negotiation

- Telemetry: prefer `telemetry.push`; else `telemetry.poll`; else derive from `telemetry.hook`. Claude Code = push; Cursor = poll + hook; Codex = per S5.
- Enforcement absent → provider is observe-only; the dashboard reflects it rather than pretending.
- Poll-only providers (Cursor, maybe Codex) need a **server-side poller** in the OpenBox backend (not on the dev machine); push/hook providers do not. This is the one place the ingestion topology varies by provider.

### Adding a new provider = one adapter + one surface spike

Onboarding a new coding agent is: (1) run an S1-style **surface spike** to fill its capability row with evidence, (2) implement the adapter (`register`/`emit`/`apply`/`capabilities`) against the normalized contract, (3) package it natively. **No core/backend change** — the normalized event schema and the `/api/v1/governance/evaluate` pipeline are untouched. This is the concrete meaning of "generic architecture, per-provider adapter."

---

## 2. Requirements → how they are met by reuse

**Functional (PRD §2):**
- FR-1 dev/install = agent, session = child → **reuse** `agent.service.ts:652` (new `kind=developer`) + existing `SessionEntity`.
- FR-2 telemetry → **adapter translates** Claude Code session telemetry (tokens/cost/tool decisions/MCP, from hooks/local OTel) into governance events POSTed to `/api/v1/governance/evaluate`; spans land via the existing payload (no OTLP intake — S6 §2).
- FR-3 per-tool/per-prompt events (observe-only) → **reuse** `POST /api/v1/governance/evaluate`; adapter treats all verdicts as allow in Phase 1.
- FR-4 tool-agnostic contract → the OpenBox governance-event schema **is** the contract; adapters map native payloads to it (Claude Code now, Cursor later).
- FR-5 session→commit binding → `OpenBox-Session: <session_id>` git trailer stamped by the adapter.
- FR-6 git action registers `DID = git hash + timestamp` → a new **`Deploy` governance event** carrying the DID, linked to `session_id`.
- FR-7 lineage read (session→commit→deploy + finops) → **reuse** `getSessionLogs` + spans; add a lineage query joining commit/deploy events.
- FR-8 reuse DID namespace + trust → same registry, same `add-trust-lifecycle-schema` records; developer events feed trust scoring exactly like agent events (`GovernanceEventService` injected at `agent.service.ts:253`).

**Nonfunctional (PRD §3):** NFR-1 privacy posture (content-capture ON by default as of 2026-07-15, opt-OUT per org — reverses the original metadata-default; OD4); NFR-2 <50 ms via Claude Code async hooks + fast Go binary invocation; NFR-3 observe fail-open; NFR-4 attribution (S3); NFR-5 org mandate via managed settings, pilot opt-in (OD10); NFR-6 reuse `AgentEntity.createHash`; NFR-7 retention (OD4).

---

## 3. Key technology decisions (all resolved toward reuse per the guiding principle)

- **D1 — No new ingestion service; onboard onto the existing governance pipeline.** Events flow to the **existing** openbox-core `EvaluateGovernanceEvent` (`governance.go:28`); read/serve via the **existing** backend `GovernanceEventService`. *(Resolves former OD-A → reuse openbox-backend + openbox-core as-is.)*
- **D2 — CORRECTED by spike S6: there is NO OTLP intake in OpenBox** (no receiver in core or backend; no `@opentelemetry` dependency). The ingestion path is: the **adapter translates** the tool's session telemetry (from its hooks / locally-read OTel) into a `GovernanceEventPayload` and **POSTs it to the existing `/api/v1/governance/evaluate`** on openbox-core (`content/governance.go:186`), which already accepts a `spans[]` array + metadata (with first-class `file_operation`/`function`/`mcp_tool_call` span fields) and persists via its workflow → datastore. **One ingestion path, no new collector.** *(Supersedes the earlier "reuse an OTLP/span path" claim — that path does not exist; verified S6 §2.)*
- **D3 — Reuse `SessionEntity`/`GovernanceEventEntity`/`SpanEntity`/`SessionMerkleLeafEntity`.** Developer sessions are `SessionEntity` rows; session events are `GovernanceEventEntity` (already `@Index` on `session_id`, `ManyToOne` → `SessionEntity`). The pre-existing `SessionMerkleLeafEntity` gives **tamper-evident lineage for free**, advancing the Phase-3 attestation goal at no extra cost. *(Resolves former OD-C → reuse `governance_events`, not new tables.)*
- **D4 — Extend the event-type enum, don't fork the schema.** Add developer-runtime *lifecycle* types alongside the existing Temporal ones: `SessionStarted`, `PromptSubmitted`, `ToolCall`, `ToolResult`, `SessionEnded`, `CommitCreated`, `Deploy`. **S6 confirmed this is exactly 3 additive, no-migration edits in core** — constants (`content/governance.go:12`), the accept-list `isValidGovernanceEventType` (`api/governance.go:273`), and the session-lifecycle switch (`storage_session.go:40`, map `SessionStarted`→create / `SessionEnded`→terminal). The *semantic span* types these events carry (`file_write`, `mcp_tool_call`, `llm_call`) already exist — the contract (E1-S1) must respect both the lifecycle and semantic axes.
- **D5 — Identity: reuse `agent/create` with `agent_type="developer"` (CORRECTED by S6 — there is no `kind` enum; `agent_type` is a free-form string, so this needs NO migration).** Registration mints `obx_` key + `did:aip:<uuidv5>` + KMS/Ed25519 signer + trust score inline, and **requires `aivss_config`** (the CLI supplies a default dev risk profile). Session ephemerality is captured by `SessionEntity` (NOT-NULL, jointly-unique `workflow_id`+`run_id` — the dev session synthesizes both) under the developer agent's DID. **The adapter must implement AIP Ed25519 request signing** (`signing_required` defaults true) — reuse the SDK's signing — unless dev agents are registered signing-off (OD16). *(Where this doc says "kind=developer" elsewhere, read it as this free-form `agent_type` value.)*
- **D6 — Adapters are the SDK-equivalent, the net-new deliverable, and implement the generic Provider Adapter Contract (§1b).** Each adapter is `register`/`emit`/`apply`/`capabilities` mapped onto one tool. Claude Code adapter = a plugin bundling hooks (`SessionStart`/`UserPromptSubmit`/`PreToolUse`/`PostToolUse`/`SessionEnd`) + managed-settings + native OTel config (push telemetry). Cursor adapter (fast-follow) = `hooks.json`/Team-hook bundle over `beforeSubmitPrompt`/`beforeMCPExecution`/`afterFileEdit` **plus a server-side Admin-API poller** (no OTel; S1 B4). Codex adapter = per spike S5. All emit the **same normalized event** to `/api/v1/governance/evaluate`; the core does not know which tool produced it.
- **D7 — Observe = report-only verdict handling.** Phase 1 adapters send events and ignore the verdict (or send with a report-only flag); Phase 2 flips the same adapters to honor deny/ask, fail-closed (OD9). Same channel, same events — only the client's response to the verdict changes.

---

## 4. Invariants

- **INV-1 (credential secrecy, NFR-6):** session `obx_` key stored/compared only as hash (`AgentEntity.createHash`); never in logs, spans, or event bodies.
- **INV-2 (privacy default, NFR-1; OD4 — default REVERSED 2026-07-15):** **Content capture is ON by default as of 2026-07-15** (brian; supersedes OD4's original metadata-only-by-default). Prompt content is captured onto emitted events and egressed unless an org opts OUT (`content_capture:false` / `OPENBOX_CONTENT_CAPTURE=0`, which restores the metadata-only projection: session/commit ids, DID, timestamps, tool/MCP names, model, tokens, cost, allow/deny). When content IS enabled it is meant to be redacted **at source via the existing Guardrail API** (PII/NSFW/regex) before egress, never blocking the tool call (INV-3) — but that layer is currently **inert** (`[EXT-guardrail-redaction]`), so with capture on, prompt content egresses **unredacted**. Structural guarantees that survive the default flip: tool **commands** and file **bodies** are still never carried on observe/T2 events (SL3-SEC-3 — the Mapper is metadata-only for tool events); Tier-1 local secret detection (E6-S9) redacts Write/Edit bodies but only in enforce mode and only on the LOCAL sidecar path. Ingestion still strips content fields when capture is disabled.
- **INV-3 (observation never blocks, NFR-3):** no Phase-1 dev-runtime path denies, delays past budget, or errors a developer tool call.
- **INV-4 (tenancy):** every session/event/span scoped to `organization_id`, as `AgentEntity`/`GovernanceEventEntity` already enforce; no cross-org read.
- **INV-5 (idempotent ingestion):** events carry a client id; retries/buffered flush never double-count tokens/cost or duplicate lineage links.
- **INV-6 (lineage integrity, NFR-4; rules from spike S3):** the commit-message trailer (`OpenBox-Session: <id>`, multiple lines allowed = fan-in, like `Co-Authored-By`) is the **single source of truth**, written idempotently in `prepare-commit-msg`, and **resolved server-side at push against the real pushed SHA — never a pre-push SHA** (git hooks are local and don't travel; SHAs are unstable until push). A commit resolves to a deduped session set (0..N) bound to the Deploy DID and anchored as a `SessionMerkleLeafEntity` leaf, OR an explicit `unattributed`/`inferred` marker with a reason (`no-trailer`|`trailer-stripped`|`non-agent`). Only loss modes: interactive `fixup` and GitHub squash-merge body-replace.
- **INV-7 (DID namespace unity, FR-8):** developer agents share the runtime-agent DID namespace and registry; no parallel identity store.
- **INV-8 (schema compatibility):** additive event types must not break existing Temporal governance consumers (`isValidGovernanceEventType`, trust scoring, dashboards).

---

## 5. Implementation guardrails

- **Reuse-first rule:** a story may not introduce a new table, endpoint, or service where an existing one (Session/GovernanceEvent/Span, `/api/v1/governance/evaluate`, `GovernanceEventService`) fits. Deviations require an ADR.
- **Server change is additive only:** developer event types extend the enum (D4); no change to the evaluate endpoint's contract or the governance_events shape in Phase 1.
- **Adapters depend on the event schema, never the reverse (FR-4):** author/version the developer event types first; both adapters run a shared conformance test.
- **Async/best-effort on the hot path (NFR-2/3):** adapters emit asynchronously (Claude Code `"async": true`); the Go `bin/openbox` invocation is fast (single-digit-ms cold start); no synchronous dependency on OpenBox for a tool call to proceed in Phase 1.
- **Secrets:** adapters read the `obx_` key + signer from the tool's secure config/env; the commit trailer carries only the opaque `session_id`, never the key.
- **Config flags explicit:** content-capture (INV-2) and retention (NFR-7) are explicit toggles. Content-capture now defaults **ON** (opt-OUT per org, 2026-07-15 — reverses the original privacy-safe-off default); retention still defaults short.
- **Disjoint write scopes for planning:** (a) core: developer event types in `content.EventType*` + `isValidGovernanceEventType`; (b) backend: `kind=developer` registration + developer-session read/lineage query in `GovernanceEventService`; (c) shared event-schema/conformance package; (d) Claude Code adapter (plugin); (e) OpenBox git action `Deploy` event. openbox-core touched only for (a).

---

## 6. Risks & deferred

- **R1 (blocking schema):** privacy/content boundary (OD4) → **spike S4** before freezing developer event types / content fields. Build metadata-only until then.
- **R2 (attribution):** multi-session/squash/rebase/fork (NFR-4/INV-6) → **spike S3** defines rules before slicing FR-5/FR-6.
- **R3 (Phase-2 latency):** fail-closed enforcement (OD9) depends on evaluate-endpoint round-trip latency from the developer machine → **spike S2**; may need a local OPA sidecar (OPA bundles already distributed, `docs/diagram/openbox.png`). Out of Phase-1 scope.
- **R4 (Cursor divergence):** no OTel, poll-based Admin API, non-interceptable Agent egress (S1 B4/B5) → Cursor adapter needs an Admin-API poller; the event schema must not assume push-only. Fast-follow.
- **R5 (event-type coupling):** adding developer types to a Temporal-oriented enum (D4) risks consumers that switch on type → INV-8 conformance test; audit `governance.evaluated` consumers before merge.
- **R6 (OTel drift):** Claude Code metric/event names version-gated (v2.1.200+); pin and monitor.

---

## 7. Open human decisions (owners; not inferred)

Former OD-A/OD-B/OD-C are **resolved to reuse** by the sponsor's guiding principle and recorded as D1/D2/D3. Remaining:

| ID | Decision | Owner | Blocks |
|---|---|---|---|
| **OD4** | **DECIDED (2026-07-07, spike S4): metadata-only default; content strictly opt-in per org; when enabled, Guardrail-redacted at-source async.** Folded into INV-2/INV-3. **SUPERSEDED (2026-07-15, brian): default REVERSED to content-capture ON (opt-OUT per org)** — see INV-2. The opt-out mechanism + the at-source-redaction intent are retained (redaction still inert, `[EXT-guardrail-redaction]`); the event schema's content fields are unchanged (only the default toggle flipped). Retention (NFR-7) + residency remain orthogonal knobs; legal follow-up (WP29/EDPB currency, DPIA trigger) with counsel — now more salient with content on by default. | brian (product/security) | Resolved; default reversed 2026-07-15. |
| **OD6** (carried) | Phase-2 hook handler type for enforcement (http vs command→local sidecar). | brian (technical) | Phase-2 only. Gate on **S2**. |
| **OD10** (carried) | Name the pilot team/repo (mandate posture already decided: opt-in; validate GitHub-squash prevalence per S3 U-1). | brian (product) | Rollout/measurement. |
| **OD12** | **DECIDED (2026-07-07), REVISED by OD18 (2026-07-08):** unifying `openbox` CLI front door — delivery/wiring uses each tool's native bundle: on Claude Code the CLI installs a **plugin bundling `bin/openbox` + hooks** (marketplace + managed `enabledPlugins`), on Codex/Cursor it lays down the config+managed-hooks bundle. | brian (product/UX) | Resolved (see OD18). |
| **OD18** | **DECIDED (2026-07-08): hybrid — plugin vehicle + CLI engine.** CC plugin bundles the Go binary (`bin/`, OD17) + hooks; `openbox dev init` installs it; CLI stays uniform front door + CI/git-action path. Revises OD12. | brian (product) | SL-2/SL-4 packaging (resolved). |
| **OD19** | **DECIDED (2026-07-08): Phase-1 CC plugin bundles `bin/openbox` + hooks only.** OpenBox MCP server (Phase-2 candidate) and skills/agents (later) deferred. | brian (product) | SL-4 scope (resolved). |
| **OD20** | Distribution channel: private CC marketplace + managed `enabledPlugins`; Codex `requirements.toml`/MDM; Cursor Team hooks — confirm org enterprise tiers. | brian (product/ops) | Rollout (ties OD10). |
| **S5** | **DONE (2026-07-07):** Codex is a STRONG governable provider (hooks block+rewrite, native OTel, MCP, base-URL egress, best-in-class requirements.toml/MDM mandate). Capability row filled above. `.fab7/sdlc/discovery/spikes/S5-codex-surfaces.md`. | brian (owner) | Resolved (Codex adapter unblocked). |
| **S6** | **DONE (2026-07-07):** deep-dive of openbox-core/backend/document. Reuse thesis holds; corrections applied (D2 no-OTLP, D4 3-edit precision, D5 agent_type). `.fab7/sdlc/discovery/spikes/S6-external-components-review.md`. | brian (owner) | Resolved. |
| **OD16** | **DECIDED (2026-07-07): sign — reuse SDK.** The adapter implements AIP Ed25519 request signing on every `/evaluate` call (canonical `METHOD\npath\ntimestamp\nnonce\nbodySHA256` + `X-OpenBox-Agent-*` headers), reusing openbox-temporal-sdk-python's signing; matches the `signing_required=true` platform default. Adds an E2 adapter requirement: secure Ed25519 key handling on the dev machine. | brian (security) | E2 adapter (resolved). |
| **OD15** (new) | Lineage storage: metadata JSONB (no migration, unindexed) vs indexed columns/lineage table (migration, queryable). The project's only real schema change. | brian (architecture/data) | E1-S5 / E3 / FR-7. External — deferrable. |
| **OD17** | **DECIDED (2026-07-07): Go.** The `openbox` CLI, the AIP-signed `/evaluate` client, and adapter shims are Go — single static binary, `crypto/ed25519` stdlib for AIP signing, matches openbox-core; adapters are thin per-tool shims that shell out to the binary. **Re-confirmed 2026-07-09** — a proposed switch to Python + `uv tool` (to match spec-kit distribution + the Temporal SDK) was **declined**: Phase-1 (SL-1..SL-4) was already built/reviewed/committed in Go (45 files, 4 modules), and Go's static `bin/` fit (OD18) + per-tool-call hook latency (NFR-2) outweigh the marginal signing-reuse/ecosystem gain. `uv tool` is Python-only, so not adopted. | brian (tech) | SL-2/SL-3/adapters (resolved). |

---

## 8. Traceability

Every §2 requirement maps to a §3 reuse decision and ≥1 §4 invariant; every reuse claim cites a repo symbol/path (agent.service.ts:652, governance.go:28/273, hook_governance.py:171/381, span_processor.py, governance-event.entity.ts, GovernanceEventService.getSessionLogs) or an S1 doc URL; unknowns (OD4/S3/S4) are marked, not invented. Ready for `readiness-check` → planning once S3/S4 land; the reuse mapping (§1) is the spine for epic slicing.
