# Phase 04 — contract surfaces, docs, CLAUDE.md reconciliation

## Context links

- Plan: [plan.md](plan.md) · Blocked on: [phase 02](phase-02-activity-status-field.md),
  [phase 03](phase-03-assistant-turn-span.md) (documents what shipped, not what was planned)
- Authority for the changes: `docs/adr/ADR-0018-dev-turn-content-carrier.md` (phase 01)
- Evidence for the stale-note fix: [scout-02 §Side note + Unknown 4](scout/scout-02-write-side-core-sdk-shiftleft.md)

## Overview

- **Date:** 2026-08-13
- **Description:** Make every document that asserts "dev sessions are span-less", "tool output
  never egresses", "status is retired" and "core's usage extractor is unmerged" true again. Four
  contract surfaces + four docs + CLAUDE.md. No behaviour change.
- **Priority:** P1 — a governance product whose docs overstate or understate itself is the failure
  it exists to prevent (CLAUDE.md working conventions). Two currently-true paragraphs become
  actively false the moment phase 03 merges.
- **Implementation status:** pending
- **Review status:** pending
- **Effort:** 2h

## Key Insights

1. **Two paragraphs go from true to false**, and both are load-bearing privacy claims:
   - `docs/data-and-privacy.md:24-34`: "The span is now gone entirely (ADR-0013), so the fields are
     not read at all and **the channel cannot be re-opened by an adapter mistake plus a
     content-capture opt-in**." After phase 03 a `response_body` channel exists again on exactly
     one carrier and is opened precisely by a content-capture opt-in.
   - `docs/architecture.md:209-221`: "**no `spans` rows at all** … there is no span-level
     attestation to check, and no server-side `semantic_type` classification".
2. **The stale note is independently confirmed.** `ExtractToolMetric` on openbox-core `develop`
   HEAD 68f0398 already excludes `activity_type=="llm_completion"` via `IsLLMCompletionActivity`
   (`internal/services/activities/observability/errors.go:320-322`, scout-02 §Side note). So
   CLAUDE.md's "green in PR #125 … but not yet merged" and `docs/architecture.md`'s "awaiting merge"
   are both stale. Verify against the sibling checkout at edit time, then state what is true.
3. **MAPPING.md's §3 tail is now wrong in three specific rows**, not just in the prose: the
   `span.response_body` row says "**dropped as an egress channel** … no adapter has ever set
   either"; the `span.stage` row says "**retained, read by nothing**"; and the "Retired with the
   span layer" list names `status`, `span_id`, `trace_id`, `attributes`
   (`contracts/dev-event/MAPPING.md:234-245`).
4. **The schema is machine-readable and already moved in phase 02** (v1.2). This phase does not
   re-edit it — it edits the prose that describes it, and checks the two agree.
5. **README appears clean** (grep for `span`/`metadata-only`/`assistant` returns nothing) — verify
   rather than assume, and only edit if a telemetry claim is actually stated there.

## Requirements

- R1: Every claim of the form "no spans / no status / tool output never / usage write-only" is
  either still true or rewritten, with the ADR-0018 link.
- R2: MAPPING.md gains the new wire rows (`status`, `spans[0].*`, `span_count`) and the amended
  retired-list, and its §7 "what a live run must confirm" list gains the new items.
- R3: COVERAGE.md's field-derivation rules cover the two new derivations (status; assistant text →
  span `response_body`) including their gates.
- R4: `docs/data-and-privacy.md` gains a row for the new egress class and rewrites the
  span-is-gone paragraph honestly (redacted, capped, gated, one carrier, and what
  `secret_detection:false` means).
- R5: `docs/architecture.md` assurance section: span rows/leaves return for turn events under
  capture; the usage-extractor status corrected.
- R6: CLAUDE.md: the span-less line, the PR #125 note, the known-limits list, and a short
  "current state" paragraph for this change with its honest verification status.
- R7: No claim without a `file:line` or upstream URL. No new claim that a live run has happened.

## Architecture

Ownership map — one surface per bullet, no overlap with phases 02/03 (which own the machine-readable
schema and the code):

