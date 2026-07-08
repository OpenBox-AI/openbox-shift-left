# Shift-Left build backlog (shift-left-repo-owned stories)

**Author:** Sol (Planning Lead) — 2026-07-07
**Scope:** the stories OWNED and BUILT in the openbox-shift-left repo. External-component work (openbox-core / openbox-backend) is **deferred, lowest-priority, and assumed-satisfied** per OD14 — listed under "External dependencies" per story, not built here.
**Grounded in:** spike S6 (verified external reality), S1/S5 (provider surfaces), S3 (attribution), architecture §1b. **Scope basis:** path-prefix (all within this repo).

## Corrected facts baked in (from S6)
- Telemetry ingestion = **adapter translates events → POST `/api/v1/governance/evaluate`** on openbox-core (base `https://core.openbox.ai`). There is **no OTLP intake** to reuse.
- Registration = existing `POST agent/create` with **`agent_type="developer"`** (free-form, no migration) + required `aivss_config`.
- The adapter **must AIP Ed25519-sign** every `/evaluate` call (OD16, reuse `openbox-temporal-sdk-python` signing; `signing_required=true`).
- **Metadata-only** by default (OD4) — stricter than platform norm; strip content at source.
- Verdict vocab canonical: `HALT > BLOCK > REQUIRE_APPROVAL > ALLOW`. Event schema respects both **lifecycle** types (SessionStarted…Deploy) and **semantic span** types (`file_write`/`mcp_tool_call`/`llm_call`, which already exist in core).

## Delivery model (OD18/OD19, 2026-07-08 — brainstorm 002)
Hybrid: **plugin vehicle + CLI engine.** The Claude Code adapter (SL-4) is packaged as a **native CC plugin that bundles `bin/openbox` (the Go engine) + the hook wiring**, distributed via a marketplace and force-enableable via managed `enabledPlugins`. SL-2's `openbox dev init --provider claude-code` **installs that plugin** (rather than hand-writing config); Codex/Cursor get config+managed-hooks bundles laid down by the CLI. Phase-1 plugin bundle = `bin/openbox` + hooks only (MCP server / skills deferred). Impact: **SL-2** = "install the plugin"; **SL-4** = "build the CC plugin bundling bin/openbox + hooks." No new stories; refines SL-2/SL-4 scope. The Go binary + event contract remain the single shared substrate (OD17/SL-1).

## External dependencies (assumed satisfied — NOT built here; lowest priority)
- **[EXT-core]** 3 additive no-migration edits in openbox-core so `/evaluate` accepts developer event types (constants, `isValidGovernanceEventType`, session-lifecycle switch) — S6 §3. Required for emitted events to be *accepted*.
- **[EXT-lineage]** git commit/deploy lineage storage + FR-7 read API (metadata-JSONB stopgap needs nothing; indexed/queryable = migration) — S6 §4, OD15.
- **[EXT-signoff]** dev agents registered/provisioned in the target OpenBox org.

---

## Stories

### SL-1 — Normalized developer event contract + conformance test  *(= E1-S1)*
- **Goal:** the versioned, tool-agnostic event schema every adapter + the client emit — the single interface. Maps 1:1 onto openbox-core's `GovernanceEventPayload` (`spans[]` + `metadata`).
- **Source:** PRD FR-4; architecture D3/D4; S6 §2/§3/§7 (both event-type axes; verdict vocab).
- **Acceptance criteria:**
  - Versioned schema (`schema_version`) with lifecycle event types `SessionStarted|PromptSubmitted|ToolCall|ToolResult|SessionEnded|CommitCreated|Deploy`, each mapping to a documented `GovernanceEventPayload` shape (which `event_type`, which span semantic type, what goes in `metadata`).
  - Fields: `openbox_session_id`→`(workflow_id, run_id)`, `developer_did`, `tool{name,kind:shell|file|mcp,mcp_server?}`, timestamps, `tokens?`, `cost?`, `content?` (absent by default).
  - Canonical verdict enum pinned (`HALT>BLOCK>REQUIRE_APPROVAL>ALLOW`).
  - Conformance test validates one sample of each type, rejects malformed and content-when-disabled.
- **Write scope:** `contracts/dev-event/`. **Deps:** none. **Gates:** G2, G3. **Invariants:** INV-2, INV-8.
- **Validation:** schema lint + conformance suite green.
- **External dep:** none to author; alignment target is core's `GovernanceEventPayload` (read-only reference).

