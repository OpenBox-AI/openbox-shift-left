# OpenBox Muse Code adapter

The third realization of the Provider Adapter Contract (see
[architecture.md](../../../docs/architecture.md)): it maps Meta's Muse Code CLI
(`muse`) hooks onto the normalized developer event contract and gates four of
them through `/evaluate`. Muse's hook stdin is Claude Code-shaped, so the mapper
follows the Claude Code and Codex adapters by pattern. Nothing is imported from
either: `internal/depguard` forbids adapter-to-adapter imports, and shared code
lives in `adapters/common/hookflow`.

**Status: read off Muse 1.4.1 captures, partly unverified.** The payload shapes
and the fixtures in `testdata/` come from scrubbed real payloads of a Muse 1.4.1
session (`testdata/README.md` says which keys were observed). What is still
guessed is listed under [Unverified](#unverified-and-what-would-settle-it); the
answer shapes Muse accepts (the `onFailure` JSON, what a `PreLLMCall` block looks
like) have not been exercised against a binary that then refused one.

**The installer** (`installer.go`) merges OpenBox's handlers into
`~/.config/muse/settings.json`; see [Installing](#installing) below. `openbox
init --provider muse` refuses a Muse older than 1.4.0 before it registers
anything.

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

## Coverage limit: detection, not prevention

OpenBox cannot stop a tool action whose hook payload is over 256 KiB: Muse skips
the hook entirely, so neither the gate nor its `onFailure` successor runs. What
it can do is notice. At `Stop` and `SessionEnd` the reconciler reads the
session's own append-only `session.jsonl` (and each subagent's) and joins its
`tool_batch.effect.started` envelopes against a content-free ledger of the tool
calls the gate was asked about (`reconcile.go`, `sessionlog.go`). Each intent
with no gate record becomes a local `evidence.gap` trace finding, and `openbox
doctor` shows the count. Nothing is blocked, latched or sent to core.

The format is observed on Muse 1.4.1. A line is a frame header or retained
marker with no `record_type` (skipped), or an envelope
(`schema_version`, `recorded_at` in unix microseconds, `record_type`,
`payload_type`, `payload`). Envelopes of every other `payload_type` and
`payload.kind` are normal and skipped. The join is on the `started` record's
`payload.record.call_id`, which equals the hook's `tool_use_id`, so it is exact;
only when a started record carries no `call_id` does the join fall back to (tool
name, ordinal), which finds how many calls went ungated but not always which.
The matching `tool_batch.effect.terminal` is not read. Only the join fields
(`schema_version`, `record_type`, `payload_type`, `recorded_at`, `payload.kind`,
`payload.record.{kind,call_id,tool_name}`) are decoded; a line's content is never
copied or logged.

The reader stops and doctor says `unverified` only when the format looks
changed: a line that is not JSON, an envelope with no `payload_type`, a
`schema_version` other than 1, a started record missing its tool name or time, or
a pass of eight or more lines none of which is an envelope.

## Event mapping

| Muse hook | Contract type | Gated | Answer |
|---|---|---|---|
| `SessionStart` | `SessionStarted`; `source` `resume` opens a new run (continue-as-new) | no | none |
| `UserPromptSubmit` | `PromptSubmitted` | yes | `{"decision":"block","reason":...}` |
| `PreToolUse` | `ToolCall` (started) | yes | `hookSpecificOutput{permissionDecision:"deny",...}`, or `updatedInput` alone |
| `PermissionRequest` | `PermissionRequest` (a record, never the gate) | yes | `hookSpecificOutput.decision{behavior:"deny",message}` |
| `PostToolUse` / `PostToolUseFailure` | `ToolResult` completed / failed | no | none |
| `SubagentStart` | `SessionStarted` of the child's own session (`agent_id` = `subagent_id`, `agent_type` = `subagent`) | no | none |
| `StopFailure` | `APIError` (no error text is bound) | no | none |
| `SessionEnd` | `SessionEnded`, then an inline spool drain | no | none |
| `PreLLMCall` | `ModelCallRequested` (started, evaluated) | yes | `{"decision":"block","reason":...}` |
| `PostLLMCall` | `ModelCallFinished` (completed, metadata only) | no | none |
| `Stop` | nothing reported: no usage is taken from a hook payload, and no completion is fabricated. Runs the session-log reconciler (see below) | no | none |
| `SubagentStop` | `SessionEnded` of the child's own session, then an inline spool drain | no | none |
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
and the turn continues as if the hook never ran, unless the handler has an
`onFailure` successor. Observed on Muse 1.4.1 with a `PreToolUse` handler that
carries one:

| The hook | Result |
|---|---|
| answers with one of the four refusals below | accepted; the call is blocked |
| exits non-zero (crash) | successor runs; denied |
| prints JSON of a shape Muse does not know | successor runs; denied |
| runs past its `timeout` | successor runs; denied |
| prints plain non-JSON text and exits 0 | **allowed**: Muse reads it as text, and no successor runs |

The last row is why every answer here is JSON built from a closed struct, and why
a crash never writes to stdout. Two layers close the rest.

1. Every gated code path ends in a schema-valid answer or a non-zero exit.
   `RunHook` does **not** recover a panic on a gated event; it reaches
   `openbox hook`'s recover, which exits with `Engine.FaultExitCode` (2) and
   writes only to stderr, so the successor denies. A panic on any other event
   gates nothing and is logged and swallowed. `fault_test.go` and
   `cmd/openbox/musefault_test.go` pin this.
2. The installer registers a deny-only `onFailure` successor on every gated
   handler, which also covers a timeout and an answer Muse rejects. The
   successor is `openbox hook muse --fail-closed <Event>` (`failclosed.go`): it
   reads no config, identity or network, writes nothing under the OpenBox home
   (`cmd/openbox` routes it before any provider store is bound), and exits 2 with
   one stderr line for a gated event. `failclosed_test.go` keeps the exit-0
   refusal shapes as a golden, unused: they are what a successor would write if
   exit 2 turned out not to deny on some release.

`OPENBOX_HOME` does not reach the hook (Muse clears its environment), so the
installer bakes `--home "<abs dir>"` into every handler and successor whenever
`OPENBOX_HOME` is set to a non-default home (`BakedHome`); `openbox hook` sets it
before binding the provider. Without it the hook would bind the default home and
govern nothing.

## Installing

- **Which events.** One catch-all handler (no matcher, so MCP tools are covered)
  per event the adapter produces something for, derived from the adapter's own
  table (`HookName.Observed`): thirteen today (`Stop` triggers the session-log
  reconciliation and `SubagentStop` closes a subagent's session); the five events
  with no contract type never get one. `ExpectedHandlers()` is what doctor counts
  against.
- **Shape.** `{"type":"command","command":"\"<engine>\" hook muse [--home \"<dir>\"] <Event>","timeout":N}`,
  with `N` 30 on a gated event and 5 elsewhere (`SessionEnd` included: it delivers inline within that ceiling), and on a gated
  handler an `onFailure` object of the same form holding the `--fail-closed`
  command. The engine is an absolute path.
- **Merge, not rewrite.** The file is edited by path, so every key and handler
  that is not OpenBox's keeps its bytes. A handler is OpenBox's only when its
  command parses to `<engine> hook muse ...` (`parseInvocation`); one that merely
  mentions it is foreign and survives install and uninstall. A second install is
  byte-identical. `schema_version: 1` is written into a new file and left alone in
  an existing one.
- **Refuse, don't repair.** A file Muse cannot read (not JSON, a wrong type, a
  `schema_version` other than 1) is refused with the file untouched, because Muse
  drops every hook in such a source. Keys this adapter does not know are warnings,
  since a newer Muse may accept them. `ValidateSettings` is shared by install and
  doctor.
- **Atomic, then read back.** The write goes through `hookflow.AtomicWriteFile`
  (through a symlink if settings.json is one, keeping its mode), and what landed on
  disk is re-read and checked for the exact handler set; on a mismatch the
  previous file is restored.
- **Uninstall** (`RemoveHooks`) removes exactly the owned handlers, and the event
  or `hooks` object it emptied, so install then uninstall gives back the original
  bytes. `schema_version` stays.
- **Version gate.** `muse --version` (2 s bound) below 1.4.0 refuses, as does a
  version that cannot be read (timeout, non-zero exit, no number: nothing proves
  it is new enough) and a prerelease of 1.4.0; absent from PATH, or at 1.5.0 and
  above, installs with a warning. Doctor rates an unreadable version FAIL.
  Every `muse` subprocess goes through a `Runner`, so tests never run a real one.
- **Posture** goes through `devconfig.WriteConfig(ConfigUpdate(ref))` like the
  other adapters; a bool the run says nothing about stays unset.

An unconfigured machine (no identity) is governance inactive, not a fault: the
hook logs and exits 0, as the other adapters do. A delivery failure denies only
its own call and never latches the run; only a real HALT verdict latches.

## Run lifecycle

Muse gives no reliable lifecycle to hang a run on, so `runlifecycle.go` keeps one
small record per session id (under the spool's `lifecycle/`, locked per session)
saying which run the session is on and whether `SessionEnd` or `SubagentStop`
sealed it:

- **`muse resume` fires no `SessionStart`.** It sends `UserPromptSubmit` and the
  rest under the old session id after that session already got `SessionEnd`. The
  first event of a session whose run was sealed opens a new run (continue-as-new,
  the same as a Claude Code `SessionStart` `source=resume`) and its
  `SessionStarted` is spooled before the event. The halt latch is keyed by run,
  so the resumed session starts unlatched.
- **A subagent runs under its own session id** (`SubagentStart`/`SubagentStop`
  carry `session_id == child_session_id == turn_id` and a `subagent_id`, never a
  parent id), so it is a session of its own and cannot be linked to its parent.
  Any event of a session id with no known run opens it first. Nothing is skipped
  by `subagent_id`: a subagent's tool and model calls are gated like any other.

## Unverified, and what would settle it

Observed on Muse 1.4.1 and no longer guesses: native tool names (`bash`,
`read_file`, `write_file`, `edit_file`, ...) and their `tool_input` keys; the
`SessionStart` `source` key (`startup`, `clear`; a `clear` carries a new session
id); subagent sessions under their own ids; resume firing no `SessionStart`; the
load probe printing `Hooks: N runnable · M warnings` only when at least one
warning exists (a clean load prints nothing, and `--json` has no count), so
doctor believes a silent probe only once a muse `hook.in` record appears in the
local trace after the probe began; `muse config status` printing
`plane=... source_class=... state=...` source lines (and `muse config validate`
needing `--plane <defaults|policy> --file <path>`, so doctor does not call it);
a Muse tool call's shell carrying `MUSE_TOOL_USE_ID`, `MUSE_RELEASE_INFO` and
`CLAUDE_CODE_TOOL_USE_ID` but not `CLAUDECODE`, `CLAUDE_CODE_ENTRYPOINT` or any
session id, so a Muse commit keeps its trailer and emits no CommitCreated
(`cmd/openbox/attest.go`); Muse refusing a synchronous `Interrupt` handler (it
must be `async: true`, which is one more reason the installer registers none); an
empty stdin on a gated event (seen on `PreLLMCall` during `muse exec` teardown)
being refused, never latched. Also observed: the four refusal shapes (the
`PreToolUse` and `PermissionRequest` `hookSpecificOutput` denies, and
`{"decision":"block","reason":...}` on `UserPromptSubmit` and `PreLLMCall`) are
accepted and block; the `onFailure` key as a nested handler object
(`{"type","command","timeout"}`) runs on a crash, an unknown JSON shape and a
timeout, and our `--fail-closed` successor then denies; plain non-JSON output is
allowed with no successor (see the table above).

Still guessed by the installer and doctor (Muse documents none of them by name):

- The closed handler key set `type, command, timeout, onFailure, async`, that
  `timeout` is whole seconds, and that a missing `schema_version` is tolerated.
- That `command` may quote its executable (`"/path with space/openbox" hook ...`)
  and be split the way a shell would; the Claude Code and Codex installers rely on
  the same.
- `muse --version` printing a `major.minor.patch` token.
- Whether the echo probe (`muse exec --provider echo --trust-workspace "..."`)
  makes no model call, and is a governed session whose hooks run and report.
- What a present policy source's `state` reads (only `absent` was observed), and
  how a policy would spell "managed hook lane required" (`doctormuse.go` matches
  a pattern and otherwise says `unknown`).
- The local-tracing hook log path and its `hook.execution.terminal` records
  (one third-party source); doctor says `unverified` whenever it is absent.
- The value format of `execution.approval_modes`, left out of
  `deployments/managed/muse/policy.json`. The file itself validates on muse
  1.4.1, with every member active (top-level sections, and
  `extensions.hooks.allowed_sources: ["managed"]` as the managed-lane rule).

- Whether an MCP call fires `PreToolUse` (catch-all install is the plan).
- Whether `updatedInput` without a `permissionDecision` is accepted on
  `PreToolUse`; if not, the answer is discarded and the write proceeds unredacted.
- `PostLLMCall`'s `status` and `error` value sets.

- Whether a denied call writes a `tool_batch.effect.started` record at all (a
  call the gate denies may never reach the batch), and whether every Muse
  version keeps `call_id` equal to the hook's `tool_use_id`.
- A started record over the 8 MiB line cap whose `call_id` or `tool_name` comes
  after the cap cannot be joined; it is counted as oversize and skipped.

## Files

| File | What |
|---|---|
| `hookevent.go` | hook names, the tolerant stdin decoder |
| `mapper.go` | event mapping, tool classification, run identity |
| `llmgate.go` | the model-call gate: mapping, request id, target |
| `outputcontract.go` | the four closed answers, the caps |
| `promptgate.go`, `permissiongate.go`, `enforcetarget.go`, `enforce.go`, `enforceevaluate.go` | gate targets and the evaluator |
| `hookrun.go` | `RunHook`: the gated and observed paths, the inline drain |
| `runlifecycle.go` | the per-session run record: resume and subagent sessions open a run before their first event |
| `usage.go` | usage and trace context, local trace only |
| `sessionlog.go`, `reconcile.go` | the session-journal reader with its cursor, the gate ledger, the join and the `evidence.gap` findings |
| `engine.go` | `Engine`, `FaultExitCode`, the ceilings (Gating 30s, Other 5s) |
| `capabilities.go`, `posture.go`, `creds.go`, `adapter.go`, `paths.go` | profile, posture, credentials, spool |
| `installer.go`, `hookremove.go`, `audit.go`, `invocation.go`, `version.go`, `runner.go` | install, uninstall, settings audit, the invocation parser, the version gate and the `muse` subprocess seam |
| `failclosed.go` | the deny-only `onFailure` successor |
