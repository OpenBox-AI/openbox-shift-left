# Phase 04 — Docs reconciliation + full verification sweep

## Context links

- Plan: [plan.md](plan.md)
- Phases: [01](phase-01-stale-engine-hook-merge.md), [02](phase-02-posture-wire-keys.md), [03](phase-03-doctor-duplicate-engine-check.md)
- Doc surfaces from research: `research/researcher-02-posture-wire.md:50-59`, `research/researcher-01-hook-registration.md:23-27`

## Overview

- **Date:** 2026-08-14
- **Description:** Update every doc claim the three fixes change, confirm ADR-0017 needs no
  amendment under D2, add the dormant testbed assertion for stale-entry replacement, and run the
  full workspace sweep CI runs.
- **Priority:** P2 (last phase; blocks calling the work done)
- **Implementation status:** complete
- **Review status:** reviewed

## Key Insights

- No new ADR is required: no new table, endpoint or service (CLAUDE.md "Core principle"). Issue 1
  is a bugfix inside an existing merge; Issue 2 makes shipped code match ADR-0017:237-239, which
  is why D2 chose the key over an ADR amendment. **If D2 is reversed, this phase changes shape** —
  ADR-0017:237-239 would need an amendment saying `fail_closed` carries the failure policy.
- ADRs are historical records, not living docs. ADR-0017:222-252 and ADR-0016:51,229 are **not**
  rewritten; ADR-0016's `localhooks.go:45-55` line citation will drift after Phase 01 and that is
  acceptable for a dated decision record — verify, do not chase.
- `testbed/30-enforce.sh:185-186` already asserts `control_plane` and `fail_open` inside the
  SessionStarted row and **fails today**. Phase 02 is what makes them pass; `:187`
  (`assert_absent bundle_integrity`) passes today only via the empty-value guard and passes
  structurally afterwards. No testbed edit needed for Issue 2 — record this, do not duplicate it.
- `docs/getting-started.md:150` still claims init "pulls your org policy into a local bundle" —
  untrue since ADR-0017 deleted the bundle. Pre-existing, but it is a bullet in the exact list
  Phase 01's behavior change edits, and this repo's rule is that docs stay true. Fix it in the
  same edit; it is one line.

## Requirements

1. Every doc sentence describing how `init` merges hooks reflects replace-not-append.
2. Every doc sentence describing what posture reports is true for the new key set.
3. `openbox doctor`'s documented capability list mentions the duplicate-engine warning (only if
   Phase 03 shipped).
4. ADR-0017 verified accurate as-written; no ADR text edited unless D2 is reversed.
5. One dormant testbed assertion added for stale-entry replacement, marked as requiring a live
   stack.
6. Full workspace sweep green; a written split of what the sweep proves and what stays unproven
   without the testbed.

## Architecture

Doc surfaces, exact and complete:

| File | Line/section | Change |
|---|---|---|
| `docs/getting-started.md` | 148 | hook-merge bullet: add that a re-run **replaces** OpenBox entries left at a different engine path and prints what it replaced |
| | 150 | delete the dead "pulls your org policy into a local bundle" bullet (ADR-0017) |
| | 233, 389 | doctor description: add the duplicate-engine warning (Phase 03 only) |
| `README.md` | 93-96 | "Project scope writes the hook entries…" — one clause on replacing stale-path entries |
| | 258 | `openbox doctor` table row — append the duplicate-engine check (Phase 03 only) |
| `docs/architecture.md` | 128-131 | verify the generic "effective posture" sentence is still true; no key-level claim to change |
| | `#assurance--what-the-evidence-proves` | add the honest limit: a governed project can hold a registration from an older engine until the next `init`, and `doctor` is how that becomes visible |
| `docs/data-and-privacy.md` | 99, 140 | verify only — generic posture wording, no key list |
| `docs/adr/ADR-0017-inline-policy-evaluation.md` | 222-252 | verify accurate after Phase 02; **no edit** under D2 |
| `docs/adr/ADR-0016-default-install-posture.md` | 51, 229 | verify only; stale line citation accepted (dated record) |
| `CLAUDE.md` | "Current state" | 2-4 sentences: init now replaces its own stale-path registrations (and why the live 66-event double-count was two engines, not an ADR-0018 defect); posture now carries `decision_authority`/`failure_policy` and the five bundle keys are gone |
| `contracts/dev-event/MAPPING.md` | — | **no change** — it never mentioned posture; documenting it now is out of requested scope (research Q5) |

Testbed (dormant — `testbed/10-onboard.sh` needs a live stack; it runs `openbox auth` against the
backend at `:62`):

- Insert after the existing hook block (`testbed/10-onboard.sh:167-176`): rewrite the settings
  file's engine path to a bogus one with `sed`, re-run the same `init` invocation, then assert
  `grep -c 'hook claude-code PreToolUse' == 1`, `grep -c '<bogus path>' == 0`, and that
  `rewake claude-code` survives. Uses only `grep`/`sed` + existing `assert_eq`/`assert_absent`
  helpers (`testbed/lib/assert.sh:40,62,66`) — no new tooling.
- If Phase 03 shipped, one more line: `openbox doctor` output contains no `WARNING` about engines
  after that re-init.
- Record in the phase notes that `testbed/30-enforce.sh:185-187` is Issue 2's end-to-end proof and
  already written.

## Related code files

Read-only verification targets (claims must match code after phases 01-03):

| Path | Lines | Claim to re-check |
|---|---|---|
| `adapters/claude-code/localhooks.go` | 11-27 | file doc comment matches shipped merge behavior (Phase 01 edits it) |
| `adapters/common/devconfig/posture.go` | 242-271 | emitted key set matches every doc list |
| `cli/cmd/openbox/doctor.go` | new section | wording matches the doc bullet |
| `testbed/10-onboard.sh` | 167-176 | insertion point |
| `testbed/30-enforce.sh` | 176-190 | dormant posture assertions |
| `.github/workflows/ci.yml` | 52-68, 81, 85 | the exact sweep commands |

