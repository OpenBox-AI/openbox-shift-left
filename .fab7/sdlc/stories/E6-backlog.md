# Epic E6 — Enforcement (the `apply` leg): story backlog

**Author:** planning (2026-07-13) — drafted after the Phase-1 review (SL-1..SL-16 + Advisory SL-9 all done).
**Epic:** E6 (CLAUDE.md: "E6 Phase-2 enforcement blocked on spike S2") — flip the developer runtime from **observe/advisory** to **enforce**: honor the governance verdict by denying / asking / rewriting a tool call, subject to a bounded, fail-open-by-default policy.
**Status:** UNBLOCKED to build (2026-07-13). Spike S2 DONE; all ODs ruled (OD6/OD9/OD-ENF-SCOPE/OD-HITL); **ADR-0002 (INV-3b) + ADR-0003 (sidecar module) ratified** (G_ADR accept, brian); the **EXT-core upstream merge landed** (openbox-core `c18bbc8`, branch `feat/ext-core-dev-runtime-event-types`). Every prerequisite blocker is cleared — E6-S5 → E6-S1 … are worker-ready.

## The one architectural truth
Enforcement **inverts INV-3** ("observation never blocks"). To deny a tool call, `PreToolUse` must run **synchronously** (pre-execution, Claude Code waits) and stop the call on BLOCK/HALT. This is codified as the scoped carve-out **INV-3b (ADR-0002)**: enforce-enabled paths may block, but only pre-execution, only within a **hard timeout**, and **fail-open by default** (OD9). Observe/advisory sessions keep INV-3 verbatim. The mechanism is `PreToolUse` `permissionDecision` (`deny`/`ask`/`allow`) + `updatedInput` (rewrite) — **there is no "revert"; the decision is made before the side effect** (S2 §"mechanism settled").

## Decisions baked in
- **OD9 = fail-open at first** (DECIDED, brian 2026-07-13). On core/sidecar unavailable or timeout → allow (degrade to observe). Per-org fail-closed = later opt-in (E6-S3).
- **OD6 = command → local sidecar — CONFIRMED MANDATORY by spike S2** (2026-07-13). S2 measured `POST /evaluate` at **~0.8–1.6 s** (Temporal workflow) even on loopback — ~16–33× budget → **direct synchronous HTTP is NO-GO**. The decision MUST be local (Unix socket, OPA bundle, single-digit ms). `/evaluate` stays the **async telemetry** channel only.
- **OD-ENF-SCOPE = replicate the full SDK verdict scope** (DECIDED, brian 2026-07-13). Enforce the SDK's complete priority set — **HALT > BLOCK > guardrails > REQUIRE_APPROVAL > CONSTRAIN > ALLOW** (port `verdict_handler.enforce_verdict`) across **all tool calls** (Bash/file/Read/MCP — the dev-runtime analog of the SDK's HTTP/DB/file/function instrumentation). No reduced first-scope.
- **OD-HITL = map REQUIRE_APPROVAL → CC `ask`** (DECIDED, brian 2026-07-13). Interactive local prompt (not server-side `/approval` polling). E6-S6 is now in-scope (not conditional).
- **INV-3b** (ADR-0002, **accepted** 2026-07-13) governs the whole epic; S2 set its hard timeout ≈ **50 ms** (sidecar target <10 ms).
- **Per-call floor is a non-issue** — S2 measured the engine fork/exec + spool at **1.5 ms** and signing+local transport at **3.6 ms**.

## Reused, already-built seams (Phase-1 → Phase-2)
- **SL-9 `client.Evaluation` + `WouldBlock()`** — the verdict is already parsed + recorded; enforcement *acts* on the same value (`WouldBlock()` → an actual block). Same pipeline, different response (arch D7).
- **SL-15 fail-closed `apiVerifier` pattern** — the model for the fail-closed *attribution* path; E6-S3's fail-closed *enforcement* policy mirrors its discipline.
- **SL-13 EXT-core artifact** — makes the hard dependency (below) PR-ready.
- **SL-11 `dev verify` / signed GET** — the connectivity/preflight pattern the sidecar's core-sync reuses.

