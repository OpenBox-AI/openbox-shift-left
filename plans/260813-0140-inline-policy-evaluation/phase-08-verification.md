# Phase 08 — Verify against the real thing

## Context links

- Parent: [plan.md](plan.md) · Depends on: phase 7
- Repo rule (`CLAUDE.md`): "Verify against the real thing… unit tests are not evidence that a
  hook works." `testbed/run-all.sh` drives real headless sessions against a real local stack.
- Phase 1 answered two questions empirically. This phase answers the rest.

## Overview

- **Date:** 2026-08-13
- **Description:** Prove the inline path works end to end, prove the failure paths behave, and
  state honestly what could not be proven.
- **Priority:** P1 · **Implementation status:** pending · **Review status:** pending

## Key Insights

- **The interesting assertions here are the negative ones.** That a verdict is applied is easy;
  that a secret never egressed, that no second `ActivityStarted` was stored, and that an
  unreachable core produced the org's chosen behaviour are the claims worth a real run.
- **The raw-rego case is the headline fix and the hardest to stage.** It needs an org whose
  policy is hand-written rego — the case that silently failed open before. If the local stack
  cannot host such a policy, say so rather than claiming the fix is verified.
- **`fail_closed` needs both branches exercised**, and the fail-open branch is the one that
  looks like success while being the dangerous default: it must be observable that the call
  proceeded *and* that the timeout was recorded, not silently allowed.
- **Host-blocking is a legitimate test, not an attack.** It is the documented bypass; a test
  that blocks the core hostname and observes the outcome is how the docs' claim stays true.
- CI can catch vocabulary regressions cheaply — a grep gate for tier vocabulary costs nothing
  and prevents the rename from eroding.
- Windows remains build-verified only, and Codex enforcement depends on phase 1's ceiling
  number. Both limits stay stated rather than implied.

## Requirements

1. Testbed: a real session where a gated call is decided by `/evaluate` and the verdict is
   applied — for a class that previously never escalated (e.g. `Write`).
2. Testbed: exactly one `ActivityStarted` per gated call, with every class evaluating —
   re-confirming phase 1's finding on the shipped code rather than a scratch build.
3. Testbed: core unreachable ⇒ `fail_closed:false` proceeds **and records the failure**;
   `fail_closed:true` denies. Both asserted.
4. Testbed: core hostname blocked ⇒ the documented behaviour occurs, matching what the README
   and ADR-0017 claim.
5. Testbed: a known secret in a `Write` body never appears in anything that egressed;
   redaction still applied to the tool call with core unreachable.
6. Testbed: session posture carries the deciding policy identity; a no-verdict session reports
   failure-policy-decided.
7. Raw-rego org: gated call denied. If the local stack cannot host a raw-rego policy, record
   "not verified" with the reason — do not mark the headline fix proven.
8. CI: grep gate failing the build on tier vocabulary outside `docs/adr/`.
9. Final sweep: build, vet, `-race` for all 11 modules, plus the Windows cross-compile.
10. Record the run in `reports/`, per platform, including what was not exercised.

## Architecture

| Claim | Evidence |
|---|---|
| verdict obtained and applied inline, any class | testbed against a live local stack |
| one `ActivityStarted` per gated call | testbed count |
| fail-open records, fail-closed denies | testbed, both branches |
| bypass behaves as documented | testbed with the host blocked |
| secret never egresses; redaction still local | testbed + unit assertion |
| posture carries policy identity | testbed |
| raw-rego org enforced | testbed **if** the stack can host it; otherwise unverified |
| Windows runtime | **not verified** — cross-compile only |
| Codex enforcement ceiling | phase 1's number; runtime unverified unless a Codex session is driven |

## Related code files

| Path | Why |
|---|---|
| `testbed/run-all.sh`, `testbed/*.sh` | the pattern for a new phase script |
| `testbed/00-preflight.sh` | says whether the stack is healthy enough to trust results |
| `.github/workflows/ci.yml` | the grep gate + cross-compile |
| `docs/architecture.md#assurance--what-the-evidence-proves` | must match this table exactly |

## Implementation Steps

1. Write the inline-verdict testbed phase for a previously-unescalated class.
2. Add the duplicate-event count assertion; compare against phase 1's number.
3. Add both failure-policy branches; assert the fail-open case records the failure rather than
   silently allowing.
4. Add the host-blocked case; assert the documented outcome.
5. Add the secret-never-egresses and redaction-with-core-down assertions.
6. Add the posture-identity assertions, both the verdict and no-verdict cases.
7. Attempt the raw-rego org case; record the result or the reason it could not run.
8. Add the CI grep gate; confirm it fails on a deliberately reintroduced "tier".
9. Run `testbed/run-all.sh`; record in `reports/`. If it cannot run, record that — do not mark
   the feature verified.
10. Reconcile the assurance table in `architecture.md` with what the run actually proved.

## Todo list

- [ ] Inline verdict applied for a previously-unescalated class, asserted
- [ ] One `ActivityStarted` per gated call on shipped code
- [ ] fail-open records; fail-closed denies — both asserted
- [ ] Host-blocked case matches the documented claim
- [ ] Secret never egresses; redaction survives core-down
- [ ] Posture identity asserted, verdict and no-verdict cases
- [ ] Raw-rego case run, or its absence recorded with a reason
- [ ] CI grep gate added and demonstrably catches a regression
- [ ] All 11 modules green + Windows cross-compile
- [ ] Run recorded in `reports/` incl. what was not exercised

## Success Criteria

- A real session proves a `Write` was decided by the control plane.
- The duplicate-event count is exactly 1 per gated call, on shipped code.
- Both failure branches are demonstrated, and the fail-open one is visibly recorded.
- No secret appears in any egressed payload in a real run.
- `architecture.md`'s assurance table matches the run — no row claims more than was done.
- CI fails if tier vocabulary returns.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| Testbed cannot run (no local stack) | M×H | phase closes with no live evidence | **Adjust:** record "not run" in `reports/` and in docs. This repo's rule is that reading is not evidence — an unverified enforcement change is not shippable. |
| Raw-rego case unstageable locally | M×M | the headline fix is unproven | **Accepted, disclosed:** record it as unverified; do not let the ADR or README claim it as proven. |
| fail-open path passes trivially (call proceeds, nothing recorded) | M×H | no failure record in the events | **Adjust:** the assertion is proceed **and** record. A silent allow is the failure mode. |
| Codex runtime never exercised | M×M | a Codex-only enforcement bug ships | **Accepted, disclosed:** state that Codex is verified to the extent phase 1's ceiling allows, and name what is untested. |
| Docs updated to match a partial run | M×H | assurance table overstates | **Stop:** step 10 is the last gate; any row that overstates blocks the release. |

## Security Considerations

- The host-blocked test demonstrates a real bypass. Keep it in the suite: it is the only thing
  that keeps the documented claim honest as the code changes.
- Testbed scripts must not print credentials or the content they assert on — assert on presence
  and shape.
- A fail-open timeout that is not recorded is indistinguishable from an allowed call. That
  assertion is a security control, not a completeness nicety.

## Next steps

Plan complete. Follow-ups, each needing its own decision: the Cursor adapter (now inherits
enforcement with no bundle work); a policy version/epoch on the wire if identity-only evidence
proves insufficient (phase 5's backend ask); raising or documenting the 64KB evaluation cap;
and removing the deprecated config fields after their one-release grace period.
