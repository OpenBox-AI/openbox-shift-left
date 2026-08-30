# Phase 07 — `init` slimmed: `--scope local|global`, credentials out

## Context links

- Parent: [plan.md](plan.md) · Depends on: phase 5 (`auth` must exist to point at)
- Blocks: phase 8 (verification exercises the two-command flow)
- Created by validation on 2026-08-13 (D5, D6) — this phase did not exist in the
  original plan.

## Overview

- **Date:** 2026-08-13
- **Description:** `init` becomes what its name says: setup. It installs the
  provider's hooks at a chosen scope and writes posture. Credentials and
  registration leave for `auth`. `--local-hooks <dir>` is promoted to a
  first-class `--scope local|global` **defaulting to local**, and the flag surface
  drops from 17 to 7.
- **Priority:** P1 · **Implementation status:** implemented 2026-08-13 · **Review status:** self-reviewed

## Key Insights

- **`init` today is two commands wearing one name.** Its own package comment says
  it "registers a developer agent with OpenBox, captures the agent's credentials
  into the OS secret store, **and** delegates the tool's native config to that
  provider's adapter installer" (`main.go:3-6`). Splitting on that `and` is the
  whole phase.
- **Global scope cannot be self-activated, and that is the strongest argument for
  the local default.** `Install` "does NOT modify global/managed Claude Code
  settings" (`installer.go:99-101`); it prints
  `{"enabledPlugins": ["openbox-observe"]}` for someone to apply. Local scope, by
  contrast, `init` completes itself by merging into
  `<dir>/.claude/settings.local.json` (`localhooks.go:45-55`). Defaulting to local
  means the command's exit status matches what it actually accomplished.
- **Scope is activation only; the bundle is always installed.** `Install`
  unconditionally materializes the plugin bundle, places the engine binary and
  writes config, *then* optionally merges project hooks
  (`installer.go:102-124`). So `--scope` must not be modelled as "install here vs
  there" — it selects whether the project settings merge happens.
- **The mechanism already exists in the SPI.** `CredentialRef.LocalHooksDir`
  (`provider/provider.go:82-88`) is the seam; `--scope local` sets it to the
  working directory, `--scope global` leaves it empty. No new SPI surface — but the
  field's name and doc comment now describe the *exceptional* case as if it were
  still exceptional, so rename to `HookScope`/`ProjectDir` and rewrite the comment.
- **Codex has no project scope, and pretending otherwise would be the bad kind of
  default.** Codex hooks live at `$CODEX_HOME/hooks.json` or `~/.codex/hooks.json`;
  repo-level `.codex/hooks.json` is "an alternative location this installer
  deliberately does not touch" (`adapters/codex/installer.go:353-357`). So for
  Codex, `--scope local` must **error** with that reason, and a bare `init
  --provider codex` resolves to global while saying so. Silently governing every
  Codex session when the user asked for one project is worse than an error.
- **`--local-hooks`'s flag text is evidence, not just prose.** It reads "LOCAL
TESTING opt-in … production posture is managed-settings/global activation; never
set this in production" (`main.go:359`). Promoting that path to the default
directly contradicts shipped guidance, which is exactly why that decision must
land first and why this phase rewrites the text rather than leaving it to rot.
- **`--enforce` must keep working after the split.** Enforce is posture, posture is
  `init`'s, and `--no-enforce` exists precisely because "a plain re-init leaves an
  existing posture alone rather than silently downgrading it" (`main.go:362`). Do
  not let the slimming sweep either of them away.
