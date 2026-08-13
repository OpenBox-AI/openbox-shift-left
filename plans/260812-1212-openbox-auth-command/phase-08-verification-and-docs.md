# Phase 08 — Cross-platform verification + docs

## Context links

- Parent: [plan.md](plan.md) · Depends on: phases 3, 5, 6, 7
- Repo rule (`CLAUDE.md`): "Verify against the real thing… unit tests are not
  evidence that a hook works." `testbed/run-all.sh` drives real headless sessions.
- CI today: `.github/workflows/ci.yml:22` — **ubuntu-only**
- Release today: `.goreleaser.yaml:30-33` — tar.gz, **no Windows**
- Was phase 07 before validation added the `init` phase on 2026-08-13.

## Overview

- **Date:** 2026-08-12 (revised 2026-08-13)
- **Description:** Prove the `auth` → `init` flow works on all three OSes to the
  extent it can be proven, state honestly what is not proven, and update user docs
  to the two-command flow and the project-local scope default.
- **Priority:** P1 · **Implementation status:** implemented 2026-08-13 · **Review status:** self-reviewed

## Key Insights

- **Windows and Linux runtime behaviour cannot be verified from this macOS dev
  host.** The plan says so rather than implying coverage. CI covers what CI can;
  the rest is a named manual checklist.
- CI is ubuntu-only, so a Windows *compile* regression is currently invisible.
  Adding `GOOS=windows go build ./...` is cheap and catches the most likely
  breakage class (a stray unix-only call).
- **Linux OS-store CI coverage is now moot** — the keychain is gone, so there is no
  `secret-tool` path left to test. A whole category of untestable integration
  disappeared with the keychain. Worth stating: the simplification bought real
  verifiability, not just fewer files.
- The dotenv codec and credential resolution are pure Go with no platform calls, so
  **unit tests genuinely do cover them on every OS** — unlike hooks. Be precise
  about which claims rest on unit tests and which need a live run.
- `install.sh` is bash. Even with a Windows binary there would be no Windows
  installer, which is why the release decision was build-from-source.

## Requirements

1. CI matrix: keep ubuntu for build/vet/`-race`; **add** a `GOOS=windows go build
   ./...` step (cross-compile, no runner needed) and a macOS job if runner budget
   allows.
2. A `testbed/` phase (or extension of an existing one) that runs `openbox auth`
   non-interactively via the stdin path against the local stack, then `openbox
   init` at **default (local) scope**, then drives a real session and asserts
   events arrive for the configured agent. The scope default is new behaviour, so
   the testbed must exercise the default rather than passing `--scope` explicitly.
3. A negative scope assertion: a session driven from a directory where `init` was
   **not** run produces **no** events. This is the governance gap ADR-0016 records,
   and it should be demonstrated rather than assumed.
4. Manual acceptance checklist, per OS, in the plan's `reports/`: fresh install
   (`auth` then `init`), re-run/update, `--rotate`, env-shadow warning, migration
   from a legacy `os.UserConfigDir()` layout, and `--scope global`.
5. Docs updated: `docs/getting-started.md` (**`auth` first, `init` second** — auth
   is the credential front door, init installs hooks at a scope and writes
   posture), `README.md` quickstart, and a migration note for existing installs.
6. The migration note must cover **both** legacy stores:
   - the opt-in `secrets.json` file backend — say where it lives and that it should
     be deleted (it is a stale plaintext copy of live credentials);
   - the **OS keychain**, whose credentials are not migrated (D1). Give the literal
     read commands — `security find-generic-password -s <service> -a <account> -w`
     on macOS, `secret-tool lookup service <service> account <account>` on Linux —
     so a user can copy values into `auth` and keep their agent, and name
     `auth --rotate` as the alternative for anyone holding an org key.
7. Docs must state the project-local scope default **and** what it leaves
   ungoverned, in getting-started where a user will actually read it — not only in
   ADR-0016. Include how to govern everything (`--scope global` + managed
   settings), and that Codex is user-scoped only.
8. `docs/data-and-privacy.md` and `docs/architecture.md` cross-checked against what
   actually shipped — phase 1 wrote them ahead of the code; reconcile any drift.
9. State explicitly, in docs, which platforms were exercised live and which were
   only cross-compiled.

## Architecture

Verification splits three ways, and the docs must not blur them:

| Claim | Evidence |
|---|---|
| dotenv parse/write, precedence, migration | unit tests, all OSes (pure Go) |
| the `.env`/`dev.json` split holds (no coordinate in `.env`) | unit tests, all OSes |
| Windows compiles | CI cross-compile |
| `auth` → `init` → session → events arrive | testbed against a live local stack (macOS/Linux) |
| project-local scope governs only that project | testbed negative assertion (macOS/Linux) |
| `--scope global` activation | **not verifiable by us** — needs a managed-settings deployment |
| Windows/Linux *runtime* | **manual checklist only** — not automated |

## Related code files

| Path | Why |
|---|---|
| `.github/workflows/ci.yml:22` | add the windows cross-compile step |
| `.goreleaser.yaml:30-33` | unchanged by decision; note the gap in docs |
| `testbed/run-all.sh`, `testbed/*.sh` | pattern for a new phase script |
| `docs/getting-started.md`, `README.md` | user-facing flow |
| `install.sh` | bash-only; named as a gap, not fixed |

## Implementation Steps