## Hard dependency (was "assumed-satisfied", now a Phase-2 blocker) — ✅ MERGED
- **[EXT-core] — MERGED 2026-07-13** (openbox-core commit `c18bbc8`, branch `feat/ext-core-dev-runtime-event-types`; 3 additive governance edits, byte-identical to the SL-13 artifact `contracts/dev-event/ext-core/`; build + vet green). Core now accept-lists the 7 developer event types, so a verdict can actually be evaluated on them instead of `400 invalid event_type`. The "assumed-satisfied" caveat is retired. Live acceptance re-run (`contracts/dev-event/acceptance/`) is a stack-up formality — the pipeline was already live-verified end-to-end during SL-15. Remaining upstream step (outward-facing, brian to run): push the branch + open the PR against OpenBox-AI/openbox-core.

---

## Stories (drafted; not worker-ready until S2 + ODs + ADR-0002)

### E6-S0 — Spike S2: enforcement latency & sidecar mechanism  *(gate 0 — blocks everything)*
- **Goal:** measure dev→core `/evaluate` latency (incl. VPN) + prototype the local sidecar; produce the per-call **timeout budget** and the **HTTP-vs-sidecar** recommendation (OD6).
- **Artifact:** `.fab7/sdlc/discovery/spikes/S2-enforcement-latency.md` (written). **Blocks:** all of E6.

### E6-S1 — Synchronous evaluate path (enforce mode)
- **Goal:** a new **enforce mode** where `PreToolUse` obtains a decision *before* the tool runs — via the local sidecar (OD6) — and returns the `client.Evaluation`. Reuses SL-9's `Evaluation`; SL-15's fail-closed error handling is the model. The current async observe spool path is untouched for observe/advisory sessions.
- **Write scope:** `adapters/claude-code/`, likely a new sidecar client. **Deps:** E6-S0, SL-9. **Gates:** G3, **G_SEC**. **Invariants:** INV-3b, INV-1.

### E6-S2 — `apply(verdict)` on the Claude Code adapter
- **Goal:** map `Evaluation.Verdict` → CC `permissionDecision`: `BLOCK`/`HALT` → `deny` (+ reason); `REQUIRE_APPROVAL` → `ask` (pending OD-HITL); `CONSTRAIN` → allow-with-constraints logged; `ALLOW` → allow. Guardrail redaction → `updatedInput` (E6-S4). The observe→enforce **flip is a config flag** (arch D7); default observe. `WouldBlock()` becomes the real block.
- **Write scope:** `adapters/claude-code/`. **Deps:** E6-S1. **Gates:** G3, **G_SEC**. **Invariants:** INV-3b (blocks only pre-execution, bounded), INV-2.

### E6-S3 — Fail-closed policy engine (per-org override + timeout)
- **Goal:** the failure policy — **fail-open default** (OD9); per-org opt-in **fail-closed**; the hard per-call **timeout** (from S2). On timeout/unavailable → the policy decides (open=allow, closed=deny). Mirrors the SDK's `governance_policy`.
- **Write scope:** `adapters/claude-code/` (+ config). **Deps:** E6-S1, S2. **Gates:** G3, **G_SEC** (fail-closed is a distinct risk profile — an outage blocks work). **Invariants:** INV-3b.

### E6-S4 — Guardrail redaction application  *(gated on content posture)*
- **Goal:** apply `Evaluation.Guardrail` redacted input to the tool input before it runs (via `updatedInput`) — port the SDK's `_apply_input_redaction`. **Only meaningful when content-capture is on** (OD4); reuse the existing Guardrail API (do not build new — S4 §4).
- **Write scope:** `adapters/claude-code/`. **Deps:** E6-S1, content-capture posture (OD4/OD-FINOPS lineage). **Gates:** G3, **G_SEC**. **Invariants:** INV-2, INV-3b.

