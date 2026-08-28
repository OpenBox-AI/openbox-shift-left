# Phase 07 — stage-A docs reconciliation

## Context links

- Parent: [plan.md](plan.md) · Depends: 01–06 (documents what they changed)
- Evidence: [audit-260827-2227](../reports/audit-260827-2227-oss-replacement-shipped-code.md)

## Overview

- Date: 2026-08-27 · Priority: P2 · Effort: 3h
- Implementation status: **done** · Review status: pending
- Report: [verification-260828-phase-07](reports/verification-260828-phase-07-stage-a-docs.md)
- Make the docs true again after six phases moved the dependency story, the Go
  floor, and one security control's scope. Docs-only; no code. This is the
  stage-A checkpoint — stage B's lanes get their own reconciliation in
  [phase 14](phase-14-coverage-and-docs.md).

## Key insights

- **The repo's stated rule is that an overstating doc is the failure the product
  exists to prevent.** Three claims go stale in this stage and each currently reads
  as more assurance than will exist: the `x/term` pin instruction, "one external
  dependency", and the credential guard's scope.
- **`CLAUDE.md`'s `x/term` paragraph must be deleted, not amended.** It instructs
  future agents not to let `go mod tidy` bump the pin. After phase 01 that
  instruction is wrong, and a wrong instruction in the agent-context file is worse
  than a missing one — it will be followed.
- Dependency count goes **1 → 6** module-scoped external dependencies at this
  checkpoint (phases 01–06 add gitleaks, santhosh-tekuri, go-toml, godotenv,
  kardianos, renameio; goproxy and otlpreceiver arrive later in this plan, phases
  [09](phase-09-telemetry-receiver-daemon.md)/[11](phase-11-transport-proxy-service.md),
  documented by phase 14). The docs currently lean on "essentially no
  dependencies" as an assurance argument. That argument has to change shape, not
  volume.
- **Nothing in stage A changes what egresses.** No content field, no gate, no
  cap. `data-and-privacy.md`'s substantive claims stand; only the *mechanism*
  sentence about secret detection changes (phase 06).

## Requirements

1. `CLAUDE.md`: `x/term` pin paragraph deleted; dependency story updated; the
   credential-guard scope sentence corrected; the secret-detection paragraph
   updated for gitleaks.
2. `docs/data-and-privacy.md`: secret-detection mechanism updated — what the
   detector now catches and what it still misses.
3. `COVERAGE.md`: dependency and detection-coverage rows updated.
4. `docs/architecture.md#assurance--what-the-evidence-proves`: the
   "local secret detection is keyword-driven so an unlabelled high-entropy value
   is invisible" limit is **re-measured**, not assumed — phase 06's soak decides
   whether it still reads that way.
5. ADR index updated with phase 05's guard-scope ADR.
6. `.goreleaser.yaml`'s "all 11 modules" comment corrected to 12.
7. Every changed claim cites its source (repo symbol/path or upstream URL).

## Architecture

Documentation only. Surfaces and their owners:

```
CLAUDE.md                     agent working context — pin instruction, deps, guard scope
docs/data-and-privacy.md      what egresses and what protects it
COVERAGE.md §3                per-provider coverage + detection differences
docs/architecture.md          the assurance/limits section
docs/adr/                     phase 05's ADR + index
.goreleaser.yaml              stale module count in a comment
```

## Related code files

- edit: `CLAUDE.md`, `docs/data-and-privacy.md`, `COVERAGE.md`,
  `docs/architecture.md`, `docs/adr/` (index), `.goreleaser.yaml`
- reference: each phase's report in `reports/`

## Implementation steps

1. Read each target before editing it (repo rule), and collect the six phases'
   reports first — the docs describe what *happened*, not what was planned.
2. `CLAUDE.md`: delete the `x/term` pin paragraph. Replace the dependency
   sentence with the real list and each dependency's owning module. Correct the
   guard-scope description to direct-requires. Rewrite the secret-detection
   paragraph against phase 06's measured results.
3. `docs/data-and-privacy.md`: update the detection mechanism. Keep the honest
   limit sentence, but **re-derive it** from the soak rather than copying the old
   wording — the boundary may have moved.
4. `COVERAGE.md`: dependency rows; the Codex-vs-Claude-Code detection difference
   (Codex's mapper still has no redactor — that does not change here and must
   not be smoothed away).
