# Full shift-left E2E; no mocks, real stack, real sessions

> **Scope.** What has to exist for shift-left to be verified end to end against the
> local OpenBox stack with **nothing stubbed**: real hooks, real `/evaluate`, real
> OPA, real Postgres, real MCP server, real commits, real deploys, real approvals.
> This is the plan; the partial testbeds it replaced are described in §1.

**Status: built and green** (`test/`, 2026-08-03). To run it:

```bash. test/env.sh          # settings; secrets come from test/.state or your env./test/env.sh mint     # once: the org credential the assertions read through./test/run-all.sh      # preflight → onboard → capture → enforce → approvals →
                          # lineage → visibility, then teardown ALWAYS./test/run-all.sh lineage visibility     # or just some phases; teardown still runs
```

Findings from the first full run are in §8a.

**Teardown is no longer opt-in.** The suite installs real supervisor daemons
under fixed labels and writes the real user-wide settings file, so it runs on
`trap EXIT INT TERM` — every way out, including Ctrl-C, gives the box back. A
second Ctrl-C during teardown aborts it and names what is left. And preflight
now REFUSES a host that already carries a real OpenBox install, because the
lane labels are fixed constants that collide regardless of `$HOME`: without
that refusal the suite would replace a developer's own units and the teardown
would then delete them.

## 1. Why this doc exists

Before this suite there were three partial testbeds, and none of them verified
the product: two scripts that drove the real binaries but posted to a **fake
sink** and asserted nothing, and a human read-along for the approval loop that
needed a person to watch it. Everything else, capture, enforcement, attestation,
lineage, was covered by unit tests plus hand-run SQL recorded in prose.

The cost showed up in the data: the lineage chain had never been exercised once
*as a chain*. The local evidence base was a single commit whose two
`deploy_session_links` rows were written **before** the read API and the DID
verifier that consume them existed. Unit tests cannot catch that; nothing was
asserting end to end.

(Those predecessors were removed once this suite covered them.)

## 2. What "no mocks" means here, precisely

**Replaced:** the human at the keyboard. `claude -p "<prompt>"` runs a real
Claude Code session; the same hook entries, the same spool, the same `POST
/api/v1/governance/evaluate`, the same OPA bundle. Nothing under test is
simulated; only the typing is. Codex has the equivalent non-interactive mode, so
adapter parity can be driven the same way.

**Not replaced by anything:** core, backend, OPA, guardrails, AGE, Postgres,
Temporal workers, the MCP server, git, the FE. All real, all local.

**Deliberately still absent** (§9): a production runtime session and GitHub-App
webhooks.

Verified Claude Code flags for the headless driver (against the installed CLI):
`-p/--print`, `--output-format json`, `--model`, `--mcp-config`,
`--strict-mcp-config`, `--allowedTools` / `--disallowedTools`,
`--permission-mode` (`plan` among the choices), `--settings`,
`--append-system-prompt`, `--no-session-persistence`. There is **no
`--max-turns`** in this version; bound work by prompt and tool allow-list
instead.

**One property of headless operation shapes the approval phase** (found while
building it): `claude -p` does not exit while the async rewake watcher is still
waiting on an **undecided** approval, and that watcher's window is 45 minutes.
Interactively this is invisible; the developer is still in the session. Headless
it means (a) the hook's answer must be read from `enforcements.jsonl` while the
session is still up, and (b) the marker directory is shared, so one leftover
undecided request hangs the *next* session too. `40-approvals.sh` therefore
drives sessions in the background and settles the queue between scenarios. The
upside: the session ending on a late decision is now a direct assertion that the
rewake watcher works.

## 3. Prerequisites; the real work is here, not in the scripts

