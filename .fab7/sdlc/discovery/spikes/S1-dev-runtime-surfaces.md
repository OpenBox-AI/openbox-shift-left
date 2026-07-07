# Spike S1+ — Claude Code & Cursor integration surfaces for shift-left governance

**Question (one sentence):** What extension, interception, and telemetry surfaces do Claude Code and Cursor expose that OpenBox shift-left can integrate with to observe coding-agent sessions and enforce policy?

**Status:** Claude Code section complete (official docs, verified 2026-07-07). Cursor section pending deep-research verification.
**Method:** claude-code-guide agent over official docs (code.claude.com/docs) + deep-research workflow (5-angle web sweep, 3-vote adversarial claim verification).
**Owner:** brian@openbox.ai

---

## Part A — Claude Code (evidence-backed, doc URL per claim)

Valid as of Claude Code v2.1.200+ (July 2026).

### A1. Hooks — the enforcement surface

- **Events:** per-session `SessionStart`/`SessionEnd`; per-turn `UserPromptSubmit`/`Stop`/`StopFailure`; per-tool-call `PreToolUse`/`PostToolUse`; lifecycle `ConfigChange`/`CwdChanged`/`FileChanged`/`Setup`. — https://code.claude.com/docs/en/hooks.md
- **Handler types:** `command` (JSON stdin/stdout), **`http` (POST to an endpoint — OpenBox Core can be the hook target directly, no local shim required)**, `mcp_tool`, `prompt` (LLM-evaluated), `agent` (experimental). — https://code.claude.com/docs/en/hooks.md
- **Hook input includes** `session_id`, `prompt_id`, `transcript_path`, `cwd`, `permission_mode`, plus event fields (`tool_name`, `tool_input`). — https://code.claude.com/docs/en/hooks.md
- **Blocking semantics:** exit code 2 = block (stderr shown as reason); JSON `permissionDecision: deny|ask` in `hookSpecificOutput`; **`updatedInput` (PreToolUse) can rewrite a tool call and `updatedToolOutput` (PostToolUse) can rewrite results — i.e., redaction, not just deny**; `additionalContext` injects guidance. — https://code.claude.com/docs/en/hooks.md
- **MCP coverage:** matchers address MCP tools (`mcp__server__tool`, `mcp__*`, regex), so one PreToolUse hook governs shell, file, AND MCP calls uniformly. — https://code.claude.com/docs/en/permissions.md
- **Async hooks** (`"async": true`) allow non-blocking telemetry emission without adding latency. — https://code.claude.com/docs/en/hooks.md

### A2. Plugin system — the packaging/distribution surface

- A plugin bundles skills, **hooks**, MCP servers, agents, commands, LSP servers, background monitors, executables (`bin/`), and default settings — everything an "OpenBox governance plugin" needs in one artifact. — https://code.claude.com/docs/en/plugins.md
- Distribution: official/community marketplaces or **private self-hosted marketplaces** (git repo/URL/local path) with versioning, release channels, private-repo auth. — https://code.claude.com/docs/en/plugin-marketplaces.md
- **Org-wide enforcement:** managed settings `enabledPlugins` force-enables plugins users cannot disable; `strictKnownMarketplaces`/`blockedMarketplaces` control sources; `strictPluginOnlyCustomization` blocks non-plugin hooks/skills/MCP. — https://code.claude.com/docs/en/server-managed-settings.md, https://code.claude.com/docs/en/permissions.md

### A3. Managed/enterprise settings — the mandate surface

- Delivery: server-managed (claude.ai admin console → cached `~/.claude/remote-settings.json`, polled hourly) or endpoint MDM files (`/etc/claude-code/managed-settings.json` on Linux; `/Library/Application Support/ClaudeCode/managed-settings.json` on macOS; HKLM registry/`C:\Program Files\ClaudeCode` on Windows). — https://code.claude.com/docs/en/server-managed-settings.md
- Precedence: managed > CLI args > local project > project > user; managed cannot be overridden. — https://code.claude.com/docs/en/server-managed-settings.md#settings-precedence
- Governance-critical managed-only keys: `allowManagedHooksOnly`, `allowManagedMcpServersOnly`, `allowManagedPermissionRulesOnly`, `disableSideloadFlags`, `forceRemoteSettingsRefresh` (**fail-closed startup**), `disableBypassPermissionsMode`, `policyHelper` (dynamic policy computation — a natural OpenBox integration point). — https://code.claude.com/docs/en/permissions.md
- Caveat: server-managed settings require Teams/Enterprise org OAuth; NOT available on Bedrock/Vertex/Foundry/custom gateways (MDM files still work there). — https://code.claude.com/docs/en/server-managed-settings.md

