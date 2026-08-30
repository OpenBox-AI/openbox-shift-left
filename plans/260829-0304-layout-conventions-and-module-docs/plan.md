--- title: "Layout conventions written down, module docs made true, drift made detectable" description: "Port the one portable idea from golang-standards/project-layout — every directory states what belongs in it — into a 14-module workspace, without touching the module topology that decision protects.
Closes two undocumented modules, one stale count, and the absence of any written naming convention." status: superseded progress: "0 of 6 phases" updated: 2026-08-30 priority: P3 effort: ~8h (6 phases; phase 06 droppable) branch: feat/tool-content-capture tags: [docs, layout, conventions, naming, ci,
editorconfig, no-code-change] created: 2026-08-29 ---

# Layout conventions written down, module docs made true, drift made detectable

> **Superseded 2026-08-30 by
> [260830-0257-project-layout-collapse](../260830-0257-project-layout-collapse/plan.md).**
> Owner decision reversed this plan's central refusal: the module topology *is* being
> collapsed and the source's directory vocabulary *is* being adopted. The measurement
> below — the naming survey, the `.editorconfig` evidence, the three filename
> deviations — remains valid and is carried into that plan's phase 07. The "What this
> plan is not" section is kept as the record of what was decided against, and why the
> reversal is a decision rather than an oversight.


Source: `golang-standards/project-layout` @ `a9d6fae` (master, 2026-04-29).
Analysis, dependency matrix, challenge and decision matrix:
[xia-260829-0304](../reports/xia-260829-0304-project-layout-conventions.md).

