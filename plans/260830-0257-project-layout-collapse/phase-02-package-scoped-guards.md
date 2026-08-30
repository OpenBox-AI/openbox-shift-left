# Phase 02 — Rebuild the six guards, package-scoped

## Context links

- Parent: [plan.md](plan.md) · Depends on: [phase 01](phase-01-baseline-and-rewrite-instrument.md)
- Governing: that decision,
That decision reason 2

## Overview

- **Date:** 2026-08-30
- **Description:** Convert every control that depends on the module boundary into one
  that depends on a package subtree, and prove each conversion equivalent **while the
  module boundary still exists**.
- **Priority:** P1 — this is the phase that decides whether the collapse is safe.
- **Implementation status:** COMPLETE (2026-08-30) · **Review status:** self-verified; report at `reports/guards-260830.md`
- **Mutates Go code:** yes (tests only; no production code)

## Key insights

- **Five of six guards read `go.mod`.** `decision/guard_test.go`,
  `telemetry/guard_test.go`, `transport/guard_test.go`,
  `gateway/guardscope_test.go` and `contracts/dev-event/conformance/deps_test.go`
  all do `os.ReadFile("go.mod")` and assert a short allowlist. After the collapse
  that file names all 19 external modules. Widening the allowlists to make them pass
  is the failure mode — it looks like a fix and removes the control.
- **The replacement must stay at *direct* granularity.** That decision deliberately
narrowed from transitive to direct because enumerating the closure "would make the
allowlist unreadable — the one thing it exists to be." A `go list -deps` transitive
version reintroduces exactly what that decision record rejected. The correct analogue
of "direct requires of a module" is **the non-stdlib, non-repo import paths appearing
in the files of a subtree**. `gateway/guard_test.go` already walks ASTs; extend that
machinery rather than inventing a second one.
- **`gateway`'s credential guard survives almost unchanged** — it is an AST walk over
  a directory, and a directory is exactly what it will still have. Only its *scope
  expression* changes: module-path prefix → path prefix. `TestSelfModuleExemptionIsNarrow`
  keeps its `gatewayfoo` lookalike case, now as `internal/gatewayfoo`.
- **One rule has no `/internal` expression at all.** "No adapter imports another
adapter" was free while they were separate modules; under `internal/adapters/`
both are siblings and the compiler permits the import. It becomes a test or it
becomes nothing. This is the honest downgrade that decision reason 2 predicted.

## Requirements

1. Six replacements, each scoped to a directory subtree, each with the same allowlist
   contents as today (no entry added).
2. Each replacement runs green **now**, against the current tree, alongside the
   original. Same commit. That co-residency is the equivalence evidence.
3. Each replacement has a mutation drill that is **run**, not asserted.
4. `layering_test.go` gains the adapter-to-adapter prohibition.

## Architecture

One shared helper, six thin callers — the repo's own stated lesson about six copies.

