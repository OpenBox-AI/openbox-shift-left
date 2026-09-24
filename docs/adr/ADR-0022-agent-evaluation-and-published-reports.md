# ADR-0022 — Agent evaluation and published security reports

Status: Accepted for local-stack. Date: 2026-09-24.

## Context

A project security report ends in defects and a suggested OpenBox rule, but it
lived only on the developer's disk. Nobody could see it in the dashboard, apply
the rule there, or check afterwards that the defect had closed. OpenBox also
has no way to judge a runtime session against what an agent is supposed to do.
ElevenLabs offers per-agent success criteria with pass/fail/unknown per
conversation; OpenBox already stores a richer, attested record of every session
(`plans/260924-agent-evaluation/proposal.md`).

## Decision

1. **Two new backend tables**, created by one migration
   (`openbox-backend/src/migrations/1782000000000-create-evaluation-tables.ts`):
   - `evaluation_criteria`: per-agent deterministic checks, each with a type,
     params and an optional standard tag;
   - `security_reports`: sealed report packs, one row per (agent, pack digest).
     `evaluation_id` (added by `1782100000000-link-security-reports-to-runs.ts`)
     is the sandboxed run the pack observed. Core records that same id as the
     session's `run_id`, so a session's evaluation returns `report_id`, and the
     Results table and the Verify card link to the report.
2. **Results are computed on read and never stored.** Four check types read
   only what already exists: `governance_events` verdicts, `decided_at` and
   `metadata.openbox_assurance.input_trust`; `session_attestations`; and
   `age_evaluations`. The four types are `approval_required`,
   `untrusted_input_not_acted_on`, `attested` and `no_goal_drift`. Every
   citation is a row the query read, so it resolves to an event in that
   session. A check with nothing to judge returns `unknown`, never `pass`.
3. **Routes**, all gated by the `agent_evaluation` feature flag and scoped by
   the global `TeamAccessGuard` on `:agentId`:
   - `/agent/:agentId/evaluation/criteria` (CRUD);
   - `/agent/:agentId/evaluation/results`;
   - `/agent/:agentId/sessions/:sessionId/evaluation`;
   - `/agent/:agentId/security-reports`.
4. **The backend does not verify seals.** A report is published only by
   `publish-report.zsh`, after `openbox project verify` passes. The backend
   checks only the report schema, the digest shape, and the pack's own
   `observation/run.json`: it must name this agent, and its evaluation id is
   the run link.
5. **Accept uses the existing versioned-policy flow.** A policy change is a new
   version, and POST makes it the agent's only active policy. So the dashboard's
   Accept builds the new version from the current policy's rules plus the
   suggested one (`useAddPolicyRule`, reusing `buildPolicyDeployPayload`). The
   report's suggested delivery is always POST; the earlier "PUT-merge" advice
   was wrong.
6. **Agent reads no longer return `token`.** It is the SHA-256 of the agent's
   API key: credential-derived, so target posture refuses to seal a report that
   contains it. The plaintext key is still returned once, at create and rotate.
7. **Policy update names its entity.** `PolicyService.updatePolicy` saved the
   plain object `getPolicy` returns, and TypeORM refused it, so deactivating a
   policy in the dashboard failed with a 500. The demo's reset step needs it.

## Why not core

Approvals are decided in the backend, often after a session ends, and a
decision rewrites the verdict (approve → ALLOW). A result frozen when the
session finishes would go stale, and `WorkflowCompleted` is stored after the
session is marked terminal. Reading on demand needs no new core table, no Bob
regeneration and no workflow, and it is always current.

## What this weakens or strengthens

- **Strengthens:** a defect found before deploy is visible, and fixable, where
  the runtime is governed. The same check then shows whether it closed.
- **Weakens nothing in the lane.** Evaluation still writes no control.
  Publishing a report is not a control, and a rule is applied only when a
  person clicks Accept.

## Deferred

- LLM-judged goal and standard criteria.
- The `session.evaluated` webhook.
- Trend charts.
- Results on the sessions list.
- Persisting Reject decisions.