| # | Prerequisite | Why | Status |
|---|---|---|---|
| P1 | **A read credential for assertions**; org key with `read:agent`, `read:agent_session`, `read:agent_log` | `60-visibility` and half of `50-lineage` assert through the backend read API, not SQL. This is the step that blocks a cold run today. | Minted once by `test/env.sh`, deactivated in `99-teardown` |
| P2 | **`test/env.sh`** | one place that knows the stack's URLs, the org, and where the credential lives; the predecessor scripts each carried their own copy | New |
| P3 | **The `everything` MCP server** (§6) | No MCP span exists anywhere in the local data; MCP capture is unproven end to end | New |
| P4 | **A decision credential**; the org key `test/env.sh` mints | Scenario F and every approval assertion need a credential that is *not* the developer's runtime key. Requests are decided through the dashboard's own REST route via `tb_decide`, which is what a person's click calls | Minted by `test/env.sh` |
| P5 | **Mechanism-B seeding** (`projects`, `agent_definitions`) or an explicit skip | `projects` is 0 rows, so the `lineage-architecture.md` §6 convergence is untested either way | Decide (§11 an owner decision) |
| P6 | Assert **through the read API** where one exists; SQL only where core has none (spans, Merkle leaves) | Otherwise the harness tests the database rather than the product | Convention |

## 4. Layout

```
test/
  env.sh              # stack URLs, org id, credentials, MCP config path (P1, P2)
  lib/assert.sh       # assert_eq / assert_gt / assert_json / assert_absent
  lib/sql.sh          # psql through the container, one row per call
  lib/session.sh      # drive a real headless Claude Code / Codex session
  lib/policy.sh       # the org gate policy + the approval queue (40 and 70 share it)
  mcp/everything.json # --mcp-config for the everything server (P3)
  00-preflight.sh
  10-onboard.sh
  20-capture.sh
  25-realtime.sh
  28-usage.sh
  30-enforce.sh
  35-telemetry.sh
  40-approvals.sh
  50-lineage.sh
  60-visibility.sh
  99-teardown.sh
  run-all.sh          # one tag per phase; its phases array is the authoritative list
```

Scripts are numbered because they share state deliberately: `50-lineage` needs
the session `20-capture` created, and `60-visibility` asserts what `50` wrote.
Each is individually re-runnable against an existing stack.

## 5. Coverage matrix

### 00-preflight
Container roster; `POST $OPA/v1/data/...` returns a decision (**not** just "OPA
is up"; a container bound to its own localhost looks healthy and enforces
nothing, the failure that started this suite); backend `:3000` answers 401
rather than connection-refused; core reachable; `governance-worker` and
`attestation-worker` both present. **Fails loudly rather than skipping**; a
green run on a half-dead stack is worse than no run.

### 10-onboard
Credentials provisioned directly — `openbox auth` prompts and takes no flags, so
a headless run writes the two files it would have written, which is one of the
two routes the product documents. Then real `openbox init --provider
claude-code` (enforce by default, every lane the provider supports) against
`http://localhost:3000`. Asserts: `agents` row with `agent_type=developer` and
`signing_required=t`; config written, with **no** `enforce` key, because nothing
can write one and an absent key resolves to on; hooks installed **user-wide**,
so every session on the machine is governed; `openbox doctor` reports every
posture flag with provenance, plus whether the control plane is reachable. A planted stale-engine copy of our own hook (the
residue of an `init` once run under a different `HOME`) is **replaced** on
re-init and the swap names what it retired; ownership is decided by argv shape,
so a genuinely foreign hook survives.

> **Trap:** enforcement written to the *global* config
> fail-closed-denies every Claude Code session on the box. The harness writes
> project-scoped hooks and exports enforce only into the driver's own environment.

### 20-capture
One headless session that uses **Read, Grep, Bash, Edit, and the `everything`
MCP server**. Asserts:

- `sessions` +1, `WorkflowStarted` → `WorkflowCompleted` present;
- **Tool calls are activity pairs**: every tool call is an `ActivityStarted`
  **and** an `ActivityCompleted` sharing one `activity_id`, counts equal, no
  unpaired completed row. Under the old hook shape both halves were
  `ActivityStarted` with the same `activity_id`, which matched core's whole
  dedupe key, so the completed half never became a row; this assertion is what
  proves it does now;
- `activity_type` is the tool name; at least one completed row carries a real
  `duration_ms` (> 0) and an `activity_output`;
- **`spans` is empty, asserted deliberately.** Dev sessions write zero span
  rows. This is the accepted trade-off, not a regression, and the assertion
  exists so a future reader does not "fix" it;
