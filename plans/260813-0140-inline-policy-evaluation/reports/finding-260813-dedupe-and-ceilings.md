# Finding — dedupe under universal escalation + Codex ceiling

Phase: [phase-01](../phase-01-gate-dedupe-and-ceilings.md) · Date: 2026-08-13 · Branch: `feat/inline-policy-evaluation`

## Verdict: **PROCEED-WITH-FIX**

One double-store path is **confirmed open** and must be fixed before phase 3 widens
escalation. Codex's ceiling is **not unknown** — the phase premise was stale. The
empirical row-count (requirement 1) is **still owed**: no local stack was reachable.

| Req | Answer | Evidence strength |
|---|---|---|
| 1. `ActivityStarted` per gated call, universal escalation, real run | **not obtained** | blocked — no stack |
| 1b. Suppression mechanism + whether it is class-independent | delivered-path holds; timeout path does **not** | `-race` test on real code |
| 2. Codex hook ceiling + source + configurability | 30s installed, self-imposed, configurable | repo source, cited |
| 3. Finding written incl. negative case | this file | — |

## Q1 — dedupe

### Mechanism (step 1, from `a901969`)

Client-side suppression, at the **gate** layer, keyed on **transport delivery**:

- `gate.go:77-85` — `delivered` flag, `Tier2.OnDelivered` sets it, a `defer` spools the
  observe copy only `if !delivered`. Value receiver, so no cross-process shared state.
- `tier2.go:117-127` — `OnDelivered` fires after `cl.Emit` returns **nil error**, before
  verdict mapping. Keyed on transport, not verdict — deliberate: a delivered-but-unusable
  verdict and a delivered REQUIRE_APPROVAL both still stored the event.
- Default direction is **spool** (redundant copy is a bug, missing copy is lost telemetry).

**Server-side dedupe is absent.** `a901969`'s own message says so: "core does not dedupe
developer events on that id" and "Server-side dedupe remains absent, so the lost-200 retry
path can still double-store." This contradicts phase-01's Key Insight that core's
`event_type`-bearing dedupe key "says it *should* hold" — that key governs *activities*,
not developer-event ids. **The client-side suppression is the only protection.**

### Class-independence: holds

Nothing in the suppression consults risk class. Removing `t.HighRisk()` at `gate.go:124`
changes *which* calls escalate, not how the flag is computed. On the delivered path,
universal escalation is structurally identical to today.

### The open path: budget-exceeded escalation — CONFIRMED

`Escalate` (`tier2.go:102-108`) returns via `<-cctx.Done()` when the budget expires while
the POST is in flight. Two defects, both reproduced against unmodified source with a
scratch test (`-race`, since deleted — the phase sanctions an uncommitted instrument):

1. **Data race.** Write `gate.go:79` from the transport goroutine vs. read `gate.go:81` in
   the defer. The `<-resultCh` path has a happens-before edge through the channel; the
   `<-cctx.Done()` path has **none**. Race detector output names both lines.
2. **Double store.** Budget expires → gate spools the observe copy → the in-flight POST
   still lands → core stores it too. Test logged `spooled: true` with the escalation
   having delivered. **Two `ActivityStarted` rows, two Merkle leaves, one tool call.**

**This is live today**, not introduced by this plan: Bash/MCP already escalate. What the
plan changes is blast radius — from high-risk-only to 100% of gated calls, which is
precisely phase-01's "tolerable at 5%, not at 100%".

**Honest limit on the reproduction.** The fake `Emit` ignores `ctx`, so it proves the
*mechanism* and the race, not the *frequency*. In production `cl.Emit` is cancelled with
`cctx` (`defer cancel()`), so the real window needs core to commit the row before
cancellation lands — the classic lost-200. Narrower than the test, not closed by it. The
frequency question needs the stack run.

### Fix — **landed** as `eb53827`, standalone (OD: operator chose "its own change, now")

`delivered` is an `atomic.Bool`; the read is defined rather than racing. Behaviour on every
settled path is unchanged. The regression test takes the timeout branch and pins both
halves; negative control (reverting the atomic) reproduces the race and fails only that
test. All 11 modules build/vet/`-race` green.

