# Phase 07 — Local daemon, doctor, and MDM enablement

## Context links

- Parent: [plan.md](plan.md) · Previous: [phase-06](phase-06-gateway-enforcement.md)
- Scope amendment: [phase-03](phase-03-decisions.md) →
- Settings precedence: https://code.claude.com/docs/en/settings
- Assurance tiers: that decision (phase 03)
- Depends on: 06

## Overview

- Date: 2026-08-25 (local-gateway revision)
- Description: make the gateway a supervised local service a developer gets from
  `openbox init`, make its health and bypass exposure visible through `doctor`, and ship
  the artifacts an MDM-capable org needs to harden it — without OpenBox operating anything.
- Priority: P1
- Implementation status: **all seven requirements built.** `openbox init --gateway`
  installs, starts, verifies and points the machine at the gateway;
  `--remove-gateway` reverses it. Reqs 3-6 as below. Req 7 (Windows deferred) is an
  explicit error rather than a silent skip.
- Review status: not reviewed

## Key insights

- **Project scope still does not work for this config.** `ANTHROPIC_BASE_URL` is read from
managed settings and `~/.claude/settings.json` where the Desktop app manages the
connection; background agents need settings, not shell exports. The that decision
amendment stands — the value now points at `127.0.0.1`, not an org host.
- **The env block shrinks to one owned key.** Pass-through deleted `ANTHROPIC_AUTH_TOKEN`;
  `forceLoginMethod` dropped to optional org-side hardening. Less to write is less to
  revert wrongly — but the `flagPassed` lesson still applies to what remains.
- **Supervision is the availability story.** launchd (macOS) / systemd user unit (Linux)
  with keep-alive: a crashed gateway restarts; a stopped one leaves model calls failing
  against a dead localhost — the safe direction, and `doctor` must say WHY calls fail.
- **Doctor's job is the detection tier.** Liveness, config target (does the active
  settings scope point at our port), config ownership (root-owned = MDM tier active,
  user-owned = base tier), and bypass exposure — framed as visibility, never prevention.
- **Rotation caveat survives:** an already-running Claude Code supervisor keeps the launch
  configuration it started with — `claude daemon stop --any` stays in the runbook for
  config changes. Trust-gating: user scope avoids the per-project trust gate, note it.
- **MDM enablement is artifacts + a recipe, full stop.** Root-ownable daemon layout,
  managed-settings template, an egress-control example. Building MDM profiles or agents is
  owning prevention through the back door — out of scope by owner decision.

## Requirements

1. `openbox init` installs and starts the gateway daemon (launchd plist / systemd user
   unit) and writes `ANTHROPIC_BASE_URL=http://127.0.0.1:<port>` at **user scope**.
2. Existing hook installation unchanged and still project-scoped — only gateway config and
   the daemon are machine-level.
3. Env-block writer preserves foreign keys and replaces only what it owns (`ownedLocalHook`
   discipline applied to env keys); a plain re-run never reverts a deliberate opt-out.
4. `openbox doctor` reports: gateway liveness, active config scope + target, config
   ownership (tier detection), and bypass exposure.
5. Uninstall path: `init --remove` (or documented equivalent) stops the daemon and removes
   only owned keys/files.
6. MDM enablement shipped: root-ownable layout, managed-settings template in
   `cli/internal/managed/templates/`, recipe doc (ownership, optional egress control).
7. Windows: daemon packaging deferred; build-verified only, stated in docs (matches repo
   posture).

## Architecture

Extend the existing non-destructive `settings.json` writer to an `env` block at user
scope. Daemon lifecycle is owned by the OS supervisor, not by openbox processes: `init`
writes the unit/plist and loads it; the unit runs
`openbox gateway --addr <loopback host:port> --upstream <base URL>`.
The unit must also set its stop timeout to match `--shutdown-grace` (default 30s):
`http.Server.Shutdown` never force-closes an ACTIVE connection, so whatever is still
streaming when the window expires is cut when the process exits. Exceeding the service
manager's own timeout buys nothing — launchd SIGKILLs at 20s, systemd at 90s — so the two
numbers have to be chosen together, not independently.