- Merkle: event leaves for both halves, and **no** span leaves;
- Tool classes reach core through `activity_input` rather than a server-computed
  span type; `kind` covers file/shell, and an MCP call carries its `mcp_server`
  + `mcp_tool`. That MCP assertion is the point of P3: before this suite no MCP
    call had ever reached the local stack;
- One `policy_evaluations` / `guardrails_evaluations` / `age_evaluations` row
  per evaluation;
- Spool drains at SessionEnd inside the flush budget;
> INV-1, INV-2 and INV-3 are defined once in [the contract's Invariants glossary](../dev-event-contract.md#invariants); this file cites them rather than restating them.

- **The privacy assertion (INV-2):** with content capture on, the prompt is
  present on the `prompt_submitted` signal **and so are the tool command and
  file body**: v1.3 retired the metadata-only posture, so this phase asserts the gate
  open and 35-telemetry.sh asserts it closed on a capture-off session. Inverting
  rather than deleting matters: "the marker is nowhere" and "the runtime emitted
  nothing" are the same observation, and only the positive form separates them;
- **The turn's thinking **, checked by KEY presence rather than by a marker
  string, and **skipped rather than failed** when the session produced no block.
  No prompt can make a model think a chosen phrase, and extended thinking is a
  client setting this suite does not control; so a marker assertion here would
  measure the model's compliance instead of the pipeline. The capture-off half
  in 35-telemetry.sh IS strict, because absence needs no cooperation from a
  model.

Its activity counts are scoped to **tool** activities (`activity_type is
distinct from 'llm_completion'`). A session also emits model-turn activities on
the same two wire types, so an unscoped count would let "4 tool calls captured"
pass on two tool calls plus two turns. Turn pairing lives in `28-usage`.

### 25-realtime
Telemetry reaches core **while the session is still running**
(`hookflow.RealtimeTrigger`, on by default; the phase deliberately sets no
`OPENBOX_REALTIME`, because the default posture is the claim). The proof is
**ordering, not latency**: the session's `WorkflowStarted` and tool activity are
queryable in core while the driver process is still alive. Then the join proves
completeness survived; exactly one `WorkflowStarted`/`WorkflowCompleted` pair,
so an overlapping realtime drain and end-of-session drain never double-count
(server-side `Idempotency-Key` dedupe).

### 28-usage
Per-turn model + token usage, and the arithmetic that makes it worth having.
**Counting is the assertion here, not existence**; every failure mode that
matters (a double-counted turn, an off-by-one cursor, a missed turn, a subagent
whose tokens are claimed twice) passes an existence check and fails a count. The
standing lesson is the duplicate-`ActivityStarted` bug on the escalation path,
which shipped because the only assertion that would have caught it ran in a mode
where the bug could not occur.

One session with a deterministic step sequence and one subagent task, at the
default posture (`OPENBOX_FINOPS` deliberately unset, so the phase also proves
the default). Asserts:

- **Pairing**: `llm_completion` Started count == Completed count; no unpaired
  Completed; no `activity_id` duplicated within a half;
- **Id shape**: ids carry the `<session>:turn:<n>` form, stored verbatim by
  core, a colon-shaped `activity_id` is new for a dev event, every earlier one
  was `cc-act-<hex>`, and none collides with the tool-call shape;
- **Contiguity**: main-thread turn indexes run `0..n-1` with no gaps. A gap is a
  window that was read and whose events never stored: a silently lost turn;
- **Payload**: every Completed half carries `activity_output` with a non-empty
  `model` and all four counts, none negative; the Started half carries **no**
  `activity_input` (a turn's input is the prompt, which rides the
  `prompt_submitted` signal under the content gate); **no cost** anywhere,
  because the client never derives one;
- **Reconciliation**: Σ per-turn == the `SessionEnd` rollup, **field by field**
  across all four counts. Two independent derivations of one quantity,
  comparable only because contract v1.1 stopped folding the cache counts into
  `input`. This is the assertion that catches double-counting;
- **Subagent**: separate `<session>:agent:<id>:turn:<n>` records, attributed via
  `agent_id`, complete pairs. Reported as an honest **skip** when no such
  records appear; which is itself the answer to the one question static analysis
  could not settle: whether `SubagentStop`'s window carries `isSidechain` lines;
- **INV-2, end to end**: the shell, file and prompt markers appear on **no**
  turn row, and no raw transcript timestamp does either. This is the only
  end-to-end proof that INV-2 holds after v1.1 replaced the
  projection's structural impossibility with an allowlist; the unit sentinel
  test is necessary, not sufficient, and this is the assertion a privacy
  reviewer should be pointed at;
- **Tool-metric pollution**: recorded, not asserted away. Core's
  `ExtractToolMetric` accepts any non-empty `activity_type`, so until the
  core-side exclusion ships, `llm_completion` also appears in the dashboards
  **as a tool**, with call counts and latency percentiles. The phase names the
  cause and links the core issue so nobody reads it as a shift-left defect; when
  core ships, the step flips to asserting absence;
- **The opt-out is real**: a second session with `OPENBOX_FINOPS=0` produces
  zero `llm_completion` rows, no token rollup, and no `model` beyond
  `SessionStarted`'s own hook field; while still capturing ordinary tool
  telemetry. A security assertion, not a feature test;
- **Posture evidence**: `SessionStarted` records `finops` true for the default
  session and false for the opt-out one. This is what makes a default-on egress
  defensible after the fact.

### 30-enforce
A **raw-rego** org policy that denies; the shape the deleted local evaluator
served fail-open, so these sessions used to proceed ungoverned; a file
containing a synthetic `AKIA…` and `sk-ant-…`; core killed for both
failure-policy branches; findings channel on. Asserts: the deny is sourced from
`evaluate` with a `policy_id`; a class that never used to escalate (`Write`) is
decided by the server and stored once; the written file contains
`OPENBOX_REDACTED` and the secret never egresses; fail-closed synthesizes a HALT
while fail-open proceeds **and is recorded** as ungoverned; redaction survives
the outage; findings surface in-session. Two later additions ride the same
phase: a published raw-rego **HALT ends the session**; applied as a session
stop, the turn stops before its follow-up write, the latch file appears under
`OPENBOX_HALT_DIR`, and a same-session replay is audited as
`source:"session-halt"`; and the `SessionStarted` posture names **who decides**
(`control_plane`) and the failure policy while making no bundle-integrity claim,
`doctor` agrees, and the retired `dev sync` fails loudly rather than appearing
to succeed.

### 35-telemetry
Tool outcome, the failure/lifecycle signals, and the one content-bearing turn
span. Its own phase because it needs a session that **fails** a tool call and
one that spawns a subagent; neither `20-capture` nor `28-usage` drives either,
and bending them into it would make their own counts noisier. The load-bearing
checks, in the order their failures matter: `status` on the completed row (Tool
Health can compute at all); a failed call stored `failed` (success% means
something); ONE span, `llm_completion` (Goal Alignment has text to score);
capture off ⇒ **no** span rows (the gate is real server-side, not just on the
wire); `signal_args` **populated** on the new signals and the session goal still
equal to the last `prompt_submitted` afterwards (v1.9: the payload is now the
policy input, and core's goal gate is what stops ~140 signals a session
overwriting the goal — an ungated core fails this check, which is the point of
running it); and capture off ⇒ **no `thinking` key** on any row, while the
turn's token numbers survive (otherwise "no content" would pass
for a client that stopped emitting turns). The single list a live run must
confirm is [`mapping.md`](../mapping.md) §7 items 15–24; the script is
that list's executable form and defers to it. **Dormant: written, never run**;
its own header says not to cite it as evidence until it has.

