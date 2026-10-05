---
title: "Backend-owned security evaluation"
description: "Developers run the project locally through the OpenBox sandbox; evaluation, report and rule suggestions move into the OpenBox backend, and the local analysis lane is deleted"
status: active
priority: P0
created: 2026-10-02
supersedes:
  - plans/260825-1623-lean-openshell-project-assurance (analysis, finalize, pack and skill phases; execution phase is kept)
amends:
  - docs/adr/ADR-0020-project-assurance-native-sandbox.md
  - docs/adr/ADR-0022-agent-evaluation-and-published-reports.md (§4: the backend now produces the report)
---

# Backend-owned security evaluation

## Intent

```text
developer machine                         OpenBox
-----------------                         -------
openbox project evaluate
  run the project in the sandbox  ──SDK──▶ Core ──▶ sessions / events / spans   (already true)
  ask for an evaluation of run_id ───────▶ backend
                                            wait for terminal session
                                            read evidence from its own tables
                                            analyst (Mastra agent, org's BYOK model)
                                            deterministic validation + rule mapping
                                            report, stored
  print link / summary            ◀─────── dashboard: report, Accept
```

Nothing but the run itself and one request leaves the developer's machine. The
SDK already delivers every behavior event to Core; the backend already stores
it. No pack, no upload of evidence, no second copy.

## Decisions taken (2026-10-02)

| # | Decision |
|---|---|
| 1 | **BYOK.** An org configures its own model connector; OpenBox pays for no inference. No connector → the request is refused with `connector_required`. This is a precondition, not a degraded mode. |
| 2 | **Backend, built on Mastra.** The analyst is a Mastra agent embedded as a library in the NestJS backend. |
| 3 | **No sandbox upload.** The sandbox/SDK path is unchanged. The open question was who triggers the backend evaluation and when — see *Trigger*. |
| 4 | **Delete, don't fall back.** The local skill, finalize, packs and everything only they used are removed. No flag, no legacy reader, no migration code — shift-left has no users yet. |
| 5 | **Evaluation is explicit.** No automatic or sampled evaluation, now or as a planned follow-up. |
| 6 | **Minimum permissions.** The evaluate path loses every read it only needed for local crawling; see *Permissions*. |
| 7 | **No key/signing work in the sandbox.** Runs stay bearer-observed (`signing_required=false`). Attribution is reported as absent, not engineered around. |

## Trigger — who decides, and when

The backend decides *whether and how* to evaluate; the requester decides *which
session*. There is one evaluator over any terminal session; a sandbox run is
just a session whose `run_id` is the evaluation id Core already records.

| Requester | When | Status |
|---|---|---|
| `openbox project evaluate` | as its last step, for the run it just made | in scope |
| Dashboard "Evaluate" on a session | on demand | in scope |
| ~~Automatic on session end~~ | — | **excluded by decision.** Evaluation is always an explicit request; the backend never starts one itself. |

Timing: core stores `WorkflowCompleted` *after* marking the session terminal
(ADR-0022 "Why not core"). The backend therefore enqueues a BullMQ job (already a
dependency) that retries until the terminal event is present or a bound
expires, then fails the evaluation visibly. Idempotent per
(session, evaluator version, connector model).

## Provenance, honestly

The pack seals are gone. What replaces them:

- **Evidence** is Core's stored, Merkle-attested events. The report records an
  *evidence snapshot digest* (ordered event ids plus their stored hashes).
- **The analyst** is recorded, not self-labelled: connector model id, prompt
  digest, standards-catalog version, evaluator version.
- **Citations** must resolve to an event in that session, or the issue is
  dropped to `unknown` (same rule `securityreport.Prepare` enforces today).
- **Not claimed:** sandbox-side facts (image, policy hash, isolation) are no
  longer evidence inputs. Signed attribution is absent and the report says so.

## Backend design (openbox-backend)

Builds on `src/modules/evaluation/` (criteria, `checks.ts`, `security_reports`).

