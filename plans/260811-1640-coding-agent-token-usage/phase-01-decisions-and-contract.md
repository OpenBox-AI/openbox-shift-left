# Phase 01 — Contract: turn-as-activity, that decision, and the core issue

## Context links

- Parent: [plan.md](plan.md) · decisions settled in its Validation Summary (rounds 1–2)
- Evidence: [research/researcher-01-hooks-usage-surface.md](research/researcher-01-hooks-usage-surface.md),
  [scout/scout-01-existing-finops-surface.md](scout/scout-01-existing-finops-surface.md)
- Precedent: `that decision-*` (span layer retired; tool call = activity),
`that decision-*` (base wire unification)
- Constraint: `CLAUDE.md` — INV-2, INV-8; cross-repo additions need an issue, not a fork

## Overview

- Date: 2026-08-11 (revised after validation round 2)
- Description: extend the dev-event contract with the turn pair, widen `Tokens`,
write that decision that records the INV-2 allowlist, and file the one core-side
consumer issue. The consumer check that used to gate this phase is **answered**:
core aggregates usage from spans only, the backend reads model-keyed composite
metrics, and neither reads anything a dev session emits today — the extractor
issue is the fix, and fe/backend need no change.
- Priority: P1 (fixes the contract every later phase emits against)
- Implementation status: **complete**
- Review status: reviewed (code-reviewer, 2026-08-11) — findings applied

## Key insights

- **The carrier is the activity pair, not a signal.** `ActivityStarted`/
  `ActivityCompleted` are already accept-listed (`client/payload.go:82-88`, citing
  core `internal/api/governance.go:273-286`), so INV-8 needs nothing new. The
  `llm_completion` name matches core's existing `SemanticTypeLLMCompletion`
  (`openbox-core internal/content/session.go:105`) — one vocabulary across both
  runtimes.
- **The model id is the aggregation key, not garnish.** The backend sums tokens
  only from `metric_type='model'` composite keys (`<model>.input_tokens`,
  `observability.service.ts:216-241`); `metric_type='token'` is a documented dead
  path. The core issue must therefore feed `ModelMetric`/`UpdateModelUsageActivity`,
  and model-less turns need an `unknown` bucket or they are dashboard-invisible.
- `client.Tokens` today is `Input`/`Output`/`Total`, and the SessionEnd rollup
  **folds cache tokens into `Input`** (`adapters/claude-code/usage.go:137`).
  Widening changes that semantic: `Input` becomes pure input, cache counts ride
  their own fields, `Total` keeps meaning whole-throughput. Golden fixtures pin the
  bytes, so the change is visible, versioned, and deliberate.
- The turn's `activity_id` is a **different derivation** from the tool-call hash
  (`activityIDFor`, `client/payload.go:243-250`): deterministic
  `<session_id>:turn:<index>`, readable in stored rows, collision-free with
  `cc-act-<hex>`. Core treats the id as an opaque string (dedupe:
  `validation.go:96`), and re-emitting the same id+event_type returns the cached
  verdict — crash re-counting is absorbed server-side.
- that decision is load-bearing, not ceremony: `usage.go`'s stated guarantee is that
  content *cannot* enter memory. After this plan that sentence is false as written;
  the replacement is a curated allowlist a test enforces. Leaving the old claim in
  place would be exactly the self-overstatement CLAUDE.md forbids.

## Requirements

1. New event types `EventTurnStarted`/`EventTurnCompleted` (+ `AllEventTypes`),
   mapped in `wireTypeFor` to `wireActivityStarted`/`wireActivityCompleted`.
2. `activityLabel` returns `"llm_completion"` for both turn types (core's
   pass-through `activity_type` column; also what the core extractor keys on).
3. Widen `client.Tokens` with `CacheCreationInput`/`CacheRead` (`omitempty`);
   `Input` becomes pure input tokens; document `Total` = input+output+both caches.
4. `Model string` as a sibling field on `DevEvent` (identifier-class,
   `capStr`-bounded at the mapper), feeding both `activity_output` and
   `metadata.model`.
