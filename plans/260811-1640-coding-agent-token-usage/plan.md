---
title: "Per-turn model and token usage for the developer runtime"
description: "Capture which model spent which tokens per turn in Claude Code and Codex sessions, matching the AI-Agent finops signal on a span-less wire."
status: implemented-pending-live-verification
priority: P2
effort: 14h
branch: fix/tier2-duplicate-activity-started
tags: [finops, telemetry, privacy, adapters, contract, decision record]
created: 2026-08-11
---

# Per-turn model and token usage for the developer runtime

Bring Coding Agent sessions to finops parity with AI Agent sessions: know which
model ran, and how many tokens it spent, per turn.

## Why this is smaller and stranger than it looks

Most of it is built — `usage.go` in **both** adapters, `client.Tokens`/`Cost`,
`metadata.model`. Two facts reshape the work:

1. **Hooks expose no token usage whatsoever**, and `model` only on `SessionStart`,
   "not guaranteed to be present". The transcript JSONL is the only source — which
   the repo already reads.
2. **Nothing flows today.** `ResolveFinops()` is default-off: across 130 live event
   rows there is no usage data at all, and `metadata.model` appears once per session.

So this is not "add token capture". It is: add a turn boundary, bind model to
usage, widen the wire, and turn it on.

## Accepted decisions (2026-08-11, validation rounds 1–2)

| Decision | Choice |
|---|---|
| Fidelity | **Per-turn**, on a new `Stop` hook |
| Carrier | **Turn-as-activity**: `ActivityStarted`+`ActivityCompleted` pair, `activity_type: "llm_completion"`, usage in `activity_output` shaped like the LLM span's `response_body` (`{model, usage{…}}`) |
| Trigger | **Both halves emitted from `Stop`** — atomic pair; Started's timestamp is the locally-parsed turn-open time |
| `activity_id` | **`<session_id>:turn:<index>`**, pinned by a byte-identity test |
| Subagents | **`SubagentStop`** as a second hook; separate per-subagent records; sidechain partition (one bound bool) if measurement shows subagent usage in the parent transcript |
| Codex | **SessionEnd rollup activity** (`<session_id>:usage:rollup`) + per-turn limit documented as deliberate scope (its `Stop` hook exists, unwired) |
| INV-2 posture | **Curated identifier allowlist** — `model` becomes the one string the projection egresses; needs a decision record |
| Default | **ON**, mirroring the 2026-07-15 content-capture reversal; requires `Finops` → `*bool` first |
| Cost | **Absent from the client** — never derived here; core/backend already derive it server-side from model-keyed metrics |
| Cross-repo | **One issue, openbox-core only** — backend and fe confirmed no-change |

## Shape constraint, stated plainly

The AI-Agent signal lives in an `llm_completion` span (`response_body.model`,
`response_body.usage.*`). Dev sessions write **no spans**, so the turn rides the
activity shape instead: an `ActivityStarted`/`ActivityCompleted` pair with
`activity_type: "llm_completion"` and `activity_output` mirroring the span's
`response_body`. Both wire types are already accept-listed (INV-8 needs nothing
new), and the name matches core's existing `SemanticTypeLLMCompletion` vocabulary
(`openbox-core internal/content/session.go:105`).

The consumer question is answered, not open (validation round 2): core aggregates
usage only from spans today, and the backend dashboard reads model-keyed composite
metrics — so one core-side extractor (a single issue, filed in Phase 01) makes dev
activities aggregate into dashboards that already exist. Backend and fe need no
change. Per-LLM-call granularity stays unreachable: hooks fire per turn, not per
model call.

## Phases

| # | Phase | Effort | Status | Depends on |
|---|---|---|---|---|
| 01 | [Contract: turn-as-activity and that decision](phase-01-decisions-and-contract.md) | 2h | **complete** (core issue filed + implemented) | — |
| 02 | [Turn boundary: Stop + SubagentStop](phase-02-turn-boundary-stop-hook.md) | 3h | **complete** | 01 |
| 03 | [Per-turn usage and model extraction](phase-03-usage-and-model-extraction.md) | 3.5h | **complete** | 01, 02 |
| 04 | [Codex: session-rollup activity](phase-04-codex-parity.md) | 2h | **complete** | 03 |
| 05 | [Posture flip and privacy docs](phase-05-posture-flip-and-docs.md) | 1.5h | **complete** | 03 |
| 06 | [Live verification](phase-06-live-verification.md) | 2h | **assertions written, RUN NOT DONE** | 04, 05 |