| Piece | Notes |
|---|---|
| `llm_connectors` (org-scoped) | base URL, model id, optional headers, API key encrypted with `KmsService`. **Never returned by any read** (the `token` precedent, ADR-0022 §6), never logged. New table → ADR. Pattern to mirror: webhook secrets — verify in Phase 0. |
| `POST /agent/:id/security-evaluations` `{session_id \| run_id}` | creates a row and enqueues the job; returns the id. Gated by `agent_evaluation`. |
| `GET …/security-evaluations/:id` | status `queued → waiting_for_session → analyzing → complete \| failed` plus the report. |
| Evidence reader | reads `governance_events`, `spans`, policy/guardrail/AGE evaluations directly. Replaces the CLI's HTTP crawl, pagination and stability rechecks. |
| Analyst | Mastra `Agent` with `model: { url, id, apiKey, headers }` from the connector and `structuredOutput` as a Zod port of `candidate.schema.json`. A Mastra workflow chains `loadEvidence → analyze → validate → map → persist`. Library use only: no Mastra server, storage, memory or tracing. |
| Validator + mapper | port of `securityreport.Prepare` and `catalog.go`: forbidden-key check, citation resolution, `*_assertion` relabelling, only-enforceable-rules, POST-a-new-version delivery. Posture comes from the backend's own policy tables — no GET snapshot, no control token. |
| Standards | CWE 4.20 / ATLAS 2026.09 / OWASP LLM 2025 corpora (1,162 entries) move server-side and version independently of the CLI. |
| `security_reports` | reshaped: report is produced by the backend; `PublishSecurityReportDto` and its client-asserted `manifest.pack_digest` are removed. |

Invariants carried over unchanged: `severity` is the literal `unavailable`;
`security_pass` is `false`; the model finds and deterministic code prescribes;
`inconclusive` needs a missing *required* authority; session content is data,
never instructions; the model cannot write a control.

