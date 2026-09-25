# Provider coverage & mapping rules

How the three target coding tools' real event surfaces map onto this contract's
12 lifecycle types, plus the **bounded non-goals**. Sourced from a G3_REVIEW
pass over the tools' official docs (Claude Code hooks + OTel; Cursor hooks +
Admin API; OpenAI Codex hooks + `notify` + Usage/Cost API). This is the
reference an adapter author
implements `emit` against.

> **Who this is for.** Someone writing or reviewing an adapter, or deciding
> whether a governance claim holds for a given tool. If you are setting a
> machine up, you want [Getting started](getting-started.md); if you want the
> overall shape first, [Architecture](architecture.md). Everything below assumes
> both.

**Adapter status:** Claude Code and Codex are **shipped**; Cursor is a surface
survey, not shipped support. Each adapter's `Capabilities` — in
`internal/adapters/<tool>/capabilities.go` — is the authoritative per-provider
profile, and this document must agree with it.

**One producer is not an adapter at all.** The local gateway (contract v1.5) emits a `TurnCompleted` for each relayed model call without going through
any provider adapter or hook; it observes the HTTP exchange itself. It is Claude
Code only, opt-in per machine, and nothing in this document's matrix describes
it: a lifecycle matrix maps hooks, and the gateway has none. Its fields are in
[mapping.md](mapping.md) §3 and its own §7 verification items.

**Narrower than "Claude Code": the terminal CLI only.** Measured 2026-08-27; a
CLI session relayed and was captured; a desktop-app session over the same
install produced nothing, because the desktop app routes through its own
third-party inference configuration rather than `ANTHROPIC_BASE_URL`. State it
rather than averaging it: two developers on one org and one posture, one working
in the terminal and one in the desktop app, send *entirely different* model-call
evidence; the first sends whole request and response bodies, the second sends
none and nothing reports the gap.

**Two more non-adapter producers, at different stages** (contract v1.6). A
local OTLP **telemetry** receiver (`:otel:`) and a local in-path TLS
**transport** relay (`:proxy:`) are meant to cover the desktop and
subscription-OAuth calls the gateway lane cannot reach; phases 09–13 of plan
`260827-2301-go127-oss-three-lanes`.

Both are installed by `openbox init --provider claude-code` — there is no flag
to opt into either, and `openbox uninstall` backs them out. Both remain
unconfirmed against a real client.

**Read this before reading any row below.** Both new lanes are verified by
replay: real recorded traffic through the shipped code path, bind-free, with the
relay's upstream dial substituted (`internal/gateway/gatewaytest`) and no socket
anywhere. That proves the bytes the relay forwards and captures, the mapping,
the gate and the caps; it proves nothing about bind, listen, TLS to a real
socket, the OTLP HTTP intake, or what core stores. Those live only in the
live stack these lanes have never been run against.

- **`:proxy:` (transport)**; a CONNECT to a host in the installed providers'
  host-table union is TLS-terminated with a project CA and served by the
  existing gateway relay, and the evidence reaches the spool. The CA itself is
  generated unconstrained (owner ruling 2026-09-22); the allowlist is the
  containment — see [Architecture](architecture.md)'s decision record. **A recorded model call now crosses that path
  byte-identically in both directions, and a recorded 60-frame SSE response
  streams through it per chunk** (phase 13); retiring phase 11's "no response
  body has ever traversed this lane". Refusal is no longer fully dormant:
  `transport.WithGate` wires a latch-only decorator that refuses a relayed
  call for a run some lane already latched HALTed, with no `/evaluate` round
  trip; a synchronous per-call server verdict still has no production
  caller — see §5's refusal-row note.
- **`:otel:` (telemetry)**; the receiver, the mapper and `openbox telemetry` all
  exist and are wired, and a real recorded OTLP export maps end to end: 20
  records, 16 event types, of which `api_request` becomes a conformant
  `TurnCompleted` and the other 15 are **counted as drops** so a lane dropping
  everything is distinguishable from a quiet session. Emission is suppressed
  unless this lane wins the producer election. Two things are still unconfirmed.
  The replay enters one layer below the HTTP server, so it adds nothing about
  the intake. **One synthetic export has crossed that intake end to end on a
  bind-capable host** (phase 09's control test: real command, real receiver,
  real POST, real spool); but **it was JSON, and production is configured for
  `http/protobuf`** (`internal/cli/activation/keys.go`). No test in this
  repository drives the collector's protobuf decoder, so the wire format real
  traffic will actually use is unexercised, and the real client has never
  exported to this lane at all. And **it has never been confirmed that the env
  keys the installer writes are the ones the client actually reads**; they are
  copied verbatim from a proven set in a sibling lab repo and pinned as a
  literal list, but every test around them asserts JSON we wrote, and the client
  silently ignores a name it does not recognize. A rename yields a green suite
  and a receiver that never gets a record.

**Exactly one lane emits a model-call turn per session**, decided by an election
derived from where the tool's settings route model calls; precedence transport >
gateway > telemetry, in-path outranking client-asserted. That is a correctness
invariant rather than a preference: the three namespaces are deliberately
disjoint so core's dedupe cannot absorb one lane's event as another's, which
means two lanes emitting would both store and double every token count with no
error anywhere. The election is answered PER record rather than once per daemon:
resolving it at startup shipped that exact double-count into review, because
an install brings telemetry up before transport, and the daemon froze an answer
that was correct only for the second it was taken.

So a `:otel:` or `:proxy:` row can now legitimately appear in a developer's
data. What a reader must NOT infer from a declared discriminator, or from an
installed lane, is that evidence is arriving: `openbox doctor` names the elected
producer and warns when the elected lane has nothing listening behind it, and
that is the check to run.

The claims are **not** interchangeable and this document must not flatten them:
transport and gateway observe the bytes in path; telemetry is the governed tool
reporting its own calls, so it is suppressible by the thing it observes. A
lane's presence in a row will say which one produced the evidence.

**Per-hook client-surface marker (§1 below).** A different axis from the lane
matrix above: this marks whether a given lifecycle hook is reachable from the
**terminal CLI**, the **desktop app**, or both -- not which model-call lane
observes it. Recorded as an annotation inside the existing Claude Code cell
rather than a fourth column, matching how the table already carries other
per-row caveats. Fixed vocabulary: `both` (observed firing on both surfaces)
· `terminal` (observed on the terminal CLI only) · `desktop` (observed on
the desktop app only) · `vendor-gap(<surface>)` (the named surface has no
path to this event at all -- a provider gap, not an adapter defect) ·
`unsurveyed` (no per-surface observation exists; the default, and never
inferred as `both`). Unrelated to the Cursor column's own `*(unsurveyed)*`,
which answers a different question -- whether Cursor supports the hook at
all, not which Claude Code surface it fires on. Every marked row cites the
session or audit section that observed it; an uncited claim is downgraded to
`unsurveyed`. Seven rows below are marked from one measurement (audit §3.3,
2026-09-12); the rest stay `unsurveyed` pending a desktop-specific run of the
same exercise.

## 1. Lifecycle coverage matrix