Phase 01 still gates 02–06 — it fixes the contract every later phase emits against —
but the consumer hard gate is resolved: the carrier is decided and the core-side
reader is a filed issue, not an open question.

## Implementation status — 2026-08-11

**Phases 01–05 implemented, reviewed, and green across all 11 modules
(`go test`, `go vet`, `gofmt`, `-race` on the concurrency-sensitive ones).
Phase 06's assertions are written but HAVE NOT RUN — so by this repo's own rule
the feature is implemented, not verified.**

### Against the acceptance criteria

| Criterion | State |
|---|---|
| N turns ⇒ N pairs, one shared `activity_id`, never double-counted; subagent usage once | **implemented, unit-verified.** Cursor + spool-then-cursor ordering + exhaustive sidechain partition; `TestRunHook_StopEmitsOneDisjointPairPerFiring` asserts three firings ⇒ three disjoint pairs with contiguous indexes. Live counting is Phase 06 |
| Codex at session granularity; per-turn limit documented as scope | **done.** `<session>:usage:rollup` pair; the capabilities test now fails on the words "cannot"/"impossible" |
| No content on the wire via the usage path; cost absent, not fabricated | **done.** Sentinel test rewritten to the narrowed claim and run against the real signed body with content-capture ON; cost asserted absent in both adapters |
| `Tokens` expresses every number; `Input` pure everywhere | **done.** Four counts in both the per-turn records and the rollup, so Σ-per-turn vs rollup compares like quantities. Contract v1.1 |
| The openbox-core consumer issue is filed | **done, and implemented.** [PROD-296](https://krnl-labs.atlassian.net/browse/PROD-296) under the Shift-left epic; all five asks shipped in [openbox-core#125](https://github.com/OpenBox-AI/openbox-core/pull/125) → `develop`, CI green, awaiting merge |
| Verified by a live testbed run | **NOT DONE.** No local stack in this session |

### What changed from the plan as written

- **The windowed read streams instead of truncating.** Review caught that reading
  up to a 64 MiB cap would report a large window's head as turn N and fold its tail
  into turn N+1 — total tokens conserved, but two turns carrying each other's
  numbers. It now aggregates in 4 MiB chunks, so the window is complete and memory
  bounded. Realistic trigger, not hypothetical: enabling capture mid-session makes
  the first firing read the whole transcript to date as one window.
- **`SessionRollup` is an explicit flag, not an absent `TurnIndex`.** Inferring
  Codex's rollup from a missing index would turn a Claude Code bug (index failed to
  set) into a silent collapse of every turn onto one `activity_id`.
- **`deriveID` folds in `TurnIndex` and `AgentID`** (only when set, so existing ids
  are byte-identical). Without it a main-thread turn and a subagent turn closing in
  the same nanosecond derive one `event_id` and a server that dedupes on it drops
  one.
- **Codex's cache arithmetic is the inverse of Claude Code's** — its cache counts
  are sub-counts of `input_tokens`, so they are subtracted. Verified against 12 real
  rollouts, not assumed from the Claude Code shape.
- **`20-capture`'s tool-call counts are now scoped** to non-`llm_completion`
  activities; turn pairs on the same wire types had quietly weakened its
  `assert_ge 4`.

### Empirically open (pre-decided either way, both carried to Phase 06)

1. Does `Stop` fire on a tool-only turn? Window sums are exact regardless.
2. Which transcript does `SubagentStop` name, and are its lines `isSidechain`?
   Measured 0 sidechain lines across 32 real transcripts, with the field present on
   every line — inconclusive, so the partition is unconditional because it cannot
   double-count under any answer. Worst case is a subagent reporting nothing, which
   Phase 06 step E surfaces as a skip rather than a pass.

