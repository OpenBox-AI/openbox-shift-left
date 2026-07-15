# E7-S8 — Dashboard tool-call DURATION

**Goal (FR / dashboard gap):** the Verify-tab "Event Log Timeline" DURATION column
reads `governance_event.duration_ms`. For developer-runtime tool calls it was
always blank. This story lands a real, non-zero `duration_ms` (and `end_time`)
without breaking the wire, the classified completed span (the E7-S2 win), or
INV-2/INV-3. Closes the last item from `dashboard-devruntime-display-gaps`
(E7-S6/S7 landed activity + input; duration was deferred here).

## Why it was blank (the two coupled problems)

1. **No cross-process start time.** A Claude Code tool call is a `PreToolUse`
   (started) then a `PostToolUse` (completed), each a **separate short-lived hook
   process**. `PostToolUse` exposes no start time, so the completed span's
   `start_time` was its own timestamp and `duration_ns` was **0**
   (documented F1 in `client/payload.go buildHookSpan`).
2. **Core never fills event-level duration on the completed hook.** A tool call is
   ONE `ActivityStarted` (started+completed merged by `activity_id`; both stages
   are `ActivityStarted`, paired by shared `span_id` — the E7-S4 correction).
   Core writes `duration_ms`/`end_time`/`output` only on the **event-creating
   (started) path** (`setOptionalPayloadFields`); the started event has no
   duration, and the completed hook (`StoreHookSpanActivity`) stored the span but
   **did not update the event row** (only verdict, and only when non-ALLOW).

## Decision — OPTION C (additive), not B

- **C (chosen):** the completed-hook path additively updates the event's
  `duration_ms`/`end_time` from the completed span, PLUS the adapter threads the
  started timestamp so the completed span carries a real `duration_ns`. Keeps the
  single-`ActivityStarted` model and the classified completed span (`span_type`).
- **B (rejected):** emit an `ActivityStarted`+`ActivityCompleted` lifecycle pair.
  Rejected because `ActivityCompleted` is hook-less and **cannot carry the
  classification span**, which would regress the E7-S2 shell/mcp classification.
  (B would also fix the multiagent-timeline `pairActivities` two-row nicety — left
  as a known, separate cosmetic gap.)

## Change 1 — adapter: thread the started timestamp (shift-left)

`adapters/claude-code/duration.go` (+ `adapter.go threadDuration`, wired in
`Observe`). A `durationStash` mirrors the SL-5 session registry:

- `PreToolUse` (ToolCall) → write a tiny record keyed by the tool call's pairing
  key (`toolCallStartKey`: session + tool name + file/function locator — the same
  structural fields `client.activityPairKey` uses to pair the two spans onto one
  `span_id`). Content = the RFC3339 `StartedAt`. Atomic temp+rename, `0700`/`0600`.
- `PostToolUse` (ToolResult) → read + delete the record and stamp `ev.StartedAt`
  **before the event is spooled**, so the completed `DevEvent` is self-contained
  (robust to the SL-4 spool's rotation/recovery-file splitting — the pairing is
  done before the network, not at flush). `buildHookSpan` then computes
  `duration_ns = end - start` and the completed span's `start_time` equals the
  started span's.
- `SessionEnded` → sweep the session's stash subdir (records whose `PostToolUse`
  never fired do not accumulate).

Stash lives under `<spool-dir>/durations/<session>/…`, so it is auto-isolated by
`OPENBOX_SPOOL_DIR` in tests and the spool's `FlushAll` (which skips subdirs)
never mistakes it for a spool file. Best-effort throughout (INV-3): a stash fault
only costs duration accuracy, never a tool call. Structural timestamps only —
no content (INV-2). The 0-duration path still exists as the **stash-miss
fallback** (unpaired completed / lost started record).

## Change 2 — core: fill event-level duration on the completed hook (openbox-core)

- `internal/content/0_contract.go`: new `DatastoreGovernanceEvent.UpdateCompletion(id, durationMS, endTime)`.
- `internal/datastore/governance_event_pgx.go`: implement it (mirror of
  `UpdateVerdict` — a `GovernanceEventSetter` with only the completion columns,
  `um.Where(id)`, parameterized). `nil` args leave a column unchanged.
