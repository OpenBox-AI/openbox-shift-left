# Getting started

This guide takes one developer machine from nothing to governed, then covers
configuration, day-to-day operation and troubleshooting. The
[README](../README.md) has the short version.

Setup is two commands:

```
openbox auth                     save your platform URLs and organization key (once)
openbox init --provider <tool>   register that tool's agent and install its hooks (once per tool)
```

## Before you start

- [ ] An OpenBox platform is running, and you know the URLs of its backend
      (control plane) and core (data plane). The hosted service is the default.
- [ ] You have an **organization API key** (see [step 2](#2-get-the-right-credential)).
- [ ] An OpenBox admin has initialized your organization's identity provider.
      Every agent `openbox` registers is a `keycloak_workload` identity, which
      needs this done once per organization. If it is not, `init` stops with
      "your org's identity provider is not initialized".
- [ ] You are on macOS or Linux, with Claude Code or Codex installed.

## 1. Install

```bash
curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
openbox version
```

The script downloads the prebuilt binary for your OS and CPU (macOS or Linux,
amd64 or arm64), verifies its checksum and installs it to `~/.local/bin`
(override with `OPENBOX_INSTALL_DIR`). If no prebuilt binary matches, it builds
from source, which needs Go 1.27+.

**Windows:** the binary builds, but `install.sh` is bash and nothing tests
Windows at runtime. Build from source with `go build ./cmd/openbox`.

## 2. Get the right credential

OpenBox has two kinds of key, and the dashboard shows both. You need the
**organization key**.

| | Organization key (you need this) | Agent key (not this) |
|---|---|---|
| Looks like | `obx_key_…` | `obx_…` or `obx_test_…` |
| Belongs to | your organization | one agent |
| Where | dashboard → **Organization → API Keys** | dashboard → agent detail page |
| Used for | registering a new agent | the agent's own runtime calls |
| Stored as | `OPENBOX_CONTROL_TOKEN` | `OPENBOX_API_KEY`, written by `init` |

The organization key needs `create:agent` and `read:agent`. It is only used to
**register** an agent. Once a tool has an agent, re-running `init` for it works
offline with no organization key at all. Agent keys are created by
registration; they are never an input to it.

A secret is never accepted as a command-line flag, so it cannot leak through
shell history or `ps`.

## 3. Connect your organization

```bash
openbox auth
```

```
Backend URL (control plane)   [https://api.openbox.ai]:
Core URL (data plane)         [https://core.openbox.ai]:
Organization control token (obx_key_… or JWT):

✓ wrote ~/.openbox/.env       (0600; plaintext;)
✓ wrote ~/.openbox/dev.json   (URLs; no secrets)

Next: openbox init --provider <claude-code|codex>
```

Each prompt is prefilled with the current value, and a blank answer keeps it,
so re-running `auth` to fix one URL is safe. `auth` registers nothing.

**Self-hosted platform:** answer **both** URL prompts with your own hosts. The
backend cannot tell `openbox` where your core is, so leaving core at its
default sends every event to the hosted service, which answers 401.

The organization key is saved in plaintext in `~/.openbox/.env`, because
`auth` and `init` run as separate processes. This key can create agents for
your whole organization. If you would rather it never touch the disk, leave
the prompt blank and export `OPENBOX_CONTROL_TOKEN` for the one `init` run
instead.

## 4. Govern a tool

```bash
openbox init --provider claude-code
```

`init` first settles the tool's **identity**:

1. **The tool already has an agent** (`~/.openbox/claude-code/.env` exists):
   it is reused, offline.
2. **No agent yet, and you are at a terminal:** `init` asks whether to **adopt**
   an existing agent. Say yes to move an agent from another machine; you paste
   its agent id, its API key and its workload private-key file.
3. **Otherwise** it registers a new agent with your organization key. If the
   default name is taken (for example by a lost earlier install), it registers
   `<name>-<6 hex>` instead and prints both.

Then it installs:

- the hooks, in your **user-wide** settings (`~/.claude/settings.json` or
  `~/.codex/hooks.json`). Your own hooks are left alone; stale or duplicate
  OpenBox entries are removed and reported;
- the tool's settings and identity, in `~/.openbox/<tool>/dev.json`;
- the model-call lanes, if the tool supports them (see
  [below](#model-call-lanes-claude-code));
- a git `prepare-commit-msg` hook that stamps commits for
  [lineage](lineage.md). Set `OPENBOX_INSTALL_GIT_HOOK=false` to skip it;
- on Claude Code, `showThinkingSummaries: true`, so thinking blocks arrive
  with content. `openbox uninstall` restores the previous value.

The change takes effect immediately, for every session on the machine,
including open ones: the tool watches its settings file. `init` prints what it
changed.

### Codex differences

Same command with `--provider codex`. The agent lives in `~/.openbox/codex/`.

- Codex asks you to **trust new hooks** (`/hooks` inside Codex) before running
  them. Until you do, nothing is governed.
- An approval-required verdict becomes a **deny**, because Codex has no way to
  pause for one.
- A HALT verdict ends the session only on a prompt. On a tool call it denies
  that call.
- Codex reports token usage once per session, not per turn.

## 5. Confirm it

```bash
openbox doctor
```

`doctor` prints, per tool: the identity in use, every setting with its value
and source (default, config file, environment, or org mandate), whether the
platform is reachable and accepts the credential, the model-call lane status,
halted sessions, and warnings for anything silently broken. It is the first
thing to run for any problem.

## Settings

Settings live in each tool's `~/.openbox/<tool>/dev.json`. An environment
variable overrides the file. An org can set defaults, or lock a value so
neither can change it, through managed config (see
[`deployments/managed/`](../deployments/managed/)).

| Key | Env var | Default | Effect |
|---|---|---|---|
| `content_capture` | `OPENBOX_CONTENT_CAPTURE` | `true` | Send prompts, replies, thinking, tool input and output. See [Data and privacy](data-and-privacy.md). |
| `finops` | `OPENBOX_FINOPS` | `true` | Send token counts and model id per turn. |
| `secret_detection` | `OPENBOX_SECRET_DETECTION` | `true` | Redact secrets locally before sending. |
| `findings` | `OPENBOX_FINDINGS` | `false` | Show asynchronous guardrail findings back in the session. |
| `realtime_flush` | `OPENBOX_REALTIME` | `true` | Deliver events within seconds, instead of at session end. |
| `approval_hold_ms` | `OPENBOX_APPROVAL_HOLD_MS` | `20000` | How long a call waits for an approver. |

For the environment variables, `1`, `true`, `yes` or `on` means on; any other
value means off.

`enforce`, `fail_closed` and a few other retired keys still parse, so an old
config does not break, but they do nothing. `openbox doctor` lists them as
ignored. Enforcement and fail-closed delivery cannot be turned off.

## Automation and CI

`auth` needs a terminal. On a machine without one, use one of these instead:

```bash
# Option 1 (recommended): export the organization key and let init register agents.
export OPENBOX_CONTROL_TOKEN=obx_key_…
openbox init --provider claude-code

# Option 2: provide an existing agent's identity directly. These variables
# apply to EVERY governed tool on the machine, so use this for single-tool
# images only.
export OPENBOX_API_KEY=… OPENBOX_WORKLOAD_PRIVATE_KEY=… OPENBOX_AGENT_ID=…
```

Other useful variables:

| Variable | Use |
|---|---|
| `OPENBOX_BACKEND_URL`, `OPENBOX_BASE_URL` | Override the backend and core URLs at runtime. |
| `OPENBOX_HOME` | Move the whole `~/.openbox` directory. |
| `OPENBOX_CONFIG` | Point at one specific `dev.json`. Use it for one tool only: two tools sharing it would share an agent id but not a key, and the platform rejects that. |

Precedence: an environment variable beats every file, for every tool. The one
exception is `OPENBOX_CONTROL_TOKEN`: the saved `~/.openbox/.env` wins, and the
variable is used only when the file has none. This stops a forgotten `export`
in an old shell from silently overriding what `auth` just saved.

## Where files live

| Path | Contents |
|---|---|
| `~/.openbox/.env` | organization key (`0600`, plaintext) |
| `~/.openbox/dev.json` | backend and core URLs |
| `~/.openbox/<tool>/.env` | that tool's agent API key and workload private key (`0600`, plaintext) |
| `~/.openbox/<tool>/dev.json` | that tool's agent id, URLs and settings |
| `~/.openbox/<tool>/workload-token.json` | short-lived access token cache (deletable) |
| `~/.openbox/transport-ca.*`, `activation.json`, `*.log` | model-call lane files |
| `~/Library/Application Support/openbox/` (macOS), `~/.config/openbox/` (Linux) | runtime state: the event queue, enforcement log, halted-session markers |

`init` copies the URLs from `~/.openbox/dev.json` into each tool's `dev.json`.
If you change a URL with `auth`, re-run `init` for each tool to apply it;
`doctor` flags the mismatch.

Never commit any of these files. What each one holds, and what is protected
and what is not, is in [Credentials](credentials-and-secrets.md) and
[Data and privacy](data-and-privacy.md#local-files).

## Model-call lanes (Claude Code)

Hooks see what the agent does, but not the request it sends to the model. The
**lanes** fill that gap. `init --provider claude-code` installs two, as
background services on your machine:

- **telemetry**: a local OTLP receiver. Claude Code sends its own telemetry
  here (token usage, model, timing). It never sits in the path of a model call.
  Codex gets this lane too.
- **transport**: a local HTTPS proxy. It decrypts traffic to the model
  provider's hosts using a certificate authority generated on your machine,
  records the request and response, and passes it on unchanged. Traffic to any
  other host is passed through without decryption.

Only one lane reports each model call, so token counts are never doubled.
`openbox doctor` shows which one and warns if it is not running.

Each lane is installed in a safe order: start the service, confirm it is
listening, and only then point the tool at it. If any step fails, the service
is removed and your settings are left untouched. Every setting the lanes
change is recorded with its previous value in `~/.openbox/activation.json` and
restored on uninstall. If you edited one of those values since, uninstall
refuses to overwrite it and tells you which.

Restart open sessions after installing or removing a lane: a running process
keeps the environment it started with. (Hook changes need no restart.)

**The transport lane refuses one kind of call:** a model call from a session
that is already halted. Everything else is recorded, not blocked.

**It detects bypass; it does not prevent it.** Unsetting one environment
variable routes around the lanes. What you get is a visible gap in the record
and an `openbox doctor` warning. Preventing it needs managed settings and
network egress control from your MDM.

### macOS: system-wide proxy and CA trust

On macOS, once the transport lane is running, `init` also:

1. trusts the lane's CA in the System keychain, and reads it back;
2. sets a proxy auto-config (PAC) URL on each enabled network service.

This extends coverage to desktop apps and browsers on the same hosts, for
example claude.ai chats. It asks for your `sudo` password once, and macOS may
ask again to confirm the trust change. If you decline, the lane still works
for the CLI and `init` prints the commands to finish later. A later `init`
does not ask again if nothing changed.

What this means: browser and desktop traffic to those hosts passes through a
local proxy that decrypts it with a key stored in `~/.openbox`. The CA is not
restricted to those hosts, so a leaked key could forge a certificate for any
site. If the lane stops, traffic goes direct and is not inspected.
`openbox uninstall` restores the network settings, untrusts the CA, then
deletes it.

Linux and Windows do not get this yet; `init` says so and changes nothing at
the OS level.

## Enforcement in practice

Every gated tool call and every prompt is sent to your platform before it
runs. The verdict decides what happens:

| Verdict | Result |
|---|---|
| allow | the action runs |
| block | this one action is refused |
| require approval | the session waits for an approver (see below) |
| halt | the session stops; every later prompt and tool call in it is refused |

If an event cannot be delivered (platform down, timeout, 5xx, 401), the action
is denied. If the platform explicitly refused or failed it, the session is
also halted. There is no setting to proceed instead.

A halted session stays halted until you start a new one. On Claude Code,
`/clear` and `--resume` start a fresh run and are not halted. On Codex,
`resume` continues the same run and stays halted.

## Approvals

When your policy requires approval, the call is filed with the platform and
the session waits (20 seconds by default, `approval_hold_ms`). An approver
decides from the dashboard, with their own login.

- Answered in time: the call proceeds.
- No answer: the call is denied, with the approval id in the reason. When the
  decision arrives later, Claude Code wakes the session with the outcome;
  Codex surfaces it as a finding instead.

You cannot approve your own request from the machine that made it.

## Upgrading an older install

Each tool now has its own agent in `~/.openbox/<tool>/`. Identities from older
versions (DID/seed identities, or credentials in the OS keychain) are **not
migrated** and not read. After upgrading, every gated call is denied until you
re-run `init`:

```bash
openbox doctor                        # "no agent" or "legacy" means re-init
openbox init --provider claude-code   # once per tool you use
openbox init --provider codex
```

This registers a new agent. History from the old agent stays with the old
agent. If you used `--secret-backend file` in the past, delete the old
`secrets.json` from the previous config directory; nothing reads it.

For a fleet: have the admin initialize the identity provider first, then
upgrade each machine and re-run `init` there.

## Uninstall

```bash
openbox uninstall
```

This removes everything: hooks, lanes, the CA and its key, settings, the event
queue and all credentials, including the organization key. It prints the full
list first, tries to deliver queued events, and restores every setting it
changed. It works even if credentials are already gone.

Credentials cannot be recovered afterwards. A later `init` registers a new
agent. Your organization's managed settings files are left alone.

There is no key rotation in place. Deleting a tool's `.env` and running `init`
registers a new agent. To keep the same agent on a new machine, answer yes to
the adopt prompt; that needs the agent's original workload private key.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `no agent identity for this tool, and no organization credential` | Run `openbox auth` or export `OPENBOX_CONTROL_TOKEN`, then re-run `init`. Or adopt an existing agent at the prompt. |
| `your org's identity provider is not initialized` | Ask your OpenBox admin to initialize it, then re-run `init`. |
| `your org issues agent identities from an external provider (Okta/Entra)` | Not supported: `openbox` registers only OpenBox-generated identities. |
| `looks like an AGENT RUNTIME key` | You pasted an agent key where the organization key goes. See [step 2](#2-get-the-right-credential). |
| `openbox auth needs a terminal` | Use [Automation and CI](#automation-and-ci). |
| Everything is denied after an upgrade | Old identities are not read. See [Upgrading](#upgrading-an-older-install). |
| `doctor` shows `401 identity rejected` | Usually a self-hosted core URL not set (re-run `auth` with both URLs, then `init` per tool). Otherwise the agent's key was revoked. |
| Token exchange returns 400 about the assertion or expiry | Your clock is off. The signed assertion is valid for 60 seconds; check NTP. |
| `OPENBOX_WORKLOAD_PRIVATE_KEY: not a PEM block…` | The key is truncated or in PKCS#1 format. Convert it: `openssl pkcs8 -topk8 -nocrypt -in key.pem -out key8.pem`. |
| Hooks never fire | On Codex, trust them with `/hooks`. On a managed machine, check `doctor`'s "can they run" line: managed policy may block user hooks. |
| Every prompt and tool call is refused | The session is halted: a delivery failed or a policy returned HALT. `doctor` shows why. Fix connectivity and start a new session. |
| `Session is no longer active` with no `(policy: …)` suffix | The platform's record of this session ended early (a known platform issue). Start a new session. |
| A tool call hangs | An approval is pending in the dashboard. |
| Every tool call appears twice | An OpenBox hook is registered twice. `doctor` reports it; re-running `init` fixes it. |
| Every model call fails after install | The lane is not running. Check `doctor` and `~/.openbox/transport.log`. `openbox uninstall` restores a working state immediately. |
| `… is already in use; refusing to continue` | Another program holds the lane's port. Stop it and re-run `init`. |
