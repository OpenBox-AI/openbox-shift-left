# Phase 05 — Version-scope the three caveat blocks

## Context links

- Plan: [plan.md](plan.md) · Blocks on: [phase 04](phase-04-deploy-and-live-replay.md)
- Fix: [phase 02](phase-02-core-dev-session-fix.md) — supplies the commit sha these docs cite
- Repo: **openbox-shift-left** (this repo). Docs only.

## Overview

- **Date:** 2026-08-14 · **Priority:** P1 · **Effort:** 1h
- **Description:** Rewrite the three caveat blocks from "known defect, fix under decision" to
  version-scoped ("fixed in core ≥ `<sha>`; older cores affected"). The troubleshooting row STAYS —
  old cores exist. No CLAUDE.md note (user decision). Init and flag-help strings untouched: the fix
  makes them true again.
- **Implementation status:** pending
- **Review status:** not reviewed

## Key Insights

1. **Deploy-gated by design.** A doc that says "fixed" before a running core carries the fix is
   exactly the overstatement this product exists to prevent. Hence the phase-04 dependency, not a
   convenience ordering.
2. **The troubleshooting row is not a bug report — it is a runbook entry.** A developer on an older
   core will still hit this and still needs the recovery. Version-scope it; do not delete it.
3. **Two of the three blocks make a claim about the CLIENT that is no longer the whole story.**
   README and architecture both frame it as "the client applies a HALT no policy authored". After
   the fix the client is unchanged and still would — the reason it no longer happens is that core
   stopped emitting one for dev sessions. Word it that way; do not imply a client-side guard that
   does not exist (ADR-0017 trust boundary).
4. **`init`'s promise becomes true again without touching its text.** `cli/cmd/openbox/main.go:330`
   and `cli/internal/devinit/devinit.go:411` both print "inert until your org publishes a policy,
   and fail-open" — the exact guarantee the defect falsified. Leave them; a re-word would imply the
   guarantee changed when only its truth did.
5. **The docs must keep citing sources** (repo rule). Each rewritten block cites the core commit,
   and the diagnosis report stays linked as the historical record.

## Requirements

- R1: README bullet version-scoped; the quickstart pointer to it stays coherent.
- R2: `docs/architecture.md` assurance bullet version-scoped.
- R3: `docs/getting-started.md` "Two defaults" sentence version-scoped; troubleshooting row KEPT and
  version-scoped.
- R4: every rewritten block cites the fixing core commit sha.
- R5: no CLAUDE.md note. No changes to `cli/cmd/openbox/main.go:330` or
  `cli/internal/devinit/devinit.go:411`.
- R6: no claim of "fixed" anywhere unless phase 04 verified it on a running core; if phase 04
  parked, this phase parks with it.

## Architecture

Four edits, one repo, docs only. Shared wording: name the core commit, state that older cores are
affected, and keep the recovery ("start a new session") reachable from the troubleshooting table.

## Related code files

| File:line | Change |
|---|---|
| `README.md:246-253` | the "control-plane HALT applied even when no policy authored it" bullet → version-scoped |
| `README.md:65` | quickstart pointer into "What this does not prove" — check it still reads correctly |
| `docs/architecture.md:193-205` | assurance bullet ("A control-plane verdict is applied even when no policy authored it") → version-scoped |
| `docs/getting-started.md:166-173` | "Two defaults" — the "One diagnosed defect escapes both today" sentence → version-scoped |
| `docs/getting-started.md:392` | troubleshooting row — KEPT, version-scoped, recovery preserved |
| `cli/cmd/openbox/main.go:330`, `cli/internal/devinit/devinit.go:411` | **untouched** |

## Implementation Steps

1. Take the merge commit sha and the verification outcome from phase 04. If phase 04 parked, stop
   here (R6).
2. Rewrite `README.md:246-253`: the behavior, then "fixed in core ≥ `<sha>` for developer sessions;
   cores older than that are affected". Keep the diagnosis link.
3. Re-read `README.md:65` in context; adjust only if the pointer now misleads.
4. Rewrite the `docs/architecture.md` assurance bullet the same way, keeping the assurance section's
   register (what the evidence proves / does not prove).
5. Rewrite the `docs/getting-started.md` "Two defaults" sentence: the escape is now scoped to older
   cores; keep the pointer to Troubleshooting.
6. Version-scope the troubleshooting row at `:392`; keep the symptom, the "no `(policy: …)` suffix"
   tell, and the recovery.
7. Verify every claim and link against source and against the phase-04 report before committing.

## Todo list

- [ ] Phase-04 outcome + sha in hand (or phase parked)
- [ ] README bullet rewritten
- [ ] README:65 pointer checked
- [ ] architecture.md assurance bullet rewritten
- [ ] getting-started "Two defaults" sentence rewritten
- [ ] Troubleshooting row version-scoped, recovery preserved
- [ ] No CLAUDE.md change; no CLI string change
- [ ] Links and claims verified

## Success Criteria

1. All three caveat blocks name the fixing core commit and state that older cores are affected.
2. The troubleshooting row still gives a developer on an older core the symptom, the tell and the
   recovery.
3. `git diff` touches only `README.md`, `docs/architecture.md`, `docs/getting-started.md`.
4. No document claims a client-side guard that does not exist.
5. If phase 04 parked, no document says "fixed" — the plan says "implemented, unverified" instead.

## Risk Assessment

- **R-A — docs claim "fixed" while some deployment still runs an older core.** *Break signal:* a
  developer reports the symptom after the docs say fixed. *Pre-decided response:* none needed if the
  version-scoped wording holds — that is exactly what it is for. If a block was written
  unconditionally, fix the wording immediately; do not delete the troubleshooting row.
- **R-B — phase 04 parks unverified.** *Break signal:* phase-04 status is "implemented, unverified".
  *Pre-decided response:* park this phase too, and say so in the plan status. Do not soften to
  "should be fixed" — the repo's rule is an honest limit over a confident sentence.
- **R-C — trimming removes the only recorded recovery.** *Break signal:* review finds "start a new
  session" gone. *Pre-decided response:* restore it. The row is a runbook entry, not a defect note.

## Security Considerations

These blocks are assurance claims — the honest-limits inventory a governance product is judged on.
Overstating them is the failure mode the repo names explicitly. Keep the diagnosis link as the
historical record so the claim remains auditable, and do not let the rewrite imply that content
capture, the plaintext credentials file, or any other standing limit changed: this fix touched one
rejection path in core and nothing else.

## Next steps

Plan complete. If phase 01 filed a client-side trigger bug, that is its own plan.