### SL-2 — `openbox` CLI + `dev init --provider <tool>`  *(front door, OD12)*
- **Goal:** one command onboards a developer: detect the tool, register the dev agent, obtain credentials, write the tool's native config. Governance is ambient thereafter.
- **Source:** PRD FR-1/FR-8/NFR-5; architecture D5 (S6-corrected), OD12; S6 §1/§5.
- **Acceptance criteria:**
  - `openbox dev init --provider claude-code|codex|cursor` registers via existing `POST agent/create` with `agent_type="developer"` + a sane default `aivss_config`; captures `obx_` key, `did:aip:` DID, and Ed25519 private key.
  - Writes the selected tool's native config (delegates to that adapter's installer) and stores credentials in the OS secret store (never plaintext in repo/config; INV-1).
  - Idempotent re-init; `--dry-run` prints planned changes.
  - Supports org-wide force-enable substrate but Phase-1 pilot is **opt-in** — mandate verified, not activated (NFR-5/OD10).
- **Write scope:** `cli/`. **Deps:** none (registration uses existing API; the config it writes is provided by SL-4). **Gates:** G2, G3, **security review (Sam)** (credential handling). **Invariants:** INV-1, INV-4, INV-7.
- **Validation:** CLI unit/integration tests; a `dev init` against a test OpenBox registers an agent and writes valid config.
- **External dep:** [EXT-signoff] a reachable OpenBox org.

### SL-3 — OpenBox client library (AIP-signed `/evaluate` transport)
- **Goal:** the shared, reusable client all adapters + the git action use to emit a normalized event: build payload → AIP-sign → POST `/evaluate` → parse verdict.
- **Source:** architecture D6/D7; S6 §2/§5; OD16.
- **Acceptance criteria:**
  - Given a normalized SL-1 event, builds `GovernanceEventPayload` and POSTs to `/api/v1/governance/evaluate` with `Authorization: Bearer <obx_>`.
  - **AIP Ed25519 signing** (reuse SDK): canonical `METHOD\npath\ntimestamp\nnonce\nbodySHA256` + `X-OpenBox-Agent-{DID,Timestamp,Nonce,Signature}` + `X-OpenBox-Body-SHA256`.
  - Timeout (default 30s) + retry (max 2, base 150ms); **fail-open for observe** — a failure logs and drops/buffers, never blocks the caller (INV-3, NFR-3).
  - **Metadata-only enforcement:** strips `content` fields when content-capture disabled (INV-2/NFR-1).
  - Parses the verdict (`HALT/BLOCK/REQUIRE_APPROVAL/ALLOW`) but Phase-1 callers ignore it (observe).
- **Write scope:** `client/`. **Deps:** SL-1. **Gates:** G2, G3, **security review (Sam)** (signing/keys). **Invariants:** INV-1, INV-2, INV-3, INV-5 (client event id for idempotency).
- **Validation:** unit tests incl. a signed request verified against a known Ed25519 keypair; fail-open test (unreachable endpoint → no throw).
- **External dep:** [EXT-core] events accepted only after the core 3-edit change; until then, integration tests run against a stub/mock of `/evaluate`.

### SL-4 — Claude Code adapter (observe-only)
- **Goal:** the first realization of the adapter contract — Claude Code hooks map to normalized events emitted via SL-3.
- **Source:** PRD FR-2/FR-3; S1 A1/A4; architecture D6; OD5/OD8 (Claude Code first).
- **Acceptance criteria:**
  - A Claude Code plugin bundles hooks `SessionStart`/`UserPromptSubmit`/`PreToolUse`/`PostToolUse`/`SessionEnd`; each maps native payload → SL-1 event → SL-3 emit. **Observe-only** (verdict ignored; never denies — INV-3).
  - Session/tool/prompt telemetry (tokens/cost/tool names/MCP) arrives at OpenBox as normalized events; metadata-only (NFR-1).
  - Async/best-effort (`"async": true`); adds <50 ms p95 per-tool-call overhead; never blocks a tool call if OpenBox is unreachable (NFR-2/NFR-3).
  - Supports managed-settings force-enable (verified, not activated for pilot — NFR-5).
- **Write scope:** `adapters/claude-code/`. **Deps:** SL-1, SL-3. **Gates:** G2, G3. **Invariants:** INV-2, INV-3.
- **Validation:** run a real Claude Code session with the plugin; assert normalized events emitted (against SL-3 mock or a test OpenBox), latency budget met, content absent by default.
- **External dep:** [EXT-core] for events to be accepted end-to-end.

