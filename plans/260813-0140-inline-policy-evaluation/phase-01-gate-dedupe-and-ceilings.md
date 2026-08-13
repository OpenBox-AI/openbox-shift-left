# Phase 01 — Gate: dedupe under universal escalation + Codex ceiling

## Context links

- Parent: [plan.md](plan.md) · Depends on: nothing · **Blocks every other phase**
- Investigation only. Writes a finding to `reports/`, no source changes.
- Prior fix under test: commit `a901969` "fix(hookflow): store one ActivityStarted per
  gated tool call" — the reason the current branch is named
  `fix/tier2-duplicate-activity-started`.

## Overview

- **Date:** 2026-08-13
- **Description:** Two empirical questions that can invalidate the whole direction. Answer
  both before the ADR is written, because an ADR that assumes a broken premise is worse
  than no ADR.
- **Priority:** P1 — blocking gate
- **Implementation status:** done, minus the waived stack run · **Review status:** pending
- **Finding:** [reports/finding-260813-dedupe-and-ceilings.md](reports/finding-260813-dedupe-and-ceilings.md)
  — verdict **proceed-with-fix**; the fix landed standalone as `eb53827`. Q2 answered
  (Codex 30s, self-imposed, configurable; the "unknown ceiling" premise below is stale).
  Q1 answered structurally, with a live data race found and fixed and the lost-200
  double-store window documented as irreducible client-side. The empirical row count was
  **waived** by the operator — no stack; manual verification at the end.

## Key Insights

- **The dedupe fix was scoped to a subset.** `gate.go:55` notes the escalation "POSTs the
  identical event" as the spool path, and `a901969` made that store one `ActivityStarted`
  per gated call. Today only high-risk classes escalate, so the fix covers a fraction of
  events. **Under this plan every gated call escalates**, so the same correctness now
  governs 100% of them. A double-count that was tolerable at 5% is not at 100%.
- Core's dedupe key includes `event_type`
  (`activities/governance/validation.go:96`, per `CLAUDE.md`) — which says it *should*
  hold. But this repo's own rule is that reading is not evidence.
- **Codex's hook ceiling is unknown.** Claude Code is verified: 30s PreToolUse, 5s
  everything else (`adapters/claude-code/enforce_tier2.go:20-34`). Codex's equivalent is
  not recorded anywhere in this repo. Under inline evaluation a killed hook means the tool
  proceeds ungoverned, so shipping Codex enforcement without that number is shipping a
  hole.
- Neither question is about latency. Both are failure-boundary questions that hold however
  fast core is.

## Requirements

1. Demonstrate, with a real run, exactly how many `ActivityStarted` rows core stores per
   gated tool call **when every class escalates** — not by reading the dedupe code.
2. Record Codex's hook timeout ceiling with its source (Codex docs or its config schema),
   per hook event, and whether it is configurable.
3. Write both findings to `reports/`, including the negative case if dedupe does not hold.
4. If dedupe does not hold: **stop**. The finding names the defect and this plan pauses
   until it is fixed.

## Architecture

No code. A scratch build may temporarily remove the `t.HighRisk()` narrowing in
`gate.go:126` to force universal escalation — that build is a measurement instrument and
is **not** committed.

## Related code files

| Path | Why |
|---|---|
| `adapters/common/hookflow/gate.go:55,121-130` | the narrowing being removed, and the identical-event note |
| `adapters/common/hookflow/realtime.go:57` | "assuming a dedupe that did not exist is how it came to store every escalated…" — prior art on this exact failure |
| `adapters/claude-code/enforce_tier2.go:20-42` | the verified Claude Code ceilings and the budget guard |
| `adapters/codex/*` | no ceiling recorded; find where it would come from |
| `testbed/` | the only mock-free way to answer question 1 |

## Implementation Steps

1. Read `a901969` in full; write down what it dedupes on and at which layer.
2. Scratch build with the high-risk narrowing removed and evaluation forced on.
3. Drive a real session against a local stack with a mix of Read/Write/Edit/Bash/MCP
   calls; count stored `ActivityStarted` per call.
4. Repeat with a deliberate mid-call failure (kill core between escalation and spool
   flush) to see whether the retry path re-stores.
5. Find Codex's hook timeout: its own docs first, then its config schema; record the
   number, the source, and whether the installer can influence it.
6. Write `reports/finding-<date>-dedupe-and-ceilings.md` with both answers and a verdict:
   proceed / proceed-with-fix / stop.

## Todo list

- [x] `a901969` read; dedupe mechanism written down — client-side suppression at the gate,
      keyed on transport delivery; class-independent by construction
- [x] Scratch instrument produced (not committed) — a `-race` test rather than a build:
      it reaches the failure boundary without a stack
- [ ] **Real session driven; `ActivityStarted` per call counted** — blocked, no local stack
- [ ] **Mid-call failure case exercised** — blocked, no local stack
- [x] Codex ceiling recorded with source — 30s installed under a 600s default
- [x] Finding written with an explicit proceed/stop verdict — proceed-with-fix

## Success Criteria

- Exactly one `ActivityStarted` per gated call, observed, with every class escalating.
- Codex's ceiling is a number in a report with a citation, not an assumption.
- The finding is written even if the answer is "cannot run" — in which case this plan does
  not proceed on the assumption that it works.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| Dedupe does not hold at 100% escalation | M×H | more than one `ActivityStarted` per call | **Stop.** Fix the dedupe before any deletion; a corrupted event store is worse than the tiers. |
| No local stack available | M×H | cannot drive a real session | **Stop and record.** Reading core's validation code is explicitly not evidence in this repo. |
| Codex ceiling turns out to be very short | L×H | e.g. 5s with no configurability | **Adjust:** Codex may need a narrower gated set than Claude Code; that is a design change to phase 3, decided with the number in hand. |
| Scratch build accidentally committed | L×M | narrowing missing on the branch | **Adjust:** phase 3 owns that change deliberately; revert and let it land there. |

## Security Considerations

The mid-call failure test writes real governance events. Use a local stack, never a
production org. The scratch build disables the high-risk narrowing, so it evaluates
everything — do not run it against a stack whose policy would block the operator's own
session.

## Next steps

Phase 2 writes ADR-0017 with these two numbers in it.
