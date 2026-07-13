# Spike S2 — Enforcement decision latency & the local-sidecar mechanism

**Question (one sentence):** Can a synchronous `PreToolUse` enforcement decision be made fast enough (and safely enough) on a developer's machine to block/rewrite a tool call within an acceptable per-call budget, and does that require a local decision sidecar?

**Status:** DONE (2026-07-13) — **decisive: direct synchronous HTTP to `/evaluate` is INFEASIBLE for enforcement (~0.8–1.6 s decision); a local sidecar is MANDATORY.** Confirms **OD6 = command→local sidecar** with measured evidence. Findings at the bottom. Owner of the decision: brian.
**Method (planned):** measure real `dev-machine → openbox-core /evaluate` round-trip latency (LAN, home broadband, and the hybrid **VPN** path we run today); prototype a resident local sidecar answering over a Unix domain socket with a cached/OPA-bundle policy; measure sidecar decision latency; validate the fail-open-on-timeout behavior end-to-end in a real Claude Code session.
**Convention:** [E]=cited/measured fact, [A]=applied reasoning.

---

## Enforcement mechanism (SETTLED — what S2 must validate, not rediscover)

Two OD6 questions were raised ("how to handle the delay if the tool already ran?" and "how to revert/block?"). The Claude Code hook model answers both — enforcement is a **pre-execution** decision, so there is nothing to revert:

- **`PreToolUse` fires BEFORE the tool executes and Claude Code waits for it (synchronous).** [E, architecture §1b capability matrix: Claude Code `enforce.decision ✅ deny/ask`, `enforce.rewrite ✅ updatedInput`; S1 A1.] The hook returns a permission decision: **`deny`** (tool never runs; reason fed back to Claude), **`ask`** (prompt the developer), **`allow`** (proceed), or a rewritten **`updatedInput`** (redact/mutate the input before it runs). → **Blocking = returning `deny` at `PreToolUse`. No revert exists or is needed — the side effect never happens.**
- **`PostToolUse` fires AFTER execution and CANNOT undo it** (the file is written, the command ran). [E, S5: "PostToolUse can't undo."] This is exactly why the enforcement decision MUST be pre-execution. Post-hoc realizations can only flag/record and gate *follow-on* steps.
- **The "delay" is the synchronous wait for `PreToolUse`.** Today (Phase-1) that hook is observe-only — it spools and returns immediately (non-blocking, no network on the hot path, INV-3). Enforcement flips the SAME hook to a synchronous decision (arch D7 — "same channel, same events; only the client's response to the verdict changes"). The cost is real per-call latency, which is the whole reason for a **local sidecar** and a **fail-open timeout**:
  - **OD6 (brian's preference): command → local sidecar.** The per-call `PreToolUse` invocation asks a **resident local daemon** (Unix socket, ~sub-ms/low-ms) instead of doing a network round-trip to core (tens–hundreds ms, worse over VPN). The sidecar holds the policy/OPA bundle (arch R3 — OPA bundles are already distributed) and syncs it from core **out-of-band**, off the hot path.
  - **OD9 (DECIDED — fail-open at first):** the hook sets a **tight internal timeout**; if the sidecar is down, slow past the budget, or the decision is unavailable → **allow** (degrade to observe). An infra failure NEVER blocks the developer. This bounds worst-case added latency to the timeout.

**So S2 is not "is enforcement possible" (it is) — it is "what is the latency budget, does the sidecar meet it, and what timeout keeps fail-open honest."**

## Why it blocks E6
- **E6-S1/S2** (synchronous evaluate + `apply`) need the measured budget + the sidecar-vs-direct-HTTP answer to be built correctly.
- **OD6** (HTTP vs command→sidecar) cannot be finalized without the latency numbers.
- Getting this wrong violates **NFR-2** (per-tool-call overhead) and, on a slow path, would make every tool call sluggish — the fastest way to kill pilot adoption.

## Bounded questions to answer
1. **Baseline network latency:** p50/p95/p99 of a signed `POST /evaluate` from a dev machine to core — on LAN, on home broadband, and over the **hybrid VPN** (`identity/opa/guardrails.node.lat` + core). Is a direct synchronous HTTP call ever within an acceptable per-tool-call budget?
2. **Acceptable budget:** what added per-tool-call latency is tolerable before it degrades the dev loop? (Propose a target, e.g. p95 ≤ 50–100 ms; confirm with brian.)
3. **Sidecar decision latency:** prototype the resident sidecar (Unix socket + OPA bundle / cached policy); measure decision p50/p95. Does it comfortably beat the budget?
4. **Policy freshness vs latency:** how does the sidecar stay current (pull interval / push) without putting core on the hot path? Staleness tolerance?
5. **Fail-open timeout:** what hard per-call timeout keeps fail-open (OD9) honest — small enough that a hung sidecar doesn't stall a tool call, large enough not to spuriously allow under normal load?
6. **`ask` vs `deny` UX:** does `ask` (interactive prompt) add unacceptable friction vs `deny` for the first enforced scope?
7. **Sidecar lifecycle:** who starts/owns the daemon (per-user systemd/launchd? spawned by `dev init`?), and how does it fail safe if absent (→ fail-open)?

## Deliverable
- A latency table (network paths + sidecar) vs the proposed budget.
- A go/no-go on **direct-HTTP vs command→local-sidecar** (the OD6 recommendation, with evidence).
- A recommended per-call **timeout** + the fail-open behavior, validated in a real Claude Code session (a BLOCK verdict denies a tool call; sidecar-down → the same call proceeds, observe-degraded).
- A sidecar lifecycle sketch (start/own/sync/fail-safe) for E6-S5.

## Decisions it informs (owner: brian — NOT chosen here)
- **OD6** — hook handler type: direct synchronous HTTP vs command→local sidecar (brian's stated preference; S2 confirms/quantifies).
- Feeds **OD-ENF-SCOPE** (which verdicts/tools go live first) and the **INV-3 carve-out ADR (ADR-0002)** (the timeout + fail-open semantics are the carve-out's teeth).

## Related
- **OD9 DECIDED (brian, 2026-07-13): fail-open at first** — enforcement degrades to allow on core/sidecar unavailability or timeout; per-org fail-closed is a later override (E6-S3).
- Architecture R3 (Phase-2 latency → this spike; local OPA sidecar candidate), OD6, D7 (observe→enforce flip), capability matrix (CC enforce.decision/rewrite). Reuse target: the existing OPA bundle distribution + Guardrail API (do not build new).

---

## FINDINGS — run 2026-07-13 (decisive)

Measured against the running local hybrid stack (core `:8086` **loopback** + governance/attestation workers + Temporal; UAT Guardrail/OPA falling back, `age_fallback_used=true`). Harness: the real engine binary + signed client, N=25 (`scratchpad/s2.py`).

| Path | p50 | p95 | p99 | What it is |
|---|---|---|---|---|
| **T_bin** — `PreToolUse` fork/exec + spool (no net) | 1.5 ms | 1.6 ms | 1.6 ms | the per-tool-call floor paid on EVERY call regardless |
| **T_verify** — signed `GET /auth/validate` (no workflow) | 3.6 ms | 4.0 ms | 4.2 ms | signing + loopback RTT + a simple handler |
| **T_flush** — signed `POST /evaluate` + **full workflow** | **1597 ms** | **1639 ms** | 1651 ms | the governance DECISION via core (Temporal orchestration) |

*(T_flush = a minimal-session flush of 1–2 events; per `/evaluate` ≈ **0.8–1.6 s**. Core is LOCAL loopback → this is the **FLOOR**; a real dev→core hop adds network RTT on top. min 916 ms / max 1651 ms.)*

### Decisive conclusions
1. **Direct synchronous HTTP to `/evaluate` is INFEASIBLE for enforcement.** The decision costs **~0.8–1.6 s** (dominated by the Temporal governance workflow, not the network) — **~16–33× a tolerable per-tool-call budget (~50–100 ms)**, and that's the loopback floor. Blocking a tool call on this would make every Bash/Edit/Read visibly stall for a second-plus. **NO-GO.**
2. **A local sidecar is MANDATORY (not merely preferred) — OD6 confirmed with hard evidence.** The decision must be made LOCALLY from a synced policy/OPA bundle, NOT by proxying to `/evaluate`. Achievability is proven by the other two rows: signing + local transport ≈ 3.6 ms, fork/exec ≈ 1.5 ms — a local OPA decision belongs in that single-digit-ms band, ~1000× faster than the workflow round-trip. **GO, mandatory.**
3. **`/evaluate` stays the ASYNC telemetry/observe channel** (fire-and-forget, as today) — it is NEVER on the synchronous decision path. The sidecar emits to it out-of-band.
4. **Timeout + fail-open:** PreToolUse hard timeout ≈ **50 ms** (sidecar target <10 ms); on timeout / sidecar-absent → **allow** (OD9 fail-open). The 1.6 s path must never gate a tool call.
5. **Redaction (E6-S4) must be local too.** Applying core's `redacted_input` via a round-trip hits the same ~1.6 s wall → infeasible synchronously. Guardrail redaction must run **in the sidecar** (local model / regex bundle) or be accepted as **async-advisory-only**. Flag for E6-S4.
6. **The per-call binary fork/exec (1.5 ms) is a non-issue** — NFR-2 is met by the engine itself; the risk was always the decision round-trip, now quantified.

### Impact on Epic E6
- **E6-S5 (local sidecar) is promoted from *conditional* to a REQUIRED prerequisite**, alongside/ahead of E6-S1 (the sync path targets the sidecar, not core).
- **E6-S1** = PreToolUse → local sidecar (Unix socket), NOT a direct `/evaluate` call.
- **ADR-0002** timeout is now concrete: ~50 ms hook budget, sidecar <10 ms target.
- Follow-on measurement (deferred, not blocking): sidecar OPA-bundle decision latency once the daemon exists (expected single-digit ms); real dev→core RTT for the async telemetry emit (non-critical — fire-and-forget).
