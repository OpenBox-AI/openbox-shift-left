# Phase 01 — Baseline, path map, rewrite instrument

## Context links

- Parent: [plan.md](plan.md)
- Depends on: nothing
- Source: `/tmp/project-layout` README §`/cmd`, §`/internal`
- Repo: [`go.work`](../../go.work), [`CLAUDE.md`](../../CLAUDE.md) §Current state

## Overview

- **Date:** 2026-08-30
- **Description:** Produce the exact before-state and the exact instrument, so the
  collapse in phase 03 is a mechanical application rather than a judgement call made
  444 times.
- **Priority:** P2 (blocking — every later phase reads its outputs)
- **Implementation status:** COMPLETE (2026-08-30)
- **Review status:** self-verified; report at `reports/baseline-260830.md`
- **Mutates Go code:** no

## Key insights

- **A refactor with no exact before-state cannot prove it lost nothing.** This repo
  already learned the general form: 635 tests once produced no verdict at all and
  were invisible rather than failing, and `gateway` ran 1 of its 81 tests while
  reporting a single failure. Counting declared tests against verdicts is the
  established local method; use it here as the baseline, not a fresh idea.
- **The verdict count is the acceptance number, not the pass count.** A collapse that
  drops a build tag, a `testdata` dir or a whole package still reports green.
- **Import rewriting is not `sed s/old/new/`.** Module paths are prefixes of each
  other (`.../adapters/common/git` vs `.../adapters/common/gateway`-shaped names) and
  appear in `go.mod`, `go.sum`, `.goreleaser.yaml`, shell scripts and docs as well as
  in Go imports. Longest-prefix-first ordering is required.

## Requirements

1. A machine-readable path map: every current module path and package directory →
   its target path. One file, consumed verbatim by phase 03.
2. A baseline record: per-package declared tests, verdicts, skips; the 61-gate matrix;
   binary size; `go list -deps` external set per future subtree.
3. A rewrite script that is idempotent and dry-runnable.
4. A collision report: any two source directories mapping to one target.

## Architecture

The map is the contract between phases. Shape (`path-map.tsv`):

```
kind      from                                              to
module    github.com/…/cli                                  github.com/…             (root)
dir       cli/cmd/openbox                                   cmd/openbox
dir       cli/internal/activation                           internal/cli/activation
dir       adapters/common/hookflow                          internal/adapters/common/hookflow
dir       client                                            internal/client
dir       contracts/dev-event/conformance                   internal/conformance
dir       gateway/internal/dialhook                         internal/gateway/internal/dialhook
…
```

Two rules the map must encode:

- **`internal/cli/…` keeps the `cli` grouping.** Flattening `cli/internal/*` to
  `internal/*` risks basename collisions and erases which subtree owns a package.