### 40-approvals
The five approval scenarios, scripted with timing, using the P4 approver
credential in a second process:

| Case | Assert |
|---|---|
| A; approved inside the hold | tool proceeds; `enforcements.jsonl` `source=approval:decided` + `ALLOW`; wall-clock under the hold budget |
| B; nobody answers | **deny**, never a silent allow and never the provider's own prompt; reason carries the governance event id |
| C; late approval | rewake watcher exits 2 exactly once; marker file removed exactly once; the re-run is **allowed and does not mint a second approval id** (the operation-identity regression) |
| D; rejected | blocks immediately on the decision, with the approver's reason |
| E; nothing to approve | `Read`/`Grep` byte-identical to observe mode; worker log quiet; the 30s ceiling costs nothing |
| F; MCP approval | an OPA rule gating `mcp__everything__*`; the queue row carries `arguments`, and carries `(not captured)` with content capture off |

### 50-lineage
A commit made **inside** the live session, then `openbox-git-action` for staging
and production. Asserts the trailer, the `refs/notes/openbox` mirror, the
`refs/notes/openbox-attest` envelope (`canonical_b64` / `sig_b64` / `did`,
`bundle_policy_id`, `bundle_sha256`), the Deploy events, and the
`deploy_session_links` rows; **plus the negative cases that have never been
produced locally**:

1. A human commit with **no trailer** → the honest gap hop, not a hidden one;
2. **Multi-session fan-in** → one link row per `metadata.sessions[]` entry;
3. An **unresolvable `run_id`** → `session_id` null, row still written;
4. **Re-deploy idempotency** → `run_id=deploy-<env>-<sha>` conflicts, no
   duplicate;
5. The **amber state**: a link with `verified=false` and no attestation
   envelope.

Today's only two rows are the same commit under two environments (`staging
verified=t attributed` vs `production verified=f inferred`); a useful asymmetry
that should become a pinned fixture rather than an accident.

### 60-visibility
`GET /lineage/deploys`, `GET /lineage/commits/:sha`, `GET
/lineage/sessions/:sessionId/chain`
(`openbox-backend/src/modules/project/lineage.controller.ts:31,46,59`; live
behind auth; they answer 401, not 404) plus the dashboard KPIs. Asserts the
per-hop evidence block matches the rows `50` just wrote, green **and** amber
**and** gap, and that the absent production-session hop renders as an explicit
gap. Also asserts org scoping (a second org's credential sees none of it) and
the `agent_lineage` feature gate.

### The autonomous approver phase is gone

`70-approver-auto.sh` exercised the approver **persona** — a second identity on
the developer's machine that read an org queue and decided other people's
requests, holding an organization credential that can create and rotate agents
fleet-wide. That persona is deleted; the dashboard calls the same REST route,
under a credential that never has to sit on a developer's machine at all.

The enforcement-side **hold** is untouched and still covered by `40-approvals`:
a gated call files a request and waits, an undecided request denies, and a late
decision wakes the session. That is the product; the CLI approver was a client.

### 99-teardown
Deactivate the policy and the P1 key, remove the scratch project, leave the DB
rows (they are the fixture the next run's assertions compare against).

## 6. The MCP server: `@modelcontextprotocol/server-everything`

The reference server from `modelcontextprotocol/servers` (`src/everything`), run
over stdio with no infrastructure:

```jsonc
// test/mcp/everything.json; used via --mcp-config, with --strict-mcp-config
{ "mcpServers": { "everything": {
    "command": "npx",
    "args": ["-y", "@modelcontextprotocol/server-everything@<pinned>"] } } }
