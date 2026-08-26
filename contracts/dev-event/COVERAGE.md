# Provider coverage & mapping rules

How the three target coding tools' real event surfaces map onto this contract's 12 lifecycle
types, plus the **bounded non-goals**. Sourced from a G3_REVIEW pass over the tools' official
docs (Claude Code hooks + OTel; Cursor hooks + Admin API; OpenAI Codex hooks + `notify` + Usage/Cost API).
This is the reference an adapter author (SL-4 Claude Code, SL-7 Codex, SL-8 Cursor) implements `emit()` against.

**Adapter status (keep this line current — report SL-07 was a stale-doc finding):** Claude Code
and Codex adapters are **shipped**, observe + default-on enforce + default-on usage capture; each adapter's
`Capabilities()` is the authoritative per-provider profile and this document must agree with it.
The **Cursor** column below is a *surface survey*, not shipped support — SL-8 is unbuilt. Last
reconciled 2026-08-26 (ADR-0021) against `adapters/claude-code/capabilities.go` and
`adapters/codex/capabilities.go`.

**One producer is not an adapter at all.** The local gateway
([ADR-0021](../../docs/adr/ADR-0021-openbox-local-gateway.md), v1.5) emits a
`TurnCompleted` for each relayed model call without going through any provider adapter
or hook — it observes the HTTP exchange itself. It is Claude Code only, opt-in per
machine, and nothing in this document's matrix describes it: a lifecycle matrix maps
hooks, and the gateway has none. Its fields are in
[MAPPING.md](MAPPING.md) §3 and its own §7 verification items.

**Narrower than "Claude Code": the terminal CLI only.** Measured 2026-08-27 — a CLI
session relayed and was captured; a desktop-app session over the same install
produced nothing, because the desktop app routes through its own third-party
inference configuration rather than `ANTHROPIC_BASE_URL`
([ADR-0021 §8](../../docs/adr/ADR-0021-openbox-local-gateway.md)). State it rather
than averaging it: two developers on one org and one posture, one working in the
terminal and one in the desktop app, send *entirely different* model-call evidence —
the first sends whole request and response bodies, the second sends none and nothing
reports the gap.

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
| `SubagentStarted` *(v1.2)* | `SubagentStart` hook | *(unsurveyed)* | **none** |
| `PermissionDenied` *(v1.2)* | `PermissionDenied` hook — **auto-mode classifier denials only**; a static `permissions.deny` rule denies without firing it (verified), so absence is not evidence that nothing was denied | `permissionRequest`? *(unsurveyed)* | **none** |
| `APIError` *(v1.2)* | `StopFailure` hook | *(unsurveyed)* | **none** |

## 2. Field-derivation rules

