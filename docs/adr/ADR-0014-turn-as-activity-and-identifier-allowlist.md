# ADR-0014 — A model turn is an Activity; INV-2's usage path becomes an allowlist

Status: Accepted — 2026-08-11.
Implements: `client/event.go` (`EventTurnStarted`/`EventTurnCompleted`, widened
`Tokens`, `Model`, `TurnIndex`, `AgentID`), `client/payload.go`
(`turnActivityIDFor`, `turnActivityOutput`, `activityLabel`),
`client/turn_key_pin_test.go`, `client/testdata/golden/activity_turn_*.json`,
`contracts/dev-event/schema/dev-event.schema.json` v1.1,
`adapters/*/usage.go`.
Builds on: ADR-0013 (a tool call is an Activity; the span layer is retired).
ADR-0013 is **not amended** — spans stay retired, and this rides the activity
shape it established. *(ADR-0018 later amended that: one span rides
`TurnCompleted`. The sentence above records what was true when this ADR was
accepted.)*
Narrows: the INV-2 claim that `usage.go`'s transcript projection is
*structurally* content-proof.
Amended by: **ADR-0018** — a `TurnCompleted` may carry assistant text, sourced
from the `last_assistant_message` hook field. The transcript allowlist below and
its sentinel `TestFinops_NoContentOnWire` are **untouched** by that amendment; a
transcript-bound string still needs its own.
Amendment PROPOSED by: **ADR-0019 P3** — thinking blocks and intermediate
assistant text, which are transcript-only and therefore DO require widening the
allowlist below. Not accepted, not implemented. If it is accepted, the sentinel
evolves from asserting absence to asserting redaction-and-cap, and the rule this
ADR set carries over unchanged: **a version that passes trivially is a defect.**

## Context

An AI-Agent session on the OpenBox runtime answers "which model spent how many
tokens, when". A Coding Agent session does not. Across 130 live developer event
rows there is no token usage at all, and `metadata.model` appears exactly once
per session — at `SessionStart`, from a hook field the provider documents as "not
guaranteed to be present".

Two facts shape what can be built.

**Hooks expose no token usage whatsoever.** Not per turn, not per session, not on
any hook. The session transcript JSONL is the only source, and this repo already
reads it: `adapters/claude-code/usage.go` aggregates `message.usage` into a
SessionEnd rollup, and `adapters/codex/usage.go` reads the rollout's cumulative
`total_token_usage`. So the capability largely exists.

**Nothing flows.** `ResolveFinops()` was default-off, and off-by-default is why
the rollup that does exist reached no dashboard. Turning it on is Phase 05's
concern; this ADR is about what shape the signal takes when it does flow.

The natural target shape is the one the AI-Agent runtime already emits: an
`llm_completion` **span** whose `response_body` carries `{model, usage{…}}`.
Dev sessions write no spans (ADR-0013), so that shape is unavailable — and
reviving the span layer to reach it would undo ADR-0013 for a reporting feature,
which is the wrong trade in the wrong direction. Worse, core's span-based usage
aggregation is gated on `detectLLMProvider` reading `span.GetHTTPURL()`
(`internal/services/activities/observability/invocation.go:129-131`,
`model.go:173-190`), and a hook process has no HTTP URL to report. Satisfying
that gate would mean synthesizing a provider URL for a call this process never
made — a fabricated field to unlock a code path, exactly the pattern ADR-0013
retired.

## Decision

**A model turn is an Activity**, carried by the same two accept-listed wire
types a tool call uses:

- `TurnStarted` → `ActivityStarted`
- `TurnCompleted` → `ActivityCompleted`
- both with `activity_type: "llm_completion"`
- usage in the Completed half's `activity_output`, shaped like the LLM span's
  `response_body`: `{"model": …, "usage": {input_tokens, output_tokens,
  cache_creation_input_tokens, cache_read_input_tokens}}`

The `llm_completion` name is not invented here. Core already uses it as a
semantic type for the AI-Agent runtime's model calls
(`openbox-core internal/content/session.go:105`), so one vocabulary spans both
runtimes and the core-side extractor keys on a name core already knows. What
changes is only the carrier: an `activity_type` string instead of a span's
`semantic_type`.

`activity_id` is `<session_id>:turn:<index>`, or
`<session_id>:agent:<agent_id>:turn:<index>` for a subagent's turn. Unlike the
tool-call id it is **not** a hash, for three reasons: a turn has no operation to
key on, a turn is never approved so the id is not an approval key, and a readable
id is worth having in stored rows. It cannot collide with `cc-act-<32 hex>` by
construction, and `client/turn_key_pin_test.go` pins both that separation and the
exact bytes — the same discipline the approval key gets, because core's dedupe
key includes `activity_id` and a derivation that drifts between the two halves
splits one turn across two rows.

