# Synthesis — what actually happens to a tool call, end to end

Sources: [scout-01](../scout/scout-01-shift-left-current-shape.md) (this repo, first-hand),
[researcher-01](researcher-01-openbox-core-ingestion.md) (openbox-core),
[researcher-02](researcher-02-dashboard-and-runtime.md) (openbox-fe + agent runtime).
Load-bearing core claims re-verified first-hand in `openbox-core` (citations below).

## 1. The exact lifecycle of one tool call today

shift-left sends **2 POSTs**, both `event_type=ActivityStarted`, `hook_trigger=true`,
same `activity_id`, same `span_id`, differing only in `spans[0].stage`.

Core's routing (`internal/services/governance_workflow.go`, verified first-hand):

```
hasNewSpan := existingEventResult.Exists && existingEventResult.HasNewSpans   // :236
if hasNewSpan { … StoreHookSpanActivity … return verdict, nil }                // :741, :817
// NORMAL PATH: "Create governance event only (spans stored via hook path)"    // :820
```

`Exists` comes from `FindByWorkflowRunActivityID`, which filters
`(agent_id, workflow_id, run_id, activity_id, event_type)` **all EQ — including the
event's own event_type** (`internal/datastore/governance_event_pgx.go:57-72`).
The two paths are mutually exclusive (the hook branch returns at `:817`).

So, per tool call:

| POST | Core path | What lands |
|---|---|---|
| 1. ToolCall, `stage=started` | NORMAL (no row yet for `(activity_id, ActivityStarted)`) | **1 `governance_events` row** (`span_count=1`) + event Merkle leaf. **0 span rows** — the started span is discarded. Full OPA + Guardrails + AGE, *with the span present*. |
| 2. ToolResult, `stage=completed` | HOOK (row exists, span is new by `span_id`+`stage`) | **1 `spans` row** attached to POST 1's event row via `governance_event_id` + span Merkle leaf. OPA runs; Guardrails deliberately **not** re-evaluated (`verdict.GuardrailsInternal = nil`, `:776-778`). |

**That is the screenshot, exactly.** One row labelled `ActivityStarted` (created by
the *ToolCall*), carrying exactly one span whose `stage` is `completed` (delivered by
the *ToolResult*). The header's `38,247.09ms` is the event row's own `duration_ms`;
the FE reads that field directly, not derived from the span
(`workflow-tree-view.tsx:643,703`).

`spans` has **no `activity_id` column** — it links only by `governance_event_id` FK +
`session_id` (`internal/bob/models/spans.bob.go:33-35`). That FK is *why* a completed
span renders nested under an ActivityStarted header: there is no other row it could
hang from.

## 2. What the reference implementation does

The agent runtime emits **4 events per activity**
(`openbox-temporal-sdk-python/openbox/activity_interceptor.py:199-274`):

```
1. ActivityStarted   hook-less, no spans          (:229-233)
2. ActivityStarted   hook_trigger, stage=started  (hook_governance.py:185-240)
3. ActivityStarted   hook_trigger, stage=completed
4. ActivityCompleted hook-less, spans=[]          (:620-632)
```

`activity_context` is written once with `event_type` hardcoded and only ever cleared,
never rewritten (`:236-252`, `:606-611`) — a completed-stage span riding an
`ActivityCompleted` envelope is **structurally impossible** there, not merely absent.

**shift-left emits #2 and #3 and neither bookend.** That is the real gap: the tool
execution is an activity by `activity_id` alone; no event ever *declares* or *closes*
it.

## 3. Why the literal proposal ("put the span in ActivityCompleted") fails

Not a style objection — it loses data, provably, in three independent places:

1. **Core silently drops the spans.** Span persistence has exactly one call site,
   `storeSpanToTable` ← `StoreHookSpanActivity`, reachable only via `hasNewSpan`.
   A `ToolResult` re-typed to `ActivityCompleted` has no pre-existing row for
   `(activity_id, ActivityCompleted)` → `Exists=false` → `hasNewSpan=false` → NORMAL
   PATH → the row is created, `span_count` is written from the payload
   (`storage_event.go:140`), and **zero span rows are inserted**. HTTP 200, no data.
   This holds whether or not `hook_trigger` is set: the gate needs a *pre-existing row
   of the same event_type*, which one POST cannot produce.
