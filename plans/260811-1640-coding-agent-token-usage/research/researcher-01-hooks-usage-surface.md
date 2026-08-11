# Research: what Claude Code hooks can and cannot tell us about model + token usage

Source: https://code.claude.com/docs/en/hooks (fetched 2026-08-11)
Corroborated against: real OpenBox event dumps from session
`6a66bfba-5ce4-481d-8ef4-f0144cf642b1` (130 rows) and this repo's source.

## Finding 1 — hooks carry NO token usage. At all.

Every hook input is limited to the common fields plus a few event-specific ones:

```
session_id · prompt_id · transcript_path · cwd · permission_mode
effort{level} · hook_event_name · agent_id · agent_type
```

No hook carries token counts, usage, cost, or billing data. Confirmed across all
~30 documented hook events. There is no usage-bearing hook to subscribe to, so
**the transcript JSONL is the only source** — which is exactly what this repo
already does (see the scout report).

## Finding 2 — model is available on SessionStart only, and is not guaranteed

Quoted from the doc:

> Only `SessionStart` hooks can receive a `model` field, and it is not guaranteed
> to be present. There is no `$CLAUDE_MODEL` environment variable.

So model-per-hook does not exist. A session that switches model mid-run, or where
Claude Code omits the field, is unattributable from hook input alone.

**Empirically confirmed.** Across 130 real event rows, `metadata.model` appears on
exactly one event type:

| event_type | metadata.model present |
|---|---|
| WorkflowStarted (SessionStart) | **yes — 1 of 1** |
| SignalReceived (PromptSubmitted) | no |
| ActivityStarted / ActivityCompleted | no |
| WorkflowCompleted (SessionEnded) | no |

The repo binds `Model` on its hook-event struct for every hook
(`adapters/claude-code/hookevent.go:70`) and maps it into metadata
(`mapper.go:435`), but only SessionStart ever populates it. The wiring is right;
the source is thin.

## Finding 3 — token usage is entirely absent from live events today

Same 130 rows, searched for `tokens`, `token`, `usage`, `cost`, `input_tokens` in
metadata: **absent on every event type**. Cause is not a bug — `ResolveFinops()`
is off by default, so `readTranscriptUsage` never runs. The capability exists and
has never emitted.

## Finding 4 — turn boundaries exist as hooks, and this repo subscribes to none

Candidate turn/usage-relevant hooks the docs describe, none of which this repo
installs:

| Hook | Fires | Useful for |
|---|---|---|
| `Stop` | Claude finishes responding | **per-turn boundary** — the natural per-turn usage trigger |
| `SubagentStop` | a subagent finishes | attributing Task/Explore usage separately |
| `PreCompact` / `PostCompact` | around compaction | `current_context_size`, `old/new_context_size` |
| `StopFailure` | turn ended on API error | `error_type` (rate_limit, overloaded…) |

`Stop` input: `last_assistant_message`, `stop_reason`. Note both are content —
neither is needed for usage, and neither should be read.

`PostCompact` is the one place a hook exposes a *size* number
(`old_context_size` / `new_context_size`). Not token usage, but adjacent, and
free of transcript parsing. Out of scope here; worth noting.

## Finding 5 — the AI-Agent shape cannot be reproduced

The agent-runtime side carries model + usage inside a span:
`semantic_type: "llm_completion"`, `response_body.model`,
`response_body.usage.{prompt_tokens,completion_tokens,total_tokens}` — one span
per LLM call.

Dev sessions have **no spans**: ADR-0013 (2026-08-11) retired the span layer and
`client/hookspan.go`/`spanbuilder.go` are deleted. So parity must be of
**information**, not of shape: model + usage on event metadata, not on an
`llm_completion` span. Any dashboard or query that reads spans to find LLM usage
will not see developer sessions regardless of what this plan ships.

Per-LLM-call granularity is also unavailable: hooks fire per turn and per tool,
not per model call. A turn containing several model round-trips reports as one
turn. Per-turn is the finest fidelity the hook surface permits.

## Finding 6 — what the transcript actually carries

Per assistant turn, one JSONL line: `message.usage` with `input_tokens`,
`output_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`. The
model id lives on `message.model` (a string). `costUSD` is **absent** in current
transcripts — verified in this repo's own code comments, so cost cannot be read
and must not be derived (a pricing table would fabricate the number).

## Open questions

1. Does `Stop` fire on every turn including tool-only turns, or only when Claude
   emits final text? Determines whether a Stop-driven cursor sees every turn.
2. Is `message.model` present on every assistant line, or only the first?
3. Codex has no documented `Stop` equivalent — what is its turn boundary?
   (Its `usage.go` reads a rollout `total_token_usage`, i.e. a rollup.)
4. Does openbox-core/openbox-fe read `metadata.tokens`, or only span-derived
   usage? If the latter, dev usage lands somewhere nothing renders.
