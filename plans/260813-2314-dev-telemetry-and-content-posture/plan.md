--- title: "Dev-session telemetry: status + failure hooks + widget feeds + content-posture decision record" description: "Merged plan: tool status & failure/lifecycle events (P0), one content-gated assistant-turn
span feeding the alignment widgets, that decision carrier (Accepted) + that decision full-capture posture (Proposed)" status: complete priority: P1 effort: 21h branch: main tags: [telemetry, contracts, privacy,
decision record, claude-code-adapter] created: 2026-08-13 ---

# Dev-session telemetry + content-posture decision record (merged)

**Supersedes** [260813-2200-dashboard-widget-telemetry-gaps](../260813-2200-dashboard-widget-telemetry-gaps/plan.md)
and [260813-2241-p0-tool-status-content-decision record](../260813-2241-p0-tool-status-content/plan.md), which
overlapped (duplicate `status` implementation, two competing "that decision" drafts). Their scout / research
artifacts remain the evidence of record and are linked below, not copied.

Scope: **openbox-shift-left only**. Core / backend / FE unchanged; their defects become filed or
documented follow-ups.

| Widget / gap | Root cause | Fix |
|---|---|---|
| Tool Health SUCCESS 0.0% | core counts success only on top-level `status=="completed"` (`openbox-core .../observability/errors.go:332-334`); `client/payload.go:28-78` has no `status` key → `.failed` increments on every completion | phases 2–3 |
| Failures/denials/API errors invisible | `PostToolUseFailure`, `SubagentStart`, `PermissionDenied`, `StopFailure` hooks unwired (adapter wires 7 of ~25) | phase 3 |
| Goal Alignment Trend + Recent Drift empty | AGE reads assistant text ONLY from `payload.Spans` with `Stage=="completed" && SemanticType==llm_completion && ResponseBody!=nil` (`openbox-core .../goal_alignment_session.go:64-88`); dev sessions are span-less | phase 4 (span stopgap) + filed core ask |
| Content posture undocumented | Owner decided FULL CAPTURE 2026-08-13 22:34 (research report §Decision update); no record records it | phase 7 (that decision, Proposed) |

## Phases

Strictly sequential (phases 2–4 share `client/payload.go`, `client/testdata/golden/`, the schema;
5–7 share `docs/`).

| # | Phase | Effort | Status | Blocks on |
|---|---|---|---|---|
| 1 | [that decision — dev-turn content carrier (Accepted)](phase-01-dev-turn-content-carrier.md) | 1.5h | done | — |
| 2 | [`status` on tool results](phase-02-status-on-tool-results.md) | 3h | done | 1 |
| 3 | [Failure + lifecycle hooks](phase-03-failure-and-lifecycle-hooks.md) | 5h | done | 1, 2 |
| 4 | [Assistant-turn span + core ask](phase-04-assistant-turn-span.md) | 4h | done | 1, 2, 3 |
| 5 | [Docs + contract reconciliation](phase-05-docs-and-contract-reconciliation.md) | 2.5h | done | 2, 3, 4 |
| 6 | [Manual verification guide + dormant testbed assertions](phase-06-manual-verification-and-testbed.md) | 2h | done | 5 |
| 7 | [that decision — full-content-capture posture draft (Proposed)](phase-07-content-posture-draft.md) | 3h | done (Proposed — awaiting owner acceptance) | 4, 5 (sequenced last per user request) |

## Validation Summary

**Validated:** 2026-08-13 (interview, `/ak-plan validate` on the 2200 plan; answers apply to this merge)
**Questions asked:** 6 (4 answered, 2 adopted per recommendation after interrupt — veto anytime)

### Confirmed decisions

1. **Plan overlap → merge; 2241 owns P0.** Status + failure hooks land per 2241's design, ported
   onto 2200's contract rigor (schema v1.2, closed vocabulary, conformance-on-outbound-bytes,
   Codex handling). 2200's span/docs/guide phases rebased on top. *(user answer)*
2. **decision record split → 0018 carrier (Accepted) + 0019 posture (Proposed).** 0018 unblocks widget code
   now; 0019 records the full-capture posture (P1–P3) and names the core ask that retires the span
   stopgap. *(user answer)*
3. **Span stopgap → ship + file the core ask.** Synthesized `http.method`/`http.url`
   classification keys accepted (OD-0018-1), marked `openbox.span_synthetic:true`; openbox-core
   issue filed: AGE reads assistant content from the `llm_completion` activity_output for
   span-less dev sessions. *(user answer)*
4. **Gate coupling → accepted with a constraint: `finops` and `content_capture` stay default-ON.**
Both already are (2026-07-15 flip). Alignment requiring finops ∧ content_capture is documented,
and any future default flip re-opens this decision. *(user answer, custom)*
5. **Thinking → final text only in this plan.** The span carries `last_assistant_message` only;
That decision words thinking as "not in this carrier — that decision (P3) owns it", never
"never captured" (owner posture has thinking IN SCOPE). *(adopted per recommendation)*
6. **Testbed → dormant assertions added** for status/failure/span/alignment, run deferred until a
   stack exists; the manual guide remains the near-term acceptance evidence. *(adopted per
   recommendation)*