- **Enforce becomes the default, and `Enforce` must become `*bool` first — this repo
  has already been bitten by exactly this.** `CLAUDE.md` records why `Finops` had to
  change type before its default could flip: as a plain bool "an absent config field
  and an explicit `false` were indistinguishable, so the flip would have been a
  silent no-op." `Enforce` is in the identical shape today, and worse in two ways:
  - `ResolveEnforce` passes `func(c DevConfig) *bool { b := c.Enforce; return &b }`
    (`devconfig.go:379-381`) — it returns `&b` **unconditionally**, so `resolveBool`
    never reaches its default argument. Changing that argument from `false` to `true`
    would do **nothing**.
  - the field is `Enforce bool` with `json:"enforce,omitempty"` (`devconfig.go:115`),
    so an explicit `--enforce=false` marshals to *nothing at all* — the opt-out would
    vanish from `dev.json` and silently re-default to enforce on the next read.
  Both are fixed by the same one-line type change plus a tri-state resolver, mirroring
  commit `42011e0`. **Do this before flipping the default, not after.**
- **`WouldDowngradeEnforce` stops working under a default-on posture.** It reads
  `prior.Enforce && next != nil && !*next` (`write.go:91-97`), so for a config that
  never wrote the field it sees `prior.Enforce == false` and reports "no downgrade"
  even though the *effective* posture was enforce. Once absent means on, the guard has
  to compare **resolved** postures, not the raw field.

## Requirements

1. `openbox init [--provider X] [--scope local|global] [--enforce[=false]]
   [--install-git-hook] [--role dev|approver] [--dry-run]`. Seven flags, down from
   seventeen.
1a. **`Enforce` becomes `*bool` and defaults to ON**, in that order:
   - change the field to `*bool` and give `ResolveEnforce` a tri-state accessor
     (`func(c DevConfig) *bool { return c.Enforce }`), so an absent field reaches the
     default and an explicit `false` survives a round-trip;
   - flip the default to `true`; `--enforce=false` (or `OPENBOX_ENFORCE=0`) opts out
     and **persists** the opt-out;
   - rework `WouldDowngradeEnforce` to compare resolved postures so it still catches a
     real downgrade when the prior config left the field absent;
   - keep `--no-enforce` working as an alias of `--enforce=false`.
   A test must assert that `--enforce=false` → re-read → still false. That round-trip
   is the whole point of the type change.
2. **Moved to `auth`** (phase 5 already owns them): `--org`, `--agent-name`,
   `--icon`, `--description`, `--force`, `--base-url`, `--backend-url`. Each must
   error on `init` with a message naming `openbox auth`, not be silently accepted
   or silently ignored.
3. **Deleted:** `--secret-backend` (phase 3, backing code gone), `--local-hooks`
   (superseded by `--scope`), `--client-id`, `--managed-enable`. `--local-hooks`
   keeps working as a hidden deprecated alias for `--scope local` for one release,
   warning once to stderr.
4. `--scope` defaults to **local**, resolved to the process working directory.
   `--scope global` skips the project merge and prints the managed-settings snippet
   it cannot apply itself.
5. For `--provider codex`: `--scope local` errors naming the unsupported repo-level
   location; an unspecified scope resolves to global **and says so on stdout** — it
   must never look like a project-scoped install happened.
6. `init` performs **no** registration and **no** credential write. When
   credentials are absent it exits non-zero naming `openbox auth`; it does not
   prompt, and it does not half-install.
7. `init` still writes posture and still pins the org signing keys — the
   `dev.json`/`ManagedConfig` behaviour is untouched, including the tri-state merge
   that leaves absent posture fields alone.
8. `printGovernedScope` (`main.go:517-531`) tells the truth about the scope that
   was just applied: for local, which directory is governed and that others are
   not; for global, that activation is pending the managed-settings step.
9. `provider.CredentialRef`'s local-hooks field renamed and re-documented; both
   adapters updated; `TestLocalHooksMirrorPluginBundle` still pins the hook lists
   together.

## Architecture

```
openbox auth   → credentials (.env) + coordinates (dev.json) + registration
openbox init   → hooks at a scope + posture
                   ├─ always: materialize bundle, place engine, write posture
                   ├─ scope local (default): merge <cwd>/.claude/settings.local.json
                   └─ scope global: print the managed-settings snippet, apply nothing
```