Measurement evidence: [reports/measure-260811-transcript-turn-surface.md](reports/measure-260811-transcript-turn-surface.md).

## Acceptance criteria

- A turn emits model + usage (input, output, cache-creation, cache-read) attributable
  to that turn; N turns yield N `ActivityStarted`/`ActivityCompleted` pairs, each pair
  sharing one `activity_id`, never double-counted (subagent usage included exactly
  once).
- Codex reaches the same signal at session granularity (SessionEnd rollup activity);
  its per-turn limit is documented in `capabilities.go` as deliberate scope, not
  impossibility (its `Stop` hook exists, unwired).
- No content reaches the wire via the usage path — sentinel test proves the narrowed
  claim; cost stays absent from the client, not fabricated.
- The openbox-core consumer issue is filed with the round-2 spec (model-keyed
  extractor feeding `ModelMetric`, cache composite keys, `ExtractToolMetric`
  exclusion, `unknown`-model bucket).
- Verified by a live testbed run, not unit tests alone.

## Non-goals

Reviving spans or the `llm_completion` *span* shape (that decision stands
unamended; the `llm_completion` **name** is adopted as an `activity_type`) ·
per-LLM-call granularity (unreachable via hooks) · client-side cost derivation
(server-side derivation already exists and is out of scope here) · compaction
metrics (`PostCompact` sizes — adjacent, separate plan) · backfilling history.

## Evidence

- [research/researcher-01-hooks-usage-surface.md](research/researcher-01-hooks-usage-surface.md)
- [scout/scout-01-existing-finops-surface.md](scout/scout-01-existing-finops-surface.md)

## Validation Summary

**Validated:** 2026-08-11, two rounds · **Questions asked:** 7 + 4 · **Status:** all
decisions closed (round 2, cross-repo evidence); phase files still need the revision
pass before implementation.

### Confirmed decisions

| Decision | Choice |
|---|---|
| Carrier | **Turn-as-activity.** `ActivityStarted` + `ActivityCompleted` with `activity_type: "llm_completion"`; usage in `activity_output`, shaped like the LLM span's `response_body` (`{model, usage{…}}`). Not a span, not `SignalReceived` |
| Span revival | **Rejected.** That decision stands unamended; no `client/hookspan.go` restoration, no synthesized `http_url` |
| Granularity | **Started + Completed pair**, symmetric with every other dev activity |
| `activity_type` | **`llm_completion`** — one vocabulary across both runtimes |
| Cache tokens | **Keep the widening.** All four counts on the wire |
| Codex | **SessionEnd `llm_completion` activity** with the rollup total + model, plus a documented no-per-turn limit |
| Subagents | **Subscribe `SubagentStop`** — separate per-subagent records, attributed via `agent_id`/`agent_type` |
| Core changes | **In scope, cross-repo.** Shift-left collects; openbox-core consumes |

Superseded during the interview: an earlier answer chose a minimal LLM span with a
core-side `detectLLMProvider` relaxation. The activity carrier replaces both — no
provider URL exists on an activity, so that gate never applies.

### Verified during validation

- **Phase 01's hard gate is answered, not open.** Core stores payload metadata
  generically into the `governance_events` JSONB column
  (`openbox-core/internal/services/activities/governance/storage_event.go:148-203`),
  but every usage-aggregation path reads spans: `AggregateTokensFromSpans` and
  `ExtractModelMetrics` off `input.Payload.Spans`
  (`internal/services/observability_workflow.go:63,100`), both gated on
  `detectLLMProvider` → `span.GetHTTPURL()`
  (`internal/services/activities/observability/invocation.go:129-131`,
  `model.go:173-190`). openbox-fe renders only pre-aggregated API data
  (`kpi-cards.tsx:21`, `model-usage-chart.tsx:100`, `cost-analytics.tsx:14`).
  Metadata-only would be stored and queryable but never rendered.
- **Nothing reads usage from an activity today.** `ExtractToolMetric` reads only
  `activity_type`, `duration_ms`, `status`
  (`internal/services/activities/observability/errors.go:301-323`). The new core
  extractor is new code, so it can read all four counts — unlike the span parser,
  which drops both cache fields (`invocation.go:139-157`, `model.go:196-202`).
