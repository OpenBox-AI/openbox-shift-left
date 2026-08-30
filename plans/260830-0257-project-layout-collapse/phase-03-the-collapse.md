# Phase 03 — The collapse

## Context links

- Parent: [plan.md](plan.md) · Depends on: [phase 02](phase-02-package-scoped-guards.md)
- Supersedes in effect
(that decision itself is rewritten in phase
06)
- Source: `/tmp/project-layout` README §`/cmd`, §`/internal`

## Overview

- **Date:** 2026-08-30 (amended 2026-08-30 for D5/D6)
- **Description:** Prove three suspect `cmd/` trees dead and delete them, then
  15 modules → 1. Root `/cmd`, `/internal`, `/tools`. ~444 import lines rewritten,
  45 `replace` directives and `go.work` deleted.
- **Priority:** P1 · **Implementation status:** COMPLETE (2026-08-30) · **Review:** self-verified; report at `reports/collapse-260830.md`
- **Mutates Go code:** yes — deletions (commit a), then paths only (commit b)

**Two commits, not one.** (a) verification gate + deletions. (b) `git mv` + import
rewrite. D5 put the deletion inside this phase against the recommendation to do it
beforehand; the split is the mitigation. It restores `git log --follow` across the
move, and it lets a reviewer judge "is this dead?" separately from "did the move
preserve everything?" — two questions with different evidence. Do not merge them.

## Key insights

- **The whole phase is one commit or it is unreviewable.** A half-collapsed tree does
  not build, so there is no green intermediate to bisect to. Use `git mv` throughout
  so rename detection survives and `git log --follow` still works on 380 files.
- **Deleting `go.work` deletes a whole bug class.** The workspace is what hid
  `cli`'s missing `replace` for `transport`: 14 modules green, and `GOWORK=off` — the
  only path the release build takes — unable to resolve the import at all. After this
  phase there is no workspace and no `replace`, so workspace-vs-release divergence
  cannot exist. Say this in the commit message; it is the strongest single argument
  for the change and it is empirical, not aesthetic.
- **`go mod tidy` picks versions by MVS across the union.** Where two modules
  required different versions of one dependency, the merged module gets the higher.
  That is a real behaviour input for `gitleaks` (the redactor) and the collector — not
  a formatting detail.
- **Root `/internal` is importable by everything in the module**, so the collapse
  hands every package the ability to import every other. Only phase 02's tests and the
  *nested* `internal/` dirs still constrain that.

## Requirements

0. **The gate, before anything is deleted.** Each of `openbox-cc-hook`,
   `openbox-git-hook`, `openbox-git-action` gets a written reachability verdict with
   evidence. Delete only on a *proved* negative; a "could not find a caller" is not a
   proof. See the gate section below.
1. One `go.mod` at the repo root, module path
   `github.com/openbox-ai/openbox-shift-left`, `go 1.27.0`.
2. Direct requires = union of the 15 modules' direct requires (19 distinct external
   modules; `renameio` deduplicates from its two entries).
3. `go.work`, `go.work.sum`, 14 `go.mod`, 14 `go.sum`, all 45 `replace` lines gone.
4. Every path from phase 01's map applied; nothing outside it moved.
5. The five original go.mod-reading guards deleted (their replacements landed in 02).

## Architecture

### The gate (commit a)

Three trees are suspected dead. The evidence that raised the suspicion is *absence*,
which is the weakest kind, so each needs a positive check before deletion:

| Tree | Suspicion | What must be checked before deleting |
|---|---|---|
| `adapters/common/git/cmd/openbox-git-hook` | `cli/cmd/openbox/main_test.go:764` — trailer stamped "with no separate openbox-git-hook binary" | git hook installation path: does anything write a `.git/hooks/*` entry naming it? `adapters/common/git` installer argv |
| `adapters/claude-code/cmd/openbox-cc-hook` | survives only in a `hookrun.go:25` comment | `writeLocalHooks` / `ownedLocalHook` argv shapes, `plugin/hooks/hooks.json`, `localhooks.go` — the settings entries name a **command string**, so check what that string is |
| `actions/openbox-git-action/cmd/openbox-git-action` | directory has no `action.yml` | any workflow, doc or external repo referencing it; `.github/workflows/*` |

