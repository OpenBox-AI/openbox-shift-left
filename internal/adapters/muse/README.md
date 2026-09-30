# OpenBox Muse Code adapter

The third realization of the Provider Adapter Contract (see
[architecture.md](../../../docs/architecture.md)): it maps Meta's Muse Code CLI
(`muse`) hooks onto the normalized developer event contract and gates four of
them through `/evaluate`. Muse's hook stdin is Claude Code-shaped, so the mapper
follows the Claude Code and Codex adapters by pattern. Nothing is imported from
either: `internal/depguard` forbids adapter-to-adapter imports, and shared code
lives in `adapters/common/hookflow`.

**Status: doc-derived, unverified on a Muse binary.** Every payload shape and
answer shape here comes from Muse's published hook documentation (Muse Code SDK
hook-events reference, verified by Meta on 1.3.0, and the 1.4.0 changelog). No
Muse binary was available when this was written, so none of it was measured on a
live session. The fixtures in `testdata/` are doc-derived and `testdata/README.md`
lists every guessed key. Replace them with scrubbed captures from a real session
before treating any of it as ground truth.

**The installer is a later phase.** `installer.go` still refuses, so no machine
runs these hooks yet. This package is the runtime half: the handlers, the output
contract, the fault exit code. The installer will register them and the
`onFailure` successor described below.

**Hooks are a guardrail, not a tamper-proof boundary.** A developer can edit
`settings.json`, a malformed hooks file silently drops every handler from that
source, a payload over 256 KiB is never delivered to any hook, and the hooks run
as the developer. `--yolo` and Muse's approval judge do not bypass them (hooks
run outside the approval layer), but nothing here resists the developer.

```
Muse hook (stdin JSON)
   └─ openbox hook muse [--home <abs dir>] <event>
        ├─ observe (non-gated)  map → spool → exit 0, no stdout
        └─ gated                latch check → /evaluate → ApplyFailurePolicy →
                                approval hold → latch on a real HALT →
                                the event's own closed answer (outputcontract.go)
   crash on a gated event → exit 2 (FaultExitCode) → Muse's onFailure successor denies
```

## Event mapping

| Muse hook | Contract type | Gated | Answer |
|---|---|---|---|
| `SessionStart` | `SessionStarted`; `source` `resume` or `clear` opens a new run (continue-as-new) | no | none |
| `UserPromptSubmit` | `PromptSubmitted` | yes | `{"decision":"block","reason":...}` |
| `PreToolUse` | `ToolCall` (started) | yes | `hookSpecificOutput{permissionDecision:"deny",...}`, or `updatedInput` alone |
| `PermissionRequest` | `PermissionRequest` (a record, never the gate) | yes | `hookSpecificOutput.decision{behavior:"deny",message}` |
| `PostToolUse` / `PostToolUseFailure` | `ToolResult` completed / failed | no | none |
| `SubagentStart` | `SubagentStarted` | no | none |
| `StopFailure` | `APIError` (no error text is bound) | no | none |
| `SessionEnd` | `SessionEnded`, then an inline spool drain | no | none |
| `PreLLMCall` | `ModelCallRequested` (started, evaluated) | yes | `{"decision":"block","reason":...}` |
| `PostLLMCall` | `ModelCallFinished` (completed, metadata only) | no | none |
| `Stop`, `SubagentStop` | nothing: no usage is taken from a hook payload, and no completion is fabricated | - | - |
| `PreCompact`, `PostCompact`, `Notification`, `PostToolBatch`, `Interrupt` | nothing: no contract type. Never installed; one that runs anyway is a no-op | - | - |

## The model-call gate

Every `PreLLMCall` is evaluated before the request is sent, on the same gating
ceiling and through the same gate as a tool call (`llmgate.go`). It is a
**gate activity of its own** (`activity_type: model_call_gate`), not a model-call
record:

- the activity id is `<session>:llmgate:<request_id>.<attempt>`, a namespace
  disjoint from tool ids and every lane's; an id the contract cannot carry (empty,
  over 128 characters, non-ASCII) is replaced by a stable hash of the keys both
  halves share;
- it carries no usage, no turn index and no lane request id, so core cannot read
  it as an `llm_completion`;
- the started row carries `provider`, `message_count`, `tool_count`, `tool_names`
  and, under `content_capture` and after redaction, `message_previews`;
- `PostLLMCall` closes it with `status`, `finish_reason`, `response_id`,
  `error_class` (a class token, never error text), `tool_call_count`, `model`;
- a denied call, or one that never reports finished, leaves the **started row
  only**.
- the completed row carries no duration: the engine's start-time threading
  pairs tool calls only, so a gate's latency is not on the wire.