- **`activity_output` is already an inspected field**: guardrail stage-1 input
  (`internal/services/guardrail.go:191-194`) and OPA policy input
  (`internal/services/opa.go:529-531`). Token spend therefore becomes
  policy-visible — a genuine upside — and the schema must stay numbers + model id.
- **Dedupe key is `(agent_id, workflow_id, run_id, activity_id, event_type)`**
  (`internal/services/activities/governance/validation.go:95-103`). Two turns
  sharing an `activity_id` collapse into one row.
- **The Phase 05 default flip does not work as planned.** `Finops bool` is a plain
  bool whose resolver returns `&b` unconditionally
  (`adapters/common/devconfig/devconfig.go:94,263`), so an absent `finops` config
  field is indistinguishable from an explicit `false`. Flipping the default without
  changing the field to `*bool` produces a flip that silently does nothing. Compare
  `ContentCapture *bool` (`:273`).
- Codex subscribes the identical five hooks with no `Stop` and no `SubagentStop`
  equivalent (`adapters/codex/hookevent.go:19-23`).
- No `kind=developer` gate found in core's governance ingest activities (absence of
  a gate, not proof of none).

### Action items (round 1 — applied by the 2026-08-11 phase-file revision pass; the Round 2 consolidated list is authoritative)

- [x] **Phase 01 — replace the carrier.** Drop `EventTurnCompleted` +
      `SignalReceived("turn_completed")`; emit `ActivityStarted`/`ActivityCompleted`
      with `activity_type: "llm_completion"` and usage in `activity_output`. Both
      types are already accept-listed, so INV-8 needs nothing new.
- [x] **Phase 01 — retarget that decision.** Subject is now turn-as-activity plus the
      INV-2 identifier allowlist. That decision is *not* amended; state that spans stay
      retired and this rides the activity shape that decision established.
- [x] **Phase 01 — `activity_id` must be unique and deterministic per turn**, never
      colliding with tool-call ids, and derivable at both the turn-open and turn-close
      hooks so the pair correlates. Recommend `<session_id>:turn:<index>` from the
      Phase 02 cursor, pinned by a test in the manner of
      `client/approval_key_pin_test.go`. **Needs sign-off — see open questions.**
- [x] **Phase 02 — decide the Started trigger.** `Stop` fires at turn *end*, so a
      real turn-open timestamp must come from `UserPromptSubmit` (already
      subscribed). Alternative: emit both events from `Stop`. Tolerate an orphan
      `Completed` when no `UserPromptSubmit` preceded the turn.
- [x] **Phase 02 — add `SubagentStop`** with its own cursor key scoped by
      `agent_id`, plus install/pin coverage. Two hooks now, not one.
- [x] **Phase 02/03 — measure whether subagent usage also appears in the parent
      transcript.** If it does, `SubagentStop` records and the parent `Stop` record
      double-count the same tokens. This is now the plan's top correctness risk.
- [x] **Phase 03 — `activity_output` schema** mirrors the span `response_body`
      shape; all four counts; `model` remains the single bound string, `capStr`-bounded.
- [x] **Phase 04 — Codex** emits the SessionEnd `llm_completion` activity and
      records both limits (no per-turn boundary, no subagent attribution) in
      `capabilities.go` and `docs/architecture.md`.
- [x] **Phase 05 — change `Finops` to `*bool`** before flipping the default, and
      assert an absent config field resolves to on.
- [x] **Phase 06 — extend the pairing guard.** `testbed/40-approvals.sh` step G and
      any Started/Completed counting assertion now see a second activity kind per
      session. Add a tool-metric pollution check.
- [x] **plan.md line 75 — tighten the non-goal.** Spans stay retired, but the
      `llm_completion` *name* is now adopted as an `activity_type`; the current
      wording reads as forbidding both.
- [x] **Cross-repo issue against openbox-core**: activity-based usage extractor
      (model + four counts from `activity_output` where
      `activity_type == "llm_completion"`), wired into `observability_workflow.go`
      beside `ExtractModelMetrics`; exclude `llm_completion` from
      `ExtractToolMetric`; derive the provider from the model id, since an activity
      has no `http_url`.