```
contracts/dev-event/MAPPING.md   §1 envelope, §2 turn pair, §3 field homes + retired list,
                                 §semantic_type-none, §7 live-verification list
contracts/dev-event/COVERAGE.md  §2 field-derivation rules, §3 bounded non-goals
docs/data-and-privacy.md         short-version table, the ADR-0013 span paragraph,
                                 Content capture section
docs/architecture.md             §Assurance — span-level bullet, usage-extractor bullet
README.md                        verify-only; edit only if it states a telemetry/span claim
CLAUDE.md                        core-principle line 24-25, Current state, Known limits
```

## Related code files

Read (to confirm what shipped) — do not edit:
`client/payload.go`, `client/turnspan.go`, `client/event.go`,
`adapters/claude-code/{hookevent,mapper,hookrun}.go`, `adapters/codex/capabilities.go`,
`contracts/dev-event/schema/dev-event.schema.json`, `client/testdata/golden/`.

Edit:

| Path | Change |
|---|---|
| `contracts/dev-event/MAPPING.md:20-49` (§1) | add the `status` envelope row (tool completed only, ungated) |
| `…MAPPING.md:139-190` (turn pair) | the completed half MAY carry one span under content capture; response-body shape; deterministic ids; `hook_trigger` deliberately absent |
| `…MAPPING.md:191-209` (`semantic_type`: none) | retitle/amend: none for tool events; for the turn span core COMPUTES it and the wire value is ignored — with the classifier path cited |
| `…MAPPING.md:210-251` (§3 field homes) | new rows: `content.output` → `spans[0].response_body` (gated, redacted, capped); `span.stage` row corrected (read again, for one carrier); `span.response_body` row corrected (an adapter DOES set it now, via `Content.Output`); amend the "Retired with the span layer" list — `status` and the minimal span id/attribute set return, the family tuples and `AssertHookWireShape` stay retired |
| `…MAPPING.md:299-360` (§7) | add to what a live run must confirm: `status` lands in `workflow_status` + `tool.<name>.success`; the span stores a `spans` row with `span_type='llm_completion'`; `age_evaluations` gains a row; `observability_metrics` gains `goal_alignment` keys |
| `contracts/dev-event/COVERAGE.md:27-48` | §2: status derivation + its branch; assistant-text derivation, gate chain (finops ∧ content_capture ∧ window-has-usage), redaction ordering. §3: Codex alignment feed as a named bounded non-goal |
| `docs/data-and-privacy.md:5-19` | new table row: **Assistant response text** — "yes, with capture on (per model turn)"; note redaction + 64KB cap. Keep "Tool output: never" — still true |
| `docs/data-and-privacy.md:24-38` | rewrite the ADR-0013 paragraph: what changed, that a response-body channel exists again for one carrier, that it is redacted before send, and that `secret_detection:false` means unredacted |
| `docs/data-and-privacy.md:106-131` (Content capture) | the new class in the capture-on/off lists |
| `docs/architecture.md:209-221` | amend: event-level plus ONE span per model turn under capture; span leaves return for those; `semantic_type` is server-computed for that span |
| `docs/architecture.md:222-232` | correct the usage-extractor status against the sibling checkout |
| `CLAUDE.md:23-25` | "Dev sessions write no `spans` rows" → no spans EXCEPT the one turn-content carrier (ADR-0018) |
| `CLAUDE.md:229-247` | correct the PR #125 / write-only claim |
| `CLAUDE.md:264-274` | known limits: add "alignment requires finops ∧ content capture ∧ LlamaFirewall reachable"; drop or correct the stale extractor limit |
| `CLAUDE.md` Current state | one paragraph for ADR-0018 with the four non-re-litigable points (see steps) and an honest status line |

## Implementation Steps

1. Read what actually shipped (phases 02/03 diffs + the new golden fixtures). Document from the
   code, never from these plan files — a plan is a stateful record, not product authority.
2. MAPPING.md, in section order, per the table above. Keep the existing table formats; every new
   row carries the gate in its notes column.
3. COVERAGE.md §2/§3.
4. `docs/data-and-privacy.md`: table row first, then the paragraph rewrite. The rewrite must not
   read as an improvement — it is a widening, deliberately chosen, with three mitigations
   (gate, redaction, cap). Cite ADR-0018.
5. `docs/architecture.md` assurance bullets.
6. `README.md`: grep for `span`, `telemetry`, `metadata`, `assistant`, `never`; edit only what is
   actually asserted.
