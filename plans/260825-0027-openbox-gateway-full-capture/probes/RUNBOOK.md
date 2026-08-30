# Phase 03 probe runbook

**Who runs this: you, not an agent.** The probes need your real Claude Code
credentials in both auth modes and they temporarily redirect your model traffic to
a localhost server. That is the wrong blast radius for an unattended session, and
"reading is not evidence" is exactly why these exist — so they must be run, not
inferred. Budget ~30 minutes.

**The one rule.** Reports record BEHAVIOURS, never tokens. The probe server
already reduces every credential to `(kind, length, sha256[:8])` before it can be
printed, so copy its output verbatim and it stays safe. Do not paste raw terminal
scrollback from anything else.

## 0. Setup (once)

The server is stdlib-only and deliberately not part of any module, so run it from
a scratch directory:

```bash
mkdir -p /tmp/obx-probe && sed '/go:build ignore/d' plans/260825-0027-openbox-gateway-full-capture/probes/probe-server.go > /tmp/obx-probe/main.go
```

Then, in a terminal you will keep watching:

```bash
cd /tmp/obx-probe && GOWORK=off go run main.go
```

It prints one block per request: method, path, header names with sensitive values
reduced, JWT claim keys with value shapes, and the request body's top-level keys.

## 1. P0 — does `ANTHROPIC_BASE_URL` redirect this auth mode?

**This is the probe that decides who pass-through auth can cover**, so run it once
per mode and record each answer separately. A request that ARRIVES answers yes for
that mode. Silence answers no — and silence is a real answer, not a failed probe.

Use a scratch directory outside this repo so nothing governed is involved.

### 1a. API-key mode

```bash
mkdir -p /tmp/obx-probe-proj && cd /tmp/obx-probe-proj
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ANTHROPIC_API_KEY=<your API key> claude -p "say ok"
```

Record: did a block appear on the server? Which `credKind` did it report? Did
`Authorization` (or `x-api-key`) arrive at all?

### 1b. Subscription-OAuth mode

Same command **without** `ANTHROPIC_API_KEY`, in a session authenticated the way
you normally are:

```bash
cd /tmp/obx-probe-proj && ANTHROPIC_BASE_URL=http://127.0.0.1:8787 claude -p "say ok"
```

Record the same three things. **If nothing arrives**, that is the P0-negative
branch: the gateway tier covers API-key/console orgs only, and that decision plus
the product docs must say so in those words. Per the plan's risk table, Track B
still proceeds — this scopes it, it does not cancel it.

### 1c. Also record

Whether the credential arrived **verbatim** — the pass-through claim depends on
it. The server cannot show you the value (by design), so compare its
`sha256=…` against the same header sent by `curl` with the same credential. Equal
fingerprints ⇒ verbatim.

## 2. P1 — is an org identifier matchable?

Two possible sources, and only the first is visible from a throwaway server:

**2a. From the credential.** If step 1b produced a `-- bearer is a JWT --` block,
read it: a claim whose value shape is `<uuid — MATCHABLE …>` is a candidate org or
account id. **Record the claim KEY NAME only.** If the bearer is opaque (no JWT
block printed), the credential carries nothing matchable — that is the
detection-only branch.

**2b. From the provider's response headers.** Not reachable here: this server IS
the provider, so it has no upstream headers to show you. Note it as unresolved and
carry it into phase 04, where the gateway has a real upstream. Do not guess.

**2c. Local account state.** Independently of the credential, check the shape of
what Claude Code stores locally about the account:

```bash
ls ~/.claude/ && jq 'paths(scalars) | join(".")' ~/.claude.json 2>/dev/null | sort -u | head -40
```

Record KEY PATHS only — no values. What phase 05 needs to know is whether an org
UUID and an email are readable at all, and under which keys.

## 3. Probe A — which refusal shape stops a call without a retry?

**Use the REAL gateway, not the throwaway server.** The gateway now has a probe
mode, added specifically so this probe exercises the code that will actually ship
rather than a stand-in that only resembles it — and so trying a candidate costs a
restart instead of a recompile:

```bash
openbox gateway --addr 127.0.0.1:8788 --refuse-all --refusal-status 403 --refusal-error-type openbox_policy_refusal
```

It announces itself loudly on stderr, because a gateway refusing everything looks
exactly like a gateway that is broken. It consults no policy and forwards nothing.

Then, in another terminal, one session per candidate:

```bash
mkdir -p /tmp/obx-probe-proj && cd /tmp/obx-probe-proj
ANTHROPIC_BASE_URL=http://127.0.0.1:8788 claude -p "say ok"
```

Candidates worth trying, restarting the gateway between each:

| `--refusal-status` | `--refusal-error-type` | why |
|---|---|---|
| 403 | `openbox_policy_refusal` | the provisional default — unlike any transience signal |
| 403 | `permission_denied` | reads more like a policy decision; may collide with client wording rules |
| 400 | `openbox_policy_refusal` | a request-level rejection rather than an authz one |
| 451 | `openbox_policy_refusal` | rarely special-cased by clients, so rarely matched by a retry rule |

Candidates the gateway REFUSES to run are already ruled out by the requirement —
`--refusal-status 429` or `503`, or one of the provider's own error-type literals —
so it fails fast rather than spending a session proving what is already known.

The old throwaway-server route still works if you prefer it, but it tests an
approximation:

```bash
cd /tmp/obx-probe && GOWORK=off go run main.go -refuse 403 -shape anthropic
```

For each, watch **the client**, not the server, and record:

- how many requests arrived for one prompt (>1 ⇒ it retried);
- what the session printed to the user (is the refusal legible, or does it read as
  an OpenBox bug?);
- whether the session continued, stopped, or disabled something for the rest of
  its life (the capability-rejection branch — the one shape we must not pick);
- whether it exited non-zero.

The deliverable is a NAMED shape: status + body, with the observed client
behaviour beside it. If every candidate retries or corrupts the session, that is
the descope branch — phase 06 becomes observe-only and prevention stays in the
hooks (plan risk table).

## 4. Teardown — do this before you forget

```bash
pkill -f "go run main.go"; pkill -f obx-probe
rm -rf /tmp/obx-probe /tmp/obx-probe-proj
```

`ANTHROPIC_BASE_URL` was only ever set per-command above, so nothing persists —
but confirm it is unset in any shell you kept open, and confirm no probe process
survived:

```bash
env | grep ANTHROPIC_BASE_URL; pgrep -fl "go run main.go"
```

Both should print nothing.

## 5. Write the reports

Two files, from the templates beside this one:

- `plans/reports/probe-260825-baseurl-auth-coverage.md` — P0 and P1
- `plans/reports/probe-260825-halt-rendering.md` — probe A

Then phase 03's remaining steps unblock: fill that decision's `TBD(probe)`
slots, and phase 04 can start against a recorded interface instead of a guessed
one.