New guardrails: transcript size cap, per-org rate and budget cap, egress to the
connector only (and only per the agent's `content_capture`).

## Delete list (CLI repo: openbox-shift-left)

Counts include tests, from the import graph on 2026-10-02 — re-measure in
Phase 3.

| Remove | Lines | Why it can go |
|---|---:|---|
| `assurance/securityreport` | 2,523 | moves to backend |
| `assurance/observation` | 1,791 | HTTP crawl and pack writer; backend reads its own tables |
| `assurance/targetposture` | 1,048 | backend reads its own policies |
| `securityskill/` + skill install in `openbox init` + `contracts/project-security-analysis` | ~700 + bundle | analyst moves to backend |
| `assurance/report`, `assurance/evidence` | 2,634 | historical audit-pack v1 reader/renderer; only `project report` uses it |
| `assurance/inspect`, `model`, `sdkdesc`, `snapshot` | 8,986 | only `project inspect` uses them |
| `assurance/runfs`, `assurance/artifact`, `assurance/safety` | 6,505 | pack file layer; no pack remains |
| `cmd`: `project_inspect*`, `project_finalize`, `project_verify`, `project_report`, `project propose` | — | commands with nothing left to run |
| `contracts/project-assurance`, `project-observation`, `project-security-report` | — | schemas for deleted packs |
| `evaluate/effect_relay.go`, `model_relay.go`, Ollama preflight, `OPENBOX_SAFE_SINK_URL`, `OPENBOX_SANDBOX_MODEL_*` directives | ~400 | receipts that existed only to corroborate packs (decided)  |
| `testbed/…/mastra-security-demo`: `launch-claude.zsh`, `publish-report.zsh` | — | replaced by one run command |
| `OPENBOX_CONTROL_TOKEN` as a read-everything credential on the evaluate path; `observation.RequiredPermissions`; the exact-set check in `targetposture`/`securityreport`; the matching reconcile in `local-stack/scripts/bootstrap.sh` | — | replaced by the minimal set below |

Roughly 23,500 lines of Go (tests included) go away. **Kept:**
`evaluate/` (preflight, project env, sandbox policy, sandbox run, Core relay),
`sandboxclient/`, `.env.sandbox`. `project evaluate` loses `--output`: results
live at the backend and a failed run prints its error and exit code.

Other repos: **openbox-sandbox** keeps ProjectRun v2 as is; the unwired
`GovernanceClient`/`GovernedDispatcher` in `src/dispatcher/mod.rs` is a
removal candidate (verify nothing depends on it). Planned sandbox work on
signing and structured evidence is dropped. **openbox-backend** removes
`publishReport` and its DTO.

## Delivery ledger

Statuses: `planned`, `in_progress`, `implemented`, `verified`, `blocked`.
`verified` needs a live run on local-stack, not unit tests. One task
`in_progress` at a time. Deletion (P3) starts only after P2 is verified, and
lands in the same release as the cut-over — no period with both paths.

### Phase 0 — Decide and contract

| ID | Status | Work |
|---|---|---|
| BE-00-01 | verified | `docs/adr/ADR-0023-backend-owned-security-evaluation.md` written and indexed; supersedes ADR-0022 §4. |
| BE-00-02 | verified | The new tables (`llm_connectors`, `security_evaluations`, reshaped `security_reports`) are decided in ADR-0023, following ADR-0022's precedent of keeping backend-table decisions in this repo's ADR directory. |
| BE-00-03 | verified | 2026-10-02, scratch harness: `@mastra/core` 1.73.0 loads via CommonJS `require`, honours per-request `{url,id,apiKey,headers}`, returns `structuredOutput`, and connects only to the configured endpoint — on Node 22.23 and `node:20-alpine`. Caveat: it declares `engines.node >=22.13` while the backend image is Node 20; a bump to 22 is recommended and left to the owner. Verify: `@mastra/core` installs and runs inside the backend (Node/TS/Nest versions), makes no unconfigured network call, and honours `{url,id,apiKey}` per request. |
| BE-00-04 | verified | Permission named `evaluate:agent_security`; `openbox auth` uses `POST /agent/create`, `GET /agent/list`, `PUT /agent/:id`, rotate-api-key and identity/rotate, so `create/read/update:agent` stay. Finding: `KmsService` is AWS-only and unconfigured locally, so BE-01-01 adds a `local` provider (AES-256-GCM from `OPENBOX_LOCAL_KMS_SECRET`) and fails closed when encryption is unavailable. Verify: KMS usage pattern for secrets; name the one new evaluation permission and confirm `openbox auth` really needs `create/read/update:agent` (see *Permissions*). |
| BE-00-05 | verified | The relay stays: the guest reaches only `host.openshell.internal` (`192.168.127.254/32`), so a loopback Core needs it; its effect and model endpoints go. `evidence-authority.md` lists effect and model receipts only under *Strengthens*, never *Required*, so no issue class needs them. Verify: does the Core relay stay necessary, or can the sandbox policy reach Core directly with `credential_binding`? Confirm no issue class in the ported evidence-authority table requires an effect receipt (the relays are cut by decision). |

### Phase 1 — Backend foundation

| ID | Status | Work |
|---|---|---|
| BE-01-01 | verified | `llm_connectors` (migration 1782200000000), CRUD on `read:org`/`write:org`, KMS encryption with a new `local` AES-256-GCM provider (local-stack now passes `OPENBOX_LOCAL_KMS_SECRET` to the backend), fail-closed when encryption is unavailable, write-only key, URL rules (https, no internal hosts, no credentials; `LLM_CONNECTOR_ALLOW_HTTP` opt-in for local stack). Live 2026-10-02: PUT → GET returns no key; the table holds ciphertext (`plaintext_present = f`). **Dropped: a separate connector test call** — a bad key surfaces as a failed evaluation with a safe reason, so it added nothing. 19 unit tests. |
| BE-01-02 | verified | `POST/GET /agent/:id/security-evaluations` on the new `evaluate:agent_security` permission; BullMQ job retries (120 × 5s) until the terminal event is stored, then `giveUp` closes it. Live: a session that never ended stays `waiting_for_session`; a repeated request returns the same evaluation; a key without the permission gets 403; no connector gets 422 `connector_required`. The request names the session by `session_id` or `run_id` (exactly one; ambiguous run ids are refused). |
| BE-01-03 | verified | `security/evidence.ts` reads `governance_events`, spans, Merkle leaves and the attestation directly; bounded (120k chars), credential-shaped text redacted before the analyst sees it, snapshot digest over event ids + stored leaf hashes. 9 unit tests; exercised live. |

### Phase 2 — Backend analyst

| ID | Status | Work |
|---|---|---|
| BE-02-01 | verified | Mastra `Agent` per request on the org connector, `structuredOutput` Zod schema, `searchStandards` tool over the CWE/ATLAS corpora. Mastra sits behind a seam (`AnalystRunner`) because Jest cannot load its ESM-only dependencies even with `--experimental-vm-modules` (tried, abandoned); the boundary is checked by `yarn check:mastra` against a stub endpoint (url, key, headers, model name, `json_schema`, single call). Live with `granite4.1:3b`: first run mis-labelled a citation, so evidence became a plain list of ids and ids are extracted tolerantly then resolved strictly; analyst version bumped to `security-analyst/2`. |
| BE-02-02 | verified | `security/validate.ts` and `security/recommend.ts`: severity never from the model, `security_pass` false, citations resolved against the session, standards versions filled server-side, forbidden-field and credential scans, inconclusive needs a limiting gap, one rejected issue does not sink the rest and is reported under `rejected_candidates`, the approval rule is built from the cited action and delivered as a new policy version. Ported as behaviour tests (the Go fixtures were pack-shaped, so literal golden files could not carry over): 22 + 7 tests. |
| BE-02-03 | verified | 1,162-entry corpora and the six-entry recommendation catalog live in the backend (`security/data`); the report records the standards version and the catalog version and digest. The inverted behavior/guardrail templates are avoided, not used: no catalog entry templates from them, and the two unenforceable entries (prompt injection, effect sequences) keep their "Not enforceable today" limitation and write no rule. |
| BE-02-04 | verified | **Live 2026-10-02, whole chain including the dashboard.** (1) `run-demo.zsh` (exit 0, twice): `openbox project evaluate` with a key holding exactly `create/read/update:agent` + `evaluate:agent_security` ran the demo image in the sandbox from a fresh run; Core and the backend stored the session; the backend evaluated it on the org connector (local `granite4.1:3b`); the CLI printed `issues: 2 (Excessive Agency, Prompt Injection)` and `suggested rule: human-authorization on sendSupportReport`. (2) **Zero control writes by the evaluator:** the agent's controls before and after six CLI runs were policies 1 (inactive), guardrails 0, behavior rules 0 — unchanged. (3) **Dashboard click-through** (signed in by the owner past the reCAPTCHA, Playwright): session page → `Evaluate security` created evaluation `a8080427…` (analyst v3) and polled it to completion → the session's Evaluation section linked the new report → the report page showed the suggested rule → `Accept` sent `POST /agent/{id}/policies` → **201**, new version `63a28747` active and current, Rego naming `sendSupportReport`, exactly one rule (the suggested one, as its delivery note says). The dashboard's own dedupe also worked: while the current policy already held the rule it said "This rule is already in the current policy" and withheld Accept. Afterwards the version was deactivated through the API (0 active policies, 0 guardrails, 0 behavior rules; the demo org's 14 rules untouched). Found and fixed on the way: (a) the backend Dockerfile fetched OPA `latest`, so rebuilds upgraded 1.19.0 to 1.21.1, which rejects the generated behavior-rule Rego and failed every policy deploy — OPA pinned to 1.19.0; (b) a signed-in dashboard session keeps the permissions of the token it was issued, so a session older than the role sync got 403 on `evaluate:agent_security` until the owner signed in again; (c) the Evaluate panel kept the evaluation only in component state and fell back to a bare button after a re-render or reload — it now resumes the session's latest evaluation from the server (4 new tests, verified in the browser). |

