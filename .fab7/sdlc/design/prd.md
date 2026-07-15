# PRD — Shift-Left Developer-Runtime Governance (Phase 1: Observe-First)

**Status:** draft
**Author:** Paul (Product Manager) — 2026-07-07
**Sources:** `.fab7/sdlc/discovery/brief.md`, `.fab7/sdlc/discovery/spikes/S1-dev-runtime-surfaces.md`, `docs/diagram/{openbox,shift-left,diagram}.png`
**Phase scope:** Phase 1 (observe-first). Enforcement is Phase 2, explicitly out of scope here.
> **SUPERSEDED for status (2026-07-15):** this PRD's Phase-1 scoping is its historical framing. **Phase 2 enforcement (Epic E6, S1–S11) and E7 SDK wire-unification (S0–S8) have since SHIPPED** (opt-in enforce). Live status is the ledger `.fab7/sdlc/status.yaml` + `CLAUDE.md`; this doc is kept for design context, not current scope.

---

## 1. Problem, Users, Value

- **Problem (one sentence):** OpenBox governs *agent runtime* via SDK/code binding but has zero visibility or governance over the *developer runtime* — the agentic coding sessions (Claude Code, Cursor) that actually produce the code, so there is no answer to "how was this commit produced, with what tools/MCP/prompts, at what token cost?" (brief §Context; `docs/diagram/diagram.png`).
- **Target users (one sentence each):**
  - *Platform/security owner* — needs provenance and governance coverage that starts at the developer's keyboard, not just at deploy (brief §Context "Developer Governance").
  - *Engineering developer* — uses Claude Code/Cursor daily and needs governance that is near-zero friction and privacy-respecting (brief C3).
  - *Compliance/finops stakeholder* — needs per-session/per-commit token & cost attribution and an auditable session→commit→deploy lineage (brief §Context goal 2; `docs/diagram/shift-left.png`).
- **Value (one sentence):** Establish a trustworthy, low-friction lineage from dev session → commit → deploy → runtime agent, reusing existing OpenBox primitives, so governance and finops "shift left" to where code is created (brief §Context; S1 verdict).

**Decided posture carried in from discovery:** observe-first (OD1); Claude Code first, tool-agnostic contract, Cursor fast-follow (OD5/OD8); dev sessions modeled as agents in the existing registry (OD3); fail-closed for deny-class checks — a Phase-2 concern (OD9).

---

## 2. Functional Requirements

Each FR cites its source observation and names the capability it implies. IDs are stable for traceability.

| ID | Requirement | Source | Capability implied |
|---|---|---|---|
| **FR-1** | The **developer/tool install** is registered as a first-class OpenBox agent (durable DID + trust, reusing `POST agent/create` → `obx_` key); **each session is a child record** under that agent (OD11 hybrid). | brief option A1; S1 "sessions as agents" (OD3); OD11 decided; explore run: `openbox-backend/src/modules/agent/agent.controller.ts:187`, `agent.service.ts:652` | Extend agent registration to a developer agent kind + a child `session` record type; issue/attach credential to the coding tool. |
| **FR-2** | The adapter translates Claude Code session telemetry (token usage, cost, tool decisions, MCP connections — from hooks/local OTel) into normalized governance events and POSTs them to the existing `/api/v1/governance/evaluate` endpoint. **(CORRECTED by S6: OpenBox has no OTLP intake — there is no collector to reuse; the single ingestion path is `/evaluate`.)** | S1 A4; **S6 §2**; brief option A1 | Adapter-side telemetry→event translation; reuse core's existing `spans[]`/metadata ingest. |
| **FR-3** | The system captures per-tool-call and per-prompt session events via a Claude Code hook (`SessionStart`, `UserPromptSubmit`, `PreToolUse`/`PostToolUse`, `SessionEnd`) delivered to an OpenBox ingestion endpoint. In Phase 1 the hook is **observe-only** (never denies). | S1 A1 (code.claude.com/docs/en/hooks.md); brief option A1/B1 (observe subset) | A tool-agnostic OpenBox **hook event contract** + ingestion API; a Claude Code adapter implementing it. |
| **FR-4** | The hook/ingestion contract is defined tool-agnostically so a Cursor adapter (`hooks.json`, `beforeSubmitPrompt`/`beforeShellExecution`/`beforeMCPExecution`/`afterFileEdit`) can implement the same contract later without changing the OpenBox side. | S1 §Cross-tool synthesis; OD5/OD8 | Stable event schema decoupled from any one tool's payload shape. |
| **FR-5** | A commit is bound to its originating session(s) via a commit trailer (e.g. `OpenBox-Session: <session_id>`) stamped by a session hook or `prepare-commit-msg`. | brief option A2; S1 A5 (Co-Authored-By precedent) | Session→commit binding written into git metadata; attribution tolerant of multi-session commits (S3). |
| **FR-6** | The OpenBox git action reads the session trailer at push/deploy, registers `DID = git hash + timestamp`, and links the deploy DID to the originating session(s). | brief option A2; `docs/diagram/shift-left.png` (DID = git hash → timestamp; git action) | Deploy-time provenance record joining session → commit → deploy. |
| **FR-7** | A query/read surface returns the full lineage for a given commit or deploy DID: originating session(s), tools/MCP used, prompt count, and total token/cost (finops). | brief §Context goal 2; `docs/diagram/diagram.png` ("How did the developer produce this commit? … token usage?") | Lineage read API/view over the session-event + provenance stores. |
| **FR-8** | Session→agent identity reuses the existing DID namespace and trust records rather than a new store. | OD3; explore: `openbox-backend/src/migrations/1773076136339-add-trust-lifecycle-schema.ts` | One registry/DID namespace for developer and runtime governance. |