**Both halves are emitted from one hook firing.** Claude Code's `Stop` fires at
turn *end*, so a real turn-open timestamp has to come from somewhere; the
alternative was to open the pair from `UserPromptSubmit`. Emitting both from
`Stop` wins on three counts: the pair is atomic (no orphan half, no cross-hook
turn index to race), queued prompts fold into one turn rather than opening
several, and the Started half's timestamp is *consumer-invisible* anyway — core
derives duration only from `duration_ms` on the Completed half
(`observability/errors.go:314`), and `updateEventCompletion` is span-path-only
(`storage_event.go:137-167`). So the turn-open time is parsed locally from the
first new transcript line purely to compute `duration_ms`, and the timestamp
*string* never reaches the wire.

Codex reaches the same signal at **session** granularity — one
`llm_completion` pair at SessionEnd with `activity_id
<session_id>:usage:rollup`. Its `Stop` hook exists but is deliberately unwired;
that is scope, not impossibility, and `capabilities.go` says so in those words.

### The part that is a real narrowing: INV-2

`usage.go`'s guarantee has been that content *cannot* enter memory through the
transcript projection — not because anything filters it, but because the
projection struct has only numeric fields and `encoding/json` drops what has
nowhere to land. That is a structural impossibility argument, and it is the
strongest kind. **After this change it is false as written.**

The projection now binds three non-numeric fields, and the replacement guarantee
is a curated allowlist that a test enforces:

| Field | Class | Reaches the wire? |
|---|---|---|
| `message.model` | identifier | **Yes** — the one string, `capStr`-bounded |
| `timestamp` | timestamp | No — parsed to a `time.Time` for `duration_ms`, then discarded |
| `isSidechain` | bool | No — partitions subagent lines out of the parent's sums |

Everything else — `content`, `text`, `thinking`, `tool_input`, `tool_result`,
`cwd` — still has nowhere to land. The sentinel test was rewritten to prove the
narrower claim rather than the old one: sentinel content absent from the signed
wire bytes, the model present, the raw timestamp absent, sidechain sums excluded.
A sentinel test that passed unchanged through this change would have meant the
projection was never the guarantee.

Why an allowlist is defensible here: the model id is the **aggregation key**, not
garnish. The backend sums token rollups only from `metric_type='model'` composite
keys (`<model>.input_tokens`,
`openbox-backend src/modules/observability/observability.service.ts:216-241`);
`metric_type='token'` is a documented dead path. A turn without a model is
invisible to the dashboards this feature exists to fill. So the choice was: one
bounded provider identifier on the wire, or a feature that stores numbers nobody
can read.

`activity_output` is also an inspected field — core runs Guardrails stage 1 over
it (`internal/services/guardrail.go:191-194`) and feeds it to OPA
(`internal/services/opa.go:529-531`). Token spend therefore becomes
policy-visible, which is a genuine upside, and a second reason the schema must
stay four numbers plus one bounded identifier.

### Cost stays absent

Never derived in this client. Both core (`internal/content/const.go`) and the
backend (`src/common/utils/calc-llm-cost.ts`) already derive cost server-side
from model-keyed pricing tables. Deriving it here would fabricate a number from a
table this client has no business owning — the same class of error as
attributing tokens to a guessed model.

### `Tokens.Input` changes meaning

`Tokens` gains `cache_creation_input` and `cache_read`, and `Input` becomes
**pure** input. The Claude Code rollup used to fold both cache counts into
`Input` because there was nowhere else to put them, which made cache efficiency
unmeasurable and made a cache-heavy session look like it had spent its whole
context on fresh input. This is the one non-additive change in the contract, and
it is why `schema_version` goes 1.0 → 1.1 rather than staying put: the field name
is the same and the number is different, which is precisely the change a version
exists to announce. The golden fixtures pin the new bytes.

## Consequences

**Gained**

- Per-turn model + usage attribution for Claude Code, per-session for Codex, on
  a shape core and the backend can aggregate with one new extractor and no new
  table, endpoint, or event type.
- Both wire types are already accept-listed
  (`internal/api/governance.go:273-286`), so **INV-8 needs nothing new** and a
  stock core ingests turn events unpatched.
- Cache tokens become measurable for the first time — four counts where there
  were two, in both the per-turn records and the retained rollup.
- Token spend becomes guardrail- and policy-visible via `activity_output`.
- Subagent spend is attributable (`agent_id`) and partitionable, so a session
  that spawns agents can be broken down rather than blended.
