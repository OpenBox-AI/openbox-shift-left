# Scout: the finops surface that already exists

Scouted 2026-08-11 against `origin/main` @ 56411f2 (+ local fix branch).
Headline: **most of this feature is already built.** The gap is narrower and
different from what the task description assumes.

## What exists today

| Piece | Where | State |
|---|---|---|
| Transcript usage parser (claude-code) | `adapters/claude-code/usage.go` | built, 6.9 KB |
| Transcript usage parser (codex) | `adapters/codex/usage.go` | built, 11 KB |
| Wire carriers `Tokens` / `Cost` | `client/event.go` | built |
| Finops → metadata | `client/payload.go:324-329` (`m["tokens"]`, `m["cost"]`) | built |
| Opt-in flag | `devconfig.ResolveFinops()` | **default OFF** |
| SessionEnd trigger | `adapters/claude-code/hookrun.go:131-137` | built |
| Model → metadata | `hookevent.go:70`, `mapper.go:435` (`capStr`) | built |
| Declared capability | `capabilities.go:23` `telemetry.tokens` | claims opt-in SessionEnd rollup |

`capabilities.go:23`, verbatim:

> `telemetry.tokens` Supported: true — "opt-in (ResolveFinops, default off)
> transcript usage extraction at SessionEnd → client.Tokens/Cost; numbers-only
> projection parse (INV-2), off the hot path"

## The load-bearing constraint: usage.go is structurally content-proof

`usage.go`'s INV-2 argument is not filtering — it is the absence of any field for
content to land in:

```go
type usageNumbers struct {          // NO string fields, BY DESIGN
    InputTokens              int `json:"input_tokens"`
    OutputTokens             int `json:"output_tokens"`
    CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
    CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}
type transcriptLine struct {
    CostUSD *float64 `json:"costUSD"`
    Message *struct{ Usage *usageNumbers `json:"usage"` } `json:"message"`
}
```

Its own doc comment states the guarantee:

> every content-bearing field in the transcript … has nowhere to land — it is
> impossible for content to enter memory through this path

**`message.model` is a string.** Reading it adds the first string field to these
structs and converts the guarantee from *structurally impossible* to *audited
allowlist*. That is the central design decision of this plan, and the reason it
needs a decision record rather than a patch. A sentinel test (`usage_test.go`)
currently proves content-absence end-to-end and will need to prove the narrower
claim.

Other properties worth preserving: read bounded at `maxTranscriptBytes` (64 MiB,
skip-whole rather than truncate), best-effort per INV-3, cost never derived.

## Gaps, precisely

1. **No turn boundary.** Only 5 hooks are wired — `SessionStart`,
   `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `SessionEnd`
   (`hookevent.go:15-19`). No `Stop`. Per-turn usage has no trigger today.
2. **No incremental cursor** for usage. A Stop-driven parse must count only turns
   new since the last Stop or it double-counts. Prior art exists — the findings
   sink already implements a per-provider cursor (`hookflow/findings.go`).
3. **`client.Tokens` is too narrow.** `Input`/`Output`/`Total` only; the
   transcript's `cache_creation_input_tokens` / `cache_read_input_tokens` — the
   fields that actually drive Claude cost — have nowhere to go.
4. **No model↔usage binding.** Model is one string on SessionStart; usage is one
   rollup on SessionEnd. Nothing ties a token count to the model that spent it.
5. **Default off** ⇒ zero usage data in production today (confirmed empirically:
   130 live rows, no tokens/cost anywhere).
6. **INV-8 constraint on any new event.** Core's accept-list is
   `WorkflowStarted`, `WorkflowCompleted`, `SignalReceived`, `ActivityStarted`,
   `ActivityCompleted` (`client/payload.go:82-88`). A per-turn usage event must
   map onto one of those — `SignalReceived` with a new `signal_name` is the
   natural fit, mirroring how `PromptSubmitted` already rides it.

## Reuse candidates (do not rebuild)

- `hookflow/findings.go` cursor pattern → the per-turn transcript cursor.
- `client.Tokens`/`Cost` + `buildMetadata` → widen, don't replace.
- `capStr` in both mappers → already bounds a free-form model id.
- `usage.go` projection discipline → extend the struct, keep the technique.
- `hookflow` for anything provider-agnostic; both adapters must move together or
  they drift (CLAUDE.md's standing rule, and the exact failure the engine
  extraction was done to end).

## Files a change will touch

```
adapters/claude-code/{hookevent,mapper,hookrun,usage,capabilities,installer}.go
adapters/claude-code/plugin/hooks/hooks.json
adapters/codex/{hookevent,mapper,hookrun,usage,capabilities}.go
adapters/common/{devconfig/devconfig,devconfig/posture}.go
adapters/common/hookflow/  (cursor, if shared)
client/{event,payload}.go + client/testdata/golden/*
contracts/dev-event/{schema/dev-event.schema.json,MAPPING.md}
docs/{data-and-privacy,architecture}.md, the decision record*.md
testbed/
```
