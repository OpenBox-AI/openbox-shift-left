# openbox-core ingestion ground truth: event_type / activity_id / spans

Repo: /Users/phuongvu/Code/openbox/openbox-core (read-only reference). All paths repo-relative.

## Q1 — handler + request struct

Handler: `internal/api/governance.go:28` `(*GroupGovernance).EvaluateGovernanceEvent`.
Route registered: `internal/api/main.go:123` `routesAPI.POST("/governance/evaluate", groupGovernance.EvaluateGovernanceEvent)`.
Decodes into `content.GovernanceEventPayload` (`internal/api/governance.go:41`), struct defined `internal/content/governance.go:186-236`. Full field list:

| field | json tag | line |
|---|---|---|
| Source | `source` | governance.go:188 |
| EventType | `event_type` | governance.go:189 |
| WorkflowID | `workflow_id` | governance.go:190 |
| RunID | `run_id` | governance.go:191 |
| WorkflowType | `workflow_type` | governance.go:192 |
| TaskQueue | `task_queue` | governance.go:193 |
| Timestamp | `timestamp` | governance.go:194 |
| ParentWorkflowID *string | `parent_workflow_id,omitempty` | 197 |
| Status *string | `status,omitempty` | 198 |
| ActivityID *string | `activity_id,omitempty` | 201 |
| ActivityType *string | `activity_type,omitempty` | 202 |
| Attempt *int | `attempt,omitempty` | 203 |
| ActivityInput json.RawMessage | `activity_input,omitempty` | 204 |
| ActivityOutput json.RawMessage | `activity_output,omitempty` | 205 |
| SignalName *string | `signal_name,omitempty` | 208 |
| SignalArgs json.RawMessage | `signal_args,omitempty` | 209 |
| StartTime/EndTime/DurationMs *float64 | `start_time`/`end_time`/`duration_ms` | 212-214 |
| SpanCount int | `span_count,omitempty` | 217 |
| Spans []SpanData | `spans,omitempty` | 218 |
| HookTrigger bool | `hook_trigger,omitempty` | 221 |
| SDKVersion string | `sdk_version,omitempty` (server-set from `X-OpenBox-SDK-Version` header, governance.go:77) | 224 |
| Metadata json.RawMessage | `metadata,omitempty` | 225 |
| MultiAgentSessionID string | `multi_agent_session_id,omitempty` | 227 |
| FromAgentDID string | `from_agent_did,omitempty` | 232 |
| Error *ErrorInfo | `error,omitempty` | 235 |

No `activity_type` field appears elsewhere in this list beyond above — confirmed present. `SpanData` fields (governance.go:266-296ish, truncated read): `span_id, trace_id, parent_span_id, name, kind, start_time, end_time, duration_ns, attributes, status, events, request/response headers+body, semantic_type, stage, data, hook_type, attribute_key_identifiers, error, http_method/url/status_code, db_*`.

## Q2 — event_type accept-list

`internal/api/governance.go:273-286` `isValidGovernanceEventType`. Accepted exact strings (constants at `internal/content/governance.go:12-19`):
`WorkflowStarted, WorkflowCompleted, WorkflowFailed, SignalReceived, ActivityStarted, ActivityCompleted, Handoff`.
Unknown value → `400 {"invalid event_type: <value>"}`, **checked after auth** (comment governance.go:64-65: prevents unauth probing) — governance.go:66-68. `Handoff` additionally requires `multi_agent_session_id` + `from_agent_did` via `content.ValidateHandoffPayload` (governance.go:70-74, content/governance.go:240-245) else 400.

## Q3 — CRITICAL: span persistence gate

**Not gated on `event_type=="ActivityCompleted"`. Gated on `hook_trigger==true` PLUS a pre-existing `governance_events` row for the identical `(agent_id, workflow_id, run_id, activity_id, event_type)` key PLUS new-span dedup.** `event_type` can be `ActivityStarted` or `ActivityCompleted` — the gate does not care which, only that a matching row already exists.