**Not `--config`**: no such flag exists (phase 04 ships `--addr`/`--upstream`, because a
config-file reader needs `os.ReadFile` and requirement 5's guard forbids it). A unit
generated against the old wording would be rejected by flag parsing and the gateway
would fail to start on every boot.
Deterministic port from config; loopback-only (phase 04 invariant).

Base tier: everything user-owned. MDM tier: the same files, root-owned and pushed by the
org's MDM — identical bytes, different ownership; doctor distinguishes the tiers by
ownership, so OpenBox needs no tier flag.

## Related code files

| Path | Change |
|---|---|
| `adapters/claude-code/localhooks.go` | `env` block writer, user scope, foreign-key preservation |
| `cli/cmd/openbox/init.go` | daemon install/start; `--remove`; scope selection |
| `cli/cmd/openbox/gateway.go` | daemon-friendly run mode (no TTY assumptions) |
| `cli/internal/managed/templates/` | managed settings template (BASE_URL only) |
| `cli/cmd/openbox/doctor.go` | liveness, scope/target, ownership, bypass exposure |
| `docs/` | install/runbook + MDM enablement recipe |
| — | amendment applied |

## Implementation steps

1. `env` block writer with foreign-key preservation; second-invocation test — a re-run
   does not revert an opt-out (the `flagPassed` lesson, applied to env keys).
2. Daemon packaging: launchd plist (macOS) + systemd user unit (Linux), keep-alive,
   loopback port from config; install/start/stop/remove wired into `init`.
3. `doctor`: liveness probe against the local port; active scope + target resolution;
   ownership check (tier detection); bypass-exposure summary in detection language.
4. Managed template (BASE_URL only) + root-ownable layout documented.
5. Runbook: install, uninstall, config-change (`claude daemon stop --any`), trust-gating
   note, "calls fail against dead localhost" triage.
6. MDM recipe doc: ownership hardening + an egress-control example. Enablement only.

## Todo

- [ ] `env` block writer, foreign keys preserved
- [ ] Second-invocation test (re-run ≠ revert)
- [ ] launchd plist + systemd unit, keep-alive, loopback
- [ ] `init` installs/starts; `--remove` stops/cleans owned artifacts only
- [ ] `doctor`: liveness, scope/target, ownership tier, bypass exposure
- [ ] Managed template (BASE_URL only)
- [ ] Runbook incl. daemon-stop caveat and trust-gating note
- [ ] MDM recipe doc (enablement only)
- [ ] Windows deferral stated in docs

## Success criteria

- A fresh `openbox init` on a machine with zero org infrastructure produces a session
  whose model calls traverse the localhost gateway.
- Killing the gateway leaves model calls failing (closed) and `doctor` names the cause.
- A plain re-init does not revert a deliberate opt-out.
- `doctor` correctly distinguishes base tier (user-owned) from MDM tier (root-owned).
- Uninstall leaves no owned artifacts and no orphaned daemon.
- Background-agent sessions are governed identically to terminal sessions.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| Config written to a scope that is ignored | user scope default; doctor reports actual scope + target | events absent, calls succeed direct | doctor is the detector; fix scope |
| Re-init reverts an opt-out | second-invocation test | opt-out disappears after unrelated init | this bug shipped once for `enforce`; same control |
| Supervisor keeps stale launch config | runbook step (`claude daemon stop --any`) | background agents bypass after config change | documented; doctor's target check catches it |
| Daemon dies silently | keep-alive; dead localhost fails closed | model calls error | restart via supervisor; doctor triage |
| Orphaned daemon after uninstall | `--remove` owns the teardown; process-management discipline | port held by a ghost process | kill by unit name, never by pattern broader than ours |
| Trust gating delays activation | user scope avoids per-project gate; runbook note | first-run calls go direct in edge cases | accept and document; not fixable client-side |

## Security considerations

- The base tier's honest claim is visibility. Doctor output and docs must use detection
  language; "bypass-capable configuration" is a finding, not a failure.
- Root-owned MDM tier means `init` (running as the developer) can no longer rewrite the
  config — doctor must detect and report that state rather than trying to write.
- The daemon runs as the developer, holds no secrets (pass-through), and binds loopback.
  Its unit file must not widen any of those three properties.

## Next steps

Phase 08 proves the whole thing — including the account HALT and bypass visibility —
against a live stack.

## Status, 2026-08-25

**Requirement 4 is built** — `cli/internal/gatewaycheck` + `openbox doctor`'s
"Local gateway" section. This is the phase's assurance core: the plan's base claim is
DETECTION, and this is that claim in code.

It answers four questions and keeps them apart, which is the design:

- **alive** — a TCP connect, not an HTTP request. Deciding what a healthy *answer* looks
  like is not doctor's business; the gateway's job is relaying someone else's answers.
- **actually used** — does the tool's active config point at loopback at all. A gateway can
  be running perfectly while the tool talks straight to the provider, and conflating "alive"
  with "in use" is how a dashboard comes to show governance that is not happening.
- **who owns the config** — the tier is INFERRED FROM OWNERSHIP, not from a flag OpenBox
  writes. A flag would be a claim; ownership is an observation. A user-owned file at the
  managed path is reported as **base**, with the downgrade explained.
- **bypass exposure** — always printed, including when everything looks healthy, because a
  check that goes quiet on the happy path trains a reader to read silence as prevention.

**The wording is tested, not just written.** `TestReportNeverClaimsPrevention` runs every
tier and fails on any affirmative prevention claim while REQUIRING the detection framing.
Writing that test taught something worth keeping: the first version banned the word
"prevented" and failed on the honest phrase "not prevented" — so it now tests the CLAIM, not
the vocabulary. A wording guard that pushes wording the wrong way is worse than none.

Even the MDM tier reports bypass as capable: a root-owned file stops the developer rewriting
it, but a shell export still wins for a directly-launched process. Saying "cannot" there
would be the exact overstatement this product exists to prevent.

Verified on the real binary across all three states — unconfigured, configured + running,
configured + dead — not only in tests.

Windows degrades to "owner unknown" rather than silently reporting the MDM tier, because it
exposes no uid to check. Claiming a tier this build cannot observe is the same failure in a
smaller place.

`managed.ClaudeCodeManagedDir` was exported so doctor resolves the managed path through the
package that owns it. Same reason `doctor`'s duplicate-engine warning and `init`'s repair are
built on one classifier: a check and the thing it checks must not be able to disagree about
where the file lives.

### Also built (2026-08-25): `cli/internal/gatewayservice`

**Requirement 3 — the env writer, which is where this repo has shipped bugs twice.** Both
lessons are encoded as tests rather than comments:

- **Foreign keys are preserved; only owned keys are replaced.** `init` once matched its own
  hook entries by exact command string, so an entry written under a different `HOME` read as
  FOREIGN, was kept, and both engines fired — every governed call stored twice.
- **A plain re-run cannot revert a deliberate opt-out.** `TestPlainReWriteIsIdempotent` is
  the SECOND-INVOCATION test, and it exists because fifteen green enforce tests missed
  exactly this bug by each running the command once.
- An unparseable settings file is **refused, not clobbered** — a file we cannot parse is a
  file we cannot safely rewrite.
- A replacement is **reported**, so a developer or org that had pointed the variable
  elsewhere sees the change rather than discovering it later.

**Requirement 5 — uninstall.** `RemoveEnv` removes only owned keys, and drops the `env`
block only when nothing else is left in it: an org with its own variables there must not
lose them because OpenBox was removed.

**Requirement 6 — MDM enablement.** `docs/gateway-mdm-recipe.md`, plus the unit renderers.
The recipe leads with what the MDM tier does NOT buy — a root-owned settings file stops a
developer rewriting the file but a shell export still wins, so egress control is the only
item on the page that prevents rather than detects, and it is the org's to deploy.

Two defects were caught while writing that doc, both the same species as the `--config`
bug: it referenced an `openbox init --print-unit` flag that does not exist, and it claimed
`openbox init` writes the units when nothing wires them yet. Both corrected. **Documenting a
flag that does not exist is now three-for-three in this plan** — worth a sweep of the other
phase docs.

`TestUnitsUseFlagsThatExist` is the mechanical guard: it parses the rendered units and fails
on any `--flag` the CLI does not define.

### Requirement 1: wired, and the ORDER is the safety property

`cli/cmd/openbox/initgateway.go`. The sequence is
**unit -> start -> PROVE it listens -> only then write `ANTHROPIC_BASE_URL`**, and it is not
stylistic. Writing the env var first points Claude Code at a port nothing answers, and
because a dead localhost fails closed, EVERY model call on the machine fails — `init` would
have broken the developer's tool while printing success. So the env write is last and
conditional: if the daemon cannot be proven up, the variable is not written at all and the
machine is left working and ungoverned, with an error that says so in those words.

Uninstall runs the **reverse** order for the mirror reason: env var first, then the daemon,
so there is never a window where the tool points at something gone.

Both orderings are mutation-drilled — moving the env write earlier, or the daemon removal
earlier, each turns a named test red. A third check asserts the readiness probe is real:
accepting a unit is not evidence that a process is serving, so the supervisor's success and
the listener's existence are separate gates.

### `--gateway` is OPT-IN, and that is an OD-class call worth the owner's eye

That decision's lesson — a default-off headline feature stays off — argues for defaulting
this ON. It is off anyway, because the two cases are not alike: **enforcement-by-default is
INERT without an org policy, so flipping it could not break anyone.** This redirects live
model traffic through a process that has never run against a real stack, whose refusal shape
is still unprobed, and which has no daemon packaging on Windows at all.

Revisit once phase 08 has run the end-to-end path. `TestGatewayIsOffByDefault` reads `init`'s
own help text, so flipping the default is a deliberate edit rather than a silent one.

### One discoverability defect found by that test

The new flags did not appear in `init -h` at all: that command prints a CURATED block, not
`flag.PrintDefaults()`, so adding a flag does not advertise it. `--gateway` changes what a
machine sends model traffic to — the largest blast radius this command has — and an
undiscoverable flag for it is a support problem, not a tidy help screen. Now listed.

### Requirement 2

Hooks stay project-scoped: the gateway step touches only user-scope settings and the
machine-level unit, and no code path in it reaches the project hook writer.
