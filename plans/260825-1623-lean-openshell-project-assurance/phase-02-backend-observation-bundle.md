# Phase 2 — Dashboard API runtime-activity observation bundle

**Status:** verified — dashboard API runtime-activity collection qualified

**Parent:** [Lean local OpenShell project security evaluation](plan.md)

## Decision and authority

Phase 2 reuses the backend APIs already called by the OpenBox dashboard to show
an agent's persisted runtime activity. It does not add an observation endpoint,
change backend serialization, require backend build metadata, query the
database, or create a second activity API.

Implementation authority covers documentation, Shift Left collection and pack
code, focused tests, and local qualification. It does not authorize credential
creation, backend or infrastructure changes, control mutation, analysis,
recommendations, Apply, commits, or publication.

Phase 1 live qualification remains the execution prerequisite. Backend event
persistence caused by the developer image is the only intended OpenBox
mutation.

## MVP evidence boundary

For security analysis, “all runtime activities” means every session, governance
event, and attached span that the evaluated SDK integration delivered to Core
and the backend persisted for the selected run. It does not claim visibility
into uninstrumented OS behavior.

Phase 3 uses the backend runtime-activity records as its semantic analysis
source. OpenShell logs and run-owned receipts remain in the pack to validate
execution, model routing, correlation, and cleanup, but they do not replace or
invent agent activities.

The dashboard source contract is closed as
`dashboard-session-activity/v1` and consists of:

1. `GET /auth/profile` for the authenticated organization and exact scopes.
2. `GET /agent/{agentId}/sessions?page=N&perPage=100&search={evaluationId}`.
3. `GET /agent/{agentId}/sessions/{sessionId}`.
4. `GET /agent/{agentId}/sessions/{sessionId}/logs/chronological?page=N&perPage=100`.

`GET /health` remains an unauthenticated local-stack readiness check. No
`/agent/{agentId}/observation-profile`, policy, guardrail, behavior-rule,
`/version`, database, or endpoint-discovery request belongs to Phase 2.

## Public command and credentials

Keep the Phase 1 command unchanged:

```text
openbox project evaluate \
  --image IMAGE \
  --env-file FILE \
  --openbox-agent AGENT_ID \
  --output DIR
```

The host collector reads existing `OPENBOX_BACKEND_URL` and
`OPENBOX_CONTROL_TOKEN` configuration. Add no credential flag.

- `OPENBOX_BACKEND_URL` must be exactly `http://127.0.0.1:3000`.
- `OPENBOX_CONTROL_TOKEN` is sent only as `X-API-Key` to the local backend.
- `GET /auth/profile` must report API-key authentication and exactly:

  ```text
  create:agent
  read:agent
  update:agent
  read:agent_session
  read:agent_log
  read:agent_guardrail
  read:agent_policy
  read:agent_behavior_rule
  ```

  This is the existing shared `openbox auth` organization-key contract. Phase 2
  actually calls only profile, session, and log reads; it neither crawls nor
  infers controls from the additional established read scopes.
- `OPENBOX_API_KEY` remains exclusively in the OpenShell provider path. The
  control token never enters the image or provider.
- Missing Ed25519 request attribution is a coverage limitation, not a reason
  to discard persisted activity.

## Preflight and collection sequence

Before stimulus:

1. Generate the fresh evaluation ID.
2. Call `GET /health` without credentials.
3. Call `GET /auth/profile` with the control token and require the exact scope
   set and a non-pending setup state.
4. Call page zero of the dashboard session-list API with the fresh evaluation
   ID and require zero matches. This proves organization/team access to the
   requested agent before stimulus and detects an identity collision.

After the image exits:

1. Poll the dashboard session-list API from page zero and traverse every page.
2. Require exactly one session whose decoded `agent_id` and `run_id` equal the
   requested agent and evaluation ID and whose timestamps fit the bounded run
   window.
3. Wait for `completed`, `failed`, `blocked`, or `halted`. Zero, multiple,
   drifting, unknown, or indefinitely pending matches are `not_runnable`.
4. Fetch the selected session detail and every chronological log page.
5. Require every event to match the agent, session, and run identities.
6. Re-fetch detail, chronological pages, and search pages. Any identity,
   terminal-state, count, pagination, ordering, omission, or duplication drift
   prevents sealing.

## Safe dashboard projection

The collector does not retain an observation-specific backend response. It
consumes the same session/log responses as the dashboard and creates a
canonical public projection before hashing or retention:

- preserve every session, event, span, prompt, input, output, decision,
  timestamp, identity, and pagination field returned by the activity API;
