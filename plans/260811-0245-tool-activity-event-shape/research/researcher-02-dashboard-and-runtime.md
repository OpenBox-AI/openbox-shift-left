# Dashboard timeline render path + agent-runtime emission order

Repos (read-only): `openbox-fe`, `openbox-temporal-sdk-python`. All paths relative to each repo root unless noted.

## Part A — openbox-fe

### Q1 — render surface
Two separate event-list surfaces exist; only one matches the screenshot:
- `src/components/pages/agent/components/verify/components/event-log-timeline.tsx` — flat `<table>`, one `<tr>` per event, no nesting (lines 193-275). Not the screenshot (no header+nested-span structure).
- `src/components/pages/agent/components/verify/components/workflow-tree-view.tsx` — recursive tree (`renderNode`/`renderSpanNode`), matches screenshot exactly: header = `{event_type} {activity_type} {N span badge} {duration}` (line-cited below). **This is the component.**

Field shapes backing it: `src/components/pages/agent/hooks/useSessionNodes.ts:15-41` (`GovernanceSpan`: `stage`, `span_type`, `duration_ms`, `attributes`, `data` — no `activity_id`, no `hook_type` typed field) and `:43-71` (`GovernanceEvent`: `event_type`, `activity_type`, `span_count`, `spans[]`, `duration_ms` — no `hook_trigger` field).
Grep of `hook_trigger` and `run.provider`/`.provider` (non-React-Provider) across `src/` → **zero hits**. Not found anywhere in the FE; these are backend/SDK-only fields not surfaced as named FE fields (may pass through generic JSON viewers inside `span.data`/`.attributes`, unlabelled).

### Q2 — pairing/grouping logic
**No pairing exists anywhere in the FE, by event_type suffix or by activity_id/span_id.** Evidence, end to end:
- `useSessionLogs.ts` (44 lines, full file read) — plain `useQuery` fetch wrapper, zero merge/pair logic.
- `useAgentLogs.ts:64-86` (`mapLogsResponse`) — 1:1 `.map()` over raw API rows; spreads each raw log (`...log`) and only computes display helpers `operation`/`sessionId`/`timestamp`. `event_type` passes through untouched, unpaired.
- `workflow-tree-view.tsx:312-329` — builds one `SessionNode` per raw log row, 1:1, `id: log.id`.
- `workflow-tree-view.tsx:470-483` (`getChildNodes`) — child rows = *all* nodes except root, `sort`ed by `created_at`. No `.filter`/`.reduce` keyed on `activity_id` or `span_id`; no substring/suffix match on `event_type`.
- The **only** merge in the file is root-only: `workflow-tree-view.tsx:441-467` finds node where `event_type === "WorkflowStarted"` and node where `event_type === "WorkflowCompleted"` (exact literal match, not suffix-generic) and splices `duration_ms`/`workflow_status` from the completed one onto the started one — but only for `level === 0` (the single workflow root), never for `Activity*` events.

Conclusion: each backend-returned log row renders as its own row, nesting whatever `spans[]` array is embedded on *that specific row*. Any mismatch is a data-shape issue upstream, not a client pairing bug.

### Q3 — duration, span badge, header label source
- Header label: `workflow-tree-view.tsx:637-639` — `displayName = isRootWorkflow ? event.workflow_type || "Workflow" : event.event_type || "Unknown"`. **Raw `event_type` string, no display-name mapping table.** Rendered at `:677`.
- `additionalInfo` ("Bash" in screenshot) = `event.activity_type || event.signal_name`, `:640-641`, rendered `:679-683`.
- "N span" badge: `spanCount = spans.length || event.span_count` (`:643`), where `spans = getSpanNodes(node)` = `event.spans` array verbatim (`:486-490`, no computation/aggregation). Badge rendered `:689-698`.
- Row duration: `formatDuration(event.duration_ms)` — `:363-366` def (`${ms.toLocaleString()}ms`), used at `:703` (mobile) / `:729` (desktop, level>0). **Source is the top-level event's own `duration_ms` field**, not a sum/derivation from the nested span(s). `38,247.09` (2 decimals via default `toLocaleString`) implies `event.duration_ms` arrived pre-converted as a float ms value — no ns→ms conversion visible in this file (would happen upstream in API/hook mapping, not found in FE).