- [x] **Re-estimate.** `SubagentStop` and the Started/Completed pair add roughly 3h;
      dropping the span layer removes none of it. 11h → ~14h.

### Unresolved questions (round 1 — all five closed in round 2 below)

1. `activity_id` format — recommendation above needs sign-off, since it is
   load-bearing for dedupe and pinned by a test.
2. Started trigger: `UserPromptSubmit` (real timestamp, orphan risk) or both events
   from `Stop` (synthetic open time, always paired)?
3. Does subagent usage appear in the parent transcript? Determines whether
   `SubagentStop` double-counts. Empirical, blocks Phase 02's design.
4. Is a stored `spans` row or the attestation path affected by the activity route?
   Not traced during validation.
5. Does openbox-fe need its own change to surface dev-session model usage, or does
   the existing monitor chart light up once core aggregates?

### Round 2 — cross-repo revalidation (2026-08-11 21:05)

**Method:** re-read openbox-core and openbox-backend at every round-1 citation, plus
the backend read path round 1 never traced; 4-question interview on what evidence
could not decide. All round-1 citations re-confirmed at their stated locations.

#### Decisions signed off

| Decision | Choice |
|---|---|
| `activity_id` (Q1) | **`<session_id>:turn:<index>`** from the Phase 02 cursor, pinned by a byte-identity test (`approval_key_pin_test.go` pattern). Core treats the id as an opaque string (`validation.go:96`); wire field is free-form (`client/payload.go:243`); cannot collide with hash-shaped `cc-act-*` ids |
| Started trigger (Q2) | **Both halves emitted from `Stop`** — atomic pair, no cross-hook index coordination, queued prompts fold into one turn, testbed pairing guard stays exact. Turn-open time parsed *locally* from the first new transcript line to compute `duration_ms`; the timestamp string never reaches the wire, so the single-bound-string claim holds |
| Subagent contingency (Q3) | If measurement shows sidechain usage in the parent transcript: **partition by a sidechain discriminator** (bind one bool, e.g. `isSidechain`); parent turn sums exclude sidechain lines, `SubagentStop` records carry them. Round-1 SubagentStop decision stands. INV-2 cost: a bool, not a string |
| Cache counts | **Core issue adds `<model>.cache_creation_tokens` / `<model>.cache_read_tokens`** composite keys beside the existing ones — same upsert path. Cache-aware pricing in the backend is an optional follow-up, not blocking |
| Cross-repo filing (Q5) | **One issue, openbox-core only** — backend and fe confirmed no-change (evidence below) |
| Attestation/spans (Q4) | **Closed by scout**: nothing in `adapters/common/git/`, `actions/`, `cli/` reads activity events; dev sessions write no spans rows. The activity route touches only `governance_events` |

#### New evidence from openbox-core

- **Dedupe absorbs crash re-emission.** Same (agent, workflow, run, activity_id,
  event_type) returns the cached verdict, no second row
  (`internal/services/activities/governance/validation.go:75-216`). The Phase 02
  cursor's over-report-on-crash direction is server-corrected — stronger than
  round 1 knew.
- **Tool-metric pollution, with mechanism**: `ExtractToolMetric` accepts both
  activity halves with any non-empty `activity_type` (`errors.go:301-323`);
  Started increments `tool.<name>.total`, Completed increments success/failed +
  latency (`errors.go:118-135`). Without exclusion, `llm_completion` appears as a
  tool with call counts and latency percentiles. Turn pairs also increment
  `activity_event_count` (`invocation.go:80`) — acceptable, generic counter.
- Turn events are guardrail-eligible: stage-0 reads `activity_input` on Started,
  stage-1 reads `activity_output` on Completed (`governance_workflow.go:424-427`,
  `guardrail.go:180-196`) — token spend is policy/guardrail-visible as round 1
  intended.
- `llm_completion` already exists in core's vocabulary as
  `SemanticTypeLLMCompletion` (`internal/content/session.go:105`) — the
  `activity_type` choice is name-consistent across runtimes.
