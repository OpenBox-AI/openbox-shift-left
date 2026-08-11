# openbox-core issue — activity-based usage extractor for `llm_completion`

**Status: NOT FILED.** The `gh` token in this environment cannot resolve
`OpenBox-AI/openbox-core` (private; no access), so this is the issue body ready to
file verbatim. Filing is an outward-facing action on another repo and is left to a
human with access. Once filed, link it from
`docs/adr/ADR-0014-turn-as-activity-and-identifier-allowlist.md` ("Not yet
proven") and `docs/architecture.md#assurance--what-the-evidence-proves`.

Target repo: `OpenBox-AI/openbox-core`
Suggested labels: `observability`, `finops`, `shift-left`

---

## Title

Aggregate token usage from `llm_completion` activity events (developer-runtime sessions)

## Body

### Context

`openbox-shift-left` now emits per-turn model + token usage for Coding Agent
(developer-runtime) sessions. Because dev sessions write **no `spans` rows**
(shift-left ADR-0013), the signal rides an activity pair instead of an
`llm_completion` span:

| | |
|---|---|
| wire types | `ActivityStarted` + `ActivityCompleted` (both already accept-listed) |
| `activity_type` | `llm_completion` — the same name as `SemanticTypeLLMCompletion` (`internal/content/session.go:105`) |
| `activity_id` | `<session_id>:turn:<index>`, or `<session_id>:agent:<agent_id>:turn:<index>` for a subagent's turn; `<session_id>:usage:rollup` for Codex's session rollup |
| `activity_output` (Completed half only) | mirrors the LLM span's `response_body` — see below |

```json
{
  "model": "claude-opus-4-8",
  "usage": {
    "input_tokens": 1204,
    "output_tokens": 318,
    "cache_creation_input_tokens": 4096,
    "cache_read_input_tokens": 58210
  }
}
```

Both `model` and `usage` are optional (a turn the projection could not measure is
still a real turn; zero-filling would claim it spent nothing). `activity_output`
carries **four numbers and one identifier and nothing else** — by design and
enforced by a test on the shift-left side, since core runs Guardrails stage 1 and
OPA over this field.

Ingest needs no change: these are stock accept-listed types and core already
stores `activity_id` / `activity_type` / `activity_output` into dedicated
`governance_events` columns (`activities/governance/storage_event.go:310-341`).
**Aggregation is what is missing** — every usage-aggregation path today reads
spans, so this data lands in the table and reaches no dashboard.

### What is missing today

- `AggregateTokensFromSpans` and `ExtractModelMetrics` read
  `input.Payload.Spans` (`internal/services/observability_workflow.go:63,100`),
  both gated on `detectLLMProvider` → `span.GetHTTPURL()`
  (`activities/observability/invocation.go:129-131`, `model.go:173-190`). A dev
  session has no span and no HTTP URL, so neither path can ever fire.
- `ExtractToolMetric` reads only `activity_type`, `duration_ms` and `status`
  (`activities/observability/errors.go:301-323`) — it never looks at
  `activity_output`.

### Asks

**1. New extractor: model + token metrics from `llm_completion` activities.**

Select events where `activity_type == "llm_completion"` and `event_type ==
"ActivityCompleted"`, read `activity_output`, and feed
`UpdateModelUsageActivity` / `ModelMetric` — **not** `metric_type='token'`.

This is load-bearing: the backend sums token rollups only from
`metric_type='model'` composite keys (`<model>.input_tokens`), and
`metric_type='token'` is a documented dead path
(`openbox-backend src/modules/observability/observability.service.ts:216-241`).
An extractor that writes token metrics would store numbers no dashboard reads.

Wire it beside `ExtractModelMetrics` in `observability_workflow.go`.

**2. Derive the provider from the model id.**

`ModelKey(modelID, provider, suffix)` embeds the provider
(`observability/model.go:58-79`), so the write needs one. An activity has no
`http_url`, so `detectLLMProvider` cannot be reused — derive from the model id
prefix (`claude-*` → anthropic, `gpt-*`/`o*` → openai, `gemini-*` → google, …).
Required for the write, not a nice-to-have.

**3. Add the two cache composite keys.**

`<model>.cache_creation_tokens` and `<model>.cache_read_tokens`, beside the
existing `<model>.input_tokens` / `<model>.output_tokens`, on the same upsert
path. Note the span parser drops both cache fields today
(`invocation.go:139-157`, `model.go:196-202`) — this extractor is new code, so it
can carry all four counts from the start rather than inheriting that loss.
Cache-aware pricing in the backend is an optional follow-up, not blocking.

**4. Exclude `llm_completion` from `ExtractToolMetric`.**

`ExtractToolMetric` accepts both activity halves with any non-empty
`activity_type` (`errors.go:301-323`): Started increments `tool.<name>.total`,
Completed increments success/failed + latency (`errors.go:118-135`). Without an
exclusion, `llm_completion` shows up in the dashboards **as a tool**, with call
counts and latency percentiles — a model turn reported as a tool invocation.

(Turn pairs also increment the generic `activity_event_count`,
`invocation.go:80`. That is fine — they are activities.)

**5. Bucket model-less rows under `unknown`.**

`model` is absent when the transcript window named none. shift-left deliberately
does **not** back-fill it from the session's `SessionStart` model, because
attributing tokens to a model that may not have spent them is a fabricated
number. Since the model id is the aggregation key, those rows are invisible
unless the extractor buckets them — e.g. `unknown.input_tokens`. Dropping them
silently is the failure mode to avoid.

### Note, not an ask: pricing-table gaps

Cost is derived at read time from (model, input, output) — backend
`src/common/utils/calc-llm-cost.ts`, plus a second Go table in
`internal/content/const.go`. The two disagree on key style (`claude-sonnet-4` vs
`claude-sonnet-4.5`) and **neither lists the current Claude Code default models**,
so dev-session turns will likely price at 0 until the tables are updated. Flagged
here because this feature is the first thing to surface it at volume. shift-left
deliberately never derives cost client-side, so nothing on that side changes.

### Cross-repo scope

- **openbox-core**: this issue.
- **openbox-backend**: no change. The observability/dashboard read path has no
  `kind=developer` filtering anywhere, so dev agents' metrics surface in the
  existing dashboards keyed by `agent_id` once core aggregates.
- **openbox-fe**: no change. It renders only pre-aggregated API data
  (`kpi-cards.tsx:21`, `model-usage-chart.tsx:100`, `cost-analytics.tsx:14`).

### References

- shift-left ADR-0014 — `docs/adr/ADR-0014-turn-as-activity-and-identifier-allowlist.md`
- shift-left wire mapping — `contracts/dev-event/MAPPING.md` §2 "The turn pair"
- Golden wire bytes — `client/testdata/golden/activity_turn_*.json`