- **`tool.kind`**: file tools (Read/Edit/Write, `beforeReadFile`, `afterFileEdit`, Codex `apply_patch`) → `file`; shell/Bash (`beforeShellExecution`, Codex `Bash`) → `shell`; MCP (`beforeMCPExecution`, `mcp__*`) → `mcp`.
- **`tool.mcp_server`**: Claude Code/Codex — parse from `tool_name` `^mcp__([^_]+)__`. Cursor — from the hook's `url`/`command`.
- **`span.file_path`**: Claude Code — `tool_input.file_path` (nested, not root). Cursor — `file_path` on file hooks. Codex — from `apply_patch` input.
- **`span.lines_count`/`bytes_*`**: derive from `edits[]` (Cursor) / tool response; often only available `PostToolUse`/`ToolResult`.
- **`tokens`/`model`**: gated by `ResolveFinops` (**default on** since ADR-0014, opt out with `finops:false` / `OPENBOX_FINOPS=0`), off the hot path. **Claude Code is per turn**: `Stop`/`SubagentStop` → a `TurnStarted`/`TurnCompleted` pair with `activity_type: llm_completion` carrying all four counts plus the model id, plus the retained `SessionEnded` rollup. **Codex is per session**: one rollup pair at `SessionEnd` (`activity_id <session>:usage:rollup`) — its `Stop` hook exists but is deliberately unwired, which is scope, not impossibility. Both read a local file (CC's transcript, Codex's rollout JSONL), never the providers' OTel/Usage APIs, through an allowlist projection whose one egressing string is the model id (INV-2, ADR-0014). `cost` is never **derived** here — the server derives it from a model-keyed pricing table — and the turn pair never carries it at all. The only way a `cost` can appear is if a transcript itself supplies one: CC's reader still reads `costUSD` onto the `SessionEnded` rollup, and current Claude Code transcripts do not carry that field (empirical, not structural). Codex's token path has no cost field at all. Cursor (unbuilt) has no per-turn source known: its Admin API is per-user hourly/daily, so a future adapter rolls up at agent/day granularity. `tokens?`/`model?` stay optional for exactly this reason.
- **`status`** (v1.2, ADR-0018): derived **structurally** — from which hook fired, never parsed from tool output. **Claude Code**: `PostToolUse` → `completed`, `PostToolUseFailure` → `failed`. The two are mutually exclusive per call (documented by the provider, verified empirically on 2.1.229), which is what makes an unconditional `completed` on the success hook truthful. `metadata.is_interrupt` (tri-state `*bool`) separates a user cancellation from a real tool failure — both are `failed`. **Codex**: NOT reported. One `PostToolUse`, no failure hook, no exit code, no error flag; sending `completed` unconditionally would report SUCCESS 100% for a session whose calls failed, which is worse than the honest 0% it replaces. Not content-gated on either provider.
- **Assistant turn text → `spans[0].response_body`** (v1.2, ADR-0018): **Claude Code only**, from the `Stop`/`SubagentStop` payload field `last_assistant_message` — the provider's own recommended source, and the choice that leaves the transcript projection's allowlist untouched. Gate chain, all required: `finops` (turn events exist at all) ∧ the window carried usage ∧ `content_capture` (checked twice — once by the mapper, once independently by the client's `stripContent`). Secret-redacted **before** attachment, then capped at 64KB. With `secret_detection:false` it egresses unredacted. **Codex**: no assistant-text field on its hook surface; its `Stop` is deliberately unwired, and its SessionEnd rollup shares a flush with `WorkflowCompleted`, which DELETES the goal session — wrong granularity and racy ordering. So Codex sessions do not feed Goal Alignment.
- **`error_type`** (v1.2): passed through an `enumOr` allowlist of the provider's own ten values. This is not decoration: `error` is the same JSON key on `StopFailure` (a closed enum) and `PostToolUseFailure` (free text a tool wrote), so one binding decodes both and the allowlist is what keeps the free text off the wire.
- **`ToolCall`↔`ToolResult` correlation**: carry the provider's `tool_use_id` in `metadata` (all three expose it) and set `span.invocation_id` from it — a local field that never egresses and keys the cross-process duration stash. The two halves pair on the wire by a shared `activity_id` (no tool event carries a `span_id` any more; ADR-0013 — the one span that still exists is the turn carrier, ADR-0018). Both shipped adapters correlate by id rather than by heuristic. A new adapter must also supply `span.operation_id` for any class it lets the gate escalate, or an approval cannot survive a retry — `activity_id` derives from it, and an approval granted against one activity cannot be consumed by a retry that addresses another; see MAPPING.md "Operation vs invocation identity".

## 3. Bounded non-goals (Phase-1 v1.0) — documented, not gaps

The contract is honest about what it does **not** model in v1.0. None is required for the Phase-1 goals (observe / finops / session→commit→deploy lineage):

1. **Turn boundaries** (Cursor `stop`, Codex `Stop`) are **not** `SessionEnded` — they fire per agent-loop turn, and a session has many turns. Adapters must **not** map turn-stop → `SessionEnded`. (Historical note: Codex once had no session-end hook and its adapter synthesized one; `SessionEnd` is real as of 0.145.0 and the synthesis is gone.)
2. ~~**Subagent lifecycle**~~ — **retired as a non-goal in v1.2.** `SubagentStart` is wired and maps to `SubagentStarted` → `SignalReceived(subagent_started)` (ADR-0018). The old reasoning — the tree is reconstructable from `agent_id` on tool events — held for a subagent that *does* something; a subagent that spawns and calls no tool left no trace at all. `SubagentStop` stays unwired as a lifecycle marker, because it already has a job: it closes a turn (ADR-0014). Still not a `tool.kind`.
3. **Compaction** (`PreCompact`/`PostCompact`) — context-window infra; dropped in Phase 1.
4. **Assistant message/thought** — **retired as a non-goal: v1.2 for the completion text, v1.3 for tool content, v1.4 for thinking.** The completion TEXT egresses for Claude Code on the turn's span (ADR-0018); **tool output, observe-path tool input, and the free-text failure detail egress as of v1.3** (ADR-0019 P1); **thinking egresses as of v1.4** in `activity_output.thinking` (ADR-0019 P3) — all under the one `content_capture` gate, redacted before attachment and capped at 64KB. Thinking was the item that required amending ADR-0014's transcript allowlist and turning its load-bearing sentinel around; that amendment is written, and the sentinel is mutation-tested against the removal of either the redaction or the cap. What is still out of scope:
   - **intermediate assistant text** from the transcript window. Only the FINAL reply egresses, from the hook field ADR-0018 bound; the amendment authorised `thinking` and nothing else in that array.
   - **`redacted_thinking` blocks** — provider-encrypted base64. Excluded by the block-type filter, deliberately: it would spend the 64KB budget on ciphertext no reader can use.
   - **Cursor and Codex** have no equivalent wired at all. Codex binds no `tool_response` field (`adapters/codex/hookevent.go:73`, pinned by `adapters/codex/hookevent_test.go:50`) and reads no transcript thinking, so closing either for Codex is adapter work, not a contract change. **State the asymmetry rather than averaging it, and note that every version since v1.3 has WIDENED it:** a Claude Code session and a Codex session on the same org, same posture, send different amounts of content — Claude Code sends tool input, tool output, the failure detail and the turn's thinking, and with the opt-in gateway the whole model request and response as well; Codex sends none of them.
   - **The asymmetry is also about redaction, not only volume, and that direction is the dangerous one.** `adapters/claude-code/hookrun.go` wires a redactor onto the mapper as a collaborator, so every content field it attaches is scanned by construction. **The Codex mapper has no such field to wire** — its local redaction exists only on the enforce path, over an `apply_patch` body — so the prompt, the only content class it egresses, is sent **unscanned even with `secret_detection` on**. The Claude Code prompt had exactly this gap until 2026-08-26, and it survived review because that one field was assigned directly instead of through the collaborator; the same shape is still live for Codex.
   A dedicated `CompletionReceived` type was the v1.1 candidate here; it was not built, because the alignment reader that needs the text keys on a span rather than an event type.
5. **Non-session telemetry** (Cursor Tab hooks, `workspaceOpen`, cloud-agent sessions that never emit `sessionStart`) — the contract requires `openbox_session_id`, so events with no resolvable session are **not emittable** (adapter drops or synthesizes a session). Honest degradation — the architecture's "no false coverage" rule.
6. **`PermissionRequest` vs generic `preToolUse`/`PreToolUse` overlap** — adapters emit **one** `ToolCall` per tool invocation (prefer the specific pre-tool hook); `event_id` idempotency (INV-5) also guards double-counting.

Items 1 and 2 were the candidate scope for a `schema_version` bump, and both have since landed — turn boundaries in v1.1 (ADR-0014), subagent start in v1.2 (ADR-0018), tool content in v1.3 (ADR-0019 P1), thinking in v1.4 (ADR-0019 P3). Item 4 is closed for Claude Code; what remains open there is **provider parity**, which is adapter work, not a contract question. The `schema_version` field exists precisely so each bump stays non-breaking; v1.2, v1.3 and v1.4 are all purely additive, and with content capture off all three send byte-identical payloads.

## 4. Enforcement posture

All three tools have blockable hooks (Claude Code fail-controlled; **Cursor fail-open** by default, `failClosed:true` to flip; Codex feature-gated `features.hooks`, stable and on by default ≥ 0.145.0).

Enforcement **shipped** in Phase-2 (E6 for Claude Code, SL7-B for Codex) and is **on by default** as of ADR-0016; `openbox init … --enforce=false` opts out, and an observing session treats every verdict as allow (INV-3). Two bounds come with that default and both must stay true: enforcement is inert until the org publishes a policy, and `fail_closed` stays off. The verdict itself is the server's — since ADR-0017 nothing local decides one. What the hook does in-process (ADR-0006's no-sidecar shape) is apply it, **tighten-only** — it never turns a provider's own deny into an allow, and the one `allow` it may emit rides a redacting rewrite, never a grant.

Verdict mapping differs by provider surface: Claude Code has an HITL prompt so `REQUIRE_APPROVAL` → `ask`; Codex's hook parser rejects `ask`, so it maps to **deny** with the approval reference in the reason (OD-SL7-ASK — a fallthrough under `approval_policy=never` would auto-run ungoverned, so deny is the safe mapping). The deny-and-retry approval design that makes this a real four-eyes control rather than self-approval is described in `docs/architecture.md` § Approvals.

**Assurance caveat:** all of the above is enforced by a *user-local* hook. Until the managed provider config is deployed (E8-S8/S9), a developer can remove the hook or flip the local config, so treat local enforcement as prevention **without** assurance. That is a deployment property, not a code gap.