- **Nested `internal/` is preserved, deliberately.** `gateway/internal/dialhook` →
  `internal/gateway/internal/dialhook`. Under root-`/internal` the nested one still
  restricts importers to the `internal/gateway/…` subtree — the exact property
  `dialhook` exists to have ("no module outside `gateway` can import it" becomes "no
  package outside `internal/gateway` can import it"). Flattening it would silently
  widen a deliberate seam.
- **Dev instruments map to `tools/`, not `cmd/`** (D6): `cli/cmd/corpusfixture` →
  `tools/corpusfixture`, `probes/refusal-injector` → `tools/refusal-injector`.
- **Three rows are CONDITIONAL and must be marked so.** `openbox-cc-hook`,
  `openbox-git-hook` and `openbox-git-action` are deleted by phase 03's gate unless
  it proves them live (D5). Mark them `cond` in the `kind` column; the rewrite script
  must skip a `cond` row whose source no longer exists rather than erroring. A map
  that silently assumes they survive would make phase 03's gate unable to remove them
  without editing the map mid-phase.

## Related code files

| Path | Why |
|---|---|
| `go.work` | authoritative list of the 15 modules |
| every `go.mod` (15) | direct requires to union in phase 03 |
| `.goreleaser.yaml`, `.github/workflows/{ci,release}.yml` | non-Go consumers of module paths |
| `install.sh`, `testbed/lib/*`, `testbed/*.sh` | 61 shell references |

## Implementation steps

1. Enumerate modules from `go.work`; enumerate every directory containing `*.go`.
2. Draft `path-map.tsv` by the two rules above. Review it by hand — this is the one
   artefact no later automation checks.
3. Collision check: `cut -f3 path-map.tsv | sort | uniq -d` must be empty.
4. Baseline capture into `reports/baseline-260830.md`:
   - `go test -list '.*' $MODS` → declared count per package;
   - `go test -race $MODS` → `--- (PASS|FAIL|SKIP)` verdict count; record the delta;
   - `go vet`, both cross-compiles, `cd cli && GOWORK=off go build ./...`;
   - `go list -deps` external module set for each future `/internal` subtree
     (this is phase 02's input, gathered here while modules still exist);
   - release binary size via `GOWORK=off`.
5. Write the rewrite script **outside the repo** — the scratchpad, not a tracked path.
   It is throwaway tooling, and D4 means there is no `scripts/` directory to put it
   in. It applies the map to
   `*.go`, `go.mod`, `*.md`, `*.sh`, `*.yaml`, longest-prefix first, `--dry-run`
   default.
6. Dry-run; diff the would-be output; confirm the changed-line count reconciles with
   the measured 444 import lines + 45 replace lines.

## Todo list

- [x] `path-map.tsv` drafted and hand-reviewed — 34 rows (30 `dir`, 4 `cond`)
- [x] collision check empty; `go.work` coverage and Go-bearing-dir coverage also clean
- [x] baseline report written — per-PACKAGE counts (35 pkgs), 61-gate matrix, binary size, per-subtree dep sets
- [x] rewrite script written, idempotent, 14 fixture cases green, ordering drill RED on mutation
- [x] dry-run reconciles: 533 lines / 215 files, and all 4 unrewritten references accounted for (the plan's 444+45 estimate measures 437+99 today)

## Success criteria

1. `path-map.tsv` covers every `use ./x` in `go.work` and every Go-bearing directory.
2. Zero collisions.
3. Baseline records declared-vs-verdict counts per package, not just a pass/fail.
4. Dry-run produces a diff that touches no file outside the map.

## Risk assessment

| Risk | Mitigation | Signal it broke | Pre-decided response |
|---|---|---|---|
| Map misses a directory; phase 03 leaves an unbuildable import | derive from `go.work` + a filesystem walk, not by hand | dry-run diff touches a path absent from the map | fix the map, re-run; do not patch in phase 03 |
| Prefix-collision rewrite corrupts a longer path | longest-prefix-first ordering; unit-test the script on fixtures | dry-run shows a path with a doubled segment | fix ordering before proceeding |
| Baseline captured on a host with capability skips, understating coverage | record the 29 known capability skips explicitly as skips, not absences | verdict count differs from CLAUDE.md's 1,860 | reconcile before phase 02; a moving baseline invalidates criterion 3 |

## Security considerations

- **The enforce-path redactor rewrites developer files, and this phase is a bulk
  rewrite performed inside a governed session.** This repo has four demonstrated
  victims already, including a Go source file corrupted on write. After every bulk
  operation in this and all later phases: `grep -rn '\${OPENBOX_REDACTED_' .` must
  return nothing outside `plans/` and `docs/`. Treat a hit as corruption, not as a
  finding.
- No credential material is read or moved in this phase.

## Next steps

Phase 02 consumes the per-subtree external dependency sets to author the replacement
allowlists.

## Outcome (2026-08-30)

Complete. Baseline corroborates CLAUDE.md exactly — 1,278 declared, 1,860 verdicts,
29 skips, 61/61 gates, and a release binary of 40,287,986 bytes to the byte.

Seven findings changed later phases; see `reports/baseline-260830.md`. Three are
blocking-shaped and are carried into the phases they affect:

- **F1** — phase 02's "byte-identical allowlists" criterion is unachievable for
  `gateway` (its allowlist is repo-local-only, so an external-import list is empty)
  and `conformance` (its allowlist includes an `// indirect`, invisible to an
  import walk). Two guards also carry a second, unmentioned direction
  (`TestAllowlistHasNoDeadEntries`), and `gateway/guardscope_test.go` is already
  fixture-driven and needs no rescoping.
- **F2** — phase 03 breaks `schemaRelPath` and fails its own green gate before
  phase 05 gets to fix it. Phase 05's stated mechanism for that constant is also
  wrong: it resolves against the source file via `runtime.Caller(0)`, not the
  working directory.
- **F5** — phase 03 enumerates five guards to delete; **four more test files**
  (six tests) `t.Fatalf` when `go.work` disappears. One is a deletion whose
  justification is the collapse's own headline argument; three need a one-line
  marker retarget.