The split is enforced by absence: after this phase `init.go` imports nothing that
can write a secret, and `devinit`'s registration entry point is reachable only from
`auth`.

## Related code files

| Path | Action |
|---|---|
| `cli/cmd/openbox/main.go:3-6` | package comment describes the pre-split `init`; rewrite |
| `cli/cmd/openbox/main.go:350-366` | the 17-flag flagset; reduce to 7 |
| `cli/cmd/openbox/main.go:359` | `--local-hooks` "never set this in production" text; replaced by `--scope` |
| `cli/cmd/openbox/main.go:361-362` | `--enforce`/`--no-enforce`; **keep** |
| `cli/cmd/openbox/main.go:444-462` | `--secret-backend` warning path; delete with phase 3 |
| `cli/cmd/openbox/main.go:517-531` | `printGovernedScope`; must state the real scope |
| `cli/cmd/openbox/main.go:561-562` | usage lines; `auth` first, `init` second |
| `cli/cmd/openbox/init.go` | flag parsing, `--role` handling, credential preconditions |
| `cli/internal/devinit/devinit.go:106,163,214-290` | registration + `accounts()` leave for `auth` |
| `provider/provider.go:63-88` | `CredentialRef`: rename the local-hooks field, rewrite its comment |
| `adapters/claude-code/installer.go:60-124` | Plan text + the conditional project merge |
| `adapters/claude-code/localhooks.go:10-18` | doc comment calls project scope LOCAL-TESTING; now the default |
| `adapters/codex/installer.go:353-357` | the unsupported repo-level location, quoted in the error |

## Implementation Steps

1. **Flags first, behaviour second.** Reduce the flagset, add `--scope`, and make
   every moved flag a hard error naming `auth`. Test the error text for each of the
   seven moved flags — a silently ignored flag is the failure mode here.
2. Rename the SPI field and rewrite its doc comment; update both adapters; keep the
   mirror test green.
3. Default `--scope` to local; wire it to the working directory. Assert the merge
   lands in `<cwd>/.claude/settings.local.json` and that `--scope global` does not
   touch project files at all.
4. Codex: error on `--scope local`; resolve unspecified scope to global with an
   explicit stdout line. Test both.
5. Strip registration and credential writes from `init`; add the
   credentials-absent precondition error naming `auth`. Assert `init` exits
   non-zero and writes nothing when `.env` has no api key.
6. Rewrite `printGovernedScope` for the two scopes and pin its output with a test —
   this string is the user's only signal about what is actually governed.
7. Update the package comment and usage lines to the `auth` → `init` order.
8. `go build ./... && go vet ./... && go test -race ./...` for every module.

## Todo list

- [x] Flag surface is 7; each of the 7 moved flags errors naming `auth`
- [x] `Enforce` is `*bool` with a tri-state resolver **before** the default flips
- [x] `--enforce=false` round-trips: written, re-read, still false
- [x] `WouldDowngradeEnforce` catches a downgrade from an absent-field config
- [x] a bare `init` produces an enforcing install, asserted on `dev.json`
- [x] `--local-hooks` still accepted for one release, warns once to stderr
- [x] `--scope` defaults to local and merges `<cwd>/.claude/settings.local.json`
- [x] `--scope global` touches no project file and prints the pending manual step
- [x] Codex: `--scope local` errors; bare `init` resolves global and says so
- [x] `init` writes no credential and performs no registration; absent creds ⇒
      non-zero exit naming `auth`
- [x] Posture writes and org signing pins unchanged; tri-state merge intact
- [x] `printGovernedScope` states the real scope, pinned by test
- [x] SPI field renamed + re-documented; adapter mirror test green
- [x] All 11 modules build, vet, `-race` green

## Success Criteria

- `openbox init` with no flags, in a project directory, governs **that** project
  and says which one — and a session started elsewhere produces no events.
