# Developer-Runtime Lineage — Architecture

> **Scope.** How `session → commit → deploy` lineage flows across the three planes
> (**shift-left** developer runtime · **openbox-core** data plane · **control-plane
> Projects** service), and why the chain does not render end-to-end today. This doc
> is the *model*. **Implementation — branch strategy, change points, schemas,
> build/test — is in [`lineage-development.md`](./lineage-development.md).**

## TL;DR

There are **two independent lineage mechanisms** that never join:

- **Mechanism A — governance telemetry** (shift-left → core): session events, the
  `OpenBox-Session` commit trailer, and the Deploy event. Knows `commit → session →
  deploy`, but only as **references in the Deploy event's `metadata`** — there is **no
  queryable JOIN** (FR-7, deferred).
- **Mechanism B — GitHub-App "Projects"** (control plane): a repo connected via a
  GitHub App; on push, commits are attributed to "repo-agents" **by source-path**.
  Renders `commit → repo-agent`, but **ignores the `OpenBox-Session` trailer**, so it
  never links a commit to a **session** or **runtime agent**.

Result: the commit shows in the project lineage (B), the session/deploy exist in core
(A), but the dashboard cannot draw **commit → session → agent** — the trailer that
connects them is produced by A and ignored by B.

A third, orthogonal invariant: a Deploy event **cannot be a member of its authoring
session** (that session is terminal / Merkle-sealed by deploy time), so lineage must be a
**reference / JOIN, not membership**.

---

## 1. Planes & roles

```mermaid
flowchart LR
  subgraph DEV["Developer runtime — shift-left (this repo)"]
    CC["Claude Code hooks<br/>(SL-4 adapter)"]
    GH["git prepare-commit-msg<br/>(SL-5 trailer)"]
    GA["openbox-git-action<br/>(SL-6 deploy)"]
  end
  subgraph CORE["Data plane — openbox-core (Go + Temporal, EKS)"]
    EV["POST /api/v1/governance/evaluate"]
    GW["GovernanceEventWorkflow"]
    AW["AttestationWorkflow<br/>(Merkle seal)"]
    DB[("sessions · governance_events<br/>guardrails/policy_evaluations<br/>session_merkle_leaves")]
  end
  subgraph CTRL["Control plane — backend + Projects service"]
    REG["POST /agent/create<br/>(agent registry, KMS DID)"]
    PROJ["Projects / GitHub-App lineage<br/>(project_*, agent_definitions,<br/>project_agent_lifecycle_events)"]
    DASH["Dashboard reads"]
  end
  GHUB[("GitHub<br/>(public repo)")]

  CC -- "AIP-signed events" --> EV
  GA -- "AIP-signed Deploy event" --> EV
  EV --> GW --> DB
  GW --> AW --> DB
  GA -. "OPTIONAL: ownership verify (SL-15)" .-> REG
  CC -. "dev init: register agent" .-> REG
  GH -- "stamps OpenBox-Session trailer" --> GHUB
  GA -- "git push" --> GHUB
  GHUB -- "GitHub App webhook (push)" --> PROJ
  DB --> DASH
  PROJ --> DASH
```

| Plane | Role |
|---|---|
| **shift-left** | **Produces** lineage: session events, commit trailer, deploy event |
| **openbox-core** | **Ingests & stores** governance events; runs the governance workflow; Merkle-seals sessions |
| **Control plane (backend + Projects)** | Agent registry; **GitHub-App project lineage**; dashboard reads |

---

## 2. The identity / join keys

Everything hinges on a few identifiers:

| Concept | Key |
|---|---|
| Session identity | `(workflow_id = agent DID, run_id = CC session id)` → a session (UUID). Created on `WorkflowStarted`. |
| Governance event | `{agent_id, session_id (FK, nullable), run_id, workflow_id, event_type, verdict, metadata}` |
| **commit → session** | `OpenBox-Session: <run_id>` git trailer (SL-5) — an *unverified claim* |
| **deploy → commit → session** | Deploy event `metadata.{commit_sha, deploy_id, sessions[]}` (each `sessions[].session_id` is a **run_id**) |
| commit → repo-agent | GitHub-App lifecycle event `{commit_sha, repo-agent}` — matched **by source-path** |
| runtime ↔ repo-agent | a runtime↔repo-agent registration |

> **Gotcha:** `metadata.sessions[].session_id` is a **run_id**, not the session UUID.
> Resolving it needs `(agent_id, run_id)` (the unique session key).

---

## 3. Mechanism A — governance-telemetry lineage (shift-left → core)

```mermaid
sequenceDiagram
    autonumber
    participant CC as Claude Code (SL-4)
    participant GIT as git (SL-5 hook)
    participant CI as openbox-git-action (SL-6)
    participant CORE as core /evaluate + GovernanceEventWorkflow
    participant DB as core DB

    CC->>CORE: SessionStart → WorkflowStarted (workflow_id=DID, run_id=CCsid)
    CORE->>DB: create sessions row (id=UUID, status=pending)
    CC->>CORE: UserPromptSubmit→SignalReceived, tools→ActivityStarted
    CORE->>DB: governance_events (session_id FK set) + policy/guardrail/AGE
    CC->>CORE: SessionEnd → WorkflowCompleted
    CORE->>DB: session status=completed → AttestationWorkflow Merkle-seals
    Note over GIT: developer commits → hook stamps<br/>OpenBox-Session: <run_id>  (unverified claim)
    CI->>CI: read trailer → resolve session(s)
    CI->>CORE: Deploy event (run_id="deploy-<env>-<sha>",<br/>metadata.sessions[]=[run_id], commit_sha)
    CORE->>DB: governance_events (event_type=Deploy,<br/>session_id = NULL — no session row for the deploy run)
```

