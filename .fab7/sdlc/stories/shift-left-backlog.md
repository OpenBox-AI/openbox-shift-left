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

### STORY-SL2-FILEBACKEND (DONE — G_SEC APPROVE 2026-07-09; landed) — opt-in file secret backend
Delivers the explicit escape hatch for machines with **no OS keyring** (headless Linux, containers, WSL) so onboarding does not dead-end at the SL2-SEC-1 HALT while still never silently writing plaintext. `openbox dev init --secret-backend file` (or `OPENBOX_SECRET_BACKEND=file`) stores creds in a **0600** nested-JSON file (dir 0700), written atomically (temp+chmod+rename); the CLI prints a PLAINTEXT warning on every use and `auto` **never** falls back to it. The Claude Code hook reads the same file via `OPENBOX_SECRET_FILE` / `dev.json` `secret_file` (`fileSecretLookup`). **G_SEC = APPROVE, no blockers** (`.fab7/sdlc/assignments/security-review-SL2-FILEBACKEND-20260709.result.yaml`): plaintext-at-rest is the only guarantee traded, opt-in + warned, no leak beyond at-rest, hook fails open. Committed separately from SL4-WIRE-1.
- **SL2FB-INFO-1** (follow-up; most user-visible): wire `secret_file` into `DevConfig` from `dev init --secret-backend file` so the hook auto-uses the same store — today the operator must set `OPENBOX_SECRET_FILE` manually (the CLI warning instructs this). Fail-**safe** gap (no governance until set), not a leak. Threads `provider.CredentialRef` → `claudecode` installer `writeConfig`.
- **SL2FB-LOW-1** (follow-up): stat-and-warn/refuse when a pre-existing secret file is broader than 0600 on **read** (ssh-style) — `secret/file.go load()` + `creds.go fileSecretLookup`.
- **SL2FB-LOW-2** (follow-up): `flock` the file backend to close the cross-process read-modify-write lost-update window (integrity/availability only; `dev init` is a rare manual op).
- **SL2FB-INFO-2** (note): `MkdirAll(dir,0700)` does not tighten an already-0755 `~/.config/openbox`; the 0600 **file** mode gates access, so not a confidentiality break.

