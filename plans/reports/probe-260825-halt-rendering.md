# Probe A: how a gateway refusal renders in Claude Code

Status: **TEMPLATE — NOT RUN.** Fill from a real run per
[the runbook](../260825-0027-openbox-gateway-full-capture/probes/RUNBOOK.md).
Delete this line when it holds real observations.

Date: — · Claude Code version: —
Gates: plan `260825-0027` phase 03 step 2; blocks phase 06 (and shapes phase 04).

## The question

A gateway that refuses a model call must produce a refusal the client treats as a
**policy decision** — not as a transport error it retries around, and not as a
capability it disables for the rest of the session. The last branch is the one
that must not be picked: Claude Code matches on upstream error wording, and a
shape that trips capability-rejection would silently degrade every later turn.

## Candidates and observed client behaviour

| Status | Body shape | Requests per prompt | What the user saw | Session continued? | Exit code |
|---|---|---|---|---|---|
| 403 | Anthropic error envelope | — | — | — | — |
| 403 | plain text | — | — | — | — |
| 400 | Anthropic error envelope | — | — | — | — |
| 403 | empty | — | — | — | — |

`>1` in "requests per prompt" means the client retried — disqualifying.

## Chosen shape

**—** (status + body, verbatim)

Why this one, and what it was chosen over: —

## If nothing qualified

Descope branch, pre-decided in the plan's risk table: phase 06 becomes
observe-only, and prevention stays in the hooks. Record which failure each
candidate showed rather than declaring the probe inconclusive.

## Unresolved questions

- (list anything the run could not settle)