**Assumptions (not FRs — flagged):**
- A-2: policy-eval latency (spike S2) — irrelevant to Phase 1 (observe-only), blocks Phase 2.
- A-6: Cursor extended hook event set unconfirmed — affects only the deferred Cursor adapter.

---

## 3. Nonfunctional Requirements

Each NFR has an owner and a test strategy.

| ID | NFR | Owner | Test strategy |
|---|---|---|---|
| **NFR-1 Privacy** | **Default REVERSED 2026-07-15 (brian):** content capture is now **ON by default** (opt-OUT per org via `content_capture:false` / `OPENBOX_CONTENT_CAPTURE=0`); the metadata-only projection (tokens, tool names, model, hashes) is what opting out yields. Reverses the original metadata-only-by-default. | Security owner (brian) | Assert default config now emits content fields (prompt); `content_capture:false` / env=0 strips them back to metadata-only. Redaction-at-source still inert (`[EXT-guardrail-redaction]`) → content egresses unredacted with capture on. |
| **NFR-2 Low friction / latency** | Observe-only hooks must not perceptibly slow sessions; telemetry emission is async/non-blocking. | Tech lead | Measure added per-tool-call latency with the OpenBox hook installed; target < 50 ms overhead p95; use Claude Code async hooks (`"async": true`). |
| **NFR-3 Ingestion reliability** | Telemetry/hook delivery tolerates transient OpenBox outages without breaking the developer's session (fail-open **for observation**; distinct from Phase-2 enforcement fail-closed). | Tech lead | Kill the collector mid-session; confirm session continues and events buffer/drop-with-log, no developer-visible error. |
| **NFR-4 Attribution integrity** | Session→commit binding is correct under squash/rebase/fork and multi-session commits. | Tech lead | Test matrix over rebase/squash/`--fork-session`; each commit resolves to ≥1 correct session or a logged "unattributed" (spike S3 defines rules). |
| **NFR-5 Org mandate** | The Claude Code adapter *supports* org-wide force-enable via managed settings (`enabledPlugins`, `allowManagedHooksOnly`) with MDM-file fallback, but the **Phase-1 pilot is opt-in** (OD10); the mandate path is verified, not activated. | Platform owner | Verify managed-settings force-enable prevents user disable (in a test fixture); verify `/etc/claude-code/managed-settings.json` path works on a Bedrock/Vertex-auth client; pilot rollout uses voluntary install. |
| **NFR-6 Identity security** | Dev-session `obx_` credential is stored/transmitted as a hash, never logged in cleartext; reuses existing token hashing. | Security owner | Reuse/verify `AgentEntity.createHash`; assert no cleartext key in logs/telemetry. |
| **NFR-7 Data retention** | Session-event and lineage data have a defined retention aligned with org policy. | Compliance stakeholder | Retention config present and enforced; deletion path verified. **Gated on OD4.** |

---

## 4. Success Metrics

- **M1 (coverage):** % of Claude Code sessions in a pilot team registered and emitting telemetry to OpenBox (target: ≥ 90% once managed-settings mandate is on).
- **M2 (lineage completeness):** % of deploys whose DID resolves to a complete session→commit→deploy chain (target: ≥ 80% of governed-session commits).
- **M3 (finops answerability):** For a sampled commit, the system returns tokens/cost and tools/MCP used (binary: answerable / not) — target 100% for governed sessions.
- **M4 (friction):** Added p95 per-tool-call latency from the OpenBox hook (target < 50 ms; NFR-2).
- **M5 (adoption sentiment):** Pilot developers report no material workflow disruption (qualitative pilot survey).