`PostLLMCall`'s `usage` and `PreLLMCall`'s `options.meta.traceparent` go to the
**local trace only** (`usage.go`, stage `capture.outcome`), through an integer
allowlist and a W3C format check. They never reach a DevEvent or the wire.

## Output contract

Muse discards an answer it does not accept, and a discarded answer is an allow.
So each gated event has a closed answer shape built from structs that cannot
spell a key Muse rejects, pinned by `outputcontract_golden_test.go`:

- a refusal is a plain deny or block. `continue` and `stopReason` are rejected on
  `PreToolUse` and `PermissionRequest`, so **HALT renders the same plain refusal**;
  the run-keyed latch (written because the contract reports the halt back to the
  gate) denies every later gated call of that run, model calls included, locally;
- `allow` is never written, on any event; a proceed is silence, or `updatedInput`
  alone on `PreToolUse` (a redacted write body);
- `REQUIRE_APPROVAL` is held for a real decision and renders a refusal if
  unanswered, never an ask (`ApprovalDecision()` is `deny` on every contract);
- a reason is cut to 4 KiB and a findings `systemMessage` to 1000 bytes (bytes
  counted, cut on a rune boundary), and the whole answer is held under Muse's
  16 KiB output cap by shrinking the reason, since JSON escaping can multiply a
  byte six-fold;
- a payload that cannot be parsed on a gated event is answered with the event's
  refusal rather than silence.

Findings (opt-in) are surfaced on `PostToolUse` only, never on a gated event: an
answer Muse discards there is read as a failure and denied.

## Fail-open runtime, backstopped by the onFailure successor

Muse fails open: a hook that crashes, times out or answers invalidly is recorded
and the turn continues as if the hook never ran. Two layers close that.

1. Every gated code path ends in a schema-valid answer or a non-zero exit.
   `RunHook` does **not** recover a panic on a gated event; it reaches
   `openbox hook`'s recover, which exits with `Engine.FaultExitCode` (2). Muse
   reads exit 2 as a block, and any other non-zero as a failed hook that starts
   the `onFailure` successor, so 2 denies on both readings (documented for
   PreToolUse; assumed, not yet observed, for PermissionRequest and PreLLMCall). A panic on any other
   event gates nothing and is logged and swallowed. `fault_test.go` and
   `cmd/openbox/musefault_test.go` pin this.
2. The installer (a later phase) will register a deny-only `onFailure` successor
   on every gated handler, which also covers a timeout and an answer Muse rejects.

`OPENBOX_HOME` does not reach the hook (Muse clears its environment), so the
installer bakes `--home <abs dir>` into the command for a non-default home;
`openbox hook` sets it before binding the provider.

An unconfigured machine (no identity) is governance inactive, not a fault: the
hook logs and exits 0, as the other adapters do. A delivery failure denies only
its own call and never latches the run; only a real HALT verdict latches.

## Unverified, and what would settle it

- Whether an MCP call fires `PreToolUse` (catch-all install is the plan).
- The exact `onFailure` JSON, whether invalid output triggers it on 1.4.0.
- Whether `PreLLMCall` accepts `{"decision":"block","reason":...}`; the docs say
  it can block but name no keys. If it does not, the block is discarded and the
  call is sent.
- Whether `updatedInput` without a `permissionDecision` is accepted on
  `PreToolUse`; if not, the answer is discarded and the write proceeds unredacted.
- Whether `/clear` keeps the session id (either way the new run is unlatched).
- Native tool names beyond `read_file`, `write_file` and `edit_file`, and every
  `tool_input` key. `builtinTools` and `contentFieldKeys` list the plausible
  ones, and a fixture tool missing from the table fails a test.
- `PostLLMCall`'s `status` and `error` value sets, and the `usage` inner keys.
- Muse's shell-tool environment, so no commit is attributed to a Muse session.

## Files

| File | What |
|---|---|
| `hookevent.go` | hook names, the tolerant stdin decoder |
| `mapper.go` | event mapping, tool classification, run identity |
| `llmgate.go` | the model-call gate: mapping, request id, target |
| `outputcontract.go` | the four closed answers, the caps |
| `promptgate.go`, `permissiongate.go`, `enforcetarget.go`, `enforce.go`, `enforceevaluate.go` | gate targets and the evaluator |
| `hookrun.go` | `RunHook`: the gated and observed paths, the inline drain |
| `usage.go` | usage and trace context, local trace only |
| `engine.go` | `Engine`, `FaultExitCode`, the ceilings (Gating 30s, Other 5s) |
| `capabilities.go`, `posture.go`, `creds.go`, `adapter.go`, `paths.go` | profile, posture, credentials, spool |
| `installer.go` | refuses until the installer phase |