**What this plan is not.** It does **not** adopt the source's directory vocabulary
and it does **not** touch the module topology. The source assumes a single module
with root-level `/cmd`, `/internal`, `/pkg`; this repo is 14 modules by that
decision, whose three reasons for rejecting a collapse all still hold and whose
stated revisit condition — the release path gaining real coverage — is unmet.
Collapsing is surfaced in the report as an owner decision and deliberately left
untaken. Every applicable source directory already exists here under a local name;
renaming `testbed/`→`test/` or `deploy/`→`deployments/` would break every doc and CI
reference for no behavioural gain. The source's own README disclaims wholesale
adoption ("**`NOT an official standard defined by the core Go dev team`**"; "keep
what you need and delete everything else").

**What this plan is.** The source's one portable idea — *every directory states what
belongs in it* — applied to the three places this repo currently fails it, plus the
check that stops it failing again.

No Go code changes except three compile-safe file renames in the final, droppable
phase. No new dependency, so no allowlist widening and no `go mod tidy` cascade.

## Phases

| # | Phase | Effort | Depends on |
|---|---|---|---|
| 01 | Make the two layout maps true | ~1h | — |
| 02 | Make layout drift mechanically detectable | ~2h | 01 |
| 03 | READMEs for the 8 modules that have none | ~3h | 01 |
| 04 | Write down the naming convention | ~1h | — |
| 05 | `.editorconfig`, adapted to measured reality | ~0.5h | — |
| 06 | Fix the 3 filename deviations *(droppable)* | ~0.5h | 04 |

---

## Phase 01 — Make the two layout maps true

**Why.** `telemetry/` and `transport/` appear in neither `CLAUDE.md`'s "Where things
live" table nor `docs/architecture.md` §Modules. Two of fourteen modules are
invisible in both places a reader would look, and they are that decision lanes — the
newest code in the repo.

**Files.**
- `docs/architecture.md` §Modules (line ~66)
- `CLAUDE.md` "Where things live" table, and line 800

**Steps.**
1. Add rows to `docs/architecture.md` §Modules for `gateway/`, `telemetry/`,
`transport/`. Match the existing one-line "what it owns" style; cite the governing
decision record (0021 for gateway, 0022 for telemetry and transport).
2. Add `telemetry/` and `transport/` to the `CLAUDE.md` "Where things live" table.
3. Correct `CLAUDE.md:800` — "all twelve modules" → **fourteen**.

**Do not touch.** The dated status lines `all 11 modules green` at CLAUDE.md :216,
:325, :402, :452, :487, :520, :572; that decision's "eleven Go modules" Context
line; `ci.yml:7`. These are stateful records, correct as of their date — this
repo's docs rule says a completed phase does not make a record wrong.

**Validation.** Every `use ./x` in `go.work` has a row in `docs/architecture.md`
§Modules. Phase 02 makes this mechanical.

**Rollback.** Revert the doc edits; nothing depends on them.

---

## Phase 02 — Make layout drift mechanically detectable

**Why.** A convention with no check rots. This repo has proved it three ways at once
(two undocumented modules, one stale count, three filename deviations — none catchable
by any existing test or CI step). `ci.yml` already carries the exact precedent: a step
that fails when a `go.mod` on disk is missing from `go.work`. Extend the same idea one
hop, from *registered* to *documented*.

**Files.** `.github/workflows/ci.yml`

**Steps.**
1. Next to the existing "Check go.work covers every module" step, add "Check every
   module is documented": for each `use ./x` in `go.work`, assert a row matching
   `` `x/` `` exists in `docs/architecture.md` §Modules.
2. Fail with the missing module named, and with the remedy — the same shape as the
   existing step's `::error::` message.
3. Keep it shell + grep. No new dependency.

**Design note.** Validate **one** canonical table, not two. `docs/architecture.md` is
user-facing and already cited from `CLAUDE.md`; making both authoritative recreates
the drift this phase exists to stop. Confirm the choice against unresolved question 1
in the report before implementing.

**Validation.** Delete a row from the table → CI fails naming that module. Restore →
CI passes. Run the deletion drill; do not assert it.

**Rollback.** Remove the CI step.

---

## Phase 03 — READMEs for the 8 modules that have none

**Why.** 6 of 14 modules have a `README.md`. The 8 without: `adapters/common/devconfig`,
`adapters/common/hookflow`, `decision`, `gateway`, `provider`, `telemetry`, `transport`,
`contracts/dev-event/conformance`. These include the engine every adapter runs on and
all three model-call lanes.

**Steps.** One short README per module, each answering three things and nothing else:

1. **What this module owns** — one or two sentences.
2. **What must NOT go in it** — the boundary that makes the module worth having.
   `hookflow`: anything provider-specific. `conformance`: any `require` or `replace`
   (a test already pins this). `provider`: any adapter import.
3. **that decision that governs it**, linked.

**Constraint.** A module README must not restate the `architecture.md` §Modules row.
Three copies of one table is the drift surface C4 rejects. If a module has nothing
module-specific to say beyond its table row, **do not add a README for it** — say so
in the phase report instead.

**Validation.** Each README's claims check out against the module's own code and
guard tests. Keep them short; this repo's docs rule is "keep it true, and keep it
short."

**Rollback.** Delete the added files.

---

## Phase 04 — Write down the naming convention

**Why.** Grep over every `*.md` in the repo returns no statement of a naming or layout
convention. One exists in practice, measured across ~100 Go files and every non-Go
asset, with exactly three deviations.

**Files.** `CLAUDE.md` → "Working conventions"; a short pointer from
`docs/architecture.md` if that reads better for users.

**The convention to record (measured, not invented).**

- **Go source: flat lowercase, no separators.** `approvalhold.go`, `outputcontract.go`,
  `turncursor.go`, `failurepolicy.go`, `sessionhalt.go`.
- **Underscore is reserved for build constraints**: `_test.go`, `_unix.go`,
  `_windows.go`. All six current `_GOOS.go` files also carry an explicit `//go:build`
  line, so the suffix is belt-and-braces — keep both.
- **Non-Go: kebab-case.** `dev-event.schema.json`, `managed-settings.json`,
  `00-preflight.sh`, `data-and-privacy.md`.
- **Plans:** `{YYMMDD-HHMM}-{slug}/`.
  **Testbed:** `NN-kebab.sh`, ordered by prefix.
- **Exception, load-bearing:** provider-mandated filenames are kept verbatim —
  `managed_config.toml`, `requirements.toml`. Codex reads those exact names; renaming
  them breaks the provider, not just the convention.

**Note a real conflict.** The user's global tooling guidance asserts "Go uses
snake_case" for filenames. This repo does not, and the Go standard library largely
does not either; the local convention is flat lowercase with underscore reserved for
build constraints. Record the repo's measured convention and note the divergence so
the next reader does not "fix" ~100 files toward the generic rule.

**Validation.** Spot-check the stated rule against 10 random Go files and 10 non-Go
files. If any rule fails the spot-check, the rule is wrong, not the files.

**Rollback.** Revert the doc edit.

---

## Phase 05 — `.editorconfig`, adapted to measured reality

**Why.** The source ships one; this repo has none. Adapt rather than transplant — the
source's `[*.md] indent_style = tab` is measurably wrong here (0 tabs across README,
architecture.md and CLAUDE.md).

