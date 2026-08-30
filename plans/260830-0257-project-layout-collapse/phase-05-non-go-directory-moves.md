# Phase 05 — Non-Go directory moves

## Context links

- Parent: [plan.md](plan.md) · Depends on: [phase 03](phase-03-the-collapse.md)
- Source: `/tmp/project-layout` README §`/test`, §`/deployments`, §`/api`, §`/init`
- Repo: `testbed/`, `deploy/`, `contracts/dev-event/schema/`,
  `cli/internal/laneservice/service.go`

## Overview

- **Date:** 2026-08-30 (amended 2026-08-30 — `api/` narrowed, `init/` settled as
  docs-only, `ext-core/` deleted)
- **Description:** `testbed/`→`test/`, `deploy/`→`deployments/`, schema→`api/`,
  contract prose→`docs/`, unit reference copies→`init/`.
- **Priority:** P2 · **Implementation status:** COMPLETE (2026-08-30) · **Review:** self-verified; report at `reports/directories-260830.md`
- **Mutates Go code:** yes — exactly one constant (`schemaRelPath`). The `init/`
  decision no longer carries a code change.

## Key insights

- **Only some of this phase is renaming.** `testbed/`→`test/`, `deploy/`→
  `deployments/` and the contract prose→`docs/` move inert files. The schema move
  breaks a live relative path; `init/` does not exist as files at all today — the unit
  bodies are Go strings inside `laneservice`, and the amended decision is that they
  stay there; `ext-core/` is a deletion. Treating the set as one "rename" batch is how
  the schema breakage would reach a green CI and fail at runtime.
- **`schemaRelPath = "../schema/dev-event.schema.json"` is a runtime relative path,
  not a `go:embed`.** It resolves against the test's working directory. Moving the
  schema to `/api` requires editing it, and after the phase-03 collapse the package
  also sits somewhere new — so the relative depth changes twice. This is the one
  place in the plan where a wrong path compiles fine and fails only when the
  conformance suite runs.
- **`deploy/`→`deployments/` is optional even under strict adoption.** The source
  says explicitly that some repos call it `/deploy`. It is taken here because D3 asked
  for it; worth knowing it is the cheapest move in this plan to drop if the doc churn
  is not wanted.
- **`test/` is a normal directory to the Go toolchain.** Go ignores `testdata` and
  names starting with `.` or `_`; `test/` holds only shell, so `./...` is unaffected.
- **`requirements.toml` and `managed_config.toml` keep their names** wherever they
  land. Codex reads those exact filenames; the underscore is provider-mandated, not
  a convention slip.

## Requirements

1. `testbed/` → `test/`, all 61 shell references and 34 markdown lines updated,
   including `run-all.sh`, `lib/`, `env.sh` and `docs/testbed/e2e.md`.
2. `deploy/` → `deployments/`, 6 markdown lines updated.
3. `contracts/dev-event/schema/dev-event.schema.json` → `api/`; `schemaRelPath`
   corrected; conformance suite green. **`api/` takes machine-readable artefacts
   only** — no `.md` files land there.
4. `init/` is **documentation-only**: reference copies of the rendered units plus a
   README pointing at `laneservice`. No `go:embed`, no extraction.
5. Contract prose (`MAPPING.md`, `COVERAGE.md`, `contracts/dev-event/README.md`) →
   `docs/`. `ext-core/` is **deleted** — see below.

## Architecture

| From | To | Kind |
|---|---|---|
| `testbed/**` (21 scripts, `lib/`, `mcp/`) | `test/**` | inert rename + 95 refs |
| `deploy/managed/**` (6 files) | `deployments/managed/**` | inert rename + 6 refs |
| `contracts/dev-event/schema/*.json` | `api/` | **code change** (`schemaRelPath`) |
| `contracts/dev-event/{MAPPING,COVERAGE,README}.md` | `docs/` | inert |
| `contracts/dev-event/ext-core/` | **deleted** | tombstone; ADR-0004 is the record |
| unit reference copies | `init/` | **docs-only — no code change** |

**Why the prose does not follow the schema.** `/api` is defined narrowly by the
source: "OpenAPI/Swagger specs, JSON schema files, protocol definition files" —
machine-readable artefacts. `/docs` is "Design and user documents". `MAPPING.md` is
prose about how fields land in core's columns and `COVERAGE.md` is a per-provider
matrix; both are documents *about* the wire, not the wire. Cost of the split,
accepted: someone reading the schema goes to `docs/` for the field mapping. Revisit
if contract-version bumps start touching both in lockstep.

**`ext-core/` is deleted, not moved.** `contracts/dev-event/ext-core/README.md` is a
tombstone — "RETIRED 2026-07-15" — for a directory that once patched openbox-core's
event-type accept-list. ADR-0004 already records that retirement. A duplicated
retirement note in a directory whose parent is disappearing is a drift surface, not a
safeguard. Flagged as parent unresolved question 1; flipping it to "move to `docs/`"
costs nothing if you prefer belt-and-braces.