## Review follow-ups (from SL-3 G_SEC + G3_REVIEW, 2026-07-08)
- **MAPPING-FIX** (contract; **STILL OPEN — re-confirmed 2026-07-09**): correct `contracts/dev-event/MAPPING.md` §3 — it still says core derives `semantic_type` from `file_operation`/`function`/`hook_type` and tells adapters to set `hook_type`, but core (verified live in SL-3/SL-6) **recomputes** it at ingest (`governance_workflow.go:309` → `ComputeSemanticTypeFromSpan`) from the span **`name`** + an **`attributes`** map (keys `mcp.method`/`file.path`/…) and **ignores** those fields. SL-3's client already sets what core reads (`classificationHints`); the doc must match. Routes to the SL-1/architecture owner (revises a G3-approved doc). Low impact (client behavior already correct), but the doc is misleading for the next adapter (SL-7/SL-8).
- **SL3-IDEMPOTENCY** (external, EXT-core): key ingestion dedupe on `metadata.event_id` for the developer event types (core has no event_id field and no dev-event dedupe today → a retry after a lost 200 double-counts tokens/cost/lineage). SL-3 puts the key on the wire; core must consume it. Phase-1 observe impact = telemetry skew only.
- **SL3-SEC-3** (defense-in-depth; **gate each adapter's G_SEC**): SL-4/7/8 must verify no raw content lands in the free-form `metadata` map or `tool.name` — these bypass SL-3's content stripper (which only clears the explicit content fields). SL-3's own code is clean; it is the shared chokepoint.
- **SL3-SEC-4 / F4** (low): worst-case `Emit` wall-clock ≈ (maxRetries+1)×timeout + backoff ≈ 90s. Fine for the async best-effort adapter path (NFR-2 `"async"`); inline callers should pass a bounded `ctx`.
- **SL3-SEC-7 / F3** (low): top-level `Content.*` is not egressed even when content-capture is enabled (span `request_body`/`response_body` are). Privacy-safe; revisit with SL-4 if prompt/output capture is required.

## Review follow-ups (from SL-4 build + G_SEC + G3_REVIEW, 2026-07-08)
- **SL4-BENCH-1** (low; F6): add a hot-path (`Observe`) benchmark so the NFR-2 `<50ms p95` claim is evidenced, not just by-design. The dominant real cost (per-hook fork/exec of the Go binary) is outside the code — measure it too.
- **SL4-TOKENS** (Phase-2): Claude Code hooks expose **no** token/cost usage, so the adapter emits none. Deriving finops requires parsing `transcript_path` (content — privacy-gated per INV-2/OD4). Blocked on a content-capture posture decision.
- **SL4-SEC (folded)**: the three G_SEC LOW conditions (cap identifiers `maxIdentLen=512`; enum-validate lifecycle fields drop-unknown; reject leading-dash secret-store coordinates) were **addressed in-code + tested** during the build — no open item.

## Review follow-ups (from SL-5 build, 2026-07-09)
- **FINDING — squash healing (beyond S3):** `%(trailers)` / `interpret-trailers --parse` parse ONLY the trailing trailer block; a squash concatenates each source message, leaving earlier `OpenBox-Session:` lines **mid-body** where the resolver can't see them (S3 R7's resolve command is subtly incomplete for squash). SL-5's stamper now **harvests every `OpenBox-Session:` line from the whole message and re-asserts it into the trailing block** (addIfDifferent), so multi-session fan-in is resolvable regardless of who ran the squash. Empirically confirmed with git 2.53.0; covered by `TestStamp_HealsSquashConcatenation` + `TestE2E_SquashFansInAllSessions`.
- **PARALLEL-SAFE SESSION DISCOVERY (redesign, brian 2026-07-09):** original env-var-only discovery could not handle a developer running **multiple concurrent sessions**. Docs confirm Claude Code exposes **no** session-id env var or pid to the git subprocess. New design: the adapter writes a **worktree-scoped per-session liveness registry** (`{session_id, cwd, updated_at}`, structural-only/INV-2); the `prepare-commit-msg` hook attributes the commit to the **most-recently-active session whose cwd is within the commit's worktree**. Different worktrees never collide (exact); same-worktree resolves by recency (the committing session refreshed on its `PreToolUse` ms earlier); stale/crashed records TTL-expire; `OPENBOX_SESSION` env remains an explicit override for CI/other providers. Proven end-to-end incl. a two-repo parallel case.
- **SL5-WIRE-1 (DONE 2026-07-09):** (i) the Claude Code hook writes/refreshes/removes the session registry; (ii) **opt-in ambient install** of the `prepare-commit-msg` hook — gated by `DevConfig.install_git_hook` (adapter `ResolveInstallGitHook`, env `OPENBOX_INSTALL_GIT_HOOK` overrides), OFF by default because it modifies `.git/hooks`; idempotent, foreign-safe; (iii) **`openbox dev init --install-git-hook`** flag (brian's choice, 2026-07-09) — threads through `provider.CredentialRef` → `claudecode.CredentialRef` → `DevConfig.install_git_hook`, disclosed in `--dry-run` and the manual-config output. **Remaining:** (a) OD17 binary unification — fold `openbox-cc-hook` + `openbox-git-hook` into one `openbox hook …` engine (= SL4-WIRE-2); (b) the CLI→real-installer link that actually PERSISTS the flag to `DevConfig` depends on **SL4-WIRE-1** (today the claude-code Installer in the CLI is still the SL-2 stub, so the flag is disclosed but persisted only once WIRE-1 registers the real `claudecode.Installer`); (c) confirm the pilot rollout posture (OD-SL5-D — decided: `dev init` flag).
- **SL6-SCAN** (feeds SL-6) — **DONE in SL-6** (a72e7db): the resolver prefers the healed `%(trailers)` block and full-body-scans `OpenBox-Session:` lines (`Source: body-scan`) to recover fan-in from commits squashed **before** the hook was installed (`actions/openbox-git-action/{git,resolve}.go`; `TestResolve_PreInstallSquashRecoveredByBodyScan`).
- **SL5-CHANGEID** (S3 U-4, optional): adopt a stable `OpenBox-Change-Id` trailer to re-link across body-replacing GitHub squash-merge — the one loss mode neither trailer-copy nor healing covers.
- **G_SEC (flag for brian):** SL-5's planned gates are G2/G3 only, but it touches INV-1 (secret-in-history). `validateSessionID` already rejects `obx_`-shaped and multi-line values (trailer-injection safe) and it is unit-tested; recommend a Sam G_SEC pass on that surface before Phase-2.

## Review outcomes (SL-5 independent review, 2026-07-09)
- **G_SEC = approve-with-conditions; G3 = REVISE → all fixed + re-validated** (git module 62 subtests, all 3 modules green under -race). No security blocker; no path can abort a commit.
- **F1 (must-fix, FIXED):** an empty/comment-only editor message was stamped → a junk trailer-only commit instead of git's normal empty-abort. Fixed: `hasCommitContent` guard skips content-less messages (`TestE2E_EmptyMessageStillAborts`).
- **Fixed lows:** F5 prose-harvest (validateSessionID rejects whitespace) · F3 symlinked cwd (`resolvePath` EvalSymlinks) · F4 recency window (UnixNano) · F6 shared temp name (`os.CreateTemp`) · SL5-SEC-3 validate-at-registry-write · SL5-SEC-2 ambient install bounded to `<git-common-dir>/hooks` (`HooksDirDefault`, ignores repo-controlled `core.hooksPath`).
- **SL5-SEC-1 → SL-6 (mandatory carry):** the trailer is an UNTRUSTED claim; SL-6 must bind each value to a session owned by the **authenticated pusher** and mark others unattributed/inferred (mirror SL-3 DID cross-binding). Forgeability now documented in `adapters/common/git/doc.go`. Recorded on SL-6's status_reason.
- **Accepted/noted (no code):** F2 (orphan mid-body line after heal — cosmetic; resolution correct), F7 (doc nit), F8 (dormant notes mirror — optional by S3 R5).

## Review outcomes (SL-6 independent review, 2026-07-09)
- **G_SEC = approve-with-conditions; G3 = REVISE → all fixed + re-validated** (30 tests under -race; go vet clean). No security blocker.
- **SL5-SEC-1 DISCHARGED (as designed):** every trailer is a CLAIM verified against the authenticated pusher via `OwnershipVerifier` before it can be `attributed`; Phase-1 `NoopVerifier` ⇒ deploys resolve `inferred`/verified=false (ownership read API deferred). Forged/unverified ids excluded from `verified_session_ids`.
- **Must-fixes FIXED:** SEC-6-1 (`MaxSessions`+1MiB bounded reads+`-n` rev-list, disclosed) · SEC-6-2 (`session_ids`→verified-only `verified_session_ids`) · C3 (per-commit note recovery, no silent sibling drop) · C1 (`non-agent` documented reserved) · C2 (two-pass gather; trailer beats body-scan across scope) · P1 (stable run_id) · P2 (full-SHA dedup key).
- **SL6-OWNERSHIP (feeds Phase-1.5; needs EXT-lineage/FR-7):** wire a real `OwnershipVerifier` (backend session-ownership lookup keyed on the pusher's developer identity) so owned sessions become `attributed`. Until then every deploy is honestly `inferred`.
- **SL6-SEC-6 (LOW, JOINT with SL-5):** tighten `validateSessionID` on **both** sides to reject all non-graphic runes (defense-in-depth for downstream terminal/log sinks).
- **SL6-CI (packaging):** provide the GitHub Action wrapper (`action.yml`) once the pilot repo is named (OD10).
- **Accepted (no code):** P3 (`inferred`-from-unverified-trailer carries no structured enum reason — detail is in the free-text note; none of the 3 contract reasons fit).

## Wiring stories (SL-4 → CLI integration — DECIDED **wired**, brian 2026-07-08)
Rationale: deliver the OD12 "one command onboards" front door for real (`openbox dev init --provider claude-code` currently still prints the SL-2 stub). The wiring's real content is the **generic provider-SPI seam** (architecture §1b) + the **single-engine packaging** (OD17/OD18/OD19) — designed once here so SL-7 (Codex) and SL-8 (Cursor) slot in with zero further core/CLI change. These supersede the earlier single "SL4-WIRE-1" follow-up.

### SL4-WIRE-1 — Extract the provider SPI to a shared module + register the Claude Code installer in the CLI
- **Goal:** make `openbox dev init --provider claude-code` actually install the plugin (bundle + non-secret dev config), replacing the SL-2 `stub`. Lift the `Installer` SPI (`register`/`emit`/`apply`/`capabilities` seam — here the install half) out of `cli/internal/provider` into a **shared importable module** both `cli` and every adapter depend on, so an adapter can implement it without crossing an `internal/` boundary.
- **Source:** architecture §1b (the SPI is the generic seam), OD12/OD18/OD19, D5/D6; SL-2 provider.go ("adapter stories replace these stubs with real installers"); SL-4 `claudecode.Installer` (built, tested).
- **Acceptance criteria:**
  - New shared module (e.g. `provider/`, own `go.mod`) exposing the `Installer` interface + `CredentialRef` (non-secret coordinates only — INV-1). **An ADR records the SPI package home** (structural decision, per CLAUDE.md).
  - `cli` requires the adapter module (`replace → ../adapters/claude-code`) and registers `claudecode.Installer` for `claude-code`; the SL-2 `stub` remains for still-unbuilt providers (codex/cursor).
  - `openbox dev init --provider claude-code` performs registration (existing) → **installs the plugin** (materialize bundle + write dev config with secret-store coordinates, never secret values) → reports success; `--dry-run` prints the plan (reuse SL-2); re-init idempotent.
  - No secret value ever passes through the installer (INV-1): it receives only the `CredentialRef` coordinates SL-2 stored.
- **Write scope:** `provider/` (new), `cli/` (registry + dev-init wiring), `adapters/claude-code/` (implement the shared interface; drop the self-defined types). **Deps:** SL-2, SL-4 (both done). **Gates:** G2, G3, **G_ADR** (SPI package home). **Invariants:** INV-1, INV-7.
- **Validation:** `cd cli && go build ./... && go test ./...`; `cd provider && go test ./...`; an integration test: `dev init --provider claude-code --dry-run` prints the plan and `dev init` (against a temp HOME + mock registry) materializes the bundle + config with no secret in any written file.
- **Note:** revises SL-2's (G3-approved) `provider` package by relocating the interface — flag at G3.

### SL4-WIRE-2 — Unify the hook entrypoint into the `openbox` engine + end-to-end onboarding smoke test
- **Goal:** fold the standalone `openbox-cc-hook` into the single `openbox` engine binary as `openbox hook claude-code <event>` (OD17/OD18/OD19: the plugin bundles **`bin/openbox`** + hooks, not a per-adapter binary). The installer/plugin reference the unified binary. Prove the whole onboard→observe→deliver flow once.
- **Source:** OD17 (single Go binary), OD18/OD19 (plugin bundles `bin/openbox`), architecture §1b; SL-4 `cmd/openbox-cc-hook` (to be absorbed).
- **Acceptance criteria:**
  - `openbox hook <provider> <event>` subcommand runs the adapter's observe/flush path; the standalone `cmd/openbox-cc-hook` is retired (or reduced to a thin alias).
  - **The observe-only contract survives the move into the multi-purpose CLI binary (re-verify at G_SEC):** the `hook` subcommand ALWAYS exits 0 with **empty stdout** — no cobra/flag/usage/banner text may reach stdout (it would be injected into Claude Code context on SessionStart/UserPromptSubmit). All diagnostics to stderr only.
  - `plugin/hooks/hooks.json` + the installer reference `${CLAUDE_PLUGIN_ROOT}/bin/openbox` (the unified engine); `dev init` places that binary in the bundle's `bin/`.
  - **End-to-end smoke test:** register (mock) → `dev init` install → simulate the 5 hooks via the subcommand → assert normalized events spooled → flush to a mock `/evaluate` → assert received; latency budget + content-absent asserted.
- **Write scope:** `cli/` (the `hook` subcommand), `adapters/claude-code/` (plugin/hooks.json + installer reference; absorb `cmd/`). **Deps:** SL4-WIRE-1, SL-2. **Gates:** G2, G3, **G_SEC** (re-verify observe-only exit-0/empty-stdout on the unified binary). **Invariants:** INV-3.
- **Validation:** `cd cli && go build ./... && go test ./...`; a subprocess test that runs `openbox hook claude-code PreToolUse` and asserts exit 0 + empty stdout + a spooled event (the SL-4 real-binary test, moved to the unified binary).

## Bucket #2 — Advisory-tier SDK parity (SL-9/10/11, drafted 2026-07-13)

Formalizes the **SDK↔shift-left gap analysis** (2026-07-13): shift-left already replicates the Python SDK's client transport + AIP signing **byte-for-byte** (and exceeds it on onboarding, secret storage, async hot-path, and lineage). The remaining SDK functionality is the **enforcement half** — which the design defers to Phase-2 (D7/INV-3). Bucket #2 replicates the SDK's verdict/guardrail **consumption** semantics *record-only* (the architecture's middle **Advisory** tier, §1b), leaving the codebase one flag from Enforce. **Enforcement itself (the `apply` leg) is the deferred NEXT increment** — gated on spike **S2** (dev-machine evaluate latency / local sidecar) and **OD6/OD9**; it is NOT in this bucket.

> **Correction baked in:** the 5-tier verdict enum + wire parsing **already exists** (`client/verdict.go`). Bucket #2 is about *consuming* the result + the rich sibling fields the client currently drops — not re-adding tiers.

### SL-10 — Signing / response error diagnostics *(do first; foundational)*
- **Goal:** port the SDK's `map_signing_error` (`errors.py`) — turn silent fail-open drops on a rejected `/evaluate` into one actionable diagnostic (`signature_invalid`/`nonce_replayed`/`did_agent_mismatch`/`verifier_not_configured`/`timestamp_skew`).
- **Write scope:** `client/`. **Deps:** SL-3. **Gates:** G3. **Invariants:** INV-1/2/3 (stays fail-open; no secret; categories only).

### SL-9 — Advisory-tier verdict & guardrail consumption *(centerpiece)*
- **Goal:** `Emit` returns a rich `Evaluation` (trust_tier/risk_score/constraints/guardrails_result…); the adapter **records** what would be enforced (`would_block` label) to a local advisory sink and **never blocks** (INV-3 is the load-bearing AC). This is the **Advisory** governance tier.
- **Write scope:** `client/`, `adapters/claude-code/`, `actions/openbox-git-action/`. **Deps:** SL-3, SL-4 (soft: SL-10 first, shared `client.go`). **Gates:** **G1_READY** (brian: bless the Advisory tier + sink) + G3. **Invariants:** INV-1/2/3.

### SL-11 — `openbox dev verify` (auth/validate + signing round-trip)
- **Goal:** a read-only preflight — signed `GET /api/v1/auth/validate` (endpoint exists: `openbox-core internal/api/main.go:118`) confirms the data-plane key + AIP signing work against the configured core; ✓ or an SL-10-mapped ✗ with a fix hint.
- **Write scope:** `client/` (`Validate`), `cli/` (`dev verify`). **Deps:** SL-3, SL-10. **Gates:** G3. **Invariants:** INV-1 (no secret; TLS guard), INV-7.

### Sequencing
```
SL-10 (reason map) ─► SL-9 (Evaluation + advisory record) ─► SL-11 (dev verify, reuses the map)
```

### Deferred NEXT increment — Enforcement (the `apply` leg)
Synchronous `PreToolUse` deny/ask/rewrite + fail-closed + guardrail **redaction application** + a decision on whether HITL (`/approval`) fits a dev hot-path. **Crosses INV-3.** Prerequisite: **spike S2**; decisions **OD6** (hook handler type) / **OD9** (fail-closed). Would land as a Phase-2 epic (CLAUDE.md: "E6 Phase-2 enforcement blocked on spike S2").

---

## Phase-1 debt / hardening (SL-13/14/15/16, drafted 2026-07-13)

Shift-left-owned closure of the four Phase-1 debt items surfaced in the Phase-1 review; the external openbox-core / openbox-backend pieces stay **assumed-satisfied** (OD14). Honest split: **SL-13 + SL-16 deliver real value now**; **SL-14 is thin** (most of the fix is EXT-core); **SL-15 lands ready-but-flag-off** (external-gated).

### SL-13 — EXT-core dependency: patch artifact + core-acceptance contract test *(debt #1; highest leverage)*
- **Goal:** capture the EXT-core 3-edit change (constants + `isValidGovernanceEventType` + lifecycle switch) as a versioned PR-ready artifact under `contracts/dev-event/ext-core/` (type list checked against the SL-1 enum) + a live acceptance test (7 types non-400, clean-skip offline). Makes the one hard Phase-2 blocker reproducible + verifiable.
- **Write scope:** `contracts/dev-event/`. **Deps:** SL-1, SL-3. **Gates:** G3. **Invariants:** INV-8. **External:** the upstream merge.

### SL-16 — Opt-in transcript usage extraction (tokens/cost, metadata-only) *(debt #4; fully closable)*
- **Goal:** on flush, behind an off-by-default opt-in, read `transcript_path` for **usage NUMBERS ONLY** → populate the (existing, unused) `event.Tokens`/`event.Cost`. INV-2 is the load-bearing AC (sentinel-content-absent test). The one debt item shift-left can fully close alone.
- **Write scope:** `adapters/claude-code/`. **Deps:** SL-4, SL-1. **Gates:** **G1_READY (OD-FINOPS)** + G_SEC + G3. **Invariants:** INV-1/2/3.

### SL-14 — Idempotency hardening (deterministic event_id + delivery matrix) *(debt #2; thin)*
- **Goal:** deterministic collision-safe `event_id` + explicit `Idempotency-Key` header + the crash/retry/recovery delivery-guarantee test matrix. Client is already at-most-once; server dedupe is the completing EXT-core piece.
- **Write scope:** `adapters/claude-code/`, `client/`. **Deps:** SL-3, SL-4. **Gates:** G3. **Invariants:** INV-5/3/1.

### SL-15 — Real OwnershipVerifier against the FR-7 read API *(debt #3; external-gated)*
- **Goal:** implement `apiVerifier` (AIP-signed read of the pusher's owned sessions), fail-closed, flagged with `NoopVerifier` default; owned sessions promote `inferred → attributed` with no resolver change. Discharges SL5-SEC-1 for real where FR-7 exists.
- **Write scope:** `actions/openbox-git-action/`. **Deps:** SL-6, SL-3. **Gates:** **G1_READY (OD-OWNER-API)** + G_SEC + G3. **Invariants:** INV-6, SL5-SEC-1, INV-1/4. **External (assumed):** [EXT-lineage/FR-7]; ships flag-off until confirmed.

### Sequencing
```
SL-13 (unblocks ingestion + Phase-2)  ─►  SL-16 (pending OD-FINOPS)  ─►  SL-14 (prep+tests)  ─►  SL-15 (ready, flag-off; pending FR-7)
```

---

## Phase-2 / Epic E6 — Enforcement (drafted 2026-07-13)
The `apply` leg — honor the verdict (deny/ask/rewrite). Artifacts: spike `.fab7/sdlc/discovery/spikes/S2-enforcement-latency.md`, epic `.fab7/sdlc/stories/E6-backlog.md`, `.fab7/sdlc/design/adr/ADR-0002-enforcement-blocking-invariant.md`. **Inverts INV-3** → the scoped, bounded, fail-open-by-default carve-out **INV-3b** (ADR-0002). Mechanism: `PreToolUse` (pre-execution, synchronous) `permissionDecision` deny/ask + `updatedInput` rewrite — **no revert; the decision is made before the side effect.** **BLOCKED** on spike S2 + OD-ENF-SCOPE + OD-HITL + the real EXT-core upstream merge. See E6-backlog for stories E6-S0..S7.

## Flags
- **OD9 (DECIDED 2026-07-13, brian): fail-open at first.** Enforcement degrades to allow on core/sidecar unavailability or timeout; per-org fail-closed is a later opt-in (E6-S3).
- **OD6 (CONFIRMED by spike S2, 2026-07-13): command → local sidecar.** S2 measured `POST /evaluate` at **~0.8–1.6 s** (Temporal workflow, loopback floor) — ~16–33× budget → **direct HTTP is NO-GO**. The decision must be a local sidecar (single-digit ms; fork/exec 1.5 ms, signed transport 3.6 ms). `/evaluate` stays async telemetry only. Sidecar (E6-S5) promoted to a REQUIRED prerequisite.
- **OD-ENF-SCOPE (DECIDED 2026-07-13, brian): replicate the full SDK verdict scope** — HALT>BLOCK>guardrails>REQUIRE_APPROVAL>CONSTRAIN>ALLOW (port `verdict_handler.enforce_verdict`) across all tools. No reduced first-scope.
- **OD-HITL (DECIDED 2026-07-13, brian): map REQUIRE_APPROVAL → CC `ask`** (interactive local prompt, not server-side `/approval` polling). E6-S6 in scope.
- **spike S2 (DONE 2026-07-13):** direct-HTTP infeasible, local sidecar mandatory, hook timeout ≈ 50 ms, fail-open on timeout — see `S2-enforcement-latency.md`.
- **ADR-0002 (proposed):** INV-3b enforcement carve-out — ratify at E6-S7 once spike S2 supplies the hard timeout.
- **OD-FINOPS (RESOLVED 2026-07-13):** brian confirmed reading `transcript_path` for numbers-only, behind an off-by-default opt-in (separate-flag sub-ruling) — SL-16 done.
- **OD-OWNER-API (RESOLVED 2026-07-13):** target the real openbox-backend `/agent/<id>/sessions` read with an org X-API-Key + `OPENBOX_AGENT_ID` (uuidv5-bound to the DID) — SL-15 done.
- **OD-ADV (RESOLVED 2026-07-13):** Advisory tier ADOPTED by brian (SL-9 G1_READY confirm) — kept for provenance.
- **OD10:** name the pilot repo before SL-6's squash-prevalence validation (S3 U-1) and pilot rollout.
- **OD15 (external, deferred):** lineage storage metadata-JSONB vs indexed — SL-6 uses metadata (no external dep); FR-7 *queryable* read is deferred.
- **Validation commands:** record exact per-package test/build commands in project memory `validation-commands.yaml` at draft-story.
- **Security review (Sam):** SL-2 (credentials) and SL-3 (AIP signing/keys).
- **Contract home (SL-1):** `contracts/` in this repo, published for core/backend/adapters to consume — confirm at draft-story.
