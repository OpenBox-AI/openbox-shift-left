# Verification of the Enterprise Agentic-CLI Governance Report + Realistic Shift-Left Plan

Date: 2026-07-28
Verifies: `docs/enterprise-agentic-cli-governance-report.md` (the external assessment, authored in a Codex environment per its validation note). **Note 2026-07-29: the report and `docs/e8-implementation-plan.md` have now vanished from `docs/` twice (both untracked) and were restored from session transcripts both times — commit all three governance docs to stop the recurrence.**
Re-verified 2026-07-29: all §1 verdicts re-checked independently (fresh source agents on core `8ea33bc` / backend `9beb0c5`, SL-11 re-reproduced, provider/landscape claims re-checked against live docs) — all hold; corrections and new nuances recorded in `docs/e8-implementation-plan.md` §6, and a third pass (same date, shift-left HEAD `cfc93f7`: all hold again, SL-11 re-reproduced 14+1→0) in §7.
Method: three independent verification tracks — (1) source-level check of every SL-01…SL-11 finding against this repo, including test reruns; (2) Claude Code capability claims vs current official docs; (3) Codex capability claims vs current official docs + a landscape scan of enterprise agentic-coding governance.

## 1. Verdict on the report

**The report is substantially accurate and worth acting on.** Every external capability claim checked out against current provider documentation, and 10 of 11 source-level findings are confirmed against the code (one is partially true). Its weakness is not accuracy but calibration: it under-credits mitigations shift-left already ships, frames one deliberate decision (OD-SL7-ASK) as a defect, recommends reversing a decision brian made explicitly (content-capture default), and its 30/90/180-day roadmap assumes an enterprise platform program (IdP, MDM fleet, DLP stack) rather than what this repo can sequence itself.

### 1.1 Source-level findings (SL-01…SL-11)

| ID | Report claim | Verdict | Notes |
| --- | --- | --- | --- |
| SL-01 | Enforcement config is user-local/mutable; user-level hooks | **Confirmed** | `adapters/common/devconfig/devconfig.go:73-118` (user config + env override precedence at :446-457); `adapters/codex/installer.go:106-107` explicitly defers managed config to a separate story. |
| SL-02 | Local bundle unsigned, no rollback protection; raw rego fail-open | **Confirmed** | `decision/bundle.go:144-164` (structural validation only, no signature/expiry fields); `cli/cmd/openbox/main.go:220-228` (`RawRegoUnlocalized` → local allow). Sync channel is authenticated but content is not integrity-verified. |
| SL-03 | Long-lived agent key; staleness silently skipped without token | **Confirmed (nuance)** | `obx_` key has no expiry/rotation anywhere; `adapters/codex/staleness.go:58-61` skips on missing token — logged to hook stderr, but session proceeds ungated with no user-visible warning. |
| SL-04 | README implies egress-proxy capability; none exists in code | **Confirmed** | `README.md:36-40` "Egress proxy ✅ base-URL" column; repo-wide grep finds zero proxy/allowlist implementation. Only HTTPS enforcement on OpenBox's own endpoints. |
| SL-05 | Trailer is a claim; verification defaults off; ownership ≠ production | **Confirmed** | `actions/openbox-git-action/ownership.go:23-32` NoopVerifier default; the real verifier checks session ownership, not that the session produced the commit. `docs/lineage-architecture.md:83` already says "unverified claim". |
| SL-06 | Codex session≡thread; CC adapter drops `tool_use_id`/subagent fields | **Confirmed** | `adapters/codex/hookevent.go:47-48`; CC `HookEvent` (`adapters/claude-code/hookevent.go:52-86`) parses no `tool_use_id`/`agent_id`/`agent_type` — Codex adapter actually carries `tool_use_id` (`hookevent.go:83-85`) and notes CC lacks it. `COVERAGE.md` §3.2 defers subagent fields as a Phase-1 non-goal. |
| SL-07 | Schema lifecycle-only; docs disagree on coverage | **Partially true** | Schema has `tool`/`tokens`/`cost`/`span`/`content` $defs + `schema_version`, so "only broad lifecycle fields" undersells it. But COVERAGE.md is genuinely stale vs the shipped Codex adapter (says "no SessionEnd hook", "tokens absent" — both superseded by `adapters/codex/capabilities.go:18,21`), and there is no versioned capability registry. |
| SL-08 | At-most-once delivery, best-effort transport, incomplete idempotency | **Confirmed (under-credited)** | `adapters/codex/spool.go:25,153`; `client/client.go:148-152`. Under-credit: a durable JSONL outbox, stable `event_id`, and an `Idempotency-Key` header per request (`client/client.go:206-209`) already exist — the gap is server-side dedupe, receipts, and sequencing. |
| SL-09 | Content capture defaults ON | **Confirmed** | `devconfig.go:227-235`; `QUICKSTART.md:72-74`. **But**: this is a deliberate brian decision (2026-07-15, reversing OD4), not an oversight. The report's recommendation to flip the default is an OD-class decision — see §4. |
| SL-10 | Codex REQUIRE_APPROVAL → deny | **Confirmed fact, misleading framing** | `adapters/codex/enforce.go:563-564`, but the deny is documented tighten-only rationale (OD-SL7-ASK): Codex's parser rejects `ask`, and a fallthrough under `approval_policy=never` would auto-run ungoverned. Deny is the safe mapping until Codex supports an ask-equivalent. |
| SL-11 | Ambient `CODEX_THREAD_ID` contaminates tests | **Confirmed — reproduced** | With `CODEX_THREAD_ID=test-contamination-123` exported: 14 failures in the `adapters/common/git` module + 1 in `cli` (`TestUnifiedBinaryGitHookStampsCommit`). All pass without it. The zero-value resolver (`adapters/common/git/session.go:106-131`) reads ambient env at Tier-0. |

