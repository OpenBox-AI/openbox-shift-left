# Phase 01 — that decision: the dev-turn content carrier

## Context links

- Plan: [plan.md](plan.md)
- Evidence: [scout-01](scout/scout-01-read-side-fe-backend.md) (read chains),
  [scout-02](scout/scout-02-write-side-core-sdk-shiftleft.md) (write chains),
  [research 260813-2215](../reports/research-260813-2215-session-content-capture-gaps.md)
- decision records this amends:  (deleted the span layer),
(INV-2 allowlist rule), (last decision record; next number is 0018)
- Index to update: `README.md`

## Overview

- **Date:** 2026-08-13
- **Description:** Write  — the decision record
authorising (a) a top-level `status` on tool `ActivityCompleted`, (b) ONE minimal wire span on
`TurnCompleted` carrying the assistant turn text as `response_body`, content_capture-gated, and
(c) the INV-2 / that decision amendments both require. No code.
- **Priority:** P1 within this plan — phases 2 and 3 are unmergeable without it (repo rule:
a new content class and a reversal of a shipped decision record are decision territory,
not commit-territory).
- **Implementation status:** pending
- **Review status:** pending

## Key Insights

1. **This is a partial reversal, not an extension.** That decision deleted `client/hookspan.go` +
`client/spanbuilder.go` and declared dev sessions span-less; `contracts/dev-event/MAPPING.md:239-245`
lists `status`, `span_id`, `trace_id`, `attributes` as "Retired with the span layer". Both statements
stop being true. a decision record that only adds fields would leave two authoritative documents
contradicting the code.
2. **`semantic_type` is not assertable.** Core overwrites it for every span before policy
evaluation (`openbox-core internal/services/governance_workflow.go:302-304`:
`Spans[i].SemanticType = ComputeSemanticTypeFromSpan(&Spans[i])`). The only classifier paths
returning `llm_completion` are `isLLMCall` (attributes `http.method=="POST"` + `http.url`
containing an LLM domain — `internal/content/session.go:451-476`) and the root-field twin
(`session.go:236-256`). So the span must carry **synthesized transport attributes** to be
classified. That is a claim about a request the hook never observed → that decision must name it
as a classification key, not an observation. This is the decision's real cost and OD-0018-1.
3. **The response body shape is dictated, not chosen.** Core's extractor unmarshals
   `{"choices":[{"message":{"content":"…"}}]}` and returns `choices[0].message.content`
   (`openbox-core internal/services/goal_alignment_session.go:73-88`). Any other shape logs
   "response_body was not valid JSON" and yields "".
4. **The INV-2 amendment is narrower than expected.** Source = the hook payload field
   `last_assistant_message`, so `usage.go`'s transcript projection allowlist and its sentinel
   `TestFinops_NoContentOnWire` are untouched — the sentinel keeps proving exactly what it proved
   before (transcript content cannot reach the wire) and gains one assertion. What IS amended:
   `docs/data-and-privacy.md`'s egress table gains a class, and `hookevent.go:120-129`'s "the
   absence is the safeguard" argument for `last_assistant_message` is deliberately retired for
   that one field (`stop_reason` stays unbound).
5. **`status` is NOT content-gated.** It is an enum derived from hook identity / a structural
marker. With content capture off, `status` still ships and Tool Health still works; only the
span disappears. Stating this crisply in that decision prevents a later "gate everything" edit.

## Requirements

- R1: decision record follows the house shape of that decision/0014/0017 (Status/Date/Context/Decision/
Consequences/Alternatives/What this does NOT change), cites repo `file:line` or upstream URLs
for every load-bearing claim (repo rule), and is added to `README.md`.
- R2: Covers all five subjects the plan needs authority for:
  (i) the turn-span content carrier + why (AGE feeder; core unchangeable per scope decision);
  (ii) reviving span rows server-side as an accepted side effect;
  (iii) the INV-2 amendment (assistant text egresses, gated + redacted + capped) and the exact
  scope of the allowlist change (hook field, not transcript);
  (iv) the `status` field addition and its non-gated nature;
  (v) exactly what egresses when `content_capture:false` — **nothing new** (no span, no
  `response_body`); `status` unchanged either way.
- R3: Names the synthesized classification keys explicitly, with the alternative that would
  remove them (a core-side change) recorded as the preferred long-term fix, out of scope by
  user decision.
- R4: Records the composition of gates: span requires `finops:true` (the turn event itself is
  finops-gated, `adapters/claude-code/hookrun.go:174`) AND `content_capture:true`; redaction
  requires `secret_detection` (default on) — with it off the text egresses unredacted, same
  honesty posture as the prompt today.
