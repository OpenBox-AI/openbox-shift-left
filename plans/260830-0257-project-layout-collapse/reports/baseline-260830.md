# Phase 01 baseline — the exact before-state

Captured 2026-08-30 on darwin/arm64, go1.27.0, workspace ON, from the repo root.
Every number below is measured. Where a number contradicts something the plan
assumed, the measurement wins and the contradiction is named rather than smoothed.

## Capture environment — one correction that had to be made first

`go test` in this sandbox **cannot write the default build cache**
(`~/Library/Caches/go-build`, "operation not permitted"). A capture run without a
redirect returns `[setup failed]` for **12 of 15 modules** while reporting
**0 failures and 5 skips at the verdict level** — the modules whose artefacts
happened to be warm passed, and the rest produced no verdict at all.

That is this repo's own documented failure mode reproduced *by the measurement
instrument*: a package reporting FAIL with two named tests is not a package with
two problems. All figures below were taken with `GOCACHE` and `GOTMPDIR`
redirected into the scratchpad, and the capture script asserts it saw exactly 15
`### MODULE` markers before writing its done-marker.

Two independent corroborations that the environment is the same one CLAUDE.md's
figures came from:

| Measure | CLAUDE.md | Measured here |
|---|---|---|
| declared tests | 1,278 | **1,278** |
| verdicts | 1,860 | **1,860** |
| capability skips | 29 | **29** |
| gate matrix | 61 | **61** (46 non-race + 15 `-race`) |
| release binary, darwin/arm64, `GOWORK=off` | 40,287,986 B | **40,287,986 B** |

## Test baseline

| | count |
|---|---|
| declared tests (`go test -list '.*'`, 15 modules) | **1,278** |
| verdicts (`--- PASS\|FAIL\|SKIP`) | **1,860** |
| — PASS | 1,831 |
| — FAIL | 0 |
| — SKIP (capability guards) | 29 |
| modules exiting 0 | 15 / 15 |
| packages reporting `[setup failed]` | 0 |

Verdicts exceed declared tests because a subtest emits its own verdict line. The
two are different measures and **both** are acceptance inputs: declared catches a
test that stopped being compiled, verdicts catch a subtest table that stopped
being iterated.

### Per-package declared, verdicts, skips

Package granularity is the point: phase 03's named risk is *a package silently
stopped being tested*, which a repo-wide total cannot see. Attribution comes from
`go test -json`; parsing `-v` text was tried first and over-counted by 69%, because
subtest verdicts and package result lines are not separable by regex.

```
package                                                                                     declared  verdicts  skips  fails
actions/openbox-git-action                         58        67        0      0
actions/openbox-git-action/cmd/openbox-git-action  7         7         0      0
adapters/claude-code                               191       323       0      0
adapters/claude-code/cmd/openbox-cc-hook           5         5         1      0
adapters/codex                                     99        148       0      0
adapters/common/devconfig                          86        118       0      0
adapters/common/git                                55        71        0      0
adapters/common/git/cmd/openbox-git-hook           4         4         0      0
adapters/common/hookflow                           76        88        0      0
cli/cmd/openbox                                    157       197       15     0
cli/internal/activation                            22        27        0      0
cli/internal/aivss                                 3         3         0      0
cli/internal/approver                              16        33        0      0
cli/internal/backend                               19        25        0      0
cli/internal/corpusfixture                         7         7         0      0
cli/internal/devinit                               18        18        0      0
cli/internal/gatewaycheck                          13        22        4      0
cli/internal/gatewayemit                           32        49        0      0
cli/internal/gatewayservice                        25        32        1      0
cli/internal/laneservice                           10        19        0      0
cli/internal/managed                               9         9         0      0
cli/internal/prompt                                14        27        0      0
cli/internal/providers                             5         5         0      0
cli/internal/telemetryemit                         18        37        0      0
client                                             129       219       0      0
client/acceptancetest                              4         4         1      0
client/memhttptest                                 1         1         0      0
contracts/dev-event/conformance                    19        19        0      0
decision                                           19        55        0      0
gateway                                            82        133       2      0
gateway/gatewaytest                                2         2         0      0
probes/refusal-injector                            4         4         0      0
provider                                           4         4         0      0
telemetry                                          22        33        0      0
transport                                          43        45        5      0
TOTAL                                                                                       1278      1860      29     0
```