5. `docs/architecture.md`: update the assurance limits list for the new detection
   reach and the guard's stated scope.
6. ADR index: add phase 05's ADR.
7. `.goreleaser.yaml`: 11 → 12 modules.
8. Verify every link and every claim against source. No claim without a citation.

## Todo

- [x] phase reports read; every claim re-verified against source rather than the plan
- [x] `CLAUDE.md` x/term paragraph deleted (phase 01); verified NO pin instruction survives in the live tree
- [x] `CLAUDE.md` dependency story + detection paragraph; **guard scope ADDED — no such sentence existed**
- [x] `data-and-privacy.md` mechanism + limit RE-MEASURED (20 shapes); table validated; +1 new limit, +1 false-positive class; "only the prompt is exempt" corrected
- [x] `COVERAGE.md` detection-difference row updated; **it has NO dependency section — reported, not invented**
- [x] `architecture.md` mechanism + 2 new assurance limits (detection reach, guard scope)
- [x] ADR index updated (phase 05); verified
- [x] `.goreleaser.yaml` 11 → 12 (phase 01); verified
- [x] every changed claim cites a repo path or ADR; **45 relative links checked, 0 broken**

## Success criteria

- No instruction anywhere tells a future agent to hold the `x/term` pin.
- The dependency count and per-module ownership in the docs match `go.mod` exactly.
- The credential guard's documented scope matches what `guard_test.go` actually
  checks after phase 05 — direct requires, any host.
- The secret-detection limit in the docs is what phase 06 **measured**, with the
  evidence cited; not inherited wording.
- No doc claims a protection that phases 01–06 did not deliver.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| **A stale limit is copied forward** and the docs describe a detector that no longer exists | Re-derive the limit from phase 06's soak; cite it | The doc's wording matches the pre-gitleaks text verbatim | Rewrite from the measurement |
| Docs overstate the new detection ("now catches all secrets") | Every claim cites the soak's actual numbers | A claim with no citation | Remove or measure it |
| The guard-scope reduction gets softened in prose | Phase 05's ADR is the authority; docs must match its framing | Docs say "clarified" where the ADR says "reduced" | Use the ADR's words |
| CLAUDE.md grows instead of being corrected | Net-neutral or shorter is the target | File length grows materially | Cut; this file is agent context, not a changelog |
| Docs updated before the code settles | Phase depends on 01–06 complete | A phase report is missing | Wait; do not document intent as fact |

## Security considerations

- `docs/data-and-privacy.md` is a **user-facing security claim**. Overstating what
  the detector catches here is exactly the failure mode the repo's own conventions
  name. Under-claiming is the safe direction.
- Do not remove the Codex asymmetry from `COVERAGE.md` §3.4 (Codex's mapper has no
  redactor; its prompt is the only content it sends). Nothing in stage A changes
  it, so it must survive the edit.
- Phase 05's ADR reduces a security control's scope. The docs must say so in the
  same terms, so a reader auditing the product is not misled about what the guard
  proves.

## Next steps

Stage A complete. Stage B — the convergence phases
([08](phase-08-adr-contract-decision.md)–[14](phase-14-coverage-and-docs.md)) —
proceeds on this foundation, with its version pins retired by phase 01, its
service lifecycle settled by phase 04, and its contract branches validated by
phase 02's library validator.

## Outcome (2026-08-28)

Done — see the
[verification report](reports/verification-260828-phase-07-stage-a-docs.md).
Docs-only: no `.go` file touched, 45 relative links resolve, the six
fully-runnable modules green.

**The limit was re-measured rather than inherited, and that mattered.** The
existing table in `data-and-privacy.md` was correct and every row reproduced, but
the measurement added two things the docs did not have: the **keyword-adjacency**
gap (`AWS_ACCESS_KEY_ID=…` is invisible where `access_key=…` is caught — the exact
gap behind phase 06's regression), and a **false-positive class** where the entropy
pass rewrites a base64 literal in a source assignment.

**One correction ran the other way:** the docs said "only the prompt is exempt"
from the scanner, which has been false since 2026-08-26. Understating protection is
the mirror of the failure the repo's rule names, and misleads a reader the same way.

**Two requirement premises did not hold and are reported rather than invented:**
`COVERAGE.md` has no dependency section (it is a provider-coverage document), and
no credential-guard scope sentence existed anywhere to "correct" — it had to be
written for the first time.