**Files.** new `.editorconfig` at repo root.

**Measured settings.**

| Selector | Setting | Evidence |
|---|---|---|
| `[*]` | `charset=utf-8`, `end_of_line=lf`, `insert_final_newline=true`, `trim_trailing_whitespace=true` | no trailing whitespace found in any `.go` or `.md` today |
| `[{*.go,go.mod,go.sum}]` | `indent_style=tab` | gofmt |
| `[*.{yml,yaml,json}]` | `indent_style=space`, `indent_size=2` | `ci.yml` measured |
| `[*.md]` | `indent_style=space`, `trim_trailing_whitespace=false` | measured; **diverges from source** |

**Omit `*.sh` deliberately.** The repo is split — `testbed/*.sh` indent with tabs,
`install.sh` with spaces. A single rule would put half the shell scripts in violation
on first save. Record the split as a known gap rather than papering over it; unifying
shell indentation is its own change with its own diff.

**Out of scope.** `.gitattributes`. The source's `* -text` is a blanket normalization
opt-out, wrong for a repo that ships shell scripts and cross-compiles for Windows.
The useful form is `*.sh text eol=lf`, but that is line-ending policy rather than
layout — see unresolved question 3.

**Validation.** Open a `.go`, a `.md` and a `.yml` in an EditorConfig-aware editor and
confirm no reformat-on-save diff against the committed file.

**Rollback.** Delete the file.

---

## Phase 06 — Fix the 3 filename deviations *(droppable)*

**Why.** Makes the Phase 04 convention true rather than aspirational. Purely cosmetic;
the only cost is `git blame` continuity on three files. **Drop this phase if that cost
is not worth it** — see unresolved question 2.

**Renames.**

| From | To | Note |
|---|---|---|
| `adapters/common/hookflow/consts_paths.go` | `paths.go` | no collision — `devconfig/paths.go` is a different module and package |
| `adapters/claude-code/enforce_evaluate.go` | `enforceevaluate.go` | |
| `adapters/codex/enforce_evaluate.go` | `enforceevaluate.go` | keep the cross-adapter name parallelism |

**Guardrail.** Never rename a `_test.go`, `_unix.go`, `_windows.go`, or `_GOARCH.go`
suffix. Those are build constraints. (All six here also carry explicit `//go:build`
lines, so a rename would not silently drop the constraint — but the suffix is the Go
convention and tooling readers expect it.)

**Use `git mv`** so rename detection survives.

**Validation.** `go build` and `go vet` green per module; full `-race` test run green;
both cross-compiles green. Go ignores filenames outside the constraint suffixes, so
this is expected to be a no-op to the compiler — confirm it, do not assume it.

**Rollback.** `git mv` back.

---

## Acceptance criteria

1. Every `use ./x` in `go.work` has a row in `docs/architecture.md` §Modules, and CI
   fails if one is removed *(drill run, not asserted)*.
2. `telemetry/` and `transport/` are documented in both layout maps.
3. No live claim of the module count is wrong; no dated status record was rewritten.
4. Every module either has a README or is recorded in the phase report as having
   nothing module-specific to add.
5. The naming convention is written down, matches a 20-file spot-check, and names the
   provider-mandated exception.
6. `.editorconfig` produces no reformat diff on existing files.
7. All 14 modules green under `-race`; both cross-compiles green *(phase 06 only)*.

## Out of scope, with reasons

- **Collapsing the module topology** — that decision, revisit condition unmet. Owner
  decision, surfaced in the report.
- **Directory renames** to the source's vocabulary — churn across every doc and CI
  reference, zero behavioural gain; the source itself accepts `/deploy`.
- **Adding `/pkg`** — the source calls it contested; modules already serve the role.
- **`internal/` inside the other 13 modules** — the enforcement it would buy is
  already held by `guard_test.go`, `deps_test.go` and
  `TestOnlyTheRegistryImportsAdapters`.
- **`.gitattributes`** — line-ending policy, not layout.

## Unresolved questions

1. Canonical table for the Phase 02 CI check — `docs/architecture.md` §Modules (plan's
   assumption) or `CLAUDE.md`?
2. Keep or drop Phase 06 (3 renames vs. `git blame` continuity)?
3. `.gitattributes` with `*.sh text eol=lf` — separate change, or fold into Phase 05?
