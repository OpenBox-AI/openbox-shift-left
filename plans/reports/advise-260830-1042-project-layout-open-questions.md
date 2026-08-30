# Advise — resolving the open questions in the project-layout collapse

Date: 2026-08-30 · Plan: [260830-0257-project-layout-collapse](../260830-0257-project-layout-collapse/plan.md)
Source re-analysed: `golang-standards/project-layout` @ `/tmp/project-layout` — all 19
per-directory READMEs, `.editorconfig`, `.gitattributes`, `Makefile`.

## Reframed problem

Four questions were left open because the top-level README alone did not settle them.
The per-directory READMEs do settle three; the fourth was an owner call. Two of the
answers cut against decisions already taken (D3 in part, and the plan's own `/cmd`
target tree), so this is a correction pass, not a fill-in-the-blanks pass.

**Requirements.** (1) Every answer traced to a source sentence or a local invariant,
not preference. (2) No re-litigating D1 (collapse) or D2 (`/internal` only).
(3) Advisory only — the plan is amended separately.
**Non-goals.** `/internal/app` + `/internal/pkg` substructure; implementation.

## Verdict

The plan's target tree was wrong in two places and its four open questions were
mostly answerable from evidence that was sitting in the source repo unread. Nothing
structural is at risk — D1 and D2 stand — but the `/cmd` list was overpopulated
(dev instruments belong in `/tools`, which the plan never mentioned) and the
`scripts/` move was a net negative the source itself argues against. The `init/`
question was the only one where the source's literal reading and this repo's own
engineering precedent disagree; precedent wins, and the reasoning is already written
down in this repo from the election defect in phase 12.

Two of the five answers below were owner calls and are recorded as such.

## Answers

| # | Question | Answer | Basis |
|---|---|---|---|
| 1 | contract docs → `api/` or `docs/`? | schema.json → `api/`; MAPPING.md, COVERAGE.md, contracts README → `docs/` | `/api` README: "OpenAPI/Swagger specs, JSON schema files, protocol definition files" — machine-readable only. `/docs`: "Design and user documents" |
| 2 | `init/` — `go:embed` or docs-only? | **docs-only**: reference copies + README pointing at `laneservice` | `/init` = "System init … configs" describes static files; ours are rendered per-install by `kardianos/service`. A file copy is a second store of derivable state |
| 3 | `refusal-injector` in the release archive? | **question dissolves** — it and `corpusfixture` go to `tools/`, not `cmd/` | `/tools` README: "Supporting tools for this project… can import code from `/pkg` and `/internal`". `tools/` is not a release surface |
| 4 | `install.sh` shim under `curl \| bash`? | **stays at root; no `scripts/` at all** *(owner call)* | `/build` README blesses root placement when a tool demands the path; `install.sh` is the only script outside testbed, so `scripts/` would be empty |
| 5 | *(new)* three dead `cmd/` trees | **verify, then delete, in phase 03** *(owner call)* | GoReleaser builds only `./cmd/openbox`; `main_test.go:764` states the trailer is stamped "with no separate openbox-git-hook binary"; `actions/openbox-git-action` has no `action.yml` |

### Why `init/` is docs-only despite the source's literal wording

The source's `/init` holds configs a packager drops on a machine. This repo's units
are **rendered at install time** with substituted binary path, `HOME`, args, and an
`ExitTimeOut` that must track `--shutdown-grace`. Extracting them to `init/*.plist`
and embedding creates a second copy that must stay in sync with what
`kardianos/service` renders — and this repo has already paid for exactly that shape
once: the phase-12 election was written as a stored field, tested green, and
**reverted**, because "a second store of derivable state … drift is silent in the
worst direction." Two silent failure modes are live here: a plist missing
`StandardErrorPath` logs nowhere, and an `ExitTimeOut` that stops matching gets the
daemon SIGKILLed mid-drain every restart. Neither surfaces as an error.
`TestSuppliedTemplatesSurviveRendering` exists precisely because the render is an
identity transform today; embedding puts a second authority beside it.

### `ext-core/` — a disposition, not a destination

`contracts/dev-event/ext-core/README.md` is a tombstone: "RETIRED 2026-07-15", whose
retirement is already recorded in ADR-0004. It is not a contract artifact and does
not belong in `api/`. **Recommend deleting it**, ADR-0004 being the surviving record —
a duplicated retirement note is the drift surface, not a safeguard. Flagged rather
than assumed; flipping this to "move to `docs/`" costs nothing.

## What you should do

1. Amend the plan's target tree: add `tools/`, drop `scripts/`, cut `cmd/` to the
   binaries that survive step 2.
2. Add a **verification gate** at the head of phase 03 covering the three suspect
   binaries — installer argv, `action.yml` absence, testbed and hook references —
   and record the evidence per binary before deleting anything.
3. Split phase 03 into two commits: *(a)* verify + delete, *(b)* `git mv` + import
   rewrite. You chose deletion inside phase 03; two commits recovers most of the
   reviewability that choice costs, at no schedule cost.
4. Keep `.goreleaser.yaml` → `build/` — that half of D3 is unaffected and still right.
5. Update phase 05 for the `api/` ⁄ `docs/` split and the docs-only `init/`; the
   `schemaRelPath` edit remains the one real code change in that phase.

## What you shouldn't do

- **Don't add `/internal/app` + `/internal/pkg`.** The source calls it optional and
  aimed at multiple apps sharing code. After step 2 this repo has one application.
  Pure ceremony, plus another 380-file path churn.
- **Don't create `scripts/` with one file in it**, and don't create it empty to "look
  right". The source's instruction is explicit: "keep what you need and delete
  everything else."