### Phase 3 — Cut over and delete

| ID | Status | Work |
|---|---|---|
| BE-03-01 | verified | `project evaluate` runs the sandbox, then POSTs `{run_id}` and prints `security evaluation requested: <id>`; `--wait` polls and prints a summary; no `--output`. Agent-written, independently re-verified: `gofmt` clean, `go build`, `go vet`, all packages green under `-race`, evaluate no longer imports observation/runfs/artifact, no references to the removed relays. 28 new tests; token never in an error; redirects not followed. Live run above. The demo project lost its effect-sink call (it is now a no-egress stub) and the model comes only from the gateway's `inference.local` route; the `mastra-conformance` project was folded into `mastra-security-demo` and `run-demo.zsh` replaces the prepare/launch/publish scripts (run live, exit 0). |
| BE-03-02 | verified | Deleted: `securityreport`, `observation`, `targetposture`, `report`, `evidence`, `inspect`, `model`, `sdkdesc`, `snapshot`, `runfs`, `artifact`, `safety`, `securityskill` (+ bundles), `assurance/testdata`, four `contracts/project-*` dirs, the `inspect|finalize|verify|report|propose` commands and their tests, the skill install in `openbox init`, `mastra-conformance`, `security-analysis` evals, the demo's Claude/publish scripts and 383-line runbook. The import scan found `main.go` as the only live reference and it was cut. `project_test.go` now pins that every removed route fails with usage and creates nothing; the init tests now assert **no** skill is installed. `dc/security-evaluate.md` is kept (historical plans link to it) and marked superseded. |
| BE-03-05 | verified | `sharedControlPermissionSet` is `create:agent`, `read:agent`, `update:agent`, `evaluate:agent_security`; tests written first (failed against the old set) cover the exact set, the missing-permission message naming `evaluate:agent_security`, and refusal of each of the five old read permissions. Local-stack: the bootstrap key is deliberately broader (testbed phases need policy and session permissions) and only gains `evaluate:agent_security`; bootstrap re-run granted it to the retained key. |
| BE-03-03 | verified | Backend `publishReport`, its DTO and POST route are gone (grep clean) and replaced by backend-produced reports. **Not removed, by decision:** the sandbox's unwired `GovernanceClient`/`GovernedDispatcher` (`src/dispatcher/mod.rs`, 923 lines). Nothing outside the crate's own exports and tests uses it, but it is a tested, publicly exported part of that crate and unrelated to this change, so removing it is the sandbox owner's call. Recorded as an observation. |
| BE-03-04 | verified | Run 2026-10-02: `go build ./...`, `go vet ./...`, `go test ./... -race -count=1` green in all 11 `go.work` modules (cli 10 packages ok); cross-compiles linux/arm64, windows/amd64, linux/amd64 ok; orphan scan: every remaining `cli/internal` package has a live importer; `staticcheck -checks U1000` clean after removing three leftovers, apart from the existing `devinit.nsOrLocal` (untouched file, recorded, not fixed). Backend: 104 suites / 1,926 tests green, production files lint-clean, production `tsc` clean, `yarn check:mastra` ok. Roughly 23,500 lines of Go (tests included) removed. |

