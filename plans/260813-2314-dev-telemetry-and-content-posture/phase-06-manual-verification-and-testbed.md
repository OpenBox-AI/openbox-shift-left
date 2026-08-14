# Phase 06 — manual verification guide + dormant testbed assertions

## Context links

- Plan: [plan.md](plan.md) · Depends: [phase 05](phase-05-docs-and-contract-reconciliation.md)
- Precedent to mirror: `plans/260813-0140-inline-policy-evaluation/manual-test-guide.md`
  (Setup → stub control plane → T1..Tn with ⭐ → optional real session → Cleanup → not-covered)
- Read chains: [scout-01](../260813-2200-dashboard-widget-telemetry-gaps/scout/scout-01-read-side-fe-backend.md),
  [scout-02](../260813-2200-dashboard-widget-telemetry-gaps/scout/scout-02-write-side-core-sdk-shiftleft.md)

## Overview

- **Date:** 2026-08-13 · **Priority:** P1 · **Status:** pending · **Effort:** 2h
- Produce `manual-test-guide.md` (this plan directory): stack-free walkthrough runnable in
  minutes + live-stack checklist with exact endpoints/SQL/preconditions. Per merge decision 6,
  ALSO add **dormant testbed assertions** (run deferred — no local stack) so the next live
  testbed run covers status, failure events, the span and alignment. Manual guide = near-term
  acceptance evidence; unit + conformance stay required gates.

## Key Insights

1. **Three live preconditions are outside this repo** and decide success before any client code
   matters: LlamaFirewall reachable (`performTraceCheck` returns nil when `LlamaFirewallHost`
   empty — `llama_firewall.go:31-34` — BOTH alignment widgets stay empty with a perfect client);
   Redis up (goal session store); a **fresh agent id** (old agents carry accumulated
   `tool.<name>.failed`, so SUCCESS% shows partial recovery, not 100%).
2. **Widgets are agent-scoped GETs** — no UI needed:
   `GET /agent/{id}/observability` (`agent.controller.ts:1039-1051`),
   `GET /agent/{id}/goal-alignment/trend` (`:925-936`), `…/recent-drifts` (`:944-951`).
3. **Most risk is checkable with no stack:** a stub `/evaluate` dumping request bodies proves
   status, failure events, the span, the gate and redaction ordering — the C18/C19 pattern
   (`enforce_conformance_test.go:256-320`). Stack-free is the default; live is confirmation.
4. **The negative test is the strongest:** `OPENBOX_CONTENT_CAPTURE=0` ⇒ no spans/span_count/
   assistant text, `status` still present. One check validates the whole gate design.
5. **Alignment needs a two-prompt session** (session created by `prompt_submitted`, evaluated on
   the NEXT args-bearing signal or `WorkflowCompleted` — `age.go:113-158`).
6. **Drift is not producible on demand** — honest criterion: an `age_evaluations` row exists
   (`span_id IS NULL`), not "a drift appeared".

## Requirements

- R1: Stack-free section: built binary, isolated fake `OPENBOX_HOME`, stub capturing POSTed
  bodies. No stack/Docker/network. macOS/zsh copy-pasteable (`OPENBOX_HOME` pinned for exec'd
  binaries per commit `c32e7fb`).
- R2: Live section: preconditions first and blocking; exact `curl` + SQL + expected row shapes;
  an "if empty, check X first" branch per widget.
- R3: Every check states what it proves AND what it does not.
- R4: Testbed: dormant assertions added to the relevant scripts (status/failed on tool events;
  spans row + alignment metrics), marked deferred-run per repo convention; reference the guide,
  don't duplicate it (DRY).
- R5: Cleanup + "what this does not cover" (testbed not run; Codex unfed for both features;
  subagent turns depend on `last_assistant_message` on `SubagentStop`; StopFailure possibly
  docs-only-verified; lost-200 double-store window; server-side retention of the new class).
- R6: Written for a reader who has not read this plan.

## Architecture (guide structure)

```
Setup (5 min)        build; isolated OPENBOX_HOME; fake creds; stub /evaluate teeing bodies;
                     openbox auth → openbox init --provider claude-code on a scratch project
Stack-free
  T1 ⭐ tool status   PostToolUse ⇒ status:"completed" in captured body
  T2 ⭐ failure path  failing Bash (exit 3) ⇒ per shipped branch: status:"failed" via
                     PostToolUseFailure, or the documented fallback — guide states which shipped
  T3 ⭐ turn span     Stop ⇒ ONE span, stage completed, wrapper parses, classification keys present
  T4 ⭐ the gate      OPENBOX_CONTENT_CAPTURE=0 ⇒ no spans/span_count/text; status still present
  T5 ⭐ redaction     fake AKIA-shaped literal in assistant reply ⇒ masked in captured body
  T6   lifecycle      permission denial ⇒ signal permission_denied; subagent ⇒ subagent_started
  T7   determinism    re-run same turn (delete cursor) ⇒ identical span_id
  T8   finops off     OPENBOX_FINOPS=0 ⇒ no turn events at all, hence no span
  T9   unit gates     per-module go test -race; conformance; golden diff review
Live checklist
  P0   preconditions  LlamaFirewall set, Redis up, FRESH agent id
  L1 ⭐ Tool Health    GET observability ⇒ tools[].success_calls > 0
  L2 ⭐ spans row      SQL: span_type='llm_completion', stage completed
  L3 ⭐ alignment      observability_metrics metric_type='goal_alignment' keys
  L4   drift          age_evaluations row (span_id IS NULL)
  L5   no regression  span_tools CTE selects span_type='mcp_tool_call' only (scout-01:269-284)
  L6   signals        governance_events rows for the three new signal names
Cleanup / What this does not cover
```

