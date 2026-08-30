# Phase 04 — Release path

## Context links

- Parent: [plan.md](plan.md) · Depends on: [phase 03](phase-03-the-collapse.md)
- Source: `/tmp/project-layout` README §`/build`, §`/scripts`
- Decision: parent **D4** — `install.sh` stays at root, `scripts/` is not created
- Repo: `.goreleaser.yaml`, `.github/workflows/{ci,release}.yml`, `install.sh`

## Overview

- **Date:** 2026-08-30 (amended 2026-08-30 — the installer move is dropped per D4)
- **Description:** Move the release config to `build/`, simplify CI now that there is
  no workspace to discover, and give the release path its first automated coverage.
- **Priority:** P1 — the release path is the one thing in this repo with no automated
  coverage; ADR-0011 named that as reason 1 for not collapsing.
- **Implementation status:** COMPLETE (2026-08-30) · **Review:** self-verified; report at `reports/release-260830.md`

## Key insights

- **This phase is where ADR-0011's strongest objection is either discharged or
  confirmed.** Reason 1 was that GoReleaser builds from `cli` with its own `replace`
  graph and the release path has no test coverage. After the collapse there is no
  `replace` graph and no `GOWORK=off` divergence — the config gets *simpler*. But
  "simpler" is not "verified": run `goreleaser build --snapshot` and **execute** the
  resulting binary. That is the coverage the ADR said was missing, and it is cheap
  now. It is also the thing that makes the collapse defensible rather than merely
  tidy.
- **`GOWORK=off` becomes meaningless and must be deleted, not kept "for safety".** A
  stale env var that no longer expresses anything is how a reader later concludes a
  workspace still exists. Its comment ("all 12 modules at once") is already stale by
  three modules today.
