# Provider coverage & mapping rules

How the three target coding tools' real event surfaces map onto this contract's 7 lifecycle
types, plus the **bounded non-goals**. Sourced from a G3_REVIEW pass over the tools' official
docs (Claude Code hooks + OTel; Cursor hooks + Admin API; OpenAI Codex hooks + `notify` + Usage/Cost API).
This is the reference an adapter author (SL-4 Claude Code, SL-7 Codex, SL-8 Cursor) implements `emit()` against.

## 1. Lifecycle coverage matrix

| Contract type | Claude Code | Cursor | Codex |
|---|---|---|---|
| `SessionStarted` | `SessionStart` hook | `sessionStart` | `SessionStart` hook |
| `PromptSubmitted` | `UserPromptSubmit` + `user_prompt` OTel event | `beforeSubmitPrompt` | `UserPromptSubmit` |
| `ToolCall` | `PreToolUse` | `preToolUse` / `beforeShellExecution` / `beforeMCPExecution` / `beforeReadFile` | `PreToolUse` / `PermissionRequest` |
| `ToolResult` | `PostToolUse` | `postToolUse` / `afterShellExecution` / `afterMCPExecution` / `afterFileEdit` | `PostToolUse` |
| `SessionEnded` | `SessionEnd` hook | `sessionEnd` | **synthesize** (no SessionEnd hook — only turn-scope `Stop`) |
| `CommitCreated` | *(git-level)* | *(git-level)* | *(git-level)* |
| `Deploy` | *(git-level)* | *(git-level)* | *(git-level)* |

## 2. Field-derivation rules

- **`tool.kind`**: file tools (Read/Edit/Write, `beforeReadFile`, `afterFileEdit`, Codex `apply_patch`) → `file`; shell/Bash (`beforeShellExecution`, Codex `Bash`) → `shell`; MCP (`beforeMCPExecution`, `mcp__*`) → `mcp`.
- **`tool.mcp_server`**: Claude Code/Codex — parse from `tool_name` `^mcp__([^_]+)__`. Cursor — from the hook's `url`/`command`.
- **`span.file_path`**: Claude Code — `tool_input.file_path` (nested, not root). Cursor — `file_path` on file hooks. Codex — from `apply_patch` input.
- **`span.lines_count`/`bytes_*`**: derive from `edits[]` (Cursor) / tool response; often only available `PostToolUse`/`ToolResult`.
- **`tokens`/`cost`**: Claude Code — OTel `claude_code.token.usage` / `cost.usage`, per-session (has `session.id`). Cursor/Codex — **not per session** (Cursor Admin API = per-user hourly/daily; OpenAI Usage/Cost API = per-key/project/day). So for Cursor/Codex these are **absent** on the event and finops rolls up at agent/day granularity (architecture capability matrix). This is why `tokens?`/`cost?` are optional.
- **`ToolCall`↔`ToolResult` correlation**: carry the provider's `tool_use_id` in `metadata` (all three expose it) and reuse one `span_id` across the pair.

## 3. Bounded non-goals (Phase-1 v1.0) — documented, not gaps

The contract is honest about what it does **not** model in v1.0. None is required for the Phase-1 goals (observe / finops / session→commit→deploy lineage):

1. **Turn boundaries** (Cursor `stop`, Codex `Stop`) are **not** `SessionEnded` — they fire per agent-loop turn, and a session has many turns. Adapters must **not** map turn-stop → `SessionEnded`. Codex has no session-end hook, so its adapter **synthesizes** `SessionEnded` on process exit / idle.
2. **Subagent lifecycle** (`SubagentStart`/`SubagentStop`) — a subagent's *tool calls* still emit `ToolCall`/`ToolResult` (carry `metadata.agent_id`/`agent_type`); the start/stop markers themselves are Phase-1 telemetry-only (metadata), not a distinct lifecycle type. Note: a subagent is a nested actor, **not** a `tool.kind` — no new kind is needed.
3. **Compaction** (`PreCompact`/`PostCompact`) — context-window infra; dropped in Phase 1.
4. **Assistant message/thought** (Cursor `afterAgentResponse`/`afterAgentThought`; Codex `notify` `agent-turn-complete`) — completion **metrics** (tokens/cost) ride on `PromptSubmitted`; the completion **text** is gated `content`. A dedicated `CompletionReceived` type is a **v1.1** candidate.
5. **Non-session telemetry** (Cursor Tab hooks, `workspaceOpen`, cloud-agent sessions that never emit `sessionStart`) — the contract requires `openbox_session_id`, so events with no resolvable session are **not emittable** (adapter drops or synthesizes a session). Honest degradation — the architecture's "no false coverage" rule.
6. **`PermissionRequest` vs generic `preToolUse`/`PreToolUse` overlap** — adapters emit **one** `ToolCall` per tool invocation (prefer the specific pre-tool hook); `event_id` idempotency (INV-5) also guards double-counting.

Items 1, 2, and 4 are the candidate scope for a **v1.1** `schema_version` bump (`TurnEnded`, subagent nesting, `CompletionReceived`) if Phase-2 needs them. The `schema_version` field exists precisely so that bump is non-breaking.

## 4. Enforcement posture (Phase-1 observe)

All three tools have blockable hooks (Claude Code fail-controlled; **Cursor fail-open** by default, `failClosed:true` to flip; Codex beta, feature-gated `features.hooks`). Phase-1 adapters ignore the verdict entirely (INV-3 / D7); enforcement is Phase-2. The contract's `verdict` enum exists only to **parse** responses, not to act on them yet.