### 1.2 Provider capability claims

**Claude Code — all 5 claims confirmed** against code.claude.com docs: PreToolUse carries `tool_use_id`; SubagentStart/SubagentStop carry `agent_id`/`agent_type` (and `agent_id` appears on tool calls inside subagents); native OTel exports metrics/logs/traces(beta) with `session.id`/`organization.id`/`user.id` and prompt content **off by default** (`OTEL_LOG_USER_PROMPTS` opt-in); managed settings support `allowManagedHooksOnly`, `allowManagedPermissionRulesOnly`, `disableSideloadFlags`, MCP allowlists, server-managed settings with hourly refresh and fail-closed startup (`forceRemoteSettingsRefresh`); sandboxing (Seatbelt/bubblewrap) is admin-enforceable via `failIfUnavailable` + `allowUnsandboxedCommands:false`. Beyond the report: `strictPluginOnlyCustomization`, `sandbox.credentials` masking, managed-mcp.json, `processWrapper`, version floors, audit-log API, ZDR.

**Codex — all 5 claims confirmed** (the `learn.chatgpt.com` URLs are genuine — that domain now hosts official Codex docs; `developers.openai.com/codex` 308-redirects there). Hooks: full event set incl. `PermissionRequest`, `SubagentStart/Stop`, `turn_id`; SessionEnd landed in 0.145.0 as our SL-7 work found. `requirements.toml` + `allow_managed_hooks_only` are real and MDM-deployable (`/etc/codex/`, `com.openai.codex` preference domain), with pinnable approval/sandbox/network/MCP/feature policy and cloud-managed requirement bundles. The session-tree rule verifies verbatim: *root threads use their own thread id as the session id; forked threads keep the root's session id* — so our `session ≡ thread` mapping is correct for unforked CLI runs and wrong under forks. Two nuances: the Compliance API covers ChatGPT-authenticated usage only and centers on prompts/responses **without tool-call/file-action records** (exactly the gap shift-left fills — a positioning point, not a weakness); Linux sandboxing is now documented as bwrap+seccomp.

### 1.3 Landscape (positioning intel)

