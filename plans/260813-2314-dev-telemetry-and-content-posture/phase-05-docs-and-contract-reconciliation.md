# Phase 05 — contract surfaces, docs, CLAUDE.md reconciliation

## Context links

- Plan: [plan.md](plan.md) · Depends: phases [02](phase-02-status-on-tool-results.md),
  [03](phase-03-failure-and-lifecycle-hooks.md), [04](phase-04-assistant-turn-span.md)
  (documents what SHIPPED, not what was planned)
- Authority: `docs/adr/ADR-0018-dev-turn-content-carrier.md` (phase 01)
- Design owner: superseded plan 2200 phase-04, widened for the phase-03 events

## Overview

- **Date:** 2026-08-13 · **Priority:** P1 · **Status:** pending · **Effort:** 2.5h
- Make every document asserting "dev sessions are span-less", "status is retired", "tool output
  never egresses", "core's usage extractor is unmerged" true again, and map the new failure/
  lifecycle events. Four contract surfaces + four docs + CLAUDE.md. No behaviour change. A
  governance product whose docs overstate or understate itself is the failure it exists to
  prevent.

## Key Insights

1. **Two load-bearing privacy paragraphs go from true to false** the moment phase 04 merges:
   `docs/data-and-privacy.md:24-34` ("the channel cannot be re-opened by an adapter mistake plus
   a content-capture opt-in") and `docs/architecture.md:209-221` ("no `spans` rows at all … no
   server-side `semantic_type` classification").
2. **The stale extractor note is independently confirmed:** `ExtractToolMetric` on core `develop`
   @ 68f0398 already excludes `llm_completion` (`errors.go:320-322`) — CLAUDE.md's "PR #125 not
   yet merged" and architecture.md's "awaiting merge" are stale. Re-verify against the sibling
   checkout at edit time; record the HEAD sha used.
3. **MAPPING.md §3 is wrong in three rows** after the code lands: `span.response_body` ("dropped
   as an egress channel"), `span.stage` ("retained, read by nothing"), the "Retired with the span
   layer" list (`status`, `span_id`, `trace_id`, `attributes`) — `MAPPING.md:234-245`.
4. **The machine-readable schema moved in phases 02–03 (v1.2).** This phase edits only the prose
   describing it and checks the two agree (MAPPING's own rule: fixture ↔ table,
   `MAPPING.md:247-251`).
5. **New events need rows, not just corrections:** failed ToolResult (status), the three
   `SignalReceived` names, Task `subagent_type`, and the **re-init upgrade note** (existing
   installs don't fire the new hooks until `openbox init` re-runs).

## Requirements

- R1: Every "no spans / no status / tool output never / usage write-only" claim either still true
  or rewritten with the ADR-0018 link.
- R2: MAPPING.md gains: `status` envelope row (tool completed only, ungated); the turn-span rows
  (`content.output` → `spans[0].response_body`, gated + redacted + capped; deterministic ids;
  `hook_trigger` deliberately absent); the three new signal rows + failed-ToolResult row; the
  amended retired-list; §semantic_type retitled (core COMPUTES it for the span; wire value
  ignored — classifier path cited); §7 live-run list gains: `status` → `workflow_status` +
  `tool.<name>.success`; spans row `span_type='llm_completion'`; `age_evaluations` row;
  `goal_alignment` metric keys; the new signal names in `governance_events`.
- R3: COVERAGE.md §2 gains the derivations: status (per the verified branch), assistant text →
  span response_body (gate chain finops ∧ content_capture ∧ window-has-usage; redaction ordering),
  error_type enumOr, classifier_verdict tri-state. §3 bounded non-goals: Codex alignment feed +
  Codex tool-success; `denial_reason`/`error_message`/`stop_reason`/thinking → ADR-0019.
- R4: `docs/data-and-privacy.md`: table row **Assistant response text — yes, with capture on (per
  model turn), redacted, 64KB cap**; "Tool output: never" row unchanged (still true — ADR-0019 P1
  is the change that would touch it); the span paragraph rewritten honestly (a response-body
  channel exists again for ONE carrier; `secret_detection:false` ⇒ unredacted); capture-on/off
  lists updated; new signal events noted as metadata-only.
- R5: `docs/architecture.md` assurance: event-level plus ONE span per model turn under capture;
  span leaves return for those; `semantic_type` server-computed; usage-extractor status corrected.
- R6: CLAUDE.md: the span-less line (`:23-25`), PR-#125 note, known limits (alignment requires
  finops ∧ content_capture ∧ LlamaFirewall reachable; both gates default-ON is a user constraint),
  one Current-state paragraph carrying the non-re-litigable points + honest status ("implemented,
  unit + conformance verified, **testbed has NOT run**").
- R7: README: grep `span|telemetry|metadata|assistant|never`; edit only what is actually asserted.
- R8: No claim without `file:line`/URL; no claim a live run happened; docs written from the
  shipped diff + fixtures, never from plan files.

## Related code files

Read (confirm what shipped): `client/{payload,event,turnspan}.go`,
`adapters/claude-code/{hookevent,mapper,hookrun,installer}.go`, `adapters/codex/capabilities.go`,
`contracts/dev-event/schema/dev-event.schema.json`, `client/testdata/golden/`.

Edit: `contracts/dev-event/MAPPING.md` (§1, turn pair §, semantic_type §, §3, §7) ·
`contracts/dev-event/COVERAGE.md` (§2, §3) · `docs/data-and-privacy.md` (table, span paragraph,
capture section) · `docs/architecture.md` (assurance) · `CLAUDE.md` (core-principle line,
Current state, known limits) · `README.md` (only if asserting).

## Implementation Steps

1. Read the shipped diffs + goldens; note actual field names/keys (plans are stateful records,
   not product authority).
2. MAPPING.md in section order; every new row carries its gate in the notes column; one bounding
   sentence on the retired-list amendment (one carrier; family tuples + `AssertHookWireShape`
   stay deleted).
3. COVERAGE.md §2/§3.
4. data-and-privacy.md: table row first, then the paragraph rewrite — it must read as a
   deliberate widening with three mitigations (gate, redaction, cap), not an improvement. Cite
   ADR-0018; name the `secret_detection:false` case.
5. architecture.md assurance bullets; correct the extractor status against the sibling checkout
   (record sha).
6. README grep + minimal edits.
7. CLAUDE.md last (it summarizes the others): span-is-one-carrier; semantic_type recomputed
   (deleting the synthesized keys silently kills the feature); status ungated + tool-only
   (because `payload.Status` also writes `workflow_status`); INV-2 transcript line did NOT move;
   new hooks need re-init; the filed core ask retires the span. Honest status line.
8. Verify every citation resolves; `go test ./...` still green (contracts module reads the schema).
9. Commits: `docs: reconcile the span-less and status claims with ADR-0018` +
   `docs(contracts): map status, failure signals and the turn content span`.

## Todo list

- [ ] Shipped diffs + goldens read; keys confirmed
- [ ] MAPPING.md §1 / turn pair / semantic_type / §3 (corrections + new rows) / §7
- [ ] COVERAGE.md §2 derivations + §3 non-goals
- [ ] data-and-privacy.md table + paragraph + capture lists
- [ ] architecture.md assurance + extractor status (sha recorded)
- [ ] README verified; CLAUDE.md paragraph + limits + re-init note
- [ ] Citations verified; tests green

## Success Criteria

- `grep -rn "no spans\|span-less\|Retired with the span layer\|cannot be re-opened" README.md docs/ contracts/ CLAUDE.md`
  returns only amended text.
- data-and-privacy table lists the assistant-text class with its gates; "Tool output: never"
  unchanged and still true.
- MAPPING §3 rows and golden fixtures agree (the file's own rule).
- CLAUDE.md contains no testbed-ran claim and no stale PR-#125 statement.
- A reviewer can reconstruct from docs alone exactly what a session sends with
  `content_capture:false` vs `true`, and which events exist only after re-init.

## Risk Assessment

| Risk | L×I | Mitigation / pre-decided response |
|---|---|---|
| Docs describe the plan rather than the code | M×H | Step 1 mandates the shipped diff. Signal: documented key absent from goldens. Response: fix the doc |
| Privacy claim softened into overstatement | M×H | Every mitigation sentence pairs with a limit sentence; `secret_detection:false` named |
| Core-status correction itself stale by edit time | M×M | Re-verify sibling checkout, record sha, date the claim |
| Retired-list amendment read as span-layer revival | M×M | Bounding sentence (one carrier, minimal fields) |
| CLAUDE.md grows past usefulness | M×L | One paragraph, four bullets, one status line (ADR-0013/14/17 shape) |

## Security Considerations

- This phase IS the disclosure. Prefer the honest limit over the confident sentence. Redaction is
  pattern/entropy-based and only when enabled; the prompt still egresses unredacted (existing
  limit — state as unchanged). Returned span rows add stored content + Merkle leaves, not
  assurance — say both. No real credentials/paths in examples.

## Next steps

Phase 06 — the manual verification guide + dormant testbed assertions.