### Q4 — two ActivityStarted-typed rows sharing span_id/activity_id
**Two separate rows.** `getChildNodes` (`:470-483`, cited above) has no dedupe/merge keyed on `activity_id`/`span_id` — confirmed by grep, `activity_id` appears in `useSessionNodes.ts:46` only as a passthrough type field, never as a `Map`/grouping key anywhere in `workflow-tree-view.tsx`. Each `SessionNode.id` (= the governance-event row's own `id`) is the only render key (`:646` `key={node.id}`, `:743-745` children `.map`). Two distinct log rows → two distinct tree rows, full stop, regardless of shared `activity_id`/`span_id`.

### Q5 — ActivityCompleted carrying spans
- `workflow-tree-view.tsx`: `getSpanNodes` (`:486-490`) reads `event.spans` **generically for any node**, no `event_type` branch excluding `"ActivityCompleted"`. If an `ActivityCompleted` row arrives with non-empty `spans[]`, it renders identically to any other node (chevron, "N span" badge, nested `renderSpanNode` list) — nothing blocks or special-cases it.
- Contrast, `detail-modal/tabs/overview/activity-completed-overview.tsx` (full file, 77 lines): never reads `log.spans`/`log.span_count` — shows only `id`/`activity_type`/`duration_ms`/`workflow_id`/`created_at`. It doesn't defensively assert span-less (no guard/error), it simply has no spans UI in that tab — spans on `ActivityCompleted` would silently not render *there* (different surface than the tree view).

## Part B — openbox-temporal-sdk-python

### Q6 — base SDK factory calls
**Not found.** Grepped `openbox/*.py` + `pyproject.toml` for `openbox_core`, `contracts.events`, `contracts import` → zero hits. `pyproject.toml:23-53` dependency list has no `openbox-sdk-python`/`openbox_core` entry at all (only `temporalio`, `httpx`, otel, DB drivers, `cryptography`). This SDK does **not** call `openbox_core.contracts.events.activity_started()/activity_completed()`. It builds event payloads itself, inline, in two places:
- Lifecycle events: `_send_activity_event()`, `activity_interceptor.py:643-676` — plain dict literal, `event_type` = `WorkflowEventType.ACTIVITY_STARTED`/`ACTIVITY_COMPLETED` (enum defined locally, `types.py:26-27`), not imported from any base SDK.
- Hook-triggered span events: `_build_payload()`, `hook_governance.py:185-240` — `payload = dict(activity_context)` + span attach.

### Q7 — emission order around one governed activity
Sequence, one attempt, no hook-abort, per `execute_activity` (`activity_interceptor.py:199-274`):
1. **Lifecycle `ActivityStarted`, hook-less** — `_send_activity_event(info, ACTIVITY_STARTED, activity_input=...)`, call site `:229-233`; payload template `:655-667` has no `spans`/`span_count`/`hook_trigger` keys (only extra kwarg is `activity_input`).
2. Immediately after (`:236-252`), `set_activity_context()` writes a per-`(workflow_id, activity_id)` dict with `"event_type": WorkflowEventType.ACTIVITY_STARTED.value` baked in. This dict is the **sole** source `_build_payload` ever reads (`hook_governance.py:208,226`).
3. Activity body executes (`_run_activity`, `:263-266`). Each OTel-traced call inside it (generic `@trace`, http/db/file governance hooks) invokes `_build_payload(span, span_data)` **twice**: once on entry (`stage:"started"`) and once on exit (`stage:"completed"`) — `tracing.py:33-64` (`stage` param; `end_time = ... if stage=="completed"`), same pattern `http_governance_hooks.py:127`, `db_governance_hooks.py:121`, `file_governance_hooks.py:57` (all set `"stage": stage`). Every such call produces `payload = dict(activity_context)` (**still `event_type:"ActivityStarted"`, untouched since step 2**) + `spans:[span_data]`, `span_count:1`, `hook_trigger:True` (`hook_governance.py:226-229`).
4. Activity returns; `_handle_completion` (`:584-639`) first `clear_activity_context()` (`:606-611`), *then* sends **lifecycle `ActivityCompleted`, hook-less**, with explicit `span_count=0, spans=[]` (`:620-632`).

So: `ActivityStarted(hook-less, no spans)` → `ActivityStarted(hook_trigger=True, span stage="started")` → `ActivityStarted(hook_trigger=True, span stage="completed")` → `ActivityCompleted(hook-less, spans=[])`. The completed-stage span is **by construction never** wrapped in an `event_type=ActivityCompleted` envelope — this is the direct cause of the dashboard row the human flagged as wrong; it is accurately reflecting what the SDK sent.

### Q8 — completed lifecycle event fields; completed-span envelope alternatives
- `attempt`: yes, base template field present on every event incl. `ActivityCompleted` (`activity_interceptor.py:664`, `info.attempt`).
- `error`: yes, explicit kwarg passed through (`:631`).
- `result`: no field literally named `result`. Closest is `activity_output` (`:630`, the serialized return value) — matches FE's `GovernanceEvent.output` naming (`useSessionNodes.ts:56`), not `result`, consistently across both repos.
- Completed-stage span attached elsewhere than an `ActivityStarted`+`hook_trigger` envelope: **No.** `_build_payload` is the only span-attach path; it only ever reads `activity_context`, which is written exactly once per activity with `event_type` hardcoded (`activity_interceptor.py:236-252`) and only ever cleared, never rewritten — confirmed by grepping every `set_activity_context`/`clear_activity_context`/`get_activity_context_by_trace` call site in the package (one setter `:236`, one clear `:609`, two readers `hook_governance.py:208,258`, no other writer exists). After clear (right before the `ActivityCompleted` send), `_build_payload` returns `None` for any later span (`hook_governance.py:208-213`, "no activity context found"), so no span of any stage can attach to the `ActivityCompleted` envelope either — it's structurally impossible, not just unobserved.

## Unresolved
- `hook_type: "shell"` / `shell_command` in the screenshot: no `shell_governance_hooks.py` exists in this SDK (grepped, zero hits); most likely produced via the generic `@trace`-style decorator (`tracing.py:33-64`, takes arbitrary `stage`) applied to a user workflow function, not a dedicated shell hook module. Not confirmed — would need the actual workflow/activity source that triggers it (outside both scoped repos).
- Where FE's `event.duration_ms` gets its ns→ms, 2-decimal-precision float (matching `38,247.09ms` exactly) is not visible in either scoped repo's FE code — likely an API/gateway-layer conversion, out of scope.