`cli/internal/atomicfile` has no test file and so has no row — recorded so its
absence after the collapse is not read as a loss.

## Gate matrix — 61/61 green

15 modules × {`-race`, `vet`, `GOOS=windows GOARCH=amd64`, `GOOS=linux GOARCH=arm64`}
= 60, plus `cli` under `GOWORK=off` = **61**. Measured: 46 non-race gates PASS / 0
FAIL, and 15/15 modules exit 0 under `-race`.

## Path map

`../path-map.tsv` — 34 rows (30 `dir`, 4 `cond`). All four checks clean:

- **collisions** (two sources → one target): none;
- **`go.work` coverage**: every one of the 15 `use ./x` entries is covered, either
  by its own row or by rows for its subdirectories (`cli` has no row of its own —
  it dissolves entirely into `cmd/openbox`, `tools/corpusfixture` and
  `internal/cli/*`);
- **Go-bearing directory coverage**: every directory containing a `.go` file
  outside `plans/` is covered. `plans/260825-0027-.../probes/probe-server.go` is
  excluded deliberately — it is `//go:build ignore`, belongs to no module, and its
  own header says it lives in the plan tree on purpose;
- **row sources exist**: all 34.

## Rewrite instrument

`$TMPDIR/collapse/rewrite.py` — scratchpad, per the phase's own instruction (D4
leaves no `scripts/` to hold it and it is deleted after phase 03).

**Scope is narrower than "apply the map".** It rewrites exactly one form,
`github.com/openbox-ai/openbox-shift-left/<from>` → `.../<to>`, which is
unambiguous wherever it appears. Bare directory references (`dir: cli`,
`testbed/lib/...`) are **not** touched, because rewriting a bare `cli` in prose
matches English. Those belong to phases 04–06's hand sweeps.

14 fixture cases, all green. Two defects were found by writing them first:

1. **The boundary was an allowlist of terminators, and it had a hole.** A
   backtick- or pipe-terminated path — i.e. every inline code span and every
   markdown table in this repo's docs — fell outside the list and was silently
   left behind. The boundary now *forbids segment-continuation*
   (`(?![A-Za-z0-9_~+-])`) instead of enumerating what may follow. `/` and `.` are
   deliberately allowed to follow: `/` means the path continues into a subpackage,
   and no target directory contains a dot, so allowing it catches a path that ends
   an English sentence.
2. A fixture that was wrong about the map rather than about the script, which is
   how the "no bare `cli` row" fact above got established.

**Mutation drill, run:** replacing longest-`from`-first ordering with
shortest-first turns 3 of the 14 cases RED (`cli`/`client` corruption, nested
`cmd` vs its parent, the YAML case). The ordering is load-bearing, not decorative.

### Dry-run reconciliation — every line accounted for

| carrier | rewritten | left alone | why |
|---|---|---|---|
| `*.go` | 435 | 2 | both are `const … = "github.com/openbox-ai/openbox-shift-left/"` — the bare repo prefix, not a mapped subpath (`cli/cmd/openbox/modulewiring_test.go:40`, `gateway/guard_test.go:424`). Phase 02 owns both files. |
| `go.mod` | 98 | 1 | `cli/go.mod:1`'s `module` line. `cli/` has no map row and phase 03 deletes the file. |
| `.goreleaser.yaml` | 0 | 1 | line 6, a **comment** naming `…/cli`. Phase 04 rewrites this file by hand; its step 1 currently names only the `GOWORK=off` comment, so **line 6 must be added to that step**. |
| `*.md`, `*.sh`, `*.json`, `go.sum` | 0 | 0 | no qualified module path outside `plans/` |
| | **533** | **4** | 437 `.go` prefix lines and 99 `go.mod` prefix lines, fully partitioned |

215 files changed. Nothing outside the map is touched.

## Per-subtree direct external imports — phase 02's input

Measured with `go list` (`.Imports` + `.TestImports` + `.XTestImports`), not grep:
a grep sketch reported `api.anthropic.com/..` for `transport`, which is a URL in a
string literal, and `goproxy` for `gateway`, which is a word in a comment.

