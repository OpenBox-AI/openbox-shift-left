# Phase 14 — coverage matrix and documentation reconciliation

## Context links

- Parent: [plan.md](plan.md) · Depends: phases 09–13 (and phase 07's stage-A
  checkpoint, which this phase extends rather than repeats)
- Reference form: openbox-logger `docs/CAPTURE_MODEL.md` (per-signal, per-lane table)

## Overview

- Date: 2026-08-27 · Priority: P1 · Effort: 4h
- Implementation status: implemented (2026-08-30) · Review status: pending
- Make the documentation true about what now exists — including what still does not
  work. Runs last because it documents what phases 09–13 *actually proved*, not what
  they intended.

## Key insights

- This repo's stated rule: "a governance product that overstates itself is the
  failure it exists to prevent." Three lanes with different reach make averaging
  tempting and wrong.
- Coverage is a **table**, not a sentence — the logger's `CAPTURE_MODEL.md` form:
  per signal, per lane, what is seen, what is authoritative, what is absent.
  COVERAGE.md §3.4 already refuses to average the two providers; extend that habit
  to lanes.
- `README.md` has carried a false claim before (the ADR-0019 leftover). Grep for
  claims about what is "never sent" and re-verify each against current code.
- The CA is a real security downgrade in exchange for desktop coverage. Document it
  the way ADR-0015 documents plaintext credentials: argue it against the prior
  rationale, not around it.
- **The dependency story changes shape again here** (validation round 2): phase 07
  documented the stage-A set (six module-scoped dependencies); stage B adds
  goproxy (in `transport/`) and otlpreceiver (in `telemetry/`). Document
  per-module ownership, each module's own guard as the enumerating control, and
  record **otlpreceiver's ~98-require transitive tree as an accepted cost** with
  the number phase 09 actually measured — not smoothed into "a few libraries".

## Requirements

1. `COVERAGE.md` — a per-signal × per-lane × per-provider matrix; the desktop column
   populated; the Codex column honest (its OTel support is a probe result, not an
   assumption).
2. `docs/architecture.md#assurance` — the limits list updated: T3 suppressibility,
   OD1(c) truncation, the CA's blast radius, the single-host allowlist, probe A's
   dormant refusal, and what silence means (OD4).
3. `docs/data-and-privacy.md` — telemetry content, raw bodies, the CA, and the
   removal command's data destruction.
4. `CLAUDE.md` — a status block for the new lanes in the existing voice: what is
   implemented, what is verified how, what is NOT run.
5. `contracts/dev-event/MAPPING.md` §7 — live-stack items for the new producers.
6. `README.md` — claims re-verified against code.
7. Dependency story finalized: per-module direct-dependency list (stage A's six +
   goproxy + otlpreceiver), each module's guard named as the control, the
   otlpreceiver tree size recorded as accepted, with measured numbers cited.
8. `plans/reports/verification-260827-three-lanes.md` — every claim split by
   evidence strength (measured / conformance / unit / unproven), the house form.

## Architecture

Documentation ownership, to avoid the duplication this repo warns about:

| Document | Owns |
|---|---|
| ADR-0022 | why the lanes exist; the four rulings; the adoptions |
| ADR-0021 (amended) | the gateway, and §5's reversal |
| COVERAGE.md | what each lane sees, per provider |
| architecture.md | assurance limits |
| data-and-privacy.md | what egresses and what is stored locally |
| MAPPING.md | field-by-field wire mapping |
| CLAUDE.md | working context + status |

Each claim cites its source (repo symbol/path or upstream URL). Link to
machine-owned artifacts rather than copying their content.

## Related code files

- `COVERAGE.md`, `contracts/dev-event/COVERAGE.md`, `contracts/dev-event/MAPPING.md`
- `docs/architecture.md`, `docs/data-and-privacy.md`, `docs/getting-started.md`
- `README.md`, `CLAUDE.md`
- new: `plans/reports/verification-260827-three-lanes.md`

## Implementation steps

1. Build the coverage matrix from what phase 13 proved — read the phase reports,
   do not re-derive from intent.
2. Update the assurance limits list; add the CA and truncation entries.
3. Rewrite the privacy doc's capture section for three lanes; state the removal
   command's deletions explicitly.
4. Add the CLAUDE.md status block, including the honest "testbed NOT run" line for
   whatever still has not run.
5. Finalize the dependency story: per-module list, guards as controls, the
   otlpreceiver tree and binary-size deltas from phases 09/11 cited by number.
6. Grep `README.md` and the docs tree for absolute claims ("never", "always",
   "cannot") and re-verify each against current code; fix what is now false.
7. Write the verification report splitting claims by evidence strength.
8. Update `getting-started.md` with the two commands.

