# Measurement — the Claude Code transcript's turn/usage surface

Date: 2026-08-11 · Phase 02 step 6 · Claude Code 2.1.217–2.1.220

**Method.** Static scan of 32 real transcripts under `~/.claude/projects/*/*.jsonl`
(13,439 lines carrying the session-line shape; the 20 largest inspected in detail
for line types). This is **observation of real provider output**, not a live
headless run: it settles the transcript's shape and content, and it does **not**
settle when the `Stop` hook fires or which transcript `SubagentStop` names. Those
two remain open and are Phase 06's job.

## (a) Does `Stop` fire on tool-only turns?

**Not resolved. Partial evidence that a `Stop` window spans many model calls.**

The transcript records a `Stop` firing as
`{"type":"system","subtype":"stop_hook_summary", …}` — 117 such records across
the sample, carrying `hookCount`, `hookInfos`, `stopReason`, `preventedContinuation`.

Per-session counts diverge sharply from both prompts and model calls:

| session | assistant lines with `message.usage` | `stop_hook_summary` records | user prompt lines |
|---|---|---|---|
| 72929557… | 945 | 18 | 52 |
| 06b59e53… | 930 | 18 | 49 |
| cf8a1790… | 508 | 6 | 12 |
| f49ee1e8… | 600 | 10 | 16 |

Two things follow, one solid and one not:

- **Solid: a `Stop` window is a sum over many model calls, not one.** 945 usage
  lines against 18 `Stop` records is ~52 model calls per window. The per-turn
  numbers this feature emits are therefore *window sums*, and the doc comments
  say so rather than implying call-level granularity. Per-LLM-call attribution is
  unreachable through hooks, as the plan states.
- **Not solid: the cadence.** These records only appear when a Stop hook was
  installed AND produced output (`hasOutput: true`), so the count is a floor on
  firings, not the firing rate. Whether `Stop` fires on a tool-only turn cannot
  be read off this data.

**Consequence for the design: none.** The window sum is exact regardless of
cadence — whatever `Stop` skips folds into the next window, and the cursor
guarantees no byte is read twice. The reconciliation assertion (Σ per-turn ==
SessionEnd rollup) holds either way, which is why it is the assertion that
matters.

## (b) Does subagent usage appear in the parent transcript?

**The field exists on every line and was `false` on every line. The question is
therefore NOT resolved, and the design does not depend on resolving it.**

| | count |
|---|---|
| lines carrying `isSidechain` | 13,439 (every session line) |
| `isSidechain: true` | **0** |
| `isSidechain: false` | 13,439 |
| sessions containing any `isSidechain: true` line | 0 of 32 |

No session in the sample produced sidechain lines in its parent transcript. That
is consistent with two different worlds — subagent lines never land in the
parent, or none of these 32 sessions spawned a subagent whose lines would — and
this data cannot distinguish them.

**Decision taken under that uncertainty: partition unconditionally.** `isSidechain`
is bound (one bool, present on every line, INV-2 cost of a boolean), the main
thread's window sums `isSidechain == false` lines only, and `SubagentStop`'s
window sums `isSidechain == true` lines only.

The asymmetry is deliberate — the two failure directions are not equally bad:

| world | outcome under this design |
|---|---|
| sidechain lines DO appear in the parent | exact: parent excludes them, `SubagentStop` carries them, sum unchanged |
| sidechain lines never appear in the parent | partition is a no-op for the parent; subagent records report nothing |
| `SubagentStop` names a separate file whose lines are `isSidechain: false` | subagent **under-reports** (reports 0); the parent's own sum is still exact |

Under-reporting a subagent is a documented gap. Double-counting inflates the
numbers that feed cost dashboards — the failure this feature would be worthless
for having. The partition cannot double-count in any of the three worlds, so it
is the direction chosen.

It also makes the reconciliation exact by construction when both hooks read one
file: the SessionEnd rollup sums **all** usage lines, so
Σ(main, non-sidechain) + Σ(subagent, sidechain) == Σ(all). Phase 06 asserts it,
and a mismatch is the signal that world 3 is the real one.

## (c) `SubagentStop`'s payload — which transcript does `transcript_path` name?

**Not resolved by static analysis.** It needs a live session that spawns a
subagent; carried into Phase 06. The design above is safe under either answer,
which is why it did not block.

`agent_id`/`agent_type` are already known to ride every hook payload fired inside
a subagent (verified against claude 2.1.220, cited in `hookevent.go`), so
attribution does not depend on this.

## (d) `message.usage` — the numbers actually available

All four counts are present on **every** usage line (4,784 of 4,784):

```json
{"input_tokens": 2, "cache_creation_input_tokens": 49751,
 "cache_read_input_tokens": 29282, "output_tokens": 1804,
 "service_tier": "standard", "inference_geo": "not_available", "speed": "standard",
 "cache_creation": {"ephemeral_1h_input_tokens": 49751, "ephemeral_5m_input_tokens": 0},
 "server_tool_use": {"web_search_requests": 0, "web_fetch_requests": 0},
 "iterations": [ {...per-call breakdown...} ]}
```

Two siblings are deliberately **not** bound, and both would be bugs if they were:

- `cache_creation.ephemeral_*` sums to `cache_creation_input_tokens` — binding it
  and adding it would double-count the cache-creation total.
- `iterations[]` is a per-model-call breakdown of the same line. It is the only
  place call-level numbers exist, and summing it alongside the top-level counts
  would double-count the whole line. (It is also the one thing that could
  eventually give sub-turn granularity — noted, not pursued: it is per *line*,
  not per turn, so it does not change what a hook can attribute.)

`service_tier`, `inference_geo`, `speed` are strings and stay unbound: the
allowlist admits exactly one string, and it is the model id.

## (e) `message.model` — the one egressing string

Present on **100% of usage lines** in the sample (0 usage lines lacked it), so the
`unknown` bucket the core-side issue specs should be rare in practice. The
contract still tolerates absence, because "rare" is not "never" and back-filling
from the session model would fabricate an attribution.

Values observed: `claude-opus-5` (2060), `claude-fable-5` (1726),
`claude-opus-4-8` (994), and **`<synthetic>` (4)**.

`<synthetic>` is worth naming: it is a real value in the model field that is not a
real model, and 5 lines carried it *with* usage. It is passed through unchanged —
filtering it would drop real tokens, and rewriting it would fabricate an
attribution. Server-side it becomes its own model key, which is the honest
outcome: those tokens exist and belong to no priced model. This is a second
instance of the pricing-table gap already flagged for the core issue: none of
`claude-opus-5`, `claude-fable-5`, `claude-opus-4-8` or `<synthetic>` appears in
either cost table, so dev-session turns will price at 0 until those tables are
updated.

## Still open, carried to Phase 06

1. `Stop`'s actual firing cadence — and specifically whether a tool-only turn
   produces one. Window sums are exact either way.
2. Which transcript `SubagentStop`'s `transcript_path` names, and whether its
   lines carry `isSidechain: true`. Determines whether subagent records report
   real numbers or zero; cannot cause a double-count either way.
3. Per-`Stop` transcript-parse cost with the cursor in place, against a real
   session under the hook's 5s timeout.
