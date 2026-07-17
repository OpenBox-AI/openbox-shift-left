# Developer-Runtime Lineage — Development Guide

> **Companion to [`lineage-architecture.md`](./lineage-architecture.md).** That doc
> is the *architecture* (the model, flows, and invariants). This doc is the
> *implementation*: branch strategy, exact change points (files / functions /
> lines), schemas, and how to build / test / verify. Change-point line numbers are
> anchors, not contracts — confirm against the branch tip before editing.

## 1. Branch strategy

One feature branch per repo, both named **`feat/shift-left-lineage`**:

| Repo | Branch **from** | Notes |
|---|---|---|
| **openbox-backend** | `feat/agent-lineage` | The Projects / GitHub-App lineage feature (Mechanism B) lives here — `src/modules/project/`. 50 ahead / 0 behind `develop`. |
| **openbox-core** | `develop` | Deployed staging build (`02eae4d`); has all target files. **Merge `feat/ext-core-dev-runtime-event-types` in** so this branch also carries the E7 dev-runtime work → `feat/ext-core-dev-runtime-event-types` can be **dropped**. |

```sh
# backend
git checkout -b feat/shift-left-lineage origin/feat/agent-lineage
# core
git checkout -b feat/shift-left-lineage origin/develop
git merge --no-edit origin/feat/ext-core-dev-runtime-event-types   # folds E7; merged clean
```

> Local dev-enablement patches (KMS-local provider, reCAPTCHA bypass,
> getOrganization/getUserTeams fallbacks; core Makefile) are **stashed** on `main`
> in each sibling repo (`git stash list` → "local dev patches (main) — shift-left").
> Restore them on `main` for local running; keep the feature branches clean.