1. Add the `GOOS=windows go build ./...` CI step across all modules; confirm it
   fails if a unix-only call is introduced (test by temporarily adding one).
2. Write the testbed script: `auth` via `--api-key-stdin`/`--private-key-stdin`,
   then `init` with no `--scope` flag, then a real headless session, then assert
   events for the configured `agent_id` and that `bundle_version` is **not**
   `no-policy` (proving `agent_id` landed). **Note the shelf life:** that proof rides
   the policy bundle, which
   [inline policy evaluation](../260813-0140-inline-policy-evaluation/plan.md) deletes.
   Assert on `agent_id` in the stored session itself so the test survives that change;
   treat the `bundle_version` check as a bonus while bundles exist.
3. Add the negative case: drive a session from a second directory with no
   `.claude/settings.local.json` and assert zero events.
4. Run `testbed/run-all.sh` against a local stack; record the result in
   `reports/`. If it cannot run, say so — do not mark verified.
5. Write the per-OS manual checklist into `reports/`, with a column for who ran it
   and when. Unrun rows stay unrun.
6. Update `getting-started.md` to the `auth` → `init` flow, `README.md`, and add the
   migration note covering both legacy stores.
7. Reconcile phase 1's docs (ADR-0015 **and** ADR-0016) against shipped behaviour;
   fix drift.
8. Final sweep: `go build ./... && go vet ./... && go test -race ./...` for all 11
   modules, plus the Windows cross-compile.

## Todo list

- [x] CI cross-compiles for Windows and the step demonstrably catches a regression
- [x] testbed phase: `auth` → `init` (default scope) → session → events asserted,
      incl. `agent_id` landing
- [x] testbed negative case: ungoverned directory produces zero events
- [x] testbed run recorded in `reports/` (or its absence recorded)
- [x] per-OS manual checklist written; unrun rows left unrun
- [x] `getting-started.md` leads with `auth` → `init` and states the scope gap
- [x] migration note covers `secrets.json` **and** the keychain read commands
- [x] `README.md` quickstart updated
- [x] phase-1 docs (both ADRs) reconciled with shipped behaviour
- [x] all 11 modules green + Windows cross-compile green

## Success Criteria

- CI fails on a Windows-incompatible change.
- A real session after `auth` + `init` produces events attributed to the configured
  agent, with a policy bundle (not `no-policy`).
- A session in a directory where `init` was not run produces no events, and the
  docs say so before a user discovers it.
- Docs state exactly which platforms were exercised live.
- An existing install with the legacy layout can follow the migration note and end
  up working: config migrated, `secrets.json` deleted, and keychain credentials
  either copied across or rotated.
- No doc claims keychain protection or implies Windows at-rest protection.
- No doc implies governance is ambient across a machine after a default `init`.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| Testbed cannot run (no local stack available) | M×M | phase closes with no live evidence | **Adjust:** record "not run" in `reports/` and in `docs`. Do **not** mark the feature verified — this repo's rule is that reading is not evidence. |
| Windows never actually exercised at runtime | H×M | a Windows-only runtime bug ships | **Accepted, disclosed:** cross-compile + manual checklist is the ceiling from this host. Docs must say Windows is build-verified only until someone runs the checklist. |
| Docs drift from phase 1 (written before the code) | M×M | a doc claim contradicts shipped behaviour | **Adjust:** step 6 exists for this; treat any contradiction as a release blocker. |
| Migration note misses a legacy path | M×M | a user is left with two configs and silent no-delivery | **Adjust:** enumerate legacy paths from phase 2's `migrate.go` rather than from memory. |
| Existing users hit the stranded-keychain wall with no way out | M×H | a working install breaks and the only advice is "re-register" | **Mitigated:** the keychain read commands are in both the migration note and phase 3's error text. If they prove wrong on a real machine, fix the commands — this is the one migration path D1 leaves. |
| Docs describe project-local scope as full coverage | M×H | a reader concludes their machine is governed after a default `init` | **Stop:** this is the overstatement `CLAUDE.md` forbids. ADR-0016 and getting-started must agree, and phase 7's `printGovernedScope` is the runtime echo of the same fact. |
| CI runner budget rejects a macOS job | L×L | matrix reduced | **Accepted:** ubuntu + windows-cross is the floor; macOS is covered by dev-host runs. |

## Security Considerations

- Docs must state the plaintext posture and the per-OS asymmetry in the place a
  user will actually read (getting-started), not only in the ADR. Same for the
  org control token's larger blast radius on approver installs.
- Docs must state the project-local scope default and what it leaves ungoverned, in
  the same place, for the same reason.
- The migration note must tell users to **delete** the old `secrets.json` — leaving
  it is a stale plaintext copy of live credentials. The keychain entries, by
  contrast, are harmless to leave (nothing reads them) but should be deleted once
  credentials are copied out, for the same reason.
- Testbed scripts must not print credentials; assert on presence and shape only.
- Confirm no new doc example shows a real-looking key that someone might copy.

## Next steps

Plan complete. Optional follow-ups, each needing its own decision: retiring the
deprecated `OPENBOX_ED25519_SEED`/`OPENBOX_SEED` aliases (needs an ADR amendment);
project-local hook scope for Codex (`.codex/hooks.json`, ruled out in this plan);
restoring `--managed-enable` if the org force-enable substrate is still wanted;
shipping a Windows release binary plus a PowerShell installer; and filing the
openbox-backend DTO-drift ticket from phase 6.
