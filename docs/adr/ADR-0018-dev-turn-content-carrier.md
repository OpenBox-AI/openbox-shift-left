# ADR-0018 — Tool status, and one wire span carrying the assistant turn text

Status: Accepted. Date: 2026-08-13.

Amends **ADR-0013** (dev sessions are span-less — true for tool events, no longer
true for one carrier) and **ADR-0014** (INV-2's content statement gains an egress
class outside the transcript projection). Forward-references **ADR-0019**, which
proposes the full-capture posture this is the first increment of.

## Context

Two OpenBox dashboard surfaces are empty or wrong for every developer session, and
neither is a rendering bug. Both are the client not sending a field the server
already reads.

**Tool Health reports SUCCESS 0.0%.** Core derives per-tool success from exactly
one expression
(`openbox-core internal/services/activities/observability/errors.go:333`, read at
`develop` 68f0398):

```go
metric.IsSuccess = payload.Status != nil && *payload.Status == "completed"
```

`governanceEventPayload` (`client/payload.go:28-78`) has no `status` key and never
has had one. `.total` increments on every `ActivityStarted`, so every completion
increments `tool.<name>.failed` and the ratio is 0% by construction — for every
producer, forever. No producer has ever written the field.

**Goal Alignment Trend and Recent Drift are empty.** The alignment extractor reads
assistant text from ONE place (`goal_alignment_session.go:64-88`): the LAST entry
of `payload.Spans`, and only when `Stage=="completed"`, `SemanticType` is
`llm_completion`, and `ResponseBody` is non-nil. ADR-0013 deleted the span layer
for developer sessions, so `payload.Spans` is always empty and the extractor
always returns `""`. The widget has nothing to score.

The first is a pure omission. The second cannot be fixed by sending more metadata:
the feature consumes the assistant's words, so feeding it is a content decision.
This ADR records both, because the second is a new egress class and the first
reverses a shipped statement.

## Decision 1 — `status` on tool results

**Tool `ActivityCompleted` carries a top-level `status`, enum `completed` |
`failed`.**

- **Derived structurally only** — from hook identity, a bound bool, or a bound
  int. Never parsed from tool output text. Claude Code makes this exact: the
  installed 2.1.229 binary documents `PostToolUse` as "Run after **successful**
  tool" and `PostToolUseFailure` as "Run after tool **fails**", and an empirical
  probe of a failing `Bash` produced `PostToolUseFailure` with **zero**
  `PostToolUse` firings
  (`plans/reports/probe-260813-2329-claude-code-hook-surface.md`). Hook identity
  therefore *is* the outcome; nothing is inferred.
- **Not content-gated.** A two-literal enum cannot encode content, so it ships
  byte-identically with `content_capture:false`. This is the one part of this ADR
  that changes nothing about privacy.
- **Tool results only** — never turn, lifecycle or signal events. Two reasons, and
  the second is the binding one:
  1. Core already excludes `llm_completion` from tool metrics
     (`errors.go:320-322` via `IsLLMCompletionActivity`), so a status on a turn
     would do nothing for Tool Health.
  2. `payload.Status` also writes `governance_events.workflow_status` for *any*
     event type (`storage_event.go:417`). On a lifecycle event that column is
     genuinely workflow-scoped; overwriting it with a tool outcome would corrupt
     a real field to populate a derived one. The reader inventory for that column
     is one consumer — the backend compliance evidence export
     (`openbox-backend src/modules/compliance/compliance-source-evidence.service.ts:141`)
     — which makes the tool-scoped write acceptable and the lifecycle write not.
- **Closed vocabulary, enforced client-side.** Anything outside
  {`completed`,`failed`} is omitted rather than sent. A typo would not degrade the
  metric, it would zero it — `"COMPLETED"` is not `"completed"` — so the client
  refuses to ship an unrecognized value at all.

## Decision 2 — exactly one wire span on `TurnCompleted`

**A `TurnCompleted` event carries exactly one span, whose `response_body` is the
assistant turn's text, under the content gate.**

The shape is not a design choice; it is dictated three times over by the consumer:

| Constraint | Source |
|---|---|
| read from `payload.Spans`, LAST entry only | `goal_alignment_session.go:65-68` |
| `stage == "completed"` | `:69` |
| `semantic_type == llm_completion` | `:69` |
| `response_body` = `{"choices":[{"message":{"content":"…"}}]}` | `:72-88` (anything else logs and yields `""`) |

Fixed properties:

- **Deterministic ids.** `span_id = hex(sha256("turnspan\x1f"+activity_id))[:16]`,
  `trace_id = hex(sha256("turntrace\x1f"+session_id))[:32]`. Core dedupes spans on
  `(span_id, stage)` scoped by session (`internal/content/governance.go:257-258`),
  and the turn cursor deliberately over-reports after a crash, so a re-reported
  turn must re-mint byte-identical ids or it stores a second row. Random ids would
  turn the cursor's safe direction into duplicate storage.