### SL-5 — Commit trailer stamping (session→commit binding)
- **Goal:** bind commits to their session(s) via a git trailer (provider-independent).
- **Source:** PRD FR-5; spike S3 (R1–R6).
- **Acceptance criteria:**
  - A `prepare-commit-msg` git hook (installed by the CLI/adapter) stamps `OpenBox-Session: <openbox_session_id>` idempotently (`--if-exists=addIfDifferent`); multiple sessions → multiple lines (fan-in, like `Co-Authored-By`).
  - Never writes secrets (opaque session id only — INV-1); safe under `--amend` (no duplicate lines).
  - Optional non-authoritative local `refs/notes/openbox` mirror.
- **Write scope:** `adapters/common/git/`. **Deps:** SL-2 (CLI installs the hook); session id source from SL-4. **Gates:** G2, G3. **Invariants:** INV-1, INV-6 (write side).
- **Validation:** commit/amend/rebase-squash test matrix (S3) confirms trailer presence/idempotency; fixup drop is detected downstream (SL-6).

### SL-6 — OpenBox git action (deploy lineage)
- **Goal:** at push/deploy, resolve session↔commit and register a Deploy event provably linked to the originating session(s).
- **Source:** PRD FR-6; spike S3 (R7–R14); S6 §4 (metadata-JSONB stopgap for lineage).
- **Acceptance criteria:**
  - Reads `OpenBox-Session` trailers **server-side against the real pushed SHA** (never a pre-push SHA), dedups to a session set (S3 R7/R8).
  - Emits a `Deploy` event via SL-3 carrying `DID = git hash + timestamp` and the session set **in `metadata`** (no external schema needed — S6 §4 stopgap).
  - No trailer → explicit `unattributed`/`inferred` marker with reason (`no-trailer`|`trailer-stripped`|`non-agent`) — never silent wrong attribution (INV-6). Merge commits attribute reachable originals.
- **Write scope:** `actions/openbox-git-action/`. **Deps:** SL-1 (Deploy type), SL-3 (client), SL-5 (trailers). **Gates:** G2, G3. **Invariants:** INV-6.
- **Validation:** CI-simulated push (incl. squash/fixup/force-push) resolves the correct session set or a reasoned marker.
- **External dep:** [EXT-lineage] queryable lineage (FR-7 read API) is external/deferred; SL-6 only *writes* the Deploy event via metadata (no external dep to emit).

---

## Sequencing (no cycles)
```
SL-1 (contract) ─┬─► SL-3 (client) ─┬─► SL-4 (Claude Code adapter)
                 │                   └─► SL-6 (git action) ◄── SL-5 (commit trailer) ◄── SL-2
SL-2 (CLI+init) ─┘  (SL-2 independent; installs SL-4 config + SL-5 hook)
```
- **First batch (parallel, disjoint scopes, no deps): SL-1 + SL-2.**
- Then SL-3 → SL-4; SL-5 alongside (git-level); SL-6 last (needs SL-1/SL-3/SL-5).

## Fast-follow (shift-left, after Claude Code)
- **SL-7 Codex adapter** (`adapters/codex/`) — reuses SL-3 client; hooks + rollout/OTel translate → events; requirements.toml/MDM mandate (S5). **Priority: next (OD13).**
- **SL-8 Cursor adapter** (`adapters/cursor/`) — reuses SL-3; hooks + Admin-API poller; note fail-open caveat (S1). After Codex.

## Review follow-ups (from SL-2 G_SEC + G3_REVIEW, 2026-07-08)
- **SL2-SEC-1** (deferred; **required before macOS GA / Phase-2 enforcement**): native macOS Keychain / stdin credential-write helper to close the `security -w` argv-exposure window. G_SEC accepted the transient exposure only for the Phase-1 Linux-target opt-in pilot.
- **SL2-SEC-2** (deferred; low): input validation / defense-in-depth on `--org` and agent-name before they reach the shelled-out `secret-tool`/`security` commands (no leak today — secret is on stdin).
- **SL2-ROTATE** (idea): `openbox dev rotate` to re-mint creds (`POST :agentId/rotate-api-key`) for an agent whose once-shown key/seed were lost — the honest counterpart to the no-upsert idempotency reality.

## Flags
- **OD10:** name the pilot repo before SL-6's squash-prevalence validation (S3 U-1) and pilot rollout.
- **OD15 (external, deferred):** lineage storage metadata-JSONB vs indexed — SL-6 uses metadata (no external dep); FR-7 *queryable* read is deferred.
- **Validation commands:** record exact per-package test/build commands in project memory `validation-commands.yaml` at draft-story.
- **Security review (Sam):** SL-2 (credentials) and SL-3 (AIP signing/keys).
- **Contract home (SL-1):** `contracts/` in this repo, published for core/backend/adapters to consume — confirm at draft-story.
