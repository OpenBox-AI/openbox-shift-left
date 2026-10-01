# Provider coverage

How Claude Code, Codex and Muse Code's real hook/telemetry surfaces map onto this
project's normalized lifecycle events, and where each provider falls short of
full coverage. This is the reference an adapter author works against.

**Who this is for.** Someone writing or reviewing an adapter, or checking
whether a governance claim holds for a given tool. For installing the agent,
see [Getting started](getting-started.md); for the overall shape, see
[Architecture](architecture.md); for the event contract and field-level
mapping rules, see [the event contract](dev-event-contract.md) and
[mapping.md](mapping.md).

**Shipped providers.** Claude Code, Codex and Muse Code. Each adapter's
`Capabilities()` — in `internal/adapters/<tool>/capabilities.go` — is the
authoritative per-provider profile; this document must agree with it. No other
provider is implemented.

**Muse Code is partly observed.** Its adapter was built from Meta's published
hook documentation (the hook-events reference and the 1.4.0 changelog), and its
payload shapes were then corrected against scrubbed real captures of a Muse
1.4.1 session, which the fixtures now are. The four refusal shapes and the
`onFailure` successor were then exercised by hand against Muse 1.4.1 (below),
but no automated test runs Muse, so every Muse row in §5 is at most E1 or E2.
`internal/adapters/muse/README.md` lists what is still guessed.

## 1. Lifecycle coverage matrix

| Contract type | Claude Code | Codex | Muse Code |
|---|---|---|---|
| `SessionStarted` | `SessionStart` hook | `SessionStart` hook | `SessionStart` hook; `source` `resume` opens a new run. Muse fires no `SessionStart` on resume, so the first event of such a session opens the run; a subagent is folded into its parent's run |
| `PromptSubmitted` | `UserPromptSubmit` hook | `UserPromptSubmit` hook | `UserPromptSubmit` hook |
| `ToolCall` | `PreToolUse` hook | `PreToolUse` hook | `PreToolUse` hook, catch-all, MCP tools included |
| `ToolResult` | `PostToolUse` hook | `PostToolUse` hook | `PostToolUse` (completed) or `PostToolUseFailure` (failed) |
| `SessionEnded` | `SessionEnd` hook | `SessionEnd` hook | `SessionEnd` hook |
| `SubagentStarted` | `SubagentStart` hook | `SubagentStart` hook | `SubagentStart` hook, in the parent's session; the parent is read off its journal's link record, and a child with no link stays a session of its own |
| `PermissionRequest` | `PermissionRequest` hook; content-gated; no `tool_use_id`, so it never correlates to the tool call it is about | `PermissionRequest` hook; content-gated | `PermissionRequest` hook; evaluated only so it can refuse, never to grant; no `tool_use_id` |
| `PermissionDenied` | `PermissionDenied` hook; only auto-mode classifier denials — a static deny rule or a manual denial never fires it | none | none |
| `APIError` | `StopFailure` hook | none | `StopFailure` hook; no error text is bound |
| `PreCompact` / `PostCompact` | wired; content-gated (the `/compact` instructions and the summary) | wired; content-gated | none: not installed |
| `ModelCallRequested` / `ModelCallFinished` | none | none | `PreLLMCall` / `PostLLMCall` hooks: a model-call **gate** activity (`model_call_gate`), evaluated before the request is sent. Not a record of the call — see §1b |
| `CommitCreated` | `git post-commit` hook, only where the hook is installed, agent commits only — see [mapping.md](mapping.md) | same | none: a Muse commit keeps its trailer but no marker tells the post-commit hook it was Muse's |
| `Deploy` | git-action level, not a hook | same | same |

Claude Code also wires about twenty more hooks with no Codex counterpart
(setup, instructions-loaded, prompt expansion, message display, tool batch,
notification, task created/completed, teammate idle, config change, cwd
changed, directory added, file changed, worktree remove, model switch,
elicitation and its result). Each maps to a `SignalReceived` event carrying no
`activity_id`. See `internal/adapters/claude-code/hookevent.go` for the exact
hook names and [mapping.md](mapping.md) for their field shapes.

A few of those hooks buy less than they look like:

- **`MessageDisplay`**'s `message_id` is not the API's `msg_…` id, so there is
  no join from this event back to a specific assistant message.
