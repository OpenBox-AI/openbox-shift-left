# Debug: `Session is no longer active` HALT blocked every tool call in a live session

**Date:** 2026-08-14 · **Repo:** openbox-shift-left (read-only diagnosis; no fixes applied)
**Severity: high.** A governance product denied every tool call of a developer whose org has
published no policy — the exact outcome ADR-0016's default-enforce mitigation claims cannot happen.

## Symptom

At 11:52 local, three consecutive tool calls in a live Claude Code session were denied:

```
OpenBox governance: Session is no longer active
```

Bash, Bash, then Read — the gate is `*`, so every tool. Cleared on its own by ~12:03. No code or
config changed in between.

## The message is a verbatim server reason, and it is NOT a policy decision

`grep "no longer active"` across this repo: **no match.** The string comes from the control plane.

`GovReason` (`adapters/common/hookflow/enforce.go:481-491`) renders
`"OpenBox governance: " + e.Reason`, then appends `" (policy: " + e.PolicyID + ")"` when a policy id
is present. **The observed message had no `(policy: …)` suffix**, so `PolicyID` was empty.

The durable audit record agrees (`~/Library/Application Support/openbox/enforcements.jsonl`, three
records, matching the three blocked calls by `tool_kind` shell/shell/file):

```json
{"session_id":"4459b8ed-…","tool_kind":"shell","verdict":"HALT","would_block":true,
 "applied_decision":"deny","source":"evaluate","fail_open":false,
 "approval_ref":"00000000-0000-0000-0000-000000000000"}
```

Three things follow, and each is load-bearing:

- **`source: "evaluate"`** — the verdict came from the control plane, not from any local step.
- **`fail_open: false`** — this is not the synthesized fail-closed HALT. That path sets
  `FailOpen==true` precisely so the two are distinguishable (`enforce.go:212-215`), and it is off by
  default anyway.
- **zero-UUID `approval_ref`** — `governance_event_id` came back as the zero UUID, so the server
  recorded no governance event for the decision that blocked the work.

Empty `policy_id` + zero `governance_event_id` + a lifecycle-flavoured reason string is not the
shape of a policy verdict. It is the shape of a **precondition failure expressed as one**.

## Why both advertised guarantees missed it

`openbox init` prints: *"mode: ENFORCE — inert until your org publishes a policy, and fail-open, so
an OpenBox outage never blocks you."* Neither clause covers this.

| Guarantee | Why it did not engage |
|---|---|
| "fail-open, so an outage never blocks you" | `FailurePolicy` is defined as what happens when OpenBox **"cannot produce a real verdict"** (`failurepolicy.go:5-19`). A HALT *is* a real verdict, so `ApplyFailurePolicy` never runs. Fail-open covers *unreachable*, not *reachable and answering HALT*. |
| "inert until your org publishes a policy" | Falsified directly: `PolicyID` was empty. No org policy authored this, and it blocked anyway. |

`MapVerdict` maps `HALT → deny` unconditionally (`enforce.go:428-434`). The client holds two signals
that no policy authored the verdict — empty `PolicyID`, zero `GovernanceEventID` — and uses neither.

## Scope of what was actually blocked

Enforcement gates **PreToolUse only** (`adapters/claude-code/hookrun.go:197`:
`gated := hook == HookPreToolUse && ResolveEnforce()`). So:

- the three PreToolUse HALTs were **applied** (`applied_decision: deny`);
- HALT verdicts also landed on `PromptSubmitted` and `SessionEnded` events, recorded with
  `would_block: true` but **not applied**, because those hooks do not gate.

That is luck, not design. A blocking verdict on a lifecycle event has no coherent meaning — you
cannot block a session that has already ended — and only the PreToolUse-only gate kept it inert.

## The full advisory history (all 8 records, local +07:00)

| local | session | event | verdict | blocked | drift |
|---|---|---|---|---|---|
| 08-14 00:46:01 | 37157843 | **SessionStarted** | **HALT** | true | — |
| 08-14 00:46:02 | 37157843 | SessionEnded | **HALT** | true | — |
| 08-14 01:17:03 | e800efc2 | PromptSubmitted | ALLOW | false | true |
| 08-14 01:31:48 | e800efc2 | PromptSubmitted | ALLOW | false | true |
| 08-14 01:34:14 | e800efc2 | SessionEnded | ALLOW | false | true |
| 08-14 11:52:16 | 4459b8ed | PromptSubmitted | **HALT** | true | — |
| 08-14 12:30:24 | c0e75ec0 | SessionEnded | **HALT** | true | — |
| 08-14 12:31:10 | 4459b8ed | PromptSubmitted | ALLOW | false | true |

**The decisive row is the first one.** Session `37157843` was HALTed on its own `SessionStarted` —
its very first event. A session cannot be "no longer active" at the moment it is created. So either
the reason string is inaccurate, or "session" in that message denotes something other than the
governed session.

## Mechanism — confirmed in openbox-core source

The sibling repo is on this machine, so this is read from the emitting code, not inferred.

**`openbox-core/internal/services/governance_workflow.go:233-253`** — a pre-check that runs
**before OPA, guardrails and AGE**, on every event:

```go
// Pre-check: query current session status BEFORE SessionLifecycleActivity mutates it.
// Reject events for non-pending sessions (already halted/completed/failed/blocked).
if preCheck.SessionStatus != "" && preCheck.SessionStatus != content.SessionStatusPending {
    reason := "Session is no longer active"
    if preCheck.SessionDetail != nil && *preCheck.SessionDetail != "" {
        reason = *preCheck.SessionDetail
    }
    return &content.GovernanceVerdictResponse{Verdict: content.VerdictHalt, …}
}
```

