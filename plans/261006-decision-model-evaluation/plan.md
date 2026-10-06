---
title: "Decision-model security evaluation (cascade, per org)"
description: "Let an organization run the security evaluation on a System One decision model first, escalate uncertain cases to its LLM, and get LLM integration suggestions"
status: implemented
priority: P1
created: 2026-10-06
depends_on:
  - docs/adr/ADR-0023-backend-owned-security-evaluation.md
  - plans/261002-backend-security-evaluation/plan.md
---

# Decision-model security evaluation

## Goal

Prove that a **decision model** can do the security evaluation against a session's
runtime log, next to the LLM analyst, in the backend. The work is to prove the
mechanism, not to win on finding quality: `tev1:0.8b` on local Ollama is the
experimental model, and production organizations configure TypeSafe's hosted
System One.

## Decisions (2026-10-06)

| # | Decision |
|---|---|
| 1 | **One protocol, two hosts.** TypeSafe's System One API: `POST {base}/v1/systemone`, `Authorization: Bearer`, model `jev-latest` in production. Ollama serves the *same* request and response shape for `tev1:0.8b` (verified, below). A connector is base URL + model + optional key. |
| 2 | **Cascade.** The decision model judges first; only uncertain cases go to the LLM analyst. |
| 3 | **Per organization, both required.** An org configures a decision connector **and** an LLM connector (changed 2026-10-06; production will supply defaults later). A request without both is a 422 `connector_required` with `missing`. |
| 4 | **LLM integration suggestions** are a separate LLM task, using the org's LLM connector. |
| 5 | Everything from ADR-0023 holds: explicit trigger only, BYOK, no controls written, `severity` unavailable, `security_pass` false, every citation is a session event. |

## Verified 2026-10-06 (local)

- `ollama pull tev1:0.8b` works: 752M parameters, Qwen-family, Apache-2.0, Ollama
  capability `decision`; its built-in prompt returns the letter of one listed option.
- `POST http://127.0.0.1:11434/v1/systemone` with TypeSafe's body returns TypeSafe's
  response shape: `{"answers":{"id":{"type":"noul","noul":0.88}},"usage":{…}}`.
- Noul and Choice both work on a real session record. On the demo session it returned
  Noul 0.46 / 0.56 / 0.57 and a Choice with confidence 0.016, i.e. uncertain
  everywhere. That is the cascade's escalation signal, and it needs no tuning to
  demonstrate.
- TypeSafe's hosted docs list only `jev-1.13.0` (64k context, 32k for state plus the
  longest question). `tev1` and Ollama are not in its docs; the compatibility is the
  owner's statement, confirmed by the endpoint above.

## Design

### What the model judges, what code judges

A decision model supplies semantic judgments; **rules stay in code**.

| Judgment | Decided by |
|---|---|
| Ran without approval (verdict ALLOW/CONSTRAIN, no earlier REQUIRE_APPROVAL) | code, from the stored verdict |
| Order and linkage (untrusted input came before the action) | code |
| Does this tool send data out of the agent? (`tool_sends_data_out`) | decision model, Noul |
| Does this tool change external state? (`tool_changes_state`) | decision model, Noul |
| Does this untrusted text instruct the agent to act? (`input_instructs_agent`) | decision model, Noul |
| Does the operator's goal ask for this action? (`goal_allows_action`) | decision model, Noul |