> **AMENDED 2026-08-30, after phase 01's measurement and advisory review.** The
> original spec below dropped repo-local imports. That deletes the repo-local
> **half of four of the six allowlists** — `decision` allows `…/client`,
> `transport` allows `…/gateway`, and `gateway`'s list is `{…/client,
> …/decision}` with **no external entry at all**. `telemetry`'s repo-local set is
> deliberately *empty*, and that emptiness is the quarantine.
>
> Composed with the forced `cli/internal/*` → `internal/cli/*` flattening, a
> repo-local-blind walker is a **live credential regression**: `internal/gateway`
> could import `internal/cli/devinit`, which reads and writes `~/.openbox/.env`
> (`credfile.go:65`), and every rebuilt guard would stay green. That is the worst
> outcome available in this plan and the phase as originally written shipped it.

```
test helper: subtreeImports(root, selfPrefix) -> {external, repoLocal}
  walk root; parse every .go INCLUDING _test.go, with NO build-constraint
    evaluation (six _GOOS.go files ship; a constraint-filtering walk is blind to
    the other platform)
  classify each import: stdlib (no dot in first segment) | repo-local (repoPrefix)
    | external; drop stdlib, drop self-subtree, return the other two, sorted
  membership is ENTRY-OR-ENTRY+"/" prefix, not equality
```

**Prefix matching is what makes byte-identity achievable, and without it step 2
fails on its first run.** Allowlists name **module** paths; imports name
**package** paths. `telemetry` allows `go.opentelemetry.io/collector/pdata` and
imports `pdata/plog`, `pdata/ptrace`, `pdata/pmetric`, `pdata/pcommon`;
`decision` allows `gitleaks/v8` and imports `gitleaks/v8/{config,detect,report}`.
Under equality every one of those goes red, and the obvious "fix" — rewriting
entries to package paths — grows both lists. Prefix matching is also exactly what
a `require` already meant. The slash boundary is required: `…/gateway` must not
admit `…/gatewayfoo`.

**Where the six guards live: one package, not six.** Pre-collapse the guards sit
in six different *modules*, so a shared helper would need a `require` + `replace`
+ **an allowlist entry in each of the six** — the widening this phase exists to
prevent. So the replacements are written as one test-only package that walks
subtrees **by filesystem path**. Two in-repo precedents do exactly this already:
`cli/cmd/openbox/modulewiring_test.go` and
`cli/internal/corpusfixture/committed_test.go` both walk the whole repo from
inside one package. It also means the pre- and post-collapse forms differ only in
their path literals, which is what makes phase 03 mechanical here too.

| Guard today | Scope after |
|---|---|
| `decision/guard_test.go` go.mod allowlist | `internal/decision` |
| `telemetry/guard_test.go` | `internal/telemetry` |
| `transport/guard_test.go` | `internal/transport` |
| `gateway/guardscope_test.go` go.mod half | `internal/gateway` |
| `conformance/deps_test.go` (jsonschema + x/text) | `internal/conformance` |
| `gateway/guard_test.go` AST credential guard | `internal/gateway` (prefix change only) |
| `cli/internal/providers/layering_test.go` | + adapter↔adapter rule |

**Why this is stronger, and where it is weaker.** Stronger: it sees the imports a
package actually takes, so a dependency required but unused no longer passes, and a
test-only import is now visible to the same list. Weaker: a subtree can reach an
external module *through* a repo-local package without naming it — the transitive
hole that decision already accepts and already documents. Do not widen the
allowlist to close it; that trade is already decided.

## Related code files

`decision/guard_test.go` · `telemetry/guard_test.go` · `transport/guard_test.go` ·
`gateway/guard_test.go` · `gateway/guardscope_test.go` ·
`contracts/dev-event/conformance/deps_test.go` ·
`cli/internal/providers/layering_test.go` · `cli/internal/providers/providers.go`

## Implementation steps

1. Write the shared walker with its own unit tests (fixture trees, aliased imports,
   build-tagged files, `_test.go` inclusion).
2. For each of the five go.mod guards: add a subtree-scoped test **beside** the
   existing one, seeded with today's allowlist verbatim. Both green.
3. Rescope `gateway/guard_test.go` and `guardscope_test.go` from module path to path
   prefix; keep every existing case including the lookalike.
4. Extend `layering_test.go`: assert no file under `adapters/claude-code` imports
   `adapters/codex` or vice versa, and that only the registry imports either. Also
   turn its `t.Skipf` on resolution failure into a failure — a guard that skips is
   the blindness this repo has already been bitten by, and post-collapse resolution
   is simpler, not harder.
4b. **Delete `transport/guard_test.go`'s `forbiddenCalls` map.** It is declared and
   used by nothing — a copy vestige of `telemetry`'s test — and its comment ("no
   reason to read the environment or the filesystem") is **false**: `ca.go:117,121`
   read the CA pair and `proxy.go:367` reads the environment in the self-loop clear.
   A wired version would fail on legitimate reads, which is presumably why it was
   never called. Commit `d534c49` is this repo's precedent for exactly this.
4c. `conformance`: closure check via `go list -deps -test`, mapped to
   `.Module.Path`, external set must equal `{jsonschema/v6, x/text}`. It **fails
   hard, never skips**: if the module cache were unpopulated the test binary could
   not have compiled. Plus a standing one-line root test that no `go.mod` carries a
   `replace`, which is the half of that guard outliving the phase.
5. Run the drills, per guard, recording each result. **The original matrix's
   third row was wrong** — "add a repo-local import → GREEN" contradicts
   `decision/guard_test.go`'s own seeded case, where an unlisted repo-local sibling
   is REJECTED. Corrected matrix:
   - delete the guard → RED;
   - add an unreviewed **external** import to the subtree → RED;
   - add an **unlisted repo-local** import to the subtree → RED (this is the
     credential-regression control: `internal/gateway` importing
     `internal/cli/devinit` must be RED **before** phase 03 starts);
   - add a **listed** entry's subpackage (e.g. `client/memhttptest` under a
     `…/client` entry) → GREEN (proves prefix matching does not over-fire);
   - add an unreviewed import in a **`_test.go`** file → RED (the gateway
     cross-check dissolution depends on this one);
   - a self-subtree import → GREEN.
   Budget ~30 drills, not 18.
6. Record any drill that comes back GREEN in the phase report with its cause. A green
   drill is the phase-03 stop condition.

## Todo list

- [x] shared walker + 11 unit tests, 5 mutation drills red — `cli/internal/depguard/`
- [x] 5 subtree allowlists added, co-resident with the originals, both green; allowlists carry the repo-local axis as well as the external one
- [x] gateway's import confinement replaced subtree-scoped incl. `_test.go`; the credential AST scan and `guardscope_test.go` are untouched here — phase 03 rescopes the first and deletes the second
- [x] adapter↔adapter rule added (in `depguard`, reusing the walker rather than inventing a second one); `layering_test.go`'s `t.Skipf` turned into a failure
- [x] 18 drills run, 18 correct; two harness defects found and fixed first (a `printf '%s'` that made every RED a parse error, and a neutralisation aimed at a helper with its own unit test)
- [x] no allowlist gained or lost an entry
- [x] **out of scope, taken anyway:** `ci.yml` gains `-count=1` — measured, these guards and four pre-existing ones serve a stale `ok` from the Go test cache
- [x] `transport`'s dead `forbiddenCalls` map and its false comment deleted

## Success criteria

1. Old and new guards both green on the current tree, same commit.
2. Every drill red where it must be red; every green drill explained in the report.
3. Every rebuilt allowlist is today's list with **only the phase-01 path map
   applied** — external entries byte-identical, repo-local entries and self-prefixes
   rewritten by the same map phase 03 applies to every other import string — matched
   by **module-prefix** (an entry admits its own subpackages, which is what a
   `require` already meant). No entry added, none dropped. Deviations are
   **enumerated here, not discovered later**:
   - `gateway`'s go.mod cross-check is **deleted as dissolved**, conditional on the
     import walk absorbing both things it uniquely covered — test-file imports
     (so the walk must include `_test.go`) and repo-local requires (so the walk must
     classify repo-local). Take both and its information content is a strict subset.
   - `gateway/guardscope_test.go` goes with it; its three discriminating properties
     are ported as walker fixtures.
   - `conformance` keeps `{jsonschema/v6, x/text}` via a package-**closure** check.
   Any other diff stops the phase.
4. `layering_test.go` fails if `adapters/codex` imports `adapters/claude-code`.

## Risk assessment

| Risk | Mitigation | Signal it broke | Pre-decided response |
|---|---|---|---|
| Replacement passes for a reason other than the one it names | run all three drill cases per guard, incl. the negative | the "unreviewed import" drill is green | **stop; do not begin phase 03** |
| Walker misses build-tagged files, so a `_windows.go` import is invisible | parse with no build-constraint filtering; fixture-test a `//go:build windows` file | drill on a tagged file is green | fix the walker; the six `_GOOS.go` files make this concrete, not hypothetical |
| Allowlist quietly widened to make a test pass | criterion 3 diffs allowlist contents | any new entry | revert; an added entry needs that decision amendment first, not a commit |
| Adapter↔adapter rule written so loosely it never fires | drill: make codex import claude-code | drill green | rewrite before proceeding |

## Security considerations

- These *are* the security controls. The credential guard and the
  conformance dependency floor are the two mechanisms bounding what code can reach a
  credential or enter three adapters' test graphs. Losing one silently is the worst
  outcome available in this whole plan, which is why the phase precedes the collapse
  rather than following it.
- The gateway AST guard's `forbiddenLiterals` list includes `.openbox` and
  `OPENBOX_AGENT_PRIVATE_KEY`; rescoping must not narrow the literal set.
- Post-bulk-edit check as phase 01: `grep -rn '\${OPENBOX_REDACTED_'` clean.

## Next steps

Phase 03 removes the module boundary. It deletes the five original go.mod-reading
guards, whose replacements are already carrying the load by then.

## Deliberately NOT taken in this phase

**A root-level direct-require allowlist** covering the collapsed `go.mod`'s ~19
external modules. The dissolution it would answer is real: nine modules
(`cli`, `client`, `provider`, `devconfig`, `hookflow`, `git`, both adapters,
`refusal-injector`) have **no guard today** and lose their own small `go.mod` as a
review surface, so any module already in the union graph — `viper`, `afero`,
`zerolog`, the whole gitleaks tree — becomes importable from `client` or `provider`
with no diff outside a `.go` file.

It is declined here on `--yagni`: it is a **new** control, not a rebuild of one
that the module boundary carried, and this phase's scope is conversion. Recorded as
a **named loss** for that decision amendment (phase 06), where the owner can take
it — the cheapest form is ~19 entries with their D-OSS citations plus a CI `go mod
tidy -diff` step, which phase 07 is already opening CI for.

## Unresolved questions

1. Take the root direct-require allowlist, or accept the nine-module dissolution as
a recorded loss? Deferred to the owner; that decision's amendment must say
which.
2. `conformance`'s closure check runs `go list` as a subprocess from a test. If the
   owner refuses any subprocess in a guard, the fallback is a direct-import list of
   `{jsonschema}` alone, `x/text` dropped with a comment and the retired closure pin
   recorded as a named loss. Implemented as the closure until told otherwise.