- **`hook_trigger` stays unset.** A payload with `hook_trigger` true *and* spans
  present enters core's approval-bypass fingerprint path
  (`governance_workflow.go:310-330`). A turn is not an approvable operation and
  must not touch that path.
- **`span_count` = 1**, and both keys are absent when there is no text.
- **Redacted before attach, then capped.** Local secret detection runs over the
  text before it is placed on the event, and `capBody`'s 64KB limit
  (`client/payload.go:698,706`) is applied to the TEXT before the JSON wrapper, so
  the cap bounds the content rather than the envelope. Ordering is asserted on the
  outbound bytes, following the precedent conformance case C18 set for file
  bodies.

**Source of the text: the hook payload field `last_assistant_message`** on
`Stop`/`SubagentStop` — which the provider itself recommends over the transcript
("Avoids the need to read and parse the transcript file", 2.1.229 schema
description; the transcript is written asynchronously and lags). This choice is
load-bearing for Decision 4.

## Decision 3 — synthesized classification keys (OD-0018-1, resolved)

**`semantic_type` cannot be asserted from the client.** Core recomputes it per
span before storage (`governance_workflow.go:303`), so whatever the client sends
is overwritten. The only inputs that yield `llm_completion` are `isLLMCall`'s
(`internal/content/session.go:451-476`): attribute `http.method == "POST"` plus an
`http.url` containing an LLM domain — `api.anthropic.com` is on that list
(`session.go:137-149`) — or the root-field twin `http_method`/`http_url`
(`session.go:221-256`). The span name must additionally not contain `EMBED` or
`TOOL` in uppercase, or it classifies as embedding/tool-call instead
(`session.go:323-334`).

So the span carries **attributes that describe an HTTP call this client did not
make**:

```json
{"http.method":"POST","http.url":"https://api.anthropic.com/v1/messages","openbox.span_synthetic":true}
```

This is a real wart. It was escalated as **OD-0018-1** and **resolved by the owner
on 2026-08-13: accept, mark, and file the fix.**

- **Accept** — the alternative inside this repo's scope is not having the feature.
- **Mark** — `openbox.span_synthetic:true` is on every such span, so nobody
  auditing stored spans mistakes it for an observed request.