| future subtree | direct external imports |
|---|---|
| `internal/telemetry` | the 11 collector/otel/zap modules |
| `internal/decision` | `github.com/zricethezav/gitleaks/v8` |
| `internal/transport` | `github.com/elazarl/goproxy` |
| `internal/conformance` | `github.com/santhosh-tekuri/jsonschema/v6` |
| `internal/adapters/common/devconfig` | `godotenv`, `pelletier/go-toml` |
| `internal/adapters/common/hookflow` | `google/renameio` |
| `internal/cli` + `cmd/openbox` + `tools/corpusfixture` | `renameio`, `kardianos/service`, `golang.org/x/term` |
| `internal/gateway` | **none** |
| `internal/client`, `internal/provider`, both adapters, `adapters/common/git`, `tools/refusal-injector` | none |

## Findings that change later phases

These are the reason this phase exists. Each is a place where the plan as written
would have produced a wrong or unbuildable result.

### F1 — phase 02's "byte-identical allowlists" criterion is not achievable, for two guards

Phase 02 success criterion 3 reads *"Allowlist contents byte-identical to today's,
modulo formatting."* Measured against the actual guards:

- **`gateway`'s allowlist is `{…/client, …/decision}` — both entries repo-local.**
  It names no external module at all. A "direct external import" allowlist for
  `internal/gateway` is therefore **empty**, and an empty allowlist is a vacuous
  guard. Its replacement has to be a *repo-local* import allowlist, which is a
  different kind of list from the other four.
- **`conformance/deps_test.go` does not filter `// indirect`.** It checks every
  `require` line, which is why `golang.org/x/text` (an indirect) is on its
  allowlist. An import-based replacement cannot see an indirect at all, so `x/text`
  drops out by construction.

Also unaddressed by phase 02: `telemetry` and `transport` each carry a **second**
test, `TestAllowlistHasNoDeadEntries`, asserting no allowlist entry is stale.
Phase 02 specifies one direction per guard; there are two.

And `gateway/guardscope_test.go` is **already fixture-driven** — it seeds go.mod
bodies as strings and never reads the live file — so it does not need rescoping in
the way phase 02 assumes. It tests the scope *rule*, and the rule survives.

### F2 — phase 03 breaks the conformance schema path, before phase 05 fixes it

`contracts/dev-event/conformance/schema.go:21` holds
`schemaRelPath = "../schema/dev-event.schema.json"`. Phase 03 moves the package to
`internal/conformance/`, at which point `../schema/` resolves to `internal/schema/`
— which does not exist. **Phase 03's own green gate (its criterion 3) fails**, and
it fails at test time rather than compile time, which is the exact hazard phase 05
warns about. Phase 03 must edit `schemaRelPath` as part of its move; phase 05 then
edits it again when the schema goes to `api/`.

Phase 05's stated mechanism for this constant is also wrong and should be
corrected: it says the path *"resolves against the test's working directory"*. It
does not — `LoadSchema` uses `runtime.Caller(0)` and resolves against **the source
file**, deliberately and with a comment saying so. The conclusion (it needs
editing) survives; the reasoning does not.

### F3 — the `actions/openbox-git-action` **library** is not a D5 deletion candidate, and deleting its `cmd/` orphans 58 tests

D5's stated justification is "`actions/openbox-git-action` has no `action.yml`" —
and there is indeed no `action.yml` anywhere in the repo. But phase 03's gate table
scopes the tree as `actions/openbox-git-action/cmd/openbox-git-action`, and the
library above it is a different object:

| tree | declared tests |
|---|---|
| `adapters/claude-code/cmd/openbox-cc-hook` | 5 |
| `adapters/common/git/cmd/openbox-git-hook` | 4 |
| `actions/openbox-git-action/cmd/openbox-git-action` | 7 |
| `actions/openbox-git-action` (**library**) | **58** |

The library covers attestation, ownership verification, trailer resolution and the
API verifier's fail-closed paths — the "commit→deploy lineage for CI" that
`CLAUDE.md` lists under *shipped and verified end to end*. Its **only** importer is
its own `cmd/` (measured). So deleting the `cmd` leaves a 58-test shipped feature
with no consumer, which is a decision the gate has to take explicitly rather than
inherit. Parent criterion 3 subtracts "the tests belonging to the trees D5 deletes"
— 16 if the three `cmd/` trees go, 74 if the library goes with them.

### F4 — `cli/internal/*` → `internal/cli/*` widens an import boundary

