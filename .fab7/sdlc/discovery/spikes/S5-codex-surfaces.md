# Spike S5 — OpenAI Codex CLI as a governable provider adapter

**Question (one sentence):** What are Codex CLI's extension/interception/telemetry surfaces, so it can become a provider adapter behind the generic contract (architecture §1b)?

**Status:** DONE (2026-07-07). **Owner:** brian@openbox.ai.
**Target:** `@openai/codex` / `codex` (open-source Rust CLI), docs ~v0.136+. **Beta caveat:** hooks are behind `features.hooks`; pin a tested Codex version — field names are version-sensitive.

**Big picture:** Codex has converged toward a Claude-Code-like surface — full hooks (blocking + input-rewriting), OpenTelemetry export, JSONL rollout transcripts, first-class MCP client+server, base-URL/provider egress override, and an **enterprise `requirements.toml` + MDM** lockdown story that is arguably the strongest org-mandate of the three tools.

---

## 1. Hooks / lifecycle — STRONG (block + modify)
Events: `SessionStart`, `SubagentStart`, `PreToolUse`, `PermissionRequest`, `PostToolUse`, `PreCompact`, `PostCompact`, `UserPromptSubmit`, `SubagentStop`, `Stop`. `PreToolUse` intercepts Bash, `apply_patch`, and MCP calls before execution. — developers.openai.com/codex/hooks, /config-reference
- Input (stdin JSON): `session_id`, `transcript_path`, `cwd`, `hook_event_name`, `model`, `permission_mode`; `turn_id`; `tool_name`, `tool_input`.
- Blocking: exit `2` = deny (stderr → model); JSON `hookSpecificOutput.permissionDecision="deny"`+reason (PreToolUse); `PermissionRequest` `decision.behavior="allow"|"deny"` (**any deny wins**); top-level `continue`/`stopReason`/`systemMessage`.
- **Modify:** `PreToolUse` `updatedInput` rewrites the call (redaction). `PostToolUse` `decision:"block"` does NOT undo an executed tool — replaces result with feedback (audit-grade).
- Enable `[features] hooks=true`; config `~/.codex/hooks.json` / inline `[hooks]` / repo `.codex/hooks.json`. Handler: `command` only today. Matcher = regex over `tool_name` (MCP via `mcp__<server>__<tool>`).
- **Trust:** non-managed hooks hashed + require `/hooks` trust; **managed hooks (requirements.toml/MDM) are policy-trusted and users cannot disable them.**

## 2. Approval / permission — STRONG
`approval_policy` (`untrusted`/`on-request`/`never`/granular); `sandbox_mode` (`read-only`/`workspace-write`/`danger-full-access`, OS Seatbelt/Landlock). Admin `[rules]` execpolicy decisions may only be `prompt`/`forbidden` (tighten, never loosen). — /config-reference, /agent-approvals-security, /enterprise/managed-configuration

