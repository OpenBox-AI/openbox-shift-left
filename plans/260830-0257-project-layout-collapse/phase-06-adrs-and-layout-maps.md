# Phase 06 — ADRs and the layout maps

## Context links

- Parent: [plan.md](plan.md) · Depends on: [phase 04](phase-04-release-and-install-path.md),
  [phase 05](phase-05-non-go-directory-moves.md)
- Rewrites: [ADR-0011](../../docs/adr/ADR-0011-multi-module-layout.md),
  [ADR-0023](../../docs/adr/ADR-0023-credential-guard-scope.md)
- Touches: `CLAUDE.md` §Where things live, `docs/architecture.md` §Modules

## Overview

- **Date:** 2026-08-30
- **Description:** Record the decision and its costs where the next reader will look,
  and make every path reference in the repo true again.
- **Priority:** P1 — a governance product that overstates itself is the failure it
  exists to prevent.
- **Implementation status:** not started · **Review:** not reviewed

## Key insights

- **ADR-0011 must be superseded by a new ADR, not edited.** It is a stateful record
  of an accepted decision with three named reasons; rewriting it in place erases why
  the split was ever right and makes the reversal look like it had no cost. The new
  ADR takes each of the three reasons in turn and says what happened to it.
- **Reason 1 inverted; reason 2 was genuinely lost; reason 3 was already neutral.**
  That asymmetry is the honest summary and it belongs in the ADR verbatim:
  - *R1 — release path risk.* Inverted. The `replace`/`GOWORK=off` divergence class is
    gone, and phase 04 gave the release path the snapshot coverage R1 said it lacked.
  - *R2 — module boundary doing architectural work.* **Lost, replaced by a test.**
    `/internal` cannot stop one adapter importing another; `layering_test.go` now
    does. A test is weaker than the compiler and that is the price.
  - *R3 — no whole-repo build.* Already paid by `go.work`/`ci.yml`; the collapse just
    changes who pays it.
- **ADR-0011's own revisit condition was not met, and saying so is required.** It
  reads: "If the release path ever gains real coverage, collapsing becomes a cheap
  follow-up." Phase 04 supplies that coverage *as part of the same change*, not
  before it. The new ADR must state that the condition was satisfied concurrently
  rather than as a precondition — an owner decision (2026-08-30), not a technical
  discharge.
- **ADR-0023's scope sentence becomes false on the day phase 03 lands.** "Transitive
  code is bounded at the module that took the dependency" has no referent with one
  module. The amendment says the bound is now the package subtree, and — importantly —
  that the transitive hole it already accepted is *unchanged in kind but wider in
  reach*, because any package can now import any other.

## Requirements

1. New `ADR-0024-single-module-layout.md`, status Accepted, superseding ADR-0011.
2. ADR-0011 marked `Superseded by ADR-0024` — content otherwise untouched.
3. ADR-0023 amendment section: module → subtree, with the widened transitive note.
4. `CLAUDE.md` "Where things live" table rewritten for the new tree.
5. `docs/architecture.md` §Modules → §Layout, rewritten.
6. Repo-wide path-reference sweep.

## Architecture

**What must NOT be rewritten.** This repo's documentation rule is that plans, reports
and dated status records are stateful. Leave alone:

- every dated status line in `CLAUDE.md` matching `N modules green` — each was true on
  its date. Identify them by pattern at implementation time, not by the line numbers
  in this plan: `CLAUDE.md` is edited most days and any number written here is stale
  before the phase runs;
- ADR-0011's Context ("eleven Go modules") and its body;
- every `plans/**/reports/verification-*.md`;
- the superseded plan `260829-0304-*`, beyond a status/pointer line.

**What must be rewritten** is only live claims: the "Where things live" table, the
architecture §Modules section, `docs/getting-started.md` paths, the testbed doc, the
MDM recipe, and the module-count sentence at `CLAUDE.md:800`.

The ADR-0024 outline:

```
Context   15 modules, none published, 45 replace directives; the owner decision of
          2026-08-30 to adopt golang-standards/project-layout.
Decision  One module; root /cmd and /internal; no /pkg (nothing is published);
          the four directory renames.
Why now   R1 inverted (evidence: the transport replace bug), R3 already paid.
What it   R2 lost — compiler-enforced adapter isolation replaced by a test.
costs     ADR-0023's bound narrowed from module to subtree.
          Any package may now import any other; only tests constrain layering.
Revisit   If a package here is ever published for external import, /pkg and the
          module question both reopen.
```

**Three things ADR-0024 must record beyond the topology**, because each is a decision
someone will otherwise reverse as tidying:

