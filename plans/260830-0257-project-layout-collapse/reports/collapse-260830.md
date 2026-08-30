# Phase 03 — the collapse

Two commits, 2026-08-30. Commit (a) deletes one verified-dead tree and is green on
its own; commit (b) is the move.

## Result against the acceptance criteria

| # | criterion | result |
|---|---|---|
| 1 | one `go.mod`, no `go.work`, no `replace` | **met** — `find -name go.mod` returns 1; no replace anywhere |
| 2 | build, vet, `-race`, both cross-compiles, from the root, no workspace | **met**, plus `gofmt -l .` and `GOWORK=off go build ./...` |
| 3 | declared/verdict counts = baseline − the enumerated deleted tests | **met** — see the ledger below |
| 4 | six rebuilt guards red on their drills, no allowlist grew | **met** — 18 drills **re-run on the collapsed tree**, 18 correct |
| 7 | `cmd/` holds exactly the shipped binaries plus what the gate proved live | **met** — `cmd/openbox`, `cmd/openbox-git-action` |

Criteria 5, 6, 8, 9 belong to phases 04–06.

## The test ledger — every dropped test named

1288 → 1277 declared, 1884 → 1861 verdicts, **0 failures**, skips unchanged at 28,
same 35 packages. All eleven dropped tests are `go.mod`-reading guards whose
replacements landed in phase 02:

| file deleted | tests |
|---|---|
| `cli/cmd/openbox/modulewiring_test.go` | `TestEveryIntraRepoImportIsWiredInGoMod` |
| `contracts/dev-event/conformance/deps_test.go` | `TestDependenciesAreOnTheAllowlist` |
| `decision/guard_test.go` | `TestDirectDependenciesAreReviewed`, `TestGuardRejectsAnUnreviewedDirectRequire` |
| `gateway/guardscope_test.go` | `TestGoModGuardScope`, `TestLiveGoModPassesTheGuard`, `TestNarrowingAppliesToTheLiveFile` |
| `telemetry/guard_test.go` (trimmed, not deleted) | `TestOnlyReviewedDirectRequires`, `TestAllowlistHasNoDeadEntries` — `TestNoCredentialOrFileReads` **survives** |
| `transport/guard_test.go` | `TestOnlyReviewedDirectRequires`, `TestAllowlistHasNoDeadEntries` |

`modulewiring_test.go` is the one deletion that is an argument rather than a
consequence: it exists because `cli` imported `transport` with a `require` and no
`replace`, green in the workspace and unresolvable under `GOWORK=off`. With one
module that property is not weakened, it is **unrepresentable**. Lowering its
`len(modules) < 10` guard to make it pass would have left a vacuous test behind.

## Four forms of location-encoding string, and only one is greppable

This is the transferable finding. A rewrite instrument matches the contiguous
qualified import path; **four other forms encode a location and none contains that
string.**

| form | example | found by |
|---|---|---|
| contiguous qualified import | `".../gateway"` | the instrument (432 lines) |
| leading-`../` literal | `"../schema/dev-event.schema.json"` | `grep '"\.\./'` |
| **segment-split `filepath.Join`** | `Join("..","..","..","telemetry","testdata",…)` | nothing textual |
| **concatenated onto a bare prefix** | `repoPrefix + "/client"` | nothing textual |
| **bare directory name** | `name: "decision"` | nothing textual |

**Both audits were too narrow, and the tests are what found the rest.** The first
grepped `"\.\./` and then filtered `| grep -v testdata` to drop testdata
directories — which also dropped every line that merely mentioned testdata, and
that is exactly where three fixture loaders in `cmd/openbox` lived. The second
missed `filepath.Join("..","..","..",…)`, which contains no `../` substring at
all. The complete audit is four greps:

```
grep -rn 'openbox-shift-left/' --include='*.go' .
grep -rn '"\.\./' --include='*.go' .           # no filters
grep -rnE 'Join\(\s*"\.\."' --include='*.go' .
grep -rn 'repoPrefix + "\|repo + "' --include='*.go' .
```

**One of them fails silently, and that changed the acceptance check.**
`TestManaged_ShippedTemplateLoadsAndLocks` `Skipf`s when the shipped
managed-config template is absent, so a stale path retires a test whose own
subject is that unmanaged config fails silently. **A skip is still a verdict**, so
declared and verdict totals do not move — only the per-package SKIP count does.
Skips are now compared per package, and the skip message names the path it could
not find.

