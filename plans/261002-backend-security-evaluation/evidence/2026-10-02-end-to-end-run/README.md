# End-to-end run, 2026-10-02

One run of the whole workflow on the local stack, captured step by step. The model is a
local `granite4.1:3b`: it proves the mechanism, not finding quality.

| Step | Evidence |
|---|---|
| Run the demo image in the sandbox and request the evaluation | `cli-before-accept.log` (41 s wall time), `events.txt` (the session's events) |
| Organization model connector | `deck-01-connector.png` |
| Session, integrity verified, criteria failing | `deck-02-session-verify.png` |
| Report: provenance, the two findings, the suggested rule | `deck-03-*`, `deck-04-*`, `deck-05-*` |
| Accept, then the policy active | `deck-06-after-accept.png`, `deck-07-policy-active.png` |
| Run again: the send held for a person | `cli-after-accept.log`, `deck-08-approval-held.png` |
| Same checks after the rule: Fail 2 became Pass 2 | `deck-10-after-verify.png` |

`state-before.txt` is the demo agent's controls before the run. Afterwards the accepted
policy version was deactivated: no active policy, no guardrails, no behavior rules.
The dashboard screenshots (`deck-*.png`) are not committed: they show the local dev agent's
name, which embeds a hostname. They are kept beside the repos in
`evidence-2026-10-02-end-to-end-screenshots/` and used in the team deck.
