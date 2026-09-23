# Agent evaluation — proposal

Status: proposal for review, 2026-09-24. No backend, core or frontend code yet.
Interactive mockup: published alongside as an Artifact.

## Why

ElevenLabs lets an owner define success criteria per agent; an LLM judges every
conversation against each one and returns `success | failure | unknown` with a
rationale, plus typed "data collection" values. OpenBox already stores a richer,
tamper-evident record of every session — prompts, tool inputs and outputs,
spans, verdicts, judge reasoning, Merkle proofs — but judges only goal
alignment, and that judgement never changes anything (drift is
observability-only; the per-agent alignment config is read by nothing).

Evaluation turns that record into a pass/fail answer per criterion, with
citations into the attested events, and feeds the same rule suggestions the
project security report now produces.

## Concept mapping

| ElevenLabs | OpenBox | Difference |
|---|---|---|
| Conversation | Session: `governance_events` + `spans` + policy/guardrail/age evaluations | Merkle-attested, per-event verdicts |
| Success criterion (≤30) | Evaluation criterion (≤30 per agent) | three kinds, below |
| Data collection item (≤25–40; string/boolean/integer/number) | Data collection item, same four types | values cite the event they came from |
| `success / failure / unknown` + rationale | `pass / fail / unknown` + rationale + **citations** | a verdict that cites no event is rejected |
| Post-call webhook | `session.evaluated` webhook (existing webhooks feature flag) | — |

## Criteria: three kinds

| Kind | Judged by | Example |
|---|---|---|
| **Goal** | LLM, from a natural-language prompt | "The agent never sends data externally when the operator says not to." |
| **Standard** | LLM, anchored to an entry in the pinned catalog (CWE, MITRE ATLAS, OWASP LLM — 1,162 entries) | LLM06 Excessive agency: "No data-sending tool ran without approval." |
| **Check** | Deterministic code, no LLM | "Every `sendSupportReport` call got REQUIRE_APPROVAL"; "attestation verifies"; "cost < $0.05"; "no guardrail violation" |

Checks are cheap, reproducible and run on every session; LLM criteria can be
sampled. `unknown` is a first-class result: when content capture is off, or the
events a criterion needs are absent, the judge must say so rather than guess.

## How it runs

1. A session reaches WorkflowCompleted or WorkflowFailed.
2. Core starts `SessionEvaluationWorkflow` (Temporal, beside
   `GoalAlignmentJudgeWorkflow`).
3. It builds a bounded transcript from the same reads project assurance uses
   (`sessions/:sid/logs/chronological`, spans, evaluations).
4. Checks run in code; LLM criteria go to an org-configured OpenAI-compatible
   judge endpoint (the `LLAMAFIREWALL_HOST` pattern — the backend has no LLM
   client today).
5. Every result is validated deterministically: each citation must resolve to
   an event in that session, the same rule `securityreport.Prepare` enforces.
   A result that fails validation is stored as `unknown`, never as the judge's
   claim.
6. Results are stored; `session.evaluated` fires.
7. A `fail` runs through the rule mapper from project assurance. Where OpenBox
   can enforce a fix, a rule is suggested; a person applies it; later sessions
   are re-evaluated and the pass-rate trend shows whether it closed.

Idempotent per (session, criteria version). Budget and sampling caps per agent.

## Storage and API

- Tables: `evaluation_criteria`, `data_collection_items`, `session_evaluations`,
  `session_data_values`. New tables need an ADR in openbox-backend.
- Routes: `/agent/:id/evaluation/criteria`, `/agent/:id/evaluation/data-items`,
  `/agent/:id/sessions/:sid/evaluation`, `/agent/:id/evaluation/results`.
- Webhook: `session.evaluated` with per-criterion results and data values.

## One schema, two modes

Project assurance is **offline** evaluation of a sealed pack from a sandbox run;
runtime evaluation is **online** over live sessions. Both produce the same
result shape and use the same rule mapper, so a defect found before deploy and
a failure seen in production read and resolve the same way.

## UI

| Surface | Placement | Reuses |
|---|---|---|
| Agent → **Evaluation** tab (Criteria · Data collection · Results) | `tabs[]` in `view-agent-page.tsx`, after Verify, feature-gated | sub-tab pattern from `authorize/`; `PolicyRuleDialog` for the editor |
| Session results | Verify tab, beside `SessionIntegrityCard` | `ReasoningTraceModal` row pattern; `DetailModal` opens a cited event |
| Sessions list | `SessionCard` aside + `filter-bar.tsx` | pass/fail chip and filter |
| Trend | Monitor overview | `goal-alignment-trend.tsx` pattern |
| Project **Security** report | `/projects/$projectId/security` | `findings-table.tsx`, `RequirementDetailSheet`, `policy-suggestions-section.tsx` (exists, unwired) |

Apply goes through the existing flows (`usePolicyRuleDeploy`,
`useGuardrailTemplateCreate`); Modify pre-fills `PolicyRuleDialog`. Two frontend
gaps: `CreateRuleModal` needs an `initialData` prop, and `Agent` has no
developer-vs-runtime `kind`.

## Roadmap

1. **Checks on the Verify tab** — deterministic criteria only, no LLM, no new
   service. Proves the result shape and UI.
2. **LLM criteria and data collection** — judge endpoint, sampling, budget.
3. **Rule suggestions with Apply** — the project-assurance mapper, wired to the
   existing create flows.
4. **Trends and webhooks** — pass rates over time, `session.evaluated`.

## Open questions

- Where the judge runs, and who pays for it.
- Sending transcripts to a judge is an egress of prompt content; it should follow
  the agent's content-capture setting.
- Criteria versioning: re-evaluate history on edit, or evaluate forward only.
- Known backend defects to fix before suggesting from templates: the
  `credential-read-then-egress`, `untrusted-input-then-egress` and
  `mcp-fetch-then-write` behavior templates and the `prompt-injection-markers`
  guardrail are inverted.
