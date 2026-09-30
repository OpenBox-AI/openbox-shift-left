# Muse Code hook fixtures

**Every file here is doc-derived, unverified on binary.** No Muse binary was
available when they were written (2026-09-30, documentary spike; see
`plans/260930-1107-muse-code-integration/reports/spike-findings.md`). Replace
each with a scrubbed capture from a real Muse 1.4.0 session before phase 3
treats it as ground truth.

Sources (shorthand used below):
- **HE** = Muse Code SDK, Plugins > Reference > Hook events
  (`meta-models.github.io/muse-code-sdk/next/guides/plugins/reference/hook-events/`), verified by Meta on 1.3.0.
- **HG** = Muse Code SDK, Extend > Hooks guide (sources, env allowlist, caps).
- **CL** = dev.meta.ai Muse Code changelog (1.4.0 entries).
- **S2** = third-party SigNoz "Meta Muse Code Monitoring" (`signoz.io/docs/muse-code-monitoring/`), not corroborated by Meta.
- **MC** = dev.meta.ai Muse Code overview (default model, auth).

Common keys on every file, from HE "common stdin": `hook_event_name`,
`session_id`, `turn_id` (HE: absent on SessionStart/SessionEnd), `cwd`,
`transcript_path`, `model`, `permission_mode`, `model_provider`. Values are
placeholders: `sess-0001`, `turn-0001`, `toolu-000N`, `/tmp/proj`,
`example.test`. The `model` value `muse-spark-1.3` comes from the Meta API docs
(MC); `model_provider` value `meta` and `permission_mode` value `default` are
guessed values (keys documented).

## Files

| File | Derived from | Guessed keys / values |
|---|---|---|
| `session-start-startup.json` | HE SessionStart + common keys | `source` key and value `startup` (CC-shaped; HE does not list `source`) |
| `session-start-resume.json` | same | `source: "resume"` (guessed) |
| `session-start-clear.json` | same | `source: "clear"` (guessed; whether `/clear` fires SessionStart at all is unverified) |
| `user-prompt-submit.json` | HE UserPromptSubmit | `prompt` key (CC-shaped; HE summary lists no per-event key) |
| `pre-tool-use-bash.json` | HE PreToolUse (`tool_name`, `tool_input`, `tool_use_id`); HE matcher aliases (`Bash`) | `tool_name` delivered as the CC alias `Bash` rather than Muse's native name (native names undocumented); `tool_input.command` |
| `pre-tool-use-read.json` | same (`Read` alias) | `tool_input.file_path` |
| `pre-tool-use-write.json` | same (`Write` alias); risk report H2 names native `write_file` | alias vs native `write_file`; `tool_input.file_path`, `tool_input.content` |
| `pre-tool-use-edit.json` | same (`Edit` alias); native `edit_file` named in H2 | alias vs native `edit_file`; `old_string`, `new_string` |
| `pre-tool-use-mcp.json` | HE/tool docs: MCP tools registered as `mcp__<server>__<tool>` | `tool_input` shape is the MCP tool's own schema (placeholder `query`); whether MCP fires PreToolUse is gate G1 |
| `permission-request.json` | HE PermissionRequest (`tool_name`, `tool_input`, **no** `tool_use_id`) | `tool_input.command` |
| `post-tool-use.json` | HE PostToolUse (`tool_response`) | `tool_response.output` inner key |
| `post-tool-use-failure.json` | HE event list (PostToolUseFailure) | `error` key and string shape (CC-shaped) |
| `subagent-start.json` | HE SubagentStart (`subagent_id`, `child_session_id` only; no child tool set) | none beyond placeholders |
| `subagent-stop.json` | HE event list (SubagentStop) | `subagent_id`, `child_session_id` assumed to mirror SubagentStart |
| `stop.json` | HE event list (Stop) | no per-event keys included (CC's `stop_hook_active` omitted, undocumented) |
| `stop-failure.json` | HE event list (StopFailure, 1.3.0+) | no per-event keys included (error detail undocumented, omitted) |
| `session-end.json` | HE event list (SessionEnd) | no `reason` key (undocumented, omitted); `turn_id` absent per HE |
| `pre-llm-call.json` | HE PreLLMCall (`provider`, `request_id`, `attempt`, `step`, `messages`, `message_count`, `tools`, `tool_count`, `options`); HE "256-char text previews, tool summaries"; S2 for `options.meta.traceparent`, `options.meta.reasoning.effort` | inner shape of `messages[]` (`role`, `text_preview`) and `tools[]` (`name`); `options.meta.*` (S2 only); traceparent value is a synthetic low-entropy W3C id |
| `post-llm-call.json` | HE PostLLMCall adds `status`, `response_id`, `usage`, `finish_reason`, `error`, `output_text_preview`, `tool_call_count`; S2 for usage inner keys | `usage.{input_tokens,output_tokens,cache_read_tokens,reasoning_tokens}` (S2 only); `status: "completed"` and `finish_reason: "tool_calls"` values; assumes PostLLMCall repeats PreLLMCall's keys; `error` omitted on success |
| `session-jsonl-sample.jsonl` | Risk report A1/T2: `~/.local/share/muse/sessions/YYYY/MM/DD/<id>/session.jsonl`, append-only, journals model calls, tool runs, approvals, and records each action's authorisation as `side_effect_intent` before it runs | Only the `side_effect_intent` record type name is documented. Guessed: every field name, `type` discriminator key, `model_call` and `approval_decision` type names, `seq`, `timestamp`. **`tool_use_id` is undocumented, so it is omitted and a `seq` field stands in** (gate G8: loose join on session, tool name, sequence) |

## Not included (and why)

- **Patch tool**: no Muse patch/`apply_patch` tool is documented; add one once a capture shows its name and `tool_input` keys.
- **PostToolBatch, PreCompact, PostCompact, Notification, Interrupt**: not mapped by phase 3 (no contract type).

## Hygiene

No secret-shaped values: no API keys, bearer tokens, Meta `LLM|...` keys, or
long high-entropy strings. Any secret-shaped fixture must be built in code
(CLAUDE.md, privacy posture).