- **`.github/workflows/` cannot move.** GitHub requires that exact path. Only
  `.goreleaser.yaml` goes to `build/`, and the release workflow must then pass
  `--config`. The source README anticipates precisely this ("some of the CI tools are
  very picky about the location of their config files").
- **`install.sh` does not move, and the source is why.** The same `/build` paragraph
  ends: "don't worry if it's not and if keeping those files in the root directory
  makes your life easier". `curl -fsSL …/main/install.sh | bash` demands a root URL
  exactly the way Travis demands a config path. Under `curl | bash` the script arrives
  on stdin, so `$0` is not a path and a `dirname`-based shim cannot work at all; the
  only functioning shim re-fetches over the network, adding a second trust hop to a
  script whose job is verifying release checksums. And `install.sh` is the only shell
  script outside the testbed, so `scripts/` would contain one file. Three independent
  arguments, one conclusion.

> **AMENDED 2026-08-30, during phase 03.** D4 decided `install.sh` does not
> **move**. It never decided the script could not be **corrected**, and the
> collapse forces one correction: the from-source build path (`$SRC/cli/go.mod`
> as the checkout marker, `cd "$SRC/cli"` to build) names a directory that no
> longer exists. That breaks the plain clone-and-build path, not just
> `OPENBOX_SRC`. Left as written, this phase's "unmodified" criterion and the
> parent's "unchanged by construction" would have forbidden a fix the collapse
> makes necessary, and the contradiction would have been resolved ad hoc
> mid-phase. The two-line fix landed in phase 03's commit (b), with the moved
> testbed and CI paths it belongs beside; the criterion below now guards against a
> ride-along edit rather than against any edit at all.

## Requirements

1. `build/.goreleaser.yaml`: no `dir:`, no `GOWORK=off`, `main: ./cmd/openbox`.
2. `release.yml`: `args: release --clean --config build/.goreleaser.yaml`.
3. `install.sh` stays at the repo root. No `scripts/` directory. **It is not
   byte-frozen** — see the amendment below.
4. `ci.yml`: the two workspace-discovery steps deleted, replaced by `./...`.
5. Snapshot build produced **and the binary executed**.

## Architecture

`ci.yml` loses these, which now describe nothing:

- *Check go.work covers every module* — compares `find -name go.mod | wc -l` against
  `go list -m | wc -l`. With one module both are 1; the step is vacuous, and a vacuous
  guard is worse than none because it reads as coverage.
- *Resolve workspace modules* (`MODS=…`) — existed only because "the repo root is not
  itself a module, so `./...` matches nothing". The root is a module now.

`Build`/`Vet`/`Test` become `go build ./...` etc. Keep the `gofmt -l .` step and the
Go-version comment block **verbatim**; that comment records a security rationale
(Go patches only the two most recent majors) unrelated to layout.

**What stays at the root, and why it is recorded.** `install.sh` and
`.github/workflows/` both deviate from a directory-for-directory reading of the
source. Both are tool-mandated, and the source blesses exactly that. One line in
`docs/architecture.md` §Layout (phase 06) should say so, or the next reader will
"finish the job".

## Related code files

`.goreleaser.yaml` · `.github/workflows/release.yml` (goreleaser-action step, line
~31) · `.github/workflows/ci.yml` (workspace steps) · `install.sh` (12.8 KB,
unchanged) · `README.md:21` and `docs/getting-started.md:27` (the documented URL,
unchanged)

## Implementation steps

1. `git mv .goreleaser.yaml build/.goreleaser.yaml`; delete `dir:` and the
   `GOWORK=off` env entry together with its now-false "all 12 modules" comment.
2. Confirm `main: ./cmd/openbox` still resolves after phase 03's move.
3. Add `--config build/.goreleaser.yaml` to the release workflow's `args`.
4. Rewrite the `ci.yml` steps as above; keep the formatting and Go-version steps
   untouched.
5. `goreleaser build --snapshot --clean --config build/.goreleaser.yaml`; then run
   `./dist/…/openbox --version` and `openbox doctor`. Executing it is the requirement;
   a successful build is not the same claim.
6. Compare the produced archive name against the one `install.sh` constructs from
   `uname` (`openbox_<version>_<os>_<arch>.tar.gz`). The template is the contract.
7. Confirm `install.sh` carries no diff **in this phase** — the source-path fix
   belongs to phase 03's commit (b), and anything else is a ride-along.

## Todo list

- [x] config moved; `dir:` removed in phase 03 as a moved path, `GOWORK=off` and its comment removed here
- [x] release workflow passes `--config`
- [x] **three** vacuous CI steps deleted — the third was the per-module `GOWORK=off` loop, now identical to the Build step above it; `./...` in use; gofmt and Go-version steps untouched; two stale comments corrected
- [x] snapshot built **and executed** — `--version` and `doctor` both exit 0 with real output
- [x] archive-name contract matches `install.sh:136`, unchanged by this phase
- [x] `install.sh` unchanged by this phase (its collapse fix rode with phase 03); no `scripts/`

## Success criteria

1. `goreleaser build --snapshot` succeeds from the repo root with the new config path.
2. The produced binary runs and reports a version.
3. `install.sh` is still at the repo root and `test ! -d scripts` passes. Its
   only permitted diff is the two-line source-path correction phase 03 made; any
   other hunk is the ride-along this criterion exists to stop.
4. CI contains no step that is trivially true.

## Risk assessment

| Risk | Mitigation | Signal it broke | Pre-decided response |
|---|---|---|---|
| Release path breaks and is only discovered at tag time | step 5 executes a snapshot binary — the coverage ADR-0011 said did not exist | snapshot build fails or binary won't run | fix before merging; never tag to find out |
| `main:` path stale after phase 03 | step 2 checks it explicitly | goreleaser cannot find the main package | fix the path, not the layout |
| Archive name drifts from what `install.sh` constructs | step 6 compares the two | installer 404s on the snapshot name | fix `name_template`, not `install.sh` — the template is the contract |
| A CI step deleted that was doing real work | diff the workflow and justify each removal in the PR body | a class of failure stops being caught | restore it in `./...` form |
| A later change "finishes the job" by moving `install.sh` | D4 recorded in the plan; the §Layout line from phase 06 states the exception | a PR proposes `scripts/install.sh` | point at D4; reopening it needs the three arguments answered, not just tidiness |

## Security considerations

- `install.sh` is fetched over TLS and piped to a shell; it downloads a release
  archive and verifies `checksums.txt`. D4 keeps that path at one fetch and one trust
  hop. Step 7's byte-identity check is the guard — an installer edited "while we were
  in there" is exactly the change that should not ride along in a layout refactor.
- The release workflow's `permissions` block is unchanged by this phase; confirm that
  in the diff rather than assuming it.
- Post-edit corruption check, as every phase: `grep -rn '\${OPENBOX_REDACTED_' .`
  clean outside `plans/` and `docs/`.

## Next steps

Phase 06 records the final layout, including the two deliberate root-level exceptions.
Phase 05 is independent and may run first.
