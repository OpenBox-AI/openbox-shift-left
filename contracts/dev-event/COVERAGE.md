# Provider coverage & mapping rules

How the three target coding tools' real event surfaces map onto this contract's 7 lifecycle
types, plus the **bounded non-goals**. Sourced from a G3_REVIEW pass over the tools' official
docs (Claude Code hooks + OTel; Cursor hooks + Admin API; OpenAI Codex hooks + `notify` + Usage/Cost API).
This is the reference an adapter author (SL-4 Claude Code, SL-7 Codex, SL-8 Cursor) implements `emit()` against.

**Adapter status (keep this line current — report SL-07 was a stale-doc finding):** Claude Code
and Codex adapters are **shipped**, observe + opt-in enforce + opt-in finops; each adapter's
`Capabilities()` is the authoritative per-provider profile and this document must agree with it.
The **Cursor** column below is a *surface survey*, not shipped support — SL-8 is unbuilt. Last
reconciled 2026-07-29 (E8-S2) against `adapters/claude-code/capabilities.go` and
`adapters/codex/capabilities.go`.

## 1. Lifecycle coverage matrix

| Contract type | Claude Code *(shipped)* | Cursor *(survey only — SL-8 unbuilt)* | Codex *(shipped)* |
|---|---|---|---|
| `SessionStarted` | `SessionStart` hook | `sessionStart` | `SessionStart` hook |
| `PromptSubmitted` | `UserPromptSubmit` | `beforeSubmitPrompt` | `UserPromptSubmit` |
| `ToolCall` | `PreToolUse` | `preToolUse` / `beforeShellExecution` / `beforeMCPExecution` / `beforeReadFile` | `PreToolUse` / `PermissionRequest` |
| `ToolResult` | `PostToolUse` | `postToolUse` / `afterShellExecution` / `afterMCPExecution` / `afterFileEdit` | `PostToolUse` |
| `SessionEnded` | `SessionEnd` hook | `sessionEnd` | `SessionEnd` hook (real, ≥ 0.145.0 — no longer synthesized) |
| `CommitCreated` | *(git-level)* | *(git-level)* | *(git-level)* |
| `Deploy` | *(git-level)* | *(git-level)* | *(git-level)* |

## 2. Field-derivation rules

- **`tool.kind`**: file tools (Read/Edit/Write, `beforeReadFile`, `afterFileEdit`, Codex `apply_patch`) → `file`; shell/Bash (`beforeShellExecution`, Codex `Bash`) → `shell`; MCP (`beforeMCPExecution`, `mcp__*`) → `mcp`.
- **`tool.mcp_server`**: Claude Code/Codex — parse from `tool_name` `^mcp__([^_]+)__`. Cursor — from the hook's `url`/`command`.
- **`span.file_path`**: Claude Code — `tool_input.file_path` (nested, not root). Cursor — `file_path` on file hooks. Codex — from `apply_patch` input.
- **`span.lines_count`/`bytes_*`**: derive from `edits[]` (Cursor) / tool response; often only available `PostToolUse`/`ToolResult`.
- **`tokens`/`cost`**: both shipped adapters extract **per-session** token counts at `SessionEnded`, opt-in via `ResolveFinops` (default off) and off the hot path; the parse is numbers-only so no content rides along (INV-2). Claude Code reads the local **transcript** (`usage.go`) and can populate `cost`; Codex reads the **rollout JSONL** `total_token_usage` (`SL7-C`) and leaves `cost` nil — that token path carries no cost field. Neither uses the providers' OTel/Usage APIs. Cursor (unbuilt) has no per-session source known: its Admin API is per-user hourly/daily, so a future adapter rolls finops up at agent/day granularity. `tokens?`/`cost?` stay optional for exactly this reason.
- **`ToolCall`↔`ToolResult` correlation**: carry the provider's `tool_use_id` in `metadata` (all three expose it) and reuse one `span_id` across the pair. Both shipped adapters do this by id rather than by heuristic — see MAPPING.md "Correlation metadata keys" for the `span.function` pairing channel and why it does not egress for non-MCP tools.

