# Phase 03 — Hook-ceiling capability in the SPI; widen the gate to all classes

## Context links

- Parent: [plan.md](plan.md) · Depends on: phase 2 (ADR-0017 authorizes it)
- Blocks: phases 4, 5 · Authorized by: ADR-0017
- **The behavioural heart of the plan.** After this phase every gated call is decided by
  OpenBox; the deletion in phase 6 is then just removing what nothing calls.

## Overview

- **Date:** 2026-08-13
- **Description:** Two changes. Declare each provider's hook ceiling in the SPI so the
  engine derives its own budget instead of each adapter hardcoding one. Then remove the
  narrowing that limits inline evaluation to high-risk classes.
- **Priority:** P1 · **Implementation status:** pending · **Review status:** pending

## Key Insights

- **This is a deletion, not a rewrite.** The escalation machinery already exists and is
  already provider-agnostic: bounded run, degrade-on-timeout, verdict mapping, approval
  hold, failure policy. The change is removing `t.HighRisk()` and the `ResolveTier2()`
  gate from `gate.go:126`. Resist rebuilding what is already correct.
- **The SPI declares no hook timeouts today** — verified: neither `provider/provider.go`
  nor either `capabilities.go` carries one, so `adapters/claude-code` hardcodes
  `preToolUseHookTimeoutSec = 30` and derives every budget from it. With one gate now
  covering all classes, that cliff belongs where the engine can see it. Same rule the repo
  already enforces: provider-agnostic logic in `hookflow`, provider facts in the adapter.
- **`KeepTighter` is about to lose its meaning, but not yet.** It exists to stop a server
  verdict from loosening a local one. Once local policy evaluation is deleted (phase 6) the
  local side carries only redaction, so there is nothing to compare. Keep it here — deleting
  it in the same phase that widens the gate would conflate two failure modes.
- **No inline retries.** A failed evaluation must apply `fail_closed` and return. Retrying
  inside the gate turns one core hiccup into a client-side amplifier — and this is the hook
  that runs on every tool call.
- **The budget guard is the client's own failure boundary**, not a latency knob. It survives
  E6 untouched: if the hook is killed without writing a verdict, the tool proceeds and no
  org setting can stop it.
- Renaming `tier2.go` → `evaluate.go` here rather than in phase 7 keeps the vocabulary
  change with the behaviour change, so no commit ships code called `Tier2` that no longer
  has a Tier 1 beneath it.

## Requirements

1. Provider SPI gains a declared hook ceiling per hook event (at minimum: the gating hook
   and a default for the rest), sourced from each provider's real limits — Claude Code 30s
   gating / 5s other (verified); Codex from phase 1's finding.
2. `hookflow` derives its enforce budget from the declared ceiling, with the existing safety
   margin. No adapter hardcodes a timeout that the engine cannot see.
3. `gate.go` evaluates **every** gated call inline: remove the `ResolveTier2()` and
   `t.HighRisk()` conditions.
4. The gate **always** writes a verdict before the declared ceiling. Pinned by a test per
   adapter.
5. No retry inside the gate; a failed or timed-out evaluation applies `ResolveFailurePolicy`
   and returns.
6. `Tier2*` symbols in `hookflow` and both adapters renamed to evaluation vocabulary;
   `tier2.go` → `evaluate.go`, `enforce_tier2.go` → `enforce_evaluate.go`.
7. `tier2` / `tier2_timeout_ms` config fields: keep parsing for back-compat, ignore for
   behaviour, warn once to **stderr** if present. Removal is phase 6's.
8. Approval hold, rewake, failure policy, redaction carry-over: behaviour unchanged.
9. All 11 modules build, vet, `-race` green.

## Architecture

```
PreToolUse (any tool)
  ├─ local secret redaction (unchanged, local, whole body)
  ├─ evaluate inline  ──▶ /evaluate ──▶ verdict            (bounded by declared ceiling)
  │     └─ REQUIRE_APPROVAL ⇒ hold, then rewake if late    (unchanged)
  ├─ on timeout/outage ⇒ ResolveFailurePolicy()            (no retry)
  └─ apply: allow · deny · ask · redact-and-continue
```

One path. The high-risk distinction disappears from the engine; risk stays a property of
the *policy*, which is where it belonged.