The changes map to the [architecture §6 target state](./lineage-architecture.md#6-target-state).

**Build order (decided): C2 first**, then C1, then C3. C4 is deferred.

| # | Repo(s) | Summary | Status |
|---|---|---|---|
| **C2** | **backend** (table + migration) + **core** (writer) | **`deploy_session_links`** — materialize the `commit → session → deploy` JOIN. **Do first.** | High |
| C1 | core | Attestation no-ops for sessionless (Deploy) events | High — stops a Temporal retry storm |
| C3 | backend | Bridge: **always** parse the `OpenBox-Session` trailer in the GitHub-App push handler and link `commit → session` (`verified=false`; no ownership gate) | High |
| C4 | — | Explicit runtime → project **registration** — **deferred**. For now a runtime is linked to a project **only via the commit** (dashboard derives `commit → session → agent`; **shift-left stays project-unaware**). | Deferred |
| — | shift-left | None (reference model correct; project-unaware) | — |

**Decisions (from review):** (1) the JOIN is materialized as **`deploy_session_links`**;
(2) the bridge **always** links, carrying `verified=false` (surface "inferred" in the UI —
no SL-15 gate); (3) runtime↔project is **dashboard-derived via the commit only**, shift-left
does not register with projects; (4) the **`deploy_session_links` table + migration live in
`openbox-backend`** (it owns the shared-DB schema; TypeORM), and **core writes** to it via a
regenerated Bob model.

---

## 2. C1 (core) — attestation must not fail on sessionless Deploy events

**Symptom (live):** a Deploy event (`run_id="deploy-<env>-<sha>"`, no session row) makes
`AttestationWorkflow → GetSessionActivity` retry forever with *"session not found for
workflow … run deploy-…"*.

| Piece | Location |
|---|---|
| Workflow (returns err → retries, `MaximumAttempts:3`) | `internal/services/attestation_workflow.go` — `AttestationWorkflow` L115–132 |
| Failing activity | `internal/services/activities/attestation/session.go` — `GetSessionActivity` L12–27; errors at L27 `fmt.Errorf("session not found for workflow %s run %s", …)` |
| Session lookup | `internal/datastore/session_pgx.go:65` `GetByWorkflowAndRunID` (`WHERE workflow_id=$1 AND run_id=$2`) — returns `nil,nil` for a deploy run |
| Start site | `internal/services/governance_workflow.go` L840–852 → `internal/services/activities/governance/orchestration.go` `StartAttestationWorkflowActivity` L15 |

**Change (smallest, one guard):** soften `GetSessionActivity` to return a sentinel
(`SessionID == uuid.Nil` / `NotFound bool`) instead of erroring when the row is absent;
in `AttestationWorkflow`, `if result.SessionID == uuid.Nil { return nil }` (no-op success).
One edit covers all three start-sites and stops the retry storm.

*(Alt: gate `StartAttestationWorkflowActivity` in `governance_workflow.go` on
`sessionResult.SessionID != uuid.Nil` at all 3 sites — larger, keeps attestation's
"session required" contract intact.)*

**Related invariant (do not "fix" by making the deploy a session member):**
`CheckSessionStatusActivity` (`internal/services/activities/governance/validation.go` L17)
+ the caller guard in `governance_workflow.go` L240–260 HALT any event whose session is
non-`pending`:

```go
if preCheck.SessionStatus != "" && preCheck.SessionStatus != content.SessionStatusPending {
    return &content.GovernanceVerdictResponse{ Verdict: content.VerdictHalt, ... }, nil
}
```

A deploy's authoring session is always `completed`/sealed by deploy time → appending HALTs.
This is why lineage is a **reference/JOIN (C2), not membership** (architecture §3).

---

## 3. C2 (backend table + core writer) — the `commit → session → deploy` JOIN — **do first**

**Today:** the link lives only in `governance_events.metadata` JSONB
(`metadata.sessions[]=[{session_id (a run_id), commit, verified, source}]`, `commit_sha`,
`deploy_id`, `repo`). **No lineage table exists** (no `deploy_*`/`lineage_*`/`*_links` model
in `internal/bob/models/`). `metadata.sessions[]` is never parsed anywhere.

**Ownership (decision #4):** the `deploy_session_links` **table + migration live in
`openbox-backend`** (TypeORM owns the shared-DB schema — same as `sessions`,
`governance_events`). **Core writes** the rows via a regenerated **Bob model**. So:
add the entity + migration in `openbox-backend/src/modules/project/entities/` +
`src/migrations/<ts>-add-deploy-session-links.ts`, regenerate core's Bob models, then add
the core writer below.

**New table `deploy_session_links`:**

| column | type | source |
|---|---|---|
| `id` | uuid PK | gen |
| `deploy_governance_event_id` | uuid FK → `governance_events.id` | store result |
| `deploy_id`, `commit_sha`, `repo` | text | `metadata.deploy_id/commit_sha/repo` |
| `agent_id` | uuid | event agent |
| `session_run_id` | text | `metadata.sessions[].session_id` (a run_id) |
| `session_id` | uuid FK → `sessions.id`, nullable | resolved via `(agent_id, run_id)` |
| `verified`, `source` | bool / text | `metadata.sessions[]` |

Unique `(deploy_governance_event_id, session_run_id)`.

**Populate:** new `StoreDeploySessionLinksActivity` fired from `GovernanceEventWorkflow`
immediately after `StoreGovernanceEvent` succeeds (`governance_workflow.go` L834, which
returns the parent `governance_events.id`), gated on the Deploy signal (e.g.
`EventType==SignalReceived && strings.HasPrefix(RunID, "deploy-")`, or presence of
`metadata.sessions`). Mirror the existing activity pattern:
- Store activity: `internal/services/activities/governance/storage_event.go` `StoreGovernanceEvent` L22 (`setSessionAndTimestamp` L285 leaves `session_id` NULL for deploys).
- Add a `DatastoreSession` resolver keyed by `(agent_id, run_id)` (analogous to `GetByWorkflowAndRunID`, `session_pgx.go:65`).
- Add a `DatastoreDeploySessionLink` datastore (pattern: `internal/datastore/*_pgx.go`) + register the activity in `cmd/core/main.go` alongside the other governance activities.

*(Cheaper alt: resolve+insert inside `StoreGovernanceEvent` after `Create` — it already has `Agent`+`Payload`. Trade-off: mixes concerns into the event-store activity.)*

---

## 4. C3 (backend) — bridge the trailer into Projects lineage

**Where:** `src/modules/project/` on `feat/agent-lineage`. Entities: `project`,
`project-repository`, `project-repository-branch`, `agent-definition`,
`agent-project-registration`, `agent-lifecycle-event`, `agent-code-snapshot`,
**`github-webhook-event`**. Migration `1779800000000-add-project-lineage-schema.ts`.
Logic in `project.service.ts` + `utils/path-matcher.util.ts`.

**Today:** the GitHub-App push handler in `project.service.ts` attributes each commit to a
repo-agent **by source-path** (`path-matcher.util.ts`) and writes an `agent_lifecycle_event`.
It **never reads the commit message** — confirmed: 0 references to `OpenBox-Session`/`trailer`
(the only `message` hits are approval/error strings).

**Change (decision #2 — always link, no ownership gate):** in the push handler that builds
each `agent_lifecycle_event`:
1. Parse the commit **message body** for `^OpenBox-Session:\s*(\S+)$` (git trailers live in the body).
2. Resolve that value against `sessions.run_id` (+ `agent_id`) — `sessions` has `UQ(workflow_id, run_id)`. And/or join on **`commit_sha`** to `deploy_session_links` (C2, same DB).
3. Persist `session_run_id` / `commit_sha` on the `agent_lifecycle_event` row, **always linking** (carry `verified=false` when the trailer is an unverified claim — surface "inferred" in the UI; do **not** gate on SL-15 ownership verification).

**Runtime is derived, not registered (decision #3):** the repo-agent's "Registered runtimes"
is the set of runtime agents reached via `commit → session (run_id) → sessions.agent_id`. No
explicit registration is written; the dashboard derives it from the linked commits.

**Join keys:** `sessions.run_id` (unique with `workflow_id`), `sessions.agent_id → agents.id`,
`commit_sha` (to `deploy_session_links`).

---

## 5. C4 (deferred) — explicit runtime → project registration

**Deferred (decision #3).** For now a runtime is linked to a project **only via the
commit** — the dashboard derives `commit → session → agent` (§4), so a repo-agent's
"Registered runtimes" needs **no** explicit registration and **shift-left stays
project-unaware** (no `openbox dev` project subcommand, no SDK project call). The
`agent-project-registration.entity.ts` create path (a real endpoint on
`project.controller.ts`, and/or a shift-left registration command) is a **later** item if/when
explicit, path-glob-based (non-commit) registration is wanted.

**Seat limit (context, not a change):** the org agent-seat cap (`402 "Agent seat limit
reached (N/M)"`) is enforced on the `feat/agent-lineage` line (absent from the older
`main` state). It is **not** a column on `organization_settings` — locate the
guard/entitlement before relying on it in tests.

---

## 6. shift-left — no change

The reference model is already correct: session events belong to their session, and the
Deploy stays its **own run** referencing the authoring session(s) in `metadata.sessions[]`
(`actions/openbox-git-action/deploy.go` `BuildDeployEvent`). The "make the deploy belong to
the session" attempt was **reverted** — it HALTs against the sealed authoring session. The
FR-7 producer contract (`metadata.sessions[]`) is already emitted; C2/C3 are the consumers.

---

## 7. Build / test / verify

**core** (`feat/shift-left-lineage`):
```sh
cd ../openbox-core
go build ./...            # module builds
go test ./internal/services/... ./internal/services/activities/...   # workflow + activities
```
- C1: unit-test `AttestationWorkflow` with a payload whose session row is absent → expect no-op success (no retry/error).
- C2: unit-test `StoreDeploySessionLinksActivity` — a Deploy event with `metadata.sessions[]` → rows in `deploy_session_links`, `session_id` resolved when the session exists.

**backend** (`feat/shift-left-lineage`):
```sh
cd ../openbox-backend
yarn install && yarn build
yarn test src/modules/project
```
- C3: unit-test the push handler with a commit whose message carries `OpenBox-Session: <run_id>` → the `agent_lifecycle_event` gets `session_run_id`/`commit_sha`.
- C4: e2e the register endpoint → `agent_project_registrations` row + "Registered runtimes" renders.

**End-to-end (UAT-style, the flow this whole effort validated):**
1. `openbox dev init` (real UAT backend) → agent + creds.
2. Fresh Claude Code session does work + commits → SL-5 hook auto-stamps `OpenBox-Session: <run_id>`.
3. Push → GitHub App attributes the commit (Projects) **and**, with C3, links it to the session.
4. `openbox-git-action` deploy → core stores the Deploy event; with C2 it materializes `deploy_session_links`; with C1 attestation no-longer-retries.
5. Verify: Datadog `service:openbox-core-service` — the deploy `GovernanceEventWorkflow` completes and **no `AttestationWorkflow → "session not found"` retries**; dashboard **Projects → repo-agent** renders `commit → session → runtime agent`.

*(Sibling-repo changes stay local — commit on the feature branches; no push/PR until requested.)*

---

## 8. Decisions (resolved) & remaining coordination

**Resolved (this review):**
1. **JOIN home:** one relation — **`deploy_session_links`** (no Projects-DB mirror, no cross-service read). *Table + migration in backend; rows written by core.*
2. **Attribution gate:** **always** link `commit → session`, carry `verified=false` for unverified trailers (no SL-15 gate).
3. **Runtime↔project:** **dashboard-derived via the commit only**; shift-left stays project-unaware (no `dev` project command / SDK).
4. **Migration location:** **backend** (TypeORM, shared-DB schema) — *not* core.

**Remaining coordination (mechanical):**
- **Bob regen:** after the backend `deploy_session_links` migration lands, regenerate core's Bob models so the core writer (§3) compiles against the new table.
- **Multi-session fan-in:** `deploy_session_links` stores **one row per `metadata.sessions[]` entry**, so a fan-in is fully represented (no "primary session" pick needed).
- **Shared DB:** backend and core share one Postgres; confirm the migration runs in the environment core reads from.
