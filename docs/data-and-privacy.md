# Data and privacy

What leaves the machine, what never does, and the one setting that changes it.

## The short version

| | Sent to OpenBox | Notes |
|---|---|---|
| Session, tool and MCP **metadata** | always | tool name, kind (`shell`/`file`/`mcp`), file path, MCP server + tool name, timing |
| **Token counts and the model id** | yes, by default | per model turn. `finops: false` turns it off — see [Usage capture](#usage-capture) |
| **Prompt text** | yes, by default | `content_capture: false` turns it off |
| **The assistant's reply text** | yes, by default | **this changed** — one message per model turn, scanned locally for secrets and REDACTED first, truncated at 64KB. Same `content_capture` switch. See [What a model turn sends](#what-a-model-turn-sends) |
| **The assistant's thinking** | **never** | not captured today. [ADR-0019](adr/ADR-0019-full-content-capture.md) is where that would be decided — it is deferred, not ruled out |
| **Shell command text** | on a **gated** call, with capture on | never on ordinary telemetry — see [What an enforced call sends](#what-an-enforced-call-sends) |
| **File contents** (a Write/Edit body) | on a **gated** call, with capture on | **this changed** — see below. Scanned locally for secrets and REDACTED before it is sent, and truncated at 64KB |
| **File contents** (a file you read) | **never** | |
| **Tool output** | **never** | |
| **Credentials** | **never** | they stay on your machine — in a plaintext file readable by you, see [Where credentials live](#where-credentials-live) |
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

**Exactly what is not sent, on this path:** no prompt, no thinking block, no stop
reason, no tool command, no tool output, no file body — **and no cost.** Cost is
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
> timestamp (used to compute the turn's duration and then discarded) and a boolean
> marking subagent lines. Nothing else in that file — prompts, completions,
> thinking blocks, tool inputs, tool results, file snapshots — is bound, so it has
> nowhere to land and cannot reach an event.
>
> This used to be a structural guarantee: the parser held only numbers, so content
> was *impossible* to capture. It is now an **allowlist**, because the model id is a
> string. The allowlist is enforced by a test that seeds the transcript with marker
> strings in four content field classes and asserts they are absent from the actual
> signed request body while the model id is present. See
> [ADR-0014](adr/ADR-0014-turn-as-activity-and-identifier-allowlist.md), which
> records the narrowing rather than leaving the older, stronger claim standing.

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
— you lose prompt visibility, the assistant's reply, enforced-call bodies, and any
policy or dashboard panel that depends on them (goal alignment and drift go empty).
Content capture and usage capture are separate settings on purpose: usage capture
sends no content of its own, so turning content off does not turn usage off, and
vice versa.

An org can pin the setting so a developer cannot change it, via the managed config
(`deploy/managed/`). `openbox doctor` always reports the effective value and where it
came from.

> **Redaction at source is not implemented yet.** With capture on, prompt text is
> sent as-is; the server-side Guardrail redaction layer is not wired. If that matters
> for your data, run with capture off until it is.
>
> Note the asymmetry, because it is real: the assistant's reply and enforced-call
> bodies ARE scanned locally for secrets before they are sent. The **prompt** is
> not. Nothing about that changed here — it is the same gap, now standing beside
> two paths that do have a control.

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

What is NOT sent on this path: the assistant's **thinking blocks**, the stop
reason, and any tool output the reply describes. Thinking is deferred to a future
posture decision ([ADR-0019](adr/ADR-0019-full-content-capture.md)), not ruled out.

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

- **The server sees at most the first 64KB** of a body (`capBody`). Content-based
  policy is therefore not a complete check on a large file: a rule that would match
  at byte 70,000 does not fire. Local secret detection is *not* subject to this —
  it runs before the cap and sees everything.
- **`content_capture: false` means structural-only enforcement.** No body is sent
  for any class, and policy decides on the metadata axes alone. That is coarser, not
  broken — and it is the honest trade: fidelity scales with what you let leave the
  machine.
- **`secret_detection: false` with capture on sends bodies unredacted.** Turning off
  the local detector removes the only in-transit protection there is; guardrail
  redaction at source is still not wired.

The observe copy of the same call is mapped separately and never carries content, so
ordinary telemetry is unaffected either way.

**Prompts gate too** ([ADR-0020](adr/ADR-0020-prompt-gate-and-halt-session-stop.md)):
in enforce mode the `PromptSubmitted` event is sent for a decision **at submit
time**, before the prompt is processed, instead of riding the near-real-time flush
a moment later. What the event carries did not change — prompt text only under
`content_capture`, and the prompt remains the one content path with **no local
redaction** (the asymmetry above, unchanged). What changed is only the timing and
that the verdict is applied: HALT/BLOCK refuses the prompt, and a HALT ends the
session.

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

| File | What it holds |
|---|---|
| `policy-bundle.json` | **inert leftover.** There is no local policy bundle since [ADR-0017](adr/ADR-0017-inline-policy-evaluation.md); nothing reads this file and it can be deleted |
| `enforcements.jsonl` | what enforcement did: verdict, source, whether it blocked, redaction *categories* — never the secret, never the body |
| `advisories.jsonl` | advisory verdicts and guardrail findings |
| `cc-spool/` | events awaiting flush |
| `cc-spool/turns/` | how far each turn window has been read: a byte offset and a turn index, nothing else |
| `pending-approvals/`, `stale/` | content-free markers keyed by session id |
| `halted-sessions/` | one small file per HALTed session — the policy reason, policy id and a timestamp, never tool content. It is what keeps a halted session refused ([ADR-0020](adr/ADR-0020-prompt-gate-and-halt-session-stop.md)); deleting it un-halts only this machine's view, and every verdict is already recorded server-side |
| `approvals-auto.jsonl` | an autonomous approver's decisions, if you run one |

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

## Verified, not asserted

The end-to-end suite proves this rather than documenting it: a real session writes a
file containing a synthetic AWS key and runs a shell command containing a marker,
both sourced from files so neither appears in the prompt. It then asserts the prompt
marker **is** present in what reached OpenBox and the command and file markers are
**absent from every row** the session produced. See
[`docs/testbed/e2e.md`](testbed/e2e.md) § capture.