- `internal/services/activities/governance/storage_event.go`: new
  `updateEventCompletion(ctx, logger, eventID, span)` called from
  `StoreHookSpanActivity`. **Guarded on `span.Stage == "completed" &&
  span.DurationNs != nil && *span.DurationNs > 0`.** `ns→ms` via the existing
  `calculateDurationMS`. Best-effort: a datastore error is logged, never returned
  (INV-3). The setter columns already existed — no schema change, no wire change.
  - The **positive-duration** part of the guard is the fix for correctness
    Finding 1: the client builder always emits a numeric `duration_ns` for a
    completed span (wire-required), so the dev-runtime stash-miss fallback yields
    `start==end ⇒ duration_ns 0`; writing `duration_ms=0` there would
    misrepresent "unknown timing" as "instantaneous", so a zero/negative duration
    is a no-op (column stays honestly blank — the pre-E7-S8 state).
  - **Scope note (correctness Finding 2, needs brian's call at G3/G_SEC):**
    `StoreHookSpanActivity` is the SHARED hook path, so this now also fills
    `duration_ms`/`end_time` for **base-SDK v2 completed hook spans** that carry
    `stage="completed"` + a positive `duration_ns`, not only developer-runtime.
    This is generically correct (fills a previously-null column with the real
    completed-span duration; it targets the `ActivityStarted` event row, distinct
    from the separate `ActivityCompleted` lifecycle row, so no double-write). Kept
    GENERAL rather than gated on `source=="developer-runtime"` because the fill is
    right for any hook span — but the broadened behavior is surfaced here for a
    conscious product decision rather than scoped silently.

Path check: the started hook is the first arrival for the `activity_id` → NORMAL
path (creates the event, no duration). The completed hook is a new span on an
existing event → HOOK path → `StoreHookSpanActivity` → `updateEventCompletion`.

## Invariants

- **INV-2:** stash filename is a SHA-256 hash; stash content and the new columns
  carry only structural timestamps — never command/file/output content.
- **INV-3:** observe-only fail-open preserved; the E6 enforce path is untouched
  (`enforce.go` builds its own sidecar request, never `buildPayload`/`Observe`'s
  duration threading). Enforce conformance C1–C9 stay green.
- Additive: no wire type change; `duration_ms`/`end_time` were already writable
  columns.

## Validation

- Unit + `-race` GREEN across all 9 shift-left modules and openbox-core
  (affected packages). New tests: adapter stash put/take/clear + Pre→Post
  threading round-trip + unpaired fallback + SessionEnd sweep + FlushAll-safety;
  client `TestHookPayload_ThreadedStart_RealDuration` (threaded start ⇒ real
  `duration_ns`, shared `start_time`); core `updateEventCompletion` behavior
  (completed lands duration/end_time; started / no-duration / stage-less = no-op;
  duration-without-end-time). gofmt clean.
- **Live E2E (acceptance) — PASS 2026-07-15** (RUNBOOK Path A: rebuilt core
  `server`+`governance-worker` on :8086, real `openbox hook claude-code` binary,
  dev agent org `openbox.ai` `signing_required=false`):
  - Happy path — `SessionStart→PreToolUse(Read)→[sleep 3s]→PostToolUse→SessionEnd`:
    the `ActivityStarted`/`Read` event row landed **`duration_ms = 3006.69`** with
    `end_time` filled and `updated_at` > `created_at` (completed hook updated the
    row). The completed span persisted with `span_type = file_read` — the E7-S2
    classification survived. Before E7-S8 this row's `duration_ms` was null.
  - Negative path (Finding-1) — forced stash miss (start record deleted between
    Pre and Post): the completed span carried span-level `duration_ms = 0`, but the
    **event-level `duration_ms` stayed blank** (guard skipped the zero write) —
    "unknown timing" reads as blank, not instantaneous.

## Review outcomes (independent, blind)

- **G_SEC security review: PASS.** All five invariants confirmed (no traversal —
  filename is a SHA-256 hash of structural-only locators; stash content + new
  columns carry only timestamps; parameterized bob update, no injection/authz
  surface; enforce path untouched). Two non-blocking notes: INFO-1 (added
  synchronous stash I/O ahead of the enforce gate — same class as the pre-existing
  `Spool.Append`, best-effort/unbounded); **INFO-2 FIXED** (temp file now removed
  on a failed `Rename`).
- **Correctness (blind diff) review:** 1 MEDIUM + 2 LOW.
  - **Finding 1 (MEDIUM) — FIXED:** zero-duration stash-miss would have written
    `duration_ms=0`; guard now skips non-positive duration (+ zero/negative no-op
    tests; the misleading `nil`-shape test was corrected).
  - **Finding 2 (LOW) — addressed:** broadened base-SDK scope documented above +
    the existing land-duration test is base-SDK-shaped; flagged for brian's call.
  - **Finding 3 (LOW) — documented:** parallel *identical* tool calls can collide
    on the pairing key (inherited `activityPairKey` limitation; best-effort,
    duration-only). The Finding-1 fix mitigates its worst outcome (a collided miss
    now writes nothing rather than a spurious 0).

## Gates

`G3_REVIEW` + `G_SEC` (touches the governance workflow) — independent reviews done
(findings resolved above); brian G3 + G_SEC sign-off pending. Live E2E pending.
No push (local-only per sibling/local convention).