5. `TurnIndex *int` on `DevEvent` (spool-surviving) so both halves derive
   `activity_id = <session_id>:turn:<index>` — a `turnActivityIDFor` beside
   `activityIDFor`, pinned by a byte-identity test in the
   `client/approval_key_pin_test.go` manner.
6. `turnActivityOutput` builder: the Completed half carries
   `{"model": "...", "usage": {"input_tokens", "output_tokens",
   "cache_creation_input_tokens", "cache_read_input_tokens"}}` — the span
   `response_body` shape, numbers + one bounded string, nothing else. The Started
   half carries no `activity_input`.
7. Update `contracts/dev-event/schema/dev-event.schema.json` (+
   `x-schema-version` bump) and `MAPPING.md` (new § for the turn pair; extend §7's
   live-run checklist).
8. Write that decision (turn-as-activity + INV-2 identifier allowlist) and **file the
   openbox-core issue** with the round-2 spec.

## Architecture

```
Stop hook ──▶ per-turn usage ──▶ DevEvent{TurnStarted}   ──▶ wire ActivityStarted
                             └─▶ DevEvent{TurnCompleted} ──▶ wire ActivityCompleted
    both halves:  activity_id = <session_id>:turn:<index>   (TurnIndex, spooled)
                  activity_type = "llm_completion"          (activityLabel)
    completed:    activity_output = {model, usage{4 counts}} (turnActivityOutput)
                  duration_ms     = close − open, when open time is real
    started:      timestamp = locally-parsed turn-open time; no activity_input
```

Guardrail note (verified round 2): core runs stage-0 over `activity_input` on
Started and stage-1 over `activity_output` on Completed
(`governance_workflow.go:424-427`, `guardrail.go:180-196`) — token spend becomes
policy-visible, which is intended; the schema must stay numbers + model id.

## Related code files

| File | Change |
|---|---|
| `client/event.go` | `EventTurnStarted`/`EventTurnCompleted` + `AllEventTypes`; widen `Tokens`; `Model`, `TurnIndex` fields |
| `client/payload.go` | `wireTypeFor` cases; `activityLabel` case; `turnActivityIDFor`; `turnActivityOutput`; buildPayload switch arms |
| `client/approval_key_pin_test.go` (or sibling) | byte-identity pin for `turnActivityIDFor` |
| `contracts/dev-event/schema/dev-event.schema.json` | new event types + fields; `x-schema-version` bump |
| `contracts/dev-event/MAPPING.md` | new § for the turn pair; §7 checklist extension |
| the decision record | new |
| `client/testdata/golden/` | fixtures for both halves (Phase 03 asserts them) |

## Implementation steps

1. Add the two event types and `AllEventTypes` entries; extend `wireTypeFor` and
   `activityLabel` (turn events → `"llm_completion"`).
2. Widen `Tokens` (cache fields, `omitempty`); add `Model`, `TurnIndex` to
   `DevEvent`; keep old fixtures' bytes unchanged where fields are absent.
3. Add `turnActivityIDFor` = `SessionID + ":turn:" + TurnIndex`; buildPayload arms
   for both turn types (id on both halves; output+duration on Completed only);
   pin the derivation bytes.
4. `turnActivityOutput`: marshal model + four counts; `capStr` the model at the
   boundary, matching `metadata.model` (`mapper.go:435`).
5. Bump `SchemaVersion`; update the JSON schema and `MAPPING.md`.
6. Golden fixtures for both halves; `client` + conformance modules green.
7. Write that decision. State: what INV-2 guaranteed before, what it guarantees now
(exactly one egressing string, identifier-class, bounded, test-enforced), that
that decision stands unamended and this rides the activity shape it established,
and what is given up.
8. File the openbox-core issue: activity-based extractor for
`activity_type == "llm_completion"` reading `activity_output` → feed
`ModelMetric`/`UpdateModelUsageActivity` (provider derived from model id, since an
activity has no `http_url`); add `<model>.cache_creation_tokens` /
`<model>.cache_read_tokens` composite keys; exclude `llm_completion` from
`ExtractToolMetric`; `unknown` bucket for model-less rows; note the pricing-table
gaps for current Claude Code model ids. Link the issue from that decision and
`docs/architecture.md`'s assurance section.