**The double-store itself is only narrowed, not closed.** The race is gone, but the
lost-200 window remains and is irreducible client-side while server-side dedupe is absent —
core committing the row after our cancellation is indistinguishable from never storing it.
The spool-on-unknown direction is retained deliberately. **This is the standing reason
phase 3 must not treat universal escalation as duplicate-free**, and it is a backend ask,
not work this repo can finish.

### Not obtained (steps 2-4) — accepted, deferred to manual verification

No stack: `docker ps` shows only unrelated containers; all four endpoints
(`:8086`, `:3000`, `:8181`, `:3233`) return connection-refused. Per the phase's pre-decided
response, recorded rather than substituted with code reading.

**OD:** the operator waived the stack run and will verify manually at the end. Still owed,
now against phase 8 / that manual pass: the real per-call row count under universal
escalation, and the mid-call-failure retry case. Everything this report says about the
delivered path is **structural, not observed** — the plan proceeds on that basis knowingly.

## Q2 — Codex hook ceiling

**The phase premise is stale.** "Not recorded anywhere in this repo" is wrong; it is
derived, documented and its overrun behaviour observed.

| Fact | Value | Source |
|---|---|---|
| Codex default hook timeout | 600s | `adapters/codex/installer.go:38` |
| Installed on PreToolUse (the gating hook) | **30s** | `installer.go:49` `preToolUseHookTimeoutSec` |
| Installed on hot hooks (SessionStart, UserPromptSubmit, PostToolUse) | 5s | `installer.go:48` `hotHookTimeoutSec` |
| Whole-hook enforce budget | 29s (installed − 1s margin) | `enforce.go:225-238` |
| Behaviour on overrun | kill, **fail open** — tool runs | `enforce.go:203-208`, observed: log `hook: PreToolUse Failed` then `exec … succeeded`, wall ≈ timeout |
| Configurable | yes — budgets derive from the installed constant and scale with it | `enforce.go:210-215`, `installer.go:323` |

**The ceiling is self-imposed, not provider-imposed.** Codex allows 600s; the installer
chooses 30s. So Codex has *more* headroom than Claude Code, not less — the phase's
pre-decided "Codex may need a narrower gated set" is **not** triggered.

Both adapters install **30s** on PreToolUse (`claude-code/enforce_tier2.go:30`,
`claude-code/localhooks.go:42`). The ceiling is the same number on both sides, which makes
phase 3's SPI capability a formalization of an existing constant, not a discovery.

**Doc drift found:** `hookflow/tier2.go:29-31` still says Claude Code "fixes at 4s under
its 5s kill". Reality is a 29s budget under an installed 30s PreToolUse timeout; 5s applies
to the *non-gating* hooks only. Phase 3 should correct it while touching this file.

## Consequences for the plan

- **Phase 3 gains a prerequisite:** fix the delivery-flag race + timeout double-store
  before removing the `HighRisk()` narrowing. Sequencing, not scope change.
- **Phase 3's ceiling SPI shrinks:** both providers are 30s and both derive from an
  installed constant. Codex enforcement is **not** blocked.
- **Phase 2's ADR** can state the ceiling as fact with citations, and must not repeat the
  "core dedupes on event_type" claim — it does not, for developer events.
- **Phase 8 inherits the unrun assertions:** per-call row count under universal escalation,
  and the mid-call-failure case.

## Unresolved

1. **Real per-call `ActivityStarted` count under universal escalation** — needs a stack.
   Everything above about the delivered path is structural, not observed.
2. **Frequency of the lost-200 window** in production, where cancellation aborts the POST.
   Mechanism confirmed; rate unknown.
3. **Can a local stack be brought up here?** `testbed/env.sh:32` names
   `local-stack/docker-compose.local.yml`; no such file under `..`. `../openbox-core` and
   `../openbox-backend` each ship a `docker-compose.yml`, but the 7-container roster
   `00-preflight.sh:23` expects (incl. `opa`, `governance-worker`, `attestation-worker`,
   `openbox-fe`) is not obviously assembled by either. **Operator input needed.**
4. **Does the fix belong in phase 3 or its own phase?** It is a live bug on `main`'s
   lineage, so it could ship independently of this plan.
