# ADR-0024 — One module, with root `/cmd`, `/internal` and `/tools`

Date: 2026-08-30
Status: Accepted
Supersedes: [ADR-0011](ADR-0011-multi-module-layout.md)
Amends: [ADR-0023](ADR-0023-credential-guard-scope.md) — the bound moves from the
module to the package subtree

## Context

Fifteen Go modules, none published — every inter-module requirement is `v0.0.0`
plus a relative `replace`, of which there were forty-five. A `go.work` existed so
CI and editors could see them at once.

ADR-0011 considered exactly this collapse in July and declined it, for three
stated reasons. The owner reversed that on 2026-08-30 and also adopted
`golang-standards/project-layout`'s directory vocabulary. This ADR records the
reversal and, more importantly, what each of ADR-0011's three reasons turned out
to be worth.

## Decision

One module at `github.com/openbox-ai/openbox-shift-left`. Root `/cmd`,
`/internal` and `/tools`; no `/pkg`. `go.work` and every `replace` directive
deleted. Four non-Go directories take the source's names: `testbed/`→`test/`,
`deploy/`→`deployments/`, the wire schema to `api/`, supervisor reference units
to `init/`, and the release config to `build/`.

**No `/pkg`, because nothing is published.** Every inter-module requirement was
`v0.0.0` with a relative `replace`, which is the definition of not being
consumable from outside. `/internal` is the compiler-enforced form and therefore
the honest one. The source's own README gives the same advice.

## What each of ADR-0011's three reasons was worth

The asymmetry is the point, and it is stated rather than averaged.

**R1 — "GoReleaser builds from `cli`, and the release path is the one thing with
no test coverage." INVERTED.** The `replace` graph the reason was about no longer
exists, and the divergence class it created is gone with it. That class was not
hypothetical: `cli` once imported `transport` with a `require` and no `replace`,
and every module built, vetted, tested under `-race` and cross-compiled clean
while `GOWORK=off` — the only mode the release build ran in — could not resolve
the import at all. The release artefact did not build and nothing said so. The
test written to catch that (`TestEveryIntraRepoImportIsWiredInGoMod`) is deleted
here, because with one module the property is not weakened, it is
**unrepresentable**.

**R2 — "The module boundary is doing real architectural work." LOST, and replaced
by a test.** `provider` and `devconfig` being separate modules is what made "no
adapter imports another, and the CLI reaches them only through the registry"
mechanical. Under `internal/adapters/` the two adapters are siblings and the
compiler permits the import. `internal/depguard`'s
`TestAdaptersDoNotImportEachOther` now carries it. **A test is weaker than the
compiler and that is the price of this decision.**

**R3 — "No whole-repo build, no CI." Already neutral.** It was paid by `go.work`
and `ci.yml`, not by the layout, and the collapse only changes who pays it. `./...`
from the root now covers everything, which let three CI steps be deleted as
vacuous — including the per-module `GOWORK=off` loop, whose work is now
byte-for-byte the Build step above it.