- Started's timestamp is consumer-invisible: duration comes only from
  `duration_ms` on Completed (`errors.go:314`); `updateEventCompletion` is
  span-path-only (`storage_event.go:137-167`). This is the basis for the
  both-from-Stop decision.
- `activity_id`/`activity_type`/`activity_input`/`activity_output`/`status` land
  in dedicated `governance_events` columns (`storage_event.go:310-341`).

#### New evidence from openbox-backend — the hop round 1 never traced

- **The read path is model-keyed, not token-keyed.** Token rollups are summed from
  `metric_type='model'` composite keys (`<model>.input_tokens`);
  `metric_type='token'` is a documented dead path
  (`src/modules/observability/observability.service.ts:216-241`). **Consequence:
  the core extractor must feed `UpdateModelUsageActivity`/`ModelMetric`, and the
  model id is the aggregation key** — model-less turns are dashboard-invisible
  unless the core issue specs an `unknown` bucket.
- `ModelKey(modelID, provider, suffix)` embeds the provider
  (`observability/model.go:58-79`) — deriving provider from the model id is
  required for the write, not a nice-to-have.
- **No `kind=developer` filtering** anywhere in the backend observability/dashboard
  read path — dev agents' metrics surface in the existing dashboards, keyed by
  agent_id, once core aggregates. **fe needs nothing; backend needs nothing.**
- Cost is backend-derived at read time from (model, input, output) via a static
  pricing table (`src/common/utils/calc-llm-cost.ts`); core carries a second Go
  table (`internal/content/const.go`). Both list Claude entries with mismatched
  key styles (`claude-sonnet-4` vs `claude-sonnet-4.5`) and neither lists current
  Claude Code default models — unknown ids likely price at 0. Note for the core
  issue; the client posture (never derive cost) is unaffected.

#### Consolidated action items (supersede round 1 where they overlap)

- [x] Revise phase files 01–06 to the turn-as-activity carrier; round-1 items
      stand: drop `EventTurnCompleted`-as-signal + `SignalReceived`, retarget the
      decision record, `Finops *bool`, pairing-guard extension, non-goal wording.
      *(Applied 2026-08-11 — all six phase files rewritten.)*
- [x] Phase 02: both halves from `Stop`; `activity_id = <session_id>:turn:<index>`
      with a byte-identity pin test; turn-open parsed locally for `duration_ms`;
      `SubagentStop` as second hook. *(Specced in phase-02.)*
- [x] Phase 02 execution: measured (32 real transcripts, 13,439 lines) —
      `isSidechain` present on every line, **true on none**, so the question is
      inconclusive rather than answered. The partition is implemented
      **unconditionally**, because it cannot double-count under any answer and its
      worst case is a subagent reporting nothing; a live run settles which world we
      are in. *(reports/measure-260811-transcript-turn-surface.md; Phase 06 step E
      surfaces the answer as a skip rather than a silent pass.)*
- [x] **FILED AND IMPLEMENTED** — [PROD-296](https://krnl-labs.atlassian.net/browse/PROD-296)
      (Shift-left epic PROD-156) → [openbox-core#125](https://github.com/OpenBox-AI/openbox-core/pull/125),
      all five asks shipped, CI green, awaiting merge to `develop`. Spec kept at
      [reports/core-issue-activity-usage-extractor.md](reports/core-issue-activity-usage-extractor.md).
      openbox-core issue (single, core-only): activity-based extractor for
      `activity_type == "llm_completion"` reading `activity_output` → **feed
      `ModelMetric`/`UpdateModelUsageActivity`** (model-keyed composites, provider
      derived from model id), add the two cache composite keys, exclude
      `llm_completion` from `ExtractToolMetric`, spec the model-absent `unknown`
      bucket, note the pricing-table gaps for current Claude Code model ids.
      *(Owned by phase-01 step 8.)*
- [x] Effort: ~14h stands (both-from-Stop simplification roughly offsets the
      sidechain partition work). *(plan.md frontmatter and phases table updated.)*

#### Remaining unresolved (empirical only — no open decisions)

1. Does sidechain usage appear in the parent transcript? Phase 02 measurement;
   the contingency is pre-decided either way.
2. Does `Stop` fire on tool-only turns? Phase 02 step 5, already planned.