**No policy is consulted.** That is why the verdict carried an empty `policy_id` and a zero
`governance_event_id`: the request never reached policy evaluation, and no governance event was
created. An org with no policy published is fully exposed to this path.

The session row is keyed `(workflow_id, run_id)` =
(`workspaceID || developerDID`, **the Claude Code session id**) — `client/payload.go:152-153`,
`:288-300`. Status writers, and only these two (there is no stale-session sweeper in core):

| Writer | Effect | Leaves `detail` |
|---|---|---|
| `storage_session.go:170-205` on a terminal event — `SessionEnded` → `WorkflowCompleted` | `completed` | **nil** |
| same, `WorkflowFailed` | `failed` / `blocked` / `halted` | set (error cause or governance reason) |
| `validation.go:34-46` `UpdateSessionHaltedActivity` — "when the governance verdict is HALT" | `halted` | the halting reason |

The observed message was the **generic** one, so `SessionDetail` was empty — which points at
`completed`, the one transition that leaves detail nil. I.e. **a terminal `SessionEnded` had already
been recorded for that session id**, and every later event in the still-live session was rejected.

Recovery is explained by the same file: `handleSessionCreate` (`storage_session.go:58-66`) inserts a
**new row with status `pending`** on any `WorkflowStarted`. A subsequent `SessionStarted` therefore
restores a pending row, which is why 12:31 was ALLOW with no deliberate intervention.

### The aggravating design detail

`UpdateSessionHaltedActivity` latches: **any single HALT marks the whole session `halted`**, and
every subsequent event then returns HALT from the pre-check. There is also a second rejection path
at `governance_workflow.go:274-297` for `halted`/attested sessions, whose default reason is *"The
workflow is terminated manually on Openbox"*. So one transient HALT does not block one tool call —
it bricks the remainder of that developer's session.

### Still unexplained

Session `37157843` was HALTed on its own `SessionStarted` (00:46:01). Under this model that requires
a non-pending row to already exist for that `(workflow_id, run_id)`, which a fresh UUID session id
should make impossible. Either out-of-order delivery or a second insert path is involved; it needs
the `sessions` rows to settle, and the runtime `obx_` key is scoped to core's evaluate endpoint, so
the backend API returns 401 for this query.

## Blast radius

- 3 tool calls hard-denied in one session; ~11 minutes of a developer session unusable.
- The denial is indistinguishable, to the developer, from their org's policy blocking them. The
  message names governance and cites no policy, which reads as "blocked, cause unknown".
- Fail-closed orgs are unaffected in kind but worse in degree — same path, already denying.
- Auditing is degraded: the blocking decision produced **no governance event** (zero UUID), so the
  control plane has no record of the thing it did.

## Diagnosis-time friction worth fixing regardless

`enforcements.jsonl` records carry **no timestamp and no reason**. Correlating the three denials to
a clock required joining them against `advisories.jsonl`, which does carry `ts`. Adding `ts` and the
(already content-free, policy-authored) `reason` to the enforcement record turns this from a
two-file correlation into a one-file lookup.

## Options — an OD-class decision, not inferable

Changing what enforcement does with a HALT changes the product's security posture, so this is
surfaced rather than chosen:

1. **Treat a HALT with empty `policy_id` AND zero `governance_event_id` as "no verdict"**, routing it
   into `ApplyFailurePolicy` — proceeds under the default `fail_open`, still denies under
   `fail_closed`. Restores both advertised guarantees. Risk: a genuine policy HALT that legitimately
   carries no policy id would be downgraded; needs the backend to confirm that cannot happen.
2. **Keep denying, but tell the truth in the message** — mark it operational rather than
   policy-authored, so a developer is not left believing their org blocked them. Smallest change;
   does not restore the guarantee.
3. **Fix it server-side**: do not express a precondition failure as a policy verdict, or set a
   field that distinguishes them. Note `fallback_used` already exists on the wire but **the client
   does not parse it**: it appears only inside a mock response body in `client/client_test.go:117`,
   and no struct field binds it (`client/verdict.go:192-204`). It may be the discriminator this
   needs, at the cost of a contract change — confirm the server actually sets it before relying on
   it, since a test stub is not evidence about production.

Either way, the doc and the behaviour must agree: today `init` prints a guarantee the enforce path
does not honour.

## Unresolved questions

1. What delivered a terminal `SessionEnded` for `4459b8ed` while the session was still live? A
   crash-and-resume cycle and a late spool flush are both candidates; the `sessions` rows would
   settle it.
2. Is HALT-on-precondition intentional in the control plane, or does that path mean to return an
   error the client would treat as "no verdict"? Core deliberately expresses it as a verdict, which
   is why the client cannot tell the difference — but "reject an event for a closed session" and
   "the policy halted this action" are different facts and currently share one channel.
3. Does `fallback_used` (or another field) already distinguish operational from policy verdicts? If
   so, parsing it is the cheapest fix.
4. Should a blocking verdict on a lifecycle event (`SessionStarted`/`SessionEnded`) be rejected
   client-side outright? It is inert today only because the gate is PreToolUse-only.
5. Was the 00:46 `SessionStarted` HALT the same defect, or a second one? Same message, but a
   just-created session makes the stated reason self-contradictory.