### E6-S5 — Local decision sidecar  *(REQUIRED — spike S2 promoted this from conditional; build FIRST, with/before E6-S1)*
- **Goal:** the resident daemon E6-S1 calls (Unix socket, OPA bundle, out-of-band core sync, fail-safe-absent → fail-open); lifecycle (start/own/sync). **S2 proved direct HTTP is a ~1.6 s NO-GO, so this is the only viable decision path** — not optional. Target single-digit-ms local decision.
- **Write scope:** new top-level `sidecar/` module (**home ratified — ADR-0003**), invoked as `openbox sidecar serve`. **Deps:** E6-S0. **Gates:** G3, **G_SEC**, ~~G_ADR~~ **(G_ADR pre-cleared — ADR-0003 accepted 2026-07-13; build-time G_SEC/G3 still required).** **Invariants:** INV-3b, INV-1.

### E6-S6 — REQUIRE_APPROVAL → CC `ask`  *(IN SCOPE — OD-HITL decided)*
- **Goal:** map `REQUIRE_APPROVAL` → CC's `ask` (interactive local prompt), per OD-HITL (brian 2026-07-13). NOT the SDK's server-side `/approval` polling (too heavy for the hot path). Part of the full-SDK-scope verdict handling (OD-ENF-SCOPE).
- **Deps:** E6-S2. **Gates:** G3.

### E6-S7 — Enforcement conformance + INV-3b evidence
- **Goal:** a conformance suite proving: an enforced BLOCK **denies** the call (pre-execution); a sidecar-down/timeout case **fails open** within the bound (OD9); observe/advisory sessions still uphold INV-3 verbatim; fail-closed (when opted in) denies on outage. Finalizes **ADR-0002**.
- **Write scope:** `adapters/claude-code/` (+ conformance). **Deps:** E6-S1..S3. **Gates:** G3 (**ADR-0002 already ratified 2026-07-13 with the S2 50 ms timeout; this story supplies the conformance *evidence*, not the ratification**). **Invariants:** INV-3b.

---

## Sequencing (S2 DONE + all ODs ruled → E6 is unblocked to build)
```
E6-S0 (spike S2) ✅ DONE  +  ODs ruled (OD6/OD9/OD-ENF-SCOPE/OD-HITL) ✅
   ─► E6-S5 (local sidecar — REQUIRED, build first) ─► E6-S1 (PreToolUse → sidecar, sync)
   ─► E6-S2 (apply: full-SDK verdict scope; ask/deny/rewrite) ─► E6-S3 (fail policy: open default + 50ms timeout)
   ─► E6-S4 (redaction — must be LOCAL in the sidecar, if content on) ─► E6-S6 (REQUIRE_APPROVAL→ask)
   ─► E6-S7 (conformance + ratify ADR-0002 with the 50ms timeout)
```
- **All prerequisite blockers cleared (2026-07-13):** the EXT-core upstream merge landed (openbox-core `c18bbc8`) and **G_ADR is accepted** on ADR-0002 (INV-3b) + ADR-0003 (sidecar module). E6-S5 (local sidecar) is ready to build.

## Open human decisions (owner: brian)
| ID | Decision | State |
|---|---|---|
| **OD9** | fail-open vs fail-closed default | **DECIDED: fail-open at first** (2026-07-13) |
| **OD6** | hook handler type (HTTP vs command→sidecar) | **CONFIRMED: command→local sidecar** — spike S2 proved direct HTTP is ~1.6 s (NO-GO) |
| **OD-ENF-SCOPE** | which verdicts / tools enforce | **DECIDED: full SDK scope** — HALT>BLOCK>guardrails>REQUIRE_APPROVAL>CONSTRAIN>ALLOW, all tools (2026-07-13) |
| **OD-HITL** | REQUIRE_APPROVAL fit | **DECIDED: map → CC `ask`** (2026-07-13) |
| **ADR-0002** | INV-3b carve-out (enforcement may block, bounded, fail-open default, ~50 ms timeout) | **ACCEPTED** (G_ADR, brian 2026-07-13; S2 supplied the timeout). E6-S7 remains the conformance evidence, no longer a ratification blocker. |
| **G_ADR / ADR-0003** | new sidecar module/resident component | **ACCEPTED** (brian 2026-07-13). ADR-0003 records the home: new top-level `sidecar/` module, invoked as `openbox sidecar serve` (WIRE-2 one-binary preserved). |
