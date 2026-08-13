# OpenBox Shift-Left

**Governance for the coding agents your developers already use.**

OpenBox governs agents at runtime. This extends the same pipeline one step earlier —
to Claude Code, OpenAI Codex and the other agentic tools that *write* the code — so
you can answer, for any commit or deploy:

> **who produced this, with which tools and prompts, at what cost — and was it allowed?**

One static binary. Two commands to set up. No daemon, no proxy, no second dashboard.

---

## Quickstart

**1. Install the engine.** One no-cgo static binary — CLI, hook engine, git hook and
policy evaluator in one file.

```bash
curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
```

**2. Authenticate.** `openbox auth` asks for what it needs and stores it. Every
field is prefilled with a sensible default or your current value, so a first run is
mostly pressing Enter. **Leave the agent id blank and it registers a new agent for
you** — then it stops asking, because there is nothing left to ask.

```bash
export OPENBOX_CONTROL_TOKEN=obx_key_…    # your org key, from the dashboard
openbox auth
```

```
openbox auth — credentials for this machine

  Organization            [local]                    ▏acme
  Backend URL (control)   [https://api.openbox.ai]   ▏
  Core URL (data)         [https://core.openbox.ai]  ▏
  Agent id                [not set]                  ▏   ← blank registers a new agent

  No agent id given. Register a new developer agent for org "acme"? [y/N] ▏y
  ✓ registered  agent 4f2a…  did:aip:9c1b…
  ✓ wrote ~/.openbox/.env       (api key, signing key — 0600)
  ✓ wrote ~/.openbox/dev.json   (agent id, did, urls)

Next: openbox init --provider claude-code
```

Give an existing agent id instead and it asks for that agent's DID, API key and
signing key — paste them and it writes the same two files. Secrets are masked as you
type and **no flag ever takes a secret value**, so nothing lands in your shell
history. Re-run `auth` any time to change any of it.

**3. Govern a project.** `openbox init` installs the hooks. It governs **the current
directory only**, and it **enforces** — blocking, ask-for-approval and local secret
redaction are on by default. It never touches credentials; if they are missing it
stops and points you back at `auth`.

```bash
cd ~/code/my-project
openbox init --provider claude-code
```

Want telemetry without enforcement? `--enforce=false`. Note that enforcement acts on
*your org's policy*, so until your org publishes one, nothing is blocked and you get
observability either way.

**4. Use `claude` as normal.** Governance is ambient from here — nothing to run, no
runtime environment to set.

```bash
openbox doctor      # what posture is actually in effect
openbox dev verify  # can this machine reach and authenticate to core?
```

→ **[Getting started](docs/getting-started.md)** for self-hosted, approvers and
troubleshooting.

### Two URLs, two planes

The **backend** is the control plane (agents, policy, approvals) and defaults to
`https://api.openbox.ai`. The **core** is the data plane (where events go) and
defaults to `https://core.openbox.ai`. Both defaults are the hosted service.

The control plane cannot tell the CLI where your core is, so **if you self-host, set
both explicitly**. Accept the defaults on a self-hosted install and your events go to
the hosted core, which surfaces much later as a 401 that reads as a broken install.

---

## Scope: what "governed" means

```bash
openbox init --provider claude-code                  # this project only (default)
openbox init --provider claude-code --scope global   # every project — see below
```

**Project scope** writes the hook entries into `<project>/.claude/settings.local.json`
and takes effect immediately. Sessions started **anywhere else are not governed and
produce no events** — so on a machine set up this way, absence of events is not
evidence of absence of work.

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
  .env          your secrets — API key, signing key (0600, never commit)
  dev.json      posture (enforce, tiers, capture) + coordinates (DID, agent id, URLs)
  approver.json approver config, if you are one
<project>/.claude/settings.local.json    which hooks fire here  (init --scope local)
~/.claude/plugins/openbox-observe/       the plugin bundle + engine copy
```

Secrets and non-secrets never share a file, and no value lives in two places.
A real environment variable always wins, so CI can override anything without
touching disk. `OPENBOX_HOME` relocates the whole directory.

```
secrets      OPENBOX_API_KEY, OPENBOX_AGENT_PRIVATE_KEY   env var  >  ~/.openbox/.env
coordinates  OPENBOX_AGENT_DID, OPENBOX_AGENT_ID, …       env var  >  dev.json  >  default
```

## How it works

```
 claude / codex ──hooks──▶ openbox engine ──▶ local policy (µs, no network)
                                 │                    │
                                 │                    └─▶ allow · deny · ask · redact
                                 ▼
                        spool ──▶ openbox-core /evaluate ──▶ sessions · events · lineage
                                        (AIP-signed, same endpoint as agent runtime)
