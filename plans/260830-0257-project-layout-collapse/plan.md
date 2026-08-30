---
title: "Collapse to one module and adopt golang-standards/project-layout"
description: "Fold 15 modules into one with root /cmd, /internal and /tools, take the non-Go directory renames, and rebuild the six module-boundary guards as package-scoped controls before the boundary that carries them is removed."
status: complete
progress: "7 of 7 phases"
updated: 2026-08-30
priority: P2
effort: ~35h (7 phases)
branch: feat/tool-content-capture
tags: [layout, refactor, module-collapse, project-layout, adr-0011, adr-0023, release-path]
created: 2026-08-30
---

# Collapse to one module and adopt golang-standards/project-layout

Source: `golang-standards/project-layout` @ `/tmp/project-layout` (master, 2026-04-29).
Supersedes [260829-0304-layout-conventions-and-module-docs](../260829-0304-layout-conventions-and-module-docs/plan.md),
which explicitly refused the structural refactor. That refusal is now overridden by
owner decision (2026-08-30); the old plan's phases 04/05 survive here as phase 07.

## Owner decisions taken (2026-08-30)

| # | Decision | Consequence |
|---|---|---|
| D1 | **Full collapse to one module** | 15 `go.mod` → 1, 444 import lines rewritten across 203 files, 45 `replace` directives deleted, `go.work` deleted. ADR-0011 superseded. |
| D2 | **`/internal` only — no `/pkg`** | Nothing is published today (every inter-module require is `v0.0.0` + relative `replace`), so the compiler-enforced form is the right one. Matches the source's own advice. |
| D3 | **All four root renames** — *amended 2026-08-30* | `testbed/`→`test/`, `deploy/`→`deployments/`, schema→`api/` + unit templates→`init/`, `.goreleaser.yaml`→`build/`. **`install.sh` stays at the repo root and `scripts/` is not created** — see D4. |
| D4 | **`install.sh` stays at root; no `scripts/`** | The source's `/build` README blesses root placement when a tool demands the path ("don't worry if it's … keeping those files in the root directory makes your life easier"); `curl \| bash` demands a root URL exactly that way. `install.sh` is also the only shell script outside the testbed, so `scripts/` would hold one file — against "keep what you need and delete everything else". Avoids a second network and trust hop on a script whose job is verifying release checksums. |
| D5 | **Dead `cmd/` trees are verified and deleted in phase 03** — *gate outcome 2026-08-30: only ONE of the three was dead; see [gate-260830](reports/gate-260830-dead-tree-verdicts.md)* | GoReleaser builds only `./cmd/openbox`; `cli/cmd/openbox/main_test.go:764` states the trailer is stamped "with no separate openbox-git-hook binary"; `openbox-cc-hook` survives only in a comment; `actions/openbox-git-action` has no `action.yml`. Deletion is inside phase 03 by owner call, against the recommendation to do it beforehand — mitigated by splitting the phase into two commits. |
| D6 | **`tools/`, not `cmd/`, for dev instruments** | `/tools` = "Supporting tools for this project… can import code from `/pkg` and `/internal`". `corpusfixture` and `refusal-injector` go there, which also settles whether they ship: `tools/` is not a release surface. |

## The finding that shapes the plan

**Five of the six module-boundary guards read `go.mod` directly** — `decision`,
`telemetry`, `transport`, `gateway/guardscope_test.go` and
`contracts/dev-event/conformance/deps_test.go` all call `os.ReadFile("go.mod")` and
assert a short allowlist of direct requires. Under one module that file lists all 19
external dependencies at once, so every allowlist either fails outright or gets
"fixed" by widening to the union — which **silently destroys the control**.
ADR-0023's premise is that transitive code is bounded *at the module that took the
dependency*; one module means one bound.

So the guards are rebuilt as **package-subtree direct-import allowlists** in
phase 02, **before** the boundary carrying them is removed, with both forms green in
the same commit as the equivalence evidence. Phase 02 is not cleanup; it is the
precondition. If a phase-02 mutation drill comes back green, phase 03 does not start.

## What collapse costs and what it buys — stated, not averaged

- **Buys:** the `GOWORK=off` divergence class disappears entirely. This repo has
  already shipped that bug once (`cli` imported `transport` with a `require` and no
  `replace`; all 14 modules green in the workspace, the release path — the only path
  `.goreleaser.yaml` runs — could not resolve the import). No workspace, no
  `replace`, no divergence. This is an argument ADR-0011 did not have available.
- **Costs:** ADR-0011 reason 2 — "adapters must not import each other" was mechanical
  because they were separate modules. `/internal` cannot express it (both adapters
  land under `internal/adapters/`), so it becomes a test. Phase 02 extends
  `layering_test.go` to hold it. Recorded as a real downgrade, not a wash.

## Phases

