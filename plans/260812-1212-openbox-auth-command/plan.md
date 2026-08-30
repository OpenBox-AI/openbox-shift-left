---
title: "openbox auth — one interactive credential command, plaintext file store"
description: "Adds `openbox auth`: prompts for agent credentials, writes ~/.openbox/.env, deletes keychain support, and aligns credential names with the platform docs."
status: implemented
priority: P1
effort: 28.5h
branch: feat/dev-runtime-auth-and-init
tags: [cli, credentials, onboarding, cross-platform, breaking-change, decision record]
created: 2026-08-12
validated: 2026-08-13
implemented: 2026-08-13
---

# openbox auth + openbox init

Two commands with one job each. **`openbox auth`** authenticates: it collects (or
registers) credentials and writes the secrets to a single plaintext file at a
neutral path. **`openbox init`** sets up: it installs the hooks into the coding
tool's settings at a chosen scope, and writes posture. `auth` runs first. Both
work identically on macOS, Linux and Windows.

Replaced an earlier keychain/DPAPI design on 2026-08-12 at the user's direction;
`research/researcher-01-windows-dpapi-report.md` is **superseded**, kept for
history. The DPAPI backend, build-tag split and `x/sys` storage dependency are
gone — dotenv + `os.UserHomeDir()` is platform-independent.

