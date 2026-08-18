# openbox-core governance-workflow surface: dev-session HALT-latch fix

Repo researched: `/Users/phuongvu/Code/openbox/openbox-core`. Read-only, no edits.

## Q1 — the two rejection blocks: conditions + data in scope

**Block 1, pre-check** (`internal/services/governance_workflow.go:232-254`):
```go
var preCheck *CheckSessionStatusOutput
err = workflow.ExecuteActivity(ctx, "CheckSessionStatusActivity", input.Payload.WorkflowID, input.Payload.RunID).Get(ctx, &preCheck)
...
if preCheck.SessionStatus != "" && preCheck.SessionStatus != content.SessionStatusPending {
    // returns VerdictHalt, Metadata{event_type,workflow_id,session_status}, reason "Session is no longer active" (default)
```
Returns immediately (`:244-253`) — **before** `SessionLifecycleActivity`, OPA, guardrails, AGE, or any `StoreGovernanceEvent` call. No policy_id, no governance_events row.

**Block 2, post-lifecycle** (`governance_workflow.go:273-299`):
```go
if sessionResult.SessionStatus == "halted" || sessionResult.IsAttested {
```
Also returns immediately, same shape (`metadata["attested_session"]=true` or `["halted_session"]=true`), also before OPA/guardrails/AGE/storage.

