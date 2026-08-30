# Phase 07 — Conventions and drift check

## Context links

- Parent: [plan.md](plan.md) · Depends on: [phase 06](phase-06-decisions-and-layout-maps.md)
- Inherits phases 02/04/05 of the superseded plan
  [260829-0304](../260829-0304-layout-conventions-and-module-docs/plan.md), whose
  analysis stays valid because it was measured rather than borrowed.
- Source: `/tmp/project-layout` — `.editorconfig`, and the per-directory README idea

## Overview

- **Date:** 2026-08-30
- **Description:** Write down the layout and naming conventions, and add the CI check
  that fails when the tree stops matching them.
- **Priority:** P3 — the only droppable phase, but it is what stops phase 06's work
  going stale.
- **Implementation status:** COMPLETE (2026-08-30) · **Review:** self-verified; report at `reports/conventions-260830.md`

## Key insights

- **A convention with no check rots, and this repo has the receipts.** Before this
  plan the tree had two entirely undocumented modules (`telemetry`, `transport` — the
  newest code in the repo), a stale module count, and three filename deviations. None
  was catchable by any test or CI step. The collapse resets the layout; without a
  check it will drift again from a cleaner starting point.
- **The precedent is already in `ci.yml`** — the step that failed when a `go.mod` on
  disk was missing from `go.work`. Phase 04 deletes it as vacuous. Replacing it with
  "every top-level directory has a row in `docs/architecture.md` §Layout" keeps the
  same idea one hop over, from *registered* to *documented*.
- **Validate one canonical table, not two.** `CLAUDE.md` and `architecture.md` both
  carry a layout map. Making both authoritative recreates the drift the check exists
  to stop. `architecture.md` is user-facing and already cited from `CLAUDE.md`, so it
  is the one CI reads.
- **The naming convention is measured, not invented.** Across ~380 Go files: flat
  lowercase with no separators (`approvalhold.go`, `outputcontract.go`,
  `failurepolicy.go`), underscore reserved for build constraints (`_test.go`,
  `_unix.go`, `_windows.go`), kebab-case for non-Go assets. Exactly three deviations
  existed pre-collapse.
- **A real conflict must be recorded, not silently resolved.** The user's global
  tooling guidance says "Go uses snake_case" for filenames. This repo does not, and
  neither does most of the standard library. Record the measured local convention and
  name the divergence, so the next reader does not "fix" ~380 files toward the
  generic rule.
- **The source's `[*.md] indent_style = tab` is measurably wrong here** (zero tabs
  across `README.md`, `architecture.md`, `CLAUDE.md`). Adapt, do not transplant.
- **Two source root files are rejected on measurement, and the rejection needs to be
  written down or it will be re-imported.** By this phase the source repo has been
  read closely three times, and each reading makes transplanting its root config look
  more like fidelity. It is not: (a) `.gitattributes` `* -text` is a blanket
  normalization opt-out, wrong for a repo shipping shell scripts and cross-compiling
  for Windows — the useful form is `*.sh text eol=lf`, which is line-ending policy,
  not layout, and belongs in its own change; (b) `[*.md] indent_style = tab` would put
  every markdown file in this repo in violation on first save. The source's own
  instruction covers both: "keep what you need and delete everything else."

## Requirements

1. `.editorconfig` at the root, every setting matching measured reality.
2. Naming and layout conventions written into `CLAUDE.md` §Working conventions.
3. CI step: every top-level directory documented in `docs/architecture.md` §Layout.
4. A `doc.go` or short README for each `/internal` subtree that has a boundary worth
   stating — and none for those that do not.
5. The three pre-existing filename deviations fixed, if they survived the collapse.

## Architecture

**`.editorconfig`, measured:**

| Selector | Setting | Evidence |
|---|---|---|
| `[*]` | `charset=utf-8`, `end_of_line=lf`, `insert_final_newline`, `trim_trailing_whitespace` | no trailing whitespace in any `.go` or `.md` today |
| `[{*.go,go.mod,go.sum}]` | `indent_style=tab` | gofmt |
| `[*.{yml,yaml,json}]` | `space`, size 2 | `ci.yml` measured |
| `[*.md]` | `space`, `trim_trailing_whitespace=false` | measured — **diverges from source** |

**`*.sh` is deliberately omitted.** The repo is split: `test/*.sh` indent with tabs,
the root `install.sh` with spaces. One rule would put half the shell scripts in
violation on first save. Record the split as a known gap; unifying shell indentation
is its own change with its own diff.

**The CI check**, shell + grep, no new dependency:

```
for each top-level dir (excluding dotfiles, docs, plans):
    grep -q "`<dir>/`" docs/architecture.md §Layout  ||  fail naming it + the remedy
```