Validated 2026-08-13 (see [Validation Summary](#validation-summary)); the split of
responsibilities above, the file split below and phase 7 all come from it.

## Layout this establishes

`~/.openbox/` (override `OPENBOX_HOME`) holds `.env`, `dev.json` and
`approver.json` — the latter two migrated from `os.UserConfigDir()`.

**One store per field** (D2). The two files do not overlap:

| File | Holds | Keys |
|---|---|---|
| `.env` (0600, tool-parsed) | **secrets only** | `OPENBOX_API_KEY`, `OPENBOX_AGENT_PRIVATE_KEY`, and on approver installs `OPENBOX_CONTROL_TOKEN` |
| `dev.json` | posture **+ the non-secret coordinates** | `developer_did`, `agent_id`, `backend_url`, `base_url`, plus every posture field and the managed/`Locked` layer |

Precedence for the secrets: **real env var > `~/.openbox/.env` > unset**. For the
coordinates it is unchanged from today: **real env var > `dev.json` > default**
(`devconfig.go:457-476`). Sourcing is never required.

`.env` keys are the platform's documented names. Those names are already correct
in this repo (`devconfig.go:35-67`) with **one** exception —
`OPENBOX_ED25519_SEED`, which becomes `OPENBOX_AGENT_PRIVATE_KEY` in phase 3.
A `.env` alone is deliberately **not** sufficient to run: it carries no DID, so
`dev.json` (or the env vars) must supply the coordinates.

## Phases

| # | Phase | Status | Effort | Depends on |
|---|---|---|---|---|
| 1 | [that decision + that decision + docs: plaintext posture, install defaults](phase-01-decision-and-posture-reversal.md) | done | 3h | — |
| 2 | [`~/.openbox/` layout, dotenv codec, migration](phase-02-openbox-home-and-dotenv-codec.md) | done | 4h | 1 |
| 3 | [Rewire credentials, delete keychain, rename to platform names](phase-03-rewire-and-delete-keychain.md) | done | 4.5h | 2 |
| 4 | [Prompter: TTY, masked input, `--*-stdin`](phase-04-prompter-and-input-paths.md) | done | 2.5h | 1 |
| 5 | [`auth` command wiring (owns registration)](phase-05-auth-command-wiring.md) | done | 4.5h | 3, 4 |
| 6 | [Rotate client methods + `--rotate`](phase-06-rotate-flow.md) | done | 3h | 5 |
| 7 | [`init` slimmed: `--scope local\|global`, credentials out](phase-07-init-scope-and-slimming.md) | done | 4h | 5 |
| 8 | [Cross-platform verification + docs](phase-08-verification-and-docs.md) | done | 3h | 3, 5, 6, 7 |

Parallel-safe: **2 ‖ 4** after 1. Then 3 → 5, after which **6 ‖ 7** is possible
except that both touch `main.go`'s usage text — run them sequentially unless the
usage line is split out first. Then 8 last, because it verifies all of it.

## File ownership (no two parallel phases share a file)

| Phase | Owns |
|---|---|
| 1 | `that decision-*.md`, `that decision-*.md`, `README.md`, `docs/data-and-privacy.md`, `docs/architecture.md`, `cli/go.mod`, `cli/go.sum`, `go.work.sum` |
| 2 | `adapters/common/devconfig/paths.go`, `envfile.go`, `migrate.go` (all new) |
| 3 | `adapters/common/devconfig/devconfig.go`, `cli/internal/secret/*`, `adapters/{claude-code,codex}/creds.go`, `client/client.go`, `adapters/common/git/*`, `cli/cmd/openbox/{main.go,attest.go,approve.go}`, `cli/internal/devinit/devinit.go`, `actions/openbox-git-action/*` |
| 4 | `cli/internal/prompt/*` (new) |
| 5 | `cli/cmd/openbox/auth.go` (new), `main.go` dispatch only |
| 6 | `cli/internal/backend/rotate.go` (new), `auth.go` rotate path |
| 7 | `cli/cmd/openbox/init.go`, `cli/internal/devinit/devinit.go` (options/flags), `provider/provider.go` (SPI field rename), `adapters/claude-code/{installer.go,localhooks.go}`, `adapters/codex/installer.go` |
| 8 | `.github/workflows/ci.yml`, `docs/getting-started.md`, `README.md`, `testbed/*` |

**Phases 3 and 7 both touch `devinit.go`** and are sequential for that reason: 3
removes the secret-store writes, 7 removes the registration path and reshapes the
flags. Phase 3 no longer owns `init.go` — every `init` surface change is phase 7's.

## Outcome — 2026-08-13

All eight phases implemented; all 11 modules `go build` / `go vet` /
`go test -race` green on macOS/arm64; Windows + linux/arm64 cross-compiles added to
CI and **proven to catch a unix-only call**, not merely added.

**The testbed did not run** — no local OpenBox stack was reachable from this host
(`localhost:3000` and `:8086` both refused). So the claim that a real session after
`auth` → `init` produces events, and the negative assertion that an uninitialized
directory produces none, are written and parse but are **unverified**. Per this
plan's own risk table, that is recorded rather than glossed:
[reports/verification-260813-auth-init.md](reports/verification-260813-auth-init.md)
splits every claim by evidence strength and carries the per-OS manual checklist.

### Decisions taken during implementation

| # | Decision | Why |
|---|---|---|
| D12 | **`--managed-enable` deleted** (the plan's open question). | Confirmed by the user. It wrote `managed_enable` into the agent's backend config as a Phase-1 substrate nothing read. Restoring it is one flag. Passing it now errors and points at `openbox managed install`. |
| D13 | **`org` is NOT persisted to `dev.json`**, contrary to the field table. | `DevConfig` has no org field, and adding one changes a documented config contract — which `CLAUDE.md` says needs a decision record the plan never wrote. It derives an agent name at registration time and comes per run from `--org` / `OPENBOX_ORG`, exactly as `init` did. Cost: a re-run does not remember the org. |
| D14 | **`--did` and `--agent-id` added to `auth`.** | Not in the plan's flag list, and without them the `--*-stdin` automation path cannot configure a machine at all — it can supply secrets but not the coordinates they belong to. Both are non-secret public identifiers, so a flag value is safe (unlike a secret; INV-1 unaffected). |
| D15 | **A read-side fallback to the legacy config location.** | Not in the plan. Migration runs from `auth`/`init` (write commands), so between upgrading the binary and running one, every hook would have resolved `~/.openbox/dev.json`, found nothing, and failed open into an unconfigured state — an install that looks fine and produces nothing. Reads now fall back; writes never do. |
| D16 | **`devinit.Register` extracted from `devinit.Run`.** | `auth` needs registration WITHOUT the installer that `Run` invokes. Behaviour-preserving: `devinit`'s own tests pass unchanged, which is what the plan asked for. |
| D17 | **`x/term` pinned to v0.34.0, not latest.** | v0.35.0+ declares `go 1.24.0` (v0.45.0 wants 1.25.0), which would raise the language floor across all 11 modules and `go.work` — a toolchain decision smuggled in as a dependency bump. `go mod tidy` re-upgraded it twice during implementation, so the require block now says why not to. `cli/go.mod` and `go.work` read `go 1.23.0` rather than `go 1.23`; same language version, patch component spelled out. |
| D18 | **that decision's `*bool` rationale corrected against the running code.** | The plan claimed `resolveBool` never reaches its default argument. A probe showed it does — the key-presence map in `resolveBoolWithSource` already handles a plain-bool accessor. The real reason is the write side: `omitempty` drops an explicit `false`, so `--enforce=false` would have been silently un-appliable. That decision records the correction and the general rule. |
| D19 | **`init --help` gets a custom usage.** | The moved/removed flags must stay *parseable* to error usefully, but listing all 18 would defeat cutting the surface to 7. `--help` shows the seven, then names the moved ones in one line. 25 lines total. |

### Two things found by running it, not by reading it

- **The closing block said "Governance is ambient from here"** three lines above
  "THIS PROJECT ONLY" — two true halves reading as a contradiction, and the wrong
  half is the one a hurried reader believes. Fixed, and the test widened from
  `printGovernedScope` to the whole install output, because asserting on the
  function alone is what missed it.
- **The hermeticity guard earned its keep.** Migrating `devinit`'s tests off the
  store seam left several writing a real `.env` under the sentinel HOME; the guard
  caught it, and the shared file then made five unrelated tests pass vacuously
  (credentials present ⇒ the reuse path short-circuits ⇒ the errors they assert
  never fire). Both symptoms, one cause.

## Acceptance (whole plan)

- The documented flow is **`openbox auth` then `openbox init`**, and each command
  fails with a pointer to the other when run out of order.
- `openbox auth` run twice with different input leaves the SECOND input in
  `~/.openbox/.env` (fixes: `init` can never update — `devinit.go:198`).
- A developer who follows the platform docs and sets `OPENBOX_AGENT_PRIVATE_KEY`
  is honoured (fixes the one real name divergence — see phase 3).
- `dev.json` after `auth`: coordinates written, every posture field
  byte-identical; `WouldDowngradeEnforce` never trips.
- `agent_id` always written (fixes `devinit.go:267` reuse-path gap).
- `openbox init` with no flags installs **project-local** hooks for the current
  directory and says so; `--scope global` states the manual managed-settings step
  it cannot perform itself.
- No secret ever on argv (INV-1).
- All 11 modules: `go build`, `go vet`, `go test -race` green on macOS + Linux;
  `GOOS=windows go build` green in CI. **Windows and Linux runtime behaviour is
  not verifiable from the macOS dev host** — phase 8 names what must cover it.

**Every bullet above is verified except the last two clauses of the fourth**
(`dev.json` posture identical, `WouldDowngradeEnforce` never trips) — those are
verified by test but not against a live stack — and the end-to-end delivery claim,
which the testbed would settle and did not run. See
[the verification record](reports/verification-260813-auth-init.md).

## Out of scope

Posture changes beyond the scope default; a Windows installer (`install.sh` is
bash — phase 8 names the gap, does not fill it); shipping a Windows release
binary.

Ruled out during validation, each with a reason:

- **Migrating credentials out of the OS keychain** (D1). Existing installs
  recover with `auth --rotate` or re-registration; phase 8's migration note gives
  the manual keychain-read commands as an escape hatch for anyone who wants to
  keep their agent without an org key.
- **Moving the non-secret coordinates into `.env`** (D2). Would mean auditing 12
  files / 61 references for no behavioural gain.
- **Project-local hook scope for Codex.** Codex installs to
  `~/.codex/hooks.json`; repo-level `.codex/hooks.json` is a location its
  installer "deliberately does not touch" (`adapters/codex/installer.go:356`).
  `--scope local --provider codex` therefore errors rather than pretending.
  Named as a follow-up in phase 7.
- **Automatic global activation.** `Install` does not modify managed Claude Code
  settings by design (`adapters/claude-code/installer.go:99-101`); it prints the
  `enabledPlugins` snippet. Unchanged here.

## Successor plan

[Inline policy evaluation](../260813-0140-inline-policy-evaluation/plan.md) (created
2026-08-13) makes `/evaluate` the single decision authority and deletes the local policy
evaluator, bundle sync, staleness and signing. **This plan lands first** — both edit
`devconfig.go` heavily, and that one deletes fields this one is still moving.

Four collision points in this plan are already annotated with their shelf life: the
Tier-2/Tier-3 coupling question (resolved — flip `enforce` only), that decision's tier
wording (phase 1), phase 5's `agent_id` rationale, and phase 8's `bundle_version`
assertion. Do not design around the tier model here; it is being removed.

## Validation Summary

**Validated:** 2026-08-13 · **Questions asked:** 8 · **Verdict: revise before
implementing.** Two answers change the plan's shape (D2, D5); one adds a phase.

### Confirmed decisions

| # | Decision | Effect |
|---|---|---|
| D1 | **No automated keychain migration.** Existing installs recover via `auth --rotate` or re-registration. | Nothing reads the OS keychain before it is deleted. |
| D2 | **`.env` holds secrets only** — `OPENBOX_API_KEY`, `OPENBOX_AGENT_PRIVATE_KEY`, and (approver installs) `OPENBOX_CONTROL_TOKEN`. `dev.json` keeps `developer_did`, `agent_id`, `backend_url`, `base_url`. | One store per field; no DID drift. Avoids the 12-file / 61-reference audit that moving coordinates would need. |
| D3 | **The org control token is persisted** to `.env`, and that decision must name the escalation: an `obx_key_` org key with org-wide create/rotate authority, in plaintext, readable by the agent under governance. | Strictly larger blast radius than the agent seed. |
| D4 | **Pin `golang.org/x/term`** — the repo's first external dependency, for masked input + correct Windows TTY detection. | Phase 1 unchanged. |
| D5 | **`auth` and `init` split by role.** `auth` authenticates (and registers); `init` only sets up hooks in settings, takes `--scope local\|global` **defaulting to local**, and carries as few flags as possible. `auth` runs first. | New phase; phase 3 shrinks; phase 5 grows. |
| D6 | **Default-local is accepted with the gap recorded in a decision record** — per-project scope leaves sessions in every other directory ungoverned (`adapters/claude-code/localhooks.go:18`), and docs must point enterprise deployments at global/managed settings. | Inverts today's documented production posture, deliberately. |
| D7 | **New branch carrying all 17 commits** from `fix/tier2-duplicate-activity-started`; **one PR to `main`** covering the Tier-2 duplicate-`ActivityStarted` fix, that decision/0014 (tool-call and model-turn activities, token usage), and `auth` + `init`. | Done: `feat/dev-runtime-auth-and-init`. |

Added 2026-08-13 while drafting the README (`README.draft.md`), which doubles as the
UX spec for `auth` and `init`:

| # | Decision | Effect |
|---|---|---|
| D8 | **Both URLs get hosted defaults** — backend `https://api.openbox.ai`, core `https://core.openbox.ai`. | New `DefaultBackendURL` constant; today there is no backend default at all (`main.go:163` errors when unset). |
| D9 | **Agent id, not DID, is the registration trigger**, and a blank agent id **short-circuits the rest of the prompts** — registration returns the DID and both secrets, so asking for them first is pure friction. | Phase 5's field table is now ordered and numbered; fields 5-7 are unreachable on the register path. |
| D10 | **Enforce is ON by default**; `--enforce=false` opts out. | Reverses that decision's observe default. **Requires `Enforce` to become `*bool` first** — see Unresolved and phase 7 step 1a. Folded into that decision, which now covers both install defaults. |
| D11 | Token usage stays **ON by default** — already true (that decision, `Finops *bool`, 2026-08-11), confirmed not changed. | No work. |

### Verified during validation (evidence, not assumption)

- `init` genuinely cannot update: the reuse short-circuit at `devinit.go:198`
  returns before any network call. Plan's premise holds.
- `agent_id` is written only on the registration branch (`devinit.go:267`), so
  reuse-path installs do lack it. Holds.
- **Env names already match the platform docs except one.** `OPENBOX_AGENT_DID`,
  `OPENBOX_API_KEY`, `OPENBOX_AGENT_ID`, `OPENBOX_BASE_URL`,
  `OPENBOX_BACKEND_URL`, `OPENBOX_CONTROL_TOKEN` are all correct today
  (`devconfig.go:35-67`). The divergence is `OPENBOX_ED25519_SEED` alone, plus
  `OPENBOX_SEED` in the git action — a one-variable rename, not a family.
- Zero external Go dependencies repo-wide today, so D4 really is the first.
- `dev.json` is the posture layer (`enforce`, `fail_closed`, `tier2`,
  `secret_detection`, `findings`, `realtime_flush`, `content_capture`, `finops`,
  `install_git_hook`, timeouts, org signing pins — `devconfig.go:85-172`) with an
  org-controlled `ManagedConfig`/`Locked` layer above it (`managed.go:41-51`). It
  cannot be deleted; only its four coordinates overlapped with `.env`.
- Local hook scope is **additive on top of** the global bundle: `Install` always
  materializes the plugin + engine binary, then optionally merges project hooks
  (`installer.go:102-124`). Scope decides activation only.
- `approve.go:67` already prefers `OPENBOX_CONTROL_TOKEN` from the environment
  with the store as fallback, so D3 removes a fallback rather than a primary.

### Action items — **all applied 2026-08-13**

- [x] **plan.md:** layout rewritten as a two-file table (D2); `branch:` →
      `feat/dev-runtime-auth-and-init` (D7); `effort:` 23.5h → 28.5h (+4h phase 7,
      +1h that decision, −0.5h phase 3, +0.5h phase 5); phases table, ownership table,
      acceptance and out-of-scope all updated.
- [x] **Phase 1:** That decision gains the D3 org-key escalation and the D1
      no-migration statement; **that decision** added for the scope default (a
      separate decision deserves its own decision record, and someone asking "why is
      governance per-project by default" should find it under its own heading).
      2h → 3h.
- [x] **Phase 2:** writer scope reduced to the secret keys; preserve-unknown-keys
      kept and its rationale sharpened.
- [x] **Phase 3:** precedence split — secrets `env > .env > unset`, coordinates
      unchanged. The false success criterion is gone, replaced by its inverse.
      `init.go` ownership handed to phase 7. 5h → 4.5h.
- [x] **Phase 5:** coordinates go to `dev.json`, secrets to `.env`; `auth` owns
      registration outright, including the `--force` / duplicate-name surface it
      inherits from `init`. 4h → 4.5h.
- [x] **New phase 7** written: [`init` slimmed](phase-07-init-scope-and-slimming.md).
      Verification renumbered to phase 8 (it must stay last).
- [x] **Phase 8:** docs lead with `auth` → `init`; the local-scope governance gap
      is stated in getting-started, not only that decision; migration note carries the
      keychain recovery path.
- [x] **The zero-code keychain escape hatch** is in phase 3 (error text) and
      phase 8 (migration note).

### Unresolved

- ~~**`init`'s remaining flag surface.**~~ **Resolved and shipped:** exactly as
  proposed — 7 flags (`--provider`, `--scope`, `--enforce`/`--no-enforce`,
  `--install-git-hook`, `--dry-run`, `--role`), 7 moved to `auth`, 4 deleted.
  `--managed-enable` was **deleted** on the user's confirmation (D12).
- **Codex local scope.** Left unimplemented by decision; `--scope local
  --provider codex` errors with the repo-level-hooks reason, and a bare
  `init --provider codex` resolves to global while saying so. If per-project Codex
  governance matters, that is a bounded addition to
  `adapters/codex/installer.go` and needs its own decision.
- **`org` is not persisted** (D13). A re-run of `auth` does not remember it. Adding
an `org` field to `dev.json` changes a documented config contract, so it needs a
decision record rather than a commit.
- **One identity per machine** (surfaced by review). `.env` holds a single,
  un-namespaced key pair, where the deleted keychain namespaced by
  `<org>/<provider>` and could hold several. The reuse check therefore cannot tell
  whether existing credentials belong to the org and provider a run named, and
  `--force` does not bypass it (that ordering is pre-existing). The message is now
  honest about it and the limit is disclosed, but if multi-identity machines matter,
  namespacing `.env` keys is a decision, not a patch.
- **`install_git_hook` shares the flag pattern that caused the enforce bug**: a
  plain re-init reverts a previously-enabled ambient hook, because
  `provider.ConfigUpdate` always sets it non-nil and there is no
  `--no-install-git-hook`. Pre-existing and out of scope here; same class.
- **`openbox doctor` says nothing about credential presence**, which is now the
  most foundational fact about an install.
- **The testbed has not run.** The single largest gap. Everything about live
  delivery rests on unit tests and a mock backend until
  `./testbed/run-all.sh` executes against a real local stack.
- ~~**Does enforce-by-default also turn on Tier-2 and Tier-3?**~~ **Resolved
  2026-08-13:** `--enforce` sets all three (`main.go:391`), but the tier concept is
  being removed entirely by
  [inline policy evaluation](../260813-0140-inline-policy-evaluation/plan.md) —
  Tier-2 becomes the only decision path and `tier2`/`tier2_timeout_ms` become
  deprecated config. So this plan flips **`enforce` only** and leaves the `tier2`
  and `findings` writes exactly as they are; the other plan deletes them. Do not
  spend design effort on a coupling that is about to disappear.
