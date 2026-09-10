# Getting started

Governance for your Claude Code or Codex sessions. Two commands to set up, then
nothing to run and no environment variables to keep set; unless you opt into
[governing the model call itself](#governing-the-model-call-itself), which adds
a local daemon and one environment variable.

```
openbox auth     authenticate; credentials for this machine
openbox init     set up; install the hooks, in one project or fleet-wide
```

`auth` runs first. Each command fails with a pointer to the other if you get the
order wrong.

Examples use `--provider claude-code`; substitute `--provider codex` and most
steps are the same. Codex differs in four ways worth knowing up front
(`internal/adapters/codex/README.md`): it asks you to trust new hooks via
`/hooks` before they run, it maps an approval-required verdict to a deny rather
than a prompt, it cannot wake a session, so a late approval decision reaches you
through the findings channel. Its hooks live at `~/.codex/hooks.json`, which is
user-wide, the same place every provider's now go.

## 1. Install the engine

```bash
curl -fsSL https://raw.githubusercontent.com/OpenBox-AI/openbox-shift-left/main/install.sh | bash
```

Downloads the prebuilt `openbox` binary for your platform (Linux/macOS,
amd64/arm64), verifies its checksum, and puts it on your PATH (`~/.local/bin` by
default). No Go toolchain needed. If no prebuilt asset matches your platform it
falls back to building from source, which does need Go 1.27+.

```bash
openbox version
```

**Windows:** the binary compiles and CI cross-compiles it on every change, but
`install.sh` is bash and no automated suite exercises Windows at runtime. Build
from source with Go 1.27+, and see [What is not
verified](#what-is-not-verified).

## 2. Get the right credential

This is the one step people get wrong, because OpenBox has two kinds of key and
the dashboard shows both.

| | Organization key | Agent runtime key |
|---|---|---|
| Looks like | `obx_key_…` | `obx_…` / `obx_test_…` |
| Belongs to | your **organization** | one **agent** |
| Where you find it | dashboard → **Organization → API Keys** | dashboard → Agent detail (`openbox_api_key`) |
| Used for | registering an agent, policy sync, approvals, rotation | the runtime itself, read from `~/.openbox/.env` |
| Env var | `OPENBOX_CONTROL_TOKEN` | `OPENBOX_API_KEY`, written for you by `auth` |

**You want the organization key**, and only to *register*; once you have an
agent, `auth` needs no org key at all. The agent runtime key is *minted by*
registration and is never an input to it; paste it as a control token and you
get a 401, and paste it in the API-key field and `auth` tells you which one you
used.

Permissions to grant the key, by what you intend to run:

| Command | Needs |
|---|---|
| `openbox auth` (registering a new agent) | `create:agent`, `read:agent` |
| policy reads (server-side, on every gated call) | `read:agent_policy` |
| deciding an approval (from the dashboard) | `read:agent_session`, `manage:agent_session` |

```bash
export OPENBOX_CONTROL_TOKEN=obx_key_…
```

It is read from the environment and never accepted as a flag, so it cannot leak
through your shell history or `ps`.

## 3. Authenticate

```bash
openbox auth
```

Every field is prefilled with a sensible default or your current value, so a
first run is mostly pressing Enter; with one deliberate exception. **The agent
id is never prefilled: leave it blank and a new agent is registered for you**,
and it then stops asking, because registration returns the DID and both
credentials. Reusing a specific agent is the explicit act; type its id.

```
Organization                  [local]:                    acme
Backend URL (control plane)   [https://api.openbox.ai]:
Core URL (data plane)         [https://core.openbox.ai]:
Agent id (blank registers a new agent):

✓ wrote ~/.openbox/.env       (api key, signing key; 0600)
✓ wrote ~/.openbox/dev.json   (agent id, DID, URLs; no secrets)

Next: openbox init --provider claude-code
```

Give an existing agent id instead and it asks for that agent's DID, API key and
signing key. Secrets are masked as you type, and **no flag ever takes a secret
value**, so nothing lands in your shell history. Re-run `auth` any time to
change any of it; unlike `init`, which structurally could not update.

**Automation.** `auth` takes no flags and needs a terminal, so a machine without
one is provisioned directly. Both routes are read in preference to anything
`auth` writes, so neither is a lesser path:

```bash
# 1. Export the four variables and skip auth entirely.
export OPENBOX_API_KEY=… OPENBOX_AGENT_PRIVATE_KEY=… \
       OPENBOX_AGENT_DID=did:aip:… OPENBOX_AGENT_ID=…

# 2. Or write the two files auth writes. OPENBOX_HOME relocates both.
umask 077
printf 'OPENBOX_API_KEY=%s\nOPENBOX_AGENT_PRIVATE_KEY=%s\n' "$KEY" "$SEED" \
  > ~/.openbox/.env
cat > ~/.openbox/dev.json <<'JSON'
{ "agent_id": "…", "developer_did": "did:aip:…",
  "base_url": "https://…", "backend_url": "https://…" }
JSON
```

Keep them separate. `.env` holds only secrets and `dev.json` only coordinates:
a copy of the DID in `.env` reintroduces a stale-copy bug that reverted a
corrected DID on every install.

### Where your credentials live, and what that costs

`~/.openbox/.env`, in **plaintext**. Relocate the whole directory with
`OPENBOX_HOME`. This is a deliberate trade, and it is worth understanding rather
than skipping:

- **MacOS/Linux:** `0600` under a `0700` directory, so other local users cannot
  read it. Anything running **as you** can; including the coding agent under
  governance, which by design runs arbitrary commands as you.
- **Windows:** no at-rest protection at all. `0600` is a no-op there, so other
  local accounts can read the file. Use full-disk encryption.
- **It is the only copy.** OpenBox shows the API key and signing key exactly
  once and does not store them. Lose the file and you rotate or re-register.
- **Never commit it.** It lives in your home directory rather than near a repo
  for that reason, and its own header says so.

What that means for evidence: a signed event or commit attestation proves
*origin-of-config*, a machine holding this agent's key produced it, not
tamper-resistance against you or against the agent you run.

## 4. Govern this machine

```bash
openbox init --provider claude-code
```

That command:

- Installs the Claude Code plugin into `~/.claude/plugins/openbox-observe` and
  copies the engine into it;
- Merges the hook entries into your **user** settings (`~/.claude/settings.json`
  for Claude Code, `~/.codex/hooks.json` for Codex), so every session on this
  machine is governed. Hooks you added yourself are left alone; an OpenBox entry
  left behind at a *different* engine path, what an install run with another
  `HOME` leaves, is **replaced**, and one of ours that appears twice at the
  *same* path is collapsed. It also sweeps a superseded OpenBox entry out of the
  current directory's own settings file, so nothing registers the same gate
  twice. Either way the command prints what it removed;
- Writes your posture to `~/.openbox/dev.json`;
- **Claude Code only:** sets `showThinkingSummaries: true` in that same user
  settings file, so a thinking block arrives with the model's own reasoning
  summary instead of an empty one. Whatever was there before -- absent,
  `false`, or anything else -- is recorded once, in
  `~/.openbox/claude-code-prior-settings.json`, and put back verbatim by
  `openbox uninstall`. There is no per-key opt-out; undoing it means
  uninstalling.

It never reads, writes or prompts for a credential. If none is present it stops
and points you back at `auth`, installing nothing.

### Two defaults you should know

**It governs every session on this machine, in any directory, immediately.**
The tool watches its settings file, so sessions already running are governed
too; there is nothing to restart, and absence of events is therefore evidence
about the work rather than about the scope. `init` prints what it governed and
which file it changed, every time.

**It enforces.** Blocking, ask-for-approval and local secret redaction are on by
default; on tool calls AND on prompts: every gated tool call and every submitted
prompt is decided by your org's policy before it runs, and a **HALT verdict ends
the session on the spot** (the current turn stops, and every later prompt or
tool call in that session is refused locally until you start a new session).
BLOCK refuses just the one call or prompt. Two things keep that from being a
surprise you cannot recover from: enforcement acts on *your org's policy*, so
until your org publishes one nothing is blocked and you get observability either
way; and `fail_closed` stays **off**, so an OpenBox outage never blocks a tool
call. One diagnosed defect escapes both today; a control-plane precondition
failure expressed as a HALT, which now ends the session rather than denying
calls; the symptom and the recovery are in [Troubleshooting](#troubleshooting)
under `Session is no longer active`. Want telemetry without enforcement:

```bash
OPENBOX_ENFORCE=false claude    # observe only, for this run
```

It is an environment variable rather than a flag, and nothing is persisted
either way. That is deliberate: a stored `enforce: true` written by an install
is indistinguishable from one somebody chose, so the install writes no key at
all and an absent key resolves to on.

**What happens when OpenBox is unreachable** is one setting, and it is worth
knowing before you need it. Every gated tool call is decided by OpenBox, there
is no local policy to fall back on, so `fail_closed` decides what an unreachable
control plane means:

```jsonc
// ~/.openbox/dev.json
{ "fail_closed": true }   // deny gated calls when OpenBox cannot be reached
```

It defaults to **false**: gated calls proceed, and an outage never blocks work.
That also means enforcement depends on reachability; blocking one hostname
disables it. An org that needs enforcement to survive a developer who does not
want it sets `fail_closed: true` and accepts that an outage then blocks work.
Either way an org can pin the choice through the managed config so a developer
cannot change it, and `openbox doctor` always prints the effective value.

### Governing everything (the fleet rollout)

The install above already does. One `openbox init` registers the hooks in your
user-wide settings file, so **every session on this machine is governed**, in
any directory, and it takes effect immediately — the tool watches that file, so
sessions already running are governed too.

What a fleet rollout adds is not coverage but **irreversibility**. Deploy
managed settings with `allowManagedHooksOnly`, and governance stops being the
developer's to remove; see [`deployments/managed/`](../deployments/managed/).
That is an administrator's deployment through your own MDM, not something a CLI
does for one machine.

`openbox doctor` reports both halves: whether a managed policy is in force, and
— the failure that is otherwise silent — whether that policy allows this
machine's own hooks to run at all. An org-managed machine can have hooks
installed, correct, and never executed.

### Governing the model call itself

Everything above governs what the agent *does*. No hook carries the model
request, so hooks alone leave the model call unrecorded. The install closes that
for Claude Code, with no flag to set:

```bash
openbox init --provider claude-code   # hooks + telemetry + transport
openbox uninstall                     # all of it, back out
```

- **Telemetry** — a loopback OTLP receiver. The tool exports its own telemetry to
  it. Additive: it never sits in the path of a model call, so it cannot break
  one; but it is the tool reporting on itself, which is also its weakness.
- **Transport** — a loopback CONNECT proxy that terminates TLS for the provider's
  host with a CA generated on this machine, and tunnels every other host
  uninspected. It observes the real bytes.

Each installs the same way: write the unit, start it, **prove it is listening**,
and only then write the settings the tool reads. That order is the safety
property — writing the routing first would point your tool at a dead port, and a
dead loopback listener fails closed, so every model call on the machine would
fail. If any step fails, the unit is rolled back and the settings are left
alone; `init` says so and the rest of the install still stands. Both are Claude
Code only: a provider with no lanes gets the hooks and a line saying why, rather
than an error for something it never asked for. There is no Windows daemon
packaging yet, and `init` says so there rather than reporting a service it did
not install; `openbox telemetry` and `openbox transport` still run in the
foreground, supervised by whatever you choose.

A third lane, an `ANTHROPIC_BASE_URL` gateway, is no longer installed — the
transport relay sees what it saw without needing the tool to honour a base URL.
If a previous install left one behind, `init` **retires** it and prints what it
retired, restoring any `ANTHROPIC_BASE_URL` of your own that the old install had
displaced (remembered in `~/.openbox/gateway-prior-env.json`). `openbox gateway`
still runs in the foreground if you want that path deliberately.

**Only one lane reports each model call.** They all describe the same call, so
if two of them reported it every token count you see would be doubled. OpenBox
picks one automatically — the in-path lanes outrank telemetry, because they see
the real bytes — and `openbox doctor` prints which one and why. It also warns
when the elected lane has nothing listening behind it, which is the one state
where every other line still looks healthy and nothing is being recorded at all.

Four things to know:

- **They capture; they do not refuse.** A relayed call is always forwarded. The
  refusal path exists and is unwired on purpose.
- **The assurance is detection.** Unsetting the routing is enough to go around a
  lane, and nothing here stops that; what you get is a queryable hole in the
  record. See [the MDM recipe](gateway-mdm-recipe.md) if you need more.
- **Your own settings come back.** Every key OpenBox writes is recorded with
  whatever was there before, in `~/.openbox/activation.json`, and removal puts
  it back. A corporate `HTTPS_PROXY` or `NO_PROXY` survives the round trip;
  `NO_PROXY` is merged rather than replaced while a lane is active. If a value
  changed after OpenBox set it, removal **refuses** and names the key rather
  than overwriting your edit — nothing overrides that, because a corporate proxy
  value silently reverted is an outage.
- **Your open sessions keep their old routing.** A running process keeps the
  environment it started with, so restart it after installing or removing a
  lane. This is the one place that caveat applies: hook changes are picked up by
  the tool's own file watcher and need no restart.

`openbox uninstall` **deletes data, and it is a full purge**: the hooks on every
surface, the plugin bundle, all three lane units, the CA and its private key,
the lane logs, the activation record, posture, the spool, and your credentials.
It prints the whole inventory before touching anything, flushes the spool first
and reports what could not be delivered as loss, and it works on a machine whose
credentials are already gone — an offboarded laptop can always stop routing.
The `obx_` key and the signing seed cannot be re-retrieved: `openbox auth`
afterwards registers a **new** agent with a new DID.

```bash
openbox doctor   # which lane is elected and why, and whether it is listening
```

### Self-hosted OpenBox

Set **both** URLs at `auth` time. `auth` prompts for them ("Backend URL
(control plane)" and "Core URL (data plane)"), and these environment variables
prefill the prompts:

```bash
export OPENBOX_BACKEND_URL=http://localhost:3000   # openbox-backend, control plane
export OPENBOX_BASE_URL=http://localhost:8086      # openbox-core, data plane
openbox auth
```

The control plane cannot tell the CLI where your core is, so setting only one
leaves the other at its hosted default and sends every event to
`core.openbox.ai`; which comes back as a 401 that looks like a broken install.
`openbox doctor` prints the base URL it resolved and whether it authenticated
there, so you can check before you commit to it.

## 5. Confirm it

```bash
openbox doctor   # every posture flag and where its value came from, plus
                 # whether this machine reaches and authenticates to core
```

Then just work:

```bash
claude
```

Sessions in governed directories emit normalized telemetry, commits are stamped
for lineage, and risky tool calls are gated locally. There is nothing to keep
running.

## Approvals

With enforce on, a call your policy marks approval-required is filed with
OpenBox and the session pauses briefly (~20s, with a status message) while an
approver decides. Answer inside the pause, from the dashboard, and the tool call
simply proceeds. If nobody answers, the
call is **denied** with the approval id in the reason, so the agent can say what
it is waiting on and do something else; when the decision lands later, the
session is woken with the outcome.

Two things worth knowing:

- **You cannot approve your own request.** Once a request is filed, the local
  "allow this once?" prompt is not offered: approving on the machine that made
  the request is a convenience control, not four-eyes.
- **The pause is tunable**; `approval_hold_ms` in `dev.json`, or
  `OPENBOX_APPROVAL_HOLD_MS`.

**Whoever answers does so from the dashboard**, under their own credential.
There is no second CLI persona to install: a queue client on a developer's
machine had to hold an organization credential that can create and rotate agents
fleet-wide, which is a far larger exposure than the agent key beside it, and the
dashboard calls the same route.

Approving on the machine that filed the request is refused by default.

## Upgrading an existing install

Credentials used to live in your OS keychain. **They are not migrated**;
keychain support is gone entirely, so the new binary cannot see them. Two ways
out, best first:

**1. Read them out of the keychain by hand and paste them into `openbox auth`.**
No org key needed, and it keeps the agent and DID you already have:

```bash
# macOS
security find-generic-password -s ai.openbox.dev -a '<org>/<provider>/api_key' -w
security find-generic-password -s ai.openbox.dev -a '<org>/<provider>/private_key' -w
security find-generic-password -s ai.openbox.dev -a '<org>/<provider>/did' -w

# Linux
secret-tool lookup service ai.openbox.dev account '<org>/<provider>/api_key'
secret-tool lookup service ai.openbox.dev account '<org>/<provider>/private_key'
secret-tool lookup service ai.openbox.dev account '<org>/<provider>/did'
```

`<org>` is your organization namespace or `local` if you never passed `--org`;
`<provider>` is `claude-code` or `codex`. Delete the keychain entries once the
values are copied out.

**3. Register a fresh agent**; `openbox auth` with a blank agent id. Simplest,
but the old agent's history no longer continues into the new one.

Two more things to clean up:

- **`dev.json` migrates itself** on the first run of
  `auth` or `init`, from `~/Library/Application Support/openbox/` (macOS),
  `~/.config/openbox/` (Linux) or `%AppData%\openbox\` (Windows) into
  `~/.openbox/`. The originals are left in place, so rolling back to an older
  binary still works. Runtime state, the spool and audit logs, stays where it
  is.
- **If you used `--secret-backend file`, delete its `secrets.json`** from the
  old config directory. Nothing reads it any more, and it is a stale plaintext
  copy of live credentials; worse than a current one, because nobody rotates it.

Then run `openbox init --provider <tool>` once. One install governs every
session on the machine, and enforcement is on by default.

## Changing your mind later

| Want to | Do |
|---|---|
| Turn enforcement off | `OPENBOX_ENFORCE=false` for one run, or `"enforce": false` in `~/.openbox/dev.json` to persist it |
| Turn it back on | re-run `init`; enforce is the default |
| Get the prompt gate + HALT session stop on an existing install | re-run `openbox init --provider <tool>`; the prompt gate and its raised hook timeout are installer-registered, so an old registration keeps the old behavior until then |
| Change a credential | `openbox auth` (re-run it any time) |
| Replace credentials | `openbox auth`; blank keeps what is already there |
| Stop sending prompt text | `"content_capture": false` (see [Data and privacy](data-and-privacy.md)) |
| Stop sending token counts | `"finops": false` |
| Stop forcing `showThinkingSummaries` (Claude Code) | `openbox uninstall`; it restores whatever that key held before `init` -- there is no narrower opt-out |
| Govern model calls too | already done on Claude Code: `init` installs the lanes (see [above](#governing-the-model-call-itself)) |
| Stop governing model calls | `openbox uninstall`; it is all or nothing, because a machine with hooks and no lane records tool calls while its model calls go unseen |
| Uninstall | `openbox uninstall`; it detects what is installed, prints the inventory, and removes hooks, lanes, the CA, posture, the spool and your credentials. It needs no credential to run |

A plain re-run of `init` never downgrades your posture silently: turning
enforcement off takes an explicit `OPENBOX_ENFORCE=false` or a config key, and `init` says so when it
does.

## What is not verified

Being precise about this is part of the product
([Assurance](architecture.md#assurance) has the full
list). Specific to setup:

| | Status |
|---|---|
| macOS, Linux | unit-tested, and the CLI driven by hand; the end-to-end suite (`test/`) has **not been run** against a live stack for this flow |
| Windows | **build-verified only**; CI cross-compiles every change; no automated suite runs there, and `install.sh` is bash |
| A managed-settings mandate | **not verifiable by us**; it needs a deployment in a real fleet. `openbox doctor` reports whether one is in force and whether it allows this machine's hooks to run |
| Credential at rest | not protected on any platform; `0600` on macOS/Linux, nothing on Windows |
| the `ANTHROPIC_BASE_URL` gateway | **never run against a live stack**, and no longer installed. It governs the terminal CLI and **not** the desktop app; that much is measured (2026-08-27). Whether subscription-OAuth traffic follows `ANTHROPIC_BASE_URL` for *that* lane was never settled; the two installed lanes exist for the gap |
| telemetry, transport | **verified by replay only, and never run against a live stack.** Real recorded model calls run through the shipped code on a host that cannot bind a socket: that proves the bytes, the mapping, the gate and the caps, and proves nothing about bind, listen, TLS to a real socket, or what the control plane stores. The desktop-app and OAuth coverage they exist for is **unconfirmed**. The OTLP intake itself is not in that list; a synthetic export has crossed it end to end on a bind-capable host. But that export was **JSON** and the tool is configured to send **protobuf**, so the decoder real traffic will use is untested, and the real client has never exported to this lane |
| The telemetry lane's env keys | **unconfirmed against the client.** They are copied verbatim from a set proven in a sibling lab run and pinned as a literal list, but every test asserts JSON we wrote and Claude Code silently ignores a name it does not recognise; so a rename gives a green suite and a receiver that never gets a record |

So: **no platform is end-to-end verified for this setup flow yet.** The
commands, the credential file, the scope default and the enforce default are all
covered by tests and by hand-driven runs of the real binary; but "a governed
session produces events" has not been re-confirmed against a live stack since
the flow changed.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `no credentials on this machine` from `init` | Run `openbox auth` first. `init` never writes a credential. |
| `set OPENBOX_CONTROL_TOKEN …` from `auth` | You left the agent id blank, so it is registering; that needs an org key. Give an existing agent id instead if you have one. |
| `looks like an AGENT RUNTIME key` | You pasted the key from the Agent page into the API-key field, or an `obx_key_` org key where the runtime key belongs. See step 2. |
| `--org moved to openbox auth` | Both are gone. `init` no longer registers agents; run `openbox auth`, then `openbox init --provider <tool>`. `--org` did nothing even on auth; nothing read it. |
| `--secret-backend was removed` | There is no secret store to choose. Credentials are `~/.openbox/.env`; run `openbox auth`. |
| `no obx_ API key available` | No credential in the environment or `~/.openbox/.env`. Upgrading from an older install? See [Upgrading](#upgrading-an-existing-install); keychain credentials are not migrated. |
| `the signing key is not valid base64` / `decodes to N bytes` | The pasted value is truncated. It is ~44 characters and usually ends in `=`. |
| `doctor` → `reachable NO; 401 identity rejected` | The data plane is wrong (usually a self-hosted core with only one URL set). `doctor` prints the base URL it resolved; re-run `auth` and answer BOTH URL prompts. |
| `an agent named … already exists in this org` | This machine was onboarded before and its one-time keys are gone. They are not stored server-side, so that identity cannot be recovered: if you still have them, re-run `auth` and give the agent id it names; otherwise delete that agent in the dashboard and re-run, which mints a **new** DID. |
| `agent … exists but has no signing identity provisioned` | A 404 that is **not** "unknown agent"; the agent is real but has no identity to rotate. Provision it, then rotate. |
| `openbox auth needs a terminal` | Non-interactive with no `--*-stdin` flags. Use the automation form in step 3, or export the `OPENBOX_*` variables. |
| Hooks never fire | The session was started before `init`, or you are in a directory where `init` was not run (project scope is the default). Restart the tool; for Codex run `/hooks` and trust them. |
| No events at all, and `doctor` looks fine | Check `doctor`'s `can they run` line. An org-managed machine can have the hooks installed, correct, and disabled by managed policy — the one failure that reports itself as nothing. |
| Everything is denied | `fail_closed` is on and OpenBox cannot be reached, so every gated call denies. `openbox doctor` shows the failure policy and the last decision. Restore connectivity, or set `fail_closed:false` to proceed ungoverned instead. |
| `OpenBox governance: Session is no longer active`; the session stops, and every prompt after it is refused | Not your org's policy; a policy verdict names its policy in a `(policy: …)` suffix, and this one has none. The control plane's record of this session went terminal (e.g. a `SessionEnded` was recorded while the session was live) and it answers the next event with a HALT; since a HALT ends the session; the turn stops and a local latch refuses every later prompt/tool call in it. Fail-open does not apply, because a HALT is a verdict rather than an outage. **Start a new session**; the latch is per-session and a fresh session restores the server-side record. Known core defect, fix in flight. |
| A session refuses everything with `session halted by a governance HALT verdict` | Your org's policy (or the defect above) HALTed this session earlier; the latch under `~/Library/Application Support/openbox/halted-sessions/` (Linux: `~/.config/openbox/`) is replaying it, by design. Start a new session. The halting verdict and every refusal are in `enforcements.jsonl` (`source:"evaluate"` for the verdict, `source:"session-halt"` for the replays). |
| A session hangs on a tool call | An approval is filed and undecided. It is in the dashboard's queue; deciding it releases the session. |
| Every tool call appears twice; success rates and latencies look wrong | The directory has an OpenBox hook registered twice; usually a second engine left by an `init` once run with a different `HOME`. `openbox doctor` reports both that and a repeat at one path; re-running `openbox init` there removes the extra registration. Events already stored stay duplicated. |
| `OPENBOX_ED25519_SEED is deprecated` | Harmless, and it still works. Rename it to `OPENBOX_AGENT_PRIVATE_KEY`; the name OpenBox documents. |
| Every model call fails after an install | The elected lane is not listening and a dead loopback port fails closed. `openbox doctor` says whether it is alive; `~/.openbox/transport.log` and `telemetry.log` say why it exited; `openbox transport` in the foreground shows the same in real time. `openbox uninstall` gets you working again immediately. |
| `Model-call lanes: hooks only for codex` | The lanes read the Anthropic Messages API through Claude Code's own settings, so there is nothing for them to observe on Codex. Tool calls are still governed. |
| `… is already in use; refusing to continue` | Something other than an OpenBox lane holds that port. A lane *we* installed at that address is replaced instead, and the command says so. Stop the other process. |
| The gateway runs but nothing is recorded | Two causes, both in `~/.openbox/gateway.log`: no developer DID configured (run `openbox auth`), or relayed calls carry no session header; the gateway will not invent a session, so those calls are recorded nowhere. |
| `openbox doctor` flags a bypass even though the gateway is healthy | By design. The exposure is reported at every state including the healthy one, because a check that goes quiet trains you to read silence as prevention. |

`openbox doctor` is the first thing to run for anything posture-related: it
prints every flag, its value, and whether it came from a default, your config,
the environment, or an org mandate. It also names the OpenBox engine(s)
registered in the directory you run it from, and warns when there is more than
one.
