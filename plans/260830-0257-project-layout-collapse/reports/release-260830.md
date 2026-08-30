# Phase 04 — the release path

2026-08-30. This is where ADR-0011's strongest objection is discharged rather than
argued with.

## ADR-0011 reason 1, settled

The ADR's first reason not to collapse was that GoReleaser builds from `cli` with
its own `replace` graph and **the release path has no test coverage**. After the
collapse there is no `replace` graph and no `GOWORK=off` divergence, so the config
gets simpler — but "simpler" is not "verified". So it was run:

```
goreleaser build --snapshot --clean --single-target --config build/.goreleaser.yaml
  -> build succeeded, dist/openbox_darwin_arm64_v8.0/openbox

./openbox --version   -> openbox 0.2.1-snapshot     (exit 0)
./openbox doctor      -> full posture report        (exit 0)
```

**Executing it is the requirement; a successful build is not the same claim.** The
binary runs and both commands produce real output. That is the coverage the ADR
said did not exist.

## What changed

| change | why |
|---|---|
| `.goreleaser.yaml` → `build/.goreleaser.yaml` | D3 |
| `release.yml` gains `--config build/.goreleaser.yaml` | GoReleaser looks for the config at the repo root; `.github/workflows/` is the same kind of tool-mandated exception in the other direction |
| `GOWORK=off` env entry deleted, with its comment | the comment described a workspace, `dir: cli`, and `replace` directives — none of which exist. A stale env var is how a reader later concludes a workspace still exists |
| `dir: cli` deleted | phase 03, as a moved-path consumer |
| two CI workspace steps deleted | see below |
| **a third CI step deleted** | see below |
| CI cross-compiles `$MODS` → `./...` | `$MODS` was set by a step that no longer exists |
| the goreleaser output directory added to `.gitignore` | goreleaser writes artefacts to the repo root even with the config moved |

## Three vacuous CI steps, not two

The phase named two. A third was found by reading what survived:

- *Check go.work covers every module* — compares `find -name go.mod | wc -l`
  against `go list -m | wc -l`. With one module both are 1. **Vacuous.**
- *Resolve workspace modules* — existed only because "the repo root is not itself a
  module, so `./...` matches nothing". The root is a module now.
- ***Build every module WITHOUT the workspace*** — 38 lines looping
  `GOWORK=off go build ./...` per module. With one module it is **identical to the
  Build step above it**. Its comment cites `.goreleaser.yaml: env: [GOWORK=off],
  dir: cli` and a Go test, all three of which are now deleted.

That third one is the interesting deletion, because it is the gate whose absence
let a broken release ship green — and it is removed on the grounds that the
collapse eliminated the bug class it caught, not on the grounds that it is
inconvenient. **A vacuous guard is worse than none: it reads as coverage.** The
argument belongs in ADR-0024 (phase 06), not only in a commit message.

## Two CI comments were describing a world that no longer exists

Both corrected rather than left for a doc sweep:

- the file header said modules are discovered from `go.work`;
- the `-count=1` comment said the cache does not verify reads in a sibling
  workspace module. That was measured and true, and **the collapse closed that
  half** — a same-module read *is* tracked. What is left, and what now justifies
  the flag, is that a **child process's** reads never reach the test log, and
  `internal/depguard`'s conformance closure guard shells out to `go list`.

## Verified, not assumed

| check | result |
|---|---|
| snapshot builds from the new config path | PASS |
| **the binary executes** (`--version`, `doctor`) | PASS |
| archive name contract | `name_template: openbox_{{ .Version }}_{{ .Os }}_{{ .Arch }}` + `tar.gz` vs `install.sh:136` `openbox_${ver}_${OS}_${ARCH}.tar.gz` — **match**, unchanged by this phase |
| `git diff --stat install.sh` **in this phase** | empty |
| `test ! -d scripts` | PASS |
| `install.sh` still at the repo root | PASS |
| both workflow files parse as YAML | PASS |
| every surviving CI step resolves | gofmt, build, vet, gateway-units (`./internal/cli/gatewayservice/`), fuzz (`cd internal/decision`), testbed syntax — all PASS |
| the docs-flag extractor is not vacuous | 62 flags extracted from `cmd/openbox/*.go` |

## The criterion this phase had to amend

Its own success criterion 3 read *"`git diff --stat install.sh` is empty"*, and the
parent's criterion 6 said the install URL is *"unchanged by construction"*. Both
were true about the **URL** and false about the **script**: the collapse breaks its
from-source build (`$SRC/cli/go.mod` as the checkout marker, `cd "$SRC/cli"` to
build), and that path is taken by the plain clone-and-build case, not only
`OPENBOX_SRC`.

D4 decided `install.sh` does not **move**. It never decided it could not be
**corrected**. The two-line fix landed in phase 03's commit (b) beside the moved
testbed and CI paths it belongs with, and this phase's criterion now guards against
a **ride-along** edit rather than against any edit — which is what it was for.

## Unresolved

1. `goreleaser release` itself is still unrun — only `build --snapshot
   --single-target`. The full matrix (5 os/arch pairs, archives, checksums) has
   never been produced from this config.
2. ~~`go mod tidy` is still owed~~ — **run 2026-08-30; `go.mod` unchanged, only `go.sum` pruned.** See `collapse-260830.md`.