**`init/` is documentation-only, and this is where the source's literal wording
loses.** `/init` means "System init … configs" — static files a packager installs.
These units are **rendered at install time** with substituted binary path, `HOME`,
args, and an `ExitTimeOut` that must track `--shutdown-grace`, and
`kardianos/service` owns the render. An extracted `go:embed` copy would be a second
copy of derivable state that must stay in sync with what the library produces — the
exact shape this repo already rejected in phase 12, where the election was written as
a stored field, tested green, and **reverted**, because "a second store of derivable
state … drift is silent in the worst direction". Two silent failure modes are live
here: a plist missing `StandardErrorPath` logs nowhere, and a mismatched `ExitTimeOut`
gets the daemon SIGKILLed mid-drain every restart. Neither surfaces as an error.

So `init/` holds reference copies + a README naming `laneservice` as the authority.
The copies are illustrative and must say so, or they become the second store by
another route.

## Related code files

`contracts/dev-event/conformance/schema.go:21` (`schemaRelPath`) ·
`cli/internal/laneservice/service.go` · `cli/cmd/openbox/{initlane,initgateway,telemetry,transport,doctor}.go` ·
`testbed/run-all.sh`, `testbed/env.sh`, `testbed/lib/*` · `docs/testbed/e2e.md` ·
`docs/gateway-mdm-recipe.md` · `deploy/managed/README.md`

## Implementation steps

1. `git mv testbed test`; rewrite the 61 shell refs and 34 md lines; run
   `test/00-preflight.sh` and confirm it fails for its usual reason (no stack) rather
   than a path error — the distinction is the whole point of running it.
2. `git mv deploy deployments`; update 6 md lines; check
   `docs/gateway-mdm-recipe.md` end to end since MDM users follow it literally.
3. `git mv contracts/dev-event/schema/*.json api/`; fix `schemaRelPath`; run the
   conformance suite (C1–C41) and confirm 38 cases still execute — a path error here
   presents as cases silently not running, which this repo has been bitten by before.
4. Populate `init/` with reference copies + a README naming `laneservice` as the
   authority. Each copy states it is illustrative and not loaded at runtime. Confirm
   no `go:embed` was introduced: `grep -r 'go:embed' init/` must be empty.
5. Move the contract prose to `docs/`; delete `ext-core/`; fix cross-links.
6. Full green gate again (build, vet, `-race`, both cross-compiles).

## Todo list

- [x] `test/` moved, references swept, preflight runs and fails only on the missing stack — its own remediation text already names the new path
- [x] `deployments/` moved; every path the MDM recipe documents resolves; **three user-facing CLI strings** named the old path and were corrected
- [x] schema in `api/`; **BOTH** `schemaRelPath` constants fixed (the second survived the collapse by arithmetic and not this move); 38 `C*` + 11 `CDX-C*` cases still execute, conformance's own 18 verdicts unchanged
- [x] `api/` holds no `.md` — one file, the schema
- [x] `init/` holds six units **generated by the real renderer**, each bannered illustrative, plus a README naming `laneservice` as the authority; no `go:embed`; all three plists parse as XML after a `--` in a comment was found to break one
- [x] contract prose in `docs/` (names kept — cited across stateful plan records); `ext-core/` deleted and its one live referrer rewritten to carry the fact instead of the link; `docs/testbed/` renamed too, because the sweep turned two live links into dangling ones
- [x] build, vet, both cross-compiles, gofmt; suite identical to post-collapse — 1277 / 1861 / 28 skips / 0 fails

## Success criteria

1. No path reference anywhere in the repo names `testbed/`, `deploy/`, or the old
   schema location.
2. Conformance runs the same number of cases as before, and they pass.
3. Unit rendering is **untouched** — no Go file under `internal/cli/laneservice`
   changed in this phase, and `init/` contains no `go:embed`. This is now a
   no-code-change property rather than a test-defended one.
4. `test/run-all.sh` reaches the same failure point as before the move.
5. `ls api/*.md` is empty; the contract prose resolves under `docs/`.

## Risk assessment

| Risk | Mitigation | Signal it broke | Pre-decided response |
|---|---|---|---|
| Schema path wrong; conformance cases stop executing but suite reports green | count executed cases (38), do not read pass/fail | fewer than 38 cases run | fix before proceeding; this is the repo's known "invisible tests" failure mode |
| `init/`'s reference copies drift from what `laneservice` renders and someone treats them as authority | copies are labelled illustrative; README names `laneservice`; no `go:embed` | a copy is edited as if it were the source, or an embed appears | delete the copy rather than syncing it — the whole point of docs-only is having one authority |
| A later change "improves" `init/` by embedding the templates | the rationale is recorded here and in ADR-0024 | a PR adds `go:embed` under `init/` | point at the phase-12 election precedent; this needs a decision, not a commit |
| A testbed script builds a path by string concatenation the sed misses | run preflight; grep for `testbed` after the move | runtime "no such file" in a dormant script | fix; dormant scripts get no CI cover, so grep is the only net |
| MDM recipe stale, org deployments break | walk `docs/gateway-mdm-recipe.md` literally | a documented path 404s | fix doc in the same commit as the move |

## Security considerations

- `deployments/managed/**` holds managed-settings and requirements files that gate
  what the governed tools may do. A wrong path in the MDM recipe means an org deploys
  nothing and believes it deployed policy — silent absence of governance, which OD4
  already classifies as a finding rather than an absence.
- The schema is the contract that validates outbound events. If conformance stops
  running, the wire contract stops being checked; hence criterion 2 counts executed
  cases rather than trusting green.
- Post-move corruption check: `grep -rn '\${OPENBOX_REDACTED_'` clean.

## Next steps

Phase 06 rewrites the layout maps against the finished tree.