- Turn events are deduped server-side on the same key as everything else, so a
  crash that re-reads a turn re-mints the same id and the server absorbs it. That
  is what makes the local cursor's over-report-on-crash direction the safe one.

**Lost — the accepted trade-offs**

- **INV-2's usage path is an allowlist, not an impossibility.** The three-line
  table above is now load-bearing, and so is the test that enforces it. A future
  contributor who adds a fourth bound string "while we're here" breaks a
  guarantee that no longer defends itself structurally. This is the single
  biggest cost of the change and the reason this ADR exists.
- **Per-LLM-call granularity is unreachable.** Hooks fire per turn; a turn may
  contain several model calls, and the numbers are a sum over the turn's
  transcript window. The doc comments say "window sum" rather than "call" for
  exactly this reason.
- **Codex has no per-turn boundary wired.** One rollup per session. Documented as
  scope with the upgrade path named (`Stop` + delta-from-cumulative), never as
  impossibility.
- **A model-less turn is dashboard-invisible until core buckets it.** The
  contract tolerates an absent model — back-filling it from the session's
  `SessionStart` model would attribute tokens to a model that may not have spent
  them, which is a fabricated number. The core-side issue specs an `unknown`
  bucket so those turns still count.
- **New data leaves developer machines by default** (Phase 05). Four integers and
  a model id per turn. Two documented opt-outs, and the effective state rides
  SessionStarted as posture evidence so an auditor can tell after the fact which
  sessions captured.

**Not yet proven.** As with ADR-0013, every claim about core's ingest and the
backend's read path was established by reading those repos, not by running
against them. The load-bearing assumptions are that core stores an
`ActivityCompleted` bearing a colon-shaped `activity_id` as its own row, and that
the model-keyed composite metrics the backend sums are reachable from an
activity. `testbed/` against a live stack is what settles it; until that run,
MAPPING.md §7 carries the claims as underived. Separately, the aggregation is
**write-only until the core-side extractor merges** — implemented and CI-green in
[openbox-core#125](https://github.com/OpenBox-AI/openbox-core/pull/125)
(PROD-296), which adds `ExtractModelMetricsFromActivity`, the two cache composite
keys, the `unknown` bucket, and the `ExtractToolMetric` exclusion. Until it
merges, `ExtractToolMetric` still accepts any non-empty `activity_type`
(`observability/errors.go:301-323`), so `llm_completion` will additionally appear
in the dashboards as a tool with call counts and latency percentiles. Expected,
recorded, and linked from the testbed
phase — not a shift-left defect.

## Alternatives rejected

**Revive the span layer and emit a real `llm_completion` span.** The shape the
AI-Agent runtime uses, and it would light up core's existing
`AggregateTokensFromSpans` / `ExtractModelMetrics` with no new core code. It
requires undoing ADR-0013 for a reporting feature, restoring
`client/hookspan.go`, and — because the aggregation gate reads
`span.GetHTTPURL()` — synthesizing a provider URL for an HTTP call this process
never made. Fabricating a field to unlock a code path is what ADR-0013 retired.
An earlier round of this design chose exactly this, paired with a core-side
`detectLLMProvider` relaxation; the activity carrier replaces both, and since an
activity has no `http_url`, that gate never applies.

**`SignalReceived("turn_completed")` with usage in `signal_args`.** One event
instead of two, and signals are accept-listed. But a signal has no
`duration_ms`, no paired-halves semantic, and no `activity_output` — so it is
neither guardrail-inspected at stage 1 nor comparable to any other unit of work a
session performs. It would also have been a third shape for "a thing that
happened", against ADR-0013's one-serializer direction.

**Carry usage in `metadata` only, on the existing SessionEnded event.** Zero new
event types, zero contract change — and stored, queryable, and *never rendered*:
every usage-aggregation path in core reads spans, and openbox-fe renders only
pre-aggregated API data (`kpi-cards.tsx:21`, `model-usage-chart.tsx:100`). The
result is a feature that looks shipped and shows nothing. This is what the
codebase already did, and it is why the rollup that existed reached no dashboard.

**Open the pair from `UserPromptSubmit`.** Gives the Started half a genuinely
real timestamp instead of a locally-parsed one. Costs an orphan-`Completed` case
when no prompt preceded the turn, a cross-hook turn index two processes must
agree on, and a turn per queued prompt. Since the Started timestamp is
consumer-invisible, the price bought nothing a local parse does not.

**Derive cost client-side from a pricing table.** Rejected on the standing rule
that this client never fabricates a number. Both server sides already derive it,
and their tables disagree with each other on key style
(`claude-sonnet-4` vs `claude-sonnet-4.5`) and list none of the current Claude
Code default models — so a third table here would add a third answer. Noted in
the core-side issue instead.
