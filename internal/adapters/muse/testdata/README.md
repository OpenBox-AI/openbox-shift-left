# Muse Code hook fixtures

**Captured from muse 1.4.1 (1.4.1-R4503.1), scrubbed.** Each hook payload below
is a real payload written to stdin by Muse, kept at its real key names, shapes
and value types. Ids are synthetic (`sess-0001` for the main session,
`sess-0002` for a subagent's own session, `sess-0003` for the session a
`/clear` starts, `turn-0001`, `call_0001`, `resp_0001`), paths are `/tmp/proj`,
and every prompt, file body, tool result and message text is neutral text. The
one real value kept verbatim is the model label `muse-spark-1.3-contributor`.

Shapes worth knowing (all observed):
- Common keys on every payload: `hook_event_name`, `session_id`, `cwd`,
  `transcript_path` (always `null`), `model`, `permission_mode`,
  `model_provider`; `turn_id` on everything but SessionStart/SessionEnd.
- `SessionStart.source` is `startup` or `clear`. A `clear` carries a NEW session
  id. **A resume fires no SessionStart**, so there is no resume fixture.
- Subagents run under their own session id: SubagentStart/SubagentStop carry
  `session_id == child_session_id == turn_id`, plus `subagent_id`, and no parent
  session id. Tool, permission and model-call events inside one carry the child
  session id.
- Tool names are native (`bash`, `read_file`, `write_file`, `edit_file`,
  `search`, `bash_input`, plus internal tools). `PreToolUse`/`PostToolUse` carry
  `tool_use_id` (`call_...`); `PermissionRequest` does not;
  `PostToolUseFailure` adds `error`, `is_interrupt`, `duration_ms`.
- A bash `tool_response`/`error` is a string holding a JSON object; a
  `write_file`/`edit_file` response is an object; a `read_file` response is text.
- `PreLLMCall.messages[].content` is a list of `{"type":"text","text":...}`
  blocks; `options` is a flat map with dotted keys. `PostLLMCall` repeats
  `messages`, `tools` and `options` (which is where `meta.traceparent`,
  `meta.session_id` and `meta.instructions` appear) and adds `status`
  (`success` or `tool_calls`), `usage`, `finish_reason` (may be `null`), `error`
  (`null`), `output_text_preview`, `tool_call_count`.

## Files

| File | What it is |
|---|---|
| `session-start-startup.json`, `session-start-clear.json` | SessionStart, `source` `startup` / `clear` |
| `session-end.json` | SessionEnd, `reason` `other` |
| `user-prompt-submit.json` | UserPromptSubmit |
| `pre-tool-use-bash.json`, `pre-tool-use-bash-escalated.json` | PreToolUse `bash` `{command, description, workdir}`; the second adds `sandbox_permissions` |
| `pre-tool-use-read.json`, `-write.json`, `-edit.json` | PreToolUse `read_file {path}`, `write_file {path, content}`, `edit_file {path, find, replace}` |
| `permission-request.json` | PermissionRequest for Muse's internal `submit_reminder_decision` tool (the only PermissionRequest captured); no `tool_use_id` |
| `post-tool-use.json`, `post-tool-use-write.json` | PostToolUse for `bash` (string response) and `write_file` (object response) |
| `post-tool-use-failure.json` | PostToolUseFailure for `bash` |
| `subagent-start.json`, `subagent-stop.json` | SubagentStart/Stop for the `skill-reminder` observer, under its own session id |
| `stop.json` | Stop (`stop_hook_active`, `last_assistant_message`) |
| `pre-llm-call.json`, `pre-llm-call-subagent.json` | PreLLMCall, main session and a subagent session |
| `post-llm-call.json`, `post-llm-call-tool-calls.json` | PostLLMCall, `status` `success` (null `finish_reason`) and `tool_calls` |

The message lists, tool lists and usage numbers are trimmed or set to round
values; their structure is the captured one.

## Not observed on 1.4.1 (still doc-derived, flagged)

- `pre-tool-use-mcp.json`: no MCP server was configured, so the `mcp__<server>__<tool>`
  name and the tool's `tool_input` are doc-derived. Its common keys follow the capture.
- `stop-failure.json`: no provider error occurred. Only its common keys follow the capture.
- `session-jsonl-sample.jsonl`: Muse's session journal was not captured; every
  field but `side_effect_intent` is a guess. See `sessionlog.go`.
- PreCompact, PostCompact, Notification, PostToolBatch, Interrupt: never fired,
  and have no contract type.

## Hygiene

No secret-shaped values: no API keys, bearer tokens, Meta `LLM|...` keys, or
long high-entropy strings. Any secret-shaped fixture must be built in code
(CLAUDE.md, privacy posture).
