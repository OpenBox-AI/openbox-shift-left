# ADR-0024 — Decision-model cascade for security evaluation

Status: Accepted (proof of mechanism). Date: 2026-10-06.

Plan: [`plans/261006-decision-model-evaluation/plan.md`](../../plans/261006-decision-model-evaluation/plan.md).
Amends [ADR-0023](ADR-0023-backend-owned-security-evaluation.md) §3 (one connector per org).

## Context

ADR-0023 runs the whole security judgement on the organization's language model.
A language model is the expensive tool for a question like "does this tool send data
out of the agent?". A **decision model** (TypeSafe System One) returns a calibrated
probability for one such question, and an uncertain answer is a clean signal to
spend a language-model call. The goal here is to prove that a decision model can do
the evaluation against a session's runtime log; it is not to beat the language
model on finding quality.

## Decision

1. **A cascade.** The decision model judges each tool activity first. Certain answers
   (p ≥ 0.8 yes, p ≤ 0.2 no) assemble findings directly. Activities with any
   uncertain answer go to the language model analyst, on those events, unchanged
   from ADR-0023. The thresholds are constants and are recorded in the report.
2. **Both models are required.** An organization configures a **decision** connector
   and a **language model** connector. A request without both is refused with
   `422 connector_required`, whose `missing` lists `decision` and/or `llm`. There is
   no single-model mode and no fallback.
3. **A failing decision model fails the evaluation.** It is configured, so the
   language model never quietly stands in for it. The evaluation ends `failed`
   with a reason and no report.
4. **Rules stay in code; the model supplies judgments.** Whether a tool ran without
   approval, and whether untrusted input came first, are decided from the stored
   verdicts and event order. The decision model answers four Noul questions
   (sends data out, changes state, instructs the agent, operator goal allows it).
   Finding prose is a template over cited facts and the raw probabilities.
5. **Same invariants.** `severity` is `unavailable`, `security_pass` is `false`,
   every citation resolves to a session event, and LLM output is parsed and
   validated by the same function whether it comes from a pass or the cascade.
6. **One protocol, two hosts.** A connector is base URL + model + optional key and
   speaks `POST {base}/v1/systemone`. Production uses TypeSafe's hosted System One
   (`jev-latest`, Bearer key). Local development uses Ollama's `tev1:0.8b`, which
   serves the same request and response shape. The key is sealed and write-only
   like the language model key; the URL rules are the same.
7. **LLM integration suggestions.** After the cascade, the language model picks from
   a fixed six-action menu (capture content, attest sessions, raise the event
   budget, add an effect receipt, mark untrusted inputs, require approval in the
   project) from session evidence only. Each suggestion is validated against the
   gaps and observed tools. A failure here never fails the report.
8. **Per organization, not defaults yet.** Each org configures its own models. A
   platform default for each is a later decision.

### Data changes (no new table)

`llm_connectors` gains `kind` (`llm` | `decision`) and uniqueness becomes
(organization, kind); the API key column is nullable for a keyless local decision
model. Evaluations record `model_id` as `decision+llm` and `evaluator_version`
`cascade/1`. The report gains `analyst.kind`, `stages`, per-finding `decided_by`
and `judgments`, and `integration_suggestions`.

## Evidence (2026-10-06, local stack)

- Real `tev1:0.8b` on Ollama, `granite4.1:3b` as the language model, the demo
  session: the decision model answered 0.48 / 0.43 / 0.47 / 0.38 (all uncertain), the
  activity escalated, the language model's two findings validated, and two
  integration suggestions came back.
- A decision connector pointing at a dead port: the evaluation failed with "the
  decision model could not be reached", no report, no fallback.
- Policies, guardrails and behavior rules for the agent were identical before and
  after.
- Review found that the first cascade accepted malformed or control-bearing language
  model output because it read only `result` and `issues`; it now parses through the
  same validator as the single-model path.

## What this weakens

- **Hosted System One is a second third-party egress** of session content (a bounded,
  credential-redacted state per tool activity). BYOK covers the key; the privacy
  doc says so.
- A 0.8B model is uncertain about almost everything, so in the local proof the
  language model decides every finding. That shows the cascade, not decision-model
  accuracy; calibration in this domain is unmeasured and reports say probabilities
  are not facts.
- Per-organization thresholds, Choice questions and run-record based sandbox advice
  are not built.