### Phase 4 — Dashboard, docs, testbed

| ID | Status | Work |
|---|---|---|
| BE-04-01 | verified | Agent-built, then verified here: 258 files / 2,680 tests pass, lint 0 errors, build ok, container rebuilt. **Seen in the browser (2026-10-02):** the Model connector card on organization settings (URL and model shown, key field empty with "Key is set — leave blank to keep it", Save and Remove for an Admin); `Evaluate security` on a session; the report with its suggested rule; `Accept` deploying it (201). The live check found the panel losing its state on re-render, fixed by resuming from the server (see BE-02-04). One addition beyond the brief: an optional headers field, because the backend treats omitted headers as `{}` and saving would silently wipe them. |
| BE-04-02 | verified | `docs/project-assurance.md` rewritten, `docs/architecture.md`, `docs/data-and-privacy.md` (new egress to the org's own model, redaction limits) and `docs/getting-started.md` (four-permission key) updated, the CLAUDE.md project-assurance section replaced (143 lines to about 40), ADR-0023 extended with what the build changed, `dc/security-evaluate.md` marked superseded. The testbed path is `testbed/project-assurance/mastra-security-demo/run-demo.zsh`, run live. |

## Risks

- **New egress:** session content goes from the backend to the org's own model
  endpoint. It follows `content_capture`; `docs/data-and-privacy.md` must say so.
- **Secrets in transcripts** reach the analyst. Core-side redaction is not wired
  (CLAUDE.md known limit); the report must state it.
- **Trust moves, it does not vanish.** Backend-computed results are only as good
  as the events the SDK sent; blind seams stay coverage gaps.
- **Nothing here has run.** Phase 2's live proof is the first evidence.

## Permissions

Today the shared org key must hold exactly eight permissions, five of which
exist only so the CLI can crawl sessions and snapshot controls itself.

| Permission | Today | After |
|---|---|---|
| `create:agent`, `update:agent` | register / rotate the project agent in `openbox auth` | kept only if Phase 0 confirms `auth` needs them; they are unrelated to evaluation |
| `read:agent` | agent lookup | kept for the same reason |
| `read:agent_session`, `read:agent_log` | crawl the session | **removed** — backend reads its own tables |
| `read:agent_guardrail`, `read:agent_policy`, `read:agent_behavior_rule` | posture snapshot | **removed** — backend reads its own tables |
| *new:* one evaluation permission | — | request an evaluation and read its result; nothing else |

Ceiling after the change: four permissions, and the evaluation path itself
uses only the new one. The backend routes use that permission instead of
`update:agent` / `read:agent_session`, so a key that can request evaluations
cannot read raw sessions. No policy, guardrail or API-key write is ever granted.

## Decisions resolved

| ID | Decision |
|---|---|
| OD1 | Evaluation is explicit only. No automatic or sampled trigger. |
| OD2 | Permission set shrunk to the minimum above. |
| OD3 | Effect and model relays cut; reports rest on SDK-observed behavior and say so. |
| OD4 | No cleanup or migration code; delete and move on. |
