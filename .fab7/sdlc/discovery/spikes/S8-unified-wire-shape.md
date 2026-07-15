# Spike S8 — Verify the unified Activity/hook wire shape on live Core (E7-S0)

**Story:** E7-S0 (gate-0 for Epic E7 / ADR-0004 option B-core). **Run:** 2026-07-14, live local stack (RUNBOOK Path A: Postgres/Redis/Keycloak + Temporal + openbox-core `:8086` + governance/attestation workers). **Backend bypassed** — agent registered via direct SQL (Core auths by Bearer `obx_` token → `GetAgentByToken`, and `ValidateAgentIdentity` returns nil when `signing_required=false`, `openbox-core internal/services/agent.go:108`), so no NestJS/yarn boot was needed.

**Method:** hand-crafted **base-SDK-shaped** `/api/v1/governance/evaluate` payloads (the target wire model, not shift-left's current dev-event shape) for a full developer session and POSTed them to real Core: `WorkflowStarted` → `ActivityStarted`+`hook_trigger` (file/mcp/shell) → `ActivityCompleted` → `SignalReceived` (commit/deploy) → `WorkflowCompleted`. Observed via `psql` on the shared schema + the governance-worker log.

## Verdict: GO — every ADR-0004-critical assumption holds. No wire-shape blocker.

| # | Spike question | Result | Evidence |
|---|---|---|---|
| 1 | Base wire types accepted by **stock** Core (no dev accept-list)? | ✅ **YES** | All 7 events → **HTTP 200**. `WorkflowStarted/ActivityStarted/SignalReceived/WorkflowCompleted` are already in `isValidGovernanceEventType`. → **EXT-core (SL-13) accept-list can be fully retired** (migration §6). |
| 2 | Does session=`Workflow*` disable enforcement (OPA-bypass)? | ✅ **NO — safe** | `internal/services/opa.go:204-212`: OPA is bypassed (auto-allow) **only** for `WorkflowStarted/Completed/Failed`. `ActivityStarted` (= shift-left `ToolCall`, the enforce point) and `SignalReceived` fall through to real `buildOPAInput`+OPA. Worker log confirms `AGECheckActivity`/OPA **ran** for the Activity/Signal events. So mapping sessions→`Workflow*` does not weaken enforcement — enforcement is on the tool call = `Activity`. |
| 3 | Session lifecycle on stock Core (session=workflow)? | ✅ **YES** | `WorkflowStarted` created the session row; `WorkflowCompleted` → `status=completed`. No EXT-core lifecycle edit needed. |
| 4 | `SignalReceived` carries commit/deploy lineage? | ✅ **YES** | `commit_created` + `deploy` stored with `metadata.commit_sha=abc123…`, `deploy_id=dep-9`, `repo=acme/app` (FR-5/6/7 keys survive). → confirms sub-decision #2 (commit/deploy → Signal). |
| 5 | Content gate (INV-2) on the `ActivityStarted` shape? | ✅ **YES** | 0 events carried `request_body`/`response_body`/`input`/`output` for the metadata-only payloads. Activity is guardrails-*eligible* at Core, but nothing is captured unless content-capture is on. |
| 6 | New `EvaluationResult` response shape? | ✅ **YES** | Responses carried `verdict`+`action`+`fallback_used` (the openbox_core `EvaluationResult` fields). `fallback_used=true` here (see caveat). |

## Env caveat (not a wire-shape issue) — one item to re-confirm during E7-S2
- Core's `.env` points `OPA_URL=https://opa.node.lat` and `GUARDRAIL_URL=https://openbox-guardrails.node.lat` (UAT, over a **disconnected VPN**) → OPA returned **404 → fail-open ALLOW** (`fallback_used=true`, worker log "Behavior rule OPA evaluation failed - fallback applied"). So live **verdicts** were all fail-open ALLOW — expected, and orthogonal to the wire shape.
- **`semantic_type` readback was not observable live:** started-stage **hook** spans (`ActivityStarted`) are decision-only and are not persisted to the `spans` table, and my `ActivityCompleted` events hit local persistence quirks (`total_spans:0`; a completed-without-prior-started event wasn't stored as a `governance_events` row). The `spans` table (3872 rows from prior real-SDK runs) shows the classifier does run and yields `span_type`/`semantic_type` (e.g. a no-signal function span → `internal`).
- **This is exactly E7-S2's work** (extend `ComputeSemanticTypeFromSpan`, `session.go:204`) and is unit-testable directly against the classifier. Today's behavior is established: file (`file_path`+name `file.write` → `file_write`), mcp (`attributes["mcp.method"]=="callTool"` → `mcp_tool_call`, `session.go:288-296`), **shell → `internal`** (no shell type) — which is *why* B-core adds first-class `shell`/`mcp` hook types. Re-confirm the extended classifier live under Path C (VPN, reachable OPA/Guardrail) during E7-S2.

## Implications for the E7 backlog (all confirmatory)
- **E7-S2 EXT-core retirement is CLEAR:** stock Core accepts every base wire type + session lifecycle → the SL-13 accept-list patch is removable (no residual, since commit/deploy → Signal).
- **ADR-0004 session=workflow risk is RESOLVED:** the `Workflow*` OPA-bypass is latency-only and does not cover `Activity`/`Signal`, so enforcement on `ToolCall`=`ActivityStarted` is intact.
- **Persisted spans come from `Completed`-stage / real session spans**, not started hooks — so shift-left's `ToolResult`→`ActivityCompleted` (E7-S4) is what carries the durable, classified span; `ToolCall`→`ActivityStarted`+hook is the pre-execution decision. This matches the base SDK's started/completed split and the FrameworkAdapter model.
- **No new blocker for E7-S1/S3+.** Proceed to E7-S1 (base-SDK `shell`/`mcp`/`tool` hook types) → E7-S2 (Core classifier + retire EXT-core).

## Reproduce
Driver scripts captured at `/tmp/obxlogs/spike.py` + `spike2.py` (base-wire payload POSTer). Stack booted per RUNBOOK §2 (infra via `../openbox-backend` compose; Temporal via `obx-temporal` container; core via `/tmp/obxcore {governance-worker,attestation-worker,server}`). Agent: SQL-inserted developer agent, `signing_required=false`, org `localdev.io`.
