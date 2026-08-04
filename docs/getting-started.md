# Getting started

Governance for your Claude Code or Codex sessions, ambient after one command. No
daemon to run, no environment variables to keep set.

Examples use `--provider claude-code`; substitute `--provider codex` and every step
is the same. Codex differs in three ways worth knowing up front
(`adapters/codex/README.md`): it asks you to trust new hooks via `/hooks` before
they run, it maps an approval-required verdict to a deny rather than a prompt, and
it cannot wake a session — so a late approval decision reaches you through the
findings channel instead of a system reminder.

## 1. Install the engine

```bash
curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
```

Downloads the prebuilt `openbox` binary for your platform (Linux/macOS,
amd64/arm64), verifies its checksum, and puts it on your PATH (`~/.local/bin` by
default). No Go toolchain needed. If no prebuilt asset matches your platform it
falls back to building from source, which does need Go 1.23+.

```bash
openbox version
```

## 2. Get the right credential

This is the one step people get wrong, because OpenBox has two kinds of key and
the dashboard shows both.

| | Organization key | Agent runtime key |
|---|---|---|
| Looks like | `obx_key_…` | `obx_…` / `obx_test_…` |
| Belongs to | your **organization** | one **agent** |
| Where you find it | dashboard → **Organization → API Keys** | dashboard → Agent detail (`openbox_api_key`) |
| Used for | onboarding, policy sync, approvals | the runtime itself, read from your keychain |
| Env var | `OPENBOX_CONTROL_TOKEN` | never yours to set |

**You want the organization key.** The agent runtime key is *minted by* onboarding
and is never an input to it — pass it and you get a 401. A Keycloak JWT from your
dashboard session also works, it is just short-lived.

Permissions to grant the key, by what you intend to run:

| Command | Needs |
|---|---|
| `openbox init` | `create:agent`, `read:agent` |
| `openbox dev sync`, session-start staleness checks | `read:agent_policy` |
| `openbox approve …` (human or autonomous) | `read:agent_session`, `manage:agent_session` |

```bash
export OPENBOX_CONTROL_TOKEN=obx_key_…
```

It is read from the environment and never accepted as a flag, so it cannot leak
through your shell history or `ps`.

## 3. Onboard

```bash
openbox init --provider claude-code \
  --backend-url https://<your-openbox-backend> \
  --enforce
```

That single command:

- registers a `developer` agent and stores its runtime key + signing seed in your OS
  keychain — never printed, never in a config file;
- installs the Claude Code plugin into `~/.claude/plugins/openbox-observe` and copies
  the engine into it, so every `claude` session is governed;
- pulls your org policy into a local bundle;
- writes your posture to `~/.config/openbox/dev.json`.

Drop `--enforce` for **observe-only** (telemetry and lineage, never blocks). With
`--enforce` the session also blocks, asks or redacts per your org policy, decided
in-process by the hook — nothing extra to start.

Add `--dry-run` to see the whole plan without touching anything.

### Self-hosted OpenBox

Add `--base-url` — the **data plane**, where events are sent and `dev verify`
authenticates:

```bash
openbox init --provider claude-code \
  --backend-url http://localhost:3000 \
  --base-url http://localhost:8086 \
  --enforce
```

The control plane cannot tell the CLI where your core is, so without this the
install succeeds and then signs every request against the hosted core — which comes
back as a 401 that looks like a broken install. `openbox dev verify --dry-run`
prints the base URL it resolved, so you can check before you commit to it.

## 4. Confirm it

```bash
openbox dev verify     # ✓ verified: did:aip:… @ https://…
openbox doctor         # every posture flag, and where its value came from
```

Then just work:

```bash
claude
```

Every session emits normalized telemetry, commits are stamped for lineage, and with
`--enforce` risky tool calls are gated locally. There is nothing to keep running.

## Approvals

With `--enforce` and Tier-2 on, a call your policy marks approval-required is filed
with OpenBox and the session pauses briefly (~20s, with a status message) while an
approver decides. Answer inside the pause — from the dashboard or
`openbox approve allow <id>` — and the tool call simply proceeds. If nobody
answers, the call is **denied** with the approval id in the reason, so the agent can
say what it is waiting on and do something else; when the decision lands later, the
session is woken with the outcome.

Two things worth knowing:

- **You cannot approve your own request.** Once a request is filed, the local
  "allow this once?" prompt is not offered: approving on the machine that made the
  request is a convenience control, not four-eyes.
- **The pause is tunable** — `approval_hold_ms` in `dev.json`, or
  `OPENBOX_APPROVAL_HOLD_MS`.

The approver's side is an ordinary API client, meant to run on the *approver's*
machine under their own credential:

```bash
openbox init --role approver --org <your-org> --backend-url https://<your-openbox>
openbox approve list --watch
openbox approve allow <id>
```

An approver install registers no agent and installs no hooks — it is a queue
client. To let one answer routinely without a person, see
[ADR-0012](adr/ADR-0012-autonomous-approver.md): it decides only inside an org
envelope, starts in shadow mode, and records every outcome.

## Changing your mind later

| Want to | Do |
|---|---|
| Turn enforcement off | `"enforce": false` in `~/.config/openbox/dev.json`, or re-run `init` with `--no-enforce` |
| Turn it back on | re-run `init … --enforce` |
| Stop sending prompt text | `"content_capture": false` (see [Data and privacy](data-and-privacy.md)) |
| Refresh policy by hand | `openbox dev sync` |
| Uninstall | remove `~/.claude/plugins/openbox-observe`, `~/.config/openbox/`, and the `openbox` binary |

A plain re-run of `init` never downgrades your posture silently: turning enforcement
off takes an explicit `--no-enforce`.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `set OPENBOX_CONTROL_TOKEN …` | The env var is unset, or you exported `OPENBOX_API_KEY` instead. |
| `looks like an agent runtime key` | You passed the key from the Agent page. Use an **Organization → API Keys** key (`obx_key_…`) — see step 2. |
| `policy read rejected (HTTP 401)` on init | The credential is valid-shaped but not accepted: wrong kind of key, expired JWT, or missing `read:agent_policy`. Registration still succeeded; fix the key and run `openbox dev sync`. |
| `dev verify` → `401 identity rejected` | The data plane is wrong (usually a self-hosted core with no `--base-url`). Check `openbox dev verify --dry-run`, then re-run `init` with `--base-url`. |
| `an agent named … already exists in this org` | This machine was onboarded before and its one-time keys are gone. Re-run with `--force` to register a distinctly-named agent, or delete the old one. |
| `HALT: no OS secret store` | No keyring on this machine. Install one (`secret-tool` on Linux) or opt in explicitly with `--secret-backend file` (0600 plaintext). |
| Hooks never fire | The plugin is materialised but not enabled, or the session was started before onboarding. Restart the tool; for Codex run `/hooks` and trust them. |
| Everything is denied | Fail-closed with no usable verdict. `openbox doctor` shows `fail_closed` and the bundle state; `openbox dev sync` refreshes a stale bundle. |
| A session hangs on a tool call | An approval is filed and undecided. `openbox approve list` shows it; deciding it releases the session. |

`openbox doctor` is the first thing to run for anything posture-related: it prints
every flag, its value, and whether it came from a default, your config, the
environment, or an org mandate.