Today `cli/internal/activation` is importable only from within the `cli` module.
After the map it is `internal/cli/activation`, importable by every package in the
repo. `gateway/internal/dialhook` is deliberately kept as
`internal/gateway/internal/dialhook` for precisely this reason, so the two rows
treat the same property differently.

The map follows the plan (the flattening is the plan's explicit rule, for basename
collisions and ownership legibility). Recording it as a **cost**, not a wash: 15
packages lose a compiler-enforced boundary and gain nothing but a shorter path.
Restoring it would cost one path segment per row and no behaviour.

### F8 — location-encoding strings come in four forms, and a grep finds two of them

`schemaRelPath` (F2) was the first one found. The full set only emerged when the
collapse actually broke things, and **the first two audits were both too narrow**
— which is the transferable part, because the same audit will be run again.

| form | example | found by |
|---|---|---|
| contiguous qualified import | `".../gateway"` | the rewrite instrument |
| leading-`../` literal | `"../schema/dev-event.schema.json"` | `grep '"\.\./'` |
| **segment-split `filepath.Join`** | `filepath.Join("..","..","..","telemetry","testdata",…)` | **nothing textual** |
| **concatenated from a bare prefix** | `repoPrefix + "/client"`, `repo + "gateway/gatewaytest"` | **nothing textual** |

**Audit one was too narrow twice over.** It grepped `"\.\./` and then filtered
`| grep -v testdata` to drop testdata *directories* — which also dropped every
line that merely mentioned testdata, and that is exactly where three fixture
paths lived. Two filters, one of them silently removing the answer.

**Audit two missed a whole form.** `filepath.Join("..", "..", "..", "telemetry",
…)` contains no `../` substring at all. Three fixture loaders in `cmd/openbox`
were found only when their tests failed after the move.

The complete audit is four greps, and it is worth keeping:

```
grep -rn 'openbox-shift-left/' --include='*.go' .      # qualified imports
grep -rn '"\.\./' --include='*.go' .                    # leading-../ literals, NO filters
grep -rnE 'Join\(\s*"\.\."' --include='*.go' .          # segment-split
grep -rn 'repoPrefix + "\|repo + "' --include='*.go' .  # concatenated
```

What each of the found paths needed:

| path | disposition |
|---|---|
| `conformance/schema.go` `schemaRelPath` | `../schema/…` → `../../contracts/dev-event/schema/…` |
| `cli/internal/gatewayemit/contract_test.go` `schemaRelPath` | **unchanged — survives by arithmetic**, both old and new locations are three deep |
| `devconfig/managed_test.go` deploy template | `../../../` → `../../../../`; **fails SILENTLY**, see below |
| `cmd/openbox/{telemetryreplay,transportreplay,main}_test.go` | `Join("..","..","..",X)` → `Join("..","..","internal",X)` |
| `devconfig/rolescope_test.go` `Join("..","..")` | **unchanged** — still resolves to the adapters directory |
| `gateway/guard_test.go` lookalike fixtures, `gatewaytest` dialhook path, all of `depguard` | concatenated or bare; hand-edited |

**The devconfig one is the dangerous member of the set**, and it is the reason
the acceptance criterion had to change.
`TestManaged_ShippedTemplateLoadsAndLocks` does `os.Stat` and `t.Skipf` on
failure, so a stale path **skips instead of failing** — quietly retiring a test
whose own subject is that unmanaged config fails silently. **A skip is still a
verdict**, so declared and verdict totals do not move; only the SKIP count does.
Phase 03's verification therefore compares **skips per package**, not just
declared and verdict totals, and the skip message now names the path it could not
find.

### F5 — phase 03 deletes `go.work`, and **four more test files hard-fail** on that