7. CLAUDE.md last (it summarises the others). The ADR-0018 paragraph should carry the points worth
   not re-litigating:
   - the span is ONE carrier for AGE, not a revival of the span layer (`hookspan.go`/`spanbuilder.go`
     stay deleted);
   - `semantic_type` is recomputed server-side, which is why the span carries synthesized
     classification keys — and why deleting them silently kills the feature;
   - `status` is ungated and tool-only, because `payload.Status` also writes `workflow_status`;
   - the INV-2 line that did NOT move: the transcript projection and its sentinel; the source is a
     hook payload field.
   Plus the honest status line: implemented, unit + conformance verified, **testbed has NOT run**,
   alignment additionally requires a reachable LlamaFirewall.
8. Verify every citation resolves, and every openbox-core claim against the sibling checkout at
   edit time (record the HEAD sha used, as scout-02 did).
9. Commit: `docs: reconcile the span-less and tool-output claims with ADR-0018` +
   `docs(contracts): map status and the turn content span`.

## Todo list

- [ ] Read the shipped diffs and goldens; note the actual field names/keys
- [ ] MAPPING.md §1, turn pair, `semantic_type`, §3 (3 corrected rows + 3 new), §7 list
- [ ] COVERAGE.md §2 derivations + §3 Codex non-goal
- [ ] `docs/data-and-privacy.md` table row + ADR-0013 paragraph rewrite + capture section
- [ ] `docs/architecture.md` assurance: span bullet + usage-extractor status
- [ ] README verified (edited only if it asserts something)
- [ ] CLAUDE.md: line 24-25, Current state paragraph, known limits, PR #125 note
- [ ] Every citation re-verified; core HEAD sha recorded
- [ ] `go test ./... ` still green (docs-only, but the contracts module tests read the schema)

## Success Criteria

- Grep finds no surviving claim that dev sessions produce zero spans, that tool `status` is
  retired, or that a response body cannot carry content: `grep -rn "no spans\|span-less\|Retired with the span layer\|cannot be re-opened" README.md docs/ contracts/ CLAUDE.md`
  returns only amended text.
- `docs/data-and-privacy.md`'s table lists the assistant-text class with its gate; the "Tool
  output: never" row is unchanged and still true.
- MAPPING.md §3's rows and the golden fixtures agree — the file's own rule: "If a fixture carries a
  field this table does not list, one of the two is wrong" (`MAPPING.md:247-251`).
- CLAUDE.md contains no claim that the testbed ran, and no stale PR-#125 statement.
- A reviewer can reconstruct, from the docs alone, exactly what a session sends with
  `content_capture:false` versus `true`.

## Risk Assessment

| Risk | L×I | Mitigation / signal & pre-decided response |
|---|---|---|
| Docs describe the plan rather than the code | M×H | Step 1 mandates reading the shipped diff + fixtures first. **Signal:** a documented key absent from `client/testdata/golden/`. **Response:** fix the doc, not the code |
| A privacy claim is softened into an overstatement ("we redact everything") | M×H | Rewrite must name the `secret_detection:false` case and the 64KB cap. Reviewer check: every mitigation sentence has a limit sentence |
| The core-status correction is itself stale by edit time | M×M | Re-verify against the sibling checkout and record the HEAD sha (scout-02's practice). **Signal:** claim disagrees with `errors.go`. **Response:** state what the checkout shows and date it |
| Amending MAPPING.md's retired list reads as reviving the span layer | M×M | One sentence bounding it: one carrier, minimal field set, `AssertHookWireShape` and the family tuples stay deleted |
| CLAUDE.md grows past usefulness | M×L | One paragraph, four bullets, one status line — the shape ADR-0013/0014/0017 paragraphs already use |

## Security Considerations

- This phase IS the disclosure. Understating the new egress class here is the product failure this
  repo names most often — prefer the honest limit over the confident sentence.
- Do not let the privacy doc imply that redaction is complete: local secret detection is
  pattern/entropy based, runs only when enabled, and the prompt on the same session still egresses
  unredacted (an existing limit — state it as unchanged, do not silently fix or hide it).
- Do not imply the returned span rows add assurance: they add stored content and Merkle leaves.
  Say both.
- No credential, path, or token values in any example. Examples must use obviously-fake ids.

## Next steps

Phase 05 — the manual verification guide, which the user runs.