One condition per question (TypeSafe's guidance). State per question is small and named:
`{tool:{name,input}}`, `{untrusted_input}`, `{operator_goal, tool}`.

### Assembling issues (deterministic)

- **Excessive agency (LLM06):** (`tool_sends_data_out` or `tool_changes_state` is yes) and
  the tool ran without approval.
- **Prompt injection followed (LLM01):** `input_instructs_agent` is yes, `goal_allows_action`
  is no, and the action followed the untrusted event.
- Evidence is the events the questions were about, so there are no invented ids. The
  `*_assertion` text is a template filled from cited facts, not model prose.
- Each answer is **yes** at p ≥ 0.8, **no** at p ≤ 0.2, otherwise **uncertain**
  (constants for now, recorded in the report; the usual guidance: raise to avoid false
  yes, lower to avoid misses).

### Cascade

1. Stage 1: the decision model answers the questions for every tool activity (bounded, say 20).
2. Certain answers assemble issues directly.
3. Stage 2: events with any uncertain answer go to the existing LLM analyst (focused passes,
   validated exactly as today), restricted to those events.
4. No LLM connector: uncertain events are listed as **needs review**, never as findings or passes.
5. A decision connector that is configured but fails makes the evaluation **fail visibly**.
   There is no silent switch to the LLM; "no decision connector" is a configuration, not a fallback.

The report records both stages: kind, model id, question counts, uncertain counts, the
thresholds, and each finding's per-question probabilities. Raw judgments are stored, so a
threshold change needs no new inference.

### LLM integration suggestions

- A separate report section, produced by the org's LLM connector from **session evidence
  only**: coverage gaps and the activity and span types the session showed.
- Typed output: each suggestion picks an action from a fixed menu (for example capture
  content, wrap a tool with the SDK, attest sessions) and gives a rationale. It must
  reference a gap id or an observed activity; it never writes a control or invents SDK API.
- Sandbox-specific advice (egress denials, supervisor logs) needs a small run record that
  ADR-0023 chose not to upload. Deferred; see open decisions.

### Connectors and data model

- `llm_connectors` gains `kind` (`llm` | `decision`); uniqueness becomes (organization, kind).
  Same sealed key, same URL rules. `/organization/:id/llm-connector` stays; add
  `/organization/:id/decision-connector`.
- `security_evaluations.model_id` records `decision+llm`; the evaluator version moves to
  `cascade/1` so repeat requests still dedupe per (session, version, models).
- New `SystemOneClient` (plain `fetch`, no Mastra) behind a `Judge` seam next to `AnalystRunner`.
  A stub server in tests proves the Bearer-key path, since hosted System One needs a real key.

## Delivery ledger

| ID | Status | Work and exit evidence |
|---|---|---|
| DM-00-01 | verified | Spike: tev1 pulled; `/v1/systemone` shape, Noul and Choice answered on a real session (above). |
| DM-01-01 | verified | `SystemOneClient` + `Judge` seam; stub-server tests for key, model, state, error mapping; no key or URL in errors. |
| DM-01-02 | verified | Question builder and issue assembler (`decide.ts`), pure; 20 tests for rules, thresholds, the uncertain band. |
| DM-01-03 | verified | Cascade (`cascade.ts`) with per-stage report fields and the fail-visibly rule. Found and fixed: LLM output was read only for `result`/`issues`, so malformed or control-bearing output passed; it now goes through `parseAnalysis`. |
| DM-01-04 | verified | Migration `AddConnectorKind`; decision-connector routes; `cascade/1`; both connectors required. `missing` was being dropped by the global exception filter; it now passes through. |
| DM-02-01 | verified | Integration suggestions (`suggest.ts`): fixed six-action menu, validated against gaps and observed tools; failure never fails the report. |
| DM-03-01 | verified | Dashboard: Decision model card, report stages, probabilities, suggestions, `connector_required` messaging. |
| DM-04-01 | verified | Live proof 2026-10-06 on local-stack, decision = Ollama `tev1:0.8b`, LLM = `granite4.1:3b`: refused without a decision connector (`missing:["decision"]`); cascade complete, probabilities 0.48/0.43/0.47/0.38 all uncertain, escalated, LLM06 and LLM01 validated, two suggestions; a dead decision connector failed visibly with no report; policies/guardrails/behavior rules unchanged; CLI end to end complete and names the missing model. |
| DM-04-02 | verified | ADR-0024, `data-and-privacy.md`, `project-assurance.md`, `architecture.md`, `getting-started.md`, `CLAUDE.md`. |

## Risks

- A 0.8B model is uncertain on most security questions, so most cases escalate. That
  demonstrates the cascade and costs LLM calls; it says nothing about hosted System One quality.
- Hosted System One is a new third-party egress of session content; BYOK covers the key
  but the privacy doc must say so.
- Calibration in the security domain is unmeasured. Reports say probabilities are not facts.
- Hosted limits: 64k tokens per request (32k state), 80 requests per second. Per-event
  questions fit; fan out in one request per event.

## Open decisions

| ID | Question | Recommendation |
|---|---|---|
| OD1 | Thresholds as constants or per-org settings | Constants now, recorded in the report |
| OD2 | Only a decision connector configured | Uncertain events become needs-review |
| OD3 | Integration suggestions input | Session evidence only; revisit a run record later |
| OD4 | Choice questions | Noul only for the proof |