**ADR-0011's own revisit condition was met CONCURRENTLY, not beforehand.** It
reads: *"If the release path ever gains real coverage, collapsing becomes a cheap
follow-up."* That coverage arrived in the same change — a `goreleaser build
--snapshot` whose binary is executed, not merely built. This was an owner decision
of 2026-08-30, not a technical discharge of the precondition, and saying otherwise
would misrepresent how the decision was made.

## What this costs

Beyond R2, four things, each of which someone will otherwise reverse as tidying.

**Any package may now import any other.** Root `/internal` is visible to the whole
module, so the only remaining constraints are the *nested* `internal/` directories
and `internal/depguard`'s tests. `internal/gateway/internal/dialhook` keeps its
nesting deliberately: under root-`/internal` it still restricts importers to the
`internal/gateway` subtree, which is the exact property it exists to have.

**`cli/internal/*` flattened to `internal/cli/*`, and that is a real widening.**
Fifteen packages that only the `cli` module could import are now importable by
everything. The flattening is **forced, not stylistic**: `cmd/openbox` imports
twelve of them and sits outside the subtree, so a preserved
`internal/cli/internal/*` would put them beyond the reach of the binary's own main
package. What compensates — **for the four guarded subtrees, and only those** — is the
repo-local axis of their allowlists, and the drill that proves it is
`internal/gateway` importing `internal/cli/devinit`, the package that reads
`~/.openbox/.env`. Every other package's reach into `internal/cli/*` is part of
this cost, not covered by it. Several such inversions happen to be blocked today
because `devinit` sits high in the import graph and the compiler would see a
cycle — **that is an accident of what `devinit` currently imports, not a
control**, and extracting `credfile.go` into a leaf package would dissolve it
silently.

**ADR-0023's bound changes shape.** See the amendment below.

**Nine modules lose their `go.mod` as a review surface.** `cli`, `client`,
`provider`, `devconfig`, `hookflow`, `git`, both adapters and `refusal-injector`
never had a dependency guard; what bounded them was that adding a dependency meant
editing their own small `go.mod`. Under one module, anything already in the union
graph — `viper`, `afero`, `zerolog`, the whole gitleaks tree — becomes importable
from `client` or `provider` **with no diff outside a `.go` file**. The same
dissolution took the intra-repo lattice for those nine — what each could import
from the rest of this repo was pinned by its own `require` list and is now pinned
by nothing. This is
accepted as a named loss (owner decision, 2026-08-30) rather than answered with a
new root-level allowlist. The cheapest future answer, if it is ever wanted, is
~19 entries with their D-OSS citations plus a CI `go mod tidy -diff`.

## The ADR-0023 amendment

ADR-0023's scope sentence — *"transitive code is bounded at the module that took
the dependency"* — has no referent with one module. The bound is now the **package
subtree**, and the guards live in `internal/depguard`:

- **What it sees that `go.mod` did not.** The imports a subtree actually takes. A
  dependency required but unused no longer passes, and a test-only import is
  visible to the same list.
- **What it no longer sees.** An `// indirect` requirement is not an import, so
  there is nothing to find. `internal/conformance` is the exception and keeps a
  package **closure** check (`go list -deps -test`), because `golang.org/x/text`
  arrives through `jsonschema` and a direct-import list would drop the entry while
  calling itself equivalent.
- **The transitive hole ADR-0023 already accepted is unchanged in kind and wider
  in reach**, because any package may now import any other.
- **Membership is entry-or-subpackage, with a slash boundary.** Allowlists name
  module paths; imports name package paths. `internal/telemetry` allows
  `collector/pdata` and imports `pdata/plog`; `internal/decision` allows
  `gitleaks/v8` and imports three of its packages. Under equality every one fails
  on the first run, and the obvious repair — rewriting entries to package paths —
  grows the lists, which is what ADR-0023 forbids. The slash matters:
  `internal/gateway` must not admit `internal/gatewayfoo`.
- **Both directions are enforced**: nothing unreviewed enters, and no allowlist
  entry is stale.
- **`gateway`'s go.mod cross-check is DISSOLVED, not lost**, and the verdict is
  conditional. It existed so the import list and `go.mod` could not drift apart;
  "what `internal/gateway` requires" is no longer a fact. That holds only because
  the import walk absorbs both things the cross-check uniquely covered —
  **test-file imports** (so the walk includes `_test.go`) and **repo-local
  requires** (so the walk classifies repo-local rather than dropping it). Take
  both and its information content is a strict subset. Drop either and this is a
  loss, not a dissolution.

## Two root-level exceptions are deliberate

`install.sh` stays at the repository root, and `.github/workflows/` stays where
GitHub demands. The source blesses exactly this — *"don't worry if it's not and if
keeping those files in the root directory makes your life easier"* — and
`curl … | bash` needs a root URL the same way GitHub needs its own path. Under
`curl | bash` the script arrives on stdin, so `$0` is not a path and no
`dirname`-based shim can work; the only functioning one re-fetches over the
network, adding a second trust hop to a script whose job is verifying release
checksums. **Without this paragraph the next reader completes the pattern and
breaks the install URL.**

`install.sh` does not **move**. It was **corrected** — the collapse broke its
from-source build path — and those are different things.

## `cmd/` now matches OD17, and `init/` has one authority

OD17 has always claimed one static binary that is CLI, hook, sidecar and git
hook. The layout used to contradict it with four `cmd/` trees. A reachability
gate settled each on positive evidence rather than absence:

| tree | verdict | what settled it |
|---|---|---|
| `cmd/openbox` | ships | the only target GoReleaser builds |
| `cmd/openbox-git-action` | **product path** | `test/50-lineage.sh` builds and runs it; `docs/lineage.md` documents the deploy invocation; its tool name is in two committed wire fixtures |
| `tools/openbox-git-hook` | **test path only** | `integration_test.go`'s `TestMain` builds it and panics on failure — deleting it would have taken 59 tests with it |
| `adapters/claude-code/cmd/openbox-cc-hook` | **deleted** | the installers and plugin manifest all name the engine; nothing outside its own tests invoked it |

`tools/` is not a release surface, which is also the honest statement about
whether those binaries ship.

`init/` is **documentation-only**. `internal/cli/laneservice` remains the single
authority for unit bodies, which are rendered at install time with the machine's
own binary path, `HOME`, address and a stop timeout matched to its shutdown grace.
The reasoning is the lane-election precedent, not aesthetics: a second store of
derivable state drifts silently, and here it drifts in two ways that never surface
as an error — a plist missing `StandardErrorPath` logs nowhere, and a mismatched
stop timeout gets the daemon SIGKILLed mid-drain on every restart.

## Consequences

- `./...` from the root is the whole repo. No workspace, no `replace`, no
  `GOWORK=off` divergence.
- Layering is enforced by tests, not by the compiler, everywhere except the nested
  `internal/` directories.
- Adding an external dependency edits one `go.mod`, and only the four guarded
  subtrees will notice.
- **`go mod tidy` has not been run on the merged module.** The environment the
  collapse ran in denies module-cache writes and breaks the checksum database, so
  `go.mod` was authored from the union of the fifteen at the highest version any
  pinned, and `go.sum` from the union of the twelve committed, already-verified
  files. `go build ./...` needs no additions to either. Tidy is owed once with
  network access, and the committed `go.mod` may carry indirect requires a tidied
  one would drop.

## Revisit

If any package here is ever published for external import, `/pkg` and the module
question both reopen — that is the condition this decision rests on, and it is
the same one ADR-0011 rested on from the other side.
