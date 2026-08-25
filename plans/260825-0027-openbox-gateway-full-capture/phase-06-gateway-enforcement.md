# Phase 06 — Gateway enforcement

## Context links

- Parent: [plan.md](plan.md) · Previous: [phase-05](phase-05-gateway-capture-pipeline.md)
- HALT probe: phase 03 probe A
- Prior art: `adapters/common/hookflow/gate.go`, `sessionhalt.go`, ADR-0020
- Depends on: 05, and probe A's answer

## Overview

- Date: 2026-08-25
- Description: call `/evaluate` before forwarding and refuse on verdict — the one capability
  no telemetry route can provide.
- Priority: P1
- Implementation status: pending
- Review status: not reviewed

## Key insights

- **Refusal is the only lever.** Inspect-without-modifying forbids sanitising a request; the
  gateway can forward it or refuse it, nothing between. Guardrail-redaction-at-source stays
  unreachable at this layer and the docs must keep saying so.
- **HALT has no native rendering.** There is no `ask` at the model layer. The refusal shape
  comes from probe A, chosen so Claude Code does not treat it as a capability rejection —
  its retry logic matches on upstream **error wording**, and a wrong shape makes a policy
  denial look like a transient failure to retry around.
- **Latency budget is bounded by watchdogs, not taste.** 180s byte-level on the direct API,
  300s event-level. A verdict round-trip must sit far inside that, and the connection must
  keep emitting bytes if it holds.
- **Do not gate every call.** ~52 model calls per turn window measured. Per-call synchronous
  evaluation is a round-trip per call. Gate what policy asks for; observe the rest.
- **The approval hold is unproven.** SSE pings could hold a connection while an approval
  resolves, but the event-level watchdog wants *parsed events*, not just bytes. Treat as
  optional and prove it before depending on it.
- **Reuse `hookflow`'s cascade shape, not its code path.** The gate sequence, failure policy
  ordering, and `ApplyFailurePolicy`-runs-after-evaluation lesson all transfer. Re-deriving
  them here is how the pre-ADR-0017 fail-closed bug comes back.
- **Account binding costs this phase nothing.** A core HALT on a non-org credential
  fingerprint (phase 05 evidence, core-side registry) arrives as an ordinary verdict and
  renders through the same refusal path. Build no account-specific machinery here — no
  local allowlist, no cached account state. The testbed case lands in phase 08.

## Requirements

1. `/evaluate` called before forwarding, for calls policy marks gated.
2. HALT/BLOCK → refuse with the probe-A shape; ALLOW → forward unchanged.
3. `ApplyFailurePolicy` runs **after** evaluation, never before.
4. Unreachable `/evaluate` ⇒ **refuse the gated call** — owner decision, validated
   2026-08-25, regardless of `fail_closed`. A deliberate divergence from the hook path's
   posture-driven behavior; ungated calls still forward with no round-trip. There is no
   offline grace, by design.
5. Ungated calls add no round-trip.
6. Refusal reason surfaces to the developer legibly.

## Architecture

Gate sits between capture and forward. The request is already teed, so evaluation reads the
captured copy while the forward waits — the only place in this design where forwarding is
deliberately delayed.

Approval holds are out of scope for v1 unless probe A2 proves the ping mechanism. Without
it, REQUIRE_APPROVAL renders as refusal with the approval reference in the reason — the same
choice Codex made for `ask` (OD-SL7-ASK), and for the same reason: a fallthrough that
proceeds ungoverned is worse than an over-ask.

## Related code files

| Path | Change |
|---|---|
| `gateway/gate.go` | new — evaluate → forward \| refuse |
| `gateway/refuse.go` | new — probe-A refusal shape |
| `adapters/common/hookflow/failurepolicy.go` | reuse; do not fork |
| `docs/adr/ADR-0021-openbox-local-gateway.md` | enforcement semantics |
| `docs/architecture.md` | Approvals + assurance sections |

## Implementation steps

1. Wire `/evaluate` on the gated path only; measure ungated latency stays flat.
2. Implement the refusal shape from probe A; test against a real Claude Code session that
   the refusal does not trigger retry.
3. Failure policy **after** evaluation — port the ordering lesson explicitly, with a test
   that fails if it moves. Under always-refuse this test is a **merge-blocker**: a
   synthesized refusal firing before an evaluation attempt turns every core blip into a
   full model-call outage.
4. Reason surfacing: developer sees why, not a bare 4xx.
5. Optional: probe A2 for the ping-based approval hold. If it fails, refuse and move on.

## Todo

- [ ] `/evaluate` on gated path
- [ ] Refusal shape implemented and verified against a live session
- [ ] Failure-policy ordering test (fails if reordered)
- [ ] Ungated latency unchanged
- [ ] Reason legible to the developer
- [ ] Probe A2 run, result recorded either way
- [ ] Docs: enforcement semantics + what refusal cannot do

## Success criteria

- A policy denial stops the model call; the session does not retry around it.
- Verdict round-trip p99 < 5s, well inside the 180s byte watchdog.
- Ungated path adds < 50ms p95 versus phase 04's baseline.
- Unreachable `/evaluate` ⇒ gated calls refused regardless of `fail_closed`; ungated calls
  unaffected. Both behaviors tested; refusal reason names the outage, not a policy denial.
- No synthesized HALT before an evaluation has run.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| Refusal reads as transient; CC retries | probe A drives the shape; verify on a live session | repeated identical requests after a denial | change shape; if none works, descope to observe-only |
| Failure policy runs before evaluation | ordering test that fails on reorder | fail-closed org denies everything without asking | this exact bug shipped once; the test is the control |
| Per-call gating tanks latency | gate only what policy marks | turn latency grows with call count | narrow the gate, never cache verdicts locally |
| Approval hold trips the event watchdog | prove with A2 before depending | streams abort during holds | drop the hold; refuse with a reference |
| Local gateway dies mid-session | fail direction is closed-by-accident (dead localhost = no model calls); daemon supervisor restarts (phase 07) | model calls error against localhost | doctor names the cause; blast radius is one developer, not the fleet |

## Security considerations

- Refusing on verdict makes the gateway a denial-of-service surface against its own
  developer. A bug that refuses everything is indistinguishable from an outage — but the
  blast radius is one machine, and `openbox doctor` (phase 07) distinguishes "policy
  refused" from "gateway dead" so the developer isn't debugging blind.
- The gateway sees full conversations before deciding. Evaluation payloads are content and
  ride the same content gate — the gate applies to what OpenBox *sends onward*, not to what
  it transiently holds.
- Do not add local verdict caching. The `ShouldEscalate`-only lesson from ADR-0017 applies:
  the engine second-guessing the decider is how raw-rego orgs went ungoverned.

## Next steps

Phase 07 distributes it and makes bypass fail.