- remove only internal ORM `agent` relations attached to a session,
  `current_step`, embedded governance event, or chronological event;
- never hash, log, or retain the unprojected body;
- reject the projected body if credential names or values remain; and
- label retained entries `dashboard_public_projection`. Health and auth-profile
  entries remain `backend_response`.

This projection is deliberately not described as byte-exact HTTP evidence.
Its byte length and digest cover the canonical retained projection. The raw
response remains bounded in memory by the HTTP limits and is discarded.

## Bounds and failure behavior

Keep the fixed first-lane limits: five seconds per request, one-second polling,
120 seconds for persistence collection, page size 100, at most 100 pages per
pass, 8 MiB per raw response, 64 MiB retained total, 32 KiB headers, and 1,000
requests.

Disable proxies, redirects, cookies, compression, and unbounded connection
reuse. Require `200`, JSON with optional UTF-8 charset, known envelopes,
zero-based pagination, declared-length consistency, valid UTF-8, and no trailing
JSON. Every backend request is GET-only.

On any preflight, execution, collection, projection, validation, or cleanup
failure, publish only the Phase 1 `.incomplete` diagnostic form. On complete
success, publish only the sealed observation pack. Never mix the two forms.

## Observation pack

Keep the separate `ai.openbox.project-observation/v1` inventory and the exact
payload set:

- `run.json` records invocation, image, local dashboard API contract,
  organization, selected session, lifecycle, and cleanup.
- `backend.json` records the ordered GET descriptors and retained canonical
  bodies, with `source_contract: dashboard-session-activity/v1` and a
  representation label per entry.
- `openshell.jsonl` records bounded execution-host observations.
- `effects.json` records run-owned receipt results and explicit absence.
- `behavior.json` indexes analysis-citable persisted backend events/spans and
  retains existing execution-source references needed for validation.
- `coverage.json` records observed and missing channels, including unsigned
  request attribution and SDK visibility limits.
- `manifest.json` is canonical, written last, and addresses exactly the six
  payloads.

Do not modify, migrate, or dual-write the frozen `openbox.audit-pack/v1`
inventory. Publication remains an owner-only exact-file, manifest-last,
no-replacement transaction.

## Ordered tasks

| ID | Status | Implementation and evidence |
|---|---|---|
| OS-02-01 | verified | Existing dashboard session-list, session-detail, and chronological-log collection, exact session resolution, bounded pagination, and stability checks pass focused tests and the corrected live Mastra run. |
| OS-02-02 | verified | Public evaluator integration, exclusive diagnostic/pack output, Mastra conformance execution, real terminal session, model route, safe effect, and cleanup were previously live-verified. |
| OS-02-03 | verified | Canonical dashboard public projection, credential refusal, event indexing, coverage, and schema reconciliation pass focused, adversarial, immutable-reader, and live-pack checks. |
| OS-02-04 | verified | Separate observation schemas, exact-file transaction, immutable reader, manifest-last finalization, and runfs adversarial behavior were previously verified. |
| OS-02-05 | verified | Observation-only backend endpoint/build-tuple dependencies and the control crawl are removed; backend source is unchanged, contracts/tests use dashboard APIs, and the regenerated Mastra pack contains only the corrected request sequence. |

No task is active. The corrected live evidence is
`evidence/2026-08-26-phase-02-public-mastra-dashboard-observation-04`.

## Test and acceptance plan

Deterministic tests must cover:

- exact request order and GET-only authentication placement;
- exact shared-scope profile equality, local URL, proxy, redirect, content-type,
  size, deadline, and redaction behavior;
- zero-result preflight on the dashboard session API;
- zero, multiple, delayed, pending, cross-window, wrong-agent, wrong-run,
  changed-status, changed-count, duplicate-page, malformed, and truncated
  responses;
- removal of internal `agent` relations before hashing while retaining all
  event/span activity fields;
- refusal of remaining credentials, deterministic behavior ordering, source
  references, contradictions, and missing channels;
- exact output exclusivity, permissions, symlinks, hard links, mutation,
  omission, extra files, interrupted finalization, and digest mismatch; and
- focused CLI/evaluate/observation/runfs tests, full Go modules, race, vet,
  cross-platform compilation, schema parsing, and `git diff --check`.

Phase acceptance requires one regenerated Mastra observation pack containing
one exact terminal session and every chronological event/span page through the
dashboard API contract, with no direct database access, new backend endpoint,
fake SDK receiver, raw credential, or container-log substitution and no
sandbox, registry, fixture, or loaded-model residue. A second developer image
remains post-MVP backlog.