---

## 5. Out of Scope / Deferred

**Out of scope for Phase 1 (observe-first):**
- **Enforcement** — `PreToolUse` deny/ask, prompt/output rewrite, policy evaluation against OPA. Deferred to **Phase 2**, gated on spike S2 (latency) and decision OD6 (handler type). Fail-closed posture (OD9) applies there.
- **Cursor adapter** — contract designed for it (FR-4) but implementation is **fast-follow**, gated on confirming A-6.
- **LLM egress proxy / MCP gateway** — Claude-Code-only optional extra at best; non-viable for Cursor Agent mode (S1 B5). Not in Phase 1.
- **KMS-attested provenance** (tamper-evident session→commit signing) — Phase 3.

**Explicitly rejected (S1):**
- Parsing Claude Code transcript JSONL (format unstable) or relying on Cursor server-side content logs (none exist) — ingestion rides on OTel events + hook payloads.
- Forking CLI/IDE binaries; eBPF/kernel interception; keystroke/screen capture (brief §Discarded).

---

## 6. Open Decisions (human-owned — not resolved here)

| ID | Decision | Owner | Blocks |
|---|---|---|---|
| **OD4** | **DECIDED (2026-07-07, spike S4): metadata-only default; content opt-in per org; Guardrail-redacted at-source async when enabled.** **SUPERSEDED (2026-07-15, brian): default REVERSED to content-capture ON (opt-OUT per org)** — see NFR-1 / architecture INV-2. Mechanism + at-source-redaction intent retained (redaction still inert); schema content fields unchanged, only the default toggle flipped. | brian (product/security) | Resolved; default reversed 2026-07-15. |
| **OD6** | Hook handler type — `http` direct to OpenBox Core vs `command`→local OpenBox daemon/OPA sidecar. | brian (technical) | Adapter architecture (affects Phase 2 more than Phase 1). Gate on spike S2. |
| **OD7** | Whether Phase-1 telemetry includes any content or is strictly metadata (ties to OD4). | brian (product) | Ingestion scope. |
| **OD10** | **DECIDED (2026-07-07): opt-in during pilot.** Managed-settings mandate is verified but not activated for the pilot team; flip on after friction/coverage metrics. Specific pilot team/repo still to be named. | brian (product) | Rollout plan (pilot team TBD). |
| **OD11** | **DECIDED (2026-07-07): hybrid — developer/install = agent (durable DID + trust), session = child record.** Folded into FR-1/FR-8. | brian (technical/product) | Resolved. |

---

## 7. Traceability summary

Every FR above cites a brief option or an S1 finding with a doc/repo path; every NFR has an owner and a test strategy; every product decision is an open decision with an owner. Requirements map to capabilities that the planning phase will slice into epics/stories. `prd-trace` can verify FR→capability→test coverage before planning.

---

## 8. Epics (Phase 1)

Sliced by coherent outcome, aligned to the architecture §1b provider-agnostic-core + per-adapter split so write scopes stay disjoint. **Every Phase-1 FR maps to exactly one epic** (coverage table at the end).

### E1 — Provider-agnostic governance core
**Goal:** OpenBox can register a developer session as an agent and ingest, store, normalize, and read its session events through the **existing** governance pipeline, tool-agnostically — the reusable substrate every adapter plugs into.
**Covers:** FR-1 (developer=agent `kind`, session=child), FR-4 (tool-agnostic normalized event contract + additive event types), FR-7 (lineage read surface: session→commit→deploy + finops), FR-8 (reuse DID namespace + trust records).
**Also builds (enabling infra for E2+):** the ingestion path onto `/api/v1/governance/evaluate`, additive developer event types with an INV-8 conformance test, and OTLP intake reuse — tested end-to-end via E2.
**Acceptance themes:**
- A developer/tool-install registers as an agent (`kind=developer`) reusing `POST agent/create`; sessions persist as child `SessionEntity` under its DID; credential stored only as hash (NFR-6).
- A versioned, tool-agnostic developer event schema exists; adding developer event types does not break existing Temporal governance consumers (INV-8 conformance test passes).
- Ingested events store as `GovernanceEventEntity`/`SpanEntity` scoped to `organization_id` (INV-4); ingestion is idempotent (INV-5); content capture is ON by default as of 2026-07-15 (opt-OUT strips content back to metadata-only) (NFR-1/INV-2).
- The lineage read surface returns, for a commit or deploy DID, the originating session(s), tools/MCP used, prompt count, and total tokens/cost (FR-7); retention is configurable (NFR-7).
**NFR themes:** NFR-1, NFR-4 (store side), NFR-5 (registration side), NFR-6, NFR-7.

