# Phase 07 — Local daemon, doctor, and MDM enablement

## Context links

- Parent: [plan.md](plan.md) · Previous: [phase-06](phase-06-gateway-enforcement.md)
- Scope amendment: [phase-03](phase-03-decisions-and-adrs.md) → ADR-0016
- Settings precedence: https://code.claude.com/docs/en/settings
- Assurance tiers: ADR-0021 (phase 03)
- Depends on: 06

## Overview

- Date: 2026-08-25 (local-gateway revision)
- Description: make the gateway a supervised local service a developer gets from
  `openbox init`, make its health and bypass exposure visible through `doctor`, and ship
  the artifacts an MDM-capable org needs to harden it — without OpenBox operating anything.
- Priority: P1
- Implementation status: pending
- Review status: not reviewed

## Key insights

- **Project scope still does not work for this config.** `ANTHROPIC_BASE_URL` is read from
  managed settings and `~/.claude/settings.json` where the Desktop app manages the
  connection; background agents need settings, not shell exports. The ADR-0016 amendment
  stands — the value now points at `127.0.0.1`, not an org host.
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
writes the unit/plist and loads it; the unit runs `openbox gateway --config <path>`.
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
| `docs/adr/ADR-0016-default-install-posture.md` | amendment applied |

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
