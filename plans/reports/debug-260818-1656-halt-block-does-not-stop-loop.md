# Debug: HALT/BLOCK verdicts do not stop the Claude Code loop

**Date:** 2026-08-18 · **Repo:** openbox-shift-left (read-only diagnosis; no fixes applied)
**Symptom (user report):** OpenBox returns HALT or BLOCK, Claude Code continues the agentic
loop — "not halted or blocked."

## TL;DR — three mechanisms, one of them the headline

1. **Nothing ever halts the SESSION — by construction.** The only enforcement lever the
   adapter emits is a per-call PreToolUse `permissionDecision:"deny"`. HALT and BLOCK map to
   the same thing (`hookflow.MapVerdict`, enforce.go:428-442). Claude Code's contract for
   `deny`: block that one tool call, tell the model why, keep the loop running. The model
   then routes around the deny. Claude Code's session-stop lever — top-level
   `"continue": false` (+ `stopReason`), which overrides `permissionDecision` — is never
   emitted anywhere in this repo (grep: zero hits in adapters/). Agent-runtime HALT
   semantics (GovernanceHaltError terminates the workflow) have no dev-runtime analog.
2. **Only PreToolUse ToolCall is a gate.** Every other event (SessionStarted/Ended,
   PromptSubmitted, ToolResult, TurnStarted/Completed) is observe-only: spooled, flushed,
   evaluated post-hoc. A HALT/BLOCK on those records `would_block:true` in advisories.jsonl
   and shows in the dashboard, but the action already happened — nothing exists to block.
3. **Fail-open windows.** Inline evaluation budget is 3.5s (`DefaultEvaluationTimeout`);
   miss it → `EvaluationFailOpen` → default `fail_closed:false` → the call proceeds
   ungoverned. The spooled copy is delivered later and core's verdict for it lands
   server-side only. On this machine 757/2678 current-engine gate runs (28%) failed open
   against `openbox-core.node.lat`.

**When a HALT/BLOCK does arrive inline and in budget, the deny works.** Evidence below.

## Evidence (this machine, `~/Library/Application Support/openbox/`)

Enforcement sink (`enforcements.jsonl`, 4565 rows):

| verdict | applied | source | rows |
|---|---|---|---|
| ALLOW | proceed | evaluate | 1908 |
| (none) | proceed | evaluate:fail-open | 757 |
| (none) | proceed | tier2:fail-open (pre-that decision engine) | 745 |
| ALLOW | proceed | local-bundle / tier2:evaluate (old engine) | 1142 |
| **HALT** | **deny** | evaluate | **13** |

- 13/13 inline HALTs → `applied_decision:"deny"`. Claude Code honored them — the Aug-14
  incident report documents three consecutive visibly-denied calls
  ([debug-260814-1231](debug-260814-1231-session-no-longer-active-halt.md)).
- **Zero BLOCK has ever arrived inline** on this machine. "BLOCK didn't block a gated call"
  has not actually been observed here.
- All 13 HALTs carry empty `policy_id` + zero-UUID `approval_ref`: they are the known
  **unauthored-HALT core defect** (control-plane precondition failure expressed as HALT,
  session latch), not an org policy. Documented limit: docs/getting-started.md:392,
  docs/architecture.md:194-204. Core-side fix planned, **0/5 phases executed**
  (plans/260814-2235-dev-session-unauthored-halt-fix/).

Advisory sink (`advisories.jsonl`) — verdicts that by design could not block:

| verdict | event_type | rows |
|---|---|---|
| HALT | SessionEnded / SessionStarted / PromptSubmitted | 10 / 9 / 5 |
| ALLOW | ToolCall / PromptSubmitted / SessionEnded / TurnCompleted | 31 / 22 / 5 / 1 |

Session `c5a05c62` (2026-08-17 17:18–17:52, the likely trigger of this report): 314 gated
ALLOWs + 10 gated HALT-denies + 24 HALT advisories on lifecycle/prompt events. From the
developer's chair: dashboard full of HALT, a few denied tool calls, session and loop
carrying on — exactly the reported symptom.