- R5: Records the Codex deferral with its reason, so the next reader does not read the absence
  as an oversight.
- R6: Marks the empirically-unverified claims (testbed has not run; AGE requires LlamaFirewall)
  rather than asserting the widgets will populate.

## Architecture

Data flow that decision must describe (both are one-way, additive to existing
events):

```
status (P2):
  PostToolUse hook ──▶ adapter derives enum (hook identity | structural marker)
    ──▶ DevEvent.Status ──▶ payload.status ──▶ core ExtractToolMetric.IsSuccess
    ──▶ observability_metrics tool.<name>.success ──▶ FE SUCCESS%
  side effect: governance_events.workflow_status on that activity row
    (openbox-core .../storage_event.go:416-418)

assistant span (P3), content_capture ON only:
  Stop/SubagentStop.last_assistant_message
    ──▶ hookflow redact (decision/) ──▶ capBody(64KB) ──▶ DevEvent.Content.Output
    ──▶ payload.spans[0].response_body = {"choices":[{"message":{"content":…}}]}
    ──▶ core ComputeSemanticTypeFromSpan ⇒ llm_completion
    ──▶ AGE AppendAssistantMessage (Redis goal session, created by prompt_submitted)
    ──▶ next prompt_submitted OR WorkflowCompleted ⇒ performTraceCheck (LlamaFirewall)
    ──▶ GoalAlignmentChecked/GoalDrifted ──▶ observability_metrics goal_alignment
        + age_evaluations.goal_drift ──▶ both widgets
  side effects: spans table rows + span-level Merkle leaves + age_span_evaluations
    for dev sessions (openbox-core .../storage_event.go:146,189-197)
```

Ordering dependency to record: core's append is a Lua script that **returns 0 when no goal
session exists** (`openbox-core internal/services/goal_alignment_session.go:33-41`), and the
session is created by `SignalReceived` with non-empty `signal_args` (`age.go:113-137`) — which
shift-left sends only under content capture. So the gate composes correctly in both directions:
capture off ⇒ no session created AND no span attached; capture on ⇒ prompt then turn, in that
order, is what a Claude Code session naturally produces.

## Related code files

Read-only for this phase; every claim in that decision must cite one of
these.

| Path | Why |
|---|---|
| `client/payload.go:28-78` | struct that gains `Status`, `Spans`, `SpanCount` |
| `client/payload.go:319-370` | `turnActivityOutput` — the INV-2 "four numbers and one identifier" statement to amend |
| `client/payload.go:677-690` | `stripContent` — the choke point that makes the gate true |
| `client/payload.go:694-715` | `capBody` / 64KB cap |
| `client/event.go:130-199` | `Span` (already has `ResponseBody`), `Content.Output` (already declared) |
| `adapters/claude-code/hookevent.go:119-136` | the "absence is the safeguard" comment being retired for one field |
| `adapters/claude-code/usage.go:30-54` | the INV-2 allowlist this decision record does **not** widen |
| `adapters/claude-code/hookrun.go:172-184,273-320` | finops gate + `emitTurn` step order |
| `adapters/codex/capabilities.go:15`, `adapters/codex/hookevent.go:69-76` | Codex deferral evidence |
| `contracts/dev-event/MAPPING.md:239-245` | "Retired with the span layer" text to amend (phase 4) |
| `docs/data-and-privacy.md:5-25` | the egress table this changes |
| `that decision…md`, `…that decision…md` | the two decision records amended |

## Implementation Steps

1. Read  end to end for the house shape (it is the
   closest analogue: a reversal with named non-re-litigable consequences).