SQL to embed (from the superseded 2200 phase-05, verified against scout-01): the four queries for
tool metrics, spans row, goal_alignment metrics, age_evaluations — plus the widget-side formulas
(SUCCESS% = success/total, <90% renders red, `monitor/index.tsx:85-102`; trend =
aligned/evaluations per day, `observability.service.ts:994-1029`).

## Related code files

Read-only: `adapters/claude-code/enforce_conformance_test.go` (stub recipe),
`plans/260813-0140-inline-policy-evaluation/manual-test-guide.md` (structure),
`testbed/25-realtime.sh`, `testbed/30-enforce.sh` (existing assertion style),
`client/turnspan.go`, `client/payload.go` (exact keys), `adapters/claude-code/hookrun.go:172-184`.

Write: `plans/260813-2314-dev-telemetry-and-content-posture/manual-test-guide.md`;
testbed script(s) gaining the dormant assertions (smallest owning script — likely a new
`testbed/35-telemetry.sh` following the numbering, or extending `30-enforce.sh` if assertions fit
its session; decide by reading them, prefer extending over a new file unless phases diverge).

## Implementation Steps

1. Read the precedent guide; reuse Setup verbatim where it applies (isolated home, fake creds,
   stub, scratch project).
2. Write Setup + capture/pretty-print recipe
   (`jq -r '.spans[0].response_body | fromjson'` for the nested JSON string).
3. Write T1–T9: command, exact expected substring/JSON path, proves / does-not-prove. ⭐ T1–T5.
4. Write P0/L1–L6 with SQL + a diagnosis branch per widget: Tool Health 0% → is `status` in the
   stored row's `workflow_status`? (yes ⇒ core extractor path; no ⇒ client). Alignment empty but
   span row exists → LlamaFirewall unset or Redis session missing (was `prompt_submitted`
   carrying `signal_args` — capture on?). Drift empty with alignment present → expected unless
   LlamaFirewall reported misalignment.
5. Testbed: add the dormant assertions (status, failed, span row, alignment keys, signal names),
   marked deferred-run; keep them one-source with §7 of MAPPING (reference, don't restate).
6. Cleanup (scratch project, isolated home, stub, spool dir; capture file is content-bearing —
   delete it) + not-covered list.
7. **Run the stack-free part once on this machine**; fix whatever does not work verbatim.
8. Link the guide from `plan.md`. Commit:
   `docs(plans): add the manual verification guide for dev telemetry` +
   `test(testbed): dormant assertions for status, failure signals and the turn span`.

## Todo list

- [ ] Setup runnable on macOS/zsh, pinned `OPENBOX_HOME`
- [ ] T1–T5 (⭐) + T6–T9 with expected outputs
- [ ] P0 + L1–L6 with SQL and diagnosis branches
- [ ] Dormant testbed assertions merged (deferred-run noted)
- [ ] Cleanup + not-covered sections
- [ ] Stack-free part executed once and corrected
- [ ] Guide linked from plan.md

## Success Criteria

- A reader who has not seen this plan can, in <15 min with no stack, confirm: status ships,
  failure path ships, the span ships under capture, nothing ships with capture off, secrets are
  masked.
- Every live check names its precondition and failure branch; no check ends in "should work".
- The guide asserts nothing that has not been run — the live section is explicitly a checklist,
  not a record.
- Testbed assertions exist, marked deferred; zero duplication with the guide beyond references.

## Risk Assessment

| Risk | L×I | Mitigation / pre-decided response |
|---|---|---|
| Commands don't work verbatim (flag drift) | H×M | Step 7 runs the stack-free part before committing |
| Live part run without LlamaFirewall ⇒ "feature failed" | H×M | P0 first and blocking, `llama_firewall.go:31-34` cited; documented diagnosis branch |
| Reused agent id makes Tool Health look broken | M×M | P0 requires fresh id, explains accumulated `.failed` |
| Guide read as acceptance for the whole plan | M×H | Explicit not-covered + the repo rule that unit tests are not hook evidence |
| Testbed assertions rot before ever running | M×M | Kept assertion-thin, referencing MAPPING §7 as the single list |

## Security Considerations

- Fake credentials + fake DID only (INV-1); redaction test uses a synthetic AKIA-shaped literal
  and says so — nobody pastes a live token to test masking.
- Captured bodies contain prompt + assistant text: named content-bearing, deleted in Cleanup.
- Never `cat ~/.openbox/.env` in a step (plaintext by design, ADR-0015 — don't teach the habit).

## Next steps

Phase 07 — ADR-0019 full-content-capture posture draft.
