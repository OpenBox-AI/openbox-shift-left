# Phase 05 — Evidence: the deciding `policy_id` into posture

## Context links

- Parent: [plan.md](plan.md) · Depends on: phase 3 · Parallel-safe with: phase 4
- Blocks: phase 6 (the bundle fields cannot be deleted until their evidence role is replaced)
- The feature at stake is "posture as evidence" — a session reports its own effective
  posture so the control plane never has to trust the endpoint's word.

## Overview

- **Date:** 2026-08-13
- **Description:** Deleting the bundle removes `bundle_version`, `bundle_integrity`,
  `bundle_sha256` and `policy_id` from the reported posture. Replace them with the identity
  of the policy that actually decided, which `/evaluate` already returns.
- **Priority:** P1 · **Implementation status:** done · **Review status:** pending

## Key Insights

- **This is plumbing, not a backend ask.** `/evaluate` already returns `policy_id` and the
  client already parses it into the verdict (`client/verdict.go:116,195,275`). Nothing
  server-side has to change for the basic replacement.
- **Without this, phase 6 is a silent assurance downgrade.** `openbox doctor` and session
posture report bundle identity and integrity today (`doctor.go:30-47`); delete the bundle
with no replacement and the control plane loses the ability to answer "which policy is this
endpoint actually enforcing?" That is the question that decision exists for.
- **`policy_id` is an identity, not a version.** Only `policy_id` was found on the wire — no
  epoch, no updated-at. So the new posture can answer "which policy decided" but **not**
  "which revision of it". If the control plane needs revision-level evidence (drift over
  time, "was this call judged before or after the tightening?"), that is a backend ask this
  phase files rather than fakes.
- **Integrity changes meaning, and the docs must not paper over it.** Bundle integrity was a
  *signature* claim about a local artifact. The new claim is weaker and different: the
  verdict came from the control plane over an authenticated channel. Do not reuse the word
  "verified" for it.
- Sessions with no reachable core have **no** policy identity to report. That absence is
  itself evidence — it means the call was decided by the failure policy, not by policy — and
  posture should say so explicitly rather than omitting the field.

## Requirements

1. Session posture carries the `policy_id` from the verdict that decided, plus a marker for
   "decided by failure policy, no verdict obtained".
2. `openbox doctor` reports the same, in place of the bundle fields.
3. Do **not** carry the word "verified"/"integrity" over to the new field; name it for what
   it is (e.g. control-plane verdict identity).
4. The absent case is explicit: a session that never reached core reports that its calls were
   decided locally by `fail_closed`, not an empty field.
5. Decide and record whether `policy_id` alone is sufficient evidence. If not, file the
backend ask for a version/epoch with the concrete use case, and note in that decision that
the evidence is identity-only until then.
6. No new config surface.

## Architecture

```
/evaluate verdict ──▶ client.Verdict.PolicyID ──▶ session posture ──▶ core
                                             └──▶ openbox doctor (local view)

no verdict obtained ──▶ posture: decided_by = failure_policy (fail_open|fail_closed)
```

## Related code files

| Path | Action |
|---|---|
| `client/verdict.go:116,195,275` | `PolicyID` already parsed — the source |
| `adapters/common/devconfig/posture.go:96-203` | posture fields + their serialization |
| `cli/cmd/openbox/doctor.go:30-47` | replace the bundle block |
| `docs/architecture.md#assurance--what-the-evidence-proves` | the claim changes; phase 7 rewrites the prose |

## Implementation Steps

1. Add the posture field(s); populate from the verdict on the enforce path.
2. Add the failure-policy-decided marker for the no-verdict case.
3. Replace `doctor`'s bundle block; keep its output shape recognisable for anyone with a
   script parsing it, and note the change in phase 7's docs.
4. Test: a session with a verdict reports the policy identity; a session with core
   unreachable reports failure-policy-decided; neither reports an empty field.
5. Write the sufficiency decision into that decision; file the backend ask if the answer is no.
6. All 11 modules green.

## Todo list

- [x] Posture carries the deciding `policy_id`
- [x] No-verdict sessions report failure-policy-decided, not an empty field
- [x] `doctor` reports it in place of the bundle fields
- [x] No "verified"/"integrity" language reused for a weaker claim
- [x] `policy_id`-is-enough decision recorded; backend ask filed if not
- [x] All 11 modules green

## Success Criteria

- Every session with a reachable core reports the identity of the policy that decided.
- A session run with core down reports *why* it has no policy identity.
- `openbox doctor` never shows a bundle field after phase 6.
- Nothing in posture claims cryptographic verification that no longer happens.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| Bundle fields deleted before this lands | M×H | posture has no policy provenance at all | **Stop:** phase 6 depends on this phase for exactly this reason. |
| The new field inherits "verified" phrasing | M×H | posture implies a signature that does not exist | **Adjust:** rename. This is the overstatement class `CLAUDE.md` forbids. |
| Absent identity read as "no policy" rather than "no verdict" | M×M | dashboards show unenforced sessions as unpoliced | **Adjust:** the failure-policy marker exists precisely to distinguish them. |
| `policy_id` proves insufficient after release | L×M | control plane cannot answer revision questions | **Accepted, disclosed:** That decision states evidence is identity-only; the backend ask is filed with a use case. |

## Security Considerations

Posture is the evidence channel — it is what stops the control plane having to trust an
endpoint's self-report. Weakening what it carries while keeping its confident vocabulary
would be worse than carrying less: state identity-only, say when a call was decided by the
failure policy, and never imply a signature check that no longer runs.

## Next steps

Phase 6 deletes the local policy path, now that nothing depends on it for either decisions
or evidence.