- **`cmd/` now matches OD17.** If phase 03's gate proved all three suspect trees dead,
  the tree holds exactly one application — which is what OD17 has claimed all along
  ("one static, no-cgo binary that is CLI + hook + sidecar + git-hook"). The layout
  stopped contradicting the decision. Name the deleted trees and their verdicts.
- **Two root-level exceptions are deliberate:** `install.sh` (D4) and
  `.github/workflows/` (GitHub-mandated). The source blesses both — "don't worry if
  … keeping those files in the root directory makes your life easier". Without this
  line the next reader completes the pattern and breaks the install URL.
- **`init/` is documentation-only** and `laneservice` remains the single authority for
  unit bodies. The reasoning is the phase-12 election precedent, not aesthetics.

## Related code files

`docs/adr/ADR-0011-multi-module-layout.md` · `docs/adr/ADR-0023-credential-guard-scope.md` ·
`docs/adr/README.md` (index) · `CLAUDE.md` (§Where things live, :800) ·
`docs/architecture.md` §Modules (~line 66) · `docs/getting-started.md` ·
`docs/testbed/e2e.md` · `README.md`

## Implementation steps

1. Write ADR-0024 to the outline above. Each of ADR-0011's three reasons gets a named
   disposition — no summary sentence that averages them.
2. Add the supersession header to ADR-0011; update `docs/adr/README.md`.
3. Amend ADR-0023 with the subtree scope and the widened-transitive note.
4. Rewrite the `CLAUDE.md` table against the actual tree. The one **live** module-count
   claim is the language-floor sentence at ~:1079 ("`go 1.27.0` across `go.work` and
   all fifteen modules"), which names a file that will no longer exist — rephrase it
   for one module rather than renumbering it. `.goreleaser.yaml`'s "all 12 modules"
   comment is deleted by phase 04 with the `GOWORK=off` entry it explains. Everything
   else matching `N modules green` (:750, :758, :988, :834 …) is a dated status record
   and is on the do-not-touch list above.
5. Rewrite `docs/architecture.md` §Modules as §Layout: one row per top-level
   directory, each stating what belongs in it and what does not.
6. Sweep: `grep -rn` for every old path across `*.md`, `*.sh`, `*.yaml`, `*.json`;
   fix live references only.
7. Mark the superseded plan: `status: superseded`, pointer to this plan.

## Todo list

- [ ] ADR-0024 written; three reasons individually dispositioned
- [ ] ADR-0011 superseded header; ADR index updated
- [ ] ADR-0023 amended
- [ ] `CLAUDE.md` table + :800 corrected
- [ ] `architecture.md` §Layout rewritten
- [ ] path sweep clean for live references
- [ ] old plan marked superseded

## Success criteria

1. No live claim in any doc names a path that no longer exists.
2. No dated status record was rewritten.
3. ADR-0024 states the R2 loss explicitly; a reader can find what got weaker without
   reading this plan.
4. `docs/architecture.md` §Layout has one row per top-level directory, with a
   "what must not go here" clause — the source's one portable idea.
5. The two root-level exceptions and the `init/` authority rule are each stated once,
   in a place a reader hits before proposing to "finish the job".

## Risk assessment

| Risk | Mitigation | Signal it broke | Pre-decided response |
|---|---|---|---|
| The ADR reads as a pure win and buries the R2 loss | criterion 3; the "what it costs" section is mandatory, not optional | a reviewer cannot name the downgrade after reading it | rewrite; this is the product's stated standard |
| A dated status record gets "corrected" | explicit do-not-touch list above | a `-race` line's module count changes | revert that hunk |
| Sweep fixes a historical reference and corrupts a record | fix live references only; plans/reports are out of scope | a `plans/**` file changes | revert |
| ADR-0024 claims the revisit condition was met beforehand | it was met concurrently — say so | the ADR implies a precondition was satisfied | correct the wording; the owner decision is the actual authority |

## Security considerations

- ADR-0023 is a security ADR. Its amendment must state the *new* bound plainly enough
  that someone adding a dependency knows which allowlist fails first. An amendment
  that leaves the scope ambiguous is worse than the pre-collapse state, because the
  guard still runs and no longer means what its doc says.
- The known limits list in `docs/architecture.md#assurance--what-the-evidence-proves`
  is user-facing security documentation. Nothing in this refactor changes what the
  evidence proves — verify none of those sentences was edited into a stronger claim
  while paths around them were updated.

## Next steps

Phase 07 adds the convention statement and the check that stops the new layout
drifting the way the old one did.
