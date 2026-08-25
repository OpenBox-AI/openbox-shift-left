# Phase 08 — Conformance and testbed

## Context links

- Parent: [plan.md](plan.md) · Previous: [phase-07](phase-07-distribution-assurance.md)
- Testbed: `docs/testbed/e2e.md`, `testbed/run-all.sh`
- Protocol oracle: `GET /protocol` on a running Claude apps gateway
- Depends on: 07

## Overview

- Date: 2026-08-25
- Description: prove the claims against a real stack and a real session, and pin the gateway
  contract against Anthropic's own machine-readable spec so a Claude Code release cannot
  break it silently.
- Priority: P1
- Implementation status: pending
- Review status: not reviewed

## Key insights

- **This repo's own rule: reading is not evidence.** Every phase before this one is
  unit- and conformance-verified only. `testbed/run-all.sh` drives real headless sessions
  against a real local stack and asserts what arrived. Until it runs, the claims are claims.
- **`GET /protocol` is the oracle.** A running Claude apps gateway serves a machine-readable
  version of the contract — endpoints, headers to forward, feature pass-through. Diffing
  against it in CI turns "test against new Claude Code releases rather than pinning to an
  observed list" into an automated check. Test dependency only.
- **The silent failures are the dangerous ones.** A stripped `anthropic-beta` value makes a
  capability *quietly unavailable*; a reordered `system` array poisons the prompt cache with
  no error; a failed `/v1/models` discovery falls back silently. None of these throw. All
  need explicit assertions.
- **Both existing dormant assertions land here too.** `testbed/30-enforce.sh` §A (raw-rego
  deny) and §A3 (halt → turn stop + latch) have been waiting on a stack since ADR-0017 and
  ADR-0020. Run them in the same pass.
- **Backend asks are deliverables, not footnotes.** Retention for body-class volume and
  server-side dedupe on developer events both block at the backend and have been open since
  ADR-0019.

## Requirements

1. Testbed phase driving a real session through the real LOCAL gateway (daemon-installed
   by `openbox init`) to a real core.
2. CI conformance against `GET /protocol`, run per Claude Code release.
3. Assertions for the silent failures: beta passthrough, system-array identity, discovery.
4. Session join proven: ONE session row receives both producers' events (hook activities +
   gateway spans); header agent ids join to hook-side agent ids.
5. Account binding end to end: core HALT on a non-org credential fingerprint stops a real
   model call; no retry.
6. Bypass visibility (detection tier): with gateway config removed, the session's stored
   data shows turns without gateway spans — the mismatch evidence is queryable; `doctor`
   flags the bypass-capable config. With config present but the daemon stopped, zero model
   calls succeed (fail-closed-by-accident direction).
7. Track A verified independently of Track B — phases 01–02 must pass with no gateway.
8. Backend asks filed with measurements: retention for body volume, server-side dedupe,
   account registry, session-vs-spans mismatch alert.
9. Docs reconciled to what the run actually proved.

## Architecture

New `testbed/45-gateway.sh` alongside the existing phases, reusing the harness — 45 because
`40-approvals.sh` already owns the 40 slot (collision caught in plan review). Track A gets
assertions inside the existing `10`/`20` phases so it is provable without a gateway.

The `/protocol` diff runs in CI against a pinned Claude apps gateway version, failing on a
contract change rather than on a broken session weeks later.

## Related code files

| Path | Change |
|---|---|
| `testbed/45-gateway.sh` | new (40 slot is taken by approvals) |
| `testbed/10-onboard.sh`, `20-*.sh` | Track A assertions |
| `testbed/30-enforce.sh` | run dormant §A and §A3 |
| `.github/workflows/` | `/protocol` conformance job |
| `docs/testbed/e2e.md` | new phase documented |
| `CLAUDE.md`, `docs/architecture.md` | current state after the run |

## Implementation steps

1. Track A assertions in existing phases; run without a gateway.
2. `45-gateway.sh`: `init`-installed daemon → session → gateway → core; assert headers,
   bodies, thinking, classification, fingerprint-present/raw-absent, ONE session row for
   both producers, agent ids join, no duplicate rows.
3. Silent-failure assertions: unknown beta value survives; system array unchanged; `/v1/models`
   returns without redirect.
4. Enforcement: a policy denial stops a real model call, no retry; then the account case —
   register org fingerprints, run a session under a non-org credential, assert HALT stops
   the call.
5. Bypass visibility: remove the env config ⇒ session runs direct; assert stored
   turns-without-spans mismatch is queryable and `doctor` flags it. Stop the daemon with
   config present ⇒ assert zero successful model calls.
6. Run the dormant `30-enforce.sh` §A and §A3.
7. CI `/protocol` diff job.
8. Measure volume and latency; file the backend asks (retention, dedupe, account registry,
   mismatch alert) with the numbers.
9. Reconcile docs to results — split claims by evidence strength, as prior verification
   reports in this repo do.

## Todo

- [ ] Track A assertions pass with no gateway
- [ ] `45-gateway.sh` green (daemon-installed, session join + fingerprint asserted)
- [ ] Silent-failure assertions
- [ ] Denial stops a real call, no retry
- [ ] Account HALT: non-org fingerprint stops a real call
- [ ] Bypass visibility: mismatch evidence queryable; doctor flags; daemon-stopped ⇒ zero calls
- [ ] Dormant §A and §A3 run
- [ ] CI `/protocol` job
- [ ] Volume + latency measured
- [ ] Backend asks filed (retention, dedupe, account registry, mismatch alert)
- [ ] Docs reconciled, verification report written

## Success criteria

- Full run green against a live local stack, gateway installed the way a user installs it.
- Zero stream aborts attributable to the gateway across the run.
- Headers, bodies and thinking present in `governance_events` for a real session; raw
  credential in zero stored rows, fingerprint present.
- One session row carries both producers' events; zero duplicate rows for turns both observe.
- A non-org credential is HALTed; a bypassed session leaves queryable mismatch evidence.
- CI fails on a `/protocol` contract change.
- A verification report splitting every claim by evidence strength.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| No stack reachable, again | secure the stack before phase 08 starts | phase blocked | do not mark phases verified; say testbed has NOT run, as CLAUDE.md does today |
| Silent failures pass unnoticed | explicit assertions per failure mode | testbed green, real sessions degraded | add the assertion that would have caught it |
| Volume unacceptable at real cadence | measured here before any wider rollout | ingest latency, storage growth | build the deferred body sink (phase-05 contingency, ADR-0021 amendment first); hold rollout |
| `/protocol` drifts between releases | CI job is the detector | job fails | update passthrough; never allowlist |
| Duplicate rows from two producers | collision test in phase 05, verified here | duplicated turns | server-side dedupe ask becomes blocking |

## Security considerations

- Testbed runs use real credentials against a local stack. Ensure fixtures never carry a
  real org key, and that captured bodies from test sessions are not retained past the run.
- Verify empirically that no credential header value reaches stored rows — the assertion
  that matters most, and the one a unit test cannot prove.

## Next steps

Reconcile `CLAUDE.md` and `docs/architecture.md`, then decide fleet rollout on the evidence.
