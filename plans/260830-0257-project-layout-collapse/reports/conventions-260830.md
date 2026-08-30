# Phase 07 — conventions, and the check that stops them rotting

2026-08-30. The layout is written down, the naming rule is **measured**, and one
CI step fails when the tree stops matching. Suite unchanged: 1277 declared, 1861
verdicts, 28 skips, 0 failures.

## The CI check, and the two drills that made it real

`docs/architecture.md` §Layout has one row per top-level directory. CI fails,
naming the directory, when a directory on disk has no row. It replaces the step
that failed when a `go.mod` was missing from `go.work` — the same idea, one hop
over, from *registered* to *documented*. It reads **one** table: `CLAUDE.md`
carries a layout map too, and making both authoritative would recreate the drift
the check exists to stop.

**Drill 1 caught the check passing vacuously.** The first version used `grep -q`
for the bare directory name anywhere in the section. Deleting the `tools/` row
left the `cmd/` row's *"a dev instrument belongs in `tools/`"* behind, and the
drill came back **GREEN**. The check now requires a line that *starts* a table
row. That is the exact failure the phase's own risk table predicted — "matches any
line anywhere" — and only running the drill found it.

**Its first real run found a real problem.** An emptied `contracts/` directory was
still on disk, holding nothing but git-ignored tooling leftovers. The check named
it on the first invocation.

Five drills, all correct:

| drill | expected | got |
|---|---|---|
| delete the `tools/` row | RED, naming `tools` | RED |
| delete the `init/` row | RED, naming `init` | RED |
| delete the `api/` row | RED, naming `api` | RED |
| a new undocumented directory | RED, naming it | RED |
| the `## Layout` heading renamed away | RED — a missing section must not read as "nothing to check" | RED |

## The naming rule failed its own spot-check, and the rule was wrong

The phase's criterion 3 says: *"If a rule fails the spot-check, the rule is wrong
— not the files."* It did, and it was.

The first draft said **"Go filenames are flat lowercase with no separators."** The
measurement:

| | count |
|---|---|
| `.go` files in the tree (excluding `plans/`) | 381 |
| **non-test** files whose stem uses a separator | **0 of 180 — no exceptions** |
| files whose stem uses a separator | **25 — every one a `_test.go`** |

(An earlier draft said "356 — no exceptions" for the non-test row. 356 is 381
minus the 25 separator files, which is the wrong population: the claim was true,
the denominator was not. Two of the 25 were then renamed to restore a
`foo.go`/`foo_test.go` pairing, leaving 23.)

So the rule is narrower than written: **non-test files are exceptionlessly flat;
test files may separate words to name the subject they cover**
(`localhooks_quote_test.go`, `content_conformance_test.go`). That is a real
convention and a useful one — it is how a package with thirty test files stays
navigable. `CLAUDE.md` now states both halves and records that the first draft was
wrong.

**The earlier deviation scan could not have found this.** It filtered out anything
matching `_test.go`, which removes the whole population where separators actually
occur. The correct scan strips *every* trailing toolchain suffix and then looks at
the stem.

## Three filename deviations fixed, and one pairing I broke and restored

`enforce_evaluate.go` (×2) → `enforceevaluate.go`, `consts_paths.go` →
`paths.go`. A compiler no-op, confirmed rather than assumed: build, vet and both
cross-compiles re-run after the renames.

Renaming `enforce_evaluate.go` then left `enforce_evaluate_test.go` beside it,
breaking Go's `foo.go` ↔ `foo_test.go` pairing. Both renamed to match. Under the
corrected rule the test file was not itself a violation — the *pairing* was.

## `.editorconfig`, measured

| selector | setting | evidence |
|---|---|---|
| `[*]` | utf-8, lf, final newline, trim trailing whitespace | zero trailing-whitespace files in `.go` or `.md` |
| `[{*.go,go.mod,go.sum}]` | `indent_style=tab` | gofmt |
| `[*.{yml,yaml,json}]` | space, 2 | measured in `ci.yml` |
| `[*.md]` | space, **`trim_trailing_whitespace=false`** | documented snippets and table alignment rely on exact spacing |

**Two of the source's settings are rejected on measurement**, and the rejection is
written into the file so it is not re-imported on a fourth reading of the source
repo: `[*.md] indent_style = tab` would put **every** markdown file here in
violation on first save (there is not one leading tab in `README.md`, `CLAUDE.md`
or `docs/`), and `.gitattributes`' blanket `* -text` is a line-ending policy —
wrong for a repo shipping shell and cross-compiling for Windows — and not layout
in any case.

**`*.sh` is deliberately absent.** The repo is split: 17 scripts under `test/`
indent with tabs, `install.sh` with zero. One rule would put half of them in
violation on first save. Recorded as a known gap; unifying shell indentation is
its own change with its own diff.

## Subtree signposts — five, not three

The phase asked for a doc where a subtree has a boundary worth stating, and to
skip the rest. Five guarded subtrees got a **dependency-boundary signpost**
appended to their existing package comment (`internal/{gateway, decision,
conformance, telemetry, transport}`) saying the allowlist lives in
`internal/depguard` and that widening it to make an import pass inverts. The
signpost is not the enforcement, and it says so.

Appended to the **existing** package comment rather than added as a new `doc.go`:
a second package comment in the same package is a `go vet` finding.

**Skips, recorded as the phase requires:** every other `internal/` subtree. They
have nothing to say beyond their §Layout row, and three copies of one table is the
drift surface this phase exists to prevent. `internal/depguard` is also skipped —
every file in it is a `_test.go`, so it has no package doc for `go doc` to show,
and its own header already carries the reasoning at length.

**One stale pointer found while doing this:** `internal/conformance/schema.go`
still named `deps_test.go`'s allowlist as what bounds its dependency trade. That
file was deleted in the collapse. Corrected to name `internal/depguard`.

## Verified

| check | result |
|---|---|
| deleting a §Layout row makes CI fail **and name the directory** | PASS — three separate rows drilled |
| a new undocumented directory fails | PASS |
| a missing `## Layout` section fails rather than passing vacuously | PASS |
| `.editorconfig` produces no reformat diff | PASS — spot-checked across `.go`, `.md`, `.yml` |
| the naming rule survives a spot-check | **FAILED as first written; rule corrected against 381 files** |
| no non-test Go filename uses a separator | PASS — **180/180** |
| build, vet, gofmt, both cross-compiles after the renames | PASS |
| full suite | 1277 / 1861 / 28 skips / **0 fails** — unchanged |

## Unresolved

1. **Shell indentation is split** and `.editorconfig` says nothing about it.
   Unifying it is its own change.
2. The CI check covers the §Layout **table**. Nothing keeps `CLAUDE.md`'s command
   blocks true — and they were wrong twice over before phase 06 corrected them.
3. ~~`go mod tidy` is still owed~~ — **run 2026-08-30; `go.mod` unchanged.** See `collapse-260830.md`.
