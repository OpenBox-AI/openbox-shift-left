# Phase 05 — the manual verification guide

## Context links

- Plan: [plan.md](plan.md) · Blocked on: [phase 04](phase-04-docs-and-contract-reconciliation.md)
- Precedent to mirror: `plans/260813-0140-inline-policy-evaluation/manual-test-guide.md`
  (Setup → stub control plane → T1..Tn with ⭐ on the load-bearing ones → optional real session →
  Cleanup → What this does not cover)
- Read chains being verified: [scout-01](scout/scout-01-read-side-fe-backend.md) (endpoints + SQL),
  [scout-02](scout/scout-02-write-side-core-sdk-shiftleft.md) (writers)

## Overview

- **Date:** 2026-08-13
- **Description:** Produce `manual-test-guide.md` in this plan directory: a stack-free walkthrough
  the user can run in minutes, plus a live-stack checklist for the three widgets with the exact
  endpoints, SQL and preconditions. Per user decision 3, this replaces an automated e2e run as the
  acceptance evidence; unit + conformance remain required gates.
- **Priority:** P1 — without it, the plan's central claims stay unverifiable by the person who has
  to trust them.
- **Implementation status:** pending
- **Review status:** pending
- **Effort:** 1.5h

## Key Insights

1. **Three preconditions decide whether the live check can succeed at all**, and all three are
   outside this repo:
   - LlamaFirewall/AGE must be reachable from core — `performTraceCheck` returns nil when
     `LlamaFirewallHost` is empty (`openbox-core internal/services/llama_firewall.go:31-34`), so
     `GoalAlignmentChecked` never sets and BOTH alignment widgets stay empty **even with a perfect
     client**. The guide must say this before the user concludes the feature is broken.
   - Redis must be up (the goal session store is Redis-backed —
     `openbox-core internal/services/goal_alignment_session.go:28-48`).
   - A **fresh agent id** is needed for a clean Tool Health reading: `tool.<name>.failed` counters
     have already accumulated for every existing dev agent, and SUCCESS% is computed from the sums
     over the window (`openbox-backend .../observability.service.ts:244-303`). An old agent will
     show a partial recovery, not 100%.
2. **The widgets are agent-scoped GETs**, so verification does not need the UI:
   `GET /agent/{agentId}/observability` (Tool Health — `agent.controller.ts:1039-1051`),
   `GET /agent/{agentId}/goal-alignment/trend?fromTime&toTime` (`:925-936`),
   `GET /agent/{agentId}/goal-alignment/recent-drifts?limit` (`:944-951`).
3. **Most of the risk is checkable with no stack.** The client's outbound bytes are the contract:
   a stub `/evaluate` that dumps the request body proves `status`, the span, the content gate and
   redaction ordering — which is exactly how conformance C18/C19 already work
   (`adapters/claude-code/enforce_conformance_test.go:256-320`). The guide should make the
   stack-free part the default and the stack part the optional confirmation.
4. **The negative test is the strongest one.** `OPENBOX_CONTENT_CAPTURE=0` must produce a wire body
   with no `spans`, no `span_count`, no assistant text — and still carry `status`. That single check
   validates the whole gate design, and it runs against the real binary.
5. **Alignment needs a two-prompt session.** The Redis session is created by `prompt_submitted` and
   evaluated on the NEXT `SignalReceived`-with-args or at `WorkflowCompleted`
   (`openbox-core internal/services/age.go:113-158`). A one-prompt session only evaluates at session
   end. Prescribe: prompt → wait for the turn → prompt again → end session.
6. **Drift is not producible on demand.** `goal_drift` requires LlamaFirewall to answer
   `!IsAligned` for the trace. The guide must say the honest success criterion for the Drift widget
   is "an `age_evaluations` row now exists for this agent with `span_id IS NULL`", not "a drift
   event appeared".

## Requirements

- R1: Stack-free section runs with: the built binary, a fake `~/.openbox` home, a local stub that
  captures the POSTed body. No OpenBox stack, no Docker, no network.
- R2: Live-stack section lists preconditions, the exact `curl` calls, the exact SQL, and the
  expected row shapes — with a "if this is empty, check X first" branch per widget.
