# Data and privacy

What leaves the machine, what never does, and the one setting that changes it.

## The short version

| | Sent to OpenBox | Notes |
|---|---|---|
| Session, tool and MCP **metadata** | always | tool name, kind (`shell`/`file`/`mcp`), file path, MCP server + tool name, timing |
| **Token counts and the model id** | yes, by default | per model turn. `finops: false` turns it off — see [Usage capture](#usage-capture) |
| **Prompt text** | yes, by default | on Claude Code, scanned locally for secrets and REDACTED first, like every other body on this table; **on Codex it is not** — that adapter has no redactor on the content it sends. `content_capture: false` turns it off |
| **The assistant's reply text** | yes, by default | **this changed** — one message per model turn, scanned locally for secrets and REDACTED first, truncated at 64KB. Same `content_capture` switch. See [What a model turn sends](#what-a-model-turn-sends) |
| **The assistant's thinking** | yes, by default | **this changed** — extended-thinking text — every thinking block of a turn, concatenated in file order, so one FIELD per turn rather than one block — scanned locally for secrets and REDACTED first, truncated at 64KB. Same `content_capture` switch. This captures more than Anthropic's own telemetry will: their OTel export redacts thinking unconditionally. See [What a model turn sends](#what-a-model-turn-sends) |
| **Shell command text** | yes, by default | **this changed** — it used to ride a *gated* call only; it is now on ordinary tool telemetry too, under the same `content_capture` switch. Redacted and truncated like every body |
| **File contents** (a Write/Edit body) | yes, by default | **this changed twice** — first onto gated calls, now onto ordinary tool telemetry as well. Scanned locally for secrets and REDACTED before it is sent, and truncated at 64KB |
| **File contents** (a file you read) | yes, by default | **this changed** — a read's arguments and its result both ride the tool event now |
| **Tool and MCP output** | yes, by default | **this changed** — what a tool printed or returned, including a failed tool's error text. Redacted and truncated the same way |
| **Why a tool was refused** | yes, by default | **new** — the classifier's reason on a denial, and the provider's detail on a failed turn |
| **Your provider account's email** | yes, if you are signed in | **new** — one field per session, read from Claude Code's own local account record. This is PII, and it egresses as governance evidence like your DID. Not gated by `content_capture`: it is attribution, not content. See [Account attribution](#account-attribution) |
| **Your provider organization's UUID** | yes, if you are signed in | **new** — same source, same session field |
| **Your provider organization's NAME, role, tier, billing** | **never** | all four sit in the same local file beside the two rows above, and none of them is sent. The evidence scope is org UUID + email, deliberately |
| **Model-call request and response bodies** | only with an **in-path lane** running (gateway or transport), and only when the call names a session | **new** — the whole request the tool sent the model, which includes the **system prompt**, the full message history and every tool definition, plus the model's response. This is the largest content class OpenBox collects. Three bounds apply and all three are fallible: the `content_capture` switch, local secret redaction before anything is attached, and a 64KB cap. A body the provider sent **compressed is not captured at all** — redaction cannot inspect it, so a marker is stored instead. Same session caveat as the row below |
| **Model-call HTTP headers** | only with an **in-path lane** running (gateway or transport), and only when the call names a session | **new** — and only the non-credential ones: `Authorization`, `x-api-key`, `Cookie`, `Set-Cookie` and four more are replaced with `[redacted]` by name before anything is attached. The KEY is kept so a reviewer can see one was sent. Gated by `content_capture`. A relayed call that carries no `x-claude-code-session-id` header is recorded NOWHERE — the gateway declines to invent a session, so this is a real gap in the record rather than a silent attribution |
| **A one-way fingerprint of your provider credential** | only with an **in-path lane** running (gateway or transport), and only when the call names a session | **new** — a truncated SHA-256, so OpenBox can tell WHICH registered credential made a call without holding it. Not gated by `content_capture`: it is the account-binding control, and a privacy switch that removed it would let an org opt out of being identified |
| **Credentials** | **never** | they stay on your machine — in a plaintext file readable by you, see [Where credentials live](#where-credentials-live). The gateway relays yours to the provider byte-for-byte and stores none of it |
| Git **commit trailer** and signed attestation | yes | commit sha, tree sha, session id — no diff, no file content |

The rule behind the table: content is gated at one choke point in the client, so a
new field cannot start egressing by accident. Structural identifiers (paths, tool
names, MCP server names) are metadata and always flow; bodies are content and do
not.

A tool call used to be reported as a hand-built telemetry span, and that span had
two fields — a request body and a response body — which *could* have carried a
tool's input or output text. Nothing ever put anything in them, and
[ADR-0013](adr/ADR-0013-tool-call-as-activity.md) removed the span from tool
events entirely. What a completed tool call reports instead is counts — bytes
read, bytes written, lines changed, and an exit code if the tool provides one —
plus, since [ADR-0018](adr/ADR-0018-dev-turn-content-carrier.md), whether it
**succeeded or failed**. Never the output itself.

**One span came back, deliberately, and it carries content.** This page
previously said the response-body channel "cannot be re-opened by an adapter
mistake plus a content-capture opt-in". That is no longer true, and the honest
version is: a model turn now carries exactly one span whose response body is the
assistant's reply, because OpenBox's goal-alignment engine reads assistant text
from that field and from nowhere else. It is a deliberate widening with three
bounds — the `content_capture` switch, local secret redaction before it is sent,
and a 64KB cap — and it applies to **one** carrier. Tool calls remain span-less
and carry no bodies at all.

**A second body-carrying span exists once the local gateway runs**, and it is a
larger widening than the first. The gateway observes real HTTP exchanges, so its
span's request body is the whole request the tool sent the model — system prompt,
message history, tool definitions — and its response body is the model's reply.
Unlike the turn span, this one is a genuine measurement rather than a synthesized
carrier, which is why it is the only span here without ADR-0018's `synthesized`
marker. It is bounded by the same three mechanisms, it exists only while the
gateway is running, and it records nothing at all for a call that names no
session. Two further limits on it, both deliberate: a body the provider sent
**compressed is not captured at all** — a marker is stored instead, because
compressed bytes are opaque to the secret detector and attaching them would satisfy
every redaction guarantee vacuously — and a call whose transport fails after the
request was already sent is recorded **with no response and no status**, so a
suppressed answer still leaves a trace. See
[ADR-0021](adr/ADR-0021-openbox-local-gateway.md).

**Part of the gateway span is not content-gated.** The observed method, URL (query
dropped), status and the credential fingerprint ship with `content_capture: false`
too: they are the account-binding evidence, and a privacy switch that removed them
would let an org opt out of being identified. Only the headers and bodies are gated.

Neither paragraph is a privacy improvement claim. The first is a narrowing of
what *could* egress; the second is a widening of what does.

*When* it leaves: events are delivered in near-real-time by default — a detached
flusher drains the local spool within ~2 seconds of each tool call
(`hookflow.RealtimeTrigger`), with a final drain at session end.
`realtime_flush: false` (or `OPENBOX_REALTIME=0`) delays delivery to session end
instead. Either way this changes only *timing*: what egresses is governed solely
by the table above and the content-capture posture below.

## Usage capture

Usage capture is **on by default**. It answers "which model spent how many tokens,
when" for a coding session — the same finops question the agent runtime already
answers — and it is what makes a dev session visible in the cost dashboards.

**Exactly what is sent, per model turn:**

| | |
|---|---|
| four integers | input tokens, output tokens, cache-creation tokens, cache-read tokens |
| one string | the model id, e.g. `claude-opus-5`, `gpt-5.6-sol` |
| the turn's index and duration | `<session>:turn:3`, `duration_ms` |
| the subagent id, when a subagent ran the turn | so per-agent spend is attributable |

**Exactly what is not sent, on this path:** no prompt, no stop reason — **and no
cost.** (Tool commands, tool output and file bodies are not on this path either;
they ride the *tool* events instead, under content capture — see the table at the
top.) Thinking used to be on this list too; it is not any more — it rides the same
turn event under content capture, beside the reply, and has its own section below.
Cost is
derived server-side from a model-keyed pricing table; deriving it here would mean
inventing a number from a table this client does not own.

The assistant's reply used to be on that list. It is not any more — it rides the
same turn event, under content capture, and has its own section below. The
numbers above and the reply text are separately switchable: `finops: false`
removes the numbers *and* the turn events they ride on (so the reply goes too),
while `content_capture: false` removes only the reply.

*When*: per model turn for Claude Code (its `Stop` hook), and once per session for
Codex — Codex's per-turn hook exists but is deliberately not wired, so its usage
arrives as a single session rollup. The numbers are a **sum over the turn**: a turn
usually contains several model calls, and hooks do not fire per call, so per-call
attribution is not available from either tool.

Turn it off per install:

```jsonc
// ~/.openbox/dev.json
{ "finops": false }
```

or per session with `OPENBOX_FINOPS=0`. The env setting wins either way, and an org
can pin it through the managed config. With it off, **nothing** on this path is
sent: no counts, no model id, no turn events — and the session transcript is never
opened at all.

Every session records which state was in effect, in the posture block on its
`SessionStarted` event. That is deliberate: a default that sends new data is only
defensible if you can tell afterwards which sessions it applied to.

> **How this reads the transcript, stated precisely.** The token counts are not
> available from any hook — the session transcript file is the only source, so the
> engine parses it. It binds four numeric fields, plus the model id, plus a line
> timestamp (used to compute the turn's duration and then discarded), a boolean
> marking subagent lines, and — since 2026-08-25 — the **thinking** blocks.
> Nothing else in that file — prompts, completions, tool inputs, tool results,
> file snapshots — is bound, so it has nowhere to land and cannot reach an event.
>
> This used to be a structural guarantee: the parser held only numbers, so content
> was *impossible* to capture. It is now an **allowlist**. The model id made it one
> (a string, but an identifier); thinking is the first genuinely free-form content
> in it, and what protects that field is not the type any more but three fallible
> mechanisms in a fixed order — the `content_capture` switch, local secret
> redaction, and the 64KB cap.
>
> The allowlist is enforced by a test that seeds the transcript with marker strings
> in every content field class and asserts, on the actual signed request body, that
> **all of them are absent with capture off**, and that with capture on exactly one
> — thinking — is present, redacted and capped, while the rest are still absent. It
> is also mutation-tested: deleting the redaction, or deleting the cap, must each
> make it fail. See
> [ADR-0014](adr/ADR-0014-turn-as-activity-and-identifier-allowlist.md) and its
> 2026-08-25 amendment, which record the narrowing and then the widening rather
> than leaving an older, stronger claim standing.

## Content capture

Content capture is **on by default**. Prompt text is sent so that governance can act
on it — guardrails, drift detection and policy that reasons about intent all need it.

Turn it off per install:

```jsonc
// ~/.openbox/dev.json
{ "content_capture": false }
```

or per session with `OPENBOX_CONTENT_CAPTURE=0`. With it off, sessions still produce
full metadata, lineage, token usage, tool success/failure and the lifecycle signals
— you lose prompt visibility, the assistant's reply, tool input and output,
enforced-call bodies, the refusal reasons, and any policy or dashboard panel that
depends on them (goal alignment and drift go empty).

**It is one switch on purpose.** Every content class listed in the table above
answers to this single key. A second, class-specific setting would let an org
believe it had opted out of content while one class kept egressing — so the cost
of the single switch (you cannot keep prompts and drop tool output) is deliberate.
Content capture and usage capture are separate settings on purpose: usage capture
sends no content of its own, so turning content off does not turn usage off, and
vice versa.

An org can pin the setting so a developer cannot change it, via the managed config
(`deployments/managed/`). `openbox doctor` always reports the effective value and where it
came from.

> **Redaction at source is not implemented yet.** The server-side Guardrail
> redaction layer is not wired anywhere in this product. Local secret detection is
> the only control on content in transit, and what it catches is
> [measured, not assumed](#what-the-scanner-catches--and-where-it-stops). If that
> matters for your data, run with capture off.
>
> The asymmetry that used to live here — every content class scanned except the
> prompt — **is closed on Claude Code.** Prompt text now passes through the same
> local redactor as the assistant's reply, thinking, tool input and output, the
> enforced-call bodies and the refusal reasons.
>
> **It is not closed on Codex.** That adapter's mapper has no redactor at all — its
> local redaction covers only the enforce path's file body — and the prompt is the
> only content class it sends. So a Codex prompt egresses **unscanned even with
> `secret_detection` on**. Closing it is adapter work.

## What a model turn sends

**This section describes a change in what leaves your machine.** Since
[ADR-0018](adr/ADR-0018-dev-turn-content-carrier.md), a model turn carries the
**assistant's reply text** — one message per turn, the same text you saw in your
terminal.

Why it is sent at all: OpenBox's goal-alignment and drift detection score what the
agent said against what you asked for. Those two dashboard panels were empty for
every developer session, and no amount of extra metadata could fill them — the
feature reads the assistant's words or it reads nothing.

What bounds it:

- **The `content_capture` switch**, the same one that governs prompt text. With it
  off, no reply text is sent — not truncated, not summarized: the field and the
  span carrying it are absent from the payload entirely.
- **`finops: false` also removes it**, since the reply rides the turn event and
  turn events exist only under usage capture.
- **Local secret detection runs first**, over the whole message, before it is
  attached. This is better than the prompt path, which has no such control.
- **64KB cap.** A longer reply is truncated before it is sent.

**The assistant's thinking rides the same turn event**, under the same
`content_capture` switch, redacted and capped the same way — and in its own field
(`activity_output.thinking`), never merged into the reply. Two things about it are
worth knowing rather than discovering:

- **This goes further than the provider will.** Claude Code's own OpenTelemetry
  export redacts extended thinking unconditionally, with every content flag
  enabled. There is no hook that carries it either; the session transcript is the
  only source. Capturing it is a decision an org makes about its own machine,
  recorded in the [ADR-0014 amendment](adr/ADR-0014-turn-as-activity-and-identifier-allowlist.md)
  rather than inherited from "capture everything". `content_capture: false` turns
  it off with everything else.
- **It is the densest content here.** Thinking restates prompts, file contents,
  and any credential the turn saw earlier in its reasoning, so the local secret
  scan matters more on this field than anywhere else. That scan is the same
  detector used everywhere else in this engine — **231 format rules where the
  SHAPE decides, plus a keyword-and-entropy layer for everything else** — with the
  same measured limits (see
  [Secret detection stays local](#secret-detection-stays-local)), and thinking is
  the field those limits apply to most.

What is NOT sent on this path: the stop reason, and any tool output the reply
describes.

Two consequences worth knowing rather than discovering:

- **The reply is stored server-side** as a span row, with its own integrity leaf.
  That is a real increase in what OpenBox retains about a session.
- **`secret_detection: false` with capture on sends replies unredacted**, the same
  way it does for enforced-call bodies.

## What an enforced call sends

**This section describes a change in what leaves your machine.** Until
[ADR-0017](adr/ADR-0017-inline-policy-evaluation.md), only shell and MCP calls were
sent for a decision, and a file body never was. Every gated call is now decided by
OpenBox, so a **Write or Edit body is sent** — when content capture is on.

An enforced call sends, in this order:

1. **Structural fields, always.** Tool name and kind, file path and operation, MCP
   server and tool name. These are metadata and flow whatever the capture setting.
2. **Secret detection runs locally, on the whole body.** Anything it recognizes is
   replaced with a placeholder before the payload is built.
3. **The content, only if `content_capture` is on** — and it is the **redacted**
   body, the same bytes your tool call is rewritten to. The command for a shell
   call, the arguments for an MCP call, the file body for a write.

Three limits, stated rather than implied:

- **The server sees at most the first 65,536 CHARACTERS** of a body (`capBody`).
  Characters, not bytes — so a body of non-ASCII text can exceed 64KB on the wire,
  up to about 256KB in the worst case. Content-based policy is therefore not a
  complete check on a large file: a rule that would match past the cap does not
  fire. Local secret detection is *not* subject to this — it runs before the cap.
- **`content_capture: false` means structural-only enforcement.** No body is sent
  for any class, and policy decides on the metadata axes alone. That is coarser, not
  broken — and it is the honest trade: fidelity scales with what you let leave the
  machine.
- **`secret_detection: false` with capture on sends bodies unredacted.** Turning off
  the local detector removes the only in-transit protection there is; guardrail
  redaction at source is still not wired.
- **A gated SHELL or MCP call sends its command VERBATIM, unredacted** — even with
  secret detection on. Only a file body is scanned and rewritten before it is sent
  for a decision. So `curl -H "Authorization: Bearer …"` reaches the control plane
  with the token intact if that call is gated. This is deliberate, not an
  oversight: a policy that decides whether a command is dangerous has to see the
  command that will actually run, and unlike a file body nothing here is written
  back to your machine. It is the one place where the *ordinary telemetry* copy of
  a call is better protected than the copy sent for enforcement — the observe copy
  of that same command IS redacted.

The observe copy of the same call used to be the reassurance here: mapped
separately, carrying no content, so ordinary telemetry was unaffected either way.
**That is no longer true.** Since [ADR-0019](adr/ADR-0019-full-content-capture.md)
the observe copy carries the same input under the same `content_capture` switch,
and the tool's *output* with it. The gate is now the only thing separating
ordinary telemetry from an enforced call's payload — which is why it is one
switch, asserted on the outbound bytes rather than assumed.

## What a tool call sends

With content capture on (the default), every tool and MCP call sends what it was
asked to do and what it produced:

| | |
|---|---|
| on the call | the command for a shell tool, the arguments for an MCP tool, the file body for a write, the arguments for a read |
| on the result | what the tool printed or returned — and, when the call failed, the tool's own error text instead |

Both go through the same three steps as an enforced body, in the same order:
**local secret detection first, attachment second, 64KB cap third.** The ordering
is the control, and it is asserted on the bytes actually sent (conformance cases
C32–C38), not on the code path.

Three consequences worth knowing rather than discovering:

- **This is the biggest single widening of what leaves the machine.** It happens at
  tool-call cadence, not turn cadence — a busy session is hundreds of bodies.
- **Tool output is where secrets actually surface.** An `env` dump, a `cat` of a
  dotenv file, a token in a stack trace. Local detection is the only in-transit
  control, and `secret_detection: false` removes it for these four classes too.
  **What that control does and does not catch is measured, not assumed** — see
  below.
- **`content_capture: false` removes all of it** and returns tool telemetry to
  structural fields alone — tool name, kind, path, timing, outcome.

**Prompts gate too** ([ADR-0020](adr/ADR-0020-prompt-gate-and-halt-session-stop.md)):
in enforce mode the `PromptSubmitted` event is sent for a decision **at submit
time**, before the prompt is processed, instead of riding the near-real-time flush
a moment later. What the event carries did not change — prompt text only under
`content_capture`, and the prompt remains the one content path with **no local
redaction** (the asymmetry above, unchanged). What changed is only the timing and
that the verdict is applied: HALT/BLOCK refuses the prompt, and a HALT ends the
session.

## Account attribution

Two fields, once per session: your provider account's **email** and your
organization's **UUID**. They come from `~/.claude.json`'s `oauthAccount` record —
written by Claude Code, not by OpenBox — and they exist so a stored session can be
attributed to an org.

**What is NOT sent, though it sits in the same object:** `organizationName`,
`organizationRole`, `organizationType`, `seatTier`, `billingType`,
`organizationRateLimitTier`, `userRateLimitTier`. The bound set is exactly two
fields and the Go struct that reads them *is* the allowlist, so adding a third is a
visible code change rather than a quiet widening.

The email is PII. It is not covered by `content_capture`, because it is attribution
rather than content — the same treatment your DID already gets. If that is not
acceptable for your org, the honest answer today is that there is no switch for it
short of not signing in; say so rather than assume one exists.

**What this evidence is worth.** `~/.claude.json` is written by the tool this
product governs and is readable and writable by anything running as you — the same
posture [ADR-0015](adr/ADR-0015-plaintext-credential-file.md) already concedes for
the signing key. So it proves origin-of-config, not tamper-resistance. A determined
developer can edit it. Pair it with the gateway's credential fingerprint, which is
derived from the credential actually presented on the wire, if you need the
stronger signal.

If you are not signed in, or the file is unreadable, nothing is stamped at all —
and that absence is itself informative.

## Local files

Two directories, and the split is worth knowing.

**Configuration** lives under `~/.openbox/` — relocate the whole directory with
`OPENBOX_HOME`.

**Runtime state** — spool and audit logs — lives under the OS config
directory instead, and `OPENBOX_HOME` does **not** move it:
`~/.config/openbox/` on Linux (or `$XDG_CONFIG_HOME`),
`~/Library/Application Support/openbox/` on macOS, `%AppData%\openbox\` on
Windows. `OPENBOX_SPOOL_DIR` relocates the spool specifically.

Both are readable only by you.

| File | Where | What it holds |
|---|---|---|
| `.env` | `~/.openbox/` | **your credentials**, in plaintext, `0600` — see below |
| `dev.json` | `~/.openbox/` | non-secret coordinates and your posture. No credentials |
| `approver.json` | `~/.openbox/` | approver config, if you run one. No credentials |
| `gateway.log` | `~/.openbox/` | the gateway daemon's stdio, with `--gateway` only. Diagnostics — that it started, and its throttled warnings that it is recording nothing. Not a copy of relayed traffic |
| `gateway-prior-env.json` | `~/.openbox/` | the one `ANTHROPIC_BASE_URL` the gateway install displaced, so `--remove-gateway` can restore your org's own relay instead of deleting it. A URL, no credential |
| `telemetry.log`, `transport.log` | `~/.openbox/` | the same, for the other two lanes. They exist for the same reason: launchd sends a daemon's stdio to `/dev/null` by default, and a throttled warning is the only signal that a perfectly working relay is recording nothing |
| `activation.json` | `~/.openbox/` | `0600`. Per lane: the environment keys OpenBox wrote into the tool's settings, and **the values that were there first**, with a before/after SHA-256. It is what lets `--remove-all` restore your own relay or corporate proxy key by key instead of truncating a settings file. No credentials |
| `transport-ca.pem`, `transport-ca.key` | `~/.openbox/` | **a certificate authority and its private key**, with `--transport`/`--full` only. Generated once on this machine, never transmitted, and name-constrained at generation to the single intercepted host — so a leaked key cannot mint a usable certificate for anything else. It has no more at-rest protection than `.env` does: anything running as you can read it, and with it impersonate that one host to this machine. `--remove-all` deletes it rather than leaving it behind a relay that is gone |

| File | What it holds |
|---|---|
| `policy-bundle.json` | **inert leftover.** There is no local policy bundle since [ADR-0017](adr/ADR-0017-inline-policy-evaluation.md); nothing reads this file and it can be deleted |
| `enforcements.jsonl` | what enforcement did: verdict, source, whether it blocked, redaction *categories* — never the secret, never the body |
| `advisories.jsonl` | advisory verdicts and guardrail findings |
| `cc-spool/` | events awaiting flush. With content capture on (the default) these hold the same bodies the events carry — commands, file contents, tool output — already secret-redacted, in plaintext files readable by you |
| `cc-spool/turns/` | how far each turn window has been read: a byte offset and a turn index, nothing else |
| `pending-approvals/`, `stale/` | content-free markers keyed by session id |
| `halted-sessions/` | one small file per HALTed session — the policy reason, policy id and a timestamp, never tool content. It is what keeps a halted session refused ([ADR-0020](adr/ADR-0020-prompt-gate-and-halt-session-stop.md)); deleting it un-halts only this machine's view, and every verdict is already recorded server-side |
| `approvals-auto.jsonl` | an autonomous approver's decisions, if you run one |

### The three model-call lanes send different amounts, and one sends no content at all

A model call can be observed three ways, and the privacy difference between them is
larger than the table above can show in one row.

- **`--gateway` and `--transport` are in-path relays.** They see the whole exchange,
  so they are what the three body/header/fingerprint rows above describe. Same
  `content_capture` gate, same local redaction before attachment, same 64KB cap.
- **`--telemetry` sends no content whatsoever.** It receives the tool's own
  OpenTelemetry export and binds exactly one record type to seven values: the model
  id, four token counts, a duration and one request id. No cost — the server derives
  that from a pricing table, and fabricating it here would invent a number. No prompt, no
  completion, no body, no headers, no credential fingerprint. Its mapper takes **no
  redactor**, because there is nothing to redact
  (`internal/cli/telemetryemit/mapper.go`).

**One environment key is deliberately withheld.** Claude Code supports
`OTEL_LOG_RAW_API_BODIES`, which makes the client write raw prompt and completion
bodies to disk. OpenBox does **not** set it. It would create a local liability with
no corresponding evidence, since this lane ingests no bodies.

**~97% of captured model-call requests are truncated.** Measured on 5,049 recorded
calls: 96.75% of request bodies exceed the 65,536-rune cap (p50 529,175 runes, max
2,566,660). Response bodies: 0.06%. So for an in-path lane the org typically holds
the *head* of a prompt, not the prompt. That is accepted policy (OD1(c)), and it cuts
both ways — less of your content leaves, and less of it is reviewable.

### What `--remove-all` destroys

Removal deletes local evidence. It is printed path by path as it goes, and it is not
recoverable, so export anything you need first.

**Deleted:** `transport-ca.pem` and `transport-ca.key` (leaving a trusted signing key
behind a relay that no longer exists is a strictly worse posture than removing it),
`gateway.log`, `telemetry.log`, `transport.log`, and `activation.json`.

**Restored, not deleted:** every environment key OpenBox wrote into the tool's
settings is put back to the value it displaced, key by key from `activation.json`.
The settings file is never truncated, and a key that was ours is removed rather than
blanked.

**Deliberately kept:** the **spool** of undelivered events. It lives outside
`~/.openbox/` (under the OS config directory) and it is shared with the hook path —
`--remove-all` removes lanes, not hooks, so deleting it would destroy undelivered
governed tool-call evidence belonging to a component that is still installed and
still running. The command names the directory and the file count rather than staying
silent about it; delete it by hand if you mean to discard that evidence.

## Where credentials live

`~/.openbox/.env`, in **plaintext**. Nothing is sent to OpenBox — but there is no
encryption at rest either, and the difference matters, so here it is plainly
([ADR-0015](adr/ADR-0015-plaintext-credential-file.md)):

```
OPENBOX_API_KEY='obx_…'                 # your agent's runtime key
OPENBOX_AGENT_PRIVATE_KEY='…'           # the Ed25519 key this machine signs with
OPENBOX_CONTROL_TOKEN='obx_key_…'       # approver installs only — see below
```

- **On macOS and Linux** the file is `0600` under a `0700` directory, so other
  local users cannot read it. Anything running **as you** can: a shell one-liner,
  a dependency's install script, and **the coding agent under governance**, which
  by design runs arbitrary commands as you.
- **On Windows there is no at-rest protection at all.** `0600` is a no-op there —
  it only toggles the read-only attribute — so the file inherits the parent ACL
  and other local accounts can read it. Use full-disk encryption; do not treat
  this file as protected.
- **It is the only copy.** OpenBox shows the API key and signing key exactly once,
  at registration, and does not store them. Lose the file and you rotate
  (`openbox auth --rotate`) or re-register.
- **Never commit it.** The file's own header comment says so; it lives in your
  home directory rather than anywhere near a repo for that reason.

What that means for evidence: a signed event or commit attestation proves
**origin-of-config** — a machine holding this agent's key produced it — not
tamper-resistance against the developer or the agent they run. The OS keychain
this replaced did not actually change that, since it was unlocked for the whole
desktop session and readable by the same processes; the plaintext file just makes
it obvious.

**Approver installs carry a bigger credential.** If you run `openbox approve`, the
same file holds `OPENBOX_CONTROL_TOKEN`. When that is an `obx_key_…` organization
key, it can **create and rotate agents across your whole organization** — the
signing key above compromises one agent, this one compromises the fleet. Prefer a
short-lived JWT where your deployment allows it, and do not put an approver
install on a shared host.

A real environment variable always beats the file, so CI can supply credentials
without writing anything to disk:

```
secrets      OPENBOX_API_KEY, OPENBOX_AGENT_PRIVATE_KEY   env var  >  ~/.openbox/.env
coordinates  OPENBOX_AGENT_DID, OPENBOX_AGENT_ID, …       env var  >  dev.json  >  default
```

Secrets and non-secrets never share a file, and no value lives in two places.

## Secret detection stays local

In enforce mode, a `Write`/`Edit` body is scanned locally for credential patterns
before the tool runs. A hit is redacted **in the tool input** — the file is written
with `OPENBOX_REDACTED…` in place of the secret — and the audit records the category
(`aws_key`, `entropy`, …), never the value. Nothing about the finding except the
category leaves the machine.

### What the scanner catches — and where it stops

Measured against the real detector, not asserted (conformance
`TestContentCaptureCredentialCoverage` drives a dotenv dump through a real tool
event and asserts the flushed bytes):

| In tool output | Redacted? | Why |
|---|---|---|
| an AWS / GitHub / Stripe / JWT / `sk-` key, anywhere | yes | matched by shape, so surrounding syntax is irrelevant |
| a GitLab / Shopify / Twilio / DigitalOcean / Grafana / … token, anywhere | yes | one of gitleaks' 222 rules; shape again, no key name needed |
| `OPENBOX_API_KEY=obx_…` | yes | the key name matches a known credential keyword |
| `OPENBOX_AGENT_PRIVATE_KEY=<base64>` | yes | matched as a generic API key by shape; the entropy pass would catch it too |
| `API_KEY=<64 hex chars>` | yes | keyword match — the value's alphabet does not matter |
| **`AWS_ACCESS_KEY_ID=<value in no known format>`** | **no** | **the keyword must sit NEXT TO the delimiter, and `_ID` intervenes** |
| `DEPLOY_HEX=<64 hex chars>` | **no** | no keyword, and hex cannot clear the entropy floor |
| `{"password":"…"}` or `{"key":"<base64>"}` **nested in tool output** | yes | the generic patterns tolerate JSON quoting and escaping |

The format layer is two sets that stack: nine hand-rolled regexes
(`decision/secrets.go`) beneath gitleaks' 222 maintained rules
(`decision/gitleaks.go`, D-OSS-4). The nine are LOOSE where gitleaks is PRECISE —
gitleaks adds charset, length and entropy floors and allowlists published
documentation keys — so deleting them in favour of it regressed six conformance
cases and they were restored as a floor. Both layers run before the
keyword/entropy layer.

Two standing limits and one recently closed, all measured rather than assumed:

**1. For generic secrets, the keyword decides — not the shape of the value.** A
high-entropy value next to an unrecognized key name is invisible. That one is
deliberate: the entropy floor sits above what hex can reach (16 symbols cap it at
4.0 bits per character, against a 4.5 threshold) precisely so git SHAs, UUIDs and
content hashes are never flagged. Lowering it would make the scanner fire on
ordinary identifiers — and on the enforce path the scanner **rewrites the file your
tool is about to write**, so a false positive corrupts real content.

**1b. The keyword has to be ADJACENT to the delimiter.** `access_key=…` is caught;
`AWS_ACCESS_KEY_ID=…` is not, because `_ID` sits between the recognised keyword and
the `=`. If the value happens to match one of the 231 format rules the format layer
catches it anyway — a real AWS key id is caught — but a credential-named assignment
carrying an unrecognised value is invisible. Measured, and it is the gap that caused
a real regression: when the nine format regexes were briefly deleted, six
conformance cases went red on exactly this shape.

**A false-positive class worth stating, because the enforce path rewrites files.**
The entropy pass fires on a base64-class token of ≥24 characters at ≥4.5 bits per
character **in a value position** — and a Go source line like
`myConstant := "<48 chars of base64>"` is a value position. During this work the
redactor rewrote three of the repo's own test files that way, replacing a fixture
with a placeholder on disk. Nothing detected it except a test that then measured
the wrong thing. If you keep base64 fixtures in source under a governed session,
that is the shape to know about.

**2. Nested JSON used to be a second gap. It is closed.** A tool's response is
itself JSON, so a nested value arrives escaped (`{\"key\":\"…\"}`), and both
generic mechanisms used to miss that shape — which covers `cat config.json` and
every MCP tool result. Both were widened, so a password or a high-entropy token
inside nested JSON is now redacted like a flat one. Recorded because the scanner's
behaviour changed, and because the named formats were never affected: an AWS key
in JSON was always caught while a database password was not, which is exactly what
made the gap easy to miss.

If your credentials fall under limit 1, that is the case to plan around: run with
`content_capture: false`, or keep them out of the working directory of a governed
session.

The same scanner runs on **every** content body before it is attached to an event,
enforce mode or not: the prompt, the assistant's reply, tool input, tool output, and
the refusal reasons. **The prompt is no longer exempt** — it was the one field
assigned directly instead of through the mapper's redactor, so it egressed unscanned
with `secret_detection` fully on; that was fixed on 2026-08-26 and conformance C42
asserts it on the outbound bytes (`internal/adapters/claude-code/mapper.go:225`). The same
shape is **still live for Codex**, whose mapper has no redactor at all — see
[COVERAGE.md §3.4](../docs/COVERAGE.md). Redaction runs **before**
attachment in all cases — a redaction applied
afterwards would pass every code-level test and still ship the secret, so the
ordering is asserted on the outbound bytes (conformance C18, C26, C34).

## Verified, not asserted

The end-to-end suite proves this rather than documenting it: a real session writes a
file containing a synthetic AWS key and runs a shell command containing a marker,
both sourced from files so neither appears in the prompt. It then asserts the prompt
marker **is** present in what reached OpenBox and the command and file markers are
**absent from every row** the session produced. See
[`docs/test/e2e.md`](test/e2e.md) § capture.