## Outcome

**Implemented 2026-08-11.** All contract items landed: both event types + `AllEventTypes`, `wireTypeFor`/`activityLabel`, widened `Tokens` (with `Input` re-defined as pure), `Model`/`TurnIndex`/`AgentID`/`SessionRollup` on `DevEvent`, `turnActivityIDFor` + `turnActivityOutput`, schema **v1.1** (+ `x-changelog`) and `SchemaVersion` bumped, `MAPPING.md` §2 turn-pair section and §7 checklist items 8–14, five golden fixtures, byte-identity pins in `client/turn_key_pin_test.go`. Conformance + client green; existing golden bytes unchanged, so the wire change is purely additive apart from the deliberate `Tokens` semantic.

**The core issue is filed AND implemented** (2026-08-11, after `gh` access was fixed): [PROD-296](https://krnl-labs.atlassian.net/browse/PROD-296) under the Shift-left epic PROD-156, with all five asks shipped in [openbox-core#125](https://github.com/OpenBox-AI/openbox-core/pull/125) → `develop`, CI green, awaiting merge. openbox-core's CI hard-gates on a `PROD-\d+` key in the PR title and commits, so the ticket had to exist before the PR could pass.

## Todo list

- [x] `EventTurnStarted`/`EventTurnCompleted` + `AllEventTypes`
- [x] `wireTypeFor` + `activityLabel` cases
- [x] Widen `client.Tokens`; `Model` + `TurnIndex` on `DevEvent`
- [x] `turnActivityIDFor` + byte-identity pin test
- [x] `turnActivityOutput` (numbers + bounded model only)
- [x] Schema + `x-schema-version` + `MAPPING.md`
- [x] Golden fixtures for both halves
- [x] decision record written, cross-linked from `docs/architecture.md`
- [x] openbox-core issue filed with the round-2 spec
- [x] `cd contracts/dev-event/conformance && go test ./...` green

## Success criteria

- Both turn halves build, serialize to pinned golden bytes, and ride accept-listed
  wire types with `activity_type: "llm_completion"`.
- `Tokens` can express every number the transcript provides; `Input` is pure.
- `turnActivityIDFor` is pinned; the pair shares one id; no collision with
  `cc-act-*` is possible by construction.
- decision record exists and says out loud that INV-2's usage path is now an allowlist.
- The core issue is filed and linked; conformance + `client` tests green; no core
  change required for ingest (only for aggregation, which the issue owns).

## Risk assessment

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Fixture churn from the `Tokens` semantic change | high | low | Fixtures pin bytes; the change is the point — review the diff, bump the schema version |
| Turn id drifts from the pinned derivation later | low | high — dedupe + pairing break | Byte-identity pin test, same discipline as the approval key |
| `activity_output` grows a second string "while we're here" | medium | **high** | decision record names `model` as the sole entry; Phase 03's sentinel test fails on any other bound string |
| Core issue under-specifies and the extractor reads the wrong field | medium | high — write-only feature returns | Issue text carries the exact round-2 citations (read path, keys, exclusion) |

## Security considerations

- The model id is free-form provider text: `capStr`-bound it at the mapper
  boundary; never interpolate into shell/path/SQL.
- `activity_output` carries **numbers plus the model id only** — no
  `last_assistant_message`, no `stop_reason`, no prompt. Core guardrails/OPA will
  inspect it (verified), which is the intended policy visibility.
- INV-1 unaffected: no credential path is touched.
- that decision is the deliverable that keeps the repo's privacy claims true; write it
  against `docs/data-and-privacy.md` and let Phase 05 cross-check.

## Next steps

Phase 02 — wire `Stop` + `SubagentStop`, the hooks that will produce these events.
