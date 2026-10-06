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

## session-jsonl-sample.jsonl

A **scrubbed, synthetic** journal in the envelope shape observed on Muse 1.4.1
(`~/.local/share/muse/sessions/YYYY/MM/DD/<session-id>/session.jsonl`; subagents
under `subagent/<id>/session.jsonl`). Nothing in it is copied from a real
journal: ids are synthetic (`sess-0001`, `call_0001`), text is neutral, times are
fixed. It holds a frame header (no `record_type`), a few unrelated payload kinds
(`session_opened`, a `payload`-kind-less `runtime.user_intent.accepted`, `run`,
`approval`, `reminder_cleanup`, `session_end`), two `tool_batch.effect.started` +
`terminal` pairs (`call_0001` bash, `call_0002` write_file) and one started
record (`call_0003` read_file) that no gate record matches. `call_0001` and
`call_0002` are the `tool_use_id`s of `pre-tool-use-bash.json` and
`pre-tool-use-bash-escalated.json`, which is how the end-to-end test joins them.
`record.call_id` equals the hook's `tool_use_id` (observed).

## Not observed on 1.4.1 (still doc-derived, flagged)

- `pre-tool-use-mcp.json`: no MCP server was configured, so the `mcp__<server>__<tool>`
  name and the tool's `tool_input` are doc-derived. Its common keys follow the capture.
- `stop-failure.json`: no provider error occurred. Only its common keys follow the capture.
- PreCompact, PostCompact, Notification, PostToolBatch, Interrupt: never fired,
  and have no contract type.

## Hygiene

No secret-shaped values: no API keys, bearer tokens, Meta `LLM|...` keys, or
long high-entropy strings. Any secret-shaped fixture must be built in code
(CLAUDE.md, privacy posture).

## Content-reader fixtures

`session-jsonl-content.jsonl` and `session-jsonl-content-drift.jsonl` are
synthetic: the keys and kinds (`model_response_created`,
`assistant_message_committed`, `reasoning_summary_committed`,
`assistant_tool_calls_committed`, `model_completed`, plus the decoys `output`,
`reasoning_committed` and a `model_input_trace_recorded` carrying a nested
`schema_version:2`) follow the shape of a Muse 1.4.1 log observed 2026-10-01; all
text is neutral. The drift file is the same log with envelope `schema_version:2`.
Regenerate by editing both together; the reader tests pin their ids and text.

## Lifecycle hooks, live capture (muse 1.4.2-R4684.1, 2026-10-01)

Captured on **Muse Code 1.4.2** (installed; the plan said 1.4.1) with a logging
handler (stdin to a file, exit 0) registered for the lifecycle events in a
sandbox `XDG_CONFIG_HOME` settings file (Muse honours `XDG_CONFIG_HOME`; the
developer's own settings and OpenBox hooks were not touched), against a
throwaway git workspace, driven headless via `muse exec --json` and through the
TUI on a pty. Scrubbed the same way as the files above (`sess-0001`,
`turn-0001`, `/tmp/proj`); keys, order and value types are the captured ones.

| File | Hook | Observed |
|---|---|---|
| `pre-compact.json` | PreCompact | `trigger` = `soft` (automatic compaction at the soft threshold). Common keys only: no summary, token count or message fields |
| `post-compact.json` | PostCompact | same shape, `trigger` = `soft`; fired once after a compaction that succeeded |
| `notification.json` | Notification | adds `notification_type` (`permission_prompt`), `title`, `message` to the common keys; fired a few seconds after a `PermissionRequest` while the TUI sat waiting for approval |

Behaviours that matter to a mapper:
- PreCompact fired several times in one run and PostCompact once: a compaction
  attempt that fails (`context compaction replacement still exceeds the hard
  threshold`) fires PreCompact with no PostCompact, and internal sessions
  (compaction/observer children, with `session_id == turn_id`) fire PreCompact
  under their own ids that have no session directory. Do not pair them.
- The TUI `/compact` command compacted the context ("Context compacted") but
  fired **neither** PreCompact nor PostCompact. Only automatic compaction
  (forced with `--context-compaction-soft-threshold 0.043 --context-compaction-hard-threshold 0.08`)
  fires them. `trigger` = `hard` was not observed.
- Notification did not fire when the TUI blocked on a user-question tool for
  90 s, nor for `PermissionRequest` alone; only the approval wait produced it.
  Only `permission_prompt` was observed.
- `Interrupt` is rejected by the settings loader unless the handler declares
  `async: true` (`UnsupportedHandler`); it stays uninstalled.

### PostToolBatch: N/A, never fired

No `PostToolBatch` payload on 1.4.2 across a parallel `read_file` pair (two
PreToolUse a few ms apart) in `exec` and in the TUI, three multi-tool runs in
all, with PostToolBatch registered and accepted by the loader (11 of 12 hooks
runnable). The binary's own telemetry docs say `StopFailure` and `PostToolBatch`
"are staged schema values and remain production-dark". No fixture.