Phase 03 requirement 5 enumerates *"the five original go.mod-reading guards"*.
There are four more test files that the collapse breaks, none of them named
anywhere in the plan, and **each `t.Fatalf`s rather than skipping** when its marker
disappears — a deliberate design ("a guard that quietly passes because it scanned
nothing is worse than no guard"), which here converts into six hard failures in
phase 03's green gate that will look unrelated to the move.

| file | tests | what breaks | disposition |
|---|---|---|---|
| `cli/cmd/openbox/modulewiring_test.go` | 1 | reads `go.work` to enumerate modules; `t.Fatalf` if `len(modules) < 10` | **delete** — see below |
| `client/memhttptest/guard_test.go` | 1 | `repoRoot` walks up to the dir holding `go.work`; `t.Fatalf` if absent | retarget marker to the root `go.mod` |
| `gateway/gatewaytest/guard_test.go` | 2 | same marker, same `t.Fatalf` | retarget marker |
| `cli/internal/corpusfixture/committed_test.go` | 2 | same marker, same `t.Fatalf` | retarget marker |

The retarget is one line each: after the collapse the root `go.mod` is unique and
is exactly the marker `go.work` was chosen to be ("the only marker that is true
from every module in this workspace").

**`modulewiring_test.go` is a deletion, and it is the collapse's own argument.**
`TestEveryIntraRepoImportIsWiredInGoMod` exists because `cli` imported `transport`
with a `require` and no `replace`: the workspace was green and `GOWORK=off` — the
only path the release build takes — could not resolve the import. That is the bug
class phase 03's key insight says the collapse eliminates. With one module there
are no intra-repo module imports, no `replace`, and no workspace, so the property
is not weakened — it becomes **unrepresentable**. Deleting the test is correct;
lowering its `< 10` threshold to make it pass would leave a vacuous guard behind,
which is the failure mode this repo names repeatedly. **that decision (phase 06)
should cite this file by name as the concrete evidence for R1's inversion** — it
is the artefact of the bug, and its deletion is the bug class closing.

### F6 — the collapse **gains** two compiler-enforced boundaries, which the plan's cost/benefit does not record

The plan's cost side says `/internal` cannot express "adapters must not import each
other" (true, and phase 02 converts it to a test). The other direction was not
measured. Two packages carry hand-written AST walks **whose own doc comments say
they exist only because `internal/` was unavailable across module boundaries**:

- `client/memhttptest` — *"`internal/` is not available to keep it honest — six
  modules import it, so it has to be exported — which leaves this check as the only
  thing standing between a test helper and production code that silently reroutes
  every outbound dial."*
- `gateway/gatewaytest` — *"`internal/` cannot enforce it here: the point of the
  package is to be reachable from other modules. So this walk is the enforcement."*

Under one module both reasons evaporate: `internal/client/memhttptest` and
`internal/gateway/gatewaytest` are reachable from the whole repo, and the
production-import prohibition each walk enforces could instead be structural.

This is **not** proposed as work — it is out of scope, and `--yagni` applies. It is
recorded because the plan's honest-accounting standard requires the ledger to have
both columns, and that decision's "what it costs" section currently has only one.
R2 is a real loss; these are real gains available on the same day.

### F7 — the corruption check every phase mandates is red on a clean tree

Every phase specifies `grep -rn '\${OPENBOX_REDACTED_' .` must be empty outside
`plans/` and `docs/`. On the current, uncorrupted tree it returns **~25 hits**, in
three legitimate classes: comments documenting the four known corruption
incidents; test fixtures where the marker is the redactor's *expected output*
(`adapters/claude-code/mapper_test.go:380` — `const secret =
"${OPENBOX_REDACTED_AWS_KEY}"`); and plan visuals. The plan's own review visual
already flags it as *"right control, unusable form"*.

The usable form is a property of the **diff**, not the tree: fail when the marker
appears on an **added** line in a non-prose file. `$TMPDIR/collapse/redactcheck.sh`
implements it, and it is **drilled** — clean on the current tree, RED when a marker
is planted in a `.go` file, clean again on restore. An earlier version of it
reported "clean" on every run while the pipeline was erroring out (`{` is an
interval operator in BSD BRE, so the marker needs `-F`); its drill came back GREEN
and that is how the bug was found. Use the drilled script in every later phase, not
the literal grep.

## Unresolved questions

1. **F1.** How should phase 02 re-state criterion 3, and what does `gateway`'s
   guard assert once its allowlist is repo-local-only? Advisory counsel requested.
2. **F3.** Does the `actions/openbox-git-action` library stay (as
   `internal/actions/openbox-git-action`) if its `cmd/` is deleted? The map carries
   both as `cond` rows so the gate can decide either way without editing it.
3. **F4.** Accept the widened `internal/cli/*` boundary as a recorded cost, or
   preserve it as `internal/cli/internal/*`?
4. **F6.** Are the two now-expressible boundaries (`memhttptest`, `gatewaytest`)
   worth taking in a follow-up, or recorded and left? Out of scope here either way.
