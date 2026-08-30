# Phase 01 — that decision: the dev-turn content carrier (Accepted)

## Context links

- Plan: [plan.md](plan.md) · Depends: none
- Evidence: [scout-01](../260813-2200-dashboard-widget-telemetry-gaps/scout/scout-01-read-side-fe-backend.md),
  [scout-02](../260813-2200-dashboard-widget-telemetry-gaps/scout/scout-02-write-side-core-sdk-shiftleft.md),
  `plans/reports/research-260813-2215-session-content-capture-gaps.md`
- Amends: that decision (span layer, partially), that decision (INV-2 allowlist statement). Next free decision record
number is 0018 (that decision is last). Forward-references that decision (posture, phase 07
drafts it).
- Index: `README.md`

## Overview

- **Date:** 2026-08-13 · **Priority:** P1 · **Status:** pending · **Effort:** 1.5h
- Write  (**Accepted**) authorizing (a) top-level
`status` on tool `ActivityCompleted`, (b) ONE minimal wire span on `TurnCompleted` carrying the
assistant turn text, content_capture-gated, (c) the INV-2 / that decision amendments both
require. No code. Phases 2–4 are unmergeable without it (a new content class and a reversal of
shipped decision record statements are decision territory).

## Key Insights

1. **Partial reversal, not extension.** That decision deleted `client/hookspan.go`+`spanbuilder.go` and
declared dev sessions span-less; `contracts/dev-event/MAPPING.md:239-245` lists `status`,
`span_id`, `trace_id`, `attributes` "Retired with the span layer". Both statements stop being
true; an add-only decision record would leave two authoritative docs contradicting the code.
2. **`semantic_type` is not assertable.** Core recomputes it per span
   (`openbox-core internal/services/governance_workflow.go:302-304`); only `isLLMCall`
   (attributes `http.method=="POST"` + LLM-domain `http.url`, `internal/content/session.go:451-476`)
   or its root-field twin (`session.go:236-256`) yield `llm_completion`. The span must carry
   **synthesized classification keys** — OD-0018-1, RESOLVED by owner 2026-08-13: accept, mark
   `openbox.span_synthetic:true`, and **file the core ask** (AGE reads the `llm_completion`
   activity_output) as the change that retires them (phase 04 files it).
3. **Response-body shape is dictated:** `{"choices":[{"message":{"content":"…"}}]}` —
   core unmarshals exactly that (`goal_alignment_session.go:73-88`); anything else logs and
   yields "".
4. **INV-2 amendment is narrow.** Source = hook payload field `last_assistant_message`, so the
transcript projection allowlist and its sentinel `TestFinops_NoContentOnWire`
(`adapters/claude-code/usage_test.go:485`) stay untouched. Amended: `docs/data-and-privacy.md`
gains an egress class; `hookevent.go:119-129`'s "absence is the safeguard" argument is retired
for that ONE field (`stop_reason` stays unbound — that decision owns it).
5. **`status` is NOT content-gated** — a two-literal enum derived structurally; ships identically
   with capture off. Derivation evidence: the hooks docs (verified 2026-08-13, research §2) list a
   distinct `PostToolUseFailure` event → branch B1; phase 02 step 1 confirms empirically.
6. **Thinking is deferred, not foreclosed.** Owner posture (research §Decision update) has
thinking IN SCOPE, staged last. That decision must say "not in this carrier — that decision
(P3) owns it", never "never captured" as product stance.

## Requirements

- R1: House shape of that decision/0014/0017 (Status/Date/Context/Decision/Consequences/Alternatives/
What this does NOT change); every load-bearing claim cites repo `file:line` or upstream URL;
indexed in `README.md`.
- R2: Covers: (i) the turn-span carrier + why (AGE feeder; core unchanged per scope); (ii) span
  rows reviving server-side as accepted side effect; (iii) INV-2 amendment (hook field, not
  transcript; gated + redacted + 64KB-capped); (iv) `status` addition, ungated, tool-only;
  (v) with `content_capture:false` **nothing new egresses**; `status` unchanged either way.
- R3: Names OD-0018-1 as resolved (accept + file core ask); the core-side fix recorded as the
  retirement path for the synthesized keys.
- R4: Gate composition recorded: span requires `finops:true` ∧ `content_capture:true` (turn events
  are finops-gated, `adapters/claude-code/hookrun.go:174`); redaction under `secret_detection`
  (off ⇒ unredacted, stated). **User constraint: both gates stay default-ON**; a future default
  flip re-opens the decision.
- R5: Codex deferral recorded with reasons (no assistant-text field; `Stop` unwired,
  `adapters/codex/capabilities.go:15`; SessionEnd rollup is wrong granularity).
- R6: Unverified claims marked (testbed has not run; AGE needs `LlamaFirewallHost` set,
  `llama_firewall.go:31-34`); core evidence tagged "read at develop 68f0398, not run".