## Related code files

| Path | Action |
|---|---|
| `provider/provider.go` | add the declared hook-ceiling capability |
| `adapters/claude-code/capabilities.go` | declare 30s gating / 5s other (verified) |
| `adapters/codex/capabilities.go` | declare Codex's ceiling from phase 1 |
| `adapters/common/hookflow/gate.go:121-130` | remove the narrowing; this is the core change |
| `adapters/common/hookflow/tier2.go` → `evaluate.go` | rename; budget now derived from the SPI |
| `adapters/claude-code/enforce_tier2.go:20-42` | the hardcoded ceiling moves to capabilities; keep the margin arithmetic |
| `adapters/codex/enforce_tier2.go` | same |
| `adapters/common/hookflow/failurepolicy.go` | unchanged, but now on the path for every call |

## Implementation Steps

1. Add the ceiling to the SPI; declare it in both adapters; have `hookflow` derive the
   budget. Land this alone and green — it is behaviour-preserving.
2. Pin per adapter: the gate writes a verdict strictly before the declared ceiling.
3. Remove the `ResolveTier2() && t.HighRisk()` narrowing. Land alone; this is the
   behaviour change and it should be bisectable on its own.
4. Assert no retry path exists inside the gate.
5. Rename the Tier2 vocabulary and files across `hookflow` and both adapters.
6. Deprecate `tier2` / `tier2_timeout_ms`: parsed, ignored, warned once to stderr.
7. Run the full matrix: build, vet, `-race`, all 11 modules.

## Todo list

- [ ] Hook ceiling declared in the SPI and both adapters
- [ ] `hookflow` derives its budget from the declaration; no hardcoded adapter timeout
- [ ] Verdict-before-ceiling pinned per adapter
- [ ] Narrowing removed: every gated call evaluates inline (its own commit)
- [ ] No retry inside the gate, asserted
- [ ] Tier2 vocabulary and filenames renamed
- [ ] `tier2*` config parsed-but-ignored with a once-only stderr warning
- [ ] Approval hold / rewake / redaction carry-over unchanged, tests green
- [ ] All 11 modules build, vet, `-race` green

## Success Criteria

- A `Read` call is decided by `/evaluate`; today it never reaches the server pre-execution.
- A raw-rego org's gated call is denied where it previously proceeded.
- Killing core mid-session ⇒ `fail_closed` applies; fail-open proceeds and records, and
  neither path retries.
- The hook never exceeds the declared ceiling without writing a verdict.
- Approval hold and rewake behave exactly as before for a `REQUIRE_APPROVAL` verdict — and
  now can occur for any tool class.
- `grep -rn "Tier2\|tier2" --include=*.go` returns only the deprecated config parse.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| Widening and renaming land together, so a regression is unbisectable | M×H | a failure appears and the cause is ambiguous | **Adjust:** steps 1, 3 and 5 are separate commits, in that order. |
| Approval hold now fires for common tools and surprises users | M×M | a `Read` pauses for approval | **Accepted:** it is policy-driven. Any org that gates `Read` for approval asked for it; the hold text already explains itself. |
| The declared ceiling is wrong for Codex | M×H | Codex hooks killed mid-verdict | **Stop:** phase 1 owns that number. Do not guess it here. |
| `KeepTighter` deleted early, hiding a loosened verdict | L×H | a server allow overrides a local redact | **Adjust:** it stays until phase 6, by design. |
| Removing the narrowing also removes the risk classification other code needs | M×M | `HighRisk()` callers break | **Adjust:** keep the classifier if anything else uses it (advisory, audit); only the gate stops consulting it. |

## Security Considerations

- Every gated call now depends on a network verdict. The failure boundary is the only thing
  standing between a killed hook and an ungoverned call — the verdict-before-ceiling test is
  a security control, not a hygiene test.
- No retry: a retry storm during a core incident would turn every developer's session into
  load against a struggling control plane, and delay every tool call while doing it.
- Local redaction must keep running **before** evaluation, so phase 4's ordering assumption
  holds by construction.

## Next steps

Phase 4 attaches content for the newly-gated classes, redacted first. Phase 5 plumbs the
verdict's policy identity into posture.