### E2 — `openbox` CLI + Claude Code adapter (observe-only)
**Goal:** A Claude Code developer is onboarded in one command and their session emits governed telemetry (content-capture ON by default as of 2026-07-15; opt-OUT restores metadata-only) and is bound to the commits it produces — the first realization of the adapter contract.
**Covers:** FR-2 (ingest Claude Code native OTel end-to-end), FR-3 (per-tool/per-prompt session events, observe-only), FR-5 (session→commit trailer).
**Acceptance themes:**
- `openbox dev init --provider claude-code` (OD12) detects the tool, writes its native config (plugin hooks + settings + OTel export), and registers the session-as-agent against E1.
- A running Claude Code session's token/cost/tool-decision telemetry and per-tool/per-prompt events (`SessionStart`/`UserPromptSubmit`/`PreToolUse`/`PostToolUse`/`SessionEnd`) arrive at OpenBox as normalized events (FR-2/FR-3); Phase-1 is **observe-only** — no verdict is enforced (INV-3).
- Emission is async/best-effort: adds <50 ms p95 per-tool-call overhead and never blocks or errors a tool call if OpenBox is unreachable (NFR-2/NFR-3).
- Content capture is ON by default as of 2026-07-15 (opt-OUT per org restores metadata-only); when on it is meant to be Guardrail-redacted at-source async, though that layer is currently inert (NFR-1/OD4).
- Commits are stamped with an idempotent `OpenBox-Session:` trailer (multiple lines = fan-in) via `prepare-commit-msg` (FR-5, per S3 rules).
- The Claude Code adapter supports org-wide force-enable via managed settings; **Phase-1 pilot is opt-in** — mandate path verified, not activated (NFR-5/OD10).
**NFR themes:** NFR-1, NFR-2, NFR-3, NFR-5.

### E3 — OpenBox git action (deploy lineage)
**Goal:** Every deploy is provably linked to the session(s) that produced it, resolved authoritatively at push.
**Covers:** FR-6 (git action registers `DID = git hash + timestamp` and links to originating session(s)).
**Acceptance themes:**
- At push/deploy, the action resolves `OpenBox-Session` trailers **server-side against the real pushed SHA** (never a pre-push SHA), deduping to a session set (S3/INV-6).
- Registers a `Deploy` governance event carrying the DID (via existing `didFor`/`createHash`), bound to the session set and anchored as a `SessionMerkleLeafEntity` leaf.
- A commit with no trailer yields an explicit `unattributed`/`inferred` marker with a reason (`no-trailer`|`trailer-stripped`|`non-agent`) — never a silent wrong attribution (INV-6); merge commits attribute reachable originals, not the merge node.
**NFR themes:** NFR-4 (attribution integrity, incl. the S3 rewrite/squash test matrix).

### Requirement → epic coverage

| FR | Epic | | FR | Epic |
|---|---|---|---|---|
| FR-1 | E1 | | FR-5 | E2 |
| FR-2 | E2 | | FR-6 | E3 |
| FR-3 | E2 | | FR-7 | E1 |
| FR-4 | E1 | | FR-8 | E1 |

All 8 Phase-1 FRs covered; none duplicated. FR-4 (tool-agnostic contract) is what makes the fast-follow adapter epics possible without new FRs.

### Dependencies & sequencing
- **E1 → E2:** E2's emission depends on E1's registration + ingestion endpoint + event schema. Build E1 first (or E1's contract + endpoint before E2's emitters).
- **E2 → E3:** E3 resolves the trailers E2 stamps; the Deploy event links to sessions E2 registered. E3 follows E2.
- **E1 lineage read (FR-7)** consumes E3's Deploy events for the deploy leg — deliverable incrementally (session→commit first, +deploy after E3).

### Deferred / fast-follow epics (not Phase-1 build path)
- **E5 — Codex adapter** then **E4 — Cursor adapter** (fast-follow): each re-realizes FR-2/FR-3/FR-5 for its tool against E1's contract; surface spikes done (S1/S5). No new PRD FRs. **OD13 DECIDED (2026-07-07): Codex next, then Cursor** — S5 shows Codex is the stronger, lower-risk surface (block+rewrite hooks, native OTel, best-in-class requirements.toml/MDM mandate) vs Cursor's beta/fail-open/non-interceptable-egress. Revises the pre-S5 "Cursor fast-follow" assumption.
- **E6 — Phase-2 enforcement:** flip adapters to honor deny/ask/rewrite, fail-closed (OD9). Out of Phase-1 scope (PRD §5); **blocked on spike S2 + OD6.**
