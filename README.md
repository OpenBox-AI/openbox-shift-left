# OpenBox Shift-Left

Governance for the AI coding tools your developers already use.

If your team uses [Claude Code](https://claude.com/claude-code) or [OpenAI
Codex](https://openai.com/codex), `openbox` lets your organization see and
control what those tools do: the prompts sent, the commands run, the files
changed, the tokens spent, and whether each action was allowed.

`openbox` is one small binary you install on each developer machine. It connects
to an **OpenBox platform** your organization already runs (hosted or
self-hosted). It is not a platform of its own.

## How it works

Claude Code and Codex can run a program at fixed moments, called **hooks**:
when a session starts, when a prompt is submitted, before a command runs, after
a file is edited. `openbox` installs itself as those hooks.

```
 claude / codex ──hook──▶ openbox ──▶ redact secrets locally
                                   ├─▶ OpenBox /evaluate ──▶ allow · deny · hold · halt
                                   └─▶ per-session queue ──▶ OpenBox (sessions, events, lineage)
```

For each hook call, `openbox`:

1. Turns the tool's native event into one normalized event, the same shape for
   every tool.
2. Removes secrets from any text on your machine, before anything is sent.
3. For a risky action (a tool call or a prompt), asks the platform's policy and
   applies the answer before the action runs. The platform is the only decider;
   there is no local copy of the policy.
4. Sends everything else to the platform shortly after, in order.
5. Stamps each git commit with the session that made it, so a deploy can be
   traced back to a session.

On Claude Code it also records the **model calls** themselves (the requests
sent to Anthropic), which hooks cannot see. See
[Getting started](docs/getting-started.md#model-call-lanes-claude-code).

## Requirements

- An OpenBox platform, and the URLs of its **backend** (control plane) and
  **core** (data plane). If you use the hosted service, the defaults are
  already correct.
- An **organization API key** from the dashboard: **Organization → API Keys**.
  It starts with `obx_key_` and needs `create:agent` and `read:agent`. A key
  that starts with `obx_` but not `obx_key_` is an *agent* key and will not
  work ([why](docs/getting-started.md#2-get-the-right-credential)).
- Your organization's identity provider set up once by an OpenBox admin. If it
  is not, `init` tells you so.
- macOS or Linux, with Claude Code or Codex installed. Windows builds but is
  not tested end to end.

## Quickstart

### 1. Install

```bash
curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
openbox version
```

This downloads a prebuilt binary, checks its hash, and puts it in
`~/.local/bin`. No Go toolchain needed.

### 2. Connect your organization

```bash
openbox auth
```

It asks three questions. Press Enter to keep a default.

```
Backend URL (control plane)   [https://api.openbox.ai]:
Core URL (data plane)         [https://core.openbox.ai]:
Organization control token (obx_key_… or JWT):   ← paste; not echoed
```

This only saves the URLs and the key on this machine. It registers nothing.

> **Self-hosting?** Answer **both** URL prompts with your own hosts. If you
> change only one, events go to the hosted core and fail with a 401.

### 3. Govern a tool

```bash
openbox init --provider claude-code
```

This registers an agent for Claude Code on your platform, saves its
credentials, and adds the hooks to your user-wide settings
(`~/.claude/settings.json`). Every Claude Code session on this machine is now
governed, in any folder, including sessions that are already open.

Using Codex too? Run `openbox init --provider codex`. Each tool gets its own
agent, so run `init` once per tool. Running it again is safe: it reuses the
existing agent.

### 4. Check it

```bash
openbox doctor
```

It shows each tool's identity, every setting and where its value came from,
whether the platform is reachable, and anything that is silently not working.

Then use your tool as normal (`claude` or `codex`). Nothing else needs to keep
running.

For self-hosting, CI setup, approvals, upgrades and troubleshooting, see
**[Getting started](docs/getting-started.md)**.

## Before you rely on it

- **Enforcement is always on.** Blocking, approvals and secret redaction apply
  from the first session. They act on your organization's policy, so until a
  policy is published nothing is blocked and you only get visibility.
- **It fails closed.** Each event gets one delivery attempt, and one retry if
  the platform timed out, could not be reached or returned a server error. If
  a governance check gets no answer, the action is denied. If an event still
  is not accepted after that, the rest of that session is also refused until
  you start a new one. `openbox doctor` shows halted
  sessions and why.
- **A HALT verdict from your policy ends the session.** A BLOCK verdict refuses
  only the one action.

## What leaves your machine

On by default, each with a switch in `~/.openbox/<tool>/dev.json`:

| What | Switch |
|---|---|
| Prompts, assistant replies and thinking, tool commands, file contents, tool output | `"content_capture": false` |
| Token counts and model id per turn | `"finops": false` |

Always sent: session and tool metadata (tool names, file paths, timing, exit
status) and commit trailers. Never sent: your credentials.

All text is scanned for secrets and redacted on your machine first, then cut
to 64KB. Redaction is keyword- and pattern-based, so a secret with no known
shape or label can get through. Details:
[Data and privacy](docs/data-and-privacy.md).

## Commands

| Command | What it does |
|---|---|
| `openbox auth` | Save your platform URLs and organization key. Prompts; takes no flags. |
| `openbox init --provider <claude-code\|codex>` | Register that tool's agent and install its hooks and settings. Run once per tool. |
| `openbox doctor` | Show what is in effect, where each value came from, and what is wrong. |
| `openbox uninstall` | Remove everything `openbox` installed, including credentials. |
| `openbox version` | Print the version. |

## Supported tools

| | Claude Code | Codex |
|---|---|---|
| Session and tool events | yes | yes |
| Enforcement | deny, hold for approval, redact, halt | deny, redact, halt (approval-required becomes deny) |
| Model-call capture | yes, via local lanes | token usage only |
| Org mandate | managed settings | `requirements.toml` |

Per-provider detail: [Provider coverage](docs/coverage.md).

## Known limitations

- **Credentials are in plaintext files.** They are `0600` on macOS and Linux
  and unprotected on Windows. Anything running as you, including the coding
  agent, can read them. A signed event proves which machine's key produced it,
  not that nobody tampered with it.
- **Coverage is per machine.** A machine that never ran `init` sends nothing.
  To make governance mandatory, deploy the managed settings in
  [`deployments/managed/`](deployments/managed/) with your MDM.
- **It stops mistakes, not a determined user.** Without managed settings, a
  developer can remove the hooks. Without network egress control, a developer
  can route model calls around the local lanes. Both are visible in the record
  and in `openbox doctor`, but neither is prevented.
- **Not yet verified against a live platform:** the model-call lanes, and
  Windows at runtime.

## Documentation

| Doc | Read it to |
|---|---|
| [Getting started](docs/getting-started.md) | install, configure, operate and troubleshoot |
| [Data and privacy](docs/data-and-privacy.md) | see exactly what is sent and how to turn it off |
| [Credentials and secret detection](docs/credentials-and-secrets.md) | see where keys live and what the redactor catches |
| [Architecture](docs/architecture.md) | understand the design and the code layout |
| [Lineage](docs/lineage.md) | trace a deploy back to a commit and a session |
| [Provider coverage](docs/coverage.md) | *(adapter authors)* what each tool's hooks can and cannot supply |
| [Event contract](docs/dev-event-contract.md) | *(adapter authors)* the normalized event schema |
| [Wire mapping](docs/mapping.md) | *(platform integrators)* how events map onto the platform API |

## Development

You need Go 1.27+ (`GOTOOLCHAIN=auto` fetches it). The repo is one Go module.

```bash
go build ./... && go vet ./...
go test -race -count=1 ./...                       # -count=1 is required
go test -run TestGovernanceEval -v ./cmd/openbox/  # end-to-end governance evals
```

Tests need no platform: they run the real hook binary against an in-process
fake. Read [`CLAUDE.md`](CLAUDE.md) and
[Architecture § Layout](docs/architecture.md#layout) before a change.

## License

[Apache License 2.0](LICENSE).
