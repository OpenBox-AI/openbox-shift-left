# Provider coverage

How Claude Code and Codex's real hook/telemetry surfaces map onto this
project's normalized lifecycle events, and where each provider falls short of
full coverage. This is the reference an adapter author works against.

**Who this is for.** Someone writing or reviewing an adapter, or checking
whether a governance claim holds for a given tool. For installing the agent,
see [Getting started](getting-started.md); for the overall shape, see
[Architecture](architecture.md); for the event contract and field-level
mapping rules, see [the event contract](dev-event-contract.md) and
[mapping.md](mapping.md).

**Shipped providers.** Claude Code and Codex. Each adapter's `Capabilities()`
— in `internal/adapters/<tool>/capabilities.go` — is the authoritative
per-provider profile; this document must agree with it. No other provider is
implemented.

## 1. Lifecycle coverage matrix

| Contract type | Claude Code | Codex |
|---|---|---|
| `SessionStarted` | `SessionStart` hook | `SessionStart` hook |
| `PromptSubmitted` | `UserPromptSubmit` hook | `UserPromptSubmit` hook |
| `ToolCall` | `PreToolUse` hook | `PreToolUse` hook |
| `ToolResult` | `PostToolUse` hook | `PostToolUse` hook |
| `SessionEnded` | `SessionEnd` hook | `SessionEnd` hook |
| `SubagentStarted` | `SubagentStart` hook | `SubagentStart` hook |
| `PermissionRequest` | `PermissionRequest` hook; content-gated; no `tool_use_id`, so it never correlates to the tool call it is about | `PermissionRequest` hook; content-gated |
| `PermissionDenied` | `PermissionDenied` hook; only auto-mode classifier denials — a static deny rule or a manual denial never fires it | none |
| `APIError` | `StopFailure` hook | none |
| `PreCompact` / `PostCompact` | wired; content-gated (the `/compact` instructions and the summary) | wired; content-gated |
| `CommitCreated` | `git post-commit` hook, only where the hook is installed, agent commits only — see [mapping.md](mapping.md) | same |
| `Deploy` | git-action level, not a hook | same |

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

None of the three model-call producers has a hook of its own — this table is
what each *lane* sees of a single model call, a separate axis from §1's hook
table. `openbox init --provider claude-code` installs two lanes for Claude Code:
`transport` (an in-path CONNECT/TLS relay, activity namespace `:proxy:`) and
`telemetry` (a local OTLP receiver, namespace `:otel:`). `openbox init
--provider codex` installs `telemetry` only — Codex has no in-path relay.
A legacy `gateway` lane (`:gateway:`) still exists in the codebase but is no
longer installed by `init`; treat it as retired rather than as active
coverage.

| | `transport` (Claude Code only) | `telemetry` (Claude Code, Codex) |
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
another's. `openbox doctor` names the elected lane and warns when nothing is
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
  `OPENBOX_FINOPS=0` to opt out). Both providers report **per turn**: a
  `Stop`/`SubagentStop` pair becomes a `TurnStarted`/`TurnCompleted` pair
  carrying all four token counts and the model id, read from a local
  transcript/rollout file, never the provider's own usage API. Codex derives
  its counts as the delta between cumulative usage snapshots so the same
  tokens are never counted twice. `cost` is never computed client-side.
- **`status`**: derived structurally from which hook fired, never parsed from
  tool output. Claude Code: `PostToolUse` → `completed`, `PostToolUseFailure`
  → `failed` (the two hooks are mutually exclusive per call). Codex: not
  reported — see §1.
- **Assistant turn text → `activity_output.content`**: both providers, from
  the `Stop`/`SubagentStop` payload's `last_assistant_message`, gated on
  `content_capture` (on by default) and `finops`, redacted for secrets before
  attachment, then capped at 64KB. Thinking rides the same turn under
  `activity_output.thinking`.
- **`error_type`**: passed through an allowlist of the provider's own error
  values — never free text, since the underlying JSON key also carries a
  tool's own error string on a different hook.
- **`ToolCall`↔`ToolResult` correlation**: both providers expose the tool's
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

## 4. Enforcement posture

Both providers have blockable hooks, and enforcement is unconditional on
both: every gated `PreToolUse` call and every prompt goes to `/evaluate`,
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

Neither provider ever renders an "ask" verb back to the developer: a held
approval decides, and the adapter renders only its outcome (allow, deny, or a
denial naming the approval reference if it went unanswered). On Claude Code
this avoids asking the developer to approve their own filed request; on
Codex there is no "ask" verb to render at all, and a fallthrough would run
the tool ungoverned, so deny is the safe mapping. What the developer sees is
described in [Getting started § Approvals](getting-started.md#approvals).

**Assurance caveat.** This is all enforced by a user-local hook. Until a
centrally managed provider config is deployed, a developer can remove the
hook or edit the local config — treat local enforcement as prevention without
assurance, a deployment property rather than a code gap. Claude Code's own
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
`internal/adapters/codex/`) and by the governance evals, runnable directly:

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
| Windows behaves at runtime as it does on macOS and Linux | **E0** | Cross-compiled in CI only; not run. |
| The control plane accepts, stores and keeps apart what the client sends | **E3** | Needs a live platform. |

What nothing here proves: that the control plane accepts the wire, stores a
row, keeps two rows apart, or that a socket binds and TLS terminates against
a real listener. Each of those is E3, owned entirely by the closed side.