- **Don't adopt the source's `.gitattributes` (`* -text`)** or its
  `[*.md] indent_style = tab`. Both are measurably wrong here — already covered in
  phase 07, restated because re-reading the source repo invites re-importing them.
- **Don't delete a `cmd/` tree on grep evidence alone.** Grep does not see a binary
  name built by concatenation. The gate in step 2 is what makes the deletion safe.
- **Don't let the deletion and the move land in one commit.** You accepted the risk;
  don't compound it.

## What could be better / more efficient

Ranked by effort-to-impact:

1. **Cheapest, highest value:** the two-commit split in phase 03. Minutes of
   discipline, and it restores `git log --follow` on the moved files and lets a
   reviewer check "is this dead?" separately from "did the move preserve it?".
2. **Cheap:** if step 2 proves all three binaries dead, `cmd/` holds exactly one
   entry — which is what OD17 has claimed all along ("one static binary that is
   CLI + hook + sidecar + git-hook"). The tree then documents the decision instead
   of contradicting it. Worth calling out in ADR-0024.
3. **Moderate:** deleting three modules before the collapse removes them from the
   444-line import rewrite. You chose deletion inside phase 03 rather than before it,
   so this saving is partly available anyway — sequence the deletion first within the
   phase and the rewrite gets smaller for free.

## My take and how to get there

Take all five answers as written. The route:

1. Amend `plan.md` D3 (install.sh reversal) and the target tree (`tools/`, no
   `scripts/`).
2. Amend phase 03: verification gate → delete → move, two commits.
3. Amend phase 04: drop the installer move and the shim question; keep the
   `build/.goreleaser.yaml` move and the snapshot-build coverage, which is the part
   that discharges ADR-0011 reason 1.
4. Amend phase 05: `api/` takes the schema only; `docs/` takes the prose; `init/` is
   docs-only; add the `ext-core/` deletion.
5. Close all four plan-level unresolved questions and phase-04's; carry forward only
   the `ext-core/` disposition as a flagged judgment call.

## Benefits

- Every remaining layout decision now traces to a source sentence or a local
  invariant. No open question is left to be resolved mid-implementation, where the
  cheap answer wins by default.
- `cmd/` stops advertising three binaries the product does not ship.
- The install path keeps its published URL and gains no second trust hop on a script
  whose job is verifying release checksums.
- `init/` does not acquire a second authority over unit bodies whose failure modes
  are both silent.

## Trade-offs

- **Deleting inside phase 03 costs a pure-`git mv` commit.** I recommended a separate
  pre-phase and you chose otherwise; the two-commit split is the mitigation, not a
  cure. If the verification gate turns up ambiguity on any binary, the phase now
  carries a judgment call it was designed not to have.
- **No `scripts/` means one deviation from the source's tree.** Defensible by the
  source's own words, but a reader comparing directory-for-directory will notice.
  `docs/architecture.md` §Layout should say why, in one line.
- **Docs-only `init/` is a weaker claim to the directory** than the source intends.
  The alternative is worse for this codebase; that is a local judgment, not a general
  one.
- **Splitting MAPPING.md from the schema costs DX.** Someone reading the schema now
  goes to `docs/` for the field mapping. Accepted for vocabulary fidelity; revisit if
  the contract version bumps start touching both in lockstep.
- **When this recommendation stops holding:** if a package here is ever published for
  external import, `/pkg` and the D2 decision both reopen — and `api/` would then
  plausibly grow generated client code, at which point the docs-beside-schema
  argument gets stronger. Switching then costs a directory move and a doc sweep,
  not a redesign.

## Work checklist

- [ ] `plan.md`: amend D3 — `install.sh` stays at root, no `scripts/`; `.goreleaser.yaml` still → `build/`
- [ ] `plan.md`: add `tools/` to the target tree; remove `scripts/`; reduce `cmd/`
- [ ] `plan.md`: close unresolved questions 1–3; carry `ext-core/` forward as flagged
- [ ] phase 03: add the dead-binary verification gate (argv, `action.yml`, testbed, hook refs) with per-binary evidence recorded
- [ ] phase 03: split into commit (a) verify+delete, commit (b) move+rewrite
- [ ] phase 04: drop the installer move and its unresolved question; keep `build/` move + snapshot-build coverage
- [ ] phase 05: `api/` = schema only; prose → `docs/`; `init/` docs-only; add `ext-core/` deletion
- [ ] phase 06: ADR-0024 notes `cmd/` now matches OD17's one-binary claim
- [ ] phase 07: restate that the source's `.gitattributes` and `[*.md] tab` are rejected on measurement

## Success metrics

| Metric | Target | How verified |
|---|---|---|
| Open questions in `plan.md` | 1 (`ext-core/` only), down from 3 | `grep -c` under "Unresolved questions" |
| Open questions in phase 04 | 0, down from 1 | same |
| `scripts/` exists | no | `test ! -d scripts` |
| Documented install URL resolves | 200 | `curl -fsSLI .../main/install.sh` |
| `cmd/` entries | equals the number of binaries GoReleaser builds | `ls cmd \| wc -l` vs `.goreleaser.yaml` builds |
| Dead-binary deletions justified | one evidence line per deleted tree | phase-03 report |
| `api/` contents | machine-readable only, 0 `.md` files | `ls api/*.md` empty |
| `init/` Go code | none — no `go:embed` of unit bodies | `grep -r 'go:embed' init/` empty |
| Unit rendering unchanged | green | `TestSuppliedTemplatesSurviveRendering` |

## Unresolved questions

1. `ext-core/` tombstone — delete (ADR-0004 is the surviving record, recommended) or
   relocate to `docs/`? Low stakes either way.
2. If the phase-03 gate finds one of the three binaries **is** reachable, does it stay
   in `cmd/` or move to `tools/`? Depends on what reaches it; decide on the evidence.