## 3. Bounded non-goals (Phase-1 v1.0) — documented, not gaps

The contract is honest about what it does **not** model in v1.0. None is required for the Phase-1 goals (observe / finops / session→commit→deploy lineage):

1. **Turn boundaries** (Cursor `stop`, Codex `Stop`) are **not** `SessionEnded` — they fire per agent-loop turn, and a session has many turns. Adapters must **not** map turn-stop → `SessionEnded`. (Historical note: Codex once had no session-end hook and its adapter synthesized one; `SessionEnd` is real as of 0.145.0 and the synthesis is gone.)
2. **Subagent lifecycle** (`SubagentStart`/`SubagentStop`) — the start/stop markers get no lifecycle type of their own. A subagent is a nested actor, **not** a `tool.kind` — no new kind is needed. As of **E8-S3** the Claude Code adapter carries `metadata.agent_id`/`agent_type`, which Claude Code puts on *every* payload fired inside a subagent — so the tree (which events belong to which subagent, and of what kind) is reconstructable from the tool events alone, and the boundary hooks stay unwired rather than being bent onto a prompt-shaped `SignalReceived`. Wire them only if explicit boundaries are needed for something the ids cannot answer.
3. **Compaction** (`PreCompact`/`PostCompact`) — context-window infra; dropped in Phase 1.
4. **Assistant message/thought** (Cursor `afterAgentResponse`/`afterAgentThought`; Codex `notify` `agent-turn-complete`) — completion **metrics** (tokens/cost) ride on `PromptSubmitted`; the completion **text** is gated `content`. A dedicated `CompletionReceived` type is a **v1.1** candidate.
5. **Non-session telemetry** (Cursor Tab hooks, `workspaceOpen`, cloud-agent sessions that never emit `sessionStart`) — the contract requires `openbox_session_id`, so events with no resolvable session are **not emittable** (adapter drops or synthesizes a session). Honest degradation — the architecture's "no false coverage" rule.
6. **`PermissionRequest` vs generic `preToolUse`/`PreToolUse` overlap** — adapters emit **one** `ToolCall` per tool invocation (prefer the specific pre-tool hook); `event_id` idempotency (INV-5) also guards double-counting.

Items 1, 2, and 4 are the candidate scope for a **v1.1** `schema_version` bump (`TurnEnded`, subagent nesting, `CompletionReceived`) if Phase-2 needs them. The `schema_version` field exists precisely so that bump is non-breaking.

## 4. Enforcement posture

All three tools have blockable hooks (Claude Code fail-controlled; **Cursor fail-open** by default, `failClosed:true` to flip; Codex feature-gated `features.hooks`, stable and on by default ≥ 0.145.0).

Enforcement **shipped** in Phase-2 (E6 for Claude Code, SL7-B for Codex) and is **opt-in**: enable at onboarding with `openbox dev init … --enforce`, otherwise a session observes only and every verdict is treated as allow (INV-3). The gate decides in-process (ADR-0006) and is **tighten-only** — it never turns a provider's own deny into an allow, and the one `allow` it may emit rides a redacting rewrite, never a grant.

Verdict mapping differs by provider surface: Claude Code has an HITL prompt so `REQUIRE_APPROVAL` → `ask`; Codex's hook parser rejects `ask`, so it maps to **deny** with the approval reference in the reason (OD-SL7-ASK — a fallthrough under `approval_policy=never` would auto-run ungoverned, so deny is the safe mapping). See `docs/e8-implementation-plan.md` §8 for the deny-and-retry approval design that makes this a real four-eyes control rather than self-approval.

**Assurance caveat:** all of the above is enforced by a *user-local* hook. Until the managed provider config is deployed (E8-S8/S9), a developer can remove the hook or flip the local config, so treat local enforcement as prevention **without** assurance. That is a deployment property, not a code gap.