| # | Phase | Effort | Depends on |
|---|---|---|---|
| 01 | [Baseline, path map, rewrite instrument](phase-01-baseline-and-rewrite-instrument.md) ✅ | ~4h | — |
| 02 | [Rebuild the six guards, package-scoped](phase-02-package-scoped-guards.md) ✅ | ~7h | 01 |
| 03 | [Dead-tree gate, then the collapse](phase-03-the-collapse.md) ✅ | ~10h | 02 |
| 04 | [Release path](phase-04-release-and-install-path.md) ✅ | ~3h | 03 |
| 05 | [Non-Go directory moves](phase-05-non-go-directory-moves.md) ✅ | ~4h | 03 |
| 06 | [ADRs and the layout maps](phase-06-adrs-and-layout-maps.md) ✅ | ~4h | 04, 05 |
| 07 | [Conventions and drift check](phase-07-conventions-and-drift-check.md) ✅ | ~3h | 06 |

## Target tree

```
api/            dev-event schema ONLY (no prose)      internal/    everything else (D2)
build/          .goreleaser.yaml                      test/        the e2e testbed
cmd/            the binaries that survive phase 03's  tools/       corpusfixture,
                verification gate — openbox, and                   refusal-injector
                any of the three proved live          docs/        + the contract prose
deployments/    managed configs                       install.sh   stays at root (D4)
init/           unit reference copies + README        go.mod       one
```

`api/` holds machine-readable artefacts only, per the source's own definition
("OpenAPI/Swagger specs, JSON schema files, protocol definition files"). `MAPPING.md`,
`COVERAGE.md` and the contracts README are prose *about* the wire and go to `docs/`.
`init/` is documentation-only — see phase 05 for why embedding the unit bodies is
rejected.

## Acceptance criteria

1. One `go.mod`; no `go.work`, no `replace` directive anywhere in the repo.
2. `go build ./...`, `go vet ./...`, `go test -race ./...` green, plus both
   cross-compiles — from the repo root, with no workspace involved.
3. Declared-test and verdict counts equal the phase-01 baseline **minus the tests
   belonging to the trees D5 deletes, enumerated by name** — and **skips compared
   per package**, because a stale path can turn a test into a skip, and a skip is
   still a verdict. Any other drop is a
   regression, not a simplification. (Without that subtraction this criterion would
   contradict D5 — the deleted trees carry `main_test.go` files.)
4. All six rebuilt guards are red on their mutation drills; no allowlist grew.
5. `goreleaser build --snapshot` produces a working `openbox` from `build/`.
6. The documented `curl … /main/install.sh | bash` URL still works — unchanged by
   construction under D4, asserted as a regression guard on the archive changes.
7. `cmd/` contains exactly the binaries GoReleaser builds, plus any of the three the
   phase-03 gate proved live. No entry is unaccounted for.
8. `testbed`/`test` suite unchanged in behaviour; every path reference updated.
9. ADR-0011 superseded and ADR-0023 amended, both recording what was lost.

## Out of scope

- **Behaviour changes to surviving code.** This is a move-and-rename refactor. Two
  deliberate exceptions: the `schemaRelPath` edit (phase 05), and D5's deletion of
  trees the phase-03 gate proves unreachable — which changes what the repo *contains*,
  never what the shipped binary *does*.
- **`/pkg`, `/vendor`, `/examples`, `/third_party`, `/website`, `/web`, `/configs`,
  `/assets`** — no content to put in them (D2 settles `/pkg`). `/tools` **is** taken,
  per D6.
- **`/scripts`** — would hold exactly one file, and D4 keeps that file at the root.
- **`/internal/app` + `/internal/pkg` substructure** — the source calls it optional
  and aimed at several apps sharing code. After phase 03's gate this repo has one
  application; the nesting would be ceremony plus another path churn.
- **Running the dormant testbed phases or probe A** — unrelated, still blocked on a
  live stack.

## Unresolved questions

Questions 1–3 and phase 04's are **closed** by
[advise-260830-1042](../reports/advise-260830-1042-project-layout-open-questions.md),
which re-read all 19 of the source's per-directory READMEs. Answers are folded into
D3–D6 and the phases; the report carries the reasoning. Remaining:

1. **`ext-core/` disposition.** `contracts/dev-event/ext-core/README.md` is a
   tombstone ("RETIRED 2026-07-15") whose retirement is already recorded in ADR-0004.
   It is not a contract artefact, so it does not belong in `api/`. Plan assumes
   **delete**, ADR-0004 being the surviving record — a duplicated retirement note is
   a drift surface, not a safeguard. Flipping to "move to `docs/`" costs nothing.
2. **If phase 03's gate finds one of the three suspect binaries reachable**, does it
   stay in `cmd/` or move to `tools/`? Decide on what reaches it: a product path keeps
   it in `cmd/`, a test or dev path moves it to `tools/`.