Code path:
1. `governance_workflow.go:197-225` — `CheckExistingEventActivity` looked up with `EventType: &input.Payload.EventType` (the CURRENT event's own type, not the other one) → `validation.go:75` → `FindByWorkflowRunActivityID` (`internal/datastore/governance_event_pgx.go:57-72`) filters `AgentID, WorkflowID, RunID, ActivityID, EventType` all EQ. First call for a given type → not found → `Exists:false` (validation.go:88).
2. `HasNewSpans` is set true ONLY when `input.IsHookEvent` (i.e. payload.HookTrigger) is true and the incoming last span isn't already stored (span_id/stage dedup) — `validation.go:148-196` (`IsHookEvent` set from `HookTrigger` at governance_workflow.go:204).
3. `hasNewSpan := existingEventResult.Exists && existingEventResult.HasNewSpans` — `governance_workflow.go:236`.
4. **HOOK PATH** (`governance_workflow.go:741-818`, `if hasNewSpan`): calls `StoreHookSpanActivity` (`storage_event.go:54-127`) → `storeSpanToTable` (`storage_spans.go:21-51`) → `DatastoreSpan.Create` — this is the ONLY call site of `storeSpanToTable` in the repo (verified: grep shows single caller). It stores only the **last** element of `payload.Spans` (storage_spans.go:26, storage_event.go:64-67), linked via `GovernanceEventID: existingEventResult.ExistingEvent.ID` (the row found in step 1) and `SessionID`.
5. **NORMAL PATH** (`governance_workflow.go:820-837`, when `hasNewSpan` is false — includes the very first event of any type): calls `StoreGovernanceEvent` (`storage_event.go:22-49`) which creates a NEW `governance_events` row via `DatastoreGovernanceEvent.Create` and **never touches the spans table**. `SpanCount` from the payload IS written to `governance_events.span_count` (storage_event.go:140) even though zero span rows get inserted — comment confirms design intent: "Create governance event only (spans stored via hook path)" (governance_workflow.go:821, 824-825).

**Consequence:** a plain `ActivityCompleted` (or `ActivityStarted`) event with `hook_trigger` false/absent and a populated `spans` array is accepted and 200'd, but its spans are **silently dropped** — no rows land in `spans`. Only `hook_trigger:true` events that hit an existing row of the same event_type persist spans. The comment at `storage_event.go:20-21` ("Span storage is handled separately by StoreSpanActivity") is stale — no such activity exists (grep confirms zero hits for a non-Hook `StoreSpanActivity`).

Merkle side confirms same gating: `internal/services/activities/attestation/merkle.go:19-77` `StoreLeafHashesActivity` writes leaf_type `"event"` only `if input.IsNewEvent` (line 34) and leaf_type `"span"` only for whatever `SpanIDs` were actually passed in — which trace back to `hookStoreResult.SpanIDs` from step 4, so unstored spans get no Merkle leaf either.

## Q4 — governance_events ↔ spans relationship, activity_id column, pairing

`activity_id` lands on **`governance_events.activity_id`** — `setOptionalPayloadFields`, `storage_event.go:260-261` (`params.ActivityID = omitnull.From(*payload.ActivityID)`), setter field `internal/bob/models/governance_events.bob.go` (ActivityID column, via GovernanceEventSetter).

`spans` table has **no `activity_id` column**. It links via `governance_event_id` FK + `session_id` only — `internal/bob/models/spans.bob.go:33-35,157-159` (`GovernanceEventID uuid.UUID db:"governance_event_id"`, `SessionID`, `SpanID`). Set at insert: `storage_spans.go:70-73` (`GovernanceEventID: omit.From(eventID), SessionID: omit.From(sessionID), SpanID: omit.From(span.SpanID)`).

**No server-side pairing of ActivityStarted↔ActivityCompleted.** `FindByWorkflowRunActivityID` (datastore/governance_event_pgx.go:57-72) matches `event_type` EXACTLY equal to the incoming payload's own type — so ActivityStarted and ActivityCompleted for the same activity_id produce **two separate `governance_events` rows** (own idempotency key each), never merged/joined. A second lookup exists without the event_type filter — `FindByWorkflowRunActivity` (datastore/governance_event_pgx.go:74-87, `WHERE workflow_id, run_id, activity_id`, no event_type) — but its only caller is `GetApprovalStatusByWorkflow` (`internal/services/governance.go:290-291`), used by the SDK's `/governance/approval` polling endpoint, not by ingestion. It returns whichever single row matches first (event_type-agnostic) — closest thing to "pairing" in the codebase, but it's a read-side poll, not an ingest-time join.

Migrations: only `db/migrations/001_codec_access_log.sql` exists in this repo — `governance_events`/`spans`/`session_merkle_leaves` DDL is **not present in openbox-core**; schema is owned elsewhere (openbox-backend, per project CLAUDE.md) and openbox-core only consumes it via bob-generated models.

## Q5 — ComputeSemanticTypeFromSpan

`internal/content/session.go:204-283` (approx, file truncated at read boundary — body verified to at least line 264). Keys on `span.Attributes` + `span.Name` first via `ComputeSemanticType` (session.go:172-200), priority order: `classifyMCPType → classifyLLMType → classifyLLMGenAI → classifyHTTPType → classifyDBType → classifyFileType`, default `SemanticTypeInternal`. If that returns `internal`, falls back to root-level fields `DBSystem/HTTPMethod/FilePath` (session.go:210-264+).

`classifyMCPType` (session.go:288-297) keys on `attrs["mcp.method"] == "callTool"` → `SemanticTypeMCPToolCall` — **not** on `span.HookType`.

**`span.HookType` is never read by the classifier.** Repo-wide grep: its only usage anywhere is `internal/services/opa.go:649-650`, feeding OPA policy input (`m["hook_type"] = span.HookType`), unrelated to semantic-type computation. No `SemanticTypeShell`/`shell_command` constant exists anywhere in the repo (grep, zero hits outside this negative check).

**Answer: neither `hook_type=shell` nor `hook_type=mcp` is classified first-class by hook_type.** `hook_type=mcp` only yields `mcp_tool_call` if the span separately carries `attrs["mcp.method"]=="callTool"`; absent that (or for `hook_type=shell`, which has no matching classifier or root-level field at all), it falls through to `SemanticTypeInternal`.

## Q6 — OPA/guardrails eligibility by event_type

Two separate gates, both in `governance_workflow.go`:
- **Guardrails** eligibility: `guardrailsEligible := EventType == ActivityStarted || ActivityCompleted || SignalReceived` (governance_workflow.go:429-431) — `WorkflowStarted/WorkflowCompleted/WorkflowFailed` are NOT guardrails-eligible.
- **OPA** (`PolicyEvaluationActivity`): **always runs, unconditional on event_type**, for any event that reaches that stage — "OPA always runs to completion on the parent context" / "OPA + AGE always run" (governance_workflow.go:448, 466, 475-482).

Bypasses that skip OPA entirely (not event_type accept-list, but short-circuits before the OPA launch point):
- `EventTypeHandoff` — short-circuits at the top, writes via `HandoffStorageActivity`, returns Allow/Block without calling OPA/Guardrails/AGE at all (governance_workflow.go:161-190, comment: "aren't subject to OPA/guardrails/AGE").
- Session `halted` or `IsAttested` — auto-`VerdictHalt`, skips OPA/guardrails/AGE/storage/attestation (governance_workflow.go:279-305) — session-state gate, not event_type.
- Approval-cache hit on a hook re-trigger (`hasNewSpan && HookTrigger && ActivityID!=nil && len(Spans)>0`, line 316) — auto-Allow, "skipping OPA/Guardrails/AGE" (governance_workflow.go:340-417, comment line 363).

So `WorkflowStarted/WorkflowCompleted/WorkflowFailed/ActivityStarted/ActivityCompleted/SignalReceived` all go through real OPA evaluation unless caught by one of the three bypasses above.

## Q7 — read-side API / dashboard timeline grouping

**Not found in openbox-core.** Full route inventory (grepped every `.GET/.POST/.PUT/.DELETE` across `internal/api/*.go`): `GET /` (health, main.go:88), `GET /auth/validate` (main.go:118), `POST /governance/evaluate` (main.go:123), `POST /governance/approval` (main.go:124). No query/list/timeline endpoint exists in this repo.

Only "push" mechanism from core: Redis pub/sub via `shared.PublishRealtimeEvent` (`internal/api/governance.go:144-198`), event `content.RealtimeEventGovernanceEvaluated` fired for every verdict plus verdict-specific webhook events for non-Allow — fire-and-forget, no grouping/pairing logic, just `{EventType, OrgID, AgentID, EventID, ...}`. Dashboard read/grouping almost certainly lives in the sibling **openbox-backend** (NestJS control plane, per openbox-shift-left/CLAUDE.md) — out of scope for this repo and not verified here.

## Unresolved questions
- `SpanData` full field list was read only through line ~296 (file continues beyond read window) — DB-specific fields (`db_system`, etc.) not enumerated verbatim, only referenced by name from `session.go` fallback logic.
- Exact DDL for `governance_events`/`spans`/`session_merkle_leaves` not in this repo (no migration) — column types/constraints not verified beyond Go struct tags.
- Did not verify openbox-backend's read-side timeline/grouping logic (separate repo, not fetched).