Posture: live config `~/.openbox/dev.json` has `enforce:true`, `findings:true`; default
`fail_closed:false`. (`~/Library/Application Support/openbox/dev.json` is a stale
pre-that decision leftover — keychain-era fields — not read; `OPENBOX_CONFIG` unset ⇒
`~/.openbox/dev.json` per devconfig.go:217-227.)

## Why each guarantee reads as "it didn't stop"

| Expectation | What actually happens |
|---|---|
| HALT stops the agent | HALT ⇒ deny of ONE call (enforce.go:133-137 maps HALT→deny, "strongest CC signal" — but `continue:false` is stronger and unused). Loop continues by Claude Code's own deny semantics. |
| BLOCK stops the tool call | It does — when the verdict arrives inline within budget on a PreToolUse gate. Never yet observed inline here; verdicts seen in the dashboard on non-gated events or after fail-open windows were post-hoc by construction. |
| A dashboard HALT/BLOCK row = something was blocked | Only rows whose event is the gated ToolCall AND whose verdict reached the gate in time. advisories.jsonl `would_block:true` rows are the honest record of "would have, could not". |

Note the deliberate product direction already on file: plan 260814-2235 (user-decided)
requires dev sessions to **never latch on HALT — a verdict applies to one call** (core-side).
Client-side session-stop on HALT is therefore an open semantics decision, not an oversight
with an obvious answer.

## How to tell which mechanism a given "it didn't stop" was

1. `grep '"verdict":"HALT"\|"verdict":"BLOCK"' ~/Library/Application\ Support/openbox/enforcements.jsonl`
   — row present with `applied_decision:"deny"` ⇒ the call WAS denied; complaint is about
   the loop continuing ⇒ mechanism 1.
2. Same verdict only in `advisories.jsonl` / dashboard, `event_type` not ToolCall ⇒
   mechanism 2 (policy fired on a non-gated event).
3. Gate row shows `fail_open:true, source:"evaluate:fail-open"` (or hook stderr
   "inline evaluation degrading") ⇒ mechanism 3 (verdict missed the 3.5s budget).
4. HALT with no `(policy: …)` suffix in the deny message ⇒ the unauthored-HALT core defect,
   not policy.

## Options (decisions, not implemented)

- **OD-1: map HALT → top-level `continue:false` + `stopReason` (+ deny)** in
  `outputcontract.go` Render / `MapVerdict` — gives HALT real halt-the-loop semantics and
  finally distinguishes it from BLOCK. Tighten-only holds (it only stops more). Needs: the
  OutputContract seam extended (Codex has no analog — its strongest is deny), and a decision
  that a dev-session HALT should kill the local loop while the core-side latch is being
  removed for the same sessions.
- **OD-2: `fail_closed:true`** for orgs that want "no verdict ⇒ no call" — closes mechanism
  3 at the documented cost (an outage blocks all gated work).
- **OD-3: policy authoring guidance** — a policy meant to prevent an action must match the
  ToolCall event (the only gated shape); verdicts on results/lifecycle/turns can only ever
  be advisory. Belongs in docs if policy packs ship.
- Orthogonal but pending: the unauthored-HALT core fix (plan 260814-2235) removes the
  false-HALT noise that currently dominates every HALT this machine has seen.

## Unresolved questions

1. Which project/session the user's report refers to — assumed `c5a05c62` (Aug 17) or the
   same class of test; no HALT/BLOCK recorded today (Aug 18).
2. Whether the user's org has any real HALT/BLOCK policy published — every HALT on this
   machine is the core precondition defect (empty policy_id), so a genuine policy-authored
   inline BLOCK/HALT deny has effectively never been exercised here outside the incident.
3. Whether `continue:false` on PreToolUse is honored identically across the CC versions the
   fleet runs (2.1.229 probed for other fields; this field not probed —
   plans/reports/probe-260813-2329-claude-code-hook-surface.md).