### A4. Telemetry — the observation surface

- Native OpenTelemetry: `CLAUDE_CODE_ENABLE_TELEMETRY=1`, `OTEL_METRICS_EXPORTER=otlp`, `OTEL_LOGS_EXPORTER=otlp`, `OTEL_EXPORTER_OTLP_ENDPOINT`, headers for auth (OpenBox collector = standard OTLP endpoint). — https://code.claude.com/docs/en/monitoring-usage.md
- Metrics: `claude_code.token.usage`, `cost.usage`, `session.count`, `lines_of_code.count`, **`commit.count`**, `code_edit_tool.decision`, `active_time.total`; attributes include `session.id`, `user.email`, `organization.id`. **Finops question answered natively.** — https://code.claude.com/docs/en/monitoring-usage.md
- Events: `user_prompt`, `api_request/response/error`, `tool_decision` (accept/deny/block), `tool_result`, `mcp_server_connection`, `plugin_installed/loaded`, `skill_activated`, `auth`, `compaction`; correlated by `session_id` + `prompt_id`. Content-level flags (`OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_TOOL_DETAILS`, ...) are opt-in — privacy boundary (OD4) is configurable at the source. — https://code.claude.com/docs/en/monitoring-usage.md
- Transcripts: `~/.claude/projects/<project>/<session-id>.jsonl`, 30-day retention (`cleanupPeriodDays`); format internal/unstable — **use hooks' `transcript_path` + SessionEnd archival or `/export`, not direct parsing**. — https://code.claude.com/docs/en/sessions.md

### A5. Session/commit attribution — the lineage surface

- `session_id` exposed in: hook inputs, transcript filename, `claude -p --output-format json`, SDK init message, `CLAUDE_SESSION_ID` env (subagent contexts). — https://code.claude.com/docs/en/sessions.md
- Git: auto `Co-Authored-By` trailer (`includeCoAuthoredBy`, default true); commit metric emitted; Anthropic's own analytics matches sessions→PRs within a 21-day window — precedent for our session→commit binding. A `prepare-commit-msg`/hook-stamped `OpenBox-Session:` trailer remains our mechanism. — https://code.claude.com/docs/en/analytics.md
- Resume/fork semantics (`--resume`, `--fork-session`) mean one logical change can span multiple session IDs — attribution must handle session graphs, not single IDs (spike S3 confirmed as needed).

### A6. Model/API egress — the gateway surface

- `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN`/`apiKeyHelper` routes ALL model traffic through a gateway; documented gateway protocol (Messages API pass-through, `/models` discovery). `ANTHROPIC_CUSTOM_HEADERS` for tenant/routing tags. — https://code.claude.com/docs/en/llm-gateway-connect.md
- Bedrock/Vertex/Foundry variants have their own base-URL + skip-auth envs. — https://code.claude.com/docs/en/llm-gateway-connect.md

### A7. Programmatic control — the supervision surface

- Headless `claude -p` with `--output-format json|stream-json` (returns `session_id`, `usage`, `cost`); `--json-schema` structured output. — https://code.claude.com/docs/en/headless.md
- Agent SDK (Python/TS): in-process hook callbacks (`HookMatcher`), `allowed_tools`, `permission_mode`, session resume — a governance wrapper could supervise CI/automation runs entirely via SDK. — https://code.claude.com/docs/en/agent-sdk

### Claude Code verdict for shift-left

Every Phase-1 and Phase-2 need has a first-class, documented surface:

| Shift-left need | Claude Code surface | Strength |
|---|---|---|
| Session registration/identity | SessionStart hook (+ `$CLAUDE_ENV_FILE`), session_id everywhere | strong |
| Telemetry/finops | Native OTel metrics+events to OpenBox collector | strong |
| Session→commit lineage | Hooks + git trailers; native commit metric; Co-Authored-By precedent | strong |
| Tool/MCP policy enforcement | PreToolUse deny/ask/rewrite incl. `mcp__*`; http hook type → OpenBox Core direct | strong |
| Prompt/output guardrails | UserPromptSubmit + PostToolUse rewrite; or LLM gateway | strong |
| Org mandate (can't opt out) | Managed settings: enabledPlugins, allowManagedHooksOnly, disableSideloadFlags, fail-closed refresh | strong |
| Packaging | Plugin (hooks+skill+MCP+bin) via private marketplace | strong |

---

## Part B — Cursor (deep-research workflow, official docs + adversarial verification)

Sourced from the deep-research sweep (docs.cursor.com / cursor.com/docs + practitioner sources), recovered from the workflow journal after a mid-run model-limit interruption. Cursor Hooks are **v1.7+ and beta** — treat as moving target.

### B1. Hooks — the enforcement surface (beta)

- **Introduced v1.7 (beta).** External scripts run at agent-loop stages; each hook is a standalone process, JSON over stdin/stdout. — cursor.com/docs/hooks
- **Documented lifecycle events:** `beforeSubmitPrompt`, `beforeShellExecution`, `beforeMCPExecution`, `beforeReadFile`, `afterFileEdit`, `stop` (docs list these six explicitly; some sources also cite `afterShellExecution`/`afterMCPExecution`/`sessionStart`/`sessionEnd` — treat the extended set as unverified until confirmed against current docs). — cursor.com/docs/hooks
- **Blocking semantics:** hook returns JSON `{ "continue": bool, "permission": "allow|deny|ask", "userMessage": ..., "agentMessage": ... }`; exit code 2 blocks (= `deny`). **CAUTION: other non-zero exit codes FAIL OPEN (action proceeds)** — the opposite of a fail-closed default and a real governance risk vs Claude Code. — cursor.com/docs/hooks, gitbutler.com/cursor-hooks-deep-dive
- **`afterMCPExecution` fires after a tool returns but before the response enters the agent's context** — enables output scanning/redaction (guardrail application on MCP results). — cursor.com/docs/hooks
- **Config locations & precedence:** project `.cursor/hooks.json`, user `~/.cursor/hooks.json`, enterprise `/etc/cursor/hooks.json` (macOS `/Library/Application Support/Cursor/hooks.json`); **Team hooks distributed via cloud dashboard on Enterprise plan.** Priority: **Enterprise > Team > Project > User.** — cursor.com/docs/hooks
- **MCP hook payload** includes server name, tool name, arguments, and MCP server config (URL/launch command) — enough for an org-wide MCP inventory + audit. — cursor.com/docs/hooks

### B2. MCP support

- Config: project `.cursor/mcp.json`, global `~/.cursor/mcp.json`; transports stdio / SSE / Streamable HTTP (OAuth-capable). — cursor.com/docs/mcp
- Enterprise admins allowlist which MCP servers/tools users may run, from the Cursor dashboard (by stdio command pattern or remote URL). — cursor.com/docs/mcp
- Default enforcement is a **per-call user approval prompt**, not a programmatic gate — hooks (`beforeMCPExecution`) are the programmatic path. — cursor.com/docs/mcp
- **Shared standard:** Cursor and Claude Code both speak MCP, so one OpenBox MCP gateway could front both. — cursor.com/docs/mcp

### B3. permissions.json — allowlist, NOT a security boundary

- `~/.cursor/permissions.json` (per-user) + `<workspace>/.cursor/permissions.json` (per-repo), arrays concatenated; `server:tool` glob syntax (`my-server:*`, `*:*`). — cursor.com/docs/reference/permissions
- **Docs explicitly: "Not a security boundary. Allowlists and autoRun instructions are best-effort convenience."** Do not rely on it for enforcement. — cursor.com/docs/reference/permissions
- Precedence: **Team admin (dashboard) > permissions.json > IDE settings UI**; admin settings can prevent permissions.json from adding entries. — cursor.com/docs/reference/permissions

### B4. Telemetry / observation — NO OpenTelemetry

- **No OTel export.** Built-in telemetry = basic usage metrics only. — cursor.com/docs/enterprise/compliance-and-monitoring
- **Admin API (Teams/Enterprise), server-side, pollable without client instrumentation:** `/teams/filtered-usage-events` returns per-request `inputTokens`, `outputTokens`, `cacheWrite/ReadTokens`, `totalCents`, `chargedCents`, `model`; plus `isHeadless` (background-agent flag) and `hostingType` (self-hosted spend isolation). Basic-auth API key, **rate-limited ~20 req/min per team, poll ≤ 1/hour.** This is the finops/attribution surface. — cursor.com/docs/account/teams/admin-api
- **Enterprise audit-log streaming** to SIEM (Splunk/Sumo/Datadog), webhooks, S3, Elasticsearch/CloudWatch — but **must be enabled by contacting Cursor (not self-serve)**, and the taxonomy is **administrative/config events only** (login, add_user, team_settings, team_hook, privacy_mode…), NOT per-tool-call telemetry. — cursor.com/docs/enterprise/compliance-and-monitoring
- **Cursor explicitly does NOT log agent responses or generated code.** For prompt/output content, **Cursor's own official guidance is: "use hooks to log prompts and code."** Content capture must be client-side (hooks). — cursor.com/docs/enterprise/compliance-and-monitoring
- Admin API also offers limited enforcement: repo blocklists and per-user spend limits (Enterprise-only).

### B5. Model/API egress — gateway interception is UNRELIABLE for Cursor

Verified-negative, high-confidence — this is the sharpest divergence from Claude Code:

- Ask/Plan modes honor a custom OpenAI base URL, **but Agent mode routes through Cursor's own backend (`streamFromAgentBackend`) regardless of custom base-URL settings**; practitioner confirmed zero `POST /v1/chat/completions` on their gateway during Agent flows (Cursor 3.0.4, Apr 2026). — forum.cursor.com/t/…156565
- **Sub-agents do not inherit custom base URL / BYOK** — they fall back to Cursor servers (`resource_exhausted`). — forum.cursor.com
- Cursor **ignores the custom URL when the model name is one it recognizes** (e.g. `claude-3.5-sonnet`); workaround is renaming the model to an unknown tag (`cus-…`) so Cursor treats it as a local LLM — a fragile hack, and it sends **Anthropic-schema tool defs (`input_schema`)** a proxy must translate. — practitioner sources
- **Cursor officially discourages custom LLM gateways** (latency/rate-limit/compat) and **recommends its hooks feature for security controls instead.** — cursor.com docs
- Fixed Cursor-operated egress domains (`api2/api5/api3/api4.cursor.sh`, `repo42.cursor.sh`); **TLS MITM inspection commonly breaks Agent** and Cursor advises disabling SSL inspection for its domains. Network-level can allowlist/monitor but not redirect. — practitioner sources

**Implication:** For Cursor, the LLM-egress-proxy strategy (brainstorm option B3) is effectively non-viable for Agent mode. **Hooks are the sanctioned and only reliable interception surface** — which Cursor itself endorses.

### Cursor verdict for shift-left

| Shift-left need | Cursor surface | Strength |
|---|---|---|
| Session registration/identity | hooks (`beforeSubmitPrompt`/sessionStart if confirmed); no env-based session id documented | medium |
| Telemetry/finops | Admin API `/teams/filtered-usage-events` (poll ≤1/hr, Teams/Ent); no OTel | medium (server-side, coarse cadence) |
| Session→commit lineage | via hooks (client-side); no native commit metric/trailer documented | medium |
| Tool/MCP policy enforcement | hooks deny/ask incl. `beforeMCPExecution`; **fail-open on script error** | medium (beta + fail-open caveat) |
| Prompt/output guardrails | `beforeSubmitPrompt` + `afterMCPExecution` redaction; Cursor-endorsed | medium |
| Org mandate (can't opt out) | Enterprise Team hooks (dashboard) + `/etc/cursor/hooks.json`; permissions.json is NOT a boundary | medium (Enterprise plan gated) |
| Egress proxy | **Not viable for Agent mode** — routes to Cursor backend | weak |

---

## Cross-tool synthesis

- **Hooks are the one architecture that spans both tools.** Both expose JSON-over-stdio hooks with allow/deny/ask and MCP-call interception; both endorse hooks for governance. An OpenBox hook contract (emit telemetry + call policy) ports across Claude Code and Cursor with per-tool adapters. This is the recommended integration primitive.
- **Two critical divergences to design around:**
  1. **Fail direction.** Claude Code blocks only on exit 2 (errors fail *open* too, but deny is explicit and managed deny-lists always apply); **Cursor hooks fail OPEN on any non-2 error.** OpenBox hook scripts must be defensive: explicit deny on internal error if fail-closed is required.
  2. **Egress.** Claude Code gateway routing (`ANTHROPIC_BASE_URL`) is first-class; **Cursor Agent egress is not interceptable.** A gateway strategy can only be Claude-Code-wide, never the cross-tool primitive — reinforces hooks-first.
- **Telemetry asymmetry.** Claude Code = rich native OTel (push, real-time, per-event). Cursor = poll-based Admin API (≤1/hr, coarse) + optional Enterprise audit streaming (config events only) + hooks for content. Phase-1 finops is easy on Claude Code, coarser on Cursor.
- **Org mandate exists on both** but is Enterprise-tier gated (Claude Code managed settings / Cursor Team hooks + `/etc` MDM files).

---

## Assumptions / Unknowns (running list)

- **A-1 (RESOLVED, Part B):** Cursor has hooks (v1.7 beta) with allow/deny/ask and MCP interception — rough parity with Claude Code on enforcement, weaker on telemetry (no OTel; poll-based Admin API) and on egress (Agent mode not interceptable). Hooks are the common cross-tool primitive.
- **A-2 (open → spike S2):** Real-world latency of policy-call hooks (`http`/`command`) against remote OpenBox Core vs local OPA sidecar — docs don't state per-handler timeout defaults; benchmark needed before enforcement phase.
- **A-3 (noted):** Claude Code transcript JSONL is explicitly unstable — do NOT parse it; use OTel events + hook payloads. Cursor does not persist agent-output transcripts server-side at all — content only via hooks.
- **A-4 (noted):** Claude Code server-managed settings unavailable on Bedrock/Vertex/custom-gateway auth — MDM files are the universal fallback. Cursor org mandate (Team hooks + `/etc/cursor/hooks.json`) is Enterprise-plan gated.
- **A-5 (NEW caveat):** Cursor hooks **fail open** on non-2 exit codes; Cursor Hooks are **beta** with reported instability (Oct 2025) — enforcement reliability on Cursor is lower than on Claude Code.
- **A-6 (unverified):** Cursor extended hook event set (`sessionStart/End`, `after*Execution`, `pre/postToolUse`) — one source lists them, official docs enumerate six; confirm against current cursor.com/docs/hooks before relying on session-lifecycle events for Cursor.

## Decisions surfaced (owners)

- **OD6 (owner: brian, technical):** Hook handler type for policy calls — `http` direct to OpenBox Core (Claude Code supports `http` natively) vs `command` wrapper calling a local OpenBox daemon/OPA sidecar (latency, offline, credentials). Cursor is `command`-only (external process). Gate on spike S2.
- **OD7 (owner: brian, product):** Phase-1 telemetry content boundary — metadata-only (Claude Code OTel + Cursor Admin API, both metadata) vs client-side content capture via hooks (satisfies OD4). Content path is the ONLY option on Cursor if prompts/outputs are required.
- **OD8 (owner: brian, product/scope):** Given Cursor's beta/fail-open/egress limitations, do we ship **Claude Code first (deeper, safer surface) and treat Cursor as fast-follow**, or invest in cross-tool hook parity from v1? (Feeds OD5.)
- **OD9 (owner: brian, technical):** Fail-open vs fail-closed policy for OpenBox hook scripts — must be explicit given Cursor's fail-open default; a governance product usually wants fail-closed for deny decisions.