- **`Pre`/`PostModelSwitch`** share no id; they are two independent signals,
  never a correlated pair.
- **`FileChanged`** fires only for a fixed basename list
  (`.env`, `.envrc`, `.mcp.json`, `CLAUDE.md`) — never general file coverage,
  and never the file's contents.
- **`WorktreeRemove`** is wired but `WorktreeCreate` is not: that hook's stdout
  is how Claude Code learns the created worktree path, so registering an
  observer there would break `claude --worktree`. This is a deliberate choice,
  not a gap.

Muse Code documents eighteen hook events; the adapter installs a handler for
the twelve that map to a contract type (`SessionStart`, `UserPromptSubmit`,
`PreToolUse`, `PermissionRequest`, `PostToolUse`, `PostToolUseFailure`,
`SubagentStart`, `SubagentStop`, `StopFailure`, `SessionEnd`, `PreLLMCall`,
`PostLLMCall`).
`Stop` runs only the session-log reconciler (§3) and reports nothing. `Stop`'s
turn boundary carries no usage, so Muse sends no per-turn token counts or
reply text. `PreCompact`, `PostCompact`, `Notification`,
`PostToolBatch` and `Interrupt` have no contract type and are not installed;
`Interrupt` in particular never fabricates a completion (and Muse refuses a
synchronous `Interrupt` handler, which would have to be `async: true`).

Codex documents about a dozen hook events in total; the ones not listed above
are not wired. Its own `tool.status` is not reported at all: one `PostToolUse`
and no failure hook means success and failure are not distinguishable on the
wire, and sending `completed` unconditionally would misreport every failed
call as a success — see `internal/adapters/codex/capabilities.go`.

## 1a. `/clear` and `--resume` are different, and only one continues a run

- **`--resume` continues a run.** The session id stays the same across the
  boundary; the client mints a fresh `run_id` and links it back to the sealed
  run via `continued_from_run_id`. Goal alignment carries forward: the
  continued run is judged against the goal last stated in the previous run.
- **`/clear` does not.** It seals the current run and starts a brand-new
  session id with no link back. Goal alignment resets — the new run starts
  with no goal until its first prompt.
- **`startup`, `compact` and `fork` do not continue a run either.** `compact`
  fires no `SessionEnd` at all; `fork` gets a fresh session id over copied
  history, the same non-continuation shape as `clear`.
- **Every `SessionEnd` seals its run, for every reason.** There is no
  suspended-session state and no wire value for one — never describe a
  `SessionEnd` as anything but terminal.

