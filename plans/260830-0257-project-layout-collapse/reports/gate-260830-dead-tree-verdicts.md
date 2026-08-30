# Phase 03 step 0a — the three reachability verdicts

Written **before** anything is deleted, as the phase requires. Each verdict names
the file:line that settles it. Absence of a grep hit is not used as proof for any
of the three.

One fact applies to all three and was checked first: **`.goreleaser.yaml` declares
exactly one build** (`id: openbox`, `dir: cli`, `main: ./cmd/openbox`,
line 15-19). None of the three has ever been in a release artefact, so no
developer machine has one installed. That closes the "an existing install still
names it" concern uniformly.

---

## 1. `adapters/claude-code/cmd/openbox-cc-hook` — **DEAD, delete**

| check | result |
|---|---|
| what string does the installer write? | `localhooks.go:539-541` — `localHookCommand(engine, args)` returns `"<engine>" <args>`, built from the resolved openbox binary path. Never a literal. |
| what does the plugin manifest name? | `plugin/hooks/hooks.json` — every entry is `"${CLAUDE_PLUGIN_ROOT}/bin/openbox" hook claude-code <Event>` |
| released? | no — goreleaser builds only `./cmd/openbox` |
| testbed? | no reference |
| documented status | `adapters/claude-code/README.md:133-134`: *"The standalone `cmd/openbox-cc-hook` remains as a thin backward-compat alias over the same engine; **it is no longer referenced by the plugin**."* |

**Settling line: `adapters/claude-code/localhooks.go:539-541`** — the installed
command is constructed from the engine path, so no install path can produce this
name. **Verdict: delete** (5 declared tests).

`hookrun.go:400-411` documents a runtime degradation "if this ever runs under the
legacy openbox-cc-hook alias". That comment describes behaviour of the *engine*,
not a reference to the binary, and stays accurate after deletion.

---

## 2. `adapters/common/git/cmd/openbox-git-hook` — **REACHABLE. Do NOT delete.**

**This verdict was WRONG on the first pass and is corrected here.** The first pass
checked the install paths (all three set `Command` to the running executable), the
release config, and `testbed/`. All three were clean, and the conclusion "delete"
followed. It missed a fourth kind of invocation path: **a Go test harness that
builds the binary.**

```
adapters/common/git/integration_test.go:30
    build := exec.Command("go", "build", "-o", hookBin, "./cmd/openbox-git-hook")
    if err := build.Run(); err != nil { panic("build hook binary: " + err.Error()) }
```

This is in `TestMain`, it **panics** on failure, and `hookBin` is used by five
tests. Deleting the tree does not fail one test — it kills the whole package's
suite, **59 declared tests**, through a panic in `TestMain`. That is precisely the
invisible-tests failure mode this repo already documents twice.

**Settling line: `adapters/common/git/integration_test.go:30`.** **Verdict: KEEP.**

### Where it lands

The plan's rule for a surviving tree: *"a product path keeps it in `cmd/`, a test
or dev path moves it to `tools/`."* The **only** path that reaches this one is a
test harness — no installer, no doc, no release. So it goes to
**`tools/openbox-git-hook`**, alongside `corpusfixture` and `refusal-injector`,
which is also the honest statement about whether it ships: `tools/` is not a
release surface, and goreleaser never built it.

`integration_test.go:30` must then build it by **qualified package path** rather
than the relative `./cmd/openbox-git-hook` — after the collapse the test sits at a
different depth from `tools/`, and a relative path that has to count `../` hops is
the fragile form. Forced by the move, same class as the `schemaRelPath` edit.

The two fallback literals the first pass flagged for correction —
`adapters/common/git/hook.go:88,96` and `hookrun.go:76-78`, both defaulting to the
string `"openbox-git-hook"` — **need no change now**, because the binary they name
still exists. That correction is withdrawn along with the deletion.

### Why the first pass missed it, since the shape recurs

The check asked "does any *install or invocation* path produce this executable's
name?" and looked at installers, docs, the release config and the shell testbed. A
**Go test that shells out to `go build`** is an invocation path too, and it is
invisible to a search for the binary's name in install code because the name is
assembled by the test's own `filepath.Join`. The exhaustive form of the check is
one grep — every `exec.Command("go", "build", ...)` in the repo, with its target:

| builder | target | verdict |
|---|---|---|
| `cli/cmd/openbox/main_test.go` ×4 | `.` (itself) | n/a |
| `adapters/claude-code/cmd/openbox-cc-hook/main_test.go` ×5 | `.` (itself) | goes with the tree |
| `adapters/common/git/cmd/openbox-git-hook/main_test.go` | `.` (itself) | goes with the tree |
| **`adapters/common/git/integration_test.go:30`** | **`./cmd/openbox-git-hook`** | **external builder — KEEP** |

## 3. `actions/openbox-git-action/cmd/openbox-git-action` — **REACHABLE. Do NOT delete.**

D5's premise was *"`actions/openbox-git-action` has no `action.yml`"*. That is
true — there is no `action.yml` anywhere in the repo — and it proves only that
this is **not packaged as a GitHub Action**. It is a **CLI invoked from a CI
script**, and three independent paths reach it:

| path | evidence |
|---|---|
| the testbed **builds and runs it** | `testbed/50-lineage.sh:26` — `go build -o "$ACTION" "$TB_REPO/actions/openbox-git-action/cmd/openbox-git-action"`, described at line 7 as "the real openbox-git-action against the real core"; documented in `docs/testbed/e2e.md:294` |
| user-facing documentation | `docs/lineage.md:90` — `openbox-git-action --sha "$GITHUB_SHA" --repo "$GITHUB_REPOSITORY" --environment production`; same invocation at `actions/openbox-git-action/README.md:103` |
| it is on the wire | `client/testdata/golden/signal_deploy_lineage.json:20` and `contracts/dev-event/conformance/testdata/valid/deploy.json:8` both carry `"openbox-git-action"` as the emitted `tool_name` |

`CLAUDE.md:62` and `docs/architecture.md:77` both list it as a live component, and
CLAUDE.md's Current state records lineage as *shipped and verified end to end*.

**Verdict: KEEP.** This is the case the phase's gate language was written for — *"a
binary name assembled by concatenation, or held in a settings JSON string, will not
appear as a Go identifier"* — and the reason the check had to be a positive one.

### Where it lands (parent unresolved question 2)

The plan's rule: *"a product path keeps it in `cmd/`, a test or dev path moves it
to `tools/`."* Both kinds of path reach it — the testbed (test) and
`docs/lineage.md:90` (product). A documented deploy-time command that emits real
governance events is a product path, so it **stays in `cmd/openbox-git-action`**.

That also settles phase 01's **F3**: the 58-test library above it is not orphaned,
because its only importer survives. It moves to
`internal/actions/openbox-git-action` as the map's conditional row already says.

---

## Consequences for the plan

1. **D5 is wrong about two of its three.** Only `openbox-cc-hook` is dead. The
   `action.yml` argument did not establish what the git action was used for, and
   "no separate openbox-git-hook binary" in `main_test.go:764` describes the
   *trailer stamping path*, not the binary's existence.
2. **Parent acceptance criterion 3's subtraction is 5 tests**, enumerated:
   `TestHookBinary_{ObserveOnlyContract, MaintainsSessionRegistry,
   AmbientGitHookInstall, BlockVerdictRecordsAdvisoryExitsZero, NoArgsIsSafe}`.
3. **Final `/cmd` and `/tools` membership**, satisfying criterion 7:
   - `cmd/openbox` — the only binary GoReleaser builds
   - `cmd/openbox-git-action` — product path (`docs/lineage.md:90`)
   - `tools/openbox-git-hook` — test path only (`integration_test.go:30`)
   - `tools/corpusfixture`, `tools/refusal-injector` — D6
4. **Three scripts and one test carry paths the collapse moves**:
   `testbed/10-onboard.sh:33`, `testbed/50-lineage.sh:26`,
   `adapters/common/git/integration_test.go:30`. The two testbed scripts are
   dormant and have no CI net, so the sweep is the only thing that catches them.
5. **A verdict of "dead" is only as good as its enumeration of invocation kinds.**
   The first pass had four kinds and needed five. The fifth — a test harness that
   shells out to `go build` — is now in the table above.