**Data in scope at both points** (nothing beyond what's already fetched):
- `agent *content.Agent` — fetched at `governance_workflow.go:153-157` (step 1, before either block). Full struct incl. `AgentType *string` db:"agent_type" (`internal/content/agent.go:16-41`, field at line 21), `OrganizationID`, `ID`, `Status`, `Config json.RawMessage`.
- `preCheck` = `CheckSessionStatusOutput{SessionID, SessionStatus, SessionDetail}` (`governance_workflow.go:73-77`), sourced from `content.Session{ID,Status,Detail}` only (`activities/governance/validation.go:20-31`).
- `sessionResult` (block 2 only) = `SessionLifecycleOutput{SessionID,Action,SessionStatus,SessionDetail,IsAttested}` (`governance_workflow.go:106-112`).
- `input.Payload *content.GovernanceEventPayload`, full field list at `internal/content/governance.go:194-245` (see Q2).
- `content.Session` struct, full fields at `internal/content/session.go:24-38`: ID, AgentID, WorkflowID, RunID, Status, Detail, StartedAt, CompletedAt, CreatedAt, UpdatedAt, Flagged, FlagReason, TrustEvaluatedAt.

## Q2 — is agent kind (developer) reachable here?

**No `kind=developer` marker exists anywhere in openbox-core.** Repo-wide grep (non-test) for `"developer"`, `KindDeveloper`, `AgentTypeDeveloper`, `kind.*developer` → zero hits.

- Only plausible column: `content.Agent.AgentType *string` db:"agent_type" (`internal/content/agent.go:21`), backed by a real DB column (`internal/bob/models/agents.bob.go:36`, `internal/bob/dbinfo/agents.bob.go:54,264`). Nothing in this repo ever writes or reads it as `"developer"` — it may be set by openbox-backend directly (out of scope repo) or may not encode kind at all. **Unverified.**
- `agent` (incl. `AgentType`) is already in scope before both blocks via `ValidateAgentActivity` → `ServiceAgent.ValidateAgent` (`internal/services/agent.go:279-281`) → `GetAgentByToken` (`internal/services/agent.go:260`). **Zero new query needed** to read `agent.AgentType` at either rejection point — I did not read `GetAgentByToken`'s body/SQL to confirm it actually populates `AgentType` from the row scan (not verified, only structurally inferred).
- `content.Session` (`session.go:24-38`) has **no** kind/type/source column.
- `content.GovernanceEventPayload`, full struct `internal/content/governance.go:194-245`: `Source string` (`:196`, doc comment says value is `"workflow-telemetry"`), `EventType`, `WorkflowID`, `RunID`, `WorkflowType`, `TaskQueue`, `HookTrigger`, `MultiAgentSessionID`, `FromAgentDID`, `SDKVersion`, `Metadata json.RawMessage`. **No** kind/session_type/developer field. `Source` is the closest structural hook (free string, currently only ever documented as one value) but is not populated with a dev/runtime distinction anywhere in this repo.
- Grep repo-wide (non-test) for `workspaceID`, `developerDID`, `session_type`, `SessionType` → zero hits. `WorkflowID`/`RunID` are treated as fully opaque strings by core; no format-based parsing exists to detect a composed `workspaceID||developerDID` id.

**Conclusion:** distinguishing dev vs. agent-runtime sessions inside `GovernanceEventWorkflow` today requires either (a) trusting `agent.AgentType` — reachable for free, value unverified — or (b) a new field/marker (new work, not "reuse"). No existing session/payload signal works.

## Q3 — `UpdateSessionHaltedActivity`

Full body (`activities/governance/validation.go:34-46`):
```go
func (a *GovernanceActivities) UpdateSessionHaltedActivity(ctx context.Context, sessionID uuid.UUID, reason *string) error {
    logger := activity.GetLogger(ctx)
    logger.Info("UpdateSessionHaltedActivity", "session_id", sessionID)
    err := a.DatastoreSession.UpdateStatus(ctx, sessionID, content.SessionStatusHalted, time.Now(), reason)
    if err != nil { logger.Error(...); return err }
    logger.Info("Session status updated to halted", "session_id", sessionID)
    return nil
}
```
Callers (repo-wide grep):
- `cmd/core/main.go:517-518` — Temporal activity registration only.
- **`internal/services/governance_workflow.go:740-746` — the only invocation site**, guarded by `if verdict.Verdict == content.VerdictHalt && sessionResult.SessionID != uuid.Nil` (`:741`), placed AFTER OPA/guardrails/AGE aggregation (`:700-717`) and BEFORE hook-span/governance-event storage. This fires on **any** aggregated HALT verdict (policy, guardrail, or AGE) for an in-flight session — this is the exact "latch" the background describes: once it runs, `session.Status="halted"`, and every later event for that `workflow_id`/`run_id` hits Block 1 (Q1) and is auto-HALTed with no policy_id/governance_events row, forever.
- Tests: `governance_workflow_test.go:56,114,480-481,1004-1005` — mocked for two scenarios, OPA/policy STOP (`:470-476`) and guardrails STOP (`:988-1000`), confirming it fires from downstream policy/guardrail verdicts, not from either early-rejection block. `activities/governance/validation_test.go:290-318` unit-tests the activity directly (Success, StoreError paths).

## Q4 — attested-session rejection

Not a stored session-row column. `IsAttested` is computed live by a join in `SessionLifecycleActivity`'s two sub-handlers:
- `handleSessionTerminal` (`activities/governance/storage_session.go:138-169`): queries `a.DatastoreSessionAttestation.GetBySessionID(ctx, session.ID)` (`:152`); non-nil → `isAttested=true` (`:155-156`).
- `handleSessionLookup` (`storage_session.go:243-274`): same query (`:259`), same logic (`:262-263`).
Writer of the underlying `SessionAttestation` row is the attestation workflow/signing path (`content.SessionAttestation`, `internal/content/session.go:40-50`; realtime event const at `activities/attestation/sign.go:128`) — not read in depth (out of budget).

Stated purpose, from the code comment at `governance_workflow.go:273-274`: *"reject events for halted or attested sessions only / Skip all downstream evaluations (OPA, guardrails, AGE, storage, attestation)"*. No explicit DB constraint or invariant doc found asserting immutability past attestation (attestation-workflow internals not read). Risk if storing a late dev event on an attested session: the event would be either excluded from the already-signed Merkle root (silent lineage gap — `EventMerkleNode` links events into the tree, `session.go:52-54`, not fully read) or require re-attestation, which this code path isn't designed to do.

**Important scoping note for the fix:** attested-session rejection is independent of the HALT-latch mechanism (Q3) — it's driven by a *completed attestation workflow*, not by `UpdateSessionHaltedActivity`. The background's fix directive ("never reject for session-status reasons," "never latch on a HALT") maps cleanly onto the `SessionStatus=="halted"` half of Block 2 and all of Block 1, but does **not** obviously extend to the `IsAttested` half — storing on an attested session is a genuinely different risk (lineage/signature), not a self-inflicted bricking bug. The plan should decide explicitly whether dev sessions skip the attested check too, or keep it.

## Q5 — test conventions

- `storage_event_test.go` (833 lines) mixes: (a) plain Go unit tests calling internal pure functions directly, no env, e.g. `buildGovernanceEventSetter(input, noopLogger{})` (`:43-56`) using a hand-rolled `noopLogger` (`:20-26`) implementing `shared.ActivityLogger`; (b) `testsuite.TestActivityEnvironment`-style calls — `env.RegisterActivity(activities.StoreGovernanceEvent)` then `env.ExecuteActivity(activities.StoreGovernanceEvent, services.StoreGovernanceEventInput{...})` (`:587-588`, same pattern for `StoreHookSpanActivity` at `:800-801`) against mocked datastores (testify `mock`).
- `governance_workflow_test.go` uses `testsuite.WorkflowTestSuite` → `NewTestWorkflowEnvironment()` + a `registerTestActivities(env)` helper + per-activity `env.OnActivity(activities.X, mock.Anything, ...).Return(...)`, then `env.ExecuteWorkflow(GovernanceEventWorkflow, input)` and asserts via `env.GetWorkflowResult(&result)`.
- **Existing rejection-path tests, both hit only Block 1**: `TestGovernanceEventWorkflow_HaltedSession` (`:1347-1403`) and `TestGovernanceEventWorkflow_CompletedSession` (`:1406-1460`) — both mock `CheckSessionStatusActivity` to return a non-pending status directly; `SessionLifecycleActivity` is never mocked, comment states "Pre-check rejects before any of these are called" (`:1387-1388,1444-1445`).
- **No test exercises Block 2** (`sessionResult.SessionStatus=="halted" || sessionResult.IsAttested`, `:275`) — grep for `IsAttested|Attested` in `governance_workflow_test.go` returned zero hits. The only attested-related test, `TestSessionLifecycleActivity_WorkflowCompleted_AlreadyAttested_SkipsUpdate` (`storage_session_test.go:286`), tests the activity in isolation, not the workflow's HALT-return branch.
- No replay/recorded-fixture tests for agent-runtime sessions found anywhere under `internal/services` (grep for `ReplayWorkflowHistory|history.json|golden` only matched unrelated guardrail-parity files).
- `c7a93f3` = "PROD-314 fix(governance): persist event-level goal-alignment evaluations" — ticket-prefixed conventional commit; no special fixture/replay pattern beyond the mock-env style above.

## Unresolved questions

1. Where/how is `kind=developer` actually set at agent registration (`POST agent/create`)? Not in openbox-core — likely openbox-backend (sibling repo, out of scope here). Does it land in `agents.agent_type`, or nowhere queryable at all?
2. Does `GetAgentByToken` (`internal/services/agent.go:260`) actually SELECT/scan `agent_type` into the returned `*content.Agent`? Assumed from struct shape, not verified by reading its body.
3. Should dev sessions skip the **attested** half of Block 2, or only the **halted-status** half (Q4)? Background text says "session-status reasons," which reads as status, not attestation — plan should confirm this scope explicitly.
4. Attestation-workflow internals (writer of `SessionAttestation`, Merkle-tree linkage in `EventMerkleNode`) not read — can't independently confirm the "storing post-attestation breaks lineage" claim beyond the code comment at `governance_workflow.go:273-274`.