```

`npx @modelcontextprotocol/server-everything` defaults to stdio; `sse` and
`streamableHttp` are the other transports (server README). **Pin the version**;
the tool roster is the fixture, and the harness should fail if it drifts.

Why this server rather than a hand-rolled one:

| Tool | What it buys the test |
|---|---|
| `echo` (verified in the server source, `tools/echo.ts`) | the deterministic happy path: a real `mcp__everything__echo` tool name exercising the adapter's `mcp_server`/`mcp_tool` extraction and core's span classification |
| `add` | a second tool, so "which MCP tool" is a real discriminator in policy and in the approval queue |
| `longRunningOperation` | the PreToolUse 30s ceiling, the duration stash, and the hold arithmetic; under real latency instead of a sleep |
| `printEnv` | the env-scrub negative test: the server inherits the hook environment, so this is where a leaked `OPENBOX_*` credential would show up |
| `sampleLLM`, `getTinyImage`, `annotatedMessage` | non-text and sampling content paths, which the mapper has never seen |
| resources / prompts | surface coverage beyond `tools/call` |

It also unlocks Scenario F above: an OPA rule requiring approval for
`mcp__everything__*` is the only way to test that an MCP escalation carries
`arguments` in the approval queue; a shell command tests the other
half.

Confirm the exact roster at build time (`claude mcp list` against the pinned
version) rather than trusting this table; the README is the source, and it
moves.

## 7. Identity: `openbox init` with `--role`

The test needs **two identities on possibly one machine**, a developer runtime
and an approver, and the CLI could only express the first. This is also the
smaller half of the autonomous-approver work.

> **SUPERSEDED.** `openbox init` is the only onboarding spelling, and the
> `openbox dev` namespace is gone with it — what `dev verify` proved is a
> `doctor` line now, in the place people look when something is already wrong.
> The approver persona and everything that supported it (`approver.json`, the
> install-time permission probe, the org credential in `~/.openbox/.env`, and
> `doctor`'s two-principal Identity section) are deleted rather than built out.
> Deciding an approval is a dashboard action under a credential that never has
> to reach a developer's machine.

**Surface**

```
openbox auth                                 # prompts; no flags at all
openbox init --provider <claude-code|codex>  # one flag, and it is required
openbox doctor
openbox uninstall
openbox version
```

**Config resolution**

| File | Read by |
|---|---|
| `~/.openbox/dev.json` | every hook, every adapter, `doctor`. Coordinates and posture; never a secret |
| `~/.openbox/.env` | the same, for the two secrets. Never a coordinate |

One store per field, and it is load-bearing rather than tidy: a copy of the DID
in `.env` reintroduces a stale-copy bug that reverted a corrected DID on every
install.

**Rules that keep this safe and cheap**

1. **`openbox dev` is removed, not deprecated.** Two onboarding spellings means
   two things to keep true in every doc and every message, which is how docs
   drift from code. Typing it now fails with an unknown-command error, and
   `TestDevVerbIsGone` pins it. What `dev verify` proved is a `doctor` line.
2. **The hook path resolves one config.** Role is not a runtime ambiguity
   because there is one role; the second config file and the resolver that
   chose between them are gone, along with the guard test that existed to stop
   an adapter reaching the wrong one — a compile error is the stronger guard.
3. **The control token stays out of argv, and off disk** (INV-1). Registering
   an agent reads it from the environment; nothing writes it to the plaintext
   credential file, because an organization key that can create and rotate
   agents fleet-wide is a far larger exposure than the agent key beside it.
4. **Enforcement is on and writes no key.** A bool defaulting to true cannot
   express "said nothing", so an install that wrote `enforce: true` would be
   indistinguishable from an explicit opt-in and would revert anyone who had
   opted out. `OPENBOX_ENFORCE=false` observes for one run.
5. **`openbox doctor` reports every posture flag with provenance**, plus
   whether the control plane is reachable and whether a managed policy allows
   this machine's hooks to run at all.

## 8. Sequencing

| Phase | Content | Est. |
|---|---|---|
| T1 | P1/P2 + `lib/` + `00`–`10` | 0.5 d |
| T2 | P3 + `20-capture` (incl. the MCP and privacy assertions) | 0.5 d |
| T3 | `30-enforce` + `40-approvals` (absorbs the old read-along) | 0.5–1 d |
| T4 | `50-lineage` with all five negatives + `60-visibility` | 1 d |
| T5 | §8 surface change, then the surface-reset sweep across every phase | 0.5 d |

~3 days for the harness, plus whatever it finds; and finding things is the
point: the capture and lineage paths have never been asserted against, only
observed.

## 8a. Findings from the first run

Full-suite result (2026-08-03, `./test/run-all.sh`):

| Phase | Result |
|---|---|
| preflight | 21 passed |
| onboard | 27 passed |
| capture | 18 passed, 1 skipped (no guardrails attached to the agent) |
| enforce | 18 passed |
| approvals | 25 passed |
| lineage | 25 passed |
| visibility | 30 passed, 1 skipped (the backend gap below) |
| approver | 18 passed, 1 skipped (`approve --auto` unbuilt) |
| teardown | 5 passed |

The harness earned its keep before it was finished. What it established, and
what it exposed:

**Now proven end to end for the first time**
- MCP capture: a real `mcp__everything__echo` call reaches core. Before this
  there were no MCP calls on the stack at all. (This originally asserted an
  `mcp_tool_call` **span**: tool calls carry no span, and
  the assertion moved to `activity_input.mcp_server`/`mcp_tool`. The finding
  stands; what is captured did not change, only where it is carried.)
- INV-2 in the observe posture: with capture on the prompt, the shell command
  text and the file body all egress; with capture off none of them do; both
  halves asserted against every row the session wrote, not just unit-tested.
  (Until v1.3 this read "the command and file body do not egress at
  all"; that was the retired metadata-only posture, and it is retired, not weakened by accident.)
- The whole approval loop unattended: approve-inside-hold, timeout→deny,
  late-approval→rewake (the session ends *because* the watcher saw the
  decision), reject, ungated cost, and an MCP escalation carrying its
  `arguments`.
- Lineage with its negatives: trailer, notes mirror, signed attestation
  envelope, two environments, fan-in, re-deploy idempotency, and a trailer-less
  commit that produces **no** invented link.
- The amber path: an unverified claim in `deploy_session_links` renders as
  `partial` in the read API's commit hop; the read side agrees with the row.

**Gap found in the read side** (openbox-backend, out of scope here):
`getCommitChain` anchors on `deploy_session_links`
(`lineage.service.ts:212-217`), so a **deployed commit with no authoring session
has no chain at all**; 404, rather than a chain whose session hop reads
`missing`. The deploy is visible in `/lineage/deploys` (asserted), but the one
view built to never hide a hop cannot render that commit. `60-visibility.sh`
records this as a skip naming the file and line rather than passing over it.

**Found by the clean-machine install test, now fixed:** `openbox init` had no
way to name a self-hosted **data plane**. The backend's registration reply
carries no core URL, so a local install onboarded successfully and then signed
every request at `https://core.openbox.ai`; the reachability check returned 401
"identity rejected", which reads as a broken install rather than a missing
setting. `openbox auth` asks for BOTH URLs and persists them, `openbox doctor`
prints the base URL it resolved alongside whether it authenticated there, and
`10-onboard.sh` asserts it landed in `dev.json`.

