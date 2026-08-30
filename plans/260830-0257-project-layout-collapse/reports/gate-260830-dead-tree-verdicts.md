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

## 2. `adapters/common/git/cmd/openbox-git-hook` — **DEAD, delete, with one caveat**

| check | result |
|---|---|
| does any caller install a hook naming it? | all three production callers set `Command` explicitly to the running executable: `adapters/claude-code/hookrun.go:429`, `adapters/codex/hookrun.go:315`, `adapters/common/git/hookrun.go:78` (all `HookConfig{Command: self, …}`) |
| released? | no |
| testbed? | no reference |
| stated in-repo | `cli/cmd/openbox/main_test.go:764` — the trailer is stamped "with no separate openbox-git-hook binary"; `adapters/common/git/hook.go:84-86` — "OD17: single `openbox` engine ... without this package needing to change" |

**Settling lines: `adapters/claude-code/hookrun.go:429` and
`adapters/codex/hookrun.go:315`** — the two production install paths, both naming
`self`. **Verdict: delete** (4 declared tests).

**The caveat, which grep would not have found and which the phase's own gate
language predicted.** Two places name the binary as a **fallback default**, not as
a reference:

- `adapters/common/git/hook.go:88,96` — `HookConfig.Command` documents `"" =>
  "openbox-git-hook"` and `command()` returns that literal;
- `adapters/common/git/hookrun.go:76-78` — if `os.Executable()` fails,
  `self = "openbox-git-hook"`.

Neither fires on any production path, but after deletion both name an executable
that cannot exist. They are error-path degradations for a binary that was never
released, so they are harmless — but leaving them silently pointing at nothing is
the kind of false comment this repo removes on sight. **Phase 03 should update
both in commit (a), in the same change that deletes the tree.**

---

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

1. **D5 is wrong about one of its three.** It should read *two* dead `cmd/` trees,
   not three. The `action.yml` argument does not establish what it was used for.
2. **Parent acceptance criterion 3's subtraction is 9 tests, not 16 or 74**:
   `openbox-cc-hook` (5) + `openbox-git-hook` (4). Enumerated by name:
   `TestHookBinary_{ObserveOnlyContract, MaintainsSessionRegistry,
   AmbientGitHookInstall, BlockVerdictRecordsAdvisoryExitsZero, NoArgsIsSafe}`;
   `TestBinary_{AlwaysExitsZero, StampsMessageFile, NeverStampsSecret,
   InstallStampsCommit}`.
3. **Acceptance criterion 7** ("`cmd/` contains exactly the binaries GoReleaser
   builds, plus any of the three the gate proved live") is satisfied by
   `cmd/openbox` + `cmd/openbox-git-action`.
4. `testbed/50-lineage.sh:26` carries a path that phase 03 and phase 05 both move.
   It is a **dormant** script — no CI covers it — so the sweep is the only net.