A session `--resume`d repeatedly produces several sealed runs chained under
one session id; a session `/clear`ed repeatedly produces several independent
sessions with no lineage between them. See
[mapping.md](mapping.md#1-envelope-field-mapping-every-event) for the
field-level derivation.

## 1b. Model-call coverage matrix

None of the model-call producers has a hook of its own — this table is what
each *lane* sees of a single model call, a separate axis from §1's hook table.
`openbox init --provider claude-code` installs two lanes for Claude Code:
`transport` (an in-path CONNECT/TLS relay, activity namespace `:proxy:`) and
`telemetry` (a local OTLP receiver, namespace `:otel:`). `openbox init
--provider codex` installs `telemetry` everywhere, and on macOS also the
`transport` relay plus the system PAC that routes Codex to it; on Linux and
Windows Codex stays telemetry-only. `openbox init --provider muse` installs
hooks and the `telemetry` lane (Muse's own export, redirected to the receiver
through its `settings.json`), never `transport`.
A legacy `gateway` lane (`:gateway:`) still exists in the codebase but is no
longer installed by `init`; treat it as retired rather than as active
coverage.

| Tool | Model calls recorded by | Why not more |
|---|---|---|
| Claude Code | `transport` and `telemetry`; one of them emits (below) | none |
| Codex, macOS | `telemetry` until the relay is elected, then `transport` | election needs evidence (below) |
| Codex, Linux and Windows | `telemetry` | no system proxy there |
| Muse Code | `telemetry`: metadata only (model, tokens, ids) | no proxy lane is possible (below) |

**Muse Code's model calls are recorded by `telemetry` alone, and only as
metadata.** Measured on Muse 1.4.1, not assumed:

- *The telemetry lane is Muse's own export.* With `telemetry` set to
  `{enabled: true, destination: "external", endpoint: <loopback receiver>}` in
  `~/.config/muse/settings.json`, Muse posts gzip OTLP protobuf to
  `<endpoint>/muse-code/telemetry/logs` and `/traces`, instead of to Meta's
  destinations. `OTEL_EXPORTER_OTLP_ENDPOINT` alone does nothing, and the echo
  provider exports nothing. The receiver maps the `model_call` log to one
  `llm_completion` pair (`:otel:` namespace, keyed on Muse's response id): model,
  provider, duration, input, output and cached token counts (Muse's input count
  includes the cached tokens, so the total is input plus output). A call whose
  `eof_clean` is false is marked `response_incomplete` on its close (core cannot
  be told it failed: a turn's close has no failed status), and with no response
  id it is skipped and counted apart from lost records. Every other
  event is skipped. A subagent's call is recorded in the parent session
  the hook path recorded for that child (a child nothing links stays a session of
  its own, as its hook rows do). Muse exports no body, so there is none to capture.
  Routing is by the `session_id` attribute; the election is whether the
  settings point at this receiver on loopback, so a settings file that does not
  (key absent or changed, destination not `external`, telemetry disabled, another
  port) records nothing and `doctor` says which. A policy forcing
  `privacy.telemetry` off would stop the export; `doctor` reports it when
  `muse config status` shows it, and otherwise says it is unverified.
- *No proxy lane is possible.* Muse ignores the system PAC and rejects the
  relay's CA even when `SSL_CERT_FILE` names it. Linux and Windows have no system
  proxy in this project at all.
- *The gate is not a record.* `PreLLMCall` and `PostLLMCall` produce the
  `model_call_gate` activity: the model, provider, message and tool counts,
  tool names and, under `content_capture` and after redaction, message
  previews, then a metadata-only close. It carries no usage, no turn, no
  request or response body, and is never `llm_completion`. `PostLLMCall`'s
  usage and trace context stay in the local trace.

`openbox doctor` says so for Muse on each of those points. `api.meta.ai` is in
the host rows of all three tools: a call there is attributed by its carrier
header, and with none or several it is skipped and counted (see
[mapping.md](mapping.md#model-turns)), never guessed. No Muse carrier is
known, so a Muse call through the relay is skipped.

| | `transport` (Claude Code; Codex on macOS) | `telemetry` (Claude Code, Codex, Muse) |
|---|---|---|
| Model request/response body | captured | never — this lane binds no content at all |
| Token counts + model id | yes | yes (its whole payload) |
| Refuses a call on a local HALT latch | yes, latch-only, no `/evaluate` round trip | no — receive-only, out of path |
| Suppressible by the governed tool | no — it observes bytes in path | yes — the tool reports its own calls, so it can under-report |
| Terminal CLI | covered | covered |
| Desktop app / browser | macOS only, through the system-wide PAC: claude.ai chat completions are recorded, one session per conversation (`chat:claude-ai:<uuid>`); other desktop and browser traffic is passed through unrecorded. Not implemented on Linux or Windows | unconfirmed against a real desktop client |
| Subscription-OAuth session | unconfirmed | unconfirmed |

See [Architecture](architecture.md) for the CA and PAC design this depends on.

**Exactly one lane emits a model-call turn per session.** An election derived
from the tool's own settings decides the producer (`transport` outranks
`telemetry` when both are routed); the lanes' activity-id namespaces are kept
disjoint so core's deduplication can never absorb one lane's event as
another's. Codex's election reads its own `config.toml` and the system-PAC
record, and the relay outranks telemetry only when **both** hold: the PAC
record is committed and lists Codex (with Codex's model host among the relay's
hosts), and the relay has recorded evidence that Codex really routes through it
— a marker file, `relay-observed/codex` in Codex's spool directory, written by
the relay at request time when it sees a Codex model completion that carries
both Codex's session carrier and its own `Originator: codex_cli_rs` header (the
generic request-id header alone is not enough: any OpenAI SDK script sends it),
holding the activation's nanosecond id so a re-activation needs fresh evidence. Until both,
telemetry stays the producer, so a Codex that ignores the PAC or does not trust
the relay's CA is never silenced. Once the relay is elected, telemetry records
nothing for Codex, so a Codex-host call the relay cannot record (an unrecognised
path, no session carrier) is recorded by nobody. `openbox doctor` counts those
from the local trace ("Codex-host calls skipped while the relay is elected"). `openbox doctor` names the elected lane and warns when nothing is
listening behind it — the check to run before trusting a data gap as
"nothing happened" rather than "nothing was recorded". `telemetry` ships only
the model id, four token counts, a duration and a request id — never a
prompt, a completion, or a cost (the server derives cost from a model-keyed
pricing table).

## 2. Field-derivation rules

- **`tool.kind`**: file tools (Read/Edit/Write, Codex `apply_patch`) →
  `file`; shell (Bash) → `shell`; MCP tools (`mcp__*`) → `mcp`.
- **`tool.mcp_server`**: parsed from the tool name pattern `^mcp__([^_]+)__`.
- **`tokens` / `model`**: gated by the `finops` posture key (on by default,
  `OPENBOX_FINOPS=0` to opt out). Claude Code and Codex report **per turn**
  (Muse reports none: no hook payload's usage reaches the wire): a
  `Stop`/`SubagentStop` pair becomes a `TurnStarted`/`TurnCompleted` pair
  carrying all four token counts and the model id, read from a local
  transcript/rollout file, never the provider's own usage API. Codex derives
  its counts as the delta between cumulative usage snapshots so the same
  tokens are never counted twice. `cost` is never computed client-side.
- **`status`**: derived structurally from which hook fired, never parsed from
  tool output. Claude Code: `PostToolUse` → `completed`, `PostToolUseFailure`
  → `failed` (the two hooks are mutually exclusive per call). Codex: not
  reported — see §1.
- **Assistant turn text → `activity_output.content`**: Claude Code and Codex, from
  the `Stop`/`SubagentStop` payload's `last_assistant_message`, gated on
  `content_capture` (on by default) and `finops`, redacted for secrets before
  attachment, then capped at 64KB. Thinking rides the same turn under
  `activity_output.thinking`.
- **`error_type`**: passed through an allowlist of the provider's own error
  values — never free text, since the underlying JSON key also carries a
  tool's own error string on a different hook.
- **`ToolCall`↔`ToolResult` correlation**: every provider exposes the tool's
  own call id; the adapter carries it into `span.invocation_id` and pairs the
  two events on the wire by a shared `activity_id`. A new adapter must also
  set `span.operation_id` for any class it lets the gate escalate, or an
  approval granted against one call cannot be consumed by a retry that
  addresses another — see [mapping.md](mapping.md) "Operation vs invocation
  identity".

## 3. Bounded non-goals

These are documented gaps, not missing work:

1. **Turn boundaries are not `SessionEnded`.** `Stop`/`SubagentStop` fire once
   per agent-loop turn, and a session has many turns. Never map a turn
   boundary to `SessionEnded`.
2. **Subagent lifecycle is boundary-only.** `SubagentStart` maps to
   `SubagentStarted`; a subagent tree is otherwise reconstructed from
   `agent_id` on its tool events, so a subagent that spawns and calls no tool
   leaves no trace. `SubagentStop` stays unwired as a lifecycle marker
   because it already has a job: closing a turn.
3. **Compaction internals stay out of scope.** `PreCompact`/`PostCompact` are
   wired as boundary signals; what compaction actually drops is not modeled.
4. **Content parity between providers is not a goal.** Both egress the
   prompt, the turn's reply text and thinking under `content_capture`. Only
   Claude Code also egresses tool input/output and failure detail: Codex hooks
   do not carry them (a gated Codex call still sends its tool input for the
   decision). State the asymmetry rather than average across it.
5. **Non-session telemetry is dropped, not synthesized.** Every event needs a
   resolvable `openbox_session_id`; a signal that has none is not emitted.
6. **One `ToolCall` per invocation**, even where a hook surface offers both a
   generic pre-tool hook and a more specific one; `event_id` idempotency
   guards any accidental double-count.
7. **`WorktreeCreate` is refused, not missing** (see §1) — a deliberate
   choice, not an omission.
8. **Headless coverage is best-effort.** In `claude -p`, Claude Code kills any
   hook still running at process teardown, so a short headless run can
   silently lose an async signal a longer interactive session would not.
9. **Existing installs don't pick up new hooks on their own** — a machine
   that upgrades the `openbox` binary keeps observing only its previous hook
   set until `openbox init` is re-run there.
10. **Muse Code: project hooks are not used.** A project's `.muse/hooks.json`
    is never written or governed; OpenBox installs into the user-wide
    `~/.config/muse/settings.json` only.
11. **Muse Code: `muse serve` and the Muse Server Protocol are not governed.**
    Only hooks in a CLI session are.
12. **Muse Code: the hooks lane does not record model calls.** The gate in §1b
    is an evaluation, not a record; no usage or body is taken from a hook
    payload.
13. **Muse Code: no shell-profile proxying.** OpenBox never edits a shell
    profile to point Muse at a proxy.
14. **No system proxy on Linux or Windows.** The PAC and CA trust exist on
    macOS only, so Codex there is telemetry-only and Muse has no proxy path.
15. **Muse Code older than 1.4.0 is refused.** `init` stops before writing
    anything, because the `onFailure` successor the install depends on is a
    1.4.0 feature.
16. **Muse Code: a hook payload over 256 KiB is never delivered.** Muse does
    not start the hook at all, so neither the gate nor its successor runs on
    that action. This is **detected, not prevented**: at `Stop` and
    `SessionEnd` the adapter reads Muse's own append-only session log for the
    join fields only and records a local `evidence.gap` finding for each
    action with no gate record. Nothing is blocked or sent to the platform;
    `openbox doctor` shows the count.
17. **Muse Code: commits carry the trailer but no `CommitCreated`.** The
    attestation reads an environment marker per tool. A Muse tool call's shell
    carries `MUSE_TOOL_USE_ID` and `MUSE_RELEASE_INFO` but no session id, so no
    commit is attributed to a Muse session (and it never attests as Claude Code,
    whose markers it lacks).

## 4. Enforcement posture

All three providers have blockable hooks, and enforcement is unconditional on
each: every gated `PreToolUse` call and every prompt goes to `/evaluate`,
and the verdict is the server's, never inferred locally. The local hook only
applies what came back, and only tightens — it never turns a provider's own
deny into an allow, and the one `allow` it may emit rides a redacting
rewrite, never a bare grant. A HALT verdict additionally latches the run:
every later gated call in that run is refused locally with no further round
trip. A `--resume`d run starts unlatched and is re-evaluated fresh against
the same policies; a `/clear` starts an unrelated session.

Deprecated posture keys (an old two-step evaluation toggle and a delivery
opt-out, among others) still parse — so `openbox doctor` can flag them as
set-but-ignored — but no longer change behavior. See
`internal/adapters/common/devconfig/devconfig.go` for the exact key names and
`internal/adapters/common/devconfig/posture.go` for what each one resolves to
now.

No provider ever renders an "ask" verb back to the developer: a held
approval decides, and the adapter renders only its outcome (allow, deny, or a
denial naming the approval reference if it went unanswered). On Claude Code
this avoids asking the developer to approve their own filed request; on
Codex there is no "ask" verb to render at all, and a fallthrough would run
the tool ungoverned, so deny is the safe mapping. What the developer sees is
described in [Getting started § Approvals](getting-started.md#approvals).

**Muse Code.** The same rules hold, with four differences:

- HALT renders as a plain deny, the same as BLOCK and an unanswered approval.
  Muse rejects `continue` and `stopReason` on the events that gate, and a
  rejected answer is discarded, which Muse reads as an allow. The run-keyed
  latch still refuses every later gated call of that run, locally.
- Nothing ever writes `allow`. A proceed is silence, or `updatedInput` alone
  for a redacted write body.
- **Every model call is evaluated.** `PreLLMCall` goes to `/evaluate` before
  the request is sent and can be blocked (§1b). A delivery failure denies only
  that call.
- **Muse fails open.** A hook that crashes, times out or answers invalidly
  lets the action proceed. Two layers backstop that: a gated path ends in a
  valid answer or a non-zero exit (exit 2 reads as a block), and every gated
  handler carries an `onFailure` successor that only denies
  (`openbox hook muse --fail-closed`). Checked by hand on Muse 1.4.1: all four
  refusal shapes block, and a crash, an unknown JSON shape or a timeout runs
  the successor, which denies. Plain non-JSON output on exit 0 is the one case
  Muse allows with no successor; the adapter only ever answers with JSON and a
  crash writes nothing to stdout.

**Assurance caveat.** This is all enforced by a user-local hook. Until a
centrally managed provider config is deployed, a developer can remove the
hook or edit the local config — treat local enforcement as prevention without
assurance, a deployment property rather than a code gap. For Muse, an
org-deployed hooks file and policy exist in
[`deployments/managed/muse/`](../deployments/managed/muse/); its policy keys
are unverified. Claude Code's own
`ConfigChange` hook narrows this a little: editing the settings file that
holds the hook registration is itself an observed, blockable event, except
for one provider-documented unblockable settings path it deliberately never
gates.

## 5. Evidence: what is proven, and by what

Claims in this document are graded on an evidence ladder, from weakest to
strongest:

| Class | What it means |
|---|---|
| E0 | Prose, or field observation. No test. |
| E1 | A unit fixture: an in-process value or a locally read file, never a real request. |
| E2 | Exercised end to end against a fake control plane, graded on the actual artifact the claim is about (captured outbound body, rendered decision, or on-disk state). |
| E3 | Needs a live stack. Nothing in this repository can reach it. |

A round trip that only inspects a local struct afterward is still E1, not E2
— the ladder grades what is checked, not what ran.

The adapter, mapping, and enforcement claims above are backed at E1 or E2 by
the adapters' own test suites (`internal/adapters/claude-code/`,
`internal/adapters/codex/`, `internal/adapters/muse/`) and by the governance
evals, runnable directly:

```
go test -run TestGovernanceEval -v ./cmd/openbox/
```

The governance evals run the real hook binary against an in-process fake
control plane and grade what it captured. Each grader is named here so a
reader can find the check behind a claim; `cmd/openbox/evidencetable_test.go`
fails if a citation stops resolving or a registered grader goes unnamed.

| Claim | Class | Evidence |
|---|---|---|
| A hook event reaches the wire from the real binary | E2 | `cmd/openbox/main_test.go` · `TestHookEndToEndSmoke` |
| A call that ran is reported as exactly two rows sharing one `activity_id` | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalGraders` (`pairing`) |
| Nothing is lost between the hook's stdin and the wire | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalGraders` (`completeness`) |
| A call's label is the tool that was invoked, on both halves | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalGraders` (`activity-type`) |
| One call, one row of each kind | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalGraders` (`delivery-once`) |
| A lifecycle signal carries its payload in `signal_args` | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalGraders` (`signal-args`) |
| Content leaves only when capture is on, and only in a content field | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalContentGateBothDirections` (`content-gate`) |
| A secret is rewritten before it reaches either the disk or the wire | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalRedactionRunsBeforeAttachment` (`redaction`) |
| A session's first core row is `WorkflowStarted` | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalGraders` (`start-first`) |
| Every delivery-failure class is exactly one attempt | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEvalOneAttemptPerFailureClass` (`one-attempt`) |
| An unaccepted event denies only its own call, never latches the rest of the run | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEvalCoreDownAtSessionStartDeniesAndRecovers` |
| Once a REAL HALT verdict latches a run, every later gated call of it also denies | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalGraders` (`halted-after-failure`) |
| A HALT refuses the rest of the run without asking again | E2 | `cmd/openbox/governanceeval_verdicts_test.go` · `TestGovernanceEvalHaltLatchesTheRestOfTheRun` |
| An approval unanswered denies, rejected denies, granted proceeds | E2 | `cmd/openbox/governanceeval_verdicts_test.go` · `TestGovernanceEvalApproval` |
| Every grader can actually fail | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEvalMutations` |
| A Muse gated event answers with a closed, schema-valid shape and never `allow` | E1 | `internal/adapters/muse/outputcontract_golden_test.go` · `TestContractGoldens` |
| Every Muse gated event, against every verdict, answers inside its closed key set, and only a real HALT latches | E2 | `internal/adapters/muse/enforce_conformance_test.go` · `TestEveryGatedEventAgainstEveryVerdict` |
| A secret in a Muse write body is rewritten as `updatedInput` alone, never paired with an allow | E2 | `internal/adapters/muse/enforce_conformance_test.go` · `TestSecretInAWriteBodyIsRewrittenAsUpdatedInputAlone` |
| A Muse prompt, tool call, permission request and model call deny when core is unreachable | E2 | `internal/adapters/muse/hookrun_test.go` · `TestGatedEventsDenyWhenCoreIsUnreachable` |
| A denied Muse model call leaves the gate's started row only | E2 | `internal/adapters/muse/hookrun_test.go` · `TestModelCallDenyBlocksAndLeavesTheStartedRowOnly` |
| An allowed Muse model call is one `:llmgate:` pair carrying no usage or turn | E2 | `internal/adapters/muse/hookrun_test.go` · `TestModelCallAllowThenFinishedIsOnePair` |
| A real HALT latches the Muse run, and later gated calls of it, model calls included, deny with no round trip | E2 | `internal/adapters/muse/hookrun_test.go` · `TestHaltLatchesTheRunAndDeniesLaterCallsWithoutTheNetwork` |
| A Muse gate event validates against the 1.10 contract and pairs | E1 | `internal/adapters/muse/conformance_test.go` · `TestModelCallGateEventsValidateAndPair` |
| The contract rejects usage, a turn or a producer id on a gate | E1 | `internal/conformance/modelcallgate_test.go` · `TestModelCallGateRejectsWhatItMayNotCarry` |
| A gate's message previews are gated content, redacted first | E1 | `internal/adapters/muse/llmgate_test.go` · `TestModelCallPreviewsAreGatedAndRedacted` |
| A Muse hook that crashes exits non-zero, so the `onFailure` successor runs | E2 | `cmd/openbox/musefault_test.go` · `TestMuseGatedCrashExitsNonZero` |
| The Muse fail-closed successor denies every gated event and writes nothing | E2 | `cmd/openbox/failclosed_test.go` · `TestMuseFailClosedDeniesGatedEventsAndTouchesNothing` |
| `init --provider muse` refuses a Muse older than 1.4.0 and writes nothing | E1 | `cmd/openbox/initmuse_test.go` · `TestInitMuseRefusesAnOldMuseAndWritesNothing` |
| Muse install then uninstall restores the developer's settings | E1 | `cmd/openbox/initmuse_test.go` · `TestInitMuseThenUninstallRestoresTheDevelopersSettings` |
| Two Muse `init` runs leave no stray posture key | E1 | `cmd/openbox/initmuse_test.go` · `TestInitMuseWritesNoStrayPostureKeyOnEitherRun` |
| A tool action that never reached the gate becomes a local `evidence.gap` finding, with no journal content logged | E1 | `internal/adapters/muse/reconcilehook_test.go` · `TestStopReconcilesTheSessionJournalAgainstGatedCalls` |
| Muse's usage and trace context stay in the local trace, never on a wire event | E2 | `internal/adapters/muse/hookrun_test.go` · `TestUsageAndTraceparentStayLocal` |
| The receiver serves Muse's export paths and decodes a gzip protobuf export from them | E2 | `internal/telemetry/pathalias_test.go` · `TestReceiverServesMusePathsOverHTTP` |
| Only Muse's two paths are rewritten, and the caller's request is left as received | E1 | `internal/telemetry/pathalias_test.go` · `TestPathAliasRewritesOnlyMusePaths` |
| A record is routed by its tool's session attribute, and one carrying more than one tool's is refused | E1 | `cmd/openbox/telemetryroute_test.go` · `TestTelemetryRoutingTable` |
| A Muse `model_call` becomes one `llm_completion` pair in the `:otel:` namespace with response id, model, provider and usage | E1 | `internal/cli/telemetryemit/musemapper_test.go` · `TestMuseModelCallBecomesOneLLMCompletionPair` |
| Every other Muse event is skipped | E1 | `internal/cli/telemetryemit/musemapper_test.go` · `TestMuseEveryOtherEventIsSkipped` |
| A Muse subagent's call is recorded in the session that spawned it | E1 | `internal/cli/telemetryemit/musemapper_test.go` · `TestMuseSubagentCallFollowsTheRecordedFold` |
| No Muse content-shaped attribute reaches the wire, at either capture posture | E2 | `internal/cli/telemetryemit/musemapper_test.go` · `TestMuseContentNeverReachesTheWire` |
| A scrubbed Muse 1.4.1 export yields one pair per model call through decode, routing and mapping | E1 | `cmd/openbox/telemetrymuse_test.go` · `TestMuseFixtureThroughTheChainYieldsOnePairPerModelCall` |
| The real telemetry command records a Muse export at core under Muse's own identity, with the election derived from `--muse-settings` | E2 | `cmd/openbox/telemetrymuse_test.go` · `TestTelemetryCommandRecordsMuseModelCalls` |
| Muse's telemetry election is routed only when its settings export to this receiver on loopback | E1 | `internal/cli/activation/museelection_test.go` · `TestResolveMuseElection` |
| Muse's telemetry pointer is written only after the daemon is proven listening | E1 | `cmd/openbox/initmuselane_test.go` · `TestMuseTelemetryIsWrittenOnlyAfterTheDaemonIsProvenUp` |
| A Muse install that fails after the unit is written removes the unit and leaves settings untouched | E1 | `cmd/openbox/initmuselane_test.go` · `TestAMuseInstallThatFailsAfterTheUnitIsWrittenRemovesTheUnitAndTouchesNoSettings` |
| A second Muse `init` changes neither settings, the restore record nor the unit | E1 | `cmd/openbox/initmuselane_test.go` · `TestInitMuseInstallsTheTelemetryLaneAndASecondInitChangesNothing` |
| Muse's `telemetry` key is merged by path with every foreign byte kept, and restored byte-exact | E1 | `internal/adapters/muse/telemetrykeys_test.go` · `TestWriteTelemetryKeepsEveryForeignByte` |
| A `telemetry` value the developer changed is neither overwritten nor restored over | E1 | `internal/adapters/muse/telemetrykeys_test.go` · `TestADeveloperEditAfterInitIsNeitherOverwrittenNorRestoredOver` |
| Uninstall restores Muse's prior `telemetry` value, deletes it if there was none, and reports drift | E1 | `cmd/openbox/initmuselane_test.go` · `TestUninstallRestoresMusesTelemetryExactly` |
| `doctor` says whether Muse's telemetry lane records, and why not when it does not | E1 | `cmd/openbox/doctormuse_test.go` · `TestDoctorMuseSaysWhyModelCallsAreNotRecorded` |
| `doctor` says Muse has no proxy lane, and why | E1 | `cmd/openbox/doctormuse_test.go` · `TestDoctorMuseSaysWhyThereIsNoProxyLane` |
| A call on a host several providers reach is attributed by carrier, else skipped | E1 | `internal/cli/sessionkey/attribute_test.go` · `TestAttributeProxy` |
| A relayed shared-host call with no carrier is skipped, not recorded | E1 | `cmd/openbox/transportmultiprovider_test.go` · `TestRelaySkipsASharedHostCallWithNoCarrier` |
| Codex's proxy election needs a committed PAC record listing Codex plus relay evidence | E1 | `internal/cli/activation/codexelection_test.go` · `TestResolveCodexElectionTable` |
| The relay's Codex evidence is written only under a committed record | E1 | `internal/cli/activation/codexelection_test.go` · `TestMarkCodexProxyObservedWritesOnlyUnderACommittedRecord` |
| `doctor` names the Codex lane that is producing, and warns when the elected relay is not listening | E1 | `cmd/openbox/doctorcodex_test.go` · `TestDoctorSaysTheRelayIsElectedOnceItHasSeenCodex` |
| A Meta Model API key is redacted by shape | E1 | `internal/decision/secrets_test.go` · `TestRedact_MetaAPIKey` |
| Windows behaves at runtime as it does on macOS and Linux | **E0** | Cross-compiled in CI only; not run. |
| Muse's payload, answer and `onFailure` shapes match a real Muse | **E3** | Checked by hand on Muse 1.4.1: payloads captured (the fixtures are scrubbed copies), the four refusal shapes block, and a crash, unknown JSON shape or timeout runs the deny-only successor. CI cannot install Muse, so no automated test runs a real one; CI holds the recorded goldens (E1). |
| Muse's export, its paths and the `telemetry` key behave as recorded | **E3** | Measured by hand on Muse 1.4.1 with real Meta calls (the fixture is a scrubbed copy of that export). CI cannot install Muse; the recorded fixture is E1. |
| The control plane accepts, stores and keeps apart what the client sends | **E3** | Needs a live platform. |

What nothing here proves: that the control plane accepts the wire, stores a
row, keeps two rows apart, or that a socket binds and TLS terminates against
a real listener. Each of those is E3, owned entirely by the closed side.