- R7: Cross-references forthcoming that decision for posture (P1–P3), including thinking.

## Related code files (read-only; every decision record claim cites one)

`client/payload.go:28-78,319-370,677-690,694-715` · `client/event.go:130-199` ·
`adapters/claude-code/hookevent.go:119-136` · `adapters/claude-code/usage.go:30-54` ·
`adapters/claude-code/hookrun.go:172-184,273-320` · `adapters/codex/capabilities.go:15` ·
`adapters/codex/hookevent.go:69-76` · `contracts/dev-event/MAPPING.md:239-245` ·
`docs/data-and-privacy.md:5-25` · `that decision…md`, `…that decision…md`, `…that
decision…md`

## Implementation Steps

1. Read that decision end to end for shape (closest analogue: a reversal with named non-re-litigable
consequences); skim that decision.
2. Draft  — **Accepted**, 2026-08-13:
   - **Decision 1 — `status`:** enum {`completed`,`failed`}, tool `ActivityCompleted` only,
     structural derivation only (hook identity / bound bool / bound int), NOT content-gated,
     client-side value allowlist. Names the `workflow_status` side effect
     (`storage_event.go:416-418`) and the reader inventory (compliance evidence export only).
   - **Decision 2 — one turn span:** exactly one, `stage:"completed"`, deterministic
     `span_id`/`trace_id`, `hook_trigger` deliberately unset, `response_body` = the OpenAI-chat
     shape, `activity_id` and every existing field byte-unchanged.
   - **Decision 3 — synthesized classification keys** (OD-0018-1, resolved): what they are, why
     unavoidable in scope, `openbox.span_synthetic:true` marker, the filed core ask as retirement.
   - **Decision 4 — INV-2 amendment:** new content class, gates, redaction, cap; transcript
     allowlist + sentinel unchanged; a future transcript-bound string still needs its own
     amendment (that decision is that amendment for thinking).
   - **Consequences:** span rows + Merkle span leaves + `age_span_evaluations` return for dev
     sessions; OPA/Guardrails see a `spans` array (a policy keyed on `http_url` matches every turn
     and cannot block — `Stop` never gates); compliance export gains `workflow_status`.
   - **Alternatives rejected:** transcript widening (costs the sentinel's structural guarantee);
     `activity_output.message` now (needs the core change scope forbids — it is the FILED ask, and
     That decision's target state); `metadata.status` promotion (typo zeroes the metric); random span
     ids (re-report looks like a new span to core's `(span_id, stage)` dedupe).
   - **NOT changed:** `activity_id` derivation + approval keys (pin tests), tool
     activity_input/output shapes, enforce cascade, Codex behaviour, deprecated-key handling,
     "Tool output: never" row (assistant text is not tool output; tool output is).
3. Add the `README.md` row.
4. Re-verify every citation by opening the cited line (stale decision record citations outlive code).

## Todo list

- [ ] Read that decision / 0013 / 0014 for shape and amendment mechanics
- [ ] Draft that decision with 4 decisions + consequences + alternatives + non-changes
- [ ] OD-0018-1 recorded as resolved (accept + file core ask)
- [ ] Gate composition + default-ON constraint + secret_detection honesty
- [ ] Thinking deferral wording (that decision owns it)
- [ ] Codex deferral + README index + citations verified

## Success Criteria

- that decision exists, Accepted, indexed; every claim cited and resolving.
- A reader knowing only that decision can answer from that decision alone: what egresses now, under which
gates, what returns server-side, why `semantic_type` is not asserted, which test did NOT move,
and where thinking/tool-output live.
- No implementation detail phases 2–4 must keep in sync (field names + JSON keys only).

## Risk Assessment

| Risk | L×I | Mitigation / pre-decided response |
|---|---|---|
| decision record asserts core behaviour that proves wrong (all core evidence read, not run) | M×H | Claims tagged with `file:line` + "read @ 68f0398". Signal: phase 6 live check contradicts. Response: amend decision record with observed behaviour |
| "Amends that decision" read as full revert | M×M | Scope sentence: spans return for ONE carrier; tool events span-less; `hookspan.go`/`spanbuilder.go` stay deleted |
| that decision/0019 contradict each other on posture | M×M | 0018 defers thinking/tool-output/stop_reason to 0019 explicitly; 0019 (phase 07) cross-references 0018 as first shipped increment |

## Security Considerations

- that decision is this plan's security artifact: the new egress class is recorded here. Prefer the
  honest limit ("with `secret_detection:false` the text egresses unredacted") over confident
  sentences. State redact-before-attach (precedent C18) and the 64KB cap. Do not imply the
  credential file or transcript gained protection.

## Next steps

Phase 02 — `status` on tool results.
