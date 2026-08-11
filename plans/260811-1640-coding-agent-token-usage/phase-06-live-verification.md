# Phase 06 — Live verification

## Context links

- Parent: [plan.md](plan.md) · depends on Phase 04, Phase 05
- Standing rule: `CLAUDE.md` — "Verify against the real thing … unit tests are not
  evidence that a hook works"
- Suite: `testbed/run-all.sh`, `docs/testbed/e2e.md`
- Cautionary precedent: the Tier-2 duplicate-ActivityStarted bug shipped because the
  only assertion that would have caught it ran in a mode where the bug could not
  occur (`plans/reports/fix-260811-1546-tier2-duplicate-activity-started.md`)

## Overview

- Date: 2026-08-11 (revised after validation round 2)
- Description: assert per-turn model+usage arrives as paired `llm_completion`
  activities, in the right amounts, from a real headless session against a real
  stack — including the subagent no-double-count case.
- Priority: P1
- Implementation status: **assertions written, RUN NOT DONE** — this phase is complete only when it has run
- Review status: reviewed (code-reviewer, 2026-08-11)

## Key insights

- **Counting assertions are the ones that matter here.** "Usage arrived" is nearly
  worthless; "T turns produced T pairs whose sum equals the session rollup" is
  what catches the double-count, the off-by-one cursor, and the missed turn. The
  duplicate-Started bug is the standing lesson: an existence check passed while a
  count was wrong.
- The per-turn records and the retained SessionEnd rollup are independent
  derivations of the same quantity — but only after Phase 03 aligned their
  semantics (`Input` pure, cache counts separate) are they comparable. The
  reconciliation must compare **all four fields**, not one total.
- **The pairing guard changes shape.** `testbed/40-approvals.sh` step G and any
  Started==Completed counting assertion now see a second activity kind per
  session. Extend the guard to count per `activity_type`, so tool-call pairing
  and turn pairing are each asserted exactly.
- **Tool-metric pollution is expected until the core issue ships.** On a stack
  running current core, `llm_completion` will appear under tool metrics
  (`ExtractToolMetric` accepts any activity_type — round-2 verified). The testbed
  records this state and links the core issue; once core ships the exclusion, the
  check flips to asserting absence. Do not let the interim state read as a
  shift-left defect.
- The subagent case is a first-class scenario now, not an edge: a session that
  spawns a subagent must show separate records and an exact global sum.
- Negative assertions belong here too: with capture disabled, zero usage rows and
  no model beyond SessionStart.

## Requirements

1. A testbed phase driving a session with a **known** number of turns T.
2. Assert: `llm_completion` pair count == T; every `activity_id`
   (`<session>:turn:<n>`) has exactly one Started and one Completed; indexes
   contiguous.
3. Assert: Σ per-turn == SessionEnd rollup, field by field (input, output,
   cache-creation, cache-read).
4. Assert: each Completed carries `activity_output` = model + four counts; model
   non-empty (scripted session pins the model, so absence is a failure here even
   though the contract tolerates it).
5. Assert: no content — the four sentinel classes absent from stored rows
   (`input`/`output` columns and metadata).
6. Subagent scenario: parent turns + subagent records sum to the rollup exactly
   once; subagent records attributed (`agent_id`/`agent_type` present).
7. Assert: with capture disabled, zero `llm_completion` rows and no model beyond
   SessionStart.
8. Codex: one rollup pair (`<session>:usage:rollup`) with the four counts, or the
   documented limit asserted where the stack lacks a Codex session.
9. Existing suite still green, including the extended 40-approvals step G.

## Architecture

```
testbed/NN-usage.sh
  ├─ scripted session, known turn count T (+ one subagent task)
  ├─ count llm_completion pairs                → == T (+ subagent records)
  ├─ pairing: each :turn:<n> id has 1×Started + 1×Completed; n contiguous
  ├─ Σ per-turn (4 fields) vs SessionEnd rollup → equal, field by field
  ├─ every Completed: activity_output model non-empty, counts ≥ 0
  ├─ sentinel strings in stored rows           → absent
  ├─ subagent: separate records; global sum exact; agent_id attributed
  ├─ tool-metric state recorded (pollution expected pre-core-fix; linked issue)
  └─ capture disabled → zero llm_completion rows, zero model beyond SessionStart
```

Follow the existing `tb_step` / `assert_eq` / `tb_val` conventions rather than
inventing helpers.

## Related code files

| File | Change |
|---|---|
| `testbed/NN-usage.sh` | new phase (number it after the existing capture phases) |
| `testbed/40-approvals.sh` | step G pairing guard counts per `activity_type` |
| `testbed/run-all.sh` | register the phase |
| `testbed/lib/*.sh` | only if a genuinely new helper is needed |
| `docs/testbed/e2e.md` | document what the phase proves |
| `plans/reports/` | the run's result, including measured per-Stop cost |

## Implementation steps

1. Write the scripted session with a deterministic turn count and one subagent
   task (the existing scripts show how to drive a headless session with a fixed
   prompt sequence).
2. Add the count, pairing, and contiguous-index assertions first — they catch the
   likely defects.