| Contract type | Claude Code *(shipped)* | Cursor *(survey only unbuilt)* | Codex *(shipped)* |
|---|---|---|---|
| `SessionStarted` | `SessionStart` hook — `terminal` for the `clear` source value only (audit §3.3, 2026-09-12): desktop's `/clear` instead produces `SessionEnd(other)` + `SessionStart(startup)`, never `clear` — provider truth, not an adapter bug (`enumOr` would have kept it) | `sessionStart` | `SessionStart` hook |
| `PromptSubmitted` | `UserPromptSubmit` — `unsurveyed` | `beforeSubmitPrompt` | `UserPromptSubmit` |
| `ToolCall` | `PreToolUse` — `unsurveyed` | `preToolUse` / `beforeShellExecution` / `beforeMCPExecution` / `beforeReadFile` | `PreToolUse` / `PermissionRequest` |
| `ToolResult` | `PostToolUse` — `unsurveyed` | `postToolUse` / `afterShellExecution` / `afterMCPExecution` / `afterFileEdit` | `PostToolUse` |
| `SessionEnded` | `SessionEnd` hook — `unsurveyed` | `sessionEnd` | `SessionEnd` hook (real, ≥ 0.145.0; no longer synthesized) |
| `CommitCreated` | *(git-level)* — `unsurveyed` | *(git-level)* | *(git-level)* |
| `Deploy` | *(git-level)* — `unsurveyed` | *(git-level)* | *(git-level)* |
| `SubagentStarted` *(v1.2)* | `SubagentStart` hook — `unsurveyed` | *(unsurveyed)* | **none** |
| `PermissionDenied` *(v1.2)* | `PermissionDenied` hook; **auto-mode classifier denials only**; a static `permissions.deny` rule denies without firing it (verified), so absence is not evidence that nothing was denied — `both` (audit §3.3, 2026-09-12), auto-mode only. Not regression-testable on demand: deny rules, manual denials and hook blocks all bypass it, and no forcing recipe exists; verified by mapper fixture only — `internal/adapters/claude-code/content_conformance_test.go:258-262` (`TestContentCaptureConformance` C38 `"PermissionDenied.reason"`), `internal/adapters/claude-code/enforce_conformance_test.go:395` (`TestEnforcementConformance` C23), `internal/adapters/claude-code/mapper_test.go:625-633` (`TestMap_LifecycleSignals`) | `permissionRequest`? *(unsurveyed)* | **none** |
| `APIError` *(v1.2)* | `StopFailure` hook — `unsurveyed` | *(unsurveyed)* | **none** |
| `Setup` *(v1.8)* | `Setup` hook — `unsurveyed` | *(unsurveyed)* | **none** |
| `InstructionsLoaded` *(v1.8)* | `InstructionsLoaded` hook — `unsurveyed` | *(unsurveyed)* | **none** |
| `UserPromptExpansion` *(v1.8)* | `UserPromptExpansion` hook; structural-only, no content line ever — `unsurveyed` | *(unsurveyed)* | **none** |
| `MessageDisplay` *(v1.8)* | `MessageDisplay` hook; structural-only, `message_id` is not the API's `msg_…` id — no transcript join exists (see the annotation below) — `unsurveyed` | *(unsurveyed)* | **none** |
| `PermissionRequest` *(v1.8)* | `PermissionRequest` hook; no `tool_use_id`, so it cannot pair with any tool activity (see the annotation below); **content-gated** (`requested_tool_input`) — `unsurveyed` | *(unsurveyed)* | **none** |
| `PostToolBatch` *(v1.8)* | `PostToolBatch` hook; structural-only, no `tool_calls[]` content — `unsurveyed` | *(unsurveyed)* | **none** |
| `Notification` *(v1.8)* | `Notification` hook; **content-gated** (`notification_message`) — `unsurveyed` | *(unsurveyed)* | **none** |
| `TaskCreated` *(v1.8)* | `TaskCreated` hook; **content-gated** (`task_subject`; the description is never sent) — `both` (audit §3.3, 2026-09-12), `TaskCreate` tool only | *(unsurveyed)* | **none** |
| `TaskCompleted` *(v1.8)* | `TaskCompleted` hook; same `task_subject` key as `TaskCreated`, never paired as an Activity — `both` (audit §3.3, 2026-09-12), `TaskCreate` tool only | *(unsurveyed)* | **none** |
| `TeammateIdle` *(v1.8)* | `TeammateIdle` hook — `unsurveyed` | *(unsurveyed)* | **none** |
| `ConfigChange` *(v1.8)* | `ConfigChange` hook; the one new hook that is gated (§4); `source:policy_settings` never gated — `unsurveyed` | *(unsurveyed)* | **none** |
| `CwdChanged` *(v1.8)* | `CwdChanged` hook; structural, always sent — `vendor-gap(desktop)` (audit §3.3, 2026-09-12): the desktop directory tool `mcp__ccd_directory__change_directory` moves cwd without firing it | *(unsurveyed)* | **none** |
| `DirectoryAdded` *(v1.8)* | `DirectoryAdded` hook; structural, always sent — `terminal` (audit §3.3, 2026-09-12): `/add-dir` is absent on desktop | *(unsurveyed)* | **none** |
| `FileChanged` *(v1.8)* | `FileChanged` hook; a bounded watch list, not coverage — never the file body (see §3 and the annotation below) — `both` (audit §3.3, 2026-09-12), watcher-only | *(unsurveyed)* | **none** |
| `WorktreeRemove` *(v1.8)* | `WorktreeRemove` hook; unpaired by construction — `WorktreeCreate` is refused, not missing (§3) — `unsurveyed` | *(unsurveyed)* | **none** |
| `PreCompact` *(v1.8)* | `PreCompact` hook; **content-gated** (`compact_instructions`) — `unsurveyed` | *(unsurveyed)* | **none** |
| `PostCompact` *(v1.8)* | `PostCompact` hook; **content-gated** (`compact_summary`) — `unsurveyed` (surface axis; retention decision is in [data-and-privacy.md](data-and-privacy.md)) | *(unsurveyed)* | **none** |
| `PreModelSwitch` *(v1.8)* | `PreModelSwitch` hook; never sets the token-rollup `model` field (see the annotation below) — `unsurveyed` | *(unsurveyed)* | **none** |
| `PostModelSwitch` *(v1.8)* | `PostModelSwitch` hook; not a pair with `PreModelSwitch` in either direction (see the annotation below) — `unsurveyed` | *(unsurveyed)* | **none** |
| `Elicitation` *(v1.8)* | `Elicitation` hook; **content-gated** (`elicitation_message`, the MCP server's prompt) — `unsurveyed` | *(unsurveyed)* | **none** |
| `ElicitationResult` *(v1.8)* | `ElicitationResult` hook; **content-gated** (`elicitation_response`) — your answer, form values included; residual risk in [data-and-privacy.md](data-and-privacy.md) — `unsurveyed` | *(unsurveyed)* | **none** |

**Four rows above buy less than they appear to, named individually because a matrix cell can't carry the caveat:**

- **`MessageDisplay`**'s `message_id` is **not** the API `msg_…` id, so no transcript join from this event back to a specific assistant message exists.
- **`PermissionRequest`** carries no `tool_use_id`, so it cannot pair with any tool activity — a permission request and the tool call it is about are not correlated on the wire.
- **`Pre`/`PostModelSwitch`** share no id in either direction; they are never a pair, only two independent signals.
- **`FileChanged`** fires only for a bounded watch list (`.env|.envrc|.mcp.json|CLAUDE.md`, literal basenames, no wildcard), never for coverage of "files changed" in general; see §3.

## 1a. `/clear` and `--resume` are different, and only one continues a run

Measured against installed Claude Code 2.1.263. **Each transition below was
observed exactly once** — enough to refute the claim that a `/clear` continues a
run, weaker as confirmation, and not a statistical claim about the fleet:

- **`--resume` is the one source that continues a run.** Same session id across
  the whole boundary (`bbf2b31d-cd3b-4130-8ca3-20eacc269a85`):
  `SessionStart(source=startup)` → `SessionEnd(reason=prompt_input_exit)` →
  `SessionStart(source=resume)`. The tool reuses the session id; the client
  mints a fresh v4 UUID `run_id` for the new run and sets
  `continued_from_run_id` to the sealed run it continues — the shape Temporal
  calls continue-as-new. The run that was measured used the
  **interactive picker** (`claude --resume` with no argument). `--resume <session-id>`
  and `--continue` are separate code paths, which the vendor's help text describes
  identically ("continues that session … under the same ID") but which were **not**
  exercised. Corroborating, not measured: 55 local transcripts hold one `sessionId`
  across gaps over two hours, and `--fork-session` exists to "create a new session
  ID instead of reusing the original" — a flag that only makes sense if reuse is the
  default.
- **`/clear` does NOT continue a run and does not reuse the session id.**
  Measured: `SessionEnd(reason=clear)`, then `SessionStart(source=clear)` on a
  **new** session id ~46ms later (`43b1dc13-…` → `be87310b-…`). The new session
  starts at generation 0, `run_id` equal to its own (new) session id, and
  **no** `continued_from_run_id` — there is nothing to link to; the sealed
  session is a different `openbox_session_id`, not a predecessor run of this
  one.
- **`startup`, `compact` and `fork` do not continue a run either.** `compact`
  fires no `SessionEnd` at all. `fork` carries a fresh session id over copied
  history, the same non-continuation shape as `clear`.
- **Every `SessionEnd` seals its run, for every reason, byte-identically.**
  There is no suspended-session state and **no `session_suspended` wire
  value**; never describe a `SessionEnd` as anything but terminal.

**Two consequences a reader would otherwise discover the hard way.** Goal
alignment **resets on `/clear`** — a new run starts with no goal until its
first prompt, because to core it is an unrelated session — but **is carried
forward on `--resume`**: core seeds the continued run from the previous run's
latest prompt ([phase 13](../plans/260905-2345-unified-dev-event-plan/phase-13-core-goal-carry-forward.md)),
so the first tool call after a resume is judged against the goal you last
stated (a machine with `content_capture` off never stored a prompt to carry,
so such a run re-anchors on its first prompt as before). And a session
`--resume`d repeatedly produces several sealed runs chained under **one**
`openbox_session_id`; a session `/clear`ed repeatedly produces several
**independent sessions** instead, a fresh `openbox_session_id` each time with
no lineage between them. See [mapping.md §1](mapping.md#1-envelope-field-mapping-every-event)
for the field-level derivation.

## 1b. Model-call coverage matrix; per signal, per lane

Section 1 maps hooks, and none of the three model-call producers has one. This
table is what each *lane* sees of a single model call. It is deliberately not
averaged into a "model calls are governed" sentence: the three lanes differ in
what they carry, in who can suppress them, and in how strongly each is verified.

The `:gateway:` and `:proxy:` (transport) lanes are **Claude Code only**;
`init` installs neither for any other provider and prints why rather than
erroring (`laneCapable`, `cmd/openbox/initlanes.go`). The transport allowlist
is a per-provider host table's union — installing only `claude-code` covers
`api.anthropic.com` plus `claude.ai` and its subdomains, not one host —
feeding both the allowlist and a `GET /proxy.pac` endpoint the relay now
serves; the CA is unconstrained (owner ruling 2026-09-22). A machine still
holding an older, constrained CA keeps working — it tunnels rather than
intercepts any host it cannot mint for, and `openbox doctor` names those hosts
as a "legacy constrained CA" finding until it is re-issued, which a plain
`init` re-run now does automatically. See [Architecture](architecture.md)'s
decision record.

The `:otel:` (telemetry) lane now has a Codex arm too: `init --provider codex`
writes an OpenBox-owned `[otel]` block into `$CODEX_HOME/config.toml`
(`internal/adapters/codex/oteblock.go`) alongside installing the same receiver
Claude Code uses. A Codex record keys on `conversation.id` rather than
`session.id`, is signed and attributed as the `codex` tool rather than
`claude-code` (per-provider lane identity, `cmd/openbox/laneidentity.go`), and
honours Codex's own posture (its own `content_capture`, `telemetry`) from
Codex's own store, never Claude Code's. Codex's **proxy** arm (routing its
model calls through the transport relay) is not built or verified; `openbox
doctor` says so rather than reporting silence as coverage.

**Codex: surveyed 2026-09-17, and the mechanism exists — and the `:otel:` lane
is now built.** That sentence used to read "no probe has been run", and it is
no longer true, so what replaces it is what the probe actually showed rather
than a softer version of the same absence:

- Codex **does** read an `[otel]` block from `$CODEX_HOME/config.toml` and export
  OTLP over HTTP to whatever endpoint that block names. Measured on **both**
  surfaces: a loopback sink received six protobuf batches from a single terminal
  CLI turn (probe P0.3), and **twelve** from Codex Desktop relaunched under a
  throwaway `CODEX_HOME` (probe P0.3-Desktop). Both carried `service.name`,
  `conversation.id`, `app.version`, token counts and the `environment` value the
  config set; the Desktop capture additionally carried `codex.api_request`,
  `codex.sse_event`, `codex.tool_result` and `codex.user_prompt`, at
  `protocol = "binary"` — the production wire format this document records as
  unexercised for the Claude Code `:otel:` lane.
- **Built.** `openbox init --provider codex` stands up the same telemetry
  daemon Claude Code uses, proves the listener live, and only then writes the
  `[otel]` pointer into `config.toml` — install ordering stayed the safety
  property this section anticipated: writing the pointer first would point
  Codex at a dead port while `init` printed success. The merge is
  ownership-aware (an existing foreign `[otel]` block is left untouched) and
  `openbox uninstall` removes only the block it owns, splicing the file back to
  its original bytes rather than leaving Codex exporting to a dead port.
  Unverified still: Codex Desktop against this installed lane (the probe above
  used a throwaway `CODEX_HOME`, not an install), and the production
  `protocol = "binary"` wire path end-to-end through core.
- **Still unmeasured:** whether Codex Desktop runs `hooks.json` at all. The probe
  registered all eleven events and none fired, but the app was idle and wrote no
  rollout, so nothing reached a hook; and hook trust is a second confound. That
  is a genuinely open question, not a negative result — see
  `plans/260917-0225-codex-parity-with-claude-code/probes/` for exactly what
  would settle it.

**Cursor: no lane, and no probe has been run** — unsurveyed, not measured-empty.

| Signal | `:gateway:` | `:proxy:` (transport) | `:otel:` (telemetry) |
|---|---|---|---|
| Model **request** body (system prompt, full history, tool definitions) | ✅ captured | ✅ captured | ❌ **never**; this lane binds no content at all |
| Model **response** body | ✅ captured | ✅ captured | ❌ never |
| Request/response headers | ❌ **no longer emitted** | ❌ no longer emitted | ❌ |
| 4 token counts + model id | ✅ | ✅ | ✅ (its whole payload) |
| Credential fingerprint (one-way) | ✅ on `metadata` | ✅ on `metadata` | ❌ |
| `br` response body (the large majority of recorded responses) | ✅ decompressed in the capture path | ✅ decompressed | n/a |
| `gzip` response body (nearly all the rest) | ✅ decompressed in the capture path | ✅ decompressed | n/a |
| `zstd` / `deflate` response body | ⚠️ marker naming the encoding | ⚠️ marker naming the encoding | n/a |
| Relayed call **latency** (`duration_ms`) | ✅ measured to end-of-stream | ✅ measured | ✅ from the tool's reported `duration_ms` |
| Paired `ActivityStarted`/`ActivityCompleted` | ✅ | ✅ | ✅ |
| Token-count probe told apart from a completion, and then dropped | ✅ classified `token_count`, never spooled | ✅ classified `token_count`, never spooled | n/a; this lane sees no probes |
| Refuse a call on a verdict | ⚠️ written, **dormant** | ✅ **latch-only** (cross-lane HALT, no `/evaluate` round trip); server-verdict refusal still **dormant** (§5) | ❌ impossible; out of path |
| Terminal CLI | ✅ | ✅ | ✅ |
| **Desktop app** | ❌ measured-empty 2026-08-27 | ⬜ intended, **unconfirmed** on Linux/Windows; on **macOS**, ROUTED through the relay via the system PAC (P1) but produces **no governance event** (headerless call, no chat mapper yet) | ⬜ intended, **unconfirmed** |
| Subscription-OAuth session | ⬜ open | ⬜ intended, **unconfirmed** | ⬜ intended, **unconfirmed** |
| Suppressible by the governed tool | no; in path | no; in path | **yes**; the tool reports its own calls |
| Strongest evidence to date | socket-verified relay; no live stack | **replay** (bind-free) | **replay** (bind-free), intake's JSON path crossed by a synthetic export, **protobuf path; what production uses; untested** |

✅ shipped and exercised · ⚠️ present but not active · ⬜ intended, no evidence
either way · ❌ absent by construction

**The `:otel:` row that matters most is the content one.** This lane carries no
prompt, no completion and no body; only the model id, four token counts, a
duration and one request id (`internal/cli/telemetryemit/mapper.go`). **No
cost**: the server derives that from a model-keyed pricing table, and `turnFor`
never sets it. Its mapper takes **no redactor**, deliberately, because there is
nothing to redact; that is a correct design today and the thing to re-check
first if body ingestion is ever added. `OTEL_LOG_RAW_API_BODIES`, which would
make the client dump raw prompt and completion bodies to disk, is deliberately
**subtracted** from the key set the installer writes, so the lane creates no
liability it has no evidence to justify.

**"Suppressible" is the row that decides how much a reader may lean on a lane.**
Telemetry is the governed tool reporting its own calls, so it is suppressible by
the thing it observes; the weakest claim in the product. It is adopted because
it is the only lane that even attempts desktop and OAuth coverage today, and an owner decision
is the compensating control: telemetry silence on an otherwise-active session is
a **finding**, not an absence.

**The two ⬜ columns are the honest centre of this table.** Desktop and OAuth
coverage is the reason both lanes were built, and neither has been confirmed
against a real client; the desktop cell is intent, and only a run against a
live stack and a real desktop client can turn it into a measurement. Nothing in
this repository can. Do not read "built for it" as "covers it".

**On macOS, the desktop app and browser sessions are now ROUTED, but still not
RECORDED.** `openbox init` activates a system-wide PAC once the transport lane
is up (`cmd/openbox/systempac.go`, `internal/cli/activation/sysmacos.go`): it
sets the PAC URL on every enabled network service and trusts the relay's CA in
the System keychain. P1 measured that Claude Desktop and Chrome both follow
that PAC on macOS 26, so a CONNECT from either now reaches the relay rather
than going direct -- routing is no longer the gap it was. What is still
missing is the **recording**: the relay's capture path keys on a session-id
header (`X-Claude-Code-Session-Id`) the CLI sends and desktop/browser calls do
not, so those calls are relayed and TLS-terminated but never mapped to a
governance event -- an owner decision (plan `three-lanes-one-session`, item 7)
accepted this gap explicitly rather than blocking on it, pending the headerless
chat mapper. `openbox doctor` still cannot claim desktop coverage; it can now
only say the traffic reaches the relay, not that anything was recorded from it.

Linux and Windows keep the pre-existing gap in full: routing is **not
implemented** there either, deliberately rather than by omission -- their
P5-linux/P5-win probes have not run, so `openbox init` reports "not yet
supported on this OS in this build" and touches no OS-level proxy or trust
setting on either platform. How each browser/desktop stack resolves its proxy
settings and trust anchor there remains unmeasured, and `doctor` says
"unknown", never "covered", for both.

**Environment routing is also not durable against the tool that owns the file**,
and that too is now detected rather than assumed. Observed during one planning
session: `~/.claude/settings.json` carried OpenBox's whole `env` block at 00:15
and by 00:28 held only `hooks`, `statusLine` and `switchModelsOnFlag`, with no
removal run at all. The already-running CLI kept relaying, because environment
routing binds at process start, so the un-routing was invisible from inside the
session. `doctor` now compares each lane's activation record against the settings
file as it is and names any managed key that has gone missing or changed. This is
detection, not prevention: prevention belongs to MDM, exactly as the base
architecture already records.

**Routing integrity is a separate backend feature, and does not apply to a
developer session.** Routing integrity (backend `/routing-integrity/*`) is
OpenRouter-provenance only and answers `not_applicable` for developer
sessions; a dev model call carries model and token counts, never provider,
region, own-key or cost, so no routing promise exists to honour.

## 2. Field-derivation rules

- **`tool.kind`**: file tools (Read/Edit/Write, `beforeReadFile`,
  `afterFileEdit`, Codex `apply_patch`) → `file`; shell/Bash
  (`beforeShellExecution`, Codex `Bash`) → `shell`; MCP (`beforeMCPExecution`,
  `mcp__*`) → `mcp`.
- **`tool.mcp_server`**: Claude Code/Codex; parse from `tool_name`
  `^mcp__([^_]+)__`. Cursor; from the hook's `url`/`command`.
- **`span.file_path`**: Claude Code; `tool_input.file_path` (nested, not root).
  Cursor; `file_path` on file hooks. Codex; from `apply_patch` input.
- **`span.lines_count`/`bytes_*`**: derive from `edits[]` (Cursor) / tool
  response; often only available `PostToolUse`/`ToolResult`.
- **`tokens`/`model`**: gated by `ResolveFinops` (**default on** since that
  decision, opt out with `finops:false` / `OPENBOX_FINOPS=0`), off the hot path.
  **Claude Code is per turn**: `Stop`/`SubagentStop` → a
  `TurnStarted`/`TurnCompleted` pair with `activity_type: llm_completion`
  carrying all four counts plus the model id, plus the retained `SessionEnded`
  rollup. **Codex is per turn too**: its `Stop` hook fires once per turn and emits an
  `llm_completion` pair under `<session>:turn:N` whose counts are the delta
  between cumulative rollout snapshots; the `SessionEnd` rollup pair
  (`activity_id <session>:usage:rollup`) ships only for a session that emitted
  zero turns, so the same tokens are never counted twice. Both read a local
  file (CC's transcript, Codex's rollout JSONL), never the providers' OTel/Usage
  APIs, through an allowlist projection whose one egressing string is the model
  id ([INV-2](dev-event-contract.md#invariants)). `cost` is never **derived** here, the server derives it from a
  model-keyed pricing table, and the turn pair never carries it at all. The only
  way a `cost` can appear is if a transcript itself supplies one: CC's reader
  still reads `costUSD` onto the `SessionEnded` rollup, and current Claude Code
  transcripts do not carry that field (empirical, not structural). Codex's token
  path has no cost field at all. Cursor (unbuilt) has no per-turn source known:
  its Admin API is per-user hourly/daily, so a future adapter rolls up at
  agent/day granularity. `tokens?`/`model?` stay optional for exactly this
  reason.
- **`status`** (v1.2): derived **structurally**; from which hook fired, never
  parsed from tool output. **Claude Code**: `PostToolUse` → `completed`,
  `PostToolUseFailure` → `failed`. The two are mutually exclusive per call
  (documented by the provider, verified empirically on 2.1.229), which is what
  makes an unconditional `completed` on the success hook truthful.
  `metadata.is_interrupt` (tri-state `*bool`) separates a user cancellation from
  a real tool failure; both are `failed`. **Codex**: NOT reported. One
  `PostToolUse`, no failure hook, no exit code, no error flag; sending
  `completed` unconditionally would report success 100% for a session whose
  calls failed, which is worse than the honest 0% it replaces. Not content-gated
  on either provider.
- **Assistant turn text → `activity_output.content`** (v1.2; rehomed in v1.7 from
  `spans[0].response_body`, which the control plane parses and then discards, so
  the text was never stored anywhere at all): **Claude Code
  only**, from the `Stop`/`SubagentStop` payload field `last_assistant_message`;
  the provider's own recommended source, and the choice that leaves the
  transcript projection's allowlist untouched. Gate chain, all required:
  `finops` (turn events exist at all) ∧ the window carried usage ∧
  `content_capture` (checked twice; once by the mapper, once independently by
  the client's `stripContent`). Secret-redacted **before** attachment, then
  capped at 64KB. With `secret_detection:false` it egresses unredacted.
  **Codex**: the assistant-text field **does exist** on its hook surface —
  `last_assistant_message`, required on both `Stop` and `SubagentStop`, and
  observed carrying that turn's text on codex-cli 0.150.0-alpha.8 (phase 00
  probe P0.7). What is true is narrower and is a scope statement, not a surface
  limit: OpenBox does not wire `Stop` yet, so nothing reads the field, and the
  SessionEnd rollup shares a flush with `WorkflowCompleted`, which deletes the
  goal session — wrong granularity and racy ordering. So Codex sessions do not
  feed Goal Alignment **today**. Wiring `Stop` fixes both at once: it fires once
  per turn, well before SessionEnd, so the race disappears and the granularity
  becomes per-turn.
- **`error_type`** (v1.2): passed through an `enumOr` allowlist of the
  provider's own ten values. This is not decoration: `error` is the same JSON
  key on `StopFailure` (a closed enum) and `PostToolUseFailure` (free text a
  tool wrote), so one binding decodes both and the allowlist is what keeps the
  free text off the wire.
- **`ToolCall`↔`ToolResult` correlation**: carry the provider's `tool_use_id` in
  `metadata` (all three expose it) and set `span.invocation_id` from it; a local
  field that never egresses and keys the cross-process duration stash. The two
  halves pair on the wire by a shared `activity_id` (**no event carries a `span_id`
  any more, on any lane; see mapping.md §2**). Both shipped adapters correlate by
  id rather than by heuristic. A
  new adapter must also supply `span.operation_id` for any class it lets the
  gate escalate, or an approval cannot survive a retry; `activity_id` derives
  from it, and an approval granted against one activity cannot be consumed by a
  retry that addresses another; see mapping.md "Operation vs invocation
  identity".

## 3. Bounded non-goals (Phase-1 v1.0); documented, not gaps

The contract is honest about what it does **not** model in v1.0. None is
required for the Phase-1 goals (observe / finops / session→commit→deploy
lineage):

1. **Turn boundaries** (Cursor `stop`, Codex `Stop`) are **not** `SessionEnded`;
   they fire per agent-loop turn, and a session has many turns. Adapters must
   **not** map turn-stop → `SessionEnded`. (Historical note: Codex once had no
   session-end hook and its adapter synthesized one; `SessionEnd` is real as of
   0.145.0 and the synthesis is gone.)
2. ~~**Subagent lifecycle**~~; **retired as a non-goal in v1.2.**
   `SubagentStart` is wired and maps to `SubagentStarted` →
   `SignalReceived(subagent_started)`. The old reasoning, the tree is
   reconstructable from `agent_id` on tool events, held for a subagent that
   *does* something; a subagent that spawns and calls no tool left no trace at
   all. `SubagentStop` stays unwired as a lifecycle marker, because it already
   has a job: it closes a turn. Still not a `tool.kind`.
3. ~~**Compaction** (`PreCompact`/`PostCompact`); context-window infra; dropped in
   Phase 1.~~ **Retired as a non-goal in v1.8.** Both hooks are wired:
   `PreCompact` carries `trigger` and, under the content gate, the user's
   `/compact <instructions>` text; `PostCompact` carries `trigger` and the
   compaction summary. Compaction infra itself — what gets dropped and how —
   is still out of scope; only the two boundary signals are observed now.
4. **Assistant message/thought**, **retired as a non-goal: v1.2 for the
   completion text, v1.3 for tool content, v1.4 for thinking.** The completion
   text egresses for Claude Code in `activity_output.content`; **tool output, observe-path
   tool input, and the free-text failure detail egress as of v1.3**; **thinking
   egresses as of v1.4** in `activity_output.thinking`, all under the one
   `content_capture` gate, redacted before attachment and capped at 64KB.
   Thinking was the item that required amending the transcript
   allowlist and turning its load-bearing sentinel around; that amendment is
   written, and the sentinel is mutation-tested against the removal of either
   the redaction or the cap. What is still out of scope:
   - **Intermediate assistant text** from the transcript window. Only the final
     reply egresses, from the hook field v1.2 bound; the amendment
     authorised `thinking` and nothing else in that array.
   - **`redacted_thinking` blocks**; provider-encrypted base64. Excluded by the
     block-type filter, deliberately: it would spend the 64KB budget on
     ciphertext no reader can use.
   - **Cursor and Codex** have no equivalent wired at all. Codex binds no
     `tool_response` field (`internal/adapters/codex/hookevent.go:73`, pinned by
     `internal/adapters/codex/hookevent_test.go:50`) and reads no transcript
     thinking, so closing either for Codex is adapter work, not a contract
     change. **State the asymmetry rather than averaging it, and note that every
     version since v1.3 has widened it:** a Claude Code session and a Codex
     session on the same org, same posture, send different amounts of content;
     Claude Code sends tool input, tool output, the failure detail and the
     turn's thinking, and with the opt-in gateway the whole model request and
     response as well; Codex sends none of them.
   - **The asymmetry is also about redaction, not only volume, and that
     direction is the dangerous one.**
     `internal/adapters/claude-code/hookrun.go` wires a redactor onto the mapper
     as a collaborator, so every content field it attaches is scanned by
     construction. **The Codex mapper had no such field to wire** until
     2026-09-17: its local redaction existed only on the enforce path, over an
     `apply_patch` body, so the prompt — the only content class it egresses —
     was sent **unscanned even with `secret_detection` on**.

     This is the second time the same shape shipped. The Claude Code prompt had
     exactly this gap until 2026-08-26, and it survived review because that one
     field was assigned directly instead of through the collaborator. Codex then
     repeated it, for the same reason, and it survived for the same reason. The
     history is the point: *assigned directly* is the smell, and a redactor that
     is a mapper collaborator is the fix, because a second caller of `Map`
     inherits it and a call-site redactor does not.

     **Closed 2026-09-17.** The Codex mapper now carries `RedactContent` with
     the Claude Code nil-is-identity contract, and the prompt is routed through
     it before attachment. Held by
     `internal/adapters/codex/prompt_redaction_test.go`, which asserts the same
     triple conformance C42 asserts for Claude Code — secret absent, placeholder
     present, surrounding prose intact — on the spooled record and again on the
     outbound bytes, plus the honest opt-out (detection off ⇒ verbatim) and
     `TestMapper_RedactionIsStructural`, which fails if a future change moves
     redaction back to the call site.

     The volume asymmetry widened with a dependency decision (2026-08-28): the scanner
     Claude Code's content passes through gained gitleaks' 222 format rules on
     top of the nine hand-rolled ones. Both providers' content now passes the
     same 231 formats; what still differs is how many classes each sends. A dedicated
     `CompletionReceived` type was the v1.1 candidate here; it was not built,
     because the alignment reader that needs the text keys on the activity
     fields of an existing `Activity*` row rather than on an event type of its
     own.
5. **Non-session telemetry** (Cursor Tab hooks, `workspaceOpen`, cloud-agent
   sessions that never emit `sessionStart`); the contract requires
   `openbox_session_id`, so events with no resolvable session are **not
   emittable** (adapter drops or synthesizes a session). Honest degradation; the
   architecture's "no false coverage" rule.
6. **`PermissionRequest` vs generic `preToolUse`/`PreToolUse` overlap**;
   adapters emit **one** `ToolCall` per tool invocation (prefer the specific
   pre-tool hook); `event_id` idempotency (INV-5) also guards double-counting.
7. **`WorktreeCreate` is refused, not missing.** `claude --worktree`,
   `isolation:"worktree"` subagents and background sessions rely on the
   hook's stdout **last line** being the created worktree path; configuring
   the hook replaces the default git behaviour, so a registered observer that
   emits nothing would break worktree creation outright. This adapter
   deliberately does not register it. **The ceiling is 32 of 33 documented
   Claude Code hooks, and it is a choice, not a gap.**
8. **`FileChanged` is a bounded watch list, not coverage.** It fires only for
   `.env|.envrc|.mcp.json|CLAUDE.md` — literal basenames, no wildcard, no
   user-configurable extension. A change to any other file produces no event,
   and the event it does produce never carries the file's contents.
9. **`MessageDisplay` is structural-only.** `delta`/`displayContent` are never
   bound (D1); the event carries `turn_id`, `message_id`, `index`, `final`
   only, and `message_id` is **not** the API's `msg_…` transcript id — there is
   no join from this event back to a specific assistant message.
10. **Headless coverage is best-effort, not equivalent to interactive.** In
    `claude -p`, Claude Code kills any async hook still running at process
    teardown and finalizes it `cancelled` with no error surfaced anywhere.
    **15 of the 21 new classes are async** (`Setup`, `InstructionsLoaded`,
    `UserPromptExpansion`, `MessageDisplay`, `PostToolBatch`, `Notification`,
    `TeammateIdle`, `CwdChanged`, `DirectoryAdded`, `FileChanged`,
    `WorktreeRemove`, `PreCompact`, `PostCompact`, `PreModelSwitch`,
    `PostModelSwitch`) and can be silently lost this way in a short headless
    run. The **six** sync new classes (`PermissionRequest`, `TaskCreated`,
    `TaskCompleted`, `ConfigChange`, `Elicitation`, `ElicitationResult`),
    alongside the 11 original sync hooks, cannot be lost to teardown the same
    way, because the process does not exit until they return. **Do not read
    headless coverage as equivalent to interactive coverage.**
11. **The dark-install window (D8).** These 21 registrations, like the four
    lifecycle/failure hooks before them, reach an existing install only when
    `openbox init` is re-run on that machine. Nothing in this landing
    schedules that re-run: a machine that upgrades the `openbox` binary
    without re-running `init` keeps observing only its pre-upgrade hook set
    until a human runs it.

Items 1 and 2 were the candidate scope for a `schema_version` bump, and both
have since landed; turn boundaries in v1.1, subagent start in v1.2, tool content
in v1.3, thinking in v1.4. Item 4 is closed for Claude Code; what remains open
there is **provider parity**, which is adapter work, not a contract question.
The `schema_version` field exists precisely so each bump stays non-breaking;
v1.2, v1.3 and v1.4 are all purely additive, and with content capture off all
three send byte-identical payloads.

## 4. Enforcement posture

All three tools have blockable hooks (Claude Code fail-controlled; **Cursor
fail-open** by default, `failClosed:true` to flip; Codex feature-gated
`features.hooks`, stable and on by default ≥ 0.145.0).

Enforcement **shipped** in Phase-2 (E6 for Claude Code for Codex) and is
**unconditional**; `enforce`/`OPENBOX_ENFORCE` still parse (so `openbox doctor`
can name them as ignored) but no longer opt out, and there is no
`OPENBOX_ENFORCE=false` session left that treats every verdict as advisory
([INV-3](dev-event-contract.md#invariants) is unaffected for a hook class the
adapter never gates at all). One bound still holds: enforcement is inert
until the org publishes a policy. Delivery is
unconditionally fail-closed too -- `fail_closed` still parses but no longer
selects anything, and an unaccepted event halts its run rather than proceeding.
The verdict itself is the server's; nothing local decides one. What the hook
does in-process (its no-sidecar shape) is apply it,
**tighten-only**; it never turns a provider's own deny into an allow, and the
one `allow` it may emit rides a redacting rewrite, never a grant.

`REQUIRE_APPROVAL` is held for a real decision on both providers, and denies
with the approval reference in the reason if it goes unanswered or is refused.
Neither surface renders `ask`: on Claude Code that is a deliberate refusal to
show the provider's own prompt, which would ask the developer to approve their
own filed request; on Codex the hook parser has no such verb either way (an owner decision; a fallthrough
under `approval_policy=never` would auto-run ungoverned, so deny is the safe
mapping). The deny-and-retry approval design that makes this a real four-eyes
control rather than self-approval is described in `docs/architecture.md` §
Approvals.

**Assurance caveat:** all of the above is enforced by a *user-local* hook. Until
the managed provider config is deployed (an earlier decision), a developer can remove the
hook or flip the local config, so treat local enforcement as prevention
**without** assurance. That is a deployment property, not a code gap.

**`ConfigChange` narrows that caveat, without closing it.** The settings file
that houses this adapter's own hook registrations now fires `ConfigChange`
when a developer edits it, so removing or disabling the hook is itself an
observed — and, when policy says so, blockable — event, rather than an
invisible one. Two operational facts keep this narrow, and conflating them
overstates the control: `source:policy_settings` is **never gated** — the
provider documents that path as unblockable, and gating it anyway would file
an audit line claiming a block that never happened — and settings the
provider manages centrally never fire the hook **at all**, a second, disjoint
unblockable path. A denial is announced on stderr (the same
`{"decision":"block","reason":…}` line a tool denial gets), recorded in
`enforcements.jsonl` with `tool_kind:"config"`, a timestamp and the verbatim
reason, and surfaced in `openbox doctor`'s recent-config-denials section —
because the provider surfaces nothing itself. The developer's settings file
remains owner-writable throughout, and attestation over it proves origin of
config, never tamper resistance; the caveat above still stands, only
narrower.

**V13 (required wording).** A `REQUIRE_APPROVAL` verdict on `ConfigChange`
cannot be held — a signal row has no `activity_id` for an approval to attach
to — so the edit waits the approval budget (~30s) and is then denied, exactly
as `UserPromptSubmit` behaves today. Write ALLOW or HALT rules for config
changes, not approval rules.

**V14 (the HALT latch across a continue).** The local HALT latch is scoped to
the **run**, like core's session row: a `/clear` or `--resume` opens a new run
and starts unlatched, and core re-evaluates that run's first gated call
against the same policies, so a policy that halted the previous run halts
this one on the same condition. This is not "clearing" a halt — nothing is
removed; the new run simply has no latch of its own yet.

## 5. Evidence: what is proven, and by what

The matrices above say what each provider *supplies*. They do not say what
proves any of it works, and for most of this document's life nothing did: across
§1's rows exactly one cited a test, as prose inside a cell.

This section is that axis. It follows the same epistemics as the per-hook marker
above — **an uncited claim is downgraded**, and a class is never inferred.

**The ladder.** It is the repository's own rule made countable: asserting a
struct is not asserting the wire, and asserting the wire is not asserting the
receiving type.

| Class | Means |
|---|---|
| **E0** | prose. No test. |
| **E1** | a unit fixture: a Go struct, an in-process value, a builder called directly, or a file read locally. |
| **E2** | exercised end to end against a fake control plane, **and graded on the artifact the claim is about** — the captured outbound request body for a claim about what is *reported*, the rendered decision or the on-disk state for a claim about what the binary *did*. |
| **E3** | needs a live stack. **Nothing in this repository can reach it.** |

Two things follow from the ladder that are easy to misread. A real HTTP round
trip does not by itself make a claim E2: a test that round-trips and then
inspects only a local struct is still E1. And E2 is the ceiling here — with the
shell suite retired, **nothing in this repository observes the far end of the
wire**. Every E3 row below is printed rather than omitted, because a claim with
no evidence is the one a reader most needs to see.

### 5a. Lifecycle claims (§1)

| Claim | Class | Owner |
|---|---|---|
| `SessionStarted` reaches the wire | E2 | `cmd/openbox/main_test.go` · `TestHookEndToEndSmoke` |
| `PromptSubmitted` reaches the wire, redacted | E2 | `internal/adapters/claude-code/content_conformance_test.go` · `TestContentCaptureConformance` |
| `ToolCall` maps to `ActivityStarted` | E2 | `internal/adapters/claude-code/conformance_parity_test.go` · `TestWire_ToolEventsAreActivityPairs` |
| `ToolResult` maps to `ActivityCompleted` | E2 | `internal/adapters/claude-code/conformance_parity_test.go` · `TestWire_ToolEventsAreActivityPairs` |
| `SessionEnded` reaches the wire | E2 | `internal/adapters/claude-code/usage_test.go` · `TestFinops_NoContentOnWire` |
| `CommitCreated` payload shape | E1 | `internal/client/payload_lifecycle_test.go` · `TestLifecycle_CommitLineageSurvivesSignal` |
| `CommitCreated` is emitted when a commit happens | **E0** | **nothing emits it, by design.** The type is reserved -- the wire mapping exists and no adapter produces it (`internal/client/event.go`). A commit's binding is resolved server-side at push against the real pushed SHA, because git hooks are local and never travel; the lineage that does reach the wire is `Deploy`, from the git action. Not a missing test. |
| `Deploy` reaches the wire with its metadata | E2 | `internal/actions/openbox-git-action/deploy_wire_test.go` · `TestDeployProjectsItsWholeMetadataIntoSignalArgs` -- posts through the real client to a fake core and grades the captured body |
| `Deploy` payload shape | E1 | `internal/client/payload_lifecycle_test.go` · `TestLifecycle_DeployLineageSurvivesSignal` |
| `SubagentStarted`, `PermissionDenied`, `APIError` reach the wire | E2 | `internal/adapters/claude-code/enforce_conformance_test.go` · `TestEnforcementConformance` |
| The 21 v1.8 signal classes each carry `signal_args` and no `activity_id` | E2 | `internal/adapters/claude-code/content_conformance_test.go` · `TestContentCaptureConformance` |
| The same 21 classes map correctly in process | E1 | `internal/adapters/claude-code/mapper_signals_test.go` · `TestMap_21SignalClasses` |
| `ConfigChange` is gated, and `source:policy_settings` never is | E2 | `internal/adapters/claude-code/configchange_test.go` · `TestRunHook_ConfigChange_PolicySettingsNeverGated` |
| Codex's five core types reach the spool from the real binary | E1 | `cmd/openbox/main_test.go` · `TestCodexUnifiedBinaryObserveE2E` |
| Core accepts, stores or deduplicates any of the above | **E3** | needs a live stack. Since the shell suite was retired this is owned entirely by the closed side. |

### 5b. Governance claims (the evals)

Named by claim, and enumerable: `go test -run TestGovernanceEval -v ./cmd/openbox/`.

| Claim | Class | Owner |
|---|---|---|
| A call that ran is reported exactly twice; one blocked before it ran, exactly once | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEval` |
| Pairing survives a retry that repeats the same arguments | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEval` |
| A declared denial must agree with what the binary rendered | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEvalTheWitnessMustBeCorroborated` |
| Nothing is lost between stdin and the wire | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalGraders` |
| A call's label is the tool that was invoked, on both halves | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalGraders` |
| One call, one row of each kind | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalGraders` |
| Each verdict reaches its own local effect | E2 | `cmd/openbox/governanceeval_verdicts_test.go` · `TestGovernanceEvalVerdictBranches` |
| A HALT refuses the rest of the run without asking again | E2 | `cmd/openbox/governanceeval_verdicts_test.go` · `TestGovernanceEvalHaltLatchesTheRestOfTheRun` |
| An approval unanswered denies, rejected denies, granted proceeds | E2 | `cmd/openbox/governanceeval_verdicts_test.go` · `TestGovernanceEvalApproval` |
| The failure policy runs after the evaluation, never before | E2 | `cmd/openbox/governanceeval_verdicts_test.go` · `TestGovernanceEvalFailClosed` |
| A gate outage reports the call exactly once, not twice and not never | E2 | `cmd/openbox/governanceeval_verdicts_test.go` · `TestGovernanceEvalGateOutageReportsTheCallOnce` |
| Content leaves only when capture is on, and only in a content field | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalContentGateBothDirections` |
| A secret is rewritten before it reaches either the disk or the wire | E2 | `cmd/openbox/governanceeval_graders_test.go` · `TestGovernanceEvalRedactionRunsBeforeAttachment` |
| A wrongly signed request is refused, and the event is not discarded | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEvalRejectsAWrongKey` |
| The spooled event and the wire body have separate validators | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEvalTwoObjectsTwoValidators` |
| Every grader can actually fail | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEvalMutations` |
| A renamed or wrong-typed `metadata` key is caught | E1 | `internal/conformance/conformance_test.go` · `TestInvalidSamplesRejected` |
| A turn's thinking is gated with the rest of the content | E1 | `internal/adapters/claude-code/content_conformance_test.go` · `TestContentCaptureConformance` |
| A fresh session's first core row is `WorkflowStarted`, `claude-code` and `codex` alike | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEval`; `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEvalCodexTwinOrdersTheSameWay` |
| A live binary reaches core with `WorkflowStarted` first even under a slow, real flusher (20/20) | E2 | `cmd/openbox/main_test.go` · `TestSessionStartReachesCoreFirstWithALiveFlusher` |
| A lane record and a gated hook event order through the same session spool by append order | E2 | `cmd/openbox/session_order_eval_test.go` · `TestLaneRecordAndAGatedHookOrderThroughTheSameSpool` |
| `claude -p`'s headless run opens on the prompt itself, with no `SessionStart` ahead of it | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEvalHeadlessPromptOpensTheRun` |
| Core down at session start denies the first gated call, latches the run, and a NEW session after recovery is unaffected | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEvalCoreDownAtSessionStartHaltsAndRecovers` |
| Every delivery-failure class (network, 5xx, 401, token-exchange-failure, 4xx) is exactly one attempt and latches the run | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEvalOneAttemptPerFailureClass` |
| The timeout delivery-failure class is exactly one attempt and latches the run, through the real flusher/lane-drain wiring | E2 | `internal/adapters/common/hookflow/deliveryhalt_test.go` · `TestNewEngine_TimeoutClassLatchesInExactlyOneAttempt` |
| A new run after a halt (CC `/clear`, `--resume`) is unhalted; a Codex resume of a halted session stays halted | E2 | `cmd/openbox/session_order_eval_test.go` · `TestGovernanceEvalNewRunAfterAHaltIsUnhaltedButCodexResumeStaysHalted` |
| `enforce:false`/`OPENBOX_ENFORCE=false` are ignored; every gated class still gates | E2 | `cmd/openbox/governanceeval_scenarios_test.go` · `TestGovernanceEvalEnforceIsIgnoredButEveryGatedClassStillGates` |
| A gated verdict renders within budget under a 50-record backlog and a cold token | E2 | `cmd/openbox/session_order_eval_test.go` · `TestGatedVerdictWithinBudgetUnderBacklog` |
| A gate never waits past `MaxStripeWait` for a busy stripe, but an uncontended backlog still drains past it when slack allows | E2 | `cmd/openbox/session_order_eval_test.go` · `TestGatedVerdictWhileAFlusherHoldsTheStripeOnASlowEvent`; `internal/adapters/common/hookflow/gate_test.go` · `TestGate_StripeHeldPastMaxStripeWaitSkipsTheDrainAndEscalatesPromptly`; `internal/adapters/common/hookflow/gate_test.go` · `TestGate_UncontendedBacklogStillDrainsPastMaxStripeWaitWhenSlackAllows` |

**The graders behind those rows**, each a predicate that returns reasons rather
than a verdict, and each executed against its own deliberate mutation so that it
has been watched failing: `pairing`, `completeness`, `activity-type`,
`delivery-once`, `signal-args`, `content-gate`, `redaction`, `start-first`,
`one-attempt`, `halted-after-failure`. Two further graders, `reference/parity`
and `reference/exempt-all`, are the *wrong* answers kept executable — they are
not evidence, they are what proves the fixtures still tell the two hard cases
apart. `one-attempt` and `halted-after-failure` are registered with no
`Mutate` (like `redaction`): no sequence of distinct native hook payloads can
produce a same-key double-send (claude-code's own mapper timestamps each
event fresh per invocation, INV-5) or un-halt a healthy binary mid-run: each
is proven red directly, against a hand-built `Run`, in
`internal/client/fakecore/graderreasons_test.go`'s own table instead.

Thinking sits at E1 on purpose: reaching it needs a `transcript_path` fixture
the native hook payload has no slot for, so no scenario here can produce one.

### 5c. Model-call lane claims (§1b)

The lane table's claims split by lane, so the class does too. Two structural
facts shape almost every row:

- **`internal/gateway`'s emitter is an in-process interface**, so no test in that
  package constructs a client and a fake control plane. Its capture,
  decompression and latency tests are therefore E1 however thorough they are —
  and `:proxy:` reuses that exact relay (`TestProductionHandlerIsTheGatewayRelay`),
  so the same evidence covers both lanes and neither reaches the wire through it.
- **The only lane evidence graded on a wire body lives in `internal/cli/gatewayemit`**,
  whose tests are parametrized over the lanes.

| Claim | `:gateway:` | `:proxy:` | `:otel:` | Owner |
|---|---|---|---|---|
| Request / response body captured | E2 | E2 | E1 | `cmd/openbox/transportwire_test.go` · `TestSpooledTransportEventReachesTheWire` — drives a model call through the real CONNECT and TLS chain and reads both bodies off the captured wire body; `internal/cli/telemetryemit/sentinel_test.go` · `TestContentFieldsAreUnsetOnTheEvent` |
| A tool-telemetry call egresses no body | E2 | E2 | n/a | same test. Classified as carrying no content at the relay and again at the emitter, so both gates would have to stop agreeing |
| Headers are no longer emitted | E1 | E1 | E1 | `cmd/openbox/transportcapture_test.go` · `TestTransportLaneRecordsThroughTheRealChain` |
| Four token counts and the model id | **UNOWNED** | **UNOWNED** | E2 | `internal/cli/telemetryemit/sentinel_test.go` · `TestNoContentOnWireAtEitherPosture`. Neither in-path lane sets these fields at all, so there is nothing to own |
| Credential fingerprint, one-way | E1 | E1 | E1 | `cmd/openbox/transportcapture_test.go` · `TestTransportLaneRecordsThroughTheRealChain` — checked both directions: the fingerprint is present, and the raw spooled bytes never carry the key |
| `br` response body decompressed | E1 | E1 | n/a | `internal/gateway/decodebody_test.go` · `TestBrotliResponseRelaysCompressedAndCapturesDecoded` |
| `gzip` response body decompressed | E1 | E1 | n/a | `internal/gateway/decodebody_test.go` · `TestGzippedResponseRelaysCompressedAndCapturesDecoded` |
| `zstd` / `deflate` yield a marker naming the encoding | E1 | E1 | n/a | `internal/gateway/decodebody_test.go` · `TestUnknownContentEncodingYieldsAMarkerNamingIt` |
| Relayed call latency | E1 | E1 | E1 | `internal/gateway/capture_test.go` · `TestCompleteCarriesTheMeasuredCall`; `internal/cli/telemetryemit/mapper_test.go` · `TestDurationDerivesTheTurnWindow` |
| Paired `ActivityStarted` / `ActivityCompleted` | E2 | E2 | E1 | `internal/cli/gatewayemit/lane_test.go` · `TestLaneNamesMatchTheActivityIDNamespaces` |
| A token-count probe is told apart and dropped | E2 | E2 | n/a | `internal/cli/gatewayemit/pathclass_test.go` · `TestAProbeIsClassifiedAndNotSpooled` |
| A run any lane latched HALTed is refused locally on the next relayed call, no server evaluator yet | E0 | E1 | E0 | `cmd/openbox/crosslanehalt_test.go` · `TestHaltDecoratorRefusesALatchedRun`, `TestWithGateWiredIntoTheRelayRefusesBeforeTheDial`; `internal/transport/proxy_test.go` · `TestProxyGoNeverRendersARefusalItself` |
| The three lanes never share an `activity_id` | E2 | E2 | E2 | `internal/cli/gatewayemit/lane_test.go` · `TestTheLanesAreDisjoint` |
| Terminal CLI / desktop / OAuth coverage | E0 | E0 | E0 | field observation, not a test. §1b's two ⬜ columns are the honest centre of that table |
| Core accepts, stores or deduplicates any of it | E3 | E3 | E3 | needs a live stack |

**Two gaps this axis made visible, neither of them new, both previously
unsayable.**

*The proxy lane's bodies are now graded on the wire* — that gap is closed. What
replaced it is narrower and worth naming: the body-capture predicate the binary
installs (`cmd/openbox/transport.go`) is still unexercised, because it is a
redundant outer gate. A nil predicate captures everything at the relay, and the
emitter classifies the path a second time when it builds the event, so removing
the predicate changes nothing observable. Measured, not inferred: with the
option dropped, a tool-telemetry call still spools no body. Four lines of
production wiring that no test can currently tell apart from its own absence.

*The four token counts and the model id have no owner on either in-path lane*,
because neither sets them: only the telemetry lane, which is the governed tool
reporting its own call, carries them. That is a property of the design rather
than a missing test, and the row says so instead of reading as an oversight.

**On the refusal row.** The proxy lane's gate is no longer dormant:
`transport.WithGate` (`internal/transport/proxy.go`) forwards to
`gateway.Gateway.WithGate`, and `cmd/openbox/transport.go` wires a
`haltDecorator` behind it that answers HALT from the cross-lane latch
(`hookflow.SessionHalted`) with no `/evaluate` round trip. `TestHaltDecoratorRefusesALatchedRun`
and `TestHaltDecoratorDoesNotRefuseAContinuedRunThatDidNotHalt` exercise the
decorator's own resolution directly (a local fixture, E1); `TestWithGateWiredIntoTheRelayRefusesBeforeTheDial`
drives a real relay end to end and asserts the refusal happens before the
upstream dial. `TestProxyGoNeverRendersARefusalItself` (successor to the
prior dormancy check, `TestTheGateIsNotWired`) keeps the boundary the
rewrite left in place: `proxy.go` may wire `WithGate`, but `Decide` /
`WriteRefusal` / `RefuseEverything` stay inside `gateway.Gateway.ServeHTTP`,
the one place they were reviewed — a static check over the source, still E1
by the ladder, still stronger than a sampling test for that particular shape
of claim. What remains genuinely absent, and still owned by the `:gateway:`
and `:otel:` E0s beside it, is a *server-verdict* refusal on this lane
(not built) and any refusal concept at all on the receive-only OTel lane.

**What none of these prove.** That the control plane accepts the wire, stores a
row, or keeps two rows apart; that a socket binds, that TLS terminates against a
real listener, that the OTLP protobuf path decodes, or that an attestation
verifies. Each is E3 and each is now owned entirely by the closed side.
