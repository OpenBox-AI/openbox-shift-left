# Phase 04 — ADR-0018 content-posture draft

## Context links

- Parent: [plan.md](plan.md) · Depends: none (doc work; sequenced after P0 per user request)
- Decision source: `plans/reports/research-260813-2215-session-content-capture-gaps.md` §"Decision update (2026-08-13 22:34)"
- Style/argument precedent: ADR-0015 (argues against the prior rationale, not around it), ADR-0014 (allowlist amendment mechanics), ADR-0017 (posture change disclosure)

## Overview

- Date: 2026-08-13 · Priority: P1 · Status: pending · Review: n/a
- Draft `docs/adr/ADR-0018-full-content-capture.md` (status: **Proposed**) authorizing P1–P3 capture.
  DRAFT ONLY — no P1–P3 implementation in this plan.

## Key Insights

- The ADR's hard job is retiring SL3-SEC-3 and restating INV-2 honestly: from "content has no field to land in"
  to "content egresses only under the org gate, always secret-redacted first, always 64KB-capped". Must argue
  against the original metadata-only rationale (ADR-0015 style), citing the owner's org-deployment trust model.
- Thinking capture requires amending ADR-0014's transcript allowlist — fold the amendment INTO ADR-0018
  (one decision record for one posture change) rather than a sibling; ADR-0014's sentinel-test rule stays:
  the test evolves to assert redaction+cap instead of absence, and a trivially-passing version remains a defect.
- data-and-privacy.md rows flip from "never" to gated — the doc rewrite is part of the ADR's Consequences, listed file by file.

## Requirements (ADR content outline)

1. **Context**: org-governance deployment model (company machine/code); owner decision 2026-08-13; what evaluation
   needs the data (policy /evaluate, Guardrails stage 0/1, AGE goal alignment) — cite research report.
2. **Decision**: capture ALL available session content by default under the existing `content_capture` master gate
   (no new config keys): tool inputs on the observe path, `tool_response` + structured `tool_output`, `denial_reason`/
   `error_message` (deferred P0 strings), `last_assistant_message` + `stop_reason`, transcript window assistant text +
   thinking blocks. Per-phase field map (P1/P2/P3) with exact wire destinations (`activity_input.*`, `activity_output.output`,
   `activity_output.message`, `activity_output.thinking[]`).
3. **Invariants restated**: INV-2 v2 wording; SL3-SEC-3 retired (named, with the conformance cases that pinned it
   updated deliberately); redact-before-attach extended to every class (C18 pattern per class, asserted on outbound
   bytes); capBody 64KB unchanged; event identity untouched; `stripContent`/`Emit` remains the single choke point.
4. **What full capture cannot get** (honesty section): system prompts / raw API bodies (only CC's own OTel channel —
   complementary-collector option noted), per-model-call token granularity, provider-side redactions.
5. **Alternatives considered**: per-class flags (provider precedent — rejected for now, KISS single gate, revisit on
   customer ask); metadata-only status quo (rejected by owner decision); CC OTel-collector-only approach (rejected:
   different pipeline, loses evaluate-path coupling).
6. **Consequences**: doc rewrites (data-and-privacy.md, MAPPING.md, COVERAGE.md, README, CLAUDE.md), storage/retention
   core ask (64KB-class events), core AGE ask (read assistant content from llm_completion `activity_output` for
   span-less dev sessions — unblocks Goal Alignment/Drift widgets), server-side dedupe ask unchanged, **Codex parity
   gap** disclosed (no failure hook, per-session usage, output surface TBD), Guardrail-redaction-at-source still
   not wired (existing limit, now higher stakes — restate).
7. **Sequencing**: P1 tool output → P2 assistant text + stop_reason (+ freed P0-deferred strings) → P3 thinking +
   observe-path inputs + allowlist amendment. Each phase lands with its conformance case before flush code.

## Architecture

Doc-only. `docs/adr/ADR-0018-full-content-capture.md` + `docs/adr/README.md` index row.

## Related code files

- `docs/adr/ADR-0018-full-content-capture.md` (new), `docs/adr/README.md`
- Referenced (unmodified this phase): `client/event.go` Content, `client/payload.go` stripContent/capBody,
  `adapters/claude-code/{hookevent,mapper,usage}.go`, `decision/`, `contracts/dev-event/MAPPING.md`

## Implementation Steps

1. Draft ADR per outline; every claim cites repo symbol/path or the research report (repo doc rule).
2. Cross-check no contradiction with ADR-0013/0014/0015/0016/0017 "worth not re-litigating" lists; where ADR-0014 is
   amended, say so explicitly in both files (amendment note in 0014 header pointing to 0018).
3. Index row in README.md; keep ADR ≤ house length (~0017 size).
4. Present to owner for acceptance (status stays Proposed until then).

## Todo list

- [ ] ADR-0018 drafted per outline
- [ ] ADR-0014 amendment cross-note
- [ ] README index
- [ ] Owner review requested

## Success Criteria

- ADR self-consistent with research report + decision memory; names every retired guarantee and every updated test;
  P1–P3 implementable from it without re-deciding anything.

## Risk Assessment

- Scope creep into implementation — guard: this phase produces exactly two file changes under `docs/adr/`.
- Over-promising: ADR must carry the "cannot get" section so docs never claim fuller capture than surfaces allow.

## Security Considerations

- The ADR itself makes redaction the load-bearing control — it must state the ordering requirement (detect → redact →
  attach → sign) per class and require a conformance case per class before any flush code merges.

## Next steps

After owner accepts ADR-0018 → new plan for P1 (tool output) implementation.