3. Add the four-field reconciliation assertion (Σ per-turn vs rollup).
4. Add the model-present, `activity_output`-shape, and sentinel-absent assertions.
5. Add the subagent no-double-count scenario.
6. Extend 40-approvals step G to per-`activity_type` pairing counts.
7. Add the disabled-capture negative case.
8. Record the tool-metric pollution state with a link to the core issue.
9. Register in `run-all.sh`; run the whole suite, not just the new phase; also
   re-run `testbed/25-realtime.sh` — turn events ride the same debounced flusher.
10. Record the outcome, including the measured per-Stop transcript-parse cost
    from Phase 02, in `plans/reports/`.

## Outcome

**Assertions written; THE RUN HAS NOT HAPPENED.** `testbed/28-usage.sh` exists,
is registered in `run-all.sh` under the `usage` tag, is syntax-clean, and is
documented in `docs/testbed/e2e.md`. No local OpenBox stack was available in this
session, and per this repo's own standing rule — unit tests are not evidence that a
hook works — **this phase is not complete and the feature is not verified.**

What the phase asserts, and why each is a count rather than an existence check, is
in its header comment. Two related edits landed with it:

- **`40-approvals` step G** now counts duplicates per activity kind as well as in
  aggregate. The global check already covered turn rows, but one number could not
  say WHICH kind duplicated — and the two causes are unrelated (a tool-call
  duplicate means the escalation and observe copy both stored a row; a turn
  duplicate means the cursor advanced without its events being spooled).
- **`20-capture`'s activity counts are now scoped to tool activities.** Turn pairs
  ride the same two wire types, so its unscoped `assert_ge "tool calls captured" 4`
  would have passed on two tool calls plus two turns. Found while writing this
  phase; it is a real weakening of an existing assertion that the turn feature
  introduced.

### What the run must settle

Beyond the phase's own assertions, three things are open and only a live run
closes them:

1. **`Stop`'s cadence** — does it fire on a tool-only turn? Window sums are exact
   either way, so nothing depends on the answer; it needs recording, not deciding.
2. **Which transcript `SubagentStop` names**, and whether its lines carry
   `isSidechain: true`. Step E reports an honest **skip** when no `:agent:` turn
   ids appear, and that skip IS the finding: it would mean `SubagentStop` reads a
   window whose lines are not sidechain-marked, so subagent turns report nothing.
   The partition cannot double-count under any answer, which is why this did not
   block.
3. **Per-`Stop` transcript parse cost** under the hook's 5s timeout.

### Known interim state, not a defect

`llm_completion` will appear under core's **tool** metrics until openbox-core ships
the `ExtractToolMetric` exclusion. Step G records it with its cause and a pointer to
the issue spec rather than asserting it away; when core ships, flip that step to
`assert_eq 0`.

## Todo list

- [x] Scripted session with known turn count + subagent task *(written)*
- [x] Assert pair count == T; one Started + one Completed per id; indexes contiguous *(written)*
- [x] Assert Σ per-turn == rollup, all four fields *(written)*
- [x] Assert model non-empty + `activity_output` shape on every Completed *(written)*
- [x] Assert sentinel content absent from stored rows *(written)*
- [x] Subagent: separate, attributed records; global sum exact *(written)*
- [x] 40-approvals step G extended per `activity_type` *(written; "still green" needs the run)*
- [x] Tool-metric pollution state recorded, core issue linked *(written)*
- [x] Disabled ⇒ zero `llm_completion` rows, zero model beyond SessionStart *(written)*
- [ ] Codex rollup pair covered or its limit asserted
- [x] Registered in `run-all.sh` as `usage` *(full-suite green needs the run)*
- [ ] Result + per-Stop cost recorded in `plans/reports/`

## Success criteria

- Full `testbed/run-all.sh` green against a live local stack.
- Turn count, pairing, contiguity, and four-field reconciliation assertions all
  pass — not just existence.
- Subagent tokens counted exactly once, attributed to their agent.
- Sentinel content absent in stored rows, proving INV-2 end-to-end rather than at
  the unit boundary.
- Disabled path proven silent.
- Existing phases, including the extended duplicate-activity guard, still green.

## Risk assessment

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| No local stack available | **medium — currently true** | blocks the phase | Cannot be substituted with unit tests; the plan is not complete without this run |
| Turn count non-deterministic in a scripted session | medium | medium | Fix the prompt sequence; if genuinely variable, assert the invariants (pairing + sum == rollup) rather than an absolute count |
| Existence-only assertions give false confidence | high if rushed | high | Counting, pairing, and reconciliation assertions are required, not optional |
| New events perturb existing assertions | medium | medium | Re-run the whole suite; step G extension is part of this phase, not an afterthought |
| Pollution state misread as a shift-left bug | medium | low | Recorded explicitly with the core-issue link |
| Per-Stop parse cost degrades sessions | medium | medium | Measured in Phase 02, recorded here; cursor keeps it incremental |

## Security considerations

- The sentinel-absence assertion here is the only end-to-end proof that INV-2
  holds after Phase 03 narrowed it. Unit-level absence is necessary, not
  sufficient — this is the assertion a privacy reviewer should be pointed at.
- The testbed touches a real stack with real credentials; follow the existing
  suite's handling and never print secret values into logs or reports.
- The disabled-capture case is a security assertion, not a feature test: it
  proves the documented opt-out is real.

## Next steps

Plan complete. The core-side consumer work proceeds on the issue Phase 01 filed
(activity-based extractor, cache keys, `ExtractToolMetric` exclusion); once core
ships it, flip this phase's pollution check from "recorded" to "asserted absent"
and re-run.
