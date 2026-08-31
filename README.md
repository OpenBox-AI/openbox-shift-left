# OpenBox Shift-Left

**Governance for the coding agents your developers already use.**

OpenBox governs agents at runtime. This extends the same pipeline one step
earlier, to Claude Code, OpenAI Codex and the other agentic tools that *write*
the code, so you can answer, for any commit or deploy:

> **who produced this, with which tools and prompts, at what cost; and was it allowed?**

One static binary. Two commands to set up. No second dashboard. The default
install runs no daemon and proxies nothing; governing the *model call* itself is
opt-in and does both ([the local gateway](#governing-the-model-call-itself)).

## Quickstart

**1. Install the engine.** CLI, hook engine and git hook in one no-cgo static
binary.

```bash
curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
```

**2. Authenticate.** `openbox auth` asks for what it needs and stores it. The
URLs prefill with sensible defaults or your current values, so a first run is
mostly pressing Enter. The agent id is **never** prefilled: **leave it blank and
a new agent is registered for you**; then it stops asking, because there is
nothing left to ask.

```bash
export OPENBOX_CONTROL_TOKEN=obx_key_…    # your org key, from the dashboard
openbox auth
```

```
Backend URL (control plane)   [https://api.openbox.ai]:
Core URL (data plane)         [https://core.openbox.ai]:
Agent id (blank registers a new agent):   ← press Enter

  Register a new developer agent? [y/N] y
  ✓ registered  agent 4f2a…  did:aip:9c1b…
  ✓ wrote ~/.openbox/.env       (api key, signing key; 0600)
  ✓ wrote ~/.openbox/dev.json   (agent id, DID, URLs)

Next: openbox init --provider claude-code
```

Give an existing agent id instead and it asks for that agent's DID, API key and
signing key; paste them and it writes the same two files. Secrets are masked as
you type and **no flag ever takes a secret value**, so nothing lands in your
shell history. Re-run `auth` any time to change any of it.

**3. Govern a project.** `openbox init` installs the hooks. It governs **the
current directory only**, and it **enforces**; blocking, ask-for-approval and
secret redaction are on by default, on tool calls and on submitted prompts
alike, and a HALT verdict ends the whole session, not just the call. Blocking
and approval come from OpenBox, so they need it reachable; secret redaction is
local and does not. It never touches credentials; if they are missing it stops
and points you back at `auth`.

```bash
cd ~/code/my-project
openbox init --provider claude-code
```

Want telemetry without enforcement? `--enforce=false`. Note that enforcement
acts on *your org's policy*, so until your org publishes one nothing is blocked
and you get observability either way; with one diagnosed exception, documented
in [What this does not prove](#what-this-does-not-prove).

**4. Use `claude` as normal.** Nothing to run, no runtime environment to set;
unless you added `--gateway`, which is the one flag that changes that
([below](#governing-the-model-call-itself)).

```bash
openbox doctor      # what posture is actually in effect
openbox dev verify  # can this machine reach and authenticate to core?
```

→ **[Getting started](docs/getting-started.md)** for self-hosted, approvers,
upgrading an existing install, and troubleshooting.

### Two URLs, two planes

The **backend** is the control plane (agents, policy, approvals) and defaults to
`https://api.openbox.ai`. The **core** is the data plane (where events go) and
defaults to `https://core.openbox.ai`. Both defaults are the hosted service.

The control plane cannot tell the CLI where your core is, so **if you self-host,
set both explicitly**. Accept one default and override the other and your events
go to the hosted core, which surfaces much later as a 401 that reads as a broken
install.

## Scope: what "governed" means

```bash
openbox init --provider claude-code                  # this project only (default)
openbox init --provider claude-code --scope global   # every project; see below
```

**Project scope** writes the hook entries into
`<project>/.claude/settings.local.json` and takes effect immediately. Re-running
it removes any redundant OpenBox entry, one left at a different engine path, or
one of ours registered twice, and says so, so a project cannot end up firing a
hook more than once; hooks you added yourself are preserved. Sessions started
**anywhere else are not governed and produce no events**; so on a machine set up
this way, absence of events is not evidence of absence of work.

**Global scope** is the real fleet rollout, and `init` cannot finish it alone:
Claude Code activates a plugin org-wide through managed settings, which is an
administrator's action, not a CLI's. `--scope global` installs the bundle and
prints the exact snippet to deploy (see `deployments/managed/`). It tells you
activation is pending rather than pretending it happened.

Codex is **user-scoped only**; its hooks live at `~/.codex/hooks.json`, so
`--scope local` is rejected rather than silently governing everything.

## Governing the model call itself

Hooks see what the agent *does*. No hook carries the model request, so by
default the model call is unobserved and ungoverned. `--gateway` closes that,
and it is **opt-in** because it redirects live model traffic:

```bash
openbox init --provider claude-code --gateway
openbox init --provider claude-code --remove-gateway   # and back out again
```

It installs a loopback daemon under launchd/systemd (macOS and Linux; no Windows
packaging yet), proves it is listening, and only then points Claude Code's
`ANTHROPIC_BASE_URL` at it. Every model call is relayed byte-for-byte to the
provider. A call that names a session is also captured as governance evidence;
request and response headers and bodies, plus a one-way fingerprint of the
credential that paid for it; a call that names none is relayed and recorded
nowhere, because the gateway will not invent a session. `--gateway-addr` and
`--gateway-upstream` change where it listens and where it forwards. Add
`--gateway-verbose` and the daemon logs every relayed call, and whether it was
recorded, to `~/.openbox/gateway.log`; the only way to tell a governed gateway
from a bypassed one without querying stored data.

**It governs the terminal CLI, and not the desktop app** (measured 2026-08-27,
not inferred: with the daemon listening and configured, `claude` in a terminal
produced `POST /v1/messages` lines and captured events, while a desktop-app
session produced nothing at all). The CLI reads `ANTHROPIC_BASE_URL` from
`~/.claude/settings.json`, which is what `--gateway` writes; the desktop app
routes through its own [third-party inference
configuration](https://claude.com/docs/third-party/claude-desktop/gateway)
instead and ignores that file. So on a machine where the developer works in the
desktop app, **`--gateway` governs no model calls and says nothing about it**;
`openbox doctor` reports the file it wrote, not what the app resolved. Pointing
the desktop app at the gateway is possible (`inferenceGatewayBaseUrl`,
MDM-distributable) but collides with pass-through auth: that mode replaces the
claude.ai login with a credential you supply, so there is no provider credential
left for the gateway to relay unless your org has an Anthropic API key.

Three things to know before you turn it on:

- **The claim is detection, not prevention.** A developer can unset one
  environment variable. That is *visible*, a session with model turns and no
  gateway spans is a queryable shape, and `openbox doctor` reports the exposure,
  but it is not stopped. Prevention is your MDM's job: [the MDM
  recipe](docs/gateway-mdm-recipe.md).
- **It captures, it does not yet refuse.** The refusal path is written and
  tested but nothing calls it, deliberately: the status code a refusal should
  use is unprobed, and a wrong one silently disables a Claude Code capability
  for the rest of the session.
- **It is a second process to run and diagnose**, and it has never run against a
  live stack. `openbox doctor` reports whether it is alive, whether this machine
  actually points at it, and what could bypass it.

### Two more lanes, for the calls the gateway cannot see

The gateway only governs what follows `ANTHROPIC_BASE_URL`; the terminal CLI.
Two further lanes exist for the rest, and one command installs everything:

```bash
openbox init --provider claude-code --full        # hooks + telemetry + transport
openbox init --provider claude-code --remove-all  # and back out again
```

- **`--telemetry`** runs a loopback OTLP receiver and points Claude Code's own
  OpenTelemetry export at it. It carries **no content at all**, a model id, four
  token counts, a duration and one request id, no cost, which the server
  derives. Because it is the governed tool reporting its own calls, it is
  *suppressible by the thing it observes*: the weakest claim in this product,
  adopted because it is the only lane that reaches the desktop app at all.
- **`--transport`** is an in-path CONNECT proxy. An allowlisted host, one,
  `api.anthropic.com`, is TLS-terminated with a CA generated on your machine and
  served by the same relay the gateway uses; everything else is blind-tunnelled
  untouched. It sees what the gateway sees, without needing the tool to honour a
  base URL.

Exactly one lane emits per model call, decided by where your settings actually
route model calls (transport > gateway > telemetry). That is not a preference:
the lanes' event ids are deliberately disjoint so nothing dedupes them, which
means two lanes emitting would both store and **double every token count**.
`openbox doctor` names the elected producer and warns when the elected lane has
nothing listening.

Four things to know before turning these on:

- **They are verified by replay, not by running.** Real recorded model calls run
  through the shipped code on a host that cannot open a socket. That proves the
  bytes, the mapping, the gate and the caps; it proves nothing about bind,
  listen, TLS to a real socket, or what the control plane stores. **No stack has
  ever received an event from either lane**, and the desktop coverage that
  motivated them is intent, not measurement.
- **`--transport` installs a certificate authority on your machine.** It is
  generated locally, never transmitted, and name-constrained to that single host
  so a leak cannot mint a certificate for anything else. It has no more
  protection than your credentials do; anything running as you can read it.
  `--remove-all` deletes it.
- **Neither lane refuses a call**, for the same reason the gateway does not: the
  refusal shape is unprobed.
- **The transport lane cannot chain through a corporate proxy.** It clears the
  proxy environment variables it would otherwise inherit, because a relay that
  inherits its own address dials itself until sockets run out.

## Where things live

```
~/.openbox/.env          your credentials; API key, signing key (0600, never commit)
  dev.json      posture (enforce, capture, fail_closed) + coordinates (DID, agent id, URLs)
  approver.json approver config, if you are one
                    ── with --gateway only ──
  gateway.log   the daemon's stdio; the only place it says it is recording nothing
  gateway-prior-env.json   the ANTHROPIC_BASE_URL the install displaced, so
                           --remove-gateway can put your own relay back
                    ── with --telemetry / --transport / --full ──
  telemetry.log, transport.log   the same, per lane
  activation.json  per lane: the env keys we wrote, and the values that were there
                   first, so --remove-all restores them key by key (0600)
  transport-ca.pem, transport-ca.key   the transport lane's CA and its private key; generated here, never sent, name-constrained to one host, and
                   readable by anything running as you. --remove-all deletes both
<project>/.claude/settings.local.json    which hooks fire here  (init --scope local)
~/.claude/settings.json                  ANTHROPIC_BASE_URL, with --gateway only
~/Library/LaunchAgents/ai.openbox.{gateway,telemetry,transport}.plist   lane units (macOS)
~/.config/systemd/user/openbox-{gateway,telemetry,transport}.service     lane units (Linux)
~/.claude/plugins/openbox-observe/       the plugin bundle + engine copy
<os-config-dir>/openbox/                 runtime state: spool, audit logs
                                         (~/.config on Linux, ~/Library/Application Support
                                          on macOS, %AppData% on Windows; NOT relocated
                                          by OPENBOX_HOME; OPENBOX_SPOOL_DIR moves the spool)
```

Secrets and non-secrets never share a file, and no value lives in two places. A
real environment variable always wins, so CI can override anything without
touching disk. `OPENBOX_HOME` relocates the configuration directory.

```
secrets      OPENBOX_API_KEY, OPENBOX_AGENT_PRIVATE_KEY   env var  >  ~/.openbox/.env
coordinates  OPENBOX_AGENT_DID, OPENBOX_AGENT_ID, …       env var  >  dev.json  >  default
```

---

---

## What you get

| | |
|---|---|
| **Session telemetry** | every session, prompt, tool call and MCP call as normalized governance events |
| **Per-turn finops** | which model spent how many tokens, per turn; the same signal the agent runtime reports, on by default |
| **Enforcement** | block, ask-for-approval, or redact secrets *before* a tool runs, from your org policy; **on by default** |
| **Human approval** | a risky call pauses the session; an approver answers from the dashboard or `openbox approve` |
| **Autonomous approval** | a bounded approver answers inside the pause, so routine work never waits |
| **Lineage** | `session → commit → deploy`, with a signed commit attestation |
| **Evidence** | each session reports its own effective posture, so the control plane never has to trust the endpoint's word |
| **Model-call capture** | the request the tool actually sent the model, and the response; via an opt-in local relay, Claude Code only. Two further opt-in lanes exist for the calls that relay cannot see, both verified by replay and **never run against a live stack** |

## How it works

```
 claude / codex ──hooks──▶ openbox engine ──┬──▶ redact secrets locally (µs)
                                            │
                                            ├──▶ openbox-core /evaluate  ⟵ BLOCKS the call
                                            │      └─▶ allow · deny · ask · redact
                                            ▼
                                   spool ──▶ openbox-core ──▶ sessions · events · lineage
                                              (AIP-signed, same endpoint as agent runtime)
```

A single static binary is the whole runtime: the CLI, the hook engine and the
git hook. **OpenBox decides every gated tool call**; the hook asks `/evaluate`
and waits for the verdict before the tool runs. There is one policy
implementation, on the server; nothing evaluates policy on your machine. The
hook path adds no daemon and no socket (a bounded outbound call is not a
resident process); the opt-in [gateway](#governing-the-model-call-itself) is a
resident process, which is why it is a separate flag rather than part of the
default install.

That is a trade, and it cuts both ways: enforcement now depends on reaching
OpenBox, and under the default `fail_closed:false` a gated call proceeds when it
cannot. What it buys is that an org whose policy is hand-written rego is
enforced at all; the local evaluator this replaced could not evaluate rego, so
those gates silently opened.

The one thing still decided locally is **secret redaction**: it must run before
content leaves the machine.

Telemetry is unaffected; spooled and delivered off the hot path in
near-real-time (a detached, debounced flusher drains the spool within ~2s of
each tool call; SessionEnd remains the completeness safety net), so a slow or
absent OpenBox never delays an event.

Everything provider-agnostic lives in one engine; each tool adds only a thin
adapter behind one SPI. Adding a tool is an adapter, not a fork. →
**[Architecture](docs/architecture.md)**

## Provider support

| Provider | Telemetry | Enforcement | Approvals | Model calls | Scope | Org mandate |
|---|---|---|---|---|---|---|
| **Claude Code** | shipped; hooks + durable spool | deny · ask · redact | full, incl. waking a session on a late decision | three opt-in lanes (gateway, telemetry, transport), capture only, one elected per call | project or global | managed settings |
| **Codex** | shipped; hooks + durable spool | deny · redact (no native "ask") | deny + findings channel | not built | user-wide only | `requirements.toml` / MDM (hook itself not yet mandatable) |
| **Cursor** | not built |; |; |; |; | Team hooks available |

The two providers also send **different amounts of content under one posture**:
Claude Code captures tool input, tool output, the failure detail and the turn's
thinking; Codex captures none of them. Stated rather than averaged, in
[COVERAGE](docs/COVERAGE.md) §3.

Two capabilities are provider-independent and work with any tool: OpenBox
registration and the git-trailer commit binding; so lineage and cost tracking
still apply where no adapter exists.

## What leaves your machine

Two things are **on by default**, and both are opt-out:

- **Prompt content**; your prompts are sent, and so is **the assistant's reply
  text**, one message per turn, scanned for secrets and redacted locally first
  (`content_capture: false` to stop);
- **Token usage**; four token counts and the model id per turn (`finops: false`
  to stop).

Two of those lines used to read the other way, and both changed in August 2026:

- **Tool commands, file bodies and tool output now ride ordinary telemetry**,
  not only a gated call. They still ride a gated call too, OpenBox decides every
  gated call now and cannot decide on content it cannot see, but "never on
  observe events" is no longer true.
- **The assistant's thinking is captured**; the turn's thinking blocks,
  concatenated in file order, under the same switch. This goes further than
  Anthropic's own telemetry: their OpenTelemetry export redacts extended
  thinking unconditionally, and no hook carries it, so the session transcript is
  the only source.

With the opt-in [gateway](#governing-the-model-call-itself) there is a third and
much larger class: **the whole model request and response**, which includes the
system prompt, the full message history and every tool definition. Same
`content_capture` switch, same redaction, same cap; and one more limit worth
knowing: a body the provider sent **compressed is not captured at all**, because
redaction cannot inspect bytes it cannot read, so a marker is stored in its
place.

Everything above is gated on `content_capture`, scanned for secrets and redacted
locally before it is sent, and the server sees at most the first 64KB of any
body. Credentials are never transmitted; the gateway relays yours to the
provider untouched and stores only a one-way fingerprint of it.

The exact field list is in **[Data and privacy](docs/data-and-privacy.md)**.

## Known Limitations

A governance tool that overstates its guarantees is the failure it exists to
prevent, so the limits are documented as first-class:

- **Your credentials sit in a plaintext file.** `~/.openbox/.env` is `0600` on
  macOS/Linux, but anything running as you, including the coding agent under
  governance, can read your signing key and sign events as you. On Windows
  `0600` is a no-op and other local accounts can read it. Attestation therefore
  proves origin-of-config, not tamper-resistance against the developer.
- **Project scope means partial coverage.** With the default `init`, only the
  initialized directory is governed, and sessions elsewhere produce no events at
  all; so absence of events is not evidence of absence of work.
- **Commit attribution is an inferred claim** unless the pipeline fetches the
  signed attestation note; then it is cryptographically verified.
- **Enforcement prevents mistakes, not motivated bypass**, for two independent
  reasons. The hook lives in the developer's own config until the provider's
  managed configuration is deployed (`deployments/managed/`); and every gated
  call now asks the control plane, so under the default `fail_closed:false`
  blocking one hostname disables enforcement for that machine. An org that needs
  enforcement to survive a developer who does not want it must set
  `fail_closed`, and accept that a control-plane outage then blocks work.
- **A control-plane HALT is applied even when no policy authored it.** Core can
  express an operational failure, its record of a session gone terminal while
  the session was still live, as a HALT verdict with no policy id, and the
  client applies it, even in an org that has published no policy. A HALT now
  ends the session outright (turn stops, later prompts and calls refused
  locally), so this defect now ends sessions rather than denying calls until the
  record clears; an accepted consequence of trusting every server HALT
  uniformly. Fail-open does not engage, because it covers *no verdict*, not *a
  HALT verdict*. Diagnosed live, core-side fix in flight.
- **Egress is observed, not controlled.** With no lane installed, OpenBox does
  not carry the coding tool's traffic to its model provider at all; it records
  that posture as evidence. With `--gateway` or `--transport` it relays and
  records every model call; but it still does not *refuse* one: the refusal path
  is written and unwired on **both** in-path lanes, so a model call that reaches
  either is forwarded. Nothing anywhere allow-lists the tool's other
  destinations.
- **The two newer lanes have never run against a live stack, and their reason
  for existing is unconfirmed.** `--telemetry` and `--transport` are verified by
  replaying real recorded traffic through the shipped code on a host that cannot
  bind a socket. The desktop-app and subscription-OAuth coverage they were built
  for is intent, not measurement. `--telemetry` additionally reports what the
  governed tool chooses to report, so it is suppressible by the thing it
  observes; treat its silence on an otherwise-active session as a finding rather
  than as an absence.
- **`--transport` puts a CA private key on the developer's machine.**
  Name-constrained to the one intercepted host, generated locally, never
  transmitted; and readable by anything running as that developer, exactly like
  the signing key.
- **The gateway detects a bypass, it does not stop one.** Unsetting one
  environment variable is enough, and no OpenBox default prevents that. What you
  get is a hole in the record that is queryable and attributable, plus an
  `openbox doctor` warning. Prevention needs your MDM to own the config and
  control egress ([the recipe](docs/gateway-mdm-recipe.md)).
- **Secret redaction is keyword-driven, so its reach is measured rather than
  guaranteed.** Named formats come from a maintained rule pack; beneath them sit
  an assignment-shape rule and an entropy floor. An unlabelled high-entropy value
  below that floor is invisible, and the floor cannot simply be lowered: every
  git SHA and UUID would match, and this redactor rewrites developer files.
- **Content-based policy sees at most the first 64KB of a write.** Bodies are
  truncated before egress, so a rule that would match past that offset does not
  fire. Local secret detection is not subject to the cap. For model calls that
  bound binds hard and it is now measured rather than estimated: **96.75% of
  5,049 recorded request bodies exceeded it** (p50 529,175 runes), so an org
  typically holds the head of a prompt rather than the prompt.
- **Windows is build-verified, not runtime-verified.** CI cross-compiles it on
  every change; no automated suite exercises it, and `install.sh` is bash.

Details and current status:
**[Assurance](docs/architecture.md#assurance)**.

## Commands

| | |
|---|---|
| `openbox auth` | credentials for this machine; it asks for everything authentication needs and nothing else. `--rotate` re-issues them for an agent that already exists |
| `openbox init` | install hooks + posture. `--scope`, `--enforce=false`, `--install-git-hook`, `--role approver`, `--gateway` / `--remove-gateway`, `--telemetry`, `--transport`, and `--full` / `--remove-all` for every lane at once |
| `openbox gateway` | run the model-call relay in the foreground; what the service unit invokes, and how you see why it will not start |
| `openbox telemetry` | the same, for the loopback OTLP receiver (`--telemetry` / `--full`) |
| `openbox transport` | the same, for the in-path CONNECT relay (`--transport` / `--full`) |
| `openbox doctor` | the posture actually in effect, who decides, what happens when they are unreachable, whether this directory has more than one OpenBox engine registered, and; with the gateway; whether it is alive, whether this machine points at it, and what could bypass it |
| `openbox dev verify` | can this machine reach and authenticate to core? |
| `openbox approve` | `list`, `allow`, `deny`, or `--watch --auto` for the autonomous approver |
| `openbox managed install` | write the managed-settings files for a fleet rollout |

## Documentation

| | |
|---|---|
| [Getting started](docs/getting-started.md) | install → onboard → verify, with troubleshooting |
| [Architecture](docs/architecture.md) | engine, adapters, enforcement, approvals, assurance |
| [Data and privacy](docs/data-and-privacy.md) | exactly what is captured and sent |
| [Upgrading to inline evaluation](docs/upgrading-to-inline-evaluation.md) | what changes for an existing install, incl. file bodies now egressing |
| [Lineage](docs/lineage.md) | `session → commit → deploy` and how it is verified |
| [Gateway MDM recipe](docs/gateway-mdm-recipe.md) | the artifacts to push if you need the gateway prevented-from, not just detected-around |
| [Event contract](api/dev-event.schema.json) | the normalized event schema |
| [Wire mapping](docs/MAPPING.md) | how each field lands in core's columns |
| [End-to-end tests](docs/test/e2e.md) | `test/`; a mock-free suite against a real stack |

## Contributing

Go 1.27+, no cgo; a `GOTOOLCHAIN=auto` default fetches it for you. The repo is
one Go module, laid out per `golang-standards/project-layout`:

```bash
go build./...                      # everything, from the root
go test -race -count=1./...        # -count=1 is required; see internal/depguard./test/run-all.sh                   # the end-to-end suite (needs a local OpenBox stack)
```

Anything provider-agnostic belongs in `internal/adapters/common/`. The
end-to-end suite needs an OpenBox stack it can reach; `test/00-preflight.sh`
tells you whether the one you have is healthy enough to trust the results.

## License

[Apache License 2.0](LICENSE).