**Grep is not the check.** A binary name assembled by concatenation, or held in a
settings JSON string, will not appear as a Go identifier. For each tree the check is:
does any *install or invocation* path produce that executable's name? Record the
answer per tree in the phase report, with the file and line that settles it. If a
tree turns out reachable, it stays — see parent unresolved question 2 for where it
lands.

### The move (commit b)

`/cmd` — one directory per **shipped** executable, main package only:

| Target | From | Condition |
|---|---|---|
| `cmd/openbox` | `cli/cmd/openbox` | unconditional — the only target GoReleaser builds |
| *(up to three more)* | the trees above | only if the gate proves them live |

`/tools` — supporting dev instruments, per D6. Not a release surface:

| Target | From |
|---|---|
| `tools/corpusfixture` | `cli/cmd/corpusfixture` |
| `tools/refusal-injector` | `probes/refusal-injector` |

`/internal` — everything else, keeping today's grouping:

```
internal/adapters/claude-code   internal/client         internal/gateway
internal/adapters/codex         internal/conformance      └─ internal/dialhook   (nested, deliberate)
internal/adapters/common/…      internal/decision       internal/telemetry
internal/cli/…                  internal/provider       internal/transport
```

`adapters/common/` keeps its name: dropping it would make shared code and adapters
siblings and erase which is which.

## Related code files

203 files carry a repo-internal import. Highest-fanout consumers:
`cli/cmd/openbox/*` (11 module imports), `adapters/{claude-code,codex}` (7 each),
`adapters/common/hookflow` (4). `probes/refusal-injector` ships two committed
binaries (`refusal-injector`, `refusal-injector.exe`) that must **not** move into
`tools/` — delete and `.gitignore` them.

Gate inputs: `cli/cmd/openbox/main_test.go:764` · `adapters/claude-code/hookrun.go:25` ·
`adapters/claude-code/localhooks.go` (+ `plugin/hooks/hooks.json`) ·
`actions/openbox-git-action/` (absence of `action.yml`) · `.github/workflows/*`

## Implementation steps

**Commit (a) — gate and delete.**

0a. Run the three checks in the gate table. Write the verdict + settling file:line per
    tree into the phase report **before** deleting anything.
0b. Delete only the trees with a proved-negative verdict, with their modules
    (`go.mod`, `go.sum`) and their `go.work` entries. Enumerate the `main_test.go`
    files that go with them — parent acceptance criterion 3 subtracts exactly these.
0c. Green gate on the reduced tree, still multi-module: build, vet, `-race`, both
    cross-compiles. This proves the deletion alone broke nothing, which is the whole
    point of the split.

**Commit (b) — collapse.**

1. Author the root `go.mod` by hand from the union of the *surviving* modules; do
   **not** start from `cli`'s.
2. `git mv` every directory per the map, deepest-first.
3. Run the phase-01 rewrite script for real. Verify it touched only mapped paths.
4. Delete `go.work`, `go.work.sum`, the 14 `go.mod`/`go.sum` pairs.
5. `go mod tidy`. **Diff the resulting versions against phase 01's baseline** and list
   every dependency whose version moved.
6. Delete the five superseded go.mod-reading guards.
7. Remove the committed probe binaries; add them to `.gitignore`.
8. Green gate, from the repo root:
   `go build ./...` · `go vet ./...` · `go test -race ./...` ·
   `GOOS=windows GOARCH=amd64 go build ./...` · `GOOS=linux GOARCH=arm64 go build ./...`
9. Re-run the phase-01 baseline capture and diff declared/verdict counts.
10. If any dependency version moved in step 5, re-run the redaction conformance cases
    (C18/C26/C34/C39/C42, CDX-C10) before calling the phase green.

## Todo list

**Commit (a)**

- [x] three reachability verdicts written, each with a settling file:line — **two of the three are REACHABLE**; only `openbox-cc-hook` was dead
- [x] the one proved-dead tree deleted (it had no module of its own)
- [x] deleted tests enumerated by name — 5 from the alias binary in commit (a), 11 guards in commit (b)
- [x] green gate on the reduced multi-module tree; only the deleted package's counts moved