**Per-subtree docs — the three-question form**, and nothing else: what this subtree
owns; what must *not* go in it; that decision that governs it. Skip any subtree with
nothing to say beyond its table row, and record the skip in the phase report — three
copies of one table is the drift surface this phase exists to prevent.

## Related code files

`.github/workflows/ci.yml` · `docs/architecture.md` §Layout · `CLAUDE.md` §Working
conventions · new `.editorconfig` · `internal/*/doc.go`

## Implementation steps

1. Write `.editorconfig` from the measured table.
2. Add the conventions to `CLAUDE.md`, including the provider-mandated exception
   (`managed_config.toml`, `requirements.toml` keep their underscores — Codex reads
   those exact names) and the snake_case divergence note.
3. Add the CI step next to the formatting check; fail with the directory named and
   the remedy, matching the existing `::error::` style.
4. **Run the deletion drill:** remove a row from §Layout → CI fails naming that
   directory; restore → passes. Run it; do not assert it.
5. Add subtree docs where there is a boundary to state; list the skips.
6. Fix surviving filename deviations with `git mv`; re-run build, vet and the
   cross-compiles. Go ignores filenames outside the constraint suffixes, so this
   should be a compiler no-op — confirm it rather than assuming.
7. Verify `.editorconfig` produces no reformat diff by opening a `.go`, a `.md` and a
   `.yml` in an EditorConfig-aware editor.

## Todo list

- [x] `.editorconfig` written from measurement; both rejected source settings named IN the file so they are not re-imported
- [x] conventions in `CLAUDE.md`, incl. the provider-mandated `.toml` exception and the snake_case divergence
- [x] CI check added — and its FIRST run found an emptied `contracts/` still on disk
- [x] five drills run, all red. **Drill 1 was GREEN first**: `grep -q` on the bare name matched another row's prose, so deleting the `tools/` row passed. The check now requires a table ROW
- [x] five dependency-boundary signposts appended to EXISTING package comments (a second `doc.go` is a vet finding); every other subtree skipped and the skips recorded; one stale pointer to the deleted `deps_test.go` found and fixed
- [x] three deviations fixed, plus the `foo.go`/`foo_test.go` pairing my own rename broke; build, vet and both cross-compiles re-run
- [x] no reformat diff, spot-checked across `.go`, `.md`, `.yml`

## Success criteria

1. Deleting a §Layout row makes CI fail and name the directory.
2. `.editorconfig` produces zero reformat diff against committed files.
3. The stated naming rule survives a 20-file spot-check (10 Go, 10 non-Go). If a rule
   fails the spot-check, the rule is wrong — not the files.
4. No subtree doc restates the §Layout table.
5. Full green gate after the renames.

## Risk assessment

| Risk | Mitigation | Signal it broke | Pre-decided response |
|---|---|---|---|
| CI check passes vacuously (matches any line anywhere) | run the deletion drill | drill green | rewrite the check; an unfired guard reads as coverage |
| `.editorconfig` reformats files on first save, producing noise in unrelated PRs | every setting derived from measurement, `*.sh` omitted | a save produces a diff | fix or drop the offending selector |
| Convention written from the source rather than from this tree | criterion 3 spot-check | a rule contradicts 10 sampled files | correct the rule |
| A rename silently drops a build constraint | never rename `_test.go`/`_unix.go`/`_windows.go`/`_GOARCH.go`; cross-compiles in the gate | a cross-compile fails or a file stops building | revert the rename |

## Security considerations

- None directly. One indirect: the subtree docs for `internal/gateway`,
  `internal/decision` and `internal/conformance` should state the dependency
  allowlist boundary, so someone adding an import meets the rule before the guard
  fails rather than after — the guards from phase 02 are the enforcement, the doc is
  the signpost.
- `.editorconfig`'s `trim_trailing_whitespace` must not apply to `*.md`: several
  documented shell snippets and tables rely on exact spacing.

## Next steps

Plan complete. Remaining repo work is unchanged by this refactor: the dormant testbed
phases (35, 45, 46, 47), probe A, and the `${OPENBOX_REDACTED_*}` false-positive
ruling — none of which this plan touches.

## The naming rule failed its own spot-check

Criterion 3 says *"if a rule fails the spot-check, the rule is wrong — not the
files."* It did. The first draft said "Go filenames are flat lowercase with no
separators"; measured, that is **356 of 356 non-test files with no exceptions**,
and **25 files with a separator, every one a `_test.go`**. The rule is narrower
than written: non-test files are exceptionlessly flat, and test files may separate
words to name the subject they cover. `CLAUDE.md` states both halves.

The earlier deviation scan could not have found this — it filtered out everything
matching `_test.go`, which is the entire population where separators occur.
