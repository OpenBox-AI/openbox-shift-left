# OpenBox Shift-Left

**Governance for the AI coding tools your developers already use.**

If your team uses [Claude Code](https://claude.com/claude-code) or [OpenAI
Codex](https://openai.com/codex) to write code, this project lets your
organization see and control what those tools do — which commands they ran,
which files they changed, which prompts were sent, what it cost, and whether
each action was allowed. It plugs into an OpenBox platform you already run; it
is not a platform of its own.

It is one small program, `openbox`, that you install on a developer's machine.
Two commands set it up. After that there is nothing to keep running.

- **You want to install it** → start with [Quickstart](#quickstart) below.
- **You want to understand it first** → read [How it works](#how-it-works),
  then [Architecture](docs/architecture.md).
- **You want to know what it sends about you** → [Data and
  privacy](docs/data-and-privacy.md).

## Words you will meet

You do not need to know OpenBox internals to use this, but a few terms come up
in every command and message. They are defined here once.

| Term | What it means here |
|---|---|
| **OpenBox platform** | The two servers your organization already runs. The **backend** (control plane) holds agents, policies and approvals. The **core** (data plane) receives events. Each has a URL. |
| **Governed** | A coding tool session is *governed* when `openbox` is watching it: recording what it does, and asking the platform for permission before risky actions. |
| **Hook** | A small program the coding tool runs at fixed moments — before a command, after a file edit, when a session starts. `openbox` installs itself as those hooks. That is the whole mechanism. |
| **Agent** | How the platform identifies one governed tool on one machine. Each tool you govern gets its own agent, with its own **DID** (a unique id) and its own signing key. |
| **Organization control token** | A key that belongs to your *organization*, not to one agent. It is what registers new agents. It looks like `obx_key_…`. You get it from the dashboard. |
| **Posture** | The settings in effect on one machine: is enforcement on, is content being sent, what happens when the platform is unreachable. `openbox doctor` prints it. |
| **Lane** | An optional extra that also records the *model calls* a tool makes (the actual requests to Anthropic), which hooks alone cannot see. Installed automatically on Claude Code. Explained in [Getting started](docs/getting-started.md#governing-the-model-call-itself). |

## Quickstart

### Before you start

You need:

- An **OpenBox platform** already running, hosted or self-hosted, and the
  URLs of its backend and core. (If you use the hosted service, the defaults
  are already right and you can press Enter through those prompts.)
- An **organization control token** from the dashboard: **Organization → API
  Keys**. It must be able to create and read agents. It starts with
  `obx_key_`. If you have a key starting with `obx_` and no `key_`, that is an
  *agent* key — the wrong one for setup. [Getting started §2](docs/getting-started.md#2-get-the-right-credential)
  explains the difference.
- A machine running **macOS or Linux** with **Claude Code** or **Codex**
  installed. Windows builds but is not yet tested end to end.

### 1. Install the program

```bash
curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
```

This downloads one prebuilt binary, checks its hash, and puts it in
`~/.local/bin`. No Go toolchain, no dependencies. Check it worked:

```bash
openbox version
```

### 2. Connect your organization

```bash
openbox auth
```

It asks three questions. Press Enter to accept a default.

```
Backend URL (control plane)   [https://api.openbox.ai]:
Core URL (data plane)         [https://core.openbox.ai]:
Organization control token (obx_key_… or JWT):   ← paste; it is not echoed
```

You should see:

```
✓ wrote ~/.openbox/.env       (0600; plaintext;)
✓ wrote ~/.openbox/dev.json   (URLs; no secrets)

Next: openbox init --provider <claude-code|codex>
```

This step **registers nothing**. It only tells this machine where your
platform is and stores the token so the next step can use it. Run it again any
time to change a URL; a blank answer keeps what is stored.

> **Self-hosting?** Answer *both* URL prompts with your own hosts. The backend
> cannot tell this program where your core is, so leaving one at its default
> sends events to the hosted service and fails later with a confusing 401.

### 3. Govern this machine — once per tool

```bash
openbox init --provider claude-code
```

You should see it register an agent, then install:

```
Registered developer agent "claude-code-<you>@<host>"
  id:    …
  DID:   did:aip:…
  tier:  … (trust …)
Credentials written to ~/.openbox/claude-code/.env (0600); values are not printed (INV-1).
Wrote claude-code native config (no secrets inline; the hook reads ~/.openbox/claude-code/.env at runtime).

Governed: EVERY SESSION on this machine, in any directory.
```

That one command created this tool's agent on your platform, saved its
credentials, and installed the hooks into your **user-wide** settings
(`~/.claude/settings.json`). From now on every Claude Code session on this
machine, in any folder, is governed — including sessions that were already
open, because the tool watches that file.

Also using Codex? Run it again with `--provider codex`. **Each tool gets its
own agent**, so run `init` once for each tool you use. Running it a second time
for the same tool is safe: it sees the existing agent and reuses it, offline.

### 4. Use your tools as normal

```bash
claude
```

Nothing to start, no environment variables to keep set. To check what is in
effect at any time:

```bash
openbox doctor
```

It prints one identity row per tool, every posture setting and where its value
came from, whether this machine can reach and authenticate to your platform,
and — if something is silently not working — why.

**That is the whole setup.** For self-hosted details, approvals, upgrading an
older install, key rotation and troubleshooting, continue to
**[Getting started](docs/getting-started.md)**.

### Two things to know before you rely on it

**It enforces by default.** Blocking, ask-for-approval and local secret
redaction are on from the first session. But enforcement acts on *your
organization's policy*, so until your organization publishes one, nothing is
blocked — you get observability either way. To observe only for one run:

```bash
OPENBOX_ENFORCE=false claude
```

**It fails open by default.** Every risky action is decided by your platform.
If the platform cannot be reached, the action proceeds and the outage never
blocks your work. The trade is that enforcement depends on reachability. An
organization that needs it to survive an outage sets `fail_closed: true` in
`~/.openbox/<tool>/dev.json` — and accepts that an outage then blocks work.

## How it works

```
 claude / codex ──hooks──▶ openbox engine ──┬──▶ redact secrets locally (µs)
                                            │
                                            ├──▶ openbox-core /evaluate  ⟵ BLOCKS the call
                                            │      └─▶ allow · deny · hold · redact
                                            ▼
                                   spool ──▶ openbox-core ──▶ sessions · events · lineage
                                              (AIP-signed, same endpoint as agent runtime)
```

From the top:

1. **The coding tool calls a hook** at each fixed moment: a session starts, a
   prompt is submitted, a command is about to run, a file was just edited.
2. **The hook is `openbox`.** It turns the tool's native event into one
   normalized shape that is the same for every tool.
3. **Secrets are removed locally, first.** Anything that looks like a key or
   token is redacted on your machine before a byte leaves it. This is the only
   step that never needs the platform.
4. **Risky actions ask the platform.** Before a gated action runs, the hook
   sends it to your platform's `/evaluate` endpoint and waits for the verdict:
   allow, deny, hold for a human, or redact. There is no local copy of the
   policy — the platform is the only decider.
5. **Everything else is recorded and sent shortly after.** Events go to a
   local spool and are delivered within a couple of seconds, so a slow or
   absent platform never delays your tool.
6. **Commits are linked to sessions.** A git hook stamps each commit with the
   session that made it and attaches a signed note, so a deploy can be traced
   back to a session, a tool and a prompt.

Everything provider-agnostic lives in one engine; each coding tool is a thin
adapter on top. Adding a tool is an adapter, not a fork. →
**[Architecture](docs/architecture.md)**

## What you get

| | |
|---|---|
| **Session telemetry** | every session, prompt, tool call and MCP call as normalized governance events |
| **Per-turn cost** | which model spent how many tokens, per turn |
| **Enforcement** | block, ask-for-approval, or redact secrets *before* a tool runs, from your organization's policy |
| **Human approval** | a risky call pauses the session; a reviewer answers from the dashboard |
| **Lineage** | `session → commit → deploy`, with a signed commit attestation |
| **Evidence** | each session reports its own effective posture, so the platform never has to trust the endpoint's word |
| **Model-call capture** | on Claude Code, the requests the tool actually sent the model, via the lanes |

## Provider support

| Provider | Telemetry | Enforcement | Approvals | Model calls | Org mandate |
|---|---|---|---|---|---|
| **Claude Code** | shipped | deny · hold · redact | full, incl. waking a session on a late decision | via lanes, capture only | managed settings |
| **Codex** | shipped | deny · hold · redact · halt | deny on prompt, tool **and** approval | telemetry lane only (token/model, no request capture); proxy routing not built | `requirements.toml` / MDM |
| **Cursor** | not built | — | — | — | — |

The two shipped providers send **different amounts of content** under the same
settings. Claude Code captures tool input, output and the model's thinking;
Codex captures none of those three. Codex does capture the **prompt**, and under
`secret_detection` that prompt is now scanned by the same 231-format scanner
Claude Code's content passes through — scanned, which is not the same as safe:
detection is keyword-driven, so an unlabelled high-entropy value below the floor
stays invisible to it. The per-provider detail is in
[Provider coverage](docs/coverage.md).

## What leaves your machine

By default, and each one can be turned off:

- **Prompt text and the assistant's replies and thinking**, scanned for secrets
  and redacted locally first. `content_capture: false` stops all of it.
- **Tool commands, file contents and tool output**, under the same switch.
- **Token counts and the model id** per turn. `finops: false` stops these.

Everything is redacted locally before it is sent, and the platform sees at most
the first 64KB of any body. Your credentials are never transmitted. The exact
field list, and what changed when, is in
**[Data and privacy](docs/data-and-privacy.md)**.

## Where things live

```
~/.openbox/.env                   the organization control token       (0600, never commit)
~/.openbox/dev.json               your organization's backend and core URLs
~/.openbox/<tool>/.env            that tool's agent key and signing key   (0600, never commit)
~/.openbox/<tool>/dev.json        that tool's posture and identity (DID, agent id, URLs)
~/.claude/settings.json           the Claude Code hooks (user-wide) and lane settings
~/.codex/hooks.json               the Codex hooks (user-wide)
```

Secrets and settings never share a file. A real environment variable outranks
every file — for every tool at once — which is how CI provisions a machine
without writing anything to disk. The **organization control token is the one
exception**: `~/.openbox/.env` wins for it, and the variable is the fallback
used when the file holds none, so a forgotten `export` cannot override what
`openbox auth` just wrote. `OPENBOX_HOME` relocates the whole `~/.openbox`
directory. The full inventory, including lane logs and service
units, is in [Getting started](docs/getting-started.md#where-your-credentials-live-and-what-that-costs).

## Known limitations

A governance tool that overstates its guarantees is the failure it exists to
prevent, so the limits are stated as plainly as the features.

- **Your credentials sit in a plaintext file.** `0600` on macOS and Linux; on
  Windows that is a no-op. Anything running as you — including the coding agent
  under governance — can read the signing key. So a signed event proves *a
  machine holding this agent's key produced it*, not that the developer could
  not have tampered with it. The organization control token is stored the same
  way, and it has the largest blast radius on the machine. Details:
  [Credentials](docs/credentials-and-secrets.md).
- **Coverage is per machine, and installing is the developer's own step.** A
  machine that never ran `init` produces no events at all. Making the install
  unavoidable is a fleet job: [`deployments/managed/`](deployments/managed/).
- **Enforcement prevents mistakes, not motivated bypass.** The hooks live in
  the developer's own settings until an administrator deploys managed settings,
  and under the default `fail_closed: false` blocking one hostname disables
  enforcement for that machine.
- **The lanes detect a bypass; they do not stop one.** Unsetting one
  environment variable is enough. What you get is a visible, attributable hole
  in the record and an `openbox doctor` warning. Prevention needs your MDM:
  [the recipe](docs/gateway-mdm-recipe.md).
- **The lanes have never run against a live platform.** They are verified by
  replaying real recorded traffic through the shipped code. Their reason for
  existing — reaching the Claude desktop app — is intent, not measurement.
- **Secret redaction is keyword-driven.** A high-entropy value that does not
  match a known shape is invisible to it. Content policy sees at most the first
  64KB of any body.
- **Windows is build-verified, not runtime-verified.**

The evidence behind each claim, and what is stated as unproven rather than
omitted, is in [what is proven, and by what](docs/coverage.md#5-evidence-what-is-proven-and-by-what).

## Commands

Five commands. `init` is the only one with a flag.

| | |
|---|---|
| `openbox auth` | connect your organization: two URLs and the control token. Prompts; takes no flags; blank keeps what is stored. Registers nothing |
| `openbox init --provider <claude-code\|codex>` | register that tool's own agent, then install its hooks, lanes and posture. Run once per tool; a tool that already has an agent is reused offline |
| `openbox doctor` | what is in effect on this machine and where each value came from; whether the platform is reachable; what could be silently not working |
| `openbox uninstall` | the full reversal, every tool's credentials and the control token included. It finds what is installed rather than being told |
| `openbox version` | |

Two settings are environment variables rather than flags: `OPENBOX_ENFORCE=false`
observes only for one run, and `OPENBOX_INSTALL_GIT_HOOK=false` leaves every
repository's `.git/hooks` alone.

## Documentation

Read top to bottom for the fullest picture; jump in anywhere for one question.

| | For |
|---|---|
| [Getting started](docs/getting-started.md) | **everyone.** Install, connect, govern, confirm; self-hosting, approvals, upgrading, key rotation, troubleshooting |
| [Architecture](docs/architecture.md) | how the engine, adapters, enforcement and approvals fit together; the assurance ledger |
| [Data and privacy](docs/data-and-privacy.md) | exactly what is captured and sent, and the switches |
| [Credentials and secret detection](docs/credentials-and-secrets.md) | where the keys live and what the local redactor catches |
| [Lineage](docs/lineage.md) | `session → commit → deploy`, and how sure each link is |
| [Gateway MDM recipe](docs/gateway-mdm-recipe.md) | administrators who need bypass *prevented*, not just detected |
| [Provider coverage](docs/coverage.md) | adapter authors: what each tool's native surface does and does not supply |
| [Event contract](docs/dev-event-contract.md) | adapter authors: the normalized event schema in `api/` |
| [Wire mapping](docs/mapping.md) | platform integrators: how each field lands in core's columns |

**One-time migration note**, not current authority:
[Upgrading to inline evaluation](docs/upgrading-to-inline-evaluation.md). A
fresh install needs nothing from it.

## Contributing

This is an open-source project under the [Apache License 2.0](LICENSE).
Contributions are welcome.

You need Go 1.27+ and nothing else; a `GOTOOLCHAIN=auto` default fetches it for
you. The repo is one Go module.

```bash
go build ./... && go vet ./...                    # everything, from the root
go test -race -count=1 ./...                      # -count=1 is required; see internal/depguard
go test -run TestGovernanceEval -v ./cmd/openbox/  # the governance evals, listed by claim
```

The tests need no platform: they drive the real hook entrypoint against an
in-process fake, so they run offline on any machine. Anything provider-agnostic
belongs in `internal/adapters/common/`; the layout and the invariants a change
would otherwise break are in [`CLAUDE.md`](CLAUDE.md) and
[Architecture § Layout](docs/architecture.md#layout).

**What the tests do not prove.** They grade what this binary put on the wire,
what it showed the coding agent and what it left on disk. They say nothing about
what the platform does with any of it. Those claims are named as unproven in
[what is proven, and by what](docs/coverage.md#5-evidence-what-is-proven-and-by-what)
rather than omitted.

## License

[Apache License 2.0](LICENSE).