2. Draft :
   - **Status:** Accepted · **Date:** 2026-08-13 · Amends that decision (span layer, partially) and
     That decision (INV-2 allowlist statement).
   - **Context:** the three empty widgets + the two producer gaps, each with its core-side
     `file:line`; the scope decision (shift-left only) and what that forecloses.
   - **Decision 1 — `status`:** enum {`completed`,`failed`}, tool `ActivityCompleted` only,
     structural derivation only, NOT content-gated, client-side value allowlist.
   - **Decision 2 — one turn span:** exactly one, `stage:"completed"`, deterministic
     `span_id`/`trace_id`, `hook_trigger` deliberately unset, `response_body` = the OpenAI-chat
     shape core demands, `activity_id` and every existing field byte-unchanged.
   - **Decision 3 — the synthesized classification keys** (OD-0018-1): what they are, why they
     are unavoidable in this scope, that they are inferences and marked as such on the span, and
     the core change that would delete them.
   - **Decision 4 — INV-2 amendment:** the new content class, its gate, its redaction, its cap;
     explicit statement that the transcript allowlist and its sentinel are unchanged, and that a
     future transcript-bound string still needs its own amendment.
   - **Consequences:** span rows + Merkle span leaves + `age_span_evaluations` return for dev
     sessions; OPA/Guardrails now see a `spans` array and the assistant text on turn events
     (upside: policy-visible; risk: a policy keyed on `http_url` matches every turn — and cannot
     block anything, since `Stop` never gates); compliance evidence exports gain
     `workflow_status` on activity rows (`openbox-backend src/modules/compliance/compliance-source-evidence.service.ts:141`).
   - **Alternatives rejected:** (a) transcript-projection widening for the text — costs the
     sentinel's structural guarantee for no gain; (b) `activity_output.message` instead of a span
     — needs the core change that scope forbids; (c) `metadata.status` promotion mirroring
     `exit_code` — a typo silently zeroes the success metric; (d) random span ids — a re-reported
     turn would look like a new span to core's `(span_id, stage)` dedupe.
   - **What this does NOT change:** `activity_id` derivation and the approval keys
     (`client/approval_key_pin_test.go`, `client/turn_key_pin_test.go`), the tool
     `activity_input`/`activity_output` shapes, the enforce cascade, Codex behaviour, the
     `ResolveTier2`-class deprecated keys, `docs/data-and-privacy.md`'s "Tool output: never" row
     (assistant text is not tool output).
   - **Unverified:** testbed has not run; AGE needs `LlamaFirewallHost` set or nothing is ever
     checked (`openbox-core internal/services/llama_firewall.go:31-34`).
3. Add the row to `README.md` in the existing format.
4. Re-verify every citation by opening the cited line (repo rule: reading is the minimum, and a
stale citation in a decision record outlives
the code).

## Todo list

- [ ] Read that decision for shape; skim that decision
- [ ] Draft that decision with all four decisions + consequences + alternatives + non-changes
- [ ] Name OD-0018-1 explicitly and record the preferred long-term core fix
- [ ] Record gate composition (finops ∧ content_capture; redaction under secret_detection)
- [ ] Record the Codex deferral and its reason
- [ ] Add to `README.md`
- [ ] Verify every `file:line` citation resolves

## Success Criteria

-  exists, Accepted, dated 2026-08-13, listed in
`README.md`.
- Every claim carries a repo `file:line` or an upstream URL; each citation resolves to the
  asserted content (spot-check all core-side ones — they are the ones this repo cannot test).
- A reader who knows only that decision can answer, from that decision alone: what egresses now, under
  which gates, what returns server-side, why `semantic_type` is not simply asserted, and which
  test protects the INV-2 line that did NOT move.
- Contains no implementation detail that phases 2/3 would have to keep in sync (no code, no
  struct listings beyond field names and JSON keys).

## Risk Assessment

| Risk | L×I | Mitigation / signal & pre-decided response |
|---|---|---|
| decision record asserts core behaviour that later proves wrong (all core evidence is read, not run) | M×H | Every core claim tagged with its `file:line` and marked "verified by reading openbox-core @ develop 68f0398, not by a run". **Signal:** phase 5's live check shows `goal_alignment` rows still absent with LlamaFirewall up. **Response:** adjust in-plan — amend decision record with the observed behaviour; do not delete that decision |
| OD-0018-1 rejected after that decision is written | L×M | decision record is structured so decision 3 is a separable section. **Signal:** user rejects synthesized keys. **Response:** stop-and-replan phase 3 only; phases 2/4/5 survive unchanged |
| decision record grows into a design doc and drifts from the code | M×M | Field names + JSON keys only, no Go. Phase 4 owns the contract docs |
| "Amends that decision" read as "reverts that decision" | M×M | Explicit scope sentence: spans return for ONE carrier, tool events stay span-less, `hookspan.go`/`spanbuilder.go` stay deleted |

## Security Considerations

- that decision is the security artifact for this plan: it is where the new egress class is recorded.
  Understating it (e.g. "adds telemetry fields") is the failure mode this repo names explicitly —
  prefer the honest limit.
- Must state that the assistant text is redacted by local secret detection **before** attach
  (the repo's only in-transit control, precedent `adapters/*/enforcetarget.go` + conformance C18)
  and that with `secret_detection:false` it egresses unredacted.
- Must state that the 64KB `capBody` cap means the server sees at most the first 64KB of a turn's
  text, and that thinking blocks are never captured (the provider's own OTel export redacts them
  unconditionally — keep that stance).
- Must NOT imply the credential file or the transcript gained protection; nothing here changes
  either.

## Next steps

Phase 2 (`status` end-to-end) — it is the smaller, non-content change and it lands the schema
bump that phase 3 then reuses.