## Todo

- [x] coverage matrix built from phase reports
- [x] assurance limits updated (T3, truncation, CA, allowlist, dormant refusal, OD4)
- [x] data-and-privacy rewritten for three lanes + removal deletions
- [x] CLAUDE.md status block
- [x] MAPPING.md §7 items
- [x] dependency story finalized with measured numbers (incl. otlpreceiver tree)
- [x] absolute-claim sweep across README and docs
- [x] verification report by evidence strength
- [x] getting-started: the two commands

## Success criteria

- Every lane's coverage is stated per signal and per provider; nothing averaged.
- Every limit in the proposal's §7 appears in `architecture.md`.
- No document claims a capability that phase 13 did not demonstrate.
- The dependency docs match every module's `go.mod` exactly, and the otlpreceiver
  cost is stated with its measured number.
- The verification report labels each claim measured / conformance / unit /
  unproven, and the unproven list is non-empty and specific.
- `docs.maxLoc` (800) respected per file.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| Docs claim more than phase 13 proved | Write from phase reports, not from plan text; the verification report forces the split | A claim with no evidence label | Downgrade the claim; this is the product's founding rule |
| A false claim survives (the ADR-0019 README precedent) | Explicit absolute-claim sweep | An absolute claim contradicted by code | Fix and note it in the verification report |
| Duplication drifts between ADR, COVERAGE and CLAUDE.md | Ownership table above; link rather than copy | The same fact stated differently in two files | Delete the copy, keep the owner |
| **Assumption: phases 09–13 all landed.** If a lane descoped, the docs must say so | Write last, from reality | A phase report says descoped | **Adjust**: document the descope as a stated limit — that is a valid outcome, not a failure |
| The privacy doc implies the CA is protected | Mirror ADR-0015's honesty about plaintext credentials | Any sentence implying at-rest protection | Rewrite; do not soften |
| The dependency cost gets averaged into a reassuring sentence | Per-module table with numbers; the accepted-cost framing is the record | "lightweight dependencies" or similar appears | Replace with the measured numbers |

## Security considerations

- This phase is where the CA's blast radius becomes public documentation. Understate
  nothing: anything running as the developer can read it, and it can impersonate the
  provider to this host.
- Document that `--remove-all` destroys local evidence, so an operator who needs it
  exports first.
- Restate the redactor's measured reach at the new volume rather than repeating the
  old sentence unchanged.

## Handoff from phase 13 — the sentences this phase must not get wrong

Phase 13 produced **strong replay evidence and zero live evidence**. The
overstatement most likely to appear here is collapsing the two: writing that the
transport lane "relays byte-identically" and the telemetry lane "maps provider
traffic" as verified product properties, while dropping the dial substitution, the
in-memory pipe, the un-run OTLP HTTP intake and the fact that no stack has ever
stored one of these events.

**This sentence belongs in `COVERAGE.md` and its substance echoed in CLAUDE.md's
status paragraph:**

> Both new lanes are verified by REPLAY: real recorded traffic through the shipped
> code path, bind-free, with the relay's upstream dial substituted
> (`gateway/gatewaytest`) and no socket anywhere. That proves the bytes the relay
> forwards and captures, the mapping, the gate and the caps — it proves nothing
> about bind, listen, TLS to a real socket, the OTLP HTTP intake, or what core
> stores. Those live only in the dormant `testbed/46-otel-lane.sh` and
> `47-transport.sh`.

Three companion rules:

1. **Every corpus number carries its run id.** `20260827T063932Z-225cac` is one
   machine, one workload, all subscription-OAuth. 96.75%, 70,080 bytes/call and
   the retry-count census generalize no further than that, and the three
   denominators (5,340 / 5,231 / 5,049) are different populations — the
   verification report explains which.
2. **The all-zero `x-stainless-retry-count` must never appear near ADR-0021 §9.**
   A corpus containing no refusals is evidence that the header exists, not
   evidence about retry-around behaviour. Probe A remains the only source for §9,
   and `probes/refusal-injector/` + its runbook are now the instrument.
3. **The redactor's reach is measured now and the attribution matters more than
   the headline.** 52.8% of recorded model calls carry a marker, from ~200
   distinct sites, of which our own two generic rules are 55% and gitleaks'
   `generic-api-key` is 13%. Do not restate the old "two false positives"
   sentence.

Also for this phase: the module count is **15** (`probes/refusal-injector` added),
and the backend ask gains a capacity number — roughly **334 MB of spool per
5,000-call session** — alongside the still-open server-side dedupe request.

## Next steps

Plan complete. Remaining open items — probe A's outcome, ADR-0021 §10's OAuth
branch, and the live-stack claims — are tracked in the verification report, not in
this plan.