## 3. Config & enterprise mandate — STRONG (real MDM story)
- User `~/.codex/config.toml` (`$CODEX_HOME`); project `.codex/config.toml` (trusted only); machine-local-only keys (egress/telemetry: `openai_base_url`, `model_provider`, `otel`, `notify`) can't be overridden by a repo.
- **Managed layers:** `managed_config.toml` (defaults, `/etc/codex/` or `~/.codex/` Windows) + **`requirements.toml`** (hard constraints, precedence: cloud-managed → macOS MDM `com.openai.codex:requirements_toml_base64` → `/etc/codex/requirements.toml` / `%ProgramData%\OpenAI\Codex\`). Enforces allowed approval/sandbox modes, features, network, and `[hooks]` with `managed_dir` + **`allow_managed_hooks_only`**. MDM precedence: managed prefs > managed_config > user config.

## 4. Telemetry — MEDIUM-STRONG
- **OpenTelemetry** built-in, **off by default**, opt-in `[otel]`: log events (API requests, streamed responses, user input, tool-approval decisions, tool results) + metrics (counters/histograms), tags `auth_mode`/`originator`/`session_source`/`model`/`app.version`; `exporter="otlp-http"|"otlp-grpc"`, `otel.log_user_prompt` opt-in. — /config-advanced, PR#2103
  - Gaps: `[otel]` fully honored in interactive CLI; `codex exec` = traces+logs but NO metrics; `codex mcp-server` = no telemetry (issue #12913).
- **Rollout transcripts (on-disk JSONL):** `~/.codex/sessions/YYYY/MM/DD/rollout-...-<id>.jsonl`, lines `{type,payload}` (`session_meta`, `event_msg` incl. `token_count`, `response_item`, `turn_context`). Plus `history.jsonl`, SQLite state.
- **Finops:** OpenAI Usage API + Costs API (org admin key), groupable by `project_id`/`user_id`/`api_key_id`/`model`. **Caveat: per-project/key/user, NOT per-session** — can't natively join a cost row to a rollout; needs per-developer key/project provisioning.

## 5. MCP — STRONG (client AND server)
Client: `[mcp_servers.<id>]` stdio or Streamable HTTP (OAuth via `codex mcp login`); `enabled_tools`/`disabled_tools`, `default_tools_approval_mode`, per-tool `approval_mode`. Server: `codex mcp-server` exposes `codex()`/`codex-reply()`. MCP calls interceptable via hooks (`mcp__…` matcher, `updatedInput` rewrites args).

## 6. Model/API egress — STRONG
`model_provider` + `[model_providers.<id>]` (`base_url`, `env_key`, `http_headers`, `wire_api="responses"`); top-level `openai_base_url`; `OPENAI_BASE_URL` env; Azure/Foundry supported. These keys are machine-local-only (repos can't redirect) → good for enforcement. Egress is standard HTTPS Responses-API → fully proxy/gateway-interceptable.

## 7. Session/commit attribution — MEDIUM
- Session ID reaches hooks (stdin `session_id`, `transcript_path`), rollout filenames, `session_meta`, OTel — **but NO `CODEX_SESSION_ID` env var / `codex status --json`** (open FR #8923).
- Git: `features.codex_git_commit`; `commit_attribution` (default `Codex <noreply@openai.com>`) inserted as `Co-authored-by:` trailer; `""` disables.
- **Integration:** capture session id inside a `SessionStart` hook and have OpenBox stamp the `OpenBox-Session` trailer (S3), since no env var exists.

---

## VERDICT (strong/medium/weak + sanctioned path)
- **Observe telemetry — STRONG:** native OTLP + rollout JSONL (token counts). Path: `[otel]`→OpenBox collector + rollout ingestion. Caveat: metrics gaps in `codex exec`/`mcp-server`.
- **Enforce policy — STRONG:** `PreToolUse`/`PermissionRequest` block (exit 2 / deny) + `updatedInput` rewrite over Bash/apply_patch/MCP; layered with approval/sandbox/`[rules]`. Analogous to Claude Code. Caveat: PostToolUse can't undo; hooks beta.
- **Session→commit lineage — MEDIUM:** session ids reach hooks/transcripts/OTel + co-author trailer, but assemble lineage inside a SessionStart hook (no env var).
- **Org mandate — STRONG (best-in-class):** `requirements.toml` + `managed_config.toml` + macOS MDM lock approval/sandbox/egress and install **managed hooks users can't disable** (`allow_managed_hooks_only`, `managed_dir`) — exceeds Cursor's dashboard-only, matches/beats Claude Code managed settings.

**Recommended Codex adapter = managed hooks (enforce + session capture) + OTel/rollout ingestion (observe) + Usage/Cost Admin API (finops) + pinned base-URL egress, all delivered/locked via requirements.toml/MDM.**

**Unknowns/caveats:** (1) `CODEX_SESSION_ID` env var not implemented (#8923); (2) OTel metric coverage per entry point version-dependent (#12913); (3) session-id-in-commit is a workaround, not documented; (4) `features.hooks` default may shift — pin version; (5) Usage/Cost API can't join cost→session without per-dev key/project provisioning.

**Primary sources:** developers.openai.com/codex/{config-reference, hooks, config-advanced, mcp, agent-approvals-security, enterprise/managed-configuration}; github.com/openai/codex #8923/#12913/PR#2103; learn.microsoft.com Azure/Foundry Codex; signoz.io/docs/codex-monitoring.
