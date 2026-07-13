# STORY-SL-13 — EXT-core dependency: patch artifact + core-acceptance contract test

**Risk:** low (adds a versioned artifact + a skip-when-absent integration test; no change to runtime egress code. The actual openbox-core edit is external.)

## Source
- **Architecture:** D4 (extend the event-type enum, don't fork the schema), INV-8 (additive/compatible); §7 "EXT-core caveat".
- **Backlog:** `shift-left-backlog.md` "External dependencies — [EXT-core]" (3 additive no-migration edits) and RUNBOOK Appendix A.2.
- **Session:** Phase-1 debt review (2026-07-13) item #1 — the one hard Phase-2 blocker; today the EXT-core change lives only as local working changes in openbox-core, "assumed-satisfied" but neither reproducible nor verified.

## User Value
The one external dependency that makes emitted developer events actually *accepted* (vs `400 invalid event_type`) stops being an untracked local patch: it becomes a PR-ready artifact anyone can apply/upstream, and a contract test proves — against a real core — that the dependency is satisfied instead of assumed.

## Inlined context (verified — builder need not re-read)
- **The EXT-core change is exactly 3 additive, no-migration edits** (S6 §3; RUNBOOK A.2): (1) `internal/content/governance.go` — add the constants `EventType{SessionStarted,PromptSubmitted,ToolCall,ToolResult,SessionEnded,CommitCreated,Deploy}`; (2) `internal/api/governance.go` — add them to the `isValidGovernanceEventType` switch; (3) `internal/services/activities/governance/storage_session.go` — map `SessionStarted`→create session, `SessionEnded`→terminal (rest fall through). Verified present in the local core this session (`internal/api/governance.go:283-288`).
- **Nothing captures this in shift-left today** — no `*.patch`/`*.diff` in the repo; the 7 strings are the SL-1 contract's `EventType` enum (`client/event.go`), which is the natural source of truth for the artifact.
- **The failure mode is already diagnosable:** SL-10 maps a `400`/`invalid event_type` response to an actionable reason; SL-3 fail-open drops it (INV-3). This story makes the drop *detectable in CI*, not silent.
- **7 event types must round-trip:** `SessionStarted|PromptSubmitted|ToolCall|ToolResult|SessionEnded|CommitCreated|Deploy`.

## Acceptance Criteria
- A versioned **artifact** under `contracts/dev-event/ext-core/` containing: the exact 3-edit diff (or an apply script + the constant list generated from the SL-1 enum) and a README stating scope (additive, no-migration), rationale (D4/INV-8), and how to apply/upstream. The artifact's event-type list is the SAME 7 as the SL-1 contract (a test asserts they don't drift).
- A **core-acceptance contract test** (Go, in `contracts/dev-event/` or `client/`) that, given `OPENBOX_URL` + dev creds, POSTs a minimal valid event of **each** of the 7 types to `/api/v1/governance/evaluate` and asserts a **non-400 / 2xx-or-accepted** outcome; it **skips cleanly** (not fail) when `OPENBOX_URL`/creds are absent, so unit CI stays green offline.
- The test's failure message names the missing EXT-core edit (cross-refs the artifact + SL-10 reason) so a `400` reads as "apply `contracts/dev-event/ext-core/`", not a mystery.
- **Drift guard:** a unit test fails if the artifact's type list ≠ the SL-1 `EventType` enum (keeps the patch honest as the contract evolves).

## Nonfunctional Requirements
- **maintainability:** artifact is generated-from or checked-against the SL-1 enum — single source of truth, no hand-copied drift.
- **reliability:** the live acceptance test is opt-in (env-gated) and never blocks offline unit runs.

## Write Scope
- `contracts/dev-event/` — new `ext-core/` artifact + the acceptance/drift tests (may live in `conformance/`). No change to `client/`/adapters egress code.

## Dependencies
- **Hard:** STORY-SL-1 (the enum that is the source of truth), STORY-SL-3 (the client used to POST in the live test).
- **External:** [EXT-core] the actual upstream merge in openbox-core — this story makes it PR-ready + verifiable, it does not perform the merge.

## Invariants
- **INV-8:** the artifact is strictly additive; the drift guard prevents a type appearing in shift-left that core won't accept-list.

## Human Gates
| Gate | Question | Owner | Evidence Needed | Allowed Outcomes |
|---|---|---|---|---|
| G3_REVIEW | Is the artifact faithful to the 3 edits and the acceptance test correct (2xx vs 400, clean skip)? | brian | diff review + a live run against local core | approve / revise |

## Validation
```bash
cd contracts/dev-event/conformance && go build ./... && go test ./...   # drift guard: artifact types == SL-1 enum
# live (hybrid stack up): OPENBOX_URL=http://localhost:8086 + creds → all 7 types accepted (non-400); assert
#   with EXT-core NOT applied (stock core) the same test reports the mapped "apply contracts/dev-event/ext-core/" failure.
```

## Stop conditions
- If openbox-core's accept-list mechanism has changed since S6 (e.g. the switch moved) → capture the current shape, regenerate the artifact against it, and note the drift; do not ship a stale patch.
