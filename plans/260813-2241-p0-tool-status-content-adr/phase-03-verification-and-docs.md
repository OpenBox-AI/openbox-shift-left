# Phase 03 — Verification + docs

## Context links

- Parent: [plan.md](plan.md) · Depends: phases 01–02
- Repo rule: "reading is not evidence; unit tests are not evidence a hook works" — binary-level + testbed assertions required.
- Verification-report convention: `plans/260813-0140-inline-policy-evaluation/reports/verification-260813-inline-evaluation.md` (claims split by evidence strength)

## Overview

- Date: 2026-08-13 · Priority: P1 · Status: pending · Review: n/a
- Prove P0 at the strongest reachable level (real binary over stub HTTP), extend testbed for the stack-gated
  claims, resolve the two empirical unknowns, update user docs.

## Key Insights

- Two unknowns are pre-decided either way (safe directions), so they gate documentation wording, not design:
  (a) PostToolUse-on-failure cadence; (b) older-CC unknown-hook behavior.
- The core side effect (Status → `workflow_status` on activity rows, storage_event.go:416-417) is the one
  externally visible change we did not choose — must be observed live and either accepted in writing or filed as a core ask.

## Requirements

1. Binary-level test (pattern: `TestHookRealtimeDelivery`): drive real hook binary for PostToolUse +
   PostToolUseFailure + one signal hook against mock core; assert wire bytes incl. `status` and absence assertions.
2. Empirical check on installed claude 2.1.229 (headless one-shot): force a failing tool (e.g. Bash `false` /
   nonexistent cmd) → record which hooks fire, payload fields present (esp. any error enum on PostToolUseFailure).
3. Testbed: extend `testbed/` phase asserting per-call `status` arrival + failure event + `subagent_started`
   signal; **run deferred** until a local stack is reachable — mark plan status accordingly (repo convention: implemented ≠ testbed-verified).
4. Core side-effect check (read-only, openbox-core repo): confirm no consumer misreads `workflow_status` on
   activity rows (grep readers of the column); decide accept vs core ask; record in verification report.
5. Docs: MAPPING.md §3/§7 rows + un-retire note (phase 01/02 started them — reconcile), COVERAGE.md hook table,
   `capabilities.go` strings, `docs/data-and-privacy.md` (one line: status/lifecycle enums added, still no content
   on observe path), CLAUDE.md "Current state" paragraph, `adapters/claude-code/README.md` known-limitations
   (Codex success-unknown parity gap).
6. Verification report `plans/260813-2241-p0-tool-status-content-adr/reports/verification-{date}-p0.md`
   splitting every claim by evidence strength (unit / binary / testbed-pending).

## Architecture

No production code beyond fixes surfaced by tests. Evidence ladder: unit → conformance (bytes) → real binary
over PTY/stub → testbed (pending stack).

## Related code files

- `adapters/claude-code/*_test.go` (binary-level test alongside realtime one)
- `testbed/30-enforce.sh` siblings — new `testbed/35-tool-status.sh` (or fold into existing observe phase; prefer fold if §numbering allows — reuse over new file)
- `contracts/dev-event/MAPPING.md`, `COVERAGE.md`, `docs/data-and-privacy.md`, `CLAUDE.md`, adapter README

## Implementation Steps

1. Binary-level test + fixes.
2. Live one-shot session experiments → record answers to unknowns 1–2 in verification report; adjust mapper/docs if assumption (failure-hook-instead) is wrong.
3. Testbed assertions authored; script runs locally against stub only; stack run deferred + noted.
4. Core column-reader grep + decision note (+ optional core issue).
5. Docs sweep; CLAUDE.md status paragraph ("implemented, unit+binary verified, testbed NOT run" — match house style).
6. Verification report.

## Todo list

- [ ] Binary-level test green
- [ ] Failure-cadence + old-version answers recorded
- [ ] Testbed assertions authored (run deferred)
- [ ] workflow_status side-effect decision recorded
- [ ] Docs sweep (MAPPING/COVERAGE/data-and-privacy/CLAUDE.md/README)
- [ ] Verification report with evidence split

## Success Criteria

- All 11 modules `-race` green; linux-arm64 + windows cross-compiles green; verification report exists with no
  claim stronger than its evidence; docs mention no capability the testbed hasn't confirmed without saying so.

## Risk Assessment

- Stack unavailable (likely): acceptable — plan completes with testbed-pending status, matching ADR-0013/0014/0017 precedent.
- Live experiment shows both PostToolUse+Failure fire → duplicate completed rows: document, lean on server-dedupe ask; no client suppression hack (cross-process state not worth it for over-reporting).

## Security Considerations

- Verification must include the absence assertions (no denial_reason/error_message/tool_response bytes on wire) — they are the P0 privacy claim.

## Next steps

Phase 04 (ADR draft) — independent, sequenced last per user request.