**What's solid:** every *session* event belongs to a real session (`session_id` FK set),
and terminal sessions are Merkle-sealed. The `commit → session → deploy` chain **exists**,
but only as **references in the Deploy event's `metadata.sessions[]`** — making it a
queryable `commit ⋈ session ⋈ deploy` relation is **FR-7 (deferred)**.

**Invariant — a Deploy is not a *member* of its authoring session.** A deploy always runs
*after* the coding session ends, so that session is already **terminal and Merkle-sealed**
by deploy time. Core enforces **append-only-until-complete**: adding an event to a
completed session is rejected (HALT) — that is the tamper-evidence guarantee of the Merkle
chain. (Verified live: attaching the deploy to its session → `status completed` → HALT →
dropped.) So the Deploy is its **own run** that *points at* the authoring session(s); the
connection is a **reference / JOIN, never membership**.

---

## 4. Mechanism B — GitHub-App "Projects" lineage (control plane)

```mermaid
sequenceDiagram
    autonumber
    participant GH as GitHub (push to public repo)
    participant APP as GitHub App webhook ingestion
    participant PDB as Projects tables
    participant UI as Dashboard (Projects)

    GH->>APP: push event (commit sha, author, changed files)
    APP->>APP: match changed files ⇢ repo-agent source-path globs
    APP->>PDB: lifecycle_event{commit_sha, repo-agent, "push", success}
    APP->>PDB: code_snapshot{commit_sha, changed_files}
    UI->>PDB: read repo-agent → Lifecycle events + Registered runtimes
    Note over APP: ❌ commit MESSAGE (the OpenBox-Session trailer) is NOT read<br/>❌ no link to sessions / runtime agents
```

Verified **live on UAT** (2026-07-17): a connected repo, commits attributed to a repo-agent
**by path**, rendered in the dashboard's **Projects** view.

> **Note.** Mechanism B lives in the control plane's **Projects service** and attributes
> commits to repo-agents purely by source-path — it **does not read the commit message**,
> so it never sees the `OpenBox-Session` trailer. The natural bridge key is **`commit_sha`**
> (present on both sides) and/or the **trailer → session `run_id`**.

---

## 5. The gap — where A and B fail to meet

```mermaid
flowchart TB
  subgraph A["Mechanism A (core)"]
    S["sessions<br/>(run_id, agent_id)"]
    GE["Deploy governance_event<br/>metadata.sessions[]=run_id, commit_sha"]
    S --- GE
  end
  subgraph B["Mechanism B (Projects)"]
    LE["lifecycle_event<br/>(commit_sha, repo-agent)"]
    AR["runtime ↔ repo-agent registration"]
  end
  C(["commit"])
  C -- "OpenBox-Session trailer<br/>(read by A)" --> GE
  C -- "source-path match<br/>(read by B)" --> LE
  GE -. "commit_sha — no JOIN (FR-7)" .-x LE
  LE -. "trailer ignored — no session link" .-x S
  classDef gap stroke-dasharray:5 5,stroke:#c0392b,color:#c0392b;
  class GE,LE gap;
```

- **A** has `commit → session → deploy` (trailer + Deploy metadata) but **no queryable JOIN**
  and doesn't feed the Projects UI.
- **B** has `commit → repo-agent` (by path) and renders it, but **ignores the trailer**, so
  no `session` / `runtime` link.

---

## 6. Target state

```mermaid
flowchart LR
  C(["commit + OpenBox-Session trailer"])
  subgraph CORE["core (writes rows)"]
    GE["Deploy governance_event"]
    DSL["deploy_session_links<br/>(the JOIN — table+migration in backend)"]
    S["sessions"]
    GE --> DSL --> S
  end
  subgraph PROJ["Projects"]
    LE["lifecycle_event<br/>+ session link (NEW, always · verified=false)"]
  end
  C --> GE
  C --> LE
  LE -- "trailer → run_id" --> S
  LE -- "commit_sha JOIN" --> DSL
  S --> RT["runtime agent (Agents menu)"]
  DASH["Dashboard: commit → session → agent<br/>(runtime DERIVED via commit)"]
  DSL --> DASH
  LE --> DASH
```

**Gaps to close** (conceptually — the *how* + build order is in [`lineage-development.md`](./lineage-development.md)):
1. **`deploy_session_links` (first):** materialize the `commit → session → deploy` JOIN from the Deploy event's session references (FR-7). Table + migration in the control plane; core writes the rows.
2. **Core:** attestation must tolerate sessionless (Deploy) events instead of retrying "session not found".
3. **Projects:** **always** read the `OpenBox-Session` trailer (and/or join on `commit_sha`) so the project lineage links `commit → session → runtime agent`. The runtime is **derived via the commit** — no explicit runtime→project registration (shift-left stays project-unaware).

---

## 7. Design decisions

| Question | Decision |
|---|---|
| **Attribution gate** | **Always** link `commit → session`; carry `verified=false` for unverified trailers (surface "inferred"). No SL-15 ownership gate. |
| **JOIN relation** | A single materialized relation, **`deploy_session_links`** — no Projects-DB mirror, no cross-service read. |
| **Table ownership** | The `deploy_session_links` **table + migration live in the control plane** (backend / TypeORM, shared DB); **core writes** the rows. |
| **Runtime ↔ project** | **Derived via the commit** (`commit → session → agent`), shown on the dashboard. **No explicit registration** for now; shift-left stays project-unaware. |
| **Membership vs reference** | A deploy is **never a member** of its (sealed) authoring session — the link is a **reference / JOIN** (`deploy_session_links`), not an appended event. |

*Implementation of each: [`lineage-development.md`](./lineage-development.md).*