2. **No Merkle span leaf.** `StoreLeafHashesActivity` writes `"span"` leaves only for
   `SpanIDs` returned by the hook path (`attestation/merkle.go:34`). No span stored ⇒
   no leaf ⇒ the evidence chain loses the tool call.
3. **Two contracts reject it before the wire.** `event_rules.py:79-85`
(`ACTIVITY_COMPLETED_WITH_SPANS`) and shift-left's own `AssertHookWireShape`
(`client/hookspan.go:103`). That decision already reversed this exact proposal
once.

## 4. Corrections to the research (do not propagate)

- **researcher-01 Q5 conflicts with observed reality.** It reports no `shell_command`
  semantic type in openbox-core; I confirmed the grep — zero hits in
  `internal/content/session.go`'s constant block, and that checkout is on branch
  `feat/policy-decision-full-4-verdicts`. But the screenshot shows
  `semantic_type: "shell_command"` on a live span, and the client is forbidden from
  sending it. So either the running stack is ahead of the local checkout (E7-S2
  landed) or the classifier lives in openbox-backend. **Treat the screenshot as
  authoritative; MAPPING.md:110's "E7-S2 pending" note needs re-verification against
  the deployed core, not this checkout.**
- **My earlier `workflow_type` concern is weaker than I stated.** The hook path does
  omit `workflow_type` (`client/payload.go:242-247`) where the base SDK includes it
  (`contracts/context.py:48`). But no core lookup we found keys on it —
  `FindByWorkflowRunActivityID` filters agent/workflow/run/activity/event_type only.
  So this is a **base-contract divergence and a MAPPING.md accuracy issue**
  (MAPPING.md:20-22 asserts sessions key on `(workflow_id, run_id, workflow_type)`,
  uncorroborated), not a demonstrated session split. Verify before acting.
- **openbox-fe has no pairing whatsoever**, so no shape change can be validated by
  "the dashboard will merge them": one backend row → one UI row, keyed `log.id`
  (`workflow-tree-view.tsx:312-329`, `:470-483`). The only merge is root-level
  `WorkflowStarted`/`WorkflowCompleted` at `level===0` (`:441-467`).
- **openbox-core exposes no read/timeline API** (only `/governance/evaluate`,
  `/governance/approval`, `/auth/validate`, `/`). The dashboard's read side is
  openbox-backend — unverified, and it owns the `governance_events`/`spans` DDL too
  (no migrations for them in openbox-core).

## 5. Consequences that constrain any fix

- The **started span can only ever persist if some earlier POST already created the
  `(activity_id, ActivityStarted)` row.** That is inherent to core's gate, and it is
  the only reason the reference runtime's hook-less bookend matters for storage.
- Conversely, adding that bookend **moves Guardrails off the span-bearing event**:
  today POST 1 carries the span through the NORMAL path with full Guardrails; if a
  span-less `ActivityStarted` goes first, every span-bearing POST takes the hook path,
  where Guardrails are explicitly skipped (`:776-778`). Core-side Guardrails would no
  longer see a tool span at all.
- The completed span is an information **superset** of the started one (same
  `span_id`, carries `start_time` plus `end_time`/`duration_ns`/family fields), so
  "the started span is dropped" costs an audit row, not content.
- Event volume is the real currency: this branch (`feat/realtime-event-delivery`) just
  added a debounced per-session flusher, so any multiplier here multiplies realtime
  egress too.

## Unresolved questions

1. Which shape does the org want (see the options matrix in `plan.md`)? Volume vs.
   semantic fidelity vs. cross-repo cost — an OD-class decision.
2. Does openbox-backend's read side group `Activity*` rows by `activity_id`? If it
   does, a wire change may be unnecessary; if not, only an FE change fixes the visual.
3. Is the deployed core ahead of the local checkout on semantic-type classification
   (`shell_command`)? Determines whether MAPPING.md:110 is stale.
4. Does `StoreHookSpanActivity` update the event row's `span_count`? Today the counts
   agree by coincidence (1 declared, 1 stored); a bookend changes that arithmetic.