- `openbox init --scope global` leaves the project tree untouched and states that
  activation awaits the managed-settings step.
- `openbox init --org acme` fails with a message naming `openbox auth`.
- `openbox init` on a machine with no credentials fails naming `openbox auth` and
  installs nothing.
- `openbox init --provider codex --scope local` fails with the repo-level-hooks
  reason; `openbox init --provider codex` succeeds and states global scope.
- `--enforce` and `--no-enforce` behave exactly as before the split.
- `openbox init --help` fits comfortably on one screen.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| A moved flag is silently ignored instead of erroring | M×H | a script passing `--base-url` to `init` keeps exiting 0 while the URL goes nowhere | **Adjust:** every moved flag gets an explicit error test. Silent acceptance of a flag that no longer does anything is worse than removing it loudly. |
| Default-local ships without that decision | L×H | the scope default changes with no document explaining what is ungoverned | **Stop:** phase 1 blocks this phase for exactly this reason. |
| Codex users get global scope thinking they got local | M×H | events from every Codex session on the machine after asking for one project | **Mitigated by design:** unspecified scope prints the resolved scope; `--scope local` errors. Never infer silently. |
| Slimming removes `--managed-enable` and an org needed the substrate it records | L×M | an org force-enable rollout has no local record | **Flagged, unresolved:** listed in plan.md's Unresolved — confirm before this phase lands. Restoring it is one flag, not a redesign. |
| The enforce default flips while `Enforce` is still a plain `bool` | M×H | the new default has no effect on existing configs, and `--enforce=false` does not persist | **Stop:** this is the `Finops` bug verbatim (`CLAUDE.md`, commit `42011e0`). The type change is step 1a and is not optional. |
| Enforce-by-default surprises a developer mid-task | M×M | a tool call is blocked on a machine the user thought was observing | **Accepted, disclosed:** with no org policy published nothing is blocked, `--enforce=false` is one flag, and `openbox doctor` states the posture. The README says so at the point of install. |
| `--enforce` also gating Tier-2/Tier-3 makes the flip wider than intended | M×M | hot-path secret I/O and the findings loop switch on for everyone | **Resolved 2026-08-13:** flip `enforce` only; leave the `tier2`/`findings` writes at `main.go:391` untouched. The tier concept is removed wholesale by [inline policy evaluation](../260813-0140-inline-policy-evaluation/plan.md), which deprecates `tier2`/`tier2_timeout_ms` — do not redesign a coupling that is about to be deleted. |
| `printGovernedScope` overstates coverage | M×H | it says "governance is ambient" after a project-scoped install | **Adjust:** the string is pinned by a test. This message is the one place a user learns the truth about scope. |
| Registration moves and duplicate detection regresses | M×H | `auth` registers a second agent with the same name | **Stop and replan:** phase 5 owns that behaviour; its `devinit` tests must stay green unchanged. |
| `init` becomes a no-op nobody needs | L×L | reviewers ask why it still exists | **Accepted:** it still materializes the bundle, places the engine, merges hooks and writes posture — the setup half was always the larger half. |

## Security Considerations

- The scope default is a governance downgrade; this phase must
  not soften how it is reported. `printGovernedScope` naming exactly one governed
  directory is the compensating control.
- `init` losing all credential handling is a security *improvement*: after this
  phase a command that runs in every developer's shell can no longer write, read or
  prompt for a secret. Do not re-add a convenience path that reverses it.
- `--scope local` writes into a project directory (`.claude/settings.local.json`).
  Keep the existing merge guarantees: additive, idempotent, foreign entries
  preserved, and a hard error rather than a silent overwrite when the file exists
  but is not valid JSON (`localhooks.go:56-60`).
- The settings file is per-developer and git-ignored by Claude Code convention;
  phase 8's docs should say so, because a committed `settings.local.json` would
  push one developer's engine path onto the whole team.

## Next steps

Phase 8 verifies the two-command flow end to end and states, per platform, what was
actually exercised.
