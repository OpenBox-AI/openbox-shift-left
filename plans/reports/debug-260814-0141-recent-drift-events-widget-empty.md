# Debug: "Recent Drift Events" widget empty while Goal Alignment Trend shows drift

**Date:** 2026-08-14 · **Agent:** cc91d0ec-d03d-4d79-a0b2-964a85d2c22a · **Repos:** openbox-fe, openbox-backend, openbox-core (read-only diagnosis, no fixes applied)

## Symptom

Trend endpoint returns `total_evaluations: 5, drifted_count: 3` (Aug 13). Recent Drift Events widget: "No drift events found". Live probe of `GET /agent/:id/goal-alignment/recent-drifts` → 401 without auth (not directly probed); FE parsing ruled out by code read.

## Root cause

**The two widgets read two different stores, written by two different core paths — and the path that writes the drift-events store is unreachable for every event on which a drift verdict is actually produced.** `age_evaluations` never receives event-level drift rows; the trend counters do.

### Data paths

| Widget | Backend read | Table | Core writer |
|---|---|---|---|
| Goal Alignment Trend | `observability.service.ts:994` | `observability_metrics` (`metric_type='goal_alignment'`) | `UpdateGoalAlignmentMetricsActivity` (`observability/compliance.go:336`) — fires from `ObservabilityWorkflow` for ANY event with an AGE result (`observability_workflow.go:219-227`). No span requirement. **Works.** |
| Recent Drift Events | `session.service.ts:778` — `WHERE agent_id=$1 AND goal_drift=true AND span_id IS NULL` | `age_evaluations` | Only live writer: `storeAGEResults` (`governance/storage_age_evaluation.go:20`) called from `StoreHookSpanActivity` (`storage_event.go:189`) — gated on `input.AGEResult != nil && len(storedSpans) > 0`, reachable only when `hasNewSpan`. **Never fires for drift.** |

### Why the drift writer never fires

1. **Drift verdicts are produced only on span-less events.** Alignment session logic (`openbox-core/internal/services/age.go:109-172`): trace check runs only on `SignalReceived` with non-empty `signal_args` (= PromptSubmitted, 2nd+ prompt) and `WorkflowCompleted/Failed` (= SessionEnded). All other events (incl. the ADR-0018 turn-span carrier) only append assistant content to the Redis session — `checked=false`. Trend's "5 evaluations / 3 drifted" = prompt + session-end checks.
2. **Those events always take the NORMAL workflow path.** `hasNewSpan := existingEventResult.Exists && HasNewSpans` (`governance_workflow.go:230`); `HasNewSpans` requires an existing event (dedupe key includes `activity_id` + `event_type`, `validation.go:86-103`) AND `IsHookEvent` (= `payload.hook_trigger`, `governance_workflow.go:202`) AND a new hook-span identity (`validation.go:154-174`). `SignalReceived`/`WorkflowCompleted` carry no `activity_id` → `Exists:false` → normal path, always.
3. **The normal path stores no AGE evaluations.** It runs `StoreGovernanceEvent` (event row only), policy eval, guardrails eval, and — on drift — `RecordTrustTriggersActivity` (`governance_workflow.go:964-973`). No `age_evaluations` write exists on this path.
4. **The span-independent writer is dead code.** `StoreAGETrustScoreActivity` (`governance/age.go:389`) builds exactly the event-level row the widget needs (span_id NULL, goal_drift, reason → `goal_alignment_detail`) but has **zero workflow callers** (grep: only its definition + input type; callers removed in old "refactor: clean" commits, 2026-03-14).
5. Dev sessions can't even reach the hook path by accident: shift-left never sets `hook_trigger` (deliberate, `client/payload.go:186-195`), and tool events are span-less (ADR-0013). Even on the hook path (base SDK), the event-level row is written with whatever the AGE result says — and alignment returns `checked=false, drifted=false` on span-carrying events — so `goal_drift=true` rows are never produced there either.

**Conclusion (corrected 12:17, see Addendum): no NEW drift rows for any runtime since 2026-06-05; SDK agents still RENDER drift events because their old rows persist and the query has no time filter.** Dev agents all postdate the cutoff → empty widget. Demo agents additionally have seeded rows (`openbox-backend/src/modules/agent/utils/seed-demo-agent.util.ts`).

### What IS stored per drift (proves drift was recorded)

- `observability_metrics` drifted_count (trend) ✓
- `trust_rule_triggers` row, `rule_type='goal_alignment'` (`governance/trust_trigger.go:39-53`) — no reason text ✓ (this feeds the "goal-drift findings" hook banner)
- `age_evaluations`: nothing ✗

## Blast radius (same orphaned table)

- `getGoalAlignmentStats` (`session.service.ts:710`) — Verify-tab session alignment stats: always 0/0 for real agents.
- `getReasoningTrace` (`session.service.ts:730`) — reasoning-trace modal filters `goal_alignment_checked = true`: always empty.
- `getRecentDriftEvents` (`session.service.ts:778`) — the reported widget.

## Fix options (owner decision, cross-repo)