- R3: Every check states what it proves AND what it does not.
- R4: Guide is copy-pasteable: real commands, macOS/zsh-compatible (`OPENBOX_HOME` pinned for the
  exec'd binary as commit `c32e7fb` established for the CLI tests).
- R5: Includes a cleanup section and a "what this does not cover" section naming the gaps
  (testbed not run; Codex unfed; subagent turns dependent on the payload field;
  the lost-200 double-store window).
- R6: Written for a reader who has not read this plan.

## Architecture

Guide structure to produce (`manual-test-guide.md`, this directory):

```
Setup (5 min)          build; isolated OPENBOX_HOME; fake creds; stub /evaluate that
                       tees each request body to a file; govern a scratch project
                       (openbox auth → openbox init --provider claude-code)
The stack-free tests
  T1 ⭐ tool status     a real PostToolUse produces status:"completed" in the captured body
  T2 ⭐ failure path    a failing tool call: per the phase-02 branch, either status:"failed"
                       or an unpaired ActivityStarted — the guide states which was shipped
  T3 ⭐ turn span       Stop produces ONE span, stage completed, response_body wrapper,
                       attributes carrying the classification keys
  T4 ⭐ the gate        OPENBOX_CONTENT_CAPTURE=0 → no spans/span_count/assistant text,
                       status still present
  T5 ⭐ redaction       a fake secret in the assistant reply is masked in the captured body
  T6    determinism     re-run the same turn (delete the cursor) → identical span_id
  T7    finops off      OPENBOX_FINOPS=0 → no turn events at all, hence no span
  T8    unit gates      per-module go test -race; contracts conformance; golden diff review
The live-stack checklist
  P0    preconditions   LlamaFirewall host set in core, Redis up, FRESH agent id
  L1 ⭐ Tool Health     GET /agent/{id}/observability → tools[].success_calls > 0
  L2 ⭐ spans row       SQL: spans row, span_type='llm_completion', stage completed
  L3 ⭐ alignment       observability_metrics metric_type='goal_alignment' keys present
  L4    drift           age_evaluations row (span_id IS NULL); goal_drift true only if
                        LlamaFirewall says misaligned
  L5    no regression   tool metrics unaffected by the turn span (span_tools CTE selects
                        span_type='mcp_tool_call' only)
Cleanup / What this does not cover
```

Expected shapes to state explicitly:

```sql
-- L1 (Tool Health): success now written alongside the pre-existing failed counters
SELECT metric_key, SUM(metric_value) FROM observability_metrics
 WHERE agent_id = '<fresh-agent-uuid>' AND metric_type = 'tool'
 GROUP BY metric_key ORDER BY metric_key;
-- expect tool.<Name>.total, .success (> 0 — the new one), .latency_sum_ms, .latency_count

-- L2 (the carrier arrived)
SELECT span_id, span_type, stage, length(response_body) FROM spans s
  JOIN sessions ss ON ss.id = s.session_id WHERE ss.agent_id = '<fresh-agent-uuid>';
-- expect exactly one row per model turn, span_type = 'llm_completion', stage = 'completed'

-- L3 (alignment metrics)
SELECT metric_key, SUM(metric_value) FROM observability_metrics
 WHERE agent_id = '<fresh-agent-uuid>' AND metric_type = 'goal_alignment' GROUP BY metric_key;
-- expect evaluation_count (>0) and aligned_count; drifted_count only on real drift

-- L4 (drift widget's source)
SELECT id, goal_drift, span_id, evaluated_at FROM age_evaluations
 WHERE agent_id = '<fresh-agent-uuid>' AND span_id IS NULL ORDER BY evaluated_at DESC LIMIT 5;
```

Widget-side reading of the same data (so the user can cross-check the UI):
SUCCESS% = `success_calls/total_calls*100`, and `<90%` renders red
(`openbox-fe .../monitor/index.tsx:85-102`); alignment trend =
`SUM(aligned_count)/SUM(evaluation_count)*100` per day
(`openbox-backend .../observability.service.ts:994-1029`).

## Related code files

Read-only, to keep the guide accurate:
`adapters/claude-code/enforce_conformance_test.go` (how a stub `/evaluate` is stood up in-repo),
`plans/260813-0140-inline-policy-evaluation/manual-test-guide.md` (structure + the isolated-home
recipe), `testbed/25-realtime.sh` and `testbed/30-enforce.sh` (existing live-stack assertions to
reference rather than duplicate), `client/turnspan.go` + `client/payload.go` (the exact keys to
grep for in a captured body), `adapters/claude-code/hookrun.go:172-184` (which hooks fire what).

Write: `plans/260813-2200-dashboard-widget-telemetry-gaps/manual-test-guide.md` only.

Also update (small, one line each): the testbed assertion notes if a script's expectations changed —
prefer referencing the guide over duplicating steps.

## Implementation Steps

1. Read the precedent guide end to end; reuse its Setup verbatim where it still applies (isolated
   `OPENBOX_HOME`, fake credentials, stub control plane, scratch project).
2. Write Setup, adding: how to capture each outbound body to a file, and how to pretty-print the
   captured `spans[0].response_body` (nested JSON string — `jq -r '.spans[0].response_body | fromjson'`).
3. Write T1-T8 with, for each: the command, the exact expected substring/JSON path, what it proves,
   what it does not. Mark T1-T5 ⭐.
4. Write the live checklist P0/L1-L5 with the SQL above and a diagnosis branch per widget:
   - Tool Health still 0% → is `status` in the stored row's `workflow_status`? if yes, core's
     extractor path; if no, the client;
   - alignment empty but the span row exists → LlamaFirewall unset or Redis session missing
     (was `prompt_submitted` carrying `signal_args`? i.e. content capture on?);
   - drift empty with alignment present → expected unless LlamaFirewall reported misalignment.
5. Cleanup section (remove scratch project, isolated home, stub; note the spool dir).
6. "What this does not cover": testbed not run; Codex has no alignment feed; subagent turns depend
   on `last_assistant_message` being present on `SubagentStop`; the lost-200 double-store window;
   retention of the new content class server-side.
7. Cross-check every command by running the stack-free part at least once on this machine and fixing
   what does not work verbatim.
8. Commit: `docs(plans): add the manual verification guide for dev widget telemetry`.

## Todo list

- [ ] Setup section, runnable on macOS/zsh with a pinned `OPENBOX_HOME`
- [ ] T1-T5 (⭐) + T6-T8 written with expected outputs
- [ ] P0 preconditions incl. LlamaFirewall + Redis + fresh agent id
- [ ] L1-L5 with the SQL and a diagnosis branch each
- [ ] Cleanup + What this does not cover
- [ ] Stack-free part actually executed once and corrected
- [ ] Guide linked from `plan.md`

## Success Criteria

- A reader who has not seen this plan can, in under 15 minutes and with no stack, confirm: `status`
  ships, the span ships under capture, nothing ships with capture off, secrets are masked.
- Every live check names its precondition and its failure branch; no check ends in "should work".
- The guide asserts nothing that has not been run — the live section is explicitly a checklist for
  the user, not a record of a passing run.
- Zero overlap with `testbed/*.sh` beyond references (DRY: the testbed stays the automated path).

## Risk Assessment

| Risk | L×I | Mitigation / signal & pre-decided response |
|---|---|---|
| Guide's commands do not work verbatim (drifted flags, e.g. `auth`/`init` split) | H×M | Step 7 runs the stack-free part before committing. **Signal:** a step fails on the author's machine. **Response:** fix the guide (or the CLI if it is a real defect) in-plan |
| User runs the live part without LlamaFirewall and concludes the feature failed | H×M | P0 is first and blocking, with the `llama_firewall.go:31-34` citation. **Signal:** alignment empty while an `llm_completion` span row exists. **Response:** documented diagnosis branch, no code change |
| Reused (non-fresh) agent id makes Tool Health look broken | M×M | P0 requires a fresh agent id and explains the accumulated `.failed` counters |
| Guide read as acceptance evidence for the whole plan | M×H | An explicit "what this does not cover" plus the repo's standing rule that unit tests are not evidence a hook works |
| Guide rots as the code changes | M×L | Keep it in the plan directory (a stateful record), not in `docs/`; reference `testbed/` rather than restating it |

## Security Considerations

- Use obviously fake credentials and a fake DID; never a real `obx_` key or agent private key in
  the guide (INV-1). The precedent guide's fake-credential recipe is the pattern.
- The redaction test must use a synthetic secret string (e.g. an `AKIA`-shaped literal that is not a
  real key) and the guide must say so, so nobody pastes a live token to test masking.
- Captured request bodies contain prompt and assistant text: tell the reader the capture file is
  content-bearing and to delete it in Cleanup.
- Do not print `~/.openbox/.env` contents in any step; the file is plaintext by design (ADR-0015)
  and a guide that cats it teaches the wrong habit.

## Next steps

Plan complete. Open follow-ups for other repos (not this plan): core's success derivation is dead
for every current producer; an AGE accumulator that reads the `llm_completion` activity would delete
this plan's synthesized classification keys; server-side dedupe on developer events closes the
lost-200 window.
