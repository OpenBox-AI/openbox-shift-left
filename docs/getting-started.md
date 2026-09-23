# Getting started

This guide takes one developer machine from nothing to governed. It assumes
your organization already runs an OpenBox platform and that you have a login
to its dashboard. If you have never met this project, read the
[README](../README.md) first: it defines the handful of terms used below —
*governed*, *hook*, *agent*, *control token*, *posture*, *lane* — in one table.

Two commands do the setup. After them there is nothing to keep running and no
environment variable to keep set.

```
openbox auth                     connect your organization; two URLs and the control token
openbox init --provider <tool>   register that tool's agent, install the hooks
```

`auth` runs first, once. `init` runs once **per tool you govern** — each tool
gets its own agent. Run them out of order and each one tells you which to run
instead.

## Before you start

- [ ] The OpenBox platform is running and you know its two URLs (or you use
      the hosted service, whose defaults are already filled in).
- [ ] You have an **organization control token** from the dashboard, under
      **Organization → API Keys**. It starts with `obx_key_`. [Section 2](#2-get-the-right-credential)
      explains which key that is and why the other one will not work.
- [ ] You are on **macOS or Linux**. Windows compiles but is not yet tested end
      to end; see [What is not verified](#what-is-not-verified).
- [ ] **Claude Code** or **Codex** is installed. Examples below use Claude
      Code; Codex is the same two commands with `--provider codex`, and its
      differences are collected in [Using Codex instead](#using-codex-instead).

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
| Used for | registering each tool's agent, policy sync, approvals, rotation | the runtime itself, read from `~/.openbox/<tool>/.env` |
| Env var | `OPENBOX_CONTROL_TOKEN` | `OPENBOX_API_KEY`, written for you by `init` |

**You want the organization key**, and only to *register*; once a tool has an
agent, `init --provider <tool>` re-runs offline and needs no org key at all.
Note *when* it is needed: at **`init`** time, once per tool. `openbox auth` will
store it for you so `init` can find it later, or you can export it for the one
run. The agent runtime key is *minted by*
registration and is never an input to it; paste it as a control token and you
get a 401, and paste it in the API-key field and `auth` tells you which one you
used.

Permissions to grant the key, by what you intend to run:

| Command | Needs |
|---|---|
| `openbox init --provider <tool>` (registering that tool's agent) | `create:agent`, `read:agent` |
| policy reads (server-side, on every gated call) | `read:agent_policy` |
| deciding an approval (from the dashboard) | `read:agent_session`, `manage:agent_session` |

```bash
export OPENBOX_CONTROL_TOKEN=obx_key_…
```

It is never accepted as a flag, so it cannot leak through your shell history or
`ps`. Note the precedence, which is the reverse of every other value here:
`~/.openbox/.env` wins for the control token and this variable is the fallback.
That is deliberate — `auth` writes the token and `init` reads it in a separate
process, and while the variable won, a forgotten `export` in a long-lived shell
(or in a GUI editor's environment, which every terminal it spawns inherits)
silently overrode every `auth` run, with nothing saying why.

## 3. Connect your organization

```bash
openbox auth
```

Three questions, all prefilled with a sensible default or your current value, so
a re-run to change one URL is mostly pressing Enter. **`auth` registers nothing
and writes no agent credential**; it connects this machine to an organization
and stops there.

```
Backend URL (control plane)   [https://api.openbox.ai]:
Core URL (data plane)         [https://core.openbox.ai]:
Organization control token (obx_key_… or JWT):

✓ wrote ~/.openbox/.env       (0600; plaintext;)
✓ wrote ~/.openbox/dev.json   (URLs; no secrets)

Next: openbox init --provider <claude-code|codex>
```

The token is masked as you type, and **no flag ever takes a secret value**, so
nothing lands in your shell history. A blank answer keeps the stored one, which
is what makes a re-run to fix one URL safe.

It is persisted because `auth` takes it and `init` needs it, and those are two
separate processes — see [what that
costs](#where-your-credentials-live-and-what-that-costs) below, because this is
the credential with the largest blast radius on the machine. If you would rather
it never reach the disk, skip the prompt and export `OPENBOX_CONTROL_TOKEN` for
the one `init` run instead.

**Automation.** `auth` takes no flags and needs a terminal, so a machine without
one is provisioned directly. All three routes are read in preference to anything
`auth` writes, so none is a lesser path:

```bash
# 1. Export the org token and let init register each tool's agent. The one to
#    reach for: it is the only route that mints an identity. It applies when
#    ~/.openbox/.env holds no token of its own -- that file wins for this one
#    value, so a machine that has run `auth` uses what `auth` wrote.
export OPENBOX_CONTROL_TOKEN=obx_key_…
openbox init --provider claude-code
openbox init --provider codex

# 2. Or export an agent identity directly. Note the limit: these outrank every
#    store at once, so one exported DID makes EVERY governed tool report the
#    same identity. Right for a single-tool CI image, wrong for a developer
#    machine running two.
export OPENBOX_API_KEY=… OPENBOX_AGENT_PRIVATE_KEY=… \
       OPENBOX_AGENT_DID=did:aip:… OPENBOX_AGENT_ID=…

# 3. Or write the files by hand, one store per governed tool. This is also the
#    key-rotation route. OPENBOX_HOME relocates all of it.
umask 077
mkdir -p ~/.openbox/claude-code
printf 'OPENBOX_API_KEY=%s\nOPENBOX_AGENT_PRIVATE_KEY=%s\n' "$KEY" "$SEED" \
  > ~/.openbox/claude-code/.env
cat > ~/.openbox/claude-code/dev.json <<'JSON'
{ "agent_id": "…", "developer_did": "did:aip:…",
  "base_url": "https://…", "backend_url": "https://…" }
JSON
```

Keep them separate. `.env` holds only secrets and `dev.json` only coordinates:
a copy of the DID in `.env` reintroduces a stale-copy bug that reverted a
corrected DID on every install.

**`OPENBOX_CONFIG` is a single-tool knob.** It names one `dev.json` outright and
overrides the per-tool one — but it does *not* override `.env`, which stays per
tool. So two tools governed under one `OPENBOX_CONFIG` share a DID while holding
different agent keys, and events from at least one of them are signed by an
identity that DID does not match: the control plane answers 401, and a 401 never
spends a delivery attempt, so the spool grows and nothing says why. Use it for
one tool, or not at all.

**Rotating a key.** There is no rotate command. Either rewrite
`~/.openbox/<tool>/.env` by hand as above, or delete it and re-run `openbox init
--provider <tool>`: with no store present it offers to **adopt** an existing
agent, which takes the agent id, DID, key and seed and keeps the DID. Decline
and it registers a new agent with a new DID instead.

### Where your credentials live, and what that costs

One store per governed tool — `~/.openbox/<tool>/.env` — plus one org-level
`~/.openbox/.env` holding the control token. All of it in **plaintext**.
Relocate the whole directory with `OPENBOX_HOME`. This is a deliberate trade,
and it is worth understanding rather than skipping:

- **MacOS/Linux:** `0600` under a `0700` directory, so other local users cannot
  read it. Anything running **as you** can; including the coding agent under
  governance, which by design runs arbitrary commands as you.
- **Windows:** no at-rest protection at all. `0600` is a no-op there, so other
  local accounts can read the file. Use full-disk encryption.
- **It is the only copy.** OpenBox shows the API key and signing key exactly
  once and does not store them. Lose a tool's file and you rotate or
  re-register.
- **The org control token is the big one.** It can create and rotate agents
  across your whole organization, and `auth` writes it to `~/.openbox/.env`
  under exactly the conditions above: plaintext, readable by anything running as
  you, including the governed agent. It is never written to a per-tool store, so
  one compromised tool store is not a fleet compromise — and `openbox uninstall`
  deletes it along with everything else.
- **Never commit any of it.** It lives in your home directory rather than near a
  repo for that reason, and the files' own headers say so.

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
- Writes this tool's posture and identity to `~/.openbox/claude-code/dev.json`
  (the org-level `~/.openbox/dev.json` keeps only the two URLs);
- **Claude Code only:** sets `showThinkingSummaries: true` in that same user
  settings file, so a thinking block arrives with the model's own reasoning
  summary instead of an empty one. Whatever was there before -- absent,
  `false`, or anything else -- is recorded once, in
  `~/.openbox/claude-code-prior-settings.json`, and put back verbatim by
  `openbox uninstall`. There is no per-key opt-out; undoing it means
  uninstalling.

Before any of that, it settles this tool's **identity**, in this order:

1. **This tool already has an agent** (`~/.openbox/claude-code/.env` exists and
   is complete) → it is reused, offline, with no call to the platform. This is
   the normal re-run.
2. **No store yet, but you are at a terminal** → it asks whether to **adopt** an
   existing agent. Answer yes and paste that agent's id, DID, API key and signing
   key; the DID stays the same. This is how you rotate a key or move an agent to
   a new machine, and it needs no organization token.
3. **No store, and you declined (or there is no terminal)** → it registers a
   **new** agent with your organization control token, from `auth` or from
   `OPENBOX_CONTROL_TOKEN`, and writes the credentials it is given. If there is
   no token either, it stops, installs nothing, and names both ways to get one.

Registration mints a new DID. Work attributed to an old agent stays with the old
agent, so adopt when you can.

### Using Codex instead

Same two commands with `--provider codex`. Its hooks live in `~/.codex/hooks.json`,
user-wide like Claude Code's, and it gets its own agent in `~/.openbox/codex/`.
Four things differ, and each is deliberate (`internal/adapters/codex/README.md`):

- Codex asks you to **trust new hooks** with `/hooks` inside the tool before it
  will run them. Until you do, nothing is governed and nothing says so.
- An approval-required verdict is mapped to a **deny**, because Codex has no
  prompt to hand it to.
- Codex **cannot wake a session**, so a late approval decision reaches you
  through the findings channel instead.
- There are **no model-call lanes** for Codex. `init` says so rather than
  reporting a service it did not install.

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

- **The transport lane refuses one case; nothing else does.** A relayed call
  is forwarded unless its session's current run was already latched HALTed by
  some lane (hooks, telemetry, or transport itself) — that case is refused
  locally, before the call reaches the provider, with no round trip to the
  control plane. Every other verdict still has no in-path refusal: the
  gateway lane forwards regardless, and the synchronous, per-call
  server-verdict refusal path on both lanes exists and is unwired on purpose.
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
The `obx_` keys and signing seeds cannot be re-retrieved, for any tool, and the
organization control token goes with them: `openbox init --provider <tool>`
afterwards registers a **new** agent with a new DID, and `openbox auth` takes a
new token.

```bash
openbox doctor   # which lane is elected and why, and whether it is listening
```

### System-wide PAC and CA trust (macOS)

Once the transport lane above is listening, `openbox init` on macOS goes one
step further: it activates a system-wide PAC and trusts the relay's CA in the
System keychain, so desktop apps and browser sessions on the same governed
hosts are covered too, not only the CLI you ran `init` from. Linux and Windows
are not there yet; `init` prints "not yet supported on this OS in this build"
and touches nothing at the OS level on those platforms.

What happens, in order: **trust the CA and read it back first**, then set the
PAC URL (`http://127.0.0.1:<port>/proxy.pac`) on every *enabled* network
service and read that back too. You will be asked for your **sudo password
once**; macOS may ask **once more** to confirm the trust change. Every prior
value is recorded before the first write, so a killed run leaves something
`openbox init` can reconcile on the next try, and any failure partway through
rolls back what it already changed.

Four outcomes, all of which leave your lane installed and env-routed either
way:

- **Activated** — the PAC and trust took; the install report says so and names
  the disclosure below.
- **Declined** — you said no to the prompt. The report prints the exact manual
  commands to finish it yourself later.
- **Failed** — something after authorization did not work (a scope's PAC
  write, the trust read-back). Also prints manual commands where the failure
  point has them.
- **Not attempted** — no controlling terminal was available to ask at all
  (for example, a fully non-interactive CI run). Nothing was touched.

A second `openbox init` that finds its own prior activation still matching
what is actually live does **not** prompt again.

**What this means for your data, stated plainly:** desktop apps *and* browser
sessions on the governed hosts now pass through a local relay that decrypts
them with an OpenBox-held key. The CA is unconstrained, so a leaked
`~/.openbox` key could mint a certificate for any site this Mac is made to
trust it for — file mode `0600` is the protection, and `openbox uninstall`
removes the key. The PAC's `; DIRECT` fallback means a stopped relay lets that
traffic through uninspected; that is not egress control.

`openbox uninstall` reverses this **before** deleting the CA: it restores each
network service to what it held before (or switches it back off, when the
prior value was `(null)` — macOS cannot write an empty URL back), untrusts the
CA, and only then deletes the certificate and key. A declined or failed
restore still deletes the CA **key** — a trusted certificate with no matching
key cannot mint anything new — and prints the manual commands, with the
certificate's SHA-1, to clear the now-dangling trust entry by hand.

```bash
openbox doctor   # a "System PAC" section: the live per-scope state, drift
                  # against the record, and whether the CA is trusted by SHA-1
```

### Self-hosted OpenBox

Set **both** URLs at `auth` time, by answering both prompts ("Backend URL
(control plane)" and "Core URL (data plane)"). `auth` warns if you set a
self-hosted backend and leave the core at its hosted default.

```bash
openbox auth          # answer both URL prompts with your own hosts
```

The environment variables below are read at *runtime* and outrank the files, so
they are the override for one shell or a CI job. They do **not** prefill the
`auth` prompts, which prefill from what is already on the machine:

```bash
export OPENBOX_BACKEND_URL=http://localhost:3000   # openbox-backend, control plane
export OPENBOX_BASE_URL=http://localhost:8086      # openbox-core, data plane
```

The control plane cannot tell the CLI where your core is, so setting only one
leaves the other at its hosted default and sends every event to
`core.openbox.ai`; which comes back as a 401 that looks like a broken install.
`openbox doctor` prints the base URL it resolved and whether it authenticated
there, so you can check before you commit to it.

**`auth` writes these once, to the org config, and `init` copies them into each
tool's own config.** A re-run of `auth` that corrects a URL therefore has no
effect on an already installed tool until you re-run `openbox init --provider
<tool>` for it. `openbox doctor` flags the drift.

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

### Identity is per tool now, and your machine has none until you re-init

**Read this before upgrading.** Each governed tool carries its own agent, in its
own `~/.openbox/<tool>/` store. There is **no fallback**: a machine that was
working before this release has its identity at the old org-level path, and
nothing reads it any more.

**The symptom is silence.** Hooks still fire, fail to resolve credentials, and
fail open — so the tool works normally and nothing is governed. No error, no
blocked call, no events in the dashboard.

**The check and the fix:**

```bash
openbox doctor                        # one Identity row per tool; "no agent" is the answer
openbox init --provider claude-code   # once for each tool you use
openbox init --provider codex
```

`doctor` prints a row per store and names the remedy on any tool that has none.
`init` registers a fresh agent, which **mints a new DID**: work attributed to
the old one stays attached to the old one. If you still hold the old agent's key
and seed, answer **yes** to the adopt question and paste them instead — that
keeps the DID.

If `init` halts saying an agent with that name already exists, that is your old
agent: the name now carries the tool, but a previous machine may have claimed
it. Adopt it if you have its credentials, or delete it in the dashboard and
re-run.

`openbox auth` is now the organization connection only — two URLs and the
control token. It registers nothing, so running it will not fix an ungoverned
tool.

### Credentials from the keychain era

Credentials used to live in your OS keychain. **They are not migrated**;
keychain support is gone entirely, so the new binary cannot see them. Two ways
out, best first:

**1. Read them out of the keychain by hand and adopt them.** Run `openbox init
--provider <tool>`, answer yes to the adopt question, and paste them. No org key
needed for the values themselves, and it keeps the agent and DID you already
have:

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

**2. Register a fresh agent**; `openbox init --provider <tool>`, declining the
adopt question. Simplest, but the old agent's history no longer continues into
the new one, and each tool gets its own new agent.

Two more things to clean up:

- **The org `dev.json` migrates itself** on the first run of
  `auth` or `init`, from `~/Library/Application Support/openbox/` (macOS),
  `~/.config/openbox/` (Linux) or `%AppData%\openbox\` (Windows) into
  `~/.openbox/`. That is the org-level file only; each tool's own `dev.json` is
  written by `init --provider <tool>`, which copies the organization's URLs into
  it. The originals are left in place, so rolling back to an older binary still
  works. Runtime state, the spool and audit logs, stays where it is.
- **If you used `--secret-backend file`, delete its `secrets.json`** from the
  old config directory. Nothing reads it any more, and it is a stale plaintext
  copy of live credentials; worse than a current one, because nobody rotates it.

Then run `openbox init --provider <tool>` once. One install governs every
session on the machine, and enforcement is on by default.

## Changing your mind later

| Want to | Do |
|---|---|
| Turn enforcement off | `OPENBOX_ENFORCE=false` for one run, or `"enforce": false` in `~/.openbox/<tool>/dev.json` to persist it for that tool |
| Turn it back on | re-run `init`; enforce is the default |
| Get the prompt gate + HALT session stop on an existing install | re-run `openbox init --provider <tool>`; the prompt gate and its raised hook timeout are installer-registered, so an old registration keeps the old behavior until then |
| Change the organization connection | `openbox auth` (re-run it any time; blank keeps what is already there). Re-run `init` per tool afterwards to carry a changed URL across |
| Rotate a tool's agent key | rewrite `~/.openbox/<tool>/.env`, or delete it and re-run `openbox init --provider <tool>`, which offers to adopt the same agent so the DID stays |
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
| macOS, Linux | unit-tested, the governance evals green offline, and the CLI driven by hand; **nothing here has been run against a live stack** for this flow |
| Windows | **build-verified only**; CI cross-compiles every change; no automated suite runs there, and `install.sh` is bash |
| A managed-settings mandate | **not verifiable by us**; it needs a deployment in a real fleet. `openbox doctor` reports whether one is in force and whether it allows this machine's hooks to run |
| Credentials at rest | not protected on any platform; `0600` on macOS/Linux, nothing on Windows. That covers each tool's agent key and the organization control token alike |
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
| `no agent identity for this tool, and no organization credential` from `init` | This tool has no store and there is no token to register one with. Run `openbox auth` (it stores the token) or export `OPENBOX_CONTROL_TOKEN`, then re-run. Already hold this tool's agent key and seed? Re-run with a terminal and answer **yes** to the adopt question; that needs no org credential. |
| Governance stopped after an upgrade, with no error | Identity is per tool now and there is no fallback. Run `openbox doctor`; a tool with no store says so. See [Upgrading](#upgrading-an-existing-install). |
| `looks like an AGENT RUNTIME key` | You pasted the key from the Agent page into the API-key field, or an `obx_key_` org key where the runtime key belongs. See step 2. |
| `--org moved to openbox auth` | Both are gone. Run `openbox auth` to connect the organization, then `openbox init --provider <tool>` for each tool. `--org` did nothing even on auth; nothing read it. |
| `--secret-backend was removed` | There is no secret store to choose. Each tool's credentials are `~/.openbox/<tool>/.env`, written by `openbox init --provider <tool>`. |
| `no obx_ API key available` | No credential in the environment or `~/.openbox/<tool>/.env`. Upgrading from an older install? See [Upgrading](#upgrading-an-existing-install); neither keychain credentials nor an old org-level `.env` are migrated. |
| `the signing key is not valid base64` / `decodes to N bytes` | The pasted value is truncated. It is ~44 characters and usually ends in `=`. |
| `doctor` → `reachable NO; 401 identity rejected` | The data plane is wrong (usually a self-hosted core with only one URL set). `doctor` prints the base URL it resolved per tool; re-run `auth` answering BOTH URL prompts, then `openbox init --provider <tool>` for each tool to carry them across. |
| `an agent named … already exists in this org` | This machine was onboarded before and its one-time keys are gone from this store. They are not stored server-side, so that identity cannot be recovered: if you still have them, re-run `openbox init --provider <tool>` and answer **yes** to adopt, giving the agent id it names; otherwise delete that agent in the dashboard and re-run, which mints a **new** DID. |
| `agent … exists but has no signing identity provisioned` | A 404 that is **not** "unknown agent"; the agent is real but has no identity to rotate. Provision it, then rotate. |
| `openbox auth needs a terminal` | `auth` takes no flags. Use one of the three automation routes in step 3; the first, exporting `OPENBOX_CONTROL_TOKEN` and running `init` per tool, is the one that mints an identity. |
| Hooks never fire | The session was started before `init`, or you are in a directory where `init` was not run (project scope is the default). Restart the tool; for Codex run `/hooks` and trust them. |
| No events at all, and `doctor` looks fine | Check `doctor`'s `can they run` line. An org-managed machine can have the hooks installed, correct, and disabled by managed policy — the one failure that reports itself as nothing. |
| Everything is denied | `fail_closed` is on and OpenBox cannot be reached, so every gated call denies. `openbox doctor` shows the failure policy and the last verdict. Restore connectivity, or set `fail_closed:false` to proceed ungoverned instead. |
| `OpenBox governance: Session is no longer active`; the session stops, and every prompt after it is refused | Not your org's policy; a policy verdict names its policy in a `(policy: …)` suffix, and this one has none. The control plane's record of this session went terminal (e.g. a `SessionEnded` was recorded while the session was live) and it answers the next event with a HALT; since a HALT ends the session; the turn stops and a local latch refuses every later prompt/tool call in it. Fail-open does not apply, because a HALT is a verdict rather than an outage. **Start a new session**; the latch is per-session and a fresh session restores the server-side record. Known core defect, fix in flight. |
| A session refuses everything with `session halted by a governance HALT verdict` | Your org's policy (or the defect above) HALTed this session earlier; the latch under `~/Library/Application Support/openbox/halted-sessions/` (Linux: `~/.config/openbox/`) is replaying it, by design. Start a new session. The halting verdict and every refusal are in `enforcements.jsonl` (`source:"evaluate"` for the verdict, `source:"session-halt"` for the replays). |
| A session hangs on a tool call | An approval is filed and undecided. It is in the dashboard's queue; deciding it releases the session. |
| Every tool call appears twice; success rates and latencies look wrong | The directory has an OpenBox hook registered twice; usually a second engine left by an `init` once run with a different `HOME`. `openbox doctor` reports both that and a repeat at one path; re-running `openbox init` there removes the extra registration. Events already stored stay duplicated. |
| `OPENBOX_ED25519_SEED is deprecated` | Harmless, and it still works. Rename it to `OPENBOX_AGENT_PRIVATE_KEY`; the name OpenBox documents. |
| Every model call fails after an install | The elected lane is not listening and a dead loopback port fails closed. `openbox doctor` says whether it is alive; `~/.openbox/transport.log` and `telemetry.log` say why it exited; `openbox transport` in the foreground shows the same in real time. `openbox uninstall` gets you working again immediately. |
| `Model-call lanes: hooks only for codex` | The lanes read the Anthropic Messages API through Claude Code's own settings, so there is nothing for them to observe on Codex. Tool calls are still governed. |
| `… is already in use; refusing to continue` | Something other than an OpenBox lane holds that port. A lane *we* installed at that address is replaced instead, and the command says so. Stop the other process. |
| The gateway runs but nothing is recorded | Two causes, both in `~/.openbox/gateway.log`: no developer DID configured for claude-code (run `openbox init --provider claude-code`), or relayed calls carry no session header; the gateway will not invent a session, so those calls are recorded nowhere. |
| `openbox doctor` flags a bypass even though the gateway is healthy | By design. The exposure is reported at every state including the healthy one, because a check that goes quiet trains you to read silence as prevention. |

`openbox doctor` is the first thing to run for anything posture-related: it
prints every flag, its value, and whether it came from a default, your config,
the environment, or an org mandate. It also names the OpenBox engine(s)
registered in the directory you run it from, and warns when there is more than
one.