Two paths **survive by arithmetic and were left alone**, which is worth recording
so nobody "fixes" them: `cli/internal/gatewayemit/contract_test.go`'s
`schemaRelPath` (old and new locations are both three deep) and
`devconfig/rolescope_test.go`'s `Join("..","..")` (still the adapters directory).

## The root marker, and a prefix check that reads correctly and is wrong

Four tests walked up to the directory holding `go.work` and **`Fatalf`, not skip**,
when they could not find one. Three retarget to the root `go.mod`; the fourth is
`modulewiring_test.go`.

The retarget matches the module **line**, not a prefix of the file. The first
version used `strings.HasPrefix(b, "module …")` — and the root `go.mod` opens with
a comment block, so the module line is not at byte zero. It compiled, it read
correctly, and it walked straight past the repo root to `/`. Four tests failed
with `no root go.mod above /`, which is how it was caught.

## `go mod tidy` could not run here, and that is a real gap

The sandbox denies writes to the module cache and its TLS interception breaks
`sum.golang.org`, so tidy could neither take a lock nor verify a hash. Three
approaches were tried and the reason each failed is recorded because it narrows
what is actually unproven:

1. `GOPROXY=off` with an empty require list — tidy cannot *discover* which module
   provides a package without a lookup;
2. seeded requires + `GOPROXY=off` — got past discovery, then needed the checksum
   database for a hash not in any committed `go.sum`;
3. a `file://` proxy over the existing cache with a fresh writable `GOMODCACHE` —
   got furthest, then wanted **test-of-dependency zips this machine never
   downloaded** (`gonum`, `go-tpm-tools`), which the per-module tidies never
   needed either.

So `go.mod` is authored from the union of the fifteen, at the highest version any
pinned — the same selection MVS makes — and `go.sum` is the union of the twelve
committed, already-verified files. `go build ./...` needed no additions to it,
which is the evidence that the union is complete for the build.

**Unproven and owed:** run `go mod tidy` once with network access. The committed
`go.mod` may carry indirect requires a tidied one would drop, and no dependency
version moved here, so the phase's step-10 redaction re-run has nothing to
re-verify — a claim that is only true because tidy did not run.

## The corruption check needed a third form

The diff-based check from phase 02 produced **false positives across the move**:
git renders a moved-and-edited file as delete + add, so every pre-existing
`${OPENBOX_REDACTED_*}` marker in a moved file reads as an added line. Nineteen
fired, none of them real.

The form that works for a move is a **count**: markers in non-prose files, HEAD
versus working tree. **20 → 20.** The collapse introduced none. Keep all three
forms and pick by change shape — tree-wide grep is red on a clean tree, the diff
form is red on a move, the count form is blind to a one-for-one substitution.

## Gate outcome, and where the binaries landed

`cmd/openbox` (the only thing GoReleaser builds) and `cmd/openbox-git-action`
(`docs/lineage.md:90` documents the deploy invocation). `tools/` holds the three
that ship nowhere: `corpusfixture`, `refusal-injector`, and `openbox-git-hook`,
which only `integration_test.go`'s `TestMain` builds. Full reasoning in
`gate-260830-dead-tree-verdicts.md`, including the verdict that was **wrong on the
first pass** and why.

Rename detection survived: `git log --follow` reaches 10 commits through
`internal/gateway/proxy.go` and 17 through `cmd/openbox/main.go`. The release
binary is 40,311,474 bytes against 40,287,986 — **+0.06%**. An earlier "12.7 MB
smaller" reading was an artefact of comparing a `-ldflags "-s -w"` build against
one without; the flags, not the layout.

## Carried out of scope, deliberately

- `install.sh`'s from-source build path (`$SRC/cli/go.mod`, `cd "$SRC/cli"`) is
  **fixed here**, because the collapse breaks it and phase 04's "unmodified"
  criterion forbade the fix. That phase is amended: D4 decided the script does not
  **move**, never that it could not be **corrected**.
- `.goreleaser.yaml`'s `dir: cli` is removed for the same reason; its stale
  `GOWORK=off` entry stays for phase 04, which owns it.
- `ci.yml`'s two workspace-discovery steps still run and **degenerate gracefully**
  (1 = 1, and `go list -m -f '{{.Dir}}/...'` yields the root). Phase 04 deletes
  them as vacuous.

## Unresolved

1. `go mod tidy` with network access — see above.
2. `.goreleaser.yaml` still carries `GOWORK=off` and a comment describing a
   workspace and `replace` directives that no longer exist. Phase 04.