## Implementation Steps

1. Re-read each doc line in the table above **before** editing (they may have moved); update only
   the sentences whose truth changed.
2. Edit `docs/getting-started.md` (148, 150, + 233/389 if Phase 03 shipped).
3. Edit `README.md` (93-96, + 258 if Phase 03 shipped).
4. Add the assurance-limit sentence to `docs/architecture.md`; verify 128-131 needs nothing.
5. Verify `docs/data-and-privacy.md:99,140` and both ADRs; record "verified, no edit" per file
   rather than silently skipping.
6. Update CLAUDE.md "Current state" — short, and honest about what the testbed has not run.
7. Add the dormant testbed assertions to `testbed/10-onboard.sh` with a comment saying they are
   dormant until a local stack is reachable.
8. Full sweep, mirroring CI:
   - `gofmt -l .` **first** — it is CI's opening gate (`ci.yml:31-38`) and must be
     empty. Listing only build/vet/test below is what let a formatting failure
     survive a sweep that reported green; a deleted test function leaves the blank
     lines that trip it.
   - `MODS=$(go list -m -f '{{.Dir}}/...' 2>/dev/null)` per `ci.yml:52-63`, then
     `go test -race $MODS`
   - `GOOS=windows GOARCH=amd64 go build $MODS`
   - `GOOS=linux GOARCH=arm64 go build $MODS`
   - `go vet` per module as CI does
   - the tier-vocabulary doc gate (`ci.yml:100-122`), since this phase edits
     `docs/` and `README.md`
9. Write the evidence split: unit/conformance-verified vs unverified-without-a-stack.

## Todo list

- [x] `docs/getting-started.md` 148 + 150 (+233/389)
- [x] `README.md` 93-96 (+258)
- [x] `docs/architecture.md` assurance limit added; 128-131 verified
- [x] `docs/data-and-privacy.md`, ADR-0016, ADR-0017 verified (no edit expected)
- [x] CLAUDE.md "Current state" updated
- [x] dormant assertions in `testbed/10-onboard.sh`
- [x] all 11 modules `go test -race` green
- [x] both cross-compiles green
- [x] `gofmt -l .` empty and the tier-vocabulary doc gate green (added in review —
      the first sweep skipped both, and `gofmt` was failing)
- [x] evidence split written into the plan folder
- [x] conventional commits, no plan/phase labels in code, tests, or messages

## Success Criteria

- No doc sentence describes append-only merge behavior or a posture key that is not emitted.
- `grep -rn "local bundle" docs/getting-started.md` returns nothing describing `init`.
- ADR-0017:237-239 is a true statement about shipped code, unedited (D2 held).
- All 11 modules green under `-race`; both cross-compiles green; `go vet` clean.
- The evidence split names, per claim, which artifact proves it — and explicitly states that unit
  tests do not prove a hook fires in a real session.

## Risk Assessment

| Risk | L×I | Mitigation |
|---|---|---|
| R1 A doc edit overstates the fix ("duplicates cannot happen") | Med×Med | Wording states the mechanism and its limits: replacement happens **on the next `init` in that directory**; already-stored duplicate events are not retro-fixed; an unquoted spaced path stays unrecognized (Phase 01 R2). This repo's failure mode is a confident sentence. |
| R2 Editing an ADR turns a dated record into a living doc | Med×Med | ADRs are verify-only here. The one exception is a D2 reversal, which would require an explicit amendment section, not an in-place rewrite. |
| R3 Sweep hides a failure in a module nobody touched | Low×Med | Run the workspace-wide `$MODS` form CI uses, not per-module `./...` only; `ci.yml:41-48` also asserts `go.work` covers every module (no new module was added, so this must stay green). |
| R4 (assumption) Doc line numbers in this table are still accurate at implementation time | Med×Low | **Signal:** the cited line does not contain the quoted claim. **Pre-decided response:** adjust in-plan — re-grep the claim text (`settings.local.json`, "effective posture", "decided by") and edit the owning sentence; do not edit by line number blindly. |
| R5 (assumption) The dormant testbed assertion is correct without ever running | Med×Med | It uses only `grep`/`sed` and helpers already used in the same file. **Signal:** first live run fails on the new assertion rather than on the product. **Pre-decided response:** adjust in-plan — the assertion is test code, fix it against observed output; do not weaken the product to satisfy it. |
| R6 Claiming completion on unit evidence alone | Med×High | The evidence split is a deliverable of this phase, not a footnote: the double-count disappearing end to end, and `decision_authority` landing in `governance_events`, are **unproven** until `testbed/10-onboard.sh` and `testbed/30-enforce.sh` run against a live stack. Say so in CLAUDE.md too. |

## Security Considerations

- Docs are a security surface in this product: `docs/architecture.md#assurance--what-the-evidence-proves`
  is the canonical honest-limits list, so the stale-registration window belongs there, not only in a
  changelog.
- Do not document the duplicate-engine warning as assurance — it reads one file and proves what is
  registered, not what ran.
- No doc edit may imply `settings.local.json` or `~/.openbox/.env` is protected (ADR-0015); the
  plaintext-credential framing stays as-is.

## Next steps

- Land, then run `./testbed/run-all.sh` when a local stack is reachable and retire the dormant
  markers with the observed output.
- Follow-up (not in this plan): audit `cli/internal/managed/managed.go` (`wouldWeaken`,
  `applyFile`, `render`) for a differently-shaped stale-engine problem in global scope —
  unread, unconfirmed (plan.md unresolved question 1).
