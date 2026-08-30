# Scout 02 — Write side: core + sdk-python + shift-left

Repos @ scout time: openbox-core `develop` 68f0398 ("Merge PR #129 … PROD-250"); openbox-sdk-python `main` dee5d4e; openbox-shift-left 7cfb50e.

## Widget 3: Tool Health Matrix — SUCCESS = 0.0%

Chain: FE SUCCESS% ← `observability_metrics` rows `tool.<name>.success` / `tool.<name>.failed` (`content.ToolKey`, written by `UpdateToolMetricsActivity`, openbox-core `internal/services/activities/observability/errors.go:103-164`) ← `input.IsSuccess` ← `ExtractToolMetric` (`errors.go:301-337`).

- `errors.go:332-334`: `IsSuccess = payload.Status != nil && *payload.Status == "completed"` on `ActivityCompleted`. Only literal `"completed"` in **top-level** `Status` counts.
- `metric.ToolName = *payload.ActivityType` (`errors.go:325`) — the FE "Tool" column.
- `metric.DurationMs` copied unconditionally (`errors.go:328-330`); `latency_sum_ms`/`latency_count` written whenever `DurationMs>0` (`errors.go:146-160`); `total_calls` on every `ActivityStarted` (`errors.go:118-127`) → latency/counts real while success stays 0.
- `Status` field (`internal/content/governance.go:206`): `*string`, documented for **Workflow** events (completed/failed/cancelled/terminated). A workflow-status field read for an activity-success decision.

Missing link — no producer sets it:
- shift-left `client/payload.go` never writes a `status` key on any event. `contracts/dev-event/MAPPING.md:239-241` lists `status` among fields "Retired with the span layer" (that decision — it was per-span OTel status).
- Base SDK `openbox_core/contracts/events.py:277-313` `activity_completed()` takes `result`/`error`/`attempt`/`extra` — **no `status` param**; `workflow_completed()` (204-221) none either.
- Net: `payload.Status == nil` on every dev `ActivityCompleted` → `IsSuccess` always false → `tool.<name>.failed` increments each completion → SUCCESS = 0.0% for every tool. Matches symptom exactly.

Side note: `ExtractToolMetric` on this develop HEAD already excludes `activity_type=="llm_completion"` via `IsLLMCompletionActivity` (`errors.go:320-322`, shared with `model_activity.go:157-168`) — shift-left CLAUDE.md's "only in unmerged PR #125" note is stale (PR likely merged).

## Widgets 1 & 2: Goal Alignment Trend + Recent Drift Events (both empty)

Both derive from `content.AGEResult.GoalAlignmentChecked` / `.GoalDrifted`, set by `AGEClient.manageGoalSession` (openbox-core `internal/services/age.go:109-172`), called from `evaluateGoalAlignment` (`internal/services/activities/governance/age.go:167-225`) inside `AGECheckActivity` — launched on every event ("always runs, cancellable", `governance_workflow.go:503-519`); gating is internal to `manageGoalSession`. (Sequential-pipeline description in core `docs/governance-workflow-architecture.md` §Step 6 appears stale vs this concurrent implementation.)

`GoalAlignmentChecked` becomes true only when (`age.go:109-172`):
1. `EventType==SignalReceived` with non-empty `SignalArgs` AND Redis goal session already has ≥1 accumulated assistant message (`age.go:113-129`); or
2. `EventType==WorkflowCompleted`/`WorkflowFailed` with ≥1 accumulated assistant message (`age.go:142-158`).

Assistant content is fed **only** by `extractAssistantContentFromLatestSpan(payload.Spans)` (`internal/services/goal_alignment_session.go:64-88`), fallthrough branch `age.go:165-169`, requiring a **span** with `Stage=="completed" && SemanticType==llm_completion && ResponseBody!=nil`.

Missing link: shift-left is span-less by design (`MAPPING.md:10` — dev sessions produce zero spans rows). Its model-turn signal is `TurnCompleted`→`ActivityCompleted` `activity_type:"llm_completion"` — an Activity, not a span, and its `activity_output` is {model, usage} only (INV-2: no completion text). So `payload.Spans` is always empty → assistant content never accumulates → `GoalAlignmentChecked` always false for shift-left traffic. shift-left DOES emit the trigger events (`prompt_submitted` SignalReceived, WorkflowCompleted) — span gap blocks regardless.

- Widget 1: `UpdateGoalAlignmentMetricsActivity` (`internal/services/activities/observability/compliance.go:336-381`) skips writing when `!GoalAlignmentChecked` (`compliance.go:344-347`) → keys `evaluation_count`/`aligned_count`/`drifted_count` (const.go:146-148, `MetricTypeGoalAlignment` const.go:69) never written → "No alignment data available".
- Widget 2: `GoalDrifted` only from `newGoalDriftResult` (`age.go:182-192`) in the same two branches, additionally gated on LlamaFirewall `/scan_replay` `!IsAligned` (`performTraceCheck`). Never reached → `age_evaluations.goal_drift` always false (`buildEventEvaluation`, `activities/governance/age.go:439-457`), `KeyGoalDriftedCount` never incremented → 0 events.
- Design note: genuine drift keeps `Verdict: VerdictAllow` (`age.go:186`) — drift is observational, never escalates the verdict alone.

## SDK schema recap (openbox-sdk-python)

- `ActivityStarted`/`ActivityCompleted` factories (`openbox_core/contracts/events.py:246-313`): no `status`/`success` field. `error` is a plain string on completed.
- Only span-level OTel status exists (`wire/core_span.py:55`, `contracts/otel_spans.py:166,220,280` — `{"code":"UNSET"/"OK"/"ERROR"}`) — unrelated to the payload-level field core's extractor reads.
- No alignment/drift/goal/score event types or emitter APIs in the SDK — goal-alignment is entirely server-side (Redis-backed AGEClient + LlamaFirewall). Consistent with shift-left having no emitter either.

## shift-left producer confirmation

- `MAPPING.md` §1-3 authoritative: `tool.name`→`activity_type` (the dashboard Activity column); no row maps to top-level `status`.
- Golden fixtures (`client/testdata/golden/activity_*_completed.json`) carry no `status` key.
- `MAPPING.md:280`: `payload.Error` read only for `WorkflowFailed`; client sends none.

## Unknowns

1. FE/API read handlers not traced (out of scope for this scout) — writes traced to `observability_metrics` (`MetricTypeGoalAlignment`/`MetricTypeTool`) and `age_evaluations`; all empty/zero from the same root causes regardless of which store each widget queries. → covered by scout 01.
2. The 70%/50% band thresholds not found in core `const.go` — likely FE-side.
3. Whether any framework-SDK adapter injects a `status` key via `activity_completed(extra=…)` — unchecked; if none do, core's success derivation may be dead for **every** current producer, not just shift-left.
4. Core `develop` already has the `IsLLMCompletionActivity` exclusion → shift-left CLAUDE.md note about unmerged PR #125 is stale; reconcile.