- **Entire (entire.io)** — ex-GitHub-CEO Dohmke, launched July 2026: git host storing an agent session checkpoint per commit (with secret redaction). Closest analog to our lineage story, but git-host-centric capture with **no policy enforcement plane**.
- **Endor Labs coding-agent governance** — hooks-based visibility/guardrails inside Claude Code/Cursor; architecturally closest to our adapter model.
- **Netra Security (netrasecurity.ai)** — shadow-AI / secure-AI-coding platform covering coding agents, MCP calls, secret exfiltration. (Distinct from getnetra.ai, an agent-observability/eval platform the report cites — likely a name collision in the report's references.)
- **Nightfall, Harmonic, Prompt Security (SentinelOne)** — DLP/inline-usage governance for AI coding tools; egress-plane players we should integrate around, not compete with.
- **Standards**: Sigstore gitsign + in-toto/SLSA attestations are the natural substrate for verified commit provenance; OpenID AuthZEN drafts (MCP tool authorization, access-request/approval profiles) and Okta Cross App Access are the agent-identity track; MCP spec now ships a formal authorization spec + security best practices.
- **Differentiation**: nobody else combines cross-provider normalized events + in-process enforcement + commit→deploy lineage on one governance plane. Cursor Enterprise notably has **no detailed AI-activity audit log**; Codex Compliance API has no tool-call records. Shift-left's event trail is the product.

## 2. Where the report over- and under-shoots

**Under-credits (already shipped):** Tier-1 secret/entropy redaction (`decision/secrets.go`, on by default); SL3-SEC-3 structural egress posture (tool commands/file bodies never decoded on observe); enforce opt-in with observe default and never-on-by-error; AIP Ed25519 signing + `Idempotency-Key` on every emit; trailer sink validation; ownership-verifier fail-closed hardening; bundle-load safety validation; INV-1 secret hygiene.

**Over-shoots (not ours to build, or not now):** OIDC/device-posture short-lived session tickets (an openbox-backend identity program); an egress proxy/DLP plane (use provider sandbox/network controls + partner DLP; our job is to *record posture*, not proxy traffic); a central approval broker (blocked on Codex ask-support; CC already has ask); risk-engine/SIEM/legal-hold operations (control-plane roadmap, not this repo).

## 3. Realistic plan (proposed epic E8 — "assurance & truth")

Sequenced by leverage-per-effort, reusing existing machinery per the repo rule. Sibling-repo dependencies marked.

### Phase 0 — truth & hygiene (no ADR, days)

- **E8-S1 Test isolation (SL-11).** Scrub `CODEX_THREAD_ID` (and the other ambient session vars) in the `adapters/common/git` and `cli` test harnesses; inject env access where practical; add a fork/ambient-contamination regression test. Acceptance: full suite passes with `CODEX_THREAD_ID` exported.
- **E8-S2 Doc truth (SL-04, SL-07).** README: rename/remove the "Egress proxy" column (it describes telemetry base-URL routing, not egress control) and correct "native OTel push" to hooks+spool. Refresh `contracts/dev-event/COVERAGE.md` for the shipped Codex adapter (SessionEnd real ≥0.145.0, token extraction real). State explicitly in README/QUICKSTART that the trailer is an *inferred* claim and enforce assurance depends on managed deployment. Acceptance: no doc claims egress/provenance control that code doesn't implement.

### Phase 1 — session-tree identity + posture visibility (adapter/schema, small)

- **E8-S3 CC correlation fields (SL-06).** Parse `tool_use_id`, `agent_id`, `agent_type` in `adapters/claude-code/hookevent.go` and carry them the same way the Codex adapter already carries `tool_use_id` (metadata + span pairing). Structural IDs only — INV-2-safe. Additive.
- **E8-S4 Codex session-tree fields (SL-06).** Carry `thread_id` and `session_id` as distinct fields (per the verified app-server rule: forked threads keep the root session id). For CLI runs today they're equal; the schema stops hard-coding that. Additive `schema_version` bump; wire as metadata pass-through (no core change expected — verify classifier indifference).
- **E8-S5 Posture-on-SessionStart (assurance tiers, cheap first cut).** Emit effective posture as structural metadata on SessionStarted: enforce on/off, fail-open/closed, bundle version+hash, staleness-check outcome (incl. "skipped: no token" — closes the silent-skip nuance of SL-03), secret-detection state, content-capture state, adapter+provider version. This makes the control plane able to distinguish T0/T1/T2 *without* MDM, and turns "staleness silently skipped" into recorded evidence. Backend display of a tier label is a follow-up in openbox-backend.

### Phase 2 — policy integrity (SL-02) + delivery assurance (SL-08)

- **E8-S6 Signed policy bundles.** Backend signs the bundle payload (Ed25519 — reuse AIP key machinery); local load verifies signature, monotonic version (no rollback), and expiry. In an enforce profile: unsigned/expired/unverifiable bundle, or `RawRegoUnlocalized`, no longer degrades to allow for high-risk classes (Bash/MCP) — deny or Tier-2 escalate instead. Requires openbox-backend work → **ADR-0008**. This is the single highest-value P0 in the report that is fully within our architecture.
- **E8-S7 Server dedupe + receipts.** Land the already-anticipated server-side `event_id` dedupe (the `[EXT-core]` seam in `spool.go:95-96`) and return a receipt id; spool retries per-event delivery errors instead of dropping (`spool.go:153`). Per-session sequence + prev-hash can reuse the existing `SessionMerkleLeafEntity` tamper-evidence path rather than a new chain. openbox-core work → sibling branch, local-only per convention.

### Phase 3 — managed deployment tier (SL-01)

- **E8-S8 Managed-config templates + installer mode.** Ship reference `managed-settings.json` (CC: managed hooks + `allowManagedHooksOnly`, sandbox required, sideload disabled) and `/etc/codex/requirements.toml` (managed hook + `allow_managed_hooks_only`, pinned approval/sandbox/network policy); an installer `--managed` mode that writes system paths when privileged.
- **E8-S9 System-level devconfig precedence.** `/etc/openbox/dev.json` (or equivalent) whose `enforce`/`fail_closed`/`content_capture` take precedence over user config and env — the managed layer SL-01 says is missing. `openbox doctor`-style check reports effective posture and its source (managed vs user), feeding E8-S5 so a user-level flip is visible server-side as a tier downgrade.

### Phase 4 — verified provenance (SL-05)

- **E8-S10 Signed post-commit attestation.** post-commit signs {repo identity, commit/tree/parent SHAs, session id(s), bundle hash, adapter version} with the dev agent Ed25519 key; the git action verifies signature + DID binding and emits `verified` above today's `attributed`/`inferred`. Aligns later with in-toto/gitsign; protected-deploy gating on `verified` is an openbox-backend policy follow-up.

### Explicitly deferred (recorded, not planned)

Short-lived OIDC session tickets (SL-03 remedy) — backend identity program; egress plane (beyond posture recording) — provider controls + DLP partners; approval broker (SL-10) — revisit when Codex supports an ask-equivalent; capability registry as a service (SL-07) — per-adapter `Capabilities()` + conformance tests suffice at two adapters, revisit at Cursor (SL-8).

## 4. OD decisions to surface (brian's call, not inferred)

- **OD-E8-1 — Content-capture default.** The report (SL-09) recommends metadata-only as the enterprise default; brian explicitly set default-ON on 2026-07-15. Options: keep ON (status quo); flip to metadata-only; or keep ON for the current tier but make metadata-only the default of the *managed/enterprise profile* (E8-S9) — capture stays a per-org opt-in there. The third option preserves both intents.
- **OD-E8-2 — Scope/priority of the managed tier.** Does Phase 3 (managed deployment) outrank the SL-8 Cursor adapter, or does provider breadth come first? The report says managed-first for any enterprise enforce claim; current repo "Next" says Cursor.
- **OD-E8-3 — Enforce posture without a valid bundle (E8-S6).** Today raw-rego/absent bundle serves fail-open with a warning. Deny-high-risk-classes on unverifiable policy is a behavior change that will be felt by developers; needs an explicit decision on the risk-class split.
