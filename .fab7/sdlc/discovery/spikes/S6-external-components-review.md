# Spike S6 — Deep-dive review of openbox-core, openbox-backend, openbox-document

**Question (one sentence):** For shift-left, what EXISTS today across the external OpenBox components vs what would GENUINELY need to change — so the "assume external changes are made" decision (OD14) rests on verified reality?

**Status:** DONE (2026-07-07). **Method:** three parallel code/docs-verified deep dives (general-purpose agents), file:line-cited. **Owner:** brian.
**Verdict:** the reuse thesis HOLDS. Registration is additive-zero; telemetry ingestion is client→core `/evaluate` (NOT OTel); lineage is the one genuine schema gap. Several design assumptions corrected (below).

---

## 0. The plane split that governs everything (cross-repo fact)

- **openbox-backend = control plane** — register + configure + **read** only. Auth = **Keycloak JWT**. It generates/hashes `obx_` keys but **does not validate them at ingest**. No session/event/span *write* path here (`session.service.ts` has no create; `governance-event.service.ts` only reads + `decideApproval`).
- **openbox-core = data/runtime plane** — **ingests** via `POST /api/v1/governance/evaluate`, validates `obx_` token + optional AIP signature, persists sessions/events/spans/merkle.
- **openbox-document = Docusaurus product docs** (docs.openbox.ai) — SDK+dashboard facing; no SPEC/RFC/formal AIP protocol; no published REST contract for `/evaluate`.

**Implication:** shift-left touches two planes — register at backend, emit events at core — and the docs are a conformance reference, not an API spec.

---

## 1. Registration (FR-1/FR-8) — reusable, additive-ZERO
- `POST agent/create` works today (`agent.controller.ts:187`, `agent.service.ts:652`). `CreateAgentDto.agent_type` is a **free-form string ≤100 chars** — `agent_type:"developer"` works with **no migration**. (Existing values are framework families `temporal|mastra|cloudflare` — migration `1778169600000`.)
- Registration mints `obx_live_/obx_test_` key (SHA-256 hashed, `AgentEntity.createHash`), `did:aip:<uuidv5(agentId, namespace)>` (`naming.ts:8`), KMS signing key, and initializes trust score inline from **`aivss_config` (REQUIRED on create)** via `calculateAivssScore`→`calculateTrustScore`.
- **Correction to OD3/OD11 language:** there is **no `kind` enum** — it's the free-form `agent_type` field (docs call it "Workflow Engine"/framework). "developer" registers with zero schema change; an enum + list filter is *optional* additive polish, not required.
- **Concrete adapter requirement:** registration must supply `aivss_config` (required) — the CLI needs a sane default risk profile for a dev agent.