- **File** — [openbox-core#130](https://github.com/OpenBox-AI/openbox-core/issues/130)
  asks AGE to read assistant content from the `llm_completion` **activity_output**
  for span-less developer sessions. **That issue is the retirement condition for
  these keys and for this whole span.** When it lands, the synthesized attributes
  are deleted and the text moves to `activity_output.message` — which is ADR-0019's
  P2 target state, not a second migration.

Deleting the synthesized keys before core changes silently kills the feature: the
span still stores, classifies as something else, and the extractor's
`SemanticType` check quietly fails. There is no error to notice.

## Decision 4 — the INV-2 amendment

**Assistant final text becomes a content class that egresses under the org gate.**

Precisely scoped, because the scope is the whole safety argument:

- **Source is the hook payload, not the transcript.** `usage.go`'s transcript
  projection and its sentinel `TestFinops_NoContentOnWire`
  (`adapters/claude-code/usage_test.go`) are **untouched**. ADR-0014's allowlist —
  `message.model` egresses, `content`/`text`/`thinking` have nowhere to land —
  stands exactly as written. A future transcript-bound string still needs its own
  amendment; ADR-0019 P3 is that amendment, for thinking.
- **What is retired is narrower than it looks.** `hookevent.go`'s comment that
  `Stop`/`SubagentStop` "deliberately bind NOTHING of their own … the absence is
  the safeguard" stops being true for **one field**. `last_assistant_message` is
  bound. Everything else on those payloads stays unbound.
- **`stop_reason` is not deferred — it does not exist.** The 2.1.229 `Stop`
  payload has no such field (probe report §Q2, empirically absent and absent from
  the binary's own schema). ADR-0019 should not plan to bind a field this provider
  version does not send.
- **Thinking is deferred, not foreclosed.** It is not in this carrier. The owner's
  posture (2026-08-13) has thinking in scope, staged last because capturing it
  requires amending ADR-0014's allowlist and evolving a load-bearing sentinel.
  **ADR-0019 P3 owns that decision.** Nothing here says "never".

### Gate composition

The span requires **both** gates, in this order:

```
finops:true          → turn events exist at all      (hookrun.go:179; ADR-0014)
  ∧ window-has-usage → a turn with no tokens is not a turn
  ∧ content_capture  → Mapper attaches the text, and Emit's stripContent
                       independently re-checks before egress
```

Two independent gates on the same fact is deliberate: the adapter deciding not to
attach, and the client choke point deciding not to send. With
`content_capture:false` **nothing new egresses** — no `spans`, no `span_count`, no
text — and `status` is unchanged either way.

**Both gates are default-ON today** (content capture since 2026-07-15; finops
since ADR-0014), and per the owner's constraint they stay that way. A future
default flip re-opens this decision, because alignment silently stops working when
either goes off and no error says so.

Redaction is gated separately on `secret_detection`. **With `secret_detection:false`
the assistant text egresses unredacted.** That is stated rather than mitigated.

## What this does NOT change

- `activity_id`, `event_id` and approval keys. `deriveID` hashes an explicit field
  list that excludes both `Status` and `Content`; the pin tests
  (`client/approval_key_pin_test.go`, `client/turn_key_pin_test.go`) stay green
  **unedited**, and that is the proof rather than a claim.
- Tool `activity_input`/`activity_output` shapes, the enforce cascade, the
  approval hold, lineage, deprecated-key handling.
- **"Tool output: never"** (`docs/data-and-privacy.md`). Assistant text is not tool
  output. Tool output is ADR-0019 P1 and is not authorized here.
- `client/hookspan.go` and `client/spanbuilder.go` stay **deleted**, along with the
  family root tuples and `AssertHookWireShape`. ADR-0013's deletion is not being
  reverted: one carrier gains a minimal span, tool events remain span-less.
- Codex. No assistant-text field exists on its hook surface, its `Stop` is
  deliberately unwired (`adapters/codex/capabilities.go:14`), and its usage arrives
  as one SessionEnd rollup — the same flush as `WorkflowCompleted`, which DELETES
  the goal session (`openbox-core internal/services/age.go:158-160`). Wrong
  granularity and racy ordering; recorded as non-support, not as a bug.

## Consequences

- **Span rows return for developer sessions**, for this one carrier: a `spans` row
  per captured turn, span-level Merkle leaves, `age_span_evaluations`, and
  server-side `semantic_type` classification. `docs/architecture.md`'s "no `spans`
  rows at all" and `docs/data-and-privacy.md`'s "the channel cannot be re-opened by
  an adapter mistake plus a content-capture opt-in" both stop being true and are
  rewritten rather than quietly left standing.
- **Retention increases server-side**, at up to 64KB per captured turn, outside
  this repo's control. Named here because nobody should learn it from a bill.
- **Policy sees a `spans` array again.** An OPA rule keyed on `http_url` would
  match every captured turn. It cannot block anything — `Stop` writes no stdout and
  never gates — but it can fire alerts. If that proves disruptive, the documented
  response is the root-field variant (`http_method`/`http_url`), not removing the
  marker.
- **The compliance evidence export gains `workflow_status`** on tool activity rows.
- `contracts/dev-event/MAPPING.md:235-245` is wrong in three rows the moment this
  ships (`span.response_body` "dropped as an egress channel", `span.stage`
  "retained, read by nothing", and `status` in the retired list). Reconciling them
  is part of shipping, not follow-up.

## Alternatives rejected

**Widen the transcript projection** to read assistant text from the JSONL. Rejected:
it costs the structural guarantee ADR-0014 deliberately preserved, in exchange for
a source the provider itself says lags. The hook field is better data *and* cheaper
in invariants.

**Put the text on `activity_output.message` now** — the clean shape, and the actual
target state. Rejected here only because AGE does not read it, and teaching it to
is an openbox-core change this plan's scope excludes. It is FILED
([openbox-core#130](https://github.com/OpenBox-AI/openbox-core/issues/130)), and it
retires the span.

**Promote `metadata.status`** instead of a top-level field. Rejected: core reads
`payload.Status` and nothing else. A metadata key would leave the metric at 0% while
looking correct in the payload.

**Random span ids.** Rejected: a re-reported turn would look like a new span to
core's `(span_id, stage)` dedupe and store twice.

## Evidence and its limits

- Every openbox-core claim was **read at `develop` 68f0398**, not observed in a run.
  Each carries its `file:line`. If a live run contradicts one, amend this ADR with
  the observed behaviour rather than working around it.
- The hook-surface claims are backed by an empirical probe on Claude Code 2.1.229
  plus the binary's own input schemas (`plans/reports/probe-260813-2329-…`).
  `PermissionDenied` and `StopFailure` were **not** producible on demand and are
  schema-verified only.
- **The testbed has not run.** Conformance asserts the wire bytes against a real
  `/evaluate` stub over HTTP, which is strong evidence about what this client sends
  and no evidence about what the server stores or renders.
- Goal alignment additionally requires reachable infrastructure this repo does not
  own: `LlamaFirewallHost` must be set or `performTraceCheck` returns nil
  (`openbox-core internal/services/llama_firewall.go:31-34`), and Redis must be up
  for the goal session. **Both widgets stay empty with a perfect client** if either
  is missing — a diagnosis branch, not a defect of this change.