**Commit (b)**

- [x] root `go.mod` authored from the union (19 direct, 115 indirect, highest version pinned anywhere)
- [x] 452 renames detected; `git log --follow` reaches 10 commits through `internal/gateway/proxy.go`
- [x] 432 import lines rewritten; **four other forms of location-encoding string are invisible to any grep** and were hand-edited
- [x] `go.work`, `go.work.sum` and 29 module files deleted; zero `replace` in the repo
- [ ] **`go mod tidy` COULD NOT RUN** — the sandbox denies module-cache writes and breaks the checksum database. No dependency version moved, so step 10 has nothing to re-verify; that is only true because tidy did not run. Owed with network access.
- [x] superseded guards deleted, replacements green; **18 drills re-run on the collapsed tree**, 18 correct
- [x] probe binaries removed and ignored (they were never tracked — the plan's premise was wrong)
- [x] gofmt, build, vet, `-race`, both cross-compiles, and `GOWORK=off` — all from the repo root
- [x] 1288→1277 declared, 1884→1861 verdicts, 0 fails, **skips unchanged at 28** across the same 35 packages

## Success criteria

0. Every deleted tree has a written verdict naming the file:line that proves it
   unreachable. No tree deleted on absence of a grep hit alone.
0b. Commit (a) is green on its own, before any path moved.
1. `grep -rn '^replace\|=> \.\./' --include=go.mod .` returns nothing.
2. `find . -name go.mod` returns exactly one path.
3. Build, vet, `-race` test, both cross-compiles green from the root.
4. Declared-test and verdict counts equal phase 01's baseline **minus the enumerated
   tests of the deleted trees**, and nothing else.
4b. `cmd/` holds only shipped binaries; `corpusfixture` and `refusal-injector` are in
   `tools/`; no committed binary artefact survives anywhere.
5. Every dependency version change is listed and, where it touches `decision`,
   re-verified against the redaction conformance cases.

## Risk assessment

| Risk | Mitigation | Signal it broke | Pre-decided response |
|---|---|---|---|
| **A deleted binary was actually reachable** — an install path builds its name from a string, invisible to grep | the gate checks install/invocation paths, not identifiers; commit (a) is green on its own before anything moves | a testbed phase, an installer or a hook run fails naming a missing executable | restore from commit (a)'s parent; it is one revert because the deletion is its own commit — this is what the split buys |
| Deletion and move land together, so a post-hoc failure cannot be attributed | two commits, enforced | a reviewer cannot tell whether a break came from deleting or from moving | do not merge the commits, even to tidy history |
| MVS bumps `gitleaks` and changes redaction verdicts | step 5 diff + step 10 re-run | a conformance redaction case flips | pin the prior version explicitly; a redactor behaviour change is its own decision, not a refactor side effect |
| Verdict count drops — a package silently stopped being tested | phase-01 baseline is per-package | count < baseline | find the missing package before proceeding; do not accept "tests still pass" |
| A `//go:build` file is dropped or its constraint invalidated by the move | cross-compiles in the green gate; the six `_GOOS.go` files also carry explicit build lines | windows or linux/arm64 build fails | fix; never delete a constraint to make a build pass |
| `git mv` rename detection lost, blame broken on 380 files | `git mv` only, one commit, no content edits mixed into a pure move where avoidable | `git log --follow` stops at the move | acceptable if content had to change in the same commit; record which files |
| Root `/internal` lets any package import any other; a layering violation lands unnoticed | phase-02 tests are the only remaining control | `layering_test.go` red | fix the import, not the test |

## Security considerations

- Deleting the five go.mod guards is safe **only because** phase 02 landed. Verify
  the replacements are green in the same run that deletes the originals; do not split
  those across commits.
- that decision's bound changes shape here (module → subtree). The amendment is phase 06,
  but the change of fact happens in this phase — do not let the two drift.
- Bulk-rewrite corruption check after steps 3 and 5:
  `grep -rn '\${OPENBOX_REDACTED_' .` clean outside `plans/` and `docs/`.
- No credential, key or `.env` handling changes.

## Next steps

Phases 04 and 05 are independent of each other and both depend on this one; they can
run in either order.