## 2. Telemetry (FR-2/FR-3) — path is client→core `/evaluate`, NOT OTel
- **CORRECTION (kills architecture D2's "reuse OTLP path"):** there is **no OTLP/OpenTelemetry receiver anywhere** — not in core (only 4 HTTP routes), not in backend (no `@opentelemetry/*` dependency). Spans arrive **only** as the `spans[]` array inside `GovernanceEventPayload` on `POST /evaluate` (`content/governance.go:217`).
- So the correct, cheap path: the **adapter translates** the coding tool's session telemetry (read from its hooks / local OTel) into `GovernanceEventPayload` JSON and POSTs it to `/api/v1/governance/evaluate`. One ingestion path, not two.
- **Provider-leveling consequence:** Claude Code's "native OTel" (S1) is **not** a free ride into OpenBox — every provider's adapter translates to `/evaluate` regardless. The telemetry-push row in the §1b capability matrix is therefore not a real differentiator; hooks (enforcement) and org-mandate remain the true differentiators.
- **Good news — the span model already fits:** `SpanData` has first-class `file_path/file_operation/bytes_*`, `function/module/args/result`, and semantic types `file_read/write`, `mcp_tool_call`, `llm_tool_call`, `llm_completion` (`content/governance.go:266-318`, `content/session.go:95-128`). Claude Code/Codex/Cursor tool calls map on with **no span-schema change**.

## 3. Core additive changes to ACCEPT developer events (the deferred E1-S3/S4)
Confirmed **small, additive, no migration** — exactly 3 touch-points:
1. Event-type constants — `content/governance.go:12-20` (add `SessionStarted/PromptSubmitted/ToolCall/ToolResult/SessionEnded/CommitCreated/Deploy`).
2. Accept-list — `isValidGovernanceEventType` `api/governance.go:273` (7 types today; `default:false` rejects the rest → 400).
3. Session lifecycle switch — `storage_session.go:40` (map `SessionStarted`→create, `SessionEnded`→terminal; others fall through to lookup as mid-session).
Downstream (OPA, guardrails, AGE, storage, merkle attestation, webhooks) is event-type-agnostic once allowlisted. Optional additive: guardrails-eligibility predicate, metadata extractor cases.

## 4. Lineage (FR-7) — the ONE genuine schema gap
- **No commit/deploy/git/DID-from-git/lineage concept exists** in core OR backend (grep-confirmed both). `session_handoffs` + `multi_agent_session_id` are agent→agent only.
- Options: (a) **metadata JSONB** on session/event (no migration; unindexed → poor lookup) — carries `{commit_sha, repo, branch, deploy_id}`; (b) proper **indexed columns / lineage table** (additive migration; queryable).
- **Per-session cost is NOT attributable today** — observability rollups are keyed `agent_id + bucket_time`, not `session_id` (`observability-metric.entity.ts`). Additive session-keyed rollup or span-attribute sum needed for finops-per-session.
- Sessions require **NOT-NULL, jointly-unique `workflow_id`+`run_id`** — a dev session must synthesize both (e.g. `workflow_id`=repo/agent identity, `run_id`=session/commit id). Modeling constraint, not a blocker.
- FR-7 read query is **net-new** (join targets exist; entry point + commit link do not) → lives on `governance-event.service.ts`/`session.service.ts` + a new route. This is the deferred E1-S5 + commit-storage; it carries the project's **only real schema change**.

## 5. Identity / AIP — signing is REQUIRED by default (adapter must sign)
- `did:aip:<uuidv5>`, **Ed25519**, `obx_live_/obx_test_`; private key = base64 raw 32-byte seed. Env: `OPENBOX_URL` (=`https://core.openbox.ai`), `OPENBOX_API_KEY`, `OPENBOX_AGENT_DID`, `OPENBOX_AGENT_PRIVATE_KEY`.
- **`signing_required` defaults TRUE at provisioning** (backend sets it; core enforces when true, `services/agent.go:108`). Canonical signed string = `METHOD\npath\ntimestamp\nnonce\nbodySHA256`; headers `X-OpenBox-Agent-{DID,Timestamp,Nonce,Signature}` + `X-OpenBox-Body-SHA256`.
- **Correction (softens earlier "AIP optional"):** unless a dev agent is registered with `signing_required=false`, the **adapter MUST implement Ed25519 request signing** — reuse `openbox-temporal-sdk-python`'s signing logic. This is a concrete E2 adapter requirement, and a security-posture decision (OD16 below).

## 6. Privacy (OD4) — metadata-only is STRICTER than the platform norm
- Existing SDKs send **content** (prompts/completions) with PII redaction (`redactPaths`); default is telemetry-only-**with-content**. Our metadata-only default is a **deliberate divergence** to document explicitly, not a default to inherit.
- The Guardrail API is a separate service the backend proxies (`GuardrailService.runTest` → `${GUARDRAIL_API_URL}/guardrails/run-test`) — reusable for the content-redaction path.

## 7. Vocabulary / terminology to pin
- **Verdicts:** canonical `HALT > BLOCK > REQUIRE_APPROVAL > ALLOW`; docs also show UI `WARN` and OPA-return `CONTINUE`. Pin the canonical set in the E1-S1 contract.
- **Event-type axes (important nuance):** docs describe ~24 **semantic** types (span-level: `FILE_WRITE`, `LLM_CALL`, `TOOL`…) while core has 7 **lifecycle** types (`WorkflowStarted`, `ActivityStarted`, `Handoff`). Developer events add *lifecycle* types; the *semantic span* types already exist. The E1-S1 contract must respect both axes.
- **Scoring term:** code uses **AIVSS** (`calculateAivssScore`, `aivss_config`); docs call it **Trust Score / Risk Profile**. Align terminology in shift-left docs.

## 8. Roadmap validation (openbox-document)
- Shift-left is **already on OpenBox's public roadmap**: "coming soon" stubs for **Cursor** ("governed via its official hooks system… every prompt, shell command, MCP tool call, file read… ALLOW/BLOCK/REQUIRE_APPROVAL… full session replay") and **OpenClaw** (before/after tool-call hooks + local gateway PII + OTel span capture, **fail-open**). Shift-left realizes these stubs; docs work should fill them.
- Fail posture: OpenClaw documented **fail-open** (matches our Phase-1 observe); our Phase-2 fail-closed (OD9) diverges — document.

---

## What "assume external changes are made" (OD14) concretely means — VERIFIED

| Shift-left need | External change required | Size |
|---|---|---|
| Register dev agent | none (agent_type free-form) | **zero** |
| Core accepts developer events | 3 additive edits (constants, allowlist, lifecycle switch) | **small, no migration** (E1-S3/S4) |
| Store commit/deploy lineage | metadata JSONB (no migration) OR indexed columns/table | **stopgap zero / proper = 1 migration** (E1-S5 + commit storage) |
| Per-session finops | session-keyed rollup or span sum | additive |
| FR-7 lineage read API | net-new query + route | additive, backend |

**Conclusion:** OD14 stands — the external work is real but small and almost entirely additive; the only genuine schema change is git/commit lineage (deferrable via metadata JSONB). Shift-left's own client (CLI + adapter + git action) builds against `agent/create` (exists) and `/evaluate` (exists), needing the 3-line core event-type addition to have its events *accepted*.

## New decisions surfaced
- **OD16 (shift-left-owned, affects E2 adapter):** developer-agent AIP request signing — sign every `/evaluate` call with Ed25519 (reuse SDK; `signing_required=true` default) vs register dev agents with `signing_required=false` for a simpler Phase-1 MVP.
- **OD15 (external, deferrable):** lineage storage — metadata JSONB (no migration) vs indexed columns/table. Tied to E1-S5/E3; defer with the external work.

## Corrections to apply to the design
1. Architecture **D2**: drop "reuse OTLP path"; telemetry = adapter→`/evaluate` JSON. Update §1b matrix (telemetry-push not a differentiator).
2. PRD **FR-2**: reword from "ingest native OTel" to "adapter translates session telemetry into governance events at `/evaluate`."
3. OD3/OD11: `agent_type="developer"` (free-form, no migration), not a new `kind` enum.
4. Add adapter requirements: AIP Ed25519 signing (OD16) + `aivss_config` default at registration.
5. Note privacy divergence (metadata-only stricter than norm) and pin verdict vocab in the E1-S1 contract.
