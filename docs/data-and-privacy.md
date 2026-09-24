# Data and privacy

What leaves the machine, what never does, and the one setting that changes it.

## The short version

| | Sent to OpenBox | Notes |
|---|---|---|
| Session, tool and MCP **metadata** | always | tool name, kind (`shell`/`file`/`mcp`), file path, MCP server + tool name, timing |
| **Token counts and the model id** | yes, by default | per model turn. `finops: false` turns it off; see [Usage capture](#usage-capture) |
| **Prompt text** | yes, by default | on Claude Code, scanned locally for secrets and redacted first, like every other body on this table; **on Codex it is not**; that adapter has no redactor on the content it sends. `content_capture: false` turns it off |
| **A machine-injected prompt's text** (a task notification arriving on `UserPromptSubmit` instead of typed input) | **narrowed: metadata only, never as the goal** | `signal_args` is withheld entirely, not emptied, so it cannot overwrite the session goal; `metadata.prompt_source` (the vendor's value, or `task_notification` when inferred from shape) still ships. A real prompt that merely quotes a notification mid-text is unaffected and keeps its goal |
| **The assistant's reply text** | yes, by default | **this changed**; one message per model turn, scanned locally for secrets and REDACTED first, truncated at 64KB. Same `content_capture` switch. See [What a model turn sends](#what-a-model-turn-sends) |
| **The assistant's thinking** | yes, by default | **this changed**; extended-thinking text; every thinking block of a turn, concatenated in file order, so one FIELD per turn rather than one block; scanned locally for secrets and REDACTED first, truncated at 64KB. Same `content_capture` switch. This captures more than Anthropic's own telemetry will: their OTel export redacts thinking unconditionally. See [What a model turn sends](#what-a-model-turn-sends) |
| **Shell command text** | yes, by default | **this changed**; it used to ride a *gated* call only; it is now on ordinary tool telemetry too, under the same `content_capture` switch. Redacted and truncated like every body |
| **File contents** (a Write/Edit body) | yes, by default | **this changed twice**; first onto gated calls, now onto ordinary tool telemetry as well. Scanned locally for secrets and REDACTED before it is sent, and truncated at 64KB |
| **File contents** (a file you read) | yes, by default | **this changed**; a read's arguments and its result both ride the tool event now |
| **Tool and MCP output** | yes, by default | **this changed**; what a tool printed or returned, including a failed tool's error text. Redacted and truncated the same way |
| **Why a tool was refused** | yes, by default | **new**; the classifier's reason on a denial, and the provider's detail on a failed turn |
| **Your provider account's email** | yes, if you are signed in | **new**; one field per session, read from Claude Code's own local account record. This is PII, and it egresses as governance evidence like your DID. Not gated by `content_capture`: it is attribution, not content. See [Account attribution](#account-attribution) |
| **Your provider organization's UUID** | yes, if you are signed in | **new**; same source, same session field |
| **Your provider organization's NAME, role, tier, billing** | **never** | all four sit in the same local file beside the two rows above, and none of them is sent. The evidence scope is org UUID + email, deliberately |
| **Model-call request and response bodies** | only with an **in-path lane** running (gateway or transport), and only when the call names a session | **new**; and it is a **selection** of the request rather than the whole of it. What is stored: the model id, a ~4KB head of the **system prompt**, and the **newest messages** of the conversation that fit a 48KB budget -- newest first, so what changed since the last call is what survives. Tool definitions are dropped entirely, older messages are dropped once the budget is full, and a trailing element that is not a conversation turn -- the agent runtime appends a `role:"system"` token counter, which was the newest element on 67% of measured calls -- is dropped so the newest stored element is a real turn. The stored document records how many of each of the three went, each under its own key, and never drops all of the history: a history that is entirely non-turn keeps its newest element as it is. Plus the model's response, which is kept from its start. This is still the largest content class OpenBox collects, and the system prompt and recent conversation are the sensitive part of it. Three bounds apply and all three are fallible: the `content_capture` switch, local secret redaction before anything is attached, and a 64KB cap. A body the provider sent **compressed is decompressed in the capture path** so redaction can inspect it before attachment: `gzip` and `br` are decoded, which between them is every encoding observed in the recorded corpus. An encoding outside that set (`zstd`, `deflate` -- advertised by the tool, returned by no provider) stores a marker naming it instead. Same session caveat as the row below |
| **Model-call HTTP headers** | **never** | they no longer leave the machine at all. The relay still redacts the credential headers by name locally, and the local gateway still reads two of them to attribute a call, but no header reaches the control plane under any posture. A relayed call that carries no `x-claude-code-session-id` header is recorded NOWHERE; the gateway declines to invent a session, so this is a real gap in the record rather than a silent attribution |
| **A one-way fingerprint of your provider credential** | only with an **in-path lane** running (gateway or transport), and only when the call names a session | **new**; a truncated SHA-256, so OpenBox can tell WHICH registered credential made a call without holding it. Not gated by `content_capture`: it is the account-binding control, and a privacy switch that removed it would let an org opt out of being identified |
| **Credentials** | **never** | they stay on your machine; in a plaintext file readable by you, see [Where credentials live](credentials-and-secrets.md#where-credentials-live). The gateway relays yours to the provider byte-for-byte and stores none of it |
| Git **commit trailer** and signed attestation | yes | commit sha, tree sha, session id; no diff, no file content |
| **Slash-command expansions** | **never** | `UserPromptExpansion` is structural-only; the typed `/command` itself already ships as an ordinary prompt |
| **What a tool was asked to do when permission was requested** | yes, by default | for an `Agent` request this is the **whole subagent prompt**, under `requested_tool_input` — a second surface for the same widening as the subagent-prompt row below |
| **Desktop and terminal notification text** | yes, by default | `Notification`'s `notification_message` |
| **Task titles** | yes, by default | `TaskCreated`/`TaskCompleted`'s `task_subject`; the **description is never sent** |
| **Your `/compact` instructions and the compaction summary** | yes, by default | `PreCompact`/`PostCompact` |
| **MCP elicitation prompts and your answers to them** | yes, by default | see the residual-risk paragraph under [Content capture](#content-capture) — a credential typed into an unrecognised form field is invisible to the redactor |
| **Assistant message text as it is displayed** | **never** | `MessageDisplay` ships identifiers and counts only (`turn_id`, `message_id`, `index`, `final`); it fires **per batch of streamed lines** — the provider forces synchronous execution for this one hook — not once per message, so a single reply can produce several rows; never content, but the row count is not what a first read would assume |
| **Tool inputs and outputs inside a tool batch** | **never** | `PostToolBatch` ships tool-use ids and a count only |
| **Paths that changed, directories added, worktrees removed, your working directory** | yes, always | structural, not gated; `FileChanged` reports that a file changed, with its path, and **never its contents** — including for `.env` |
| **That you `/clear`ed or resumed, and which run preceded which** | yes, always | `run_generation` and `continued_from_run_id` are **identifiers** — a small integer and a minted id, structural like `run_id` itself — so they are **not** under `content_capture` and there is nothing to opt out of. They reveal that a session was restarted and in what order, **not** what was cleared |
| **The prompt you gave a subagent** | **yes, by default** | the whole `Agent` `tool_input`, redacted then capped, under `content_capture`. Reverses the earlier "the prompt stays unread" position, which was about the metadata layer and stays true there |

The rule behind the table: content is gated at one choke point in the client, so
a new field cannot start egressing by accident. Structural identifiers (paths,
tool names, MCP server names) are metadata and always flow; bodies are content
and do not.

A tool call used to be reported as a hand-built telemetry span, and that span
had two fields, a request body and a response body, which *could* have carried a
tool's input or output text. Nothing ever put anything in them, and the span was
removed from tool events entirely. What a completed tool call reports instead is
counts, bytes read, bytes written, lines changed, and an exit code if the tool
provides one, plus whether it **succeeded or failed**. Never the output itself.

**A model turn carries content, and it is stored.** This page has said two
different things here over time, and both are now wrong. It first said the
response-body channel "cannot be re-opened by an adapter mistake plus a
content-capture opt-in"; it then said a model turn carries exactly one span whose
response body is the assistant's reply.

The honest version: **nothing carries a span any more.** A model turn's content --
the assistant's reply, its extended thinking, and on a relayed call the provider's
own request and response -- rides `activity_output` and `activity_input`, the
fields the control plane stores in dedicated columns. The span it used to ride was
parsed and then discarded on arrival, so for the whole time this page described
that carrier, the content was evaluated and retained nowhere.

That makes this a real increase in what is kept, not a relabelling, and the bounds
are unchanged: the `content_capture` switch, local secret redaction before the text
is attached, and a size cap. Tool calls still carry no bodies at all beyond the
tool input and output described above.

**A second body-carrying class appears once an in-path lane runs**, and it is a
larger widening than the first. The lane observes real HTTP exchanges, so its
`activity_input` carries a **selection** of the request the tool sent the model
-- the model id, a head of the system prompt, and the newest conversation turns
that fit the budget, with tool definitions dropped entirely -- and its
`activity_output` carries the model's reply. Unlike the turn's own content, this
is a genuine measurement of bytes on the wire rather than a re-reading of a
transcript. It is bounded by the same three mechanisms, it exists only
while the lane is running, and it records nothing at all for a call that
names no session. Two further limits on it, both deliberate: a body the provider
sent under an encoding this relay cannot decode is **not captured at all**, a
marker naming that encoding is stored instead, because compressed bytes are
opaque to the secret detector and attaching them would satisfy every redaction
guarantee vacuously -- the decode set is `gzip` and `br`, which is every encoding
observed in the recorded corpus, so in practice the marker is now the
rare case rather than the universal one -- and a call whose transport fails
after the request was already sent is recorded **with no response and no
status**, so a suppressed answer still leaves a trace.

**Part of a relayed call's record is not content-gated.** The observed method,
URL (query dropped), status and the credential fingerprint ship with
`content_capture: false` too: they are the account-binding evidence, and a
privacy switch that removed them would let an org opt out of being identified.
They ride the event's `metadata`, which is why they survive a gate that empties
the content fields. Only the bodies are gated; the headers are never sent at
all.

Neither paragraph is a privacy improvement claim. The first is a narrowing of
what *could* egress; the second is a widening of what does.

*When* it leaves: events are delivered in near-real-time by default; a detached
flusher drains the local spool within ~2 seconds of each tool call
(`hookflow.RealtimeTrigger`), with a final drain at session end.
`realtime_flush: false` (or `OPENBOX_REALTIME=0`) delays delivery to session end
instead. Either way this changes only *timing*: what egresses is governed solely
by the table above and the content-capture posture below.

## Usage capture

Usage capture is **on by default**. It answers "which model spent how many
tokens, when" for a coding session, the same finops question the agent runtime
already answers, and it is what makes a dev session visible in the cost
dashboards.

**Exactly what is sent, per model turn:**

| | |
|---|---|
| four integers | input tokens, output tokens, cache-creation tokens, cache-read tokens |
| one string | the model id, e.g. `claude-opus-5`, `gpt-5.6-sol` |
| the turn's index and duration | `<session>:turn:3`, `duration_ms` |
| the subagent id, when a subagent ran the turn | so per-agent spend is attributable |

**Exactly what is not sent, on this path:** no prompt, no stop reason; **and no
cost.** (Tool commands, tool output and file bodies are not on this path either;
they ride the *tool* events instead, under content capture; see the table at the
top.) Thinking used to be on this list too; it is not any more; it rides the
same turn event under content capture, beside the reply, and has its own section
below. Cost is derived server-side from a model-keyed pricing table; deriving it
here would mean inventing a number from a table this client does not own.

The assistant's reply used to be on that list. It is not any more; it rides the
same turn event, under content capture, and has its own section below. The
numbers above and the reply text are separately switchable: `finops: false`
removes the numbers *and* the turn events they ride on (so the reply goes too),
while `content_capture: false` removes only the reply.

*When*: per model turn for Claude Code (its `Stop` hook), and once per session
for Codex; Codex's per-turn hook exists but is deliberately not wired, so its
usage arrives as a single session rollup. The numbers are a **sum over the
turn**: a turn usually contains several model calls, and hooks do not fire per
call, so per-call attribution is not available from either tool.

Turn it off per install:

```jsonc
// ~/.openbox/dev.json
{ "finops": false }
```

Or per session with `OPENBOX_FINOPS=0`. The env setting wins either way, and an
org can pin it through the managed config. With it off, **nothing** on this path
is sent: no counts, no model id, no turn events; and the session transcript is
never opened at all.

Every session records which state was in effect, in the posture block on its
`SessionStarted` event. That is deliberate: a default that sends new data is
only defensible if you can tell afterwards which sessions it applied to.

> **How this reads the transcript, stated precisely.** The token counts are not
> available from any hook; the session transcript file is the only source, so the
> engine parses it. It binds four numeric fields, plus the model id, plus a line
> timestamp (used to compute the turn's duration and then discarded), a boolean
> marking subagent lines, and, since 2026-08-25, the **thinking** blocks.
> Nothing else in that file; prompts, completions, tool inputs, tool results,
> file snapshots; is bound, so it has nowhere to land and cannot reach an event.
>
> This used to be a structural guarantee: the parser held only numbers, so content
> was *impossible* to capture. It is now an **allowlist**. The model id made it one
> (a string, but an identifier); thinking is the first genuinely free-form content
> in it, and what protects that field is not the type any more but three fallible
> mechanisms in a fixed order; the `content_capture` switch, local secret
> redaction, and the 64KB cap.
>
> The allowlist is enforced by a test that seeds the transcript with marker strings
> in every content field class and asserts, on the actual signed request body, that
> **all of them are absent with capture off**, and that with capture on exactly one
> field, thinking, is present, redacted and capped, while the rest are absent. It
> is also mutation-tested: deleting the redaction, or deleting the cap, must each
> make it fail. The narrowing and the subsequent widening are both recorded here
> rather than leaving an older, stronger claim standing.

## Content capture

Content capture is **on by default**. Prompt text is sent so that governance can
act on it; guardrails, drift detection and policy that reasons about intent all
need it.

Turn it off per install:

```jsonc
// ~/.openbox/dev.json
{ "content_capture": false }
```

Or per session with `OPENBOX_CONTENT_CAPTURE=0`. With it off, sessions still
produce full metadata, lineage, token usage, tool success/failure and the
lifecycle signals; you lose prompt visibility, the assistant's reply, tool input
and output, enforced-call bodies, the refusal reasons, and any policy or
dashboard panel that depends on them (goal alignment and drift go empty).

**It is one switch on purpose.** Every content class listed in the table above
answers to this single key. A second, class-specific setting would let an org
believe it had opted out of content while one class kept egressing; so the cost
of the single switch (you cannot keep prompts and drop tool output) is
deliberate. Content capture and usage capture are separate settings on purpose:
usage capture sends no content of its own, so turning content off does not turn
usage off, and vice versa.

An org can pin the setting so a developer cannot change it, via the managed
config (`deployments/managed/`). `openbox doctor` always reports the effective
value and where it came from.

> **`openbox init --provider claude-code` widens what capture can see.** It
> sets `showThinkingSummaries: true` in your user-scope Claude Code settings
> (reversed by `openbox uninstall`, which restores whatever was there before;
> see [Getting started](getting-started.md#4-govern-this-machine)), because
> Claude Code only sends a non-empty reasoning summary when that key is true --
> otherwise `activity_output.thinking` is legitimately absent. There is no new
> gate: a summary that reaches the transcript egresses exactly like the
> assistant's reply, under this same `content_capture` key, through the same
> local redactor.

> **Turning content capture off also turns off command-matching enforcement.**
> This is a real trade-off rather than a footnote, so it is stated here instead of
> being discovered from an audit log.
>
> A policy can only match text that reached the control plane. The shell command a
> tool ran is content: it travels as `activity_input.command` and is dropped when
> content capture is off, exactly like a prompt. The file a tool touched is
> **structural**: `activity_input.file_path` and `activity_input.file_operation`
> are identifiers, not content, so they are always sent.
>
> With content capture **off**, an org therefore keeps every control expressed as
> "which file was touched, and how" -- the self-governance protections over
> `CLAUDE.md`, `.mcp.json`, CI configuration and lockfiles, and the
> credential-file-access controls -- and loses every control expressed as "what
> did the command say", which includes the pipe-to-shell, credential-sweep,
> recursive-delete, destructive-SQL and permission-bypass classes. Of the seeded
> control pack's 104 conditions, 33 keep working and 71 go quiet.
>
> They go quiet **silently**, and that is the part worth knowing: an unmatched
> condition is fail-safe, so nothing errors, nothing is denied, and no warning is
> emitted. `openbox doctor` reports whether content capture is on; it cannot report
> which of your org's policies stopped being able to fire because of it.

> **Redaction at source is not implemented yet.** The server-side Guardrail
> redaction layer is not wired anywhere in this product. Local secret detection is
> the only control on content in transit, and what it catches is
> [measured, not assumed](credentials-and-secrets.md#what-the-scanner-catches-and-where-it-stops). If that
> matters for your data, run with capture off.

> **An elicitation form is the one place where content capture can collect a credential you typed deliberately.** When an MCP server asks you for a value and you answer, your answer is sent under `content_capture` like every other body: redacted locally first, then capped. **Local secret detection is keyword-driven.** It finds values that look like or are labelled as known credential shapes. A password, an API key or a token typed into a form field whose name it does not recognise is not labelled, may not match a known shape, and is then **invisible to the redactor** — it egresses as ordinary text. Turning `content_capture` off is the only control that removes it.
>
> **Measured, 2026-09-08.** A 630-event live session had 107 stored values flagged "Secret-like value detected" by a control-plane template AFTER client redaction ran (690 `${OPENBOX_REDACTED_*}` markers are present in the same bodies, so redaction demonstrably ran). 82 of the 107 are in model-call request bodies — a system prompt plus tool schemas plus conversation, dense in long high-entropy tokens that are not secrets. The flag is a monitor rule, not a denial: all 630 rows recorded verdict `allow`. **Those 107 have not yet been classified** into real misses versus false positives, so this is a measured rate and not yet a verdict on the detector. No detector change was made, because narrowing on an unclassified rate would risk corrupting the developer files the redactor rewrites.
>
> The asymmetry that used to live here; every content class scanned except the
> prompt; **is closed on Claude Code.** Prompt text now passes through the same
> local redactor as the assistant's reply, thinking, tool input and output, the
> enforced-call bodies and the refusal reasons.
>
> **It is not closed on Codex.** That adapter's mapper has no redactor at all; its
> local redaction covers only the enforce path's file body; and the prompt is the
> only content class it sends. So a Codex prompt egresses **unscanned even with
> `secret_detection` on**. Closing it is adapter work.

## What a model turn sends

**This section describes a change in what leaves your machine.** Since that
decision, a model turn carries the **assistant's reply text**; one message per
turn, the same text you saw in your terminal.

Why it is sent at all: OpenBox's goal-alignment and drift detection score what
the agent said against what you asked for. Those two dashboard panels were empty
for every developer session, and no amount of extra metadata could fill them;
the feature reads the assistant's words or it reads nothing.

What bounds it:

- **The `content_capture` switch**, the same one that governs prompt text. With
  it off, no reply text is sent; not truncated, not summarized: the field is
  absent from the payload entirely.
- **`finops: false` also removes it**, since the reply rides the turn event and
  turn events exist only under usage capture.
- **Local secret detection runs first**, over the whole message, before it is
  attached. This is better than the prompt path, which has no such control.
- **64KB cap.** A longer reply is truncated before it is sent.

**The assistant's thinking rides the same turn event**, under the same
`content_capture` switch, redacted and capped the same way; and in its own field
(`activity_output.thinking`), never merged into the reply. Two things about it
are worth knowing rather than discovering:

- **This goes further than the provider will.** Claude Code's own OpenTelemetry
  export redacts extended thinking unconditionally, with every content flag
  enabled. There is no hook that carries it either; the session transcript is
  the only source. Capturing it is a decision an org makes about its own machine
  amendment rather than inherited from "capture everything". `content_capture:
  false` turns it off with everything else.
- **It is the densest content here.** Thinking restates prompts, file contents,
  and any credential the turn saw earlier in its reasoning, so the local secret
  scan matters more on this field than anywhere else. That scan is the same
  detector used everywhere else in this engine, **231 format rules where the
  shape decides, plus a keyword-and-entropy layer for everything else**, with
  the same measured limits (see [Secret detection stays
  local](credentials-and-secrets.md#secret-detection-stays-local)), and thinking is the field those limits
  apply to most.

What is NOT sent on this path: the stop reason, and any tool output the reply
describes.

Two consequences worth knowing rather than discovering:

- **The reply is stored server-side**, on the turn's own row, under
  `activity_output.content`. That is a real increase in what OpenBox retains about
  a session -- and it is a larger increase than this document previously described.
  The reply used to travel inside a `spans[]` array which the control plane parses
  and then **discards**, so it was evaluated in memory and stored nowhere. It is
  now genuinely retained.
- **Token spend and model content now share one policy-visible object.** The
  control plane runs its policy engine and its guardrails over
  `activity_output`, synchronously, before answering. That was already true of
  thinking; it is now true of the reply and, on a relayed call, of the provider's
  raw response too.
- **`secret_detection: false` with capture on sends replies unredacted**, the
- **A stored response now says whether it is whole.** `activity_output.openbox_capture` reports `{truncated, original_bytes}` beside the reply, with `truncated: false` sent explicitly rather than omitted, so a reader can tell a complete reply from a cut one instead of trusting every stored body as finished. It carries no part of the response -- a bool and a byte count only -- and is absent exactly when there is no stored body to describe, including under `content_capture: false`.
  same way it does for enforced-call bodies.

## What an enforced call sends

**This section describes a change in what leaves your machine.** Until that
decision, only shell and MCP calls were sent for a decision, and a file body
never was. Every gated call is now decided by OpenBox, so a **Write or Edit body
is sent**; when content capture is on.

An enforced call sends, in this order:

1. **Structural fields, always.** Tool name and kind, file path and operation,
   MCP server and tool name. These are metadata and flow whatever the capture
   setting.
2. **Secret detection runs locally, on the whole body.** Anything it recognizes
   is replaced with a placeholder before the payload is built.
3. **The content, only if `content_capture` is on**; and on Claude Code it is
   the **redacted** body by default, byte-identical to the observe copy of the
   same call. Three classes override that default, each because a recorded
   decision says so: a shell-kinded call's `command`, an MCP call's whole
   `tool_input`, and a file write's body, rebuilt through the same redactor so
   the enforce copy is the bytes the rewrite put on disk. That rebuild swaps
   only the body, so the whole rebuilt object is scanned afterwards as well --
   an `Edit` carries the text it replaces in `old_string`, which the decider is
   never handed, and without that second pass it egressed unscanned. Only the
   egress copy is scanned that way: the `updatedInput` written back to your
   machine keeps `old_string` verbatim, or the edit would stop matching the
   file.

Three limits, stated rather than implied:

- **The server sees at most the first 65,536 characters** of a body (`capBody`).
  Characters, not bytes; so a body of non-ASCII text can exceed 64KB on the
  wire, up to about 256KB in the worst case. A relayed model call's bodies are
  the exception: those are bounded in BYTES, and the request is bounded by
  selection rather than by a window. Content-based policy is therefore
  not a complete check on a large file: a rule that would match past the cap
  does not fire. Local secret detection is *not* subject to this; it runs before
  the cap.
- **`content_capture: false` means structural-only enforcement.** No body is
  sent for any class, and policy decides on the metadata axes alone. That is
  coarser, not broken; and it is the honest trade: fidelity scales with what you
  let leave the machine.
- **`secret_detection: false` with capture on sends bodies unredacted.** Turning
  off the local detector removes the only in-transit protection there is;
  guardrail redaction at source is still not wired.
- **A gated shell or MCP call sends its command verbatim, unredacted**; even
  with secret detection on. So `curl -H "Authorization: Bearer …"` reaches the
  control plane with the token intact if that call is gated. This is deliberate,
  not an oversight: a policy that decides whether a command is dangerous has to
  see the command that will actually run, and unlike a file body nothing here is
  written back to your machine. It is the one place where the *ordinary
  telemetry* copy of a call is better protected than the copy sent for
  enforcement; the observe copy of that same command IS redacted. Two edges of
  the carve-out are worth knowing: it is an **allowlist**, not a kind test --
  only a shell tool the adapter recognizes by name (`Bash`, `BashOutput`,
  `KillShell`) reaches it, so a tool name the adapter does not know takes the
  redacted default rather than landing here by fallthrough; and a **subagent
  spawn is outside it**, because `Agent` and `ToolSearch` are shell-kinded but
  semantically an LLM call, so their prompt takes the redacted default too.
- **Every other class is redacted on Claude Code, and verbatim on Codex.** A
  subagent prompt, a read's arguments, a glob or grep pattern: on Claude Code
  these leave exactly as their observe copy does. **This changed.** They used to
  be sent as a raw extract while this document promised redaction; one measured
  session shipped 71,051 bytes of verbatim subagent prompt to the control plane
  across 10 spawn rows. Which builtin sends what is now pinned per tool
  (`internal/adapters/claude-code/enforcetarget_census_test.go`, which fails an
  unclassified one) and on the outbound bytes (conformance C57). Codex has not
  had the same inversion: it still sends the whole `tool_input` verbatim for any
  class it does not classify as shell, and redacts only a file body; its mapper
  carries no redactor at all, the same asymmetry as its prompt.

The observe copy of the same call used to be the reassurance here: mapped
separately, carrying no content, so ordinary telemetry was unaffected either
way. **That is no longer true.** The observe copy now carries the same input
under the same `content_capture` switch, and the tool's *output* with it. The
gate is now the only thing separating ordinary telemetry from an enforced call's
payload; which is why it is one switch, asserted on the outbound bytes rather
than assumed.

## What a tool call sends

With content capture on (the default), every tool and MCP call sends what it was
asked to do and what it produced:

| | |
|---|---|
| on the call | the command for a shell tool, the arguments for an MCP tool, the file body for a write, the arguments for a read |
| on the result | what the tool printed or returned; and, when the call failed, the tool's own error text instead |

Both go through the same three steps as an enforced body, in the same order:
**local secret detection first, attachment second, 64KB cap third.** The
ordering is the control, and it is asserted on the bytes actually sent
(conformance cases C32–C38, C40–C49 and C51–C56), not on the code path.

Three consequences worth knowing rather than discovering:

- **This is the biggest single widening of what leaves the machine.** It happens
  at tool-call cadence, not turn cadence; a busy session is hundreds of bodies.
- **Tool output is where secrets actually surface.** An `env` dump, a `cat` of a
  dotenv file, a token in a stack trace. Local detection is the only in-transit
  control, and `secret_detection: false` removes it for these four classes too.
  **What that control does and does not catch is measured, not assumed**; see
  below.
- **`content_capture: false` removes all of it** and returns tool telemetry to
  structural fields alone; tool name, kind, path, timing, outcome.

**Prompts gate too**: in enforce mode the `PromptSubmitted` event is sent for a
decision **at submit time**, before the prompt is processed, instead of riding
the near-real-time flush a moment later. What the event carries did not change;
prompt text only under `content_capture`, redacted locally first on Claude Code
and not on Codex, which has no redactor on the content it sends. What changed
is only the timing and that the verdict is applied: HALT/BLOCK refuses the
prompt, and a HALT ends the session.

## Signal payload is now displayed, not just stored (v1.9)

A lifecycle signal's payload — a config change's file path, a notification's
text, a task's subject, an MCP elicitation's submitted form values — used to ride
`metadata` only. As of v1.9 the same keys also ride `signal_args`.

**What that changes is reach, not first exposure.** The Verify tab already
renders `metadata` (openbox-fe `workflow-tree-view.tsx` renders a Metadata JSON
block per node; the detail modal's overview tab renders "Event Metadata" for
every event type), so these values were already on a screen. `signal_args` is
rendered in *more* places: the session-replay event stream, event details, the
`signal_received` overview, the monitor's issue detail, guardrail violation
records, and compliance source evidence. So a value that appeared in one JSON
block behind a node expansion now also appears as first-class text in several
views, and in stored violation and evidence records.

Two of the nine keys **are** new egress: `notification_title` and
`task_description` were decoded but never sent before v1.9. The other seven
already left the machine under `content_capture`; what changed for them is where
they are shown.

If your organization decided `content_capture` on the understanding that signal
payload was effectively forensics-only, that is the understanding this changes.

**Why it changed.** `metadata` has no reader in any governance engine: OPA
matches `signal_name` + `signal_args`, and Guardrails read `signal_args` alone.
So a policy could match *that* a `config_change` happened but never *that it
touched `.env`* — 21 of 27 signal classes were write-only telemetry. Making them
enforceable means putting their payload where the engines look, and that is the
same field the UI shows.

**Which keys become visible**, all of them still gated behind `content_capture`:

| Key | Class | What it is |
|---|---|---|
| `requested_tool_input` | PermissionRequest | the tool input you were asked to approve |
| `notification_message` | Notification | the notification text |
| `notification_title` | Notification | its title — **newly bound in v1.9** |
| `task_subject` | TaskCreated / TaskCompleted | the task's subject line |
| `task_description` | TaskCreated / TaskCompleted | its body — **newly bound in v1.9** |
| `compact_instructions` | PreCompact | your custom compaction instructions |
| `compact_summary` | PostCompact | the compaction summary |
| `elicitation_message` | Elicitation | the MCP server's prompt |
| `elicitation_response` | ElicitationResult | **your submitted form values** |

**Kept in full, by explicit decision (2026-09-12): `compact_summary` is not
truncated beyond the standard cap.** It rides the same three bounds as every
other content field on this page -- the `content_capture` switch, local
redaction before attachment, and the 64KB cap (`capBodyInto`,
`internal/client/payload.go:813,:844`) -- and it is this session's single
largest content payload, since a compaction can fold an entire context window
into one field. Keeping it was reviewed rather than left as an unexamined
default: it is consistent with the full-capture posture and the one gate
everything else here answers to. This narrows nothing and widens nothing; it
turns an existing default into a stated posture.

`elicitation_response` is the most sensitive of the nine, and the keyword blind
spot below applies to it in full: a value typed into a field name the redactor
does not recognize is invisible to it and egresses as ordinary text.

Structural keys are projected too — file paths, tool names, ids, counts. A file
path is deliberately the *same* key a file tool's `activity_input` uses, so
existing path policies fire on both with no rule authoring; the accepted cost is
that a `Write` and the `file_changed` signal it triggers both carry the path, and
naive counting double-counts one edit.

Some keys classified as structural are free-form in practice and are **not**
gated or redacted, because they were never treated as content: `teammate_name`,
`team_name`, `command_name`, `globs`, and — the one worth naming — an MCP
elicitation's `url`. In URL mode that is a server-supplied URL, and a URL can
carry query parameters (an OAuth `state` or `code`, say). It is capped, never
redacted, and it ships with `content_capture` off like every other structural
key. If that is not acceptable for your MCP servers, the control is not to be
found here — it is which servers you install.

A deploy is a signal too, so the git action's whole attribution record is
projected: status, reason, the derived note, scope counts, and the per-session
claim objects, which carry attestation material. None of it is content and none
of it is gated — it never was, it simply rode `metadata` before. The consequence
worth knowing is downstream: core's guardrails extract every string leaf under
the policy input, so whatever guards your org has configured now scan a deploy
row's attestation blobs. A secrets or PII guard is likely to have opinions about
base64.

**What did not change.** Redaction still runs at the mapper, before a body is
attached — that ordering is the only in-transit control there is, and the
projection reads what the mapper already redacted, so nothing is redacted later
or less. No signal gains an `activity_id`. `metadata` keeps every key it had.
With `content_capture: false`, none of the nine appears in either destination,
asserted on the outbound bytes. `permission_suggestions[].rules[].ruleContent`
remains deliberately unbound: it can embed literal command text, and the v1.9
review deferred it again rather than binding it because the array now fits.

One bound tightened rather than loosened: a content key riding `metadata` had no
cap at all, and is now cut at 65,536 characters like every other content field.

## Account attribution

Two fields, once per session: your provider account's **email** and your
organization's **UUID**. They come from `~/.claude.json`'s `oauthAccount`
record, written by Claude Code, not by OpenBox, and they exist so a stored
session can be attributed to an org.

**What is NOT sent, though it sits in the same object:** `organizationName`,
`organizationRole`, `organizationType`, `seatTier`, `billingType`,
`organizationRateLimitTier`, `userRateLimitTier`. The bound set is exactly two
fields and the Go struct that reads them *is* the allowlist, so adding a third
is a visible code change rather than a quiet widening.

The email is PII. It is not covered by `content_capture`, because it is
attribution rather than content; the same treatment your DID already gets. If
that is not acceptable for your org, the honest answer today is that there is no
switch for it short of not signing in; say so rather than assume one exists.

**What this evidence is worth.** `~/.claude.json` is written by the tool this
product governs and is readable and writable by anything running as you; the
same posture already conceded for the signing key. So it proves
origin-of-config, not tamper-resistance. A determined developer can edit it.
Pair it with the gateway's credential fingerprint, which is derived from the
credential actually presented on the wire, if you need the stronger signal.

If you are not signed in, or the file is unreadable, nothing is stamped at all;
and that absence is itself informative.

## Local files

Two directories, and the split is worth knowing.

**Configuration** lives under `~/.openbox/`; relocate the whole directory with
`OPENBOX_HOME`.

**Runtime state**, spool and audit logs, lives under the OS config directory
instead, and `OPENBOX_HOME` does **not** move it: `~/.config/openbox/` on Linux
(or `$XDG_CONFIG_HOME`), `~/Library/Application Support/openbox/` on macOS,
`%AppData%\openbox\` on Windows. `OPENBOX_SPOOL_DIR` relocates the spool
specifically.

Both are readable only by you.

A third surface sits outside both, in the governed tool's own config: `openbox
init --provider codex` writes an OpenBox-owned `[otel]` block into
`$CODEX_HOME/config.toml` (default `~/.codex/`), pointing Codex's own exporter
at the telemetry lane. Foreign content in that file is spliced around
byte-for-byte, and `openbox uninstall` removes only the block it owns.

| File | Where | What it holds |
|---|---|---|
| `.env` | `~/.openbox/` | **your credentials**, in plaintext, `0600`; see below |
| `dev.json` | `~/.openbox/` | non-secret coordinates and your posture. No credentials |
| `gateway.log` | `~/.openbox/` | the gateway daemon's stdio, only on a machine that ran an older gateway install. Diagnostics; that it started, and its throttled warnings that it is recording nothing. Not a copy of relayed traffic |
| `gateway-prior-env.json` | `~/.openbox/` | the one `ANTHROPIC_BASE_URL` an older gateway install displaced, so retiring or removing it restores your org's own relay instead of deleting it. A URL, no credential |
| `telemetry.log`, `transport.log` | `~/.openbox/` | the same, for the other two lanes. They exist for the same reason: launchd sends a daemon's stdio to `/dev/null` by default, and a throttled warning is the only signal that a perfectly working relay is recording nothing |
| `telemetry-delivery-status.json`, `transport-delivery-status.json` | `~/.openbox/` | how many of that lane's own model-call records its in-process delivery pool has dropped, and since when; `openbox doctor` reads this for its dropped-record row. No content, no credentials |
| `activation.json` | `~/.openbox/` | `0600`. Per lane: the environment keys OpenBox wrote into the tool's settings, and **the values that were there first**, with a before/after SHA-256. It is what lets a removal restore your own relay or corporate proxy key by key instead of truncating a settings file. **On macOS**, it also carries a `system` entry once `openbox init` activates the system PAC: which network services it touched and their prior auto-proxy URL/state, and the CA's SHA-1 and keychain path — the record `openbox uninstall` restores from before untrusting and deleting the CA. No credentials |
| `claude-code-prior-settings.json` | `~/.openbox/` | `0600`. What Claude Code's `showThinkingSummaries` held before `init` forced it: whether the key was there at all, and its raw JSON value, so a removal puts back exactly those bytes rather than a boolean OpenBox reinterpreted. One key, no credential. It is a settings key rather than an environment key, which is why it is not in `activation.json` |
| `transport-ca.pem`, `transport-ca.key` | `~/.openbox/` | **a certificate authority and its private key**, on any machine whose install brought the transport lane up. Generated once on this machine, never transmitted. It has no more at-rest protection than `.env` does: anything running as you can read it. It is generated **unconstrained** (owner ruling 2026-09-22, reversing an earlier name-constraint bound), so with it a leaked key can mint a certificate for **any** site this machine is made to trust the CA for; containment is the per-provider intercept allowlist instead of the certificate — see [Architecture](architecture.md)'s decision record. A machine still holding an older, constrained CA keeps working: it tunnels rather than intercepts any host it cannot mint for, and `openbox doctor` names those hosts as a "legacy constrained CA" finding until the CA is re-issued; a plain `openbox init` re-run now does that itself. `openbox uninstall` untrusts the CA (macOS: removes it from the System keychain by SHA-1 first) then deletes it rather than leaving it behind a relay that is gone |

| File | What it holds |
|---|---|
## macOS: outside these directories

`openbox init` on macOS also changes two things that are not files under
`~/.openbox/` at all, both reversed by `openbox uninstall`:

- **A System keychain trust entry** for the transport CA (`security
  add-trusted-cert -d -r trustRoot`), so desktop apps and browser sessions
  trust it too, not only the tool `NODE_EXTRA_CA_CERTS` points at.
- **Each enabled network service's auto-proxy setting** (`networksetup
  -setautoproxyurl`/`-setautoproxystate`), pointed at the relay's PAC
  endpoint. Whatever was there before (including "off") is recorded in
  `activation.json`'s `system` entry and restored on uninstall.

Neither happens on Linux or Windows yet, and neither happens on macOS without
your `sudo` password, asked once per `openbox init` run that needs it.

What that routing records: a **claude.ai chat**, in a browser tab or the Claude
desktop app, becomes its own session in OpenBox, keyed on the conversation
(`chat:claude-ai:<conversation-uuid>`), signed by the machine's `claude-code`
agent. Each completion call is one `llm_completion` activity carrying the
request and the streamed reply under the one `content_capture` key, redacted
before it is attached like every other body; `surface` and `chat_client`
(`desktop` or `browser`, from the User-Agent) ride metadata. Nothing else on
claude.ai is recorded (titles, notices, conversation reads), the session cookie
never leaves the machine (the relay redacts it from every captured header), and
a chat session never records an end, since the relay cannot see one.

| `policy-bundle.json` | **inert leftover.** There is no local policy bundle since; nothing reads this file and it can be deleted |
| `enforcements.jsonl` | what enforcement did: verdict, source, whether it blocked, redaction *categories*; never the secret, never the body |
| `advisories.jsonl` | advisory verdicts and guardrail findings |
| `cc-spool/` | hook-derived events awaiting flush: session, prompt, tool-call and MCP-call bodies (commands, file contents, tool output, subagent prompts, notification and task text, compaction instructions and summaries, elicitation prompts and answers) with content capture on (the default); already secret-redacted, in plaintext files readable by you, drained continuously so this is an empty queue unless delivery is failing. The `telemetry` and `transport` daemons no longer spool their own model-call records here — each delivers in-process, one attempt per record, with no spool behind it (see [Architecture](architecture.md)'s lane-daemon delivery); a record that attempt cannot land is dropped and counted in `<lane>-delivery-status.json` above, never queued here. A machine still running the retired `gateway` lane is the one exception: it still spools its own model-call bodies (including decompressed provider responses) here, roughly 67 KB each |
| `cc-spool/flusher.log` | what the delivery processes said. New, and it exists because they used to say nothing at all: a flusher that died before delivering was indistinguishable from one that was never started, which is why two missed flushes had to be diagnosed from file timestamps. Diagnostics only, capped, no bodies |
| `cc-spool/.discarded` | one line per batch this machine gave up on: a timestamp, the session, and how many events were lost. Never the events themselves. It exists because the give-up was previously silent |
| `cc-spool/turns/` | how far each turn window has been read: a byte offset and a turn index, nothing else |
| `pending-approvals/`, `stale/` | content-free markers keyed by session id |
| `halted-sessions/` | one small file per HALTed session; the policy reason, policy id and a timestamp, never tool content. It is what keeps a halted session refused; deleting it un-halts only this machine's view, and every verdict is already recorded server-side |
| `approvals-auto.jsonl` | an autonomous approver's decisions, if you run one |

### The three model-call lanes send different amounts, and one sends no content at all

A model call can be observed three ways, and the privacy difference between them
is larger than the table above can show in one row.

- **The gateway and transport lanes are in-path relays.** They see the whole
  exchange. Same `content_capture` gate, same local redaction before attachment,
  same 64KB cap. Three things about them changed and each changes what leaves your
  machine:

  - **A compressed response body is now decompressed before it is inspected and
    attached.** The provider sits behind Cloudflare and compresses everything, so
    in practice *every* response body OpenBox had ever captured was an 88-byte
    "not captured; the body was content-encoded" placeholder. Real response text
    now leaves the machine where it previously did not. It is decompressed in the
    capture path only -- the bytes forwarded to your tool are untouched -- and the
    local secret scan runs over the decompressed text before attachment, which is
    the first time that scan has ever seen provider response content at all.
    `gzip` and `br` are decoded; any other encoding still yields an honest marker
    naming it. **Adding `br` was the larger half of this change by volume.** The
    decode set was gzip-only at first, on the stated belief that `br` was
    unobserved -- and a recorded corpus then showed `br` on the large majority of
    responses, gzip on nearly all the rest, and nothing else at all ([the figures
    and the corpus](architecture.md#model-calls-and-the-lanes)). So
    roughly nine in ten response bodies had been storing a marker, and now store
    provider text. The volume of provider text leaving the machine rises by about
    an order of magnitude, under the same `content_capture` gate and the same
    keyword-floor redactor that gzip already shipped under. That is the existing
    posture reaching the encoding that carries most of the traffic, not a new
    one, but it is a real change in volume and it is stated here rather than left
    to be discovered.
  - **The request and response now reach a field that is stored.** They used to
    travel inside a `spans[]` array which the control plane parses and then
    discards, so they were evaluated and retained nowhere. They now ride
    `activity_input` / `activity_output`, which map to dedicated columns.
  - **The observed HTTP headers no longer leave the machine at all.** This is the
    one change in the safer direction, and it is the largest: your live provider
    credential is on every model request, and while capture always replaced
    credential values by key name before the gate was consulted, the header maps
    were still the highest-risk class this client handled. Nothing ever read them
    (they only reached the control plane inside the discarded array), so they are
    no longer sent. The `credential_fingerprint` -- a one-way digest, never the
    credential -- still is.

  A **token-count probe** (`POST /v1/messages/count_tokens`) now sends **nothing
  whatsoever** -- no bodies, no headers, no event of any kind. It is still
  classified, locally, so it can never be miscounted as a completion, but it is
  never spooled: the control plane would drop it unread regardless (it is not a
  tool call), so recording it would be pure egress for an event that describes
  no work. A probe carries the same conversation a real call does, minus the
  reply, and the tool fires one on every keystroke-triggered recount -- so
  attaching bodies there would have egressed your whole conversation repeatedly.
  The one local trace that survives is a counter on the machine that saw it,
  never transmitted.
- **The telemetry lane sends no content whatsoever.** It receives the tool's own
  OpenTelemetry export and binds exactly one record type to seven values: the
  model id, four token counts, a duration and one request id. No cost; the
  server derives that from a pricing table, and fabricating it here would invent
  a number. No prompt, no completion, no body, no headers, no credential
  fingerprint. Its mapper takes **no redactor**, because there is nothing to
  redact (`internal/cli/telemetryemit/mapper.go`). This lane now sends **two rows**
  per model call rather than one -- an opening and a closing half of the same
  activity -- which changes the row count and not the content.

**One environment key is deliberately withheld.** Claude Code supports
`OTEL_LOG_RAW_API_BODIES`, which makes the client write raw prompt and
completion bodies to disk. OpenBox does **not** set it. It would create a local
liability with no corresponding evidence, since this lane ingests no bodies.

**Almost every captured model-call request is truncated.** Measured, not
estimated: nearly all recorded request bodies exceed the 65,536 cap, while
response bodies almost never do ([the figures and the
run](architecture.md#model-calls-and-the-lanes)). So for an in-path lane the org
typically holds a *fragment* of a prompt, not the prompt. That is accepted policy, and it
cuts both ways; less of your content leaves, and less of it is reviewable.

**Which fragment changed, and it is the more revealing one.** Truncation used to
keep the **head** of a request body. A `/v1/messages` request is a conversation
array whose newest turn is at the *end*, so keeping the head stored the system
prompt and the boilerplate -- the part that does not change between calls -- and
discarded what you had just typed. Request bodies now keep the **tail**. Given
that ~97% of them are truncated, this means the captured fragment is now usually
your most recent turn rather than the preamble: strictly more governance-useful,
and strictly more revealing about the current conversation. Response bodies still
keep the head, because a reply starts at its beginning.

Both caps are measured in **bytes** for a model call, not characters. The bound
exists to bound what the control plane must evaluate synchronously, and 64Ki runes
of CJK is 192 KB on the wire -- so a byte bound is the honest one, and it is
strictly tighter than the character bound it replaced.

### What `openbox uninstall` destroys

`uninstall` is a **full purge**, and most of what it removes is not
recoverable. It prints the whole inventory before it deletes anything, then
names each path as it goes, so export what you need first.

**Deleted:** the hook registrations on every surface; the plugin bundle,
including its copy of the engine; all three lane units; `transport-ca.pem` and
`transport-ca.key` (a trusted signing key behind a relay that no longer exists
is a strictly worse posture than no key); the lane logs; `activation.json`;
`dev.json`; the spool of undelivered events; the session registry and pending
approval markers; and, last, `~/.openbox/.env`.

**Restored, not deleted:** every environment key OpenBox wrote into the tool's
settings is put back to the value it displaced, key by key from
`activation.json`. The settings file is never truncated, and a key that was ours
is removed rather than blanked. A key whose value *changed* after OpenBox set it
belongs to whoever changed it: the removal refuses it, names it, and stops —
because a corporate proxy value silently reverted is an outage.

`showThinkingSummaries` comes back the same way, but from
`claude-code-prior-settings.json` rather than `activation.json`; it is a
settings key, not an environment one. The command reports which of four things
it did: nothing was recorded, the recorded value was put back, the key was
deleted because it was absent before `init`, or it was left alone because you
changed it yourself afterwards. The record is then deleted with the other
posture files — unless the restore could not complete, in which case it is
**kept** and named, because it is the only way that original value is ever
recovered.

**The spool is flushed first, then destroyed.** The command tries to deliver
what is queued before deleting it, prints how many events were queued and how
many remain, and describes the remainder as loss. Where the credentials are
already gone it cannot flush at all and says so rather than deleting quietly.

**The credentials cannot be recovered.** The `obx_` key and the workload
private key are shown once, at registration, and are not stored server-side
(the backend never holds the private key at all). `openbox init --provider
<tool>` afterwards registers a **new**, suffixed agent; it does not recover
this one. Deletion is an unlink, not a secure erase: the blocks are freed, not
overwritten.

**Kept, deliberately:** your organization's own files. `managed_config.toml` and
a root-owned `managed-settings.json` express a mandate rather than OpenBox
state, and removing either would silently downgrade a governed machine.

### Undelivered evidence is now retired after 30 days, and the deletion is reported

A spool file nothing can deliver used to accumulate indefinitely -- one on the
development machine held 1,048 events untouched for eighteen days. Two changes
follow, and they point in opposite directions, so both are stated:

- **A lane daemon now sweeps the spool periodically**, which means evidence that
  had never left your machine will be **delivered** the first time a sweep runs
  after an upgrade, including whatever content was captured at the time. If you
  have a backlog you do not want egressed, delete it before installing.
- **A file older than 30 days is deleted**, with its event count recorded in
  `.discarded`. Deleting evidence is irreversible in a governance product, so the
  age errs long and the deletion is loud rather than silent. Nothing inside the
  age is touched.

## Credentials and secret detection

Both moved to their own document, because two other surfaces link straight into
them: [Credentials and secret detection](credentials-and-secrets.md) — where the
workload private key and API key live, and what the keyword-and-entropy
redactor catches before a body is attached.

## How this is checked

The conformance suite asserts each of these on the actual outbound bytes: that a
gated field is absent with capture off, present and redacted with it on, and
capped. Those cases run on every change.

The end-to-end suite goes further, driving a real session that writes a file
containing a synthetic AWS key and runs a shell command carrying a marker, both
sourced from files so neither appears in the prompt, and then asserting what
reached the control plane. It has **not** been run against a live stack, so
treat it as written rather than as evidence. See
[what is proven, and by what](coverage.md).
