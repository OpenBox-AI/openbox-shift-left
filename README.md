# OpenBox Shift-Left

**Governance for the coding agents your developers already use.**

OpenBox governs agents at runtime. This extends the same pipeline one step earlier —
to Claude Code, OpenAI Codex and the other agentic tools that *write* the code — so
you can answer, for any commit or deploy:

> **who produced this, with which tools and prompts, at what cost — and was it allowed?**

One static binary. Two commands to set up. No daemon, no proxy, no second dashboard.

## Quickstart

**1. Install the engine.** CLI, hook engine and git hook in one no-cgo static
binary.

```bash
curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
```

**2. Authenticate.** `openbox auth` asks for what it needs and stores it. The URLs
prefill with sensible defaults or your current values, so a first run is mostly
pressing Enter. The agent id is **never** prefilled: **leave it blank and a new
agent is registered for you** — then it stops asking, because there is nothing left
to ask.

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
  ✓ wrote ~/.openbox/.env       (api key, signing key — 0600)
  ✓ wrote ~/.openbox/dev.json   (agent id, DID, URLs)

Next: openbox init --provider claude-code
```

Give an existing agent id instead and it asks for that agent's DID, API key and
signing key — paste them and it writes the same two files. Secrets are masked as
you type and **no flag ever takes a secret value**, so nothing lands in your shell
history. Re-run `auth` any time to change any of it.

**3. Govern a project.** `openbox init` installs the hooks. It governs **the current
directory only**, and it **enforces** — blocking, ask-for-approval and secret
redaction are on by default, on tool calls and on submitted prompts alike, and a
HALT verdict ends the whole session, not just the call
([ADR-0020](docs/adr/ADR-0020-prompt-gate-and-halt-session-stop.md)). Blocking and
approval come from OpenBox, so they need it reachable; secret redaction is local
and does not. It never touches credentials; if they are missing it stops and
points you back at `auth`.

```bash
cd ~/code/my-project
openbox init --provider claude-code
```

Want telemetry without enforcement? `--enforce=false`. Note that enforcement acts on
*your org's policy*, so until your org publishes one nothing is blocked and you get
observability either way — with one diagnosed exception, documented in
[What this does not prove](#what-this-does-not-prove).

**4. Use `claude` as normal.** Nothing to run, no runtime environment to set.

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

The control plane cannot tell the CLI where your core is, so **if you self-host, set
both explicitly**. Accept one default and override the other and your events go to
the hosted core, which surfaces much later as a 401 that reads as a broken install.

## Scope: what "governed" means

```bash
openbox init --provider claude-code                  # this project only (default)
openbox init --provider claude-code --scope global   # every project — see below
```

**Project scope** writes the hook entries into `<project>/.claude/settings.local.json`
and takes effect immediately. Re-running it removes any redundant OpenBox entry —
one left at a different engine path, or one of ours registered twice — and says
so, so a project cannot end up firing a hook more than once; hooks you added
yourself are preserved. Sessions started **anywhere else
are not governed and produce no events** — so on a machine set up this way, absence
of events is not evidence of absence of work.

**Global scope** is the real fleet rollout, and `init` cannot finish it alone: Claude
Code activates a plugin org-wide through managed settings, which is an
administrator's action, not a CLI's. `--scope global` installs the bundle and prints
the exact snippet to deploy (see `deploy/managed/`). It tells you activation is
pending rather than pretending it happened.

Codex is **user-scoped only** — its hooks live at `~/.codex/hooks.json`, so
`--scope local` is rejected rather than silently governing everything.

## Where things live

```
~/.openbox/
  .env          your credentials — API key, signing key (0600, never commit)
  dev.json      posture (enforce, capture, fail_closed) + coordinates (DID, agent id, URLs)
  approver.json approver config, if you are one
<project>/.claude/settings.local.json    which hooks fire here  (init --scope local)
~/.claude/plugins/openbox-observe/       the plugin bundle + engine copy
<os-config-dir>/openbox/                 runtime state: spool, audit logs
                                         (~/.config on Linux, ~/Library/Application Support
                                          on macOS, %AppData% on Windows — NOT relocated
                                          by OPENBOX_HOME; OPENBOX_SPOOL_DIR moves the spool)