**Accepted as-is:** re-installing on a machine whose credentials are gone fails,
because the org still holds the agent and its one-time keys cannot be re-issued;
`--force` registers a distinctly-named agent. That is the honest behaviour and
the friction is accepted (brian, 2026-08-03).

**Expected-but-worth-knowing:** the new links come out `verified=false` (amber);
core's local DID verifier has no published key for a freshly registered agent,
so the honest state is an unverified claim. Both states are now represented in
the local data, which is what the read side needs.

## 9. What this cannot cover locally, and must say so

- **The production-session hop.** No producer exists; the chain is 4 of 5 nodes
  and the fifth must render as an explicit gap, never be quietly omitted.
- **GitHub-App webhooks** (Mechanism B); needs a real app plus a tunnel; seed
  instead (P5) or skip explicitly.
- **Real-KMS attestation verification**; superseded locally by the
  `KMS_PROVIDER=local` verifier, but it is a different code path than
  production.

## 10. Do-not-chase list

A harness that flags these as failures will be ignored within a week: core has
no health/read endpoints; `agent-promotion-worker` cannot run; Codex maps
approval to a hard deny with no rewake (different UX, not a bug); prompt text
egressing under content-capture-on is the documented posture, not a leak.

## 11. Open decisions

- **an owner decision**; **decided: `test/` at the repo root.** It is run, not read. Its
  local state (`test/.state/`) and any operator secrets (`test/env.local.sh`)
  are git-ignored.
- **an owner decision**; Seed Mechanism B (P5) or leave the convergence untested locally?
- **an owner decision**; **decided (brian, 2026-08-03): credentialed client.** No DID, no
  agent row: the approver's actions are not monitored as agent activity and the
  evaluating host is free to use, so there is nothing to attest or meter.
  Consequences and the additive upgrade path are in §8.
- **an owner decision**; Does any of this run in CI? The stack is 24 containers; the honest
  answer is probably a nightly on a self-hosted runner, with the unit suite
  staying the per-push gate.