- **A. Core, cause-aligned (recommended):** persist the event-level evaluation where the verdict is produced — re-wire the existing `StoreAGETrustScoreActivity` (or an equivalent insert) on the normal path when `ageResult.GoalAlignmentChecked` (store aligned AND drifted rows so the table matches the trend counters; `ageResult.Reason` → `goal_alignment_detail`). Guard the hook path against double-writing the event-level row. Restores all three read surfaces for every runtime.
- **B. Backend-only:** re-point `getRecentDriftEvents` at `trust_rule_triggers` (+ join `governance_events` for time/session). Loses the drift reason (not stored on triggers), leaves stats/reasoning-trace broken. Weaker.

## Adjacent defects noticed (not the cause)

- `storeAGEResults` swallows insert errors (Warn) then still updates metrics (`storage_age_evaluation.go:31-47`) — guarantees counter/row divergence on any insert failure.
- Recent-drifts query `LEFT JOIN agent_trust_scores` is unscoped — multiple trust-score rows per agent would duplicate drift rows; `alignment_percentage` fallback is a synthetic constant `(1-0.35)*100 = 65` (`session.service.ts:786-789`).
- Hook-path event-level rows (base SDK) are written per hook span with `checked=false/drift=false` — noise rows in `age_evaluations`.

## Addendum (2026-08-14 12:17) — user observation: SDK agents DO show drift events

Correct, and it refines the scope rather than contradicting the mechanism. Evidence chain:

1. **Goal alignment migrated from the external AGE service into core on 2026-06-05** (`openbox-core` cd33069, "PROD-211 feat: migrate goal alignment from AGE to core"). Pre-migration, the old `AGEClient` called the external `agent-governance-engine` per event and the response carried `GoalDrifted` + per-span results (cd33069^ `age.go:84,259`) — drift could be flagged on **hook events**, which carry spans, take the hook path, and therefore DID write event-level `age_evaluations` rows with `goal_drift=true` via `storeAGEResults`. **That was the SDK drift-row writer. The migration retired it.**
2. Post-migration, drift fires only on span-less normal-path events (main analysis above) → no new rows for any runtime. SDK agents accumulated real drift rows before the cutoff; `getRecentDriftEvents` has **no time filter** (`ORDER BY evaluated_at DESC LIMIT 10`), so months-old rows render as "Recent" indefinitely. Dev agents (all registered post-July) have zero rows → empty widget. The asymmetry the user sees is row AGE, not a working SDK path.
3. **This was found before and the fix is stranded.** Branches `fix/claude-code-age-session-persistence` (f6dfb7d, 2026-06-17) and `fix/prod-000-claude-code-age-session-persistence` (ae8dbf4, 2026-06-25) — same PROD-211 ticket — change the hook-path gate `len(storedSpans) > 0` → `input.SessionID != uuid.Nil` so the event-level row survives span-insert failure. **Neither is merged** (`git merge-base --is-ancestor` → not ancestors of develop); develop still has the March gate. Note: even merged, they only fix the hook path — they would NOT restore drift rows, which are normal-path since the migration.
4. `agent-governance-engine` itself never wrote `age_evaluations` (no references in that repo); backend only seeds (demo) and updates (false-positive flag). No other writer exists.
5. Deployment caveat: `openbox-api.node.lat` = staging (`openbox-manifest-k8s-cluster/openbox-backend/values.yaml:43`). Manifest repos are stale (image tags last committed 2026-01-28; bumps now happen out-of-band), so the running core version is unverified from git. Post-June behavior on staging is inferred from: ADR-0014 extractor (merged Aug) verified against this stack, trust-trigger drift findings arriving (normal-path `RecordTrustTriggersActivity`), and dev trend cadence matching the alignment-session design.

**User-checkable discriminator:** open an SDK agent's drift event and read `evaluated_at`. Expected: nothing newer than the June migration deploy (or the agent is demo-seeded). A drift event from the last few weeks would falsify point 2 and mean staging runs pre-June core — say so and I'll re-open.

**Fix guidance updated:** Option A (normal-path event-level write when `GoalAlignmentChecked`, e.g. re-wiring `StoreAGETrustScoreActivity` after `StoreGovernanceEvent`) is now clearly the fix for **all** runtimes going forward, not just dev — SDK agents stopped getting new drift rows at the migration too, it's just masked by their old data. Folding in the stranded PROD-211 gate fix on the hook path is a small complementary correctness win. No double-write by construction: post-migration hook events always carry `checked=false/drift=false`, and the drift-bearing events (signals/lifecycle) are normal-path only — but assert it in a test.

## Unresolved questions

1. Fix in core (A) vs backend (B) — and if A, should aligned (non-drift) evaluations also persist per event (recommended for stats/trace consistency), or drift-only?
2. Should `RecordTrustTriggersActivity` and the new evaluation insert be unified (trigger has no FK to an evaluation today — comment at `governance_workflow.go:962` says exactly this)?
3. What should `alignment_percentage` mean in the widget (currently a per-agent trust-score COALESCE, not a per-event value)?
4. Confirm via dashboard: `evaluated_at` of the SDK agents' visible drift events (expect ≤ June migration deploy or seeded). Confirms/falsifies the stale-row explanation since the running staging core version is not derivable from the (stale) manifest repos.
5. Should the widget/API get a time window (e.g. last 7/30 days) so "Recent" means recent? Separate small backend/FE decision.
