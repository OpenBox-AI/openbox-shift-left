# Phase 07 — that decision: full-content-capture posture draft (Proposed)

## Context links

- Plan: [plan.md](plan.md) · Depends: phases [04](phase-04-assistant-turn-span.md),
  [05](phase-05-docs-and-contract-reconciliation.md) (references what shipped) — sequenced last
  per user request
- Decision source: `plans/reports/research-260813-2215-session-content-capture-gaps.md`
  §"Decision update (2026-08-13 22:34)" — owner posture: FULL CAPTURE
- Design owner: superseded plan 2241 phase-04, **renumbered 0019** (merge decision 2)
- Style precedent: that decision (argues against the prior rationale, not around it)
(allowlist amendment mechanics), that decision (posture-change
disclosure)

## Overview

- **Date:** 2026-08-13 · **Priority:** P1 · **Status:** pending · **Effort:** 3h
- Draft  (**Proposed** — stays Proposed until owner
accepts) authorizing the P1–P3 capture phases. **DRAFT ONLY — no P1–P3 implementation in this
plan.** Cross-references that decision as the posture's first shipped increment.

## Key Insights

1. That decision's hard job is retiring SL3-SEC-3 and restating INV-2 honestly: from "content has no
field to land in" to "content egresses only under the org gate, always secret-redacted first,
always 64KB-capped". Must argue against the original metadata-only rationale (that decision
style), citing the owner's org-deployment trust model (company machine, company code).
2. Thinking capture requires amending that decision's transcript allowlist — fold the amendment INTO
That decision (one decision record for one posture change); the sentinel-test rule stays: the
test evolves to assert redaction+cap instead of absence, and a trivially-passing version remains
a defect.
3. That decision already shipped part of the posture (status; assistant final text via the span
stopgap). That decision must position itself as the umbrella: P2 moves assistant text to
`activity_output.message` once the filed core AGE ask lands, retiring the span's synthesized
keys — one coherent story, no contradiction between the two records.
4. `docs/data-and-privacy.md` rows flip from "never" to gated — the rewrite list is part of the
decision record's Consequences, file by
file.

## Requirements (decision record content outline)

1. **Context:** org-governance deployment model; owner decision 2026-08-13; what evaluation needs
the data (policy /evaluate, Guardrails stage 0/1, AGE goal alignment) — cite the research
report; that decision as shipped increment.
2. **Decision:** capture ALL available session content by default under the existing
   `content_capture` master gate (no new config keys): tool inputs on the observe path,
   `tool_response` + structured `tool_output`, the P0-deferred strings (`denial_reason`,
   `error_message`), `stop_reason`, transcript-window assistant text + thinking blocks. Per-phase
   field map (P1/P2/P3) with exact wire destinations (`activity_input.*`,
   `activity_output.output`, `activity_output.message`, `activity_output.thinking[]`).
3. **Invariants restated:** INV-2 v2 wording; SL3-SEC-3 retired (named, with the conformance
   cases that pinned it updated deliberately); redact-before-attach extended to every class (a
   C18-pattern case per class, asserted on outbound bytes, before any flush code merges); capBody
   64KB unchanged; event identity untouched; `stripContent`/`Emit` stays the single choke point.
4. **What full capture cannot get** (honesty section): system prompts / raw API bodies (only the
   provider's own OTel channel — complementary-collector option noted), per-model-call token
   granularity, provider-side redactions (thinking is redacted in the provider's OTel export but
   present in the local transcript — cite).
5. **Alternatives considered:** per-class flags (provider precedent — rejected for now, KISS
   single gate, revisit on customer ask); metadata-only status quo (rejected by owner decision);
   OTel-collector-only (rejected: different pipeline, loses evaluate-path coupling).
6. **Consequences:** doc rewrites (data-and-privacy.md, MAPPING.md, COVERAGE.md, README,
CLAUDE.md); storage/retention core ask (64KB-class events at tool-call cadence); the filed AGE
activity_output ask (links phase 04's issue — also retires that decision's synthesized keys);
server-side dedupe ask unchanged; **Codex parity gap** disclosed (no failure hook, per-session
usage, output surface TBD); Guardrail-redaction-at-source still not wired (existing limit, now
higher stakes — restate).
7. **Sequencing:** P1 tool output → P2 assistant text on `activity_output.message` + `stop_reason`
   + the freed P0-deferred strings → P3 thinking + observe-path inputs + that decision allowlist
   amendment (staged last because the sentinel is load-bearing). Each phase lands with its
   conformance case before flush code.

## Related code files

-  (new), `README.md` (index row),
`that decision-…md` (amendment cross-note in header pointing at 0019)
- Referenced, unmodified: `client/event.go` (Content), `client/payload.go`
(stripContent/capBody), `adapters/claude-code/{hookevent,mapper,usage}.go`, `decision/`,
`contracts/dev-event/MAPPING.md`, `that decision-…md`

## Implementation Steps

1. Draft per outline; every claim cites a repo symbol/path or the research report.
2. Cross-check no contradiction with that decision/0014/0015/0016/0017/0018 "worth not re-litigating"
lists; where that decision is amended, say so in both files (note in 0014's header pointing
to 0019). Verify that decision relationship reads as increment-then-umbrella, not conflict.
3. Index row in `README.md`; keep ≤ house length (~0017 size).
4. Present to owner for acceptance (status stays Proposed until then). Record in the plan that
   P1–P3 implementation is a NEW plan after acceptance.
5. Commit: `docs(decision record): propose that decision full-content-capture posture`.

## Todo list

- [ ] that decision drafted per outline (Proposed)
- [ ] that decision amendment cross-note; that decision increment cross-reference
- [ ] README index row
- [ ] Owner review requested

## Success Criteria

- decision record self-consistent with the research report, the decision memory, and shipped; names
  every retired guarantee and every test that must evolve; P1–P3 implementable from it without
  re-deciding anything.
- Reading 0018 then 0019 yields one coherent posture story (stopgap → target state), no
  contradiction on thinking, stop_reason, tool output, or the span's lifespan.

## Risk Assessment

| Risk | L×I | Mitigation |
|---|---|---|
| Scope creep into implementation | M×M | This phase produces exactly three file changes under  |
| Over-promising capture | M×H | The "cannot get" section is mandatory; docs never claim fuller capture than the surfaces allow |
| 0018/0019 drift apart later | M×M | Explicit cross-references both directions; the span's retirement condition (core ask) named in both |

## Security Considerations

- that decision makes redaction the load-bearing control: state the ordering requirement (detect →
  redact → attach → sign) per class and require a conformance case per class before any flush
  code merges. Restate that `secret_detection:false` means unredacted egress and that the org
  gate is the only off switch.

## Next steps

Plan complete. After owner accepts that decision → new plan for P1 (tool output). Open
follow-ups for other repos: the filed AGE activity_output ask (phase 04); core success
derivation dead for the Python SDK producer too; server-side dedupe for the lost-200 window;
retention/PII posture for the new content classes (backend).