```

Enforcement decides **in-process** in microseconds
([ADR-0006](docs/adr/ADR-0006-in-process-decider.md)); telemetry is spooled and
delivered off the hot path (a detached, debounced flusher drains within ~2s of each
tool call; SessionEnd is the completeness safety net). A slow or absent OpenBox never
slows a tool call and never blocks one.

Everything provider-agnostic lives in one engine; each tool adds only a thin adapter
behind one SPI. Adding a tool is an adapter, not a fork.
→ **[Architecture](docs/architecture.md)**

## What you get

| | |
|---|---|
| **Session telemetry** | every session, prompt, tool call and MCP call as normalized governance events |
| **Per-turn finops** | which model spent how many tokens, per turn, on by default ([ADR-0014](docs/adr/ADR-0014-turn-as-activity-and-identifier-allowlist.md)) |
| **Enforcement** | block, ask-for-approval, or redact secrets *before* a tool runs, from your org policy — **on by default** |
| **Human approval** | a risky call pauses the session; an approver answers from the dashboard or `openbox approve` |
| **Autonomous approval** | a bounded approver answers inside the pause, so routine work never waits ([ADR-0012](docs/adr/ADR-0012-autonomous-approver.md)) |
| **Lineage** | `session → commit → deploy`, with a signed commit attestation |
| **Evidence** | each session reports its own effective posture, so the control plane never trusts the endpoint's word |

## Provider support

| Provider | Telemetry | Enforcement | Approvals | Scope | Org mandate |
|---|---|---|---|---|---|
| **Claude Code** | shipped | deny · ask · redact | full, incl. waking a session on a late decision | project or global | managed settings |
| **Codex** | shipped | deny · redact (no native "ask") | deny + findings channel | user-wide only | `requirements.toml` / MDM (hook itself not yet mandatable) |
| **Cursor** | not built | — | — | — | Team hooks available |

OpenBox registration and the git-trailer commit binding are provider-independent, so
lineage and cost tracking still apply where no adapter exists.

## Commands

| | |
|---|---|
| `openbox auth` | credentials for this machine. `--rotate` re-issues them for an agent that already exists |
| `openbox init` | install hooks + posture. `--scope`, `--enforce=false`, `--install-git-hook`, `--role approver` |
| `openbox doctor` | the posture actually in effect, plus policy-bundle version and integrity |
| `openbox dev verify` | can this machine reach and authenticate to core? |
| `openbox dev sync` | fetch the org policy bundle now |
| `openbox approve` | `list`, `allow`, `deny`, or `--watch --auto` for the autonomous approver |
| `openbox managed install` | write the managed-settings files for a fleet rollout |

## What leaves your machine

Two things are **on by default** and both are opt-out:

- **prompt content** — your prompts are sent (`content_capture: false` to stop);
- **token usage** — four token counts and the model id per turn
  (`finops: false` to stop).

Tool commands and file bodies are **never** sent on ordinary telemetry — only on an
approval request, because a request an approver cannot read is a gate they cannot
exercise. Credentials are never transmitted.

The exact field list is in **[Data and privacy](docs/data-and-privacy.md)**.

## What this does *not* prove

A governance tool that overstates its guarantees is the failure it exists to prevent,
so the limits are first-class:

- **Your credentials sit in a plaintext file.** `~/.openbox/.env` is `0600` on
  macOS/Linux, but anything running as you — including the coding agent under
  governance — can read your signing key and sign events as you. On Windows `0600` is
  a no-op and other local accounts can read it. Attestation therefore proves
  origin-of-config, not tamper-resistance against the developer
  ([ADR-0015](docs/adr/ADR-0015-plaintext-credential-file.md)).
- **Project scope means partial coverage.** With the default `init`, only the
  initialized directory is governed ([ADR-0016](docs/adr/ADR-0016-default-install-posture.md)).
- **Local enforcement prevents mistakes, not motivated bypass**, until the provider's
  managed configuration is deployed (`deploy/managed/`).
- **Egress is recorded, not controlled.** OpenBox does not proxy or allow-list the
  coding tool's traffic to its model provider; it records that posture as evidence.
- **Commit attribution is an inferred claim** unless your pipeline fetches the signed
  attestation note — then it is cryptographically verified.
- **Policy-bundle signatures are verified but not yet issued** — the backend does not
  sign yet, so bundles load as `unsigned` and the session posture says so.
- **Windows is build-verified, not runtime-verified.** CI cross-compiles it; no
  automated suite exercises it.

Details and current status:
**[Assurance](docs/architecture.md#assurance--what-the-evidence-proves)**.

## Upgrading an existing install

Credentials used to live in your OS keychain, and they are **not** migrated — the
keychain support is gone. Re-issue them, which keeps the same agent and DID:

```bash
export OPENBOX_CONTROL_TOKEN=obx_key_…
openbox auth --rotate
```

No org key? Ask whoever has one to rotate for you, or run `openbox auth` and register
a fresh agent. Your old credentials still sit in the keychain and can be read out by
hand if you would rather keep them — see the migration note in
[Getting started](docs/getting-started.md).

Then, per project you want governed, run `openbox init` once — scope is explicit now.

`dev.json` and `approver.json` migrate themselves on first run, with the old files
left in place. If you used the opt-in file secret backend, delete its `secrets.json`
afterwards: it is a stale plaintext copy of live credentials.

## Contributing

Go 1.23+, no cgo. A Go workspace of small modules
([ADR-0011](docs/adr/ADR-0011-multi-module-layout.md)):

```bash
go build ./cli/...                  # the binary
cd cli && go test ./...             # one module
./testbed/run-all.sh                # end-to-end, against a real local stack
```

Anything provider-agnostic belongs in `adapters/common/`; a new table, endpoint or
service needs an ADR. `testbed/00-preflight.sh` tells you whether the stack you have
is healthy enough to trust the results.

## Documentation

| | |
|---|---|
| [Getting started](docs/getting-started.md) | install → auth → init → verify, with troubleshooting |
| [Architecture](docs/architecture.md) | engine, adapters, enforcement tiers, approvals, assurance |
| [Data and privacy](docs/data-and-privacy.md) | exactly what is captured and sent |
| [Lineage](docs/lineage.md) | `session → commit → deploy` and how it is verified |
| [ADRs](docs/adr/) | the decisions, and why |
| [Event contract](contracts/dev-event/) | the normalized event schema and its wire mapping |
| [End-to-end tests](docs/testbed/e2e.md) | `testbed/` — a mock-free suite against a real stack |

## License

[Apache License 2.0](LICENSE).