### Action items from validation

- [x] Merge the two plans into this one; supersede both originals
- [x] Phase 2 step 1: empirical failure-surface check — **branch B1 confirmed** (probe report)
- [x] Phase 4: openbox-core AGE ask filed — [#130](https://github.com/OpenBox-AI/openbox-core/issues/130)
- [x] Phase 1/4: thinking stance worded as deferred (that decision owns it), never foreclosed

## Verification artifacts

- **[manual-test-guide.md](manual-test-guide.md)** — stack-free walkthrough, ~15 min.
  T1–T8 were **executed on this machine** and corrected until they ran verbatim; the
  live section (P0, L1–L6) is a checklist and has **not** been run.
- `plans/reports/probe-260813-2329-claude-code-hook-surface.md` — the hook-surface
  evidence that gated phases 02/03 (branch B1 confirmed; three plan assumptions
  corrected).
- `testbed/35-telemetry.sh` — dormant assertions, merged but never run.
- Filed: [openbox-core#130](https://github.com/OpenBox-AI/openbox-core/issues/130).

## Key references

- Research + owner decision: `plans/reports/research-260813-2215-session-content-capture-gaps.md`
  (§Decision update 2026-08-13 22:34 — FULL CAPTURE)
- Scout evidence: [read side (FE+backend)](../260813-2200-dashboard-widget-telemetry-gaps/scout/scout-01-read-side-fe-backend.md),
  [write side (core+sdk+shift-left)](../260813-2200-dashboard-widget-telemetry-gaps/scout/scout-02-write-side-core-sdk-shiftleft.md)
- Core read side (verified by reading `develop` @ 68f0398, not by a run):
  `errors.go:301-337` (IsSuccess), `storage_event.go:416-418` (Status→workflow_status copy),
  `goal_alignment_session.go:64-88` (AGE extractor), `governance_workflow.go:302-304`
  (semantic_type recompute)

## Acceptance criteria

- [x] Tool `ActivityCompleted` wire bytes carry `"status":"completed"`; failure path carries
      `"failed"` — asserted on outbound bytes in conformance; absent on turn/lifecycle/signal events
- [x] `PostToolUseFailure`, `SubagentStart`, `PermissionDenied`, `StopFailure` wired end-to-end,
      structural fields only, fail-open preserved (INV-3); Task `subagent_type` metadata
- [x] `TurnCompleted` under capture carries exactly one span; AGE-compatible `response_body`;
      deterministic ids; nothing new on the wire with `content_capture:false`; secrets redacted
      before attach (asserted on bytes)
- [x] `activity_id`/`event_id`/approval keys byte-unchanged (pin tests untouched); 11 modules green
      under `-race`; both cross-compiles
- [x] that decision Accepted + indexed; that decision Proposed + indexed; docs/contracts reconciled (no
      surviving "span-less"/"status retired"/stale PR-#125 claims)
- [x] Manual guide runs stack-free in <15 min; dormant testbed assertions merged (run deferred)
- [x] openbox-core AGE ask filed and linked

## Outcome of the unresolved questions

1. **RESOLVED — branch B1.** `PostToolUseFailure` fires INSTEAD of `PostToolUse`: a failing Bash
   on 2.1.229 produced the failure hook and zero `PostToolUse` firings, and the binary's own hook
   table documents the split. No stop-and-replan.
2. **RESOLVED, differently than planned.** Both older locally-installed versions (2.1.227,
   2.1.228) already know all four hooks, so the "older version" case could not be produced that
   way. The underlying property was tested directly instead: an unknown hook key in settings is
   silently ignored — session clean, hook never invoked, no warning.
3. **Accepted as-is.** Reader inventory re-confirmed: one consumer, the backend compliance
   evidence export. Scoping `status` to tool results is what keeps it off the events where the
   column means something else; no core ask filed.
4. **Documented, not closed.** Codex declares `tool.status` unsupported (pinned FALSE in its
capability test) and feeds no alignment. Recorded in COVERAGE.md, that decision and that
decision's parity-gap section.
5. **Still open, and outside this repo.** LlamaFirewall reachable + Redis up. It is P0 of the
   manual guide's live checklist and the first precondition `testbed/35-telemetry.sh` checks —
   both report it as a SKIP rather than a failure, because the client can be perfect and both
   widgets still empty.

6. **NEW, found during phase 3 and worth carrying forward.** Any `SignalReceived` with non-empty
   `signal_args` overwrites core's goal-alignment session goal (`age.go:112-137`). The three new
   signals therefore carry none. A future signal type must make the same choice deliberately.
