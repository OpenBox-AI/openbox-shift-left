# Scout 01 — Read side: openbox-fe + openbox-backend

## 1. Goal Alignment Trend → "No alignment data available"

FE: `src/components/pages/agent/components/monitor/components/goal-alignment-trend.tsx` — empty state when `chartData.values.length === 0` (172-178); values from `item.alignment_percentage` (64). Hook `useGoalAlignmentTrend.ts:48-79` → `agentApi.getGoalAlignmentTrend(agentId,{fromTime,toTime})`. API `src/lib/api.ts:675-693` → `GET /agent/{agentId}/goal-alignment/trend`.

Backend: `agent.controller.ts:925-936` → `agent.service.ts:1525-1531` → `observability.service.ts:994-1029`: reads `observability_metrics WHERE metric_type='goal_alignment' AND agent_id=$1 AND bucket_time in [from,to)` grouped by day; `alignment_percentage = SUM(aligned_count)/SUM(evaluation_count)*100` from `metric_key='aligned_count'/'evaluation_count'` rows (999-1016).

Empty condition: zero `observability_metrics` rows with `metric_type='goal_alignment'` for this agent/time window.

## 2. Recent Drift Events → badge 0

FE: `.../monitor/components/drift-events.tsx` — empty when `driftEvents.length===0` (51-56), badge = length (103). Hook `useRecentDrifts.ts:12-25` → API `api.ts:695-709` → `GET /agent/{agentId}/goal-alignment/recent-drifts?limit`.

Backend: `agent.controller.ts:944-951` → `agent.service.ts:1533-1535` → `session.service.ts:778-808`: `FROM age_evaluations WHERE agent_id=$1 AND goal_drift=true AND span_id IS NULL ORDER BY evaluated_at DESC LIMIT $2` (790-797).

`span_id IS NULL` is intentional (event-level vs span-level checks, `age-evaluation.entity.ts:41-60`) — not the bug. Empty condition: no rows with `goal_drift=true` for this agent.

## 3. Tool Health Matrix → SUCCESS 0.0%, real latency, red status

FE: `.../monitor/components/tool-health-matrix.tsx` renders pre-shaped props (success 64, latency 69, status 76-83/124-130). Computation in `.../monitor/index.tsx:74-114`:
- `successRate = total_calls>0 ? success_calls/total_calls*100 : "0"` (85-88)
- status: `<90% or >5000ms → "error"`; `<95% or >2000ms → "warning"`; else healthy (96-102); red styling `monitor-utils.tsx:15-31`.
Hook `useMonitor.ts:126-161` → API `api.ts:1348-1374` → `GET /agent/{agentId}/observability`.

Backend: `agent.controller.ts:1039-1051` → `agent.service.ts:1584-1597` → `observability.service.ts:27-100` (`getToolStats` line 68, attached as `tools` 86). `getToolStats` (244-303) unions two CTEs:
- `metric_tools` (254-267): `observability_metrics WHERE metric_type='tool'`, `tool_name=SPLIT_PART(metric_key,'.',1)`, sums keys `%.total/%.success/%.failed/%.latency_sum_ms/%.latency_count`
- `span_tools` (269-284): `spans JOIN sessions WHERE span_type='mcp_tool_call'` — contributes nothing for dev sessions (span-less)

Zero condition: `SUM(.success)=0` while `.total>0` and `.latency_*>0` → FE 0.0% → status "error". Exactly observed.

## Write side (from backend's view)

No INSERT into `age_evaluations` or `observability_metrics` with `metric_type IN ('tool','goal_alignment')` anywhere in openbox-backend src except migrations and the demo-agent seeder (`seed-demo-agent.util.ts:1351-1399,966,1067` — synthetic demo agents only; never writes `metric_type='tool'`). `POST /policy/evaluate` (`policy.controller.ts:35`) is unrelated (writes `metric_type='policy'`, 830-1063). Writer is external → openbox-core (confirmed by scout 02).

## Filters

No session-kind / agent-kind / org / feature-flag filters in any of the three read queries (grep confirmed). Only selective predicate is the intentional `span_id IS NULL`.

## Unresolved

1. Whether `metric_type='tool'` rows exist with `.success`=0 vs missing — needs DB access; SQL consistent with either. (Scout 02: core increments `.failed` each completion, `.success` never — so rows exist, success absent/zero.)
2. `agent.service.ts:3099 getDriftEvents` (via `governance-event.service.ts:1040`) is a second drift path NOT used by this widget.
3. Ingestion endpoint not in backend — lives in core (scout 02 confirms core writers).
