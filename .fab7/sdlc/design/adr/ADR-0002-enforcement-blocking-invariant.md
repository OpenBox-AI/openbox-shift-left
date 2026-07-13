# ADR-0002: Enforcement may block a tool call — the bounded carve-out to INV-3

## Status
accepted — G_ADR ratified by brian 2026-07-13 (S2 supplied the timeout; blocker cleared).

<!-- G_ADR gate (Epic E6, enforcement). Decision owner: brian. Records the
structural/invariant change Phase-2 forces: INV-3 ("observation never blocks")
is a Phase-1 invariant; enforcement deliberately blocks. This ADR defines the
scoped, bounded carve-out so "blocks by design" never becomes "hangs the dev
loop on an outage".

RATIFICATION (brian, 2026-07-13): the one open dependency — the concrete hook
timeout, which the "proposed" version deferred to spike S2 — is now supplied.
Spike S2 (DONE 2026-07-13) measured direct sync HTTP to /evaluate at ~0.8–1.6 s
(Temporal workflow, loopback floor) → NO-GO, and set the hook budget at ≈ 50 ms
(local sidecar target <10 ms), fail-open. With that number in hand INV-3b is
fully specified, so this ADR moves proposed → accepted. Ratified jointly with
ADR-0003 (the sidecar module the bounded wait depends on — OD6). E6-S7 remains
the conformance target that provides the evidence, but the carve-out is no longer
blocked on it. -->

## Context

Phase-1's **INV-3** is absolute: *no dev-runtime path denies, delays past budget,
or errors a developer tool call* — observation is async, fail-open, never on the
critical path. Every Phase-1 story (SL-1..SL-16) and the Advisory tier (SL-9,
which *records* `WouldBlock()` but never acts) upholds it.

Phase-2 enforcement (Epic E6) **intentionally violates INV-3**: to `deny`/`ask`/
rewrite a tool call, the `PreToolUse` hook must run **synchronously** (Claude Code
waits for it) and, on a `BLOCK`/`HALT` verdict, **stop the call**. You cannot both
"never block" and "enforce". So INV-3 as written cannot survive Phase-2 unchanged
— but dropping it wholesale would re-introduce exactly the failure it prevents (a
governance outage freezing every developer's tool call).

Forces:
- **OD9 (DECIDED, brian 2026-07-13): fail-open at first.** On core/sidecar
  unavailability or timeout, the call proceeds (degrade to observe). Per-org
  fail-closed is a later, explicit override (E6-S3), never the default.
- **OD6 (CONFIRMED by spike S2): command → local sidecar.** S2 (2026-07-13)
  measured a synchronous `POST /evaluate` at **~0.8–1.6 s** (Temporal workflow,
  loopback floor) vs ~1.5 ms fork/exec and ~3.6 ms signed local transport →
  **direct HTTP is a hard NO-GO**; the decision MUST be a local sidecar call
  (single-digit ms). This makes the bounded wait real, not aspirational.
- **NFR-2:** per-tool-call overhead must stay small; a slow enforce path kills
  adoption.
- **The Advisory seam already exists** (SL-9 `client.Evaluation` + `WouldBlock()`)
  — enforcement acts on the same verdict Advisory records; the change is the
  *response*, not the pipeline (arch D7).

## Decision

**Introduce INV-3b (enforcement carve-out), scoped and bounded, replacing INV-3
only for sessions/orgs with enforcement explicitly enabled:**

> **INV-3b:** An enforcement-enabled dev-runtime path MAY block or rewrite a tool
> call — but only at `PreToolUse` (pre-execution), only via a **synchronous
> decision bounded by a hard per-call timeout** (≈ 50 ms per spike S2; sidecar target <10 ms), and only
> subject to the org's failure policy. On timeout, decision-unavailable, or
> transport failure, the **default policy is fail-open** (allow; degrade to
> observe) (OD9); an org may opt into fail-closed (E6-S3). Enforcement is never
> a `PostToolUse` action (post-execution cannot be undone) and never blocks on a
> reason outside the bounded decision (no unbounded wait, no network on the hot
> path — the local sidecar answers, OD6).

- **INV-3 remains in force verbatim for observe-only and Advisory sessions** (the
  Phase-1 default and every non-enforced org). The carve-out is opt-in, per the
  observe→advisory→enforce tiering (arch §1b).
- The bound has teeth: the hard timeout + fail-open default mean "blocks by
  design" can never degrade into "hangs the tool call indefinitely" — the worst
  case is a bounded delay then allow.

## Consequences

Enables:
- E6 can build real `deny`/`ask`/rewrite (`apply` leg) without silently breaking
  the Phase-1 safety contract — the change is explicit, reviewed, and scoped.
- A clean conformance target (E6-S7): assert that an enforced BLOCK denies the
  call AND that a sidecar-down / timeout case fails **open** (allows) within the
  bound.

Costs / new constraints:
- **Two safety postures now coexist** — observe/advisory (INV-3) and enforce
  (INV-3b). Every enforcement story must state which it runs under; the adapter
  must make the mode explicit (config flag, arch D7) and default to observe.
- The timeout value was a hard dependency on **spike S2**; S2 (DONE 2026-07-13)
  supplied it (≈ 50 ms hook budget, sidecar target <10 ms), so this dependency is
  now discharged and the ADR is ratified.
- Fail-closed (the org override) is a genuinely different risk profile (an outage
  *does* block the dev loop) — gated separately (E6-S3) with its own review, not
  enabled by this ADR.

Follow-on:
- Spike **S2** (DONE 2026-07-13) supplied the timeout + the sidecar-vs-HTTP
  decision (OD6): direct HTTP is a ~1.6 s NO-GO, a local sidecar is mandatory.
  The sidecar this carve-out's bounded wait depends on is recorded in **ADR-0003**
  (ratified jointly with this ADR).
- **OD-ENF-SCOPE** (which verdicts/tools enforce first) and **OD-HITL** (does
  `REQUIRE_APPROVAL`/`ask` fit the hot path) are separate human decisions layered
  on top of this carve-out.

## Alternatives Considered

1. **Keep INV-3 absolute; no enforcement.** Rejected: that *is* "no Phase-2" —
   enforcement is the stated goal.
2. **Drop INV-3 entirely.** Rejected: removes the guardrail against an outage
   freezing the dev loop; fail-open-by-default (OD9) is the whole point.
3. **Enforce at `PostToolUse` (observe-then-react).** Rejected: post-execution
   cannot undo the side effect (S5) — it is not enforcement, only detection.
4. **Fail-closed by default.** Rejected by OD9 (fail-open first); fail-closed is
   an opt-in override, not the baseline — an infra failure must not block work.
5. **Synchronous direct HTTP to core (no sidecar).** **REJECTED by spike S2
   (2026-07-13):** `POST /evaluate` measured ~0.8–1.6 s (Temporal workflow) even on
   loopback — ~16–33× budget. A local sidecar (single-digit ms) is mandatory;
   `/evaluate` stays the async telemetry channel.