```

Secrets and non-secrets never share a file, and no value lives in two places. A real
environment variable always wins, so CI can override anything without touching disk.
`OPENBOX_HOME` relocates the configuration directory.

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
| **Per-turn finops** | which model spent how many tokens, per turn — the same signal the agent runtime reports, on by default ([ADR-0014](docs/adr/ADR-0014-turn-as-activity-and-identifier-allowlist.md)) |
| **Enforcement** | block, ask-for-approval, or redact secrets *before* a tool runs, from your org policy — **on by default** ([ADR-0016](docs/adr/ADR-0016-default-install-posture.md)) |
| **Human approval** | a risky call pauses the session; an approver answers from the dashboard or `openbox approve` |
| **Autonomous approval** | a bounded approver answers inside the pause, so routine work never waits ([ADR-0012](docs/adr/ADR-0012-autonomous-approver.md)) |
| **Lineage** | `session → commit → deploy`, with a signed commit attestation |
| **Evidence** | each session reports its own effective posture, so the control plane never has to trust the endpoint's word |

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

A single static binary is the whole runtime: the CLI, the hook engine and the git
hook. **OpenBox decides every gated tool call** — the hook asks `/evaluate` and
waits for the verdict before the tool runs
([ADR-0017](docs/adr/ADR-0017-inline-policy-evaluation.md)). There is one policy
implementation, on the server; nothing evaluates policy on your machine. Still no
daemon and no socket ([ADR-0006](docs/adr/ADR-0006-in-process-decider.md) stands —
a bounded outbound call is not a resident process).

That is a trade, and it cuts both ways: enforcement now depends on reaching
OpenBox, and under the default `fail_closed:false` a gated call proceeds when it
cannot. What it buys is that an org whose policy is hand-written rego is enforced
at all — the local evaluator this replaced could not evaluate rego, so those gates
silently opened.

The one thing still decided locally is **secret redaction**: it must run before
content leaves the machine.

Telemetry is unaffected — spooled and delivered off the hot path in near-real-time
(a detached, debounced flusher drains the spool within ~2s of each tool call;
SessionEnd remains the completeness safety net), so a slow or absent OpenBox never
delays an event.

Everything provider-agnostic lives in one engine; each tool adds only a thin
adapter behind one SPI. Adding a tool is an adapter, not a fork.
→ **[Architecture](docs/architecture.md)**

## Provider support

| Provider | Telemetry | Enforcement | Approvals | Scope | Org mandate |
|---|---|---|---|---|---|
| **Claude Code** | shipped — hooks + durable spool | deny · ask · redact | full, incl. waking a session on a late decision | project or global | managed settings |
| **Codex** | shipped — hooks + durable spool | deny · redact (no native "ask") | deny + findings channel | user-wide only | `requirements.toml` / MDM (hook itself not yet mandatable) |
| **Cursor** | not built | — | — | — | Team hooks available |

Two capabilities are provider-independent and work with any tool: OpenBox
registration and the git-trailer commit binding — so lineage and cost tracking
still apply where no adapter exists.

## What leaves your machine

Two things are **on by default**, and both are opt-out:

- **prompt content** — your prompts are sent, and so is **the assistant's reply
  text**, one message per turn, scanned for secrets and redacted locally first
  (`content_capture: false` to stop);
- **token usage** — four token counts and the model id per turn
  (`finops: false` to stop).

Two of those lines used to read the other way, and both changed in August 2026:

- **Tool commands, file bodies and tool output now ride ordinary telemetry**, not
  only a gated call ([ADR-0019](docs/adr/ADR-0019-full-content-capture.md) P1).
  They still ride a gated call too — OpenBox decides every gated call now
  ([ADR-0017](docs/adr/ADR-0017-inline-policy-evaluation.md)) and cannot decide on
  content it cannot see — but "never on observe events" is no longer true.
- **The assistant's thinking is captured** — the turn's thinking blocks,
  concatenated in file order, under the same
  switch. This goes further than Anthropic's own telemetry: their OpenTelemetry
  export redacts extended thinking unconditionally, and no hook carries it, so the
  session transcript is the only source
  ([the ADR-0014 amendment](docs/adr/ADR-0014-turn-as-activity-and-identifier-allowlist.md)).

Everything above is gated on `content_capture`, scanned for secrets and redacted
locally before it is sent, and the server sees at most the first 64KB of any body.
Credentials are never transmitted.

The exact field list is in **[Data and privacy](docs/data-and-privacy.md)**.

## What this does *not* prove

A governance tool that overstates its guarantees is the failure it exists to
prevent, so the limits are documented as first-class:

- **Your credentials sit in a plaintext file.** `~/.openbox/.env` is `0600` on
  macOS/Linux, but anything running as you — including the coding agent under
  governance — can read your signing key and sign events as you. On Windows `0600`
  is a no-op and other local accounts can read it. Attestation therefore proves
  origin-of-config, not tamper-resistance against the developer
  ([ADR-0015](docs/adr/ADR-0015-plaintext-credential-file.md)).
- **Project scope means partial coverage.** With the default `init`, only the
  initialized directory is governed, and sessions elsewhere produce no events at
  all — so absence of events is not evidence of absence of work
  ([ADR-0016](docs/adr/ADR-0016-default-install-posture.md)).
- **Commit attribution is an inferred claim** unless the pipeline fetches the
  signed attestation note — then it is cryptographically verified.
- **Enforcement prevents mistakes, not motivated bypass**, for two independent
  reasons. The hook lives in the developer's own config until the provider's managed
  configuration is deployed (`deploy/managed/`) — and every gated call now asks the
  control plane, so under the default `fail_closed:false` blocking one hostname
  disables enforcement for that machine. An org that needs enforcement to survive a
  developer who does not want it must set `fail_closed`, and accept that a
  control-plane outage then blocks work
  ([ADR-0017](docs/adr/ADR-0017-inline-policy-evaluation.md)).
- **A control-plane HALT is applied even when no policy authored it.** Core can
  express an operational failure — its record of a session gone terminal while the
  session was still live — as a HALT verdict with no policy id, and the client
  applies it, even in an org that has published no policy. Since
  [ADR-0020](docs/adr/ADR-0020-prompt-gate-and-halt-session-stop.md) a HALT ends
  the session outright (turn stops, later prompts and calls refused locally), so
  this defect now ends sessions rather than denying calls until the record clears
  — an accepted consequence of trusting every server HALT uniformly. Fail-open
  does not engage, because it covers *no verdict*, not *a HALT verdict*. Diagnosed
  live, core-side fix in flight
  ([diagnosis](plans/reports/debug-260814-1231-session-no-longer-active-halt.md)).
- **Egress is recorded, not controlled.** OpenBox does not proxy or allow-list the
  coding tool's traffic to its model provider; it records that posture as evidence.
- **Content-based policy sees at most the first 64KB of a write.** Bodies are
  truncated before egress, so a rule that would match past that offset does not
  fire. Local secret detection is not subject to the cap.
- **Windows is build-verified, not runtime-verified.** CI cross-compiles it on
  every change; no automated suite exercises it, and `install.sh` is bash.

Details and current status: **[Assurance](docs/architecture.md#assurance--what-the-evidence-proves)**.

## Commands

| | |
|---|---|
| `openbox auth` | credentials for this machine — it asks for everything authentication needs and nothing else. `--rotate` re-issues them for an agent that already exists |
| `openbox init` | install hooks + posture. `--scope`, `--enforce=false`, `--install-git-hook`, `--role approver` |
| `openbox doctor` | the posture actually in effect, who decides, what happens when they are unreachable, and whether this directory has more than one OpenBox engine registered |
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
| [ADRs](docs/adr/) | the decisions, and why |
| [Event contract](contracts/dev-event/) | the normalized event schema and its wire mapping |
| [End-to-end tests](docs/testbed/e2e.md) | `testbed/` — a mock-free suite against a real stack |

## Contributing

Go 1.23+, no cgo. The repo is a Go workspace of small modules
([ADR-0011](docs/adr/ADR-0011-multi-module-layout.md)):

```bash
go build ./cli/...                  # the binary
cd cli && go test ./...             # one module
./testbed/run-all.sh                # the end-to-end suite (needs a local OpenBox stack)
```

Anything provider-agnostic belongs in `adapters/common/`; a new table, endpoint or
service needs an ADR. The end-to-end suite needs an OpenBox stack it can reach —
`testbed/00-preflight.sh` tells you whether the one you have is healthy enough to
trust the results.

## License

[Apache License 2.0](LICENSE).
