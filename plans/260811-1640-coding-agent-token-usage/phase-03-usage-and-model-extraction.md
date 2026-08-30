# Phase 03 — Per-turn usage and model extraction

## Context links

- Parent: [plan.md](plan.md) · depends on Phase 01 (contract), Phase 02 (cursor + measurements)
- Existing parser: `adapters/claude-code/usage.go` (`transcriptLine`,
  `usageNumbers`, `readTranscriptUsage` — the SessionEnd rollup)
- The invariant at stake: [scout/scout-01-existing-finops-surface.md](scout/scout-01-existing-finops-surface.md) §"structurally content-proof"
- decision record from Phase 01 authorises the change made here

## Overview

- Date: 2026-08-11 (revised after validation round 2)
- Description: extend the transcript projection to per-turn window extraction —
  binding the model id (the first *egressing* string this projection has ever
  had), the line timestamp (parsed locally, never egressed), and, if Phase 02's
  measurement demands it, the sidechain discriminator — then build the
  `TurnStarted`/`TurnCompleted` pair and prove the narrowed content guarantee.
- Priority: P1
- Implementation status: **complete**
- Review status: reviewed (code-reviewer, 2026-08-11) — findings applied

## Key insights

- **This is the phase where the invariant changes.** `usage.go` today guarantees
  content cannot enter memory *because there is no field for it*. This phase adds
  fields, so the guarantee becomes a curated allowlist: `message.model` is the
  only string that reaches the wire (identifier-class, `capStr`-bounded);
  `timestamp` is bound, parsed to a `time.Time` for `duration_ms`, and discarded;
  `isSidechain` is a bool. A test proves no other field can land and that neither
  of the two non-model bindings appears in payload bytes.
- The existing sentinel test proves "no content anywhere". It must be rewritten to
  the narrower, true claim: sentinel content absent, model present, nothing else.
  A sentinel test that passes unchanged means the projection was never the
  guarantee.
- `message.model` may not be on every line of a window. Aggregate with
  last-non-empty-wins **inside the window**, never across windows, and treat
  absence as unknown rather than substituting the SessionStart model —
  attributing tokens to a model that may not have spent them is a fabricated
  number, the same class of error as a derived cost. (Round 2 sharpened the
  stakes: the model id is the backend's aggregation key, so a model-less window
  lands in the core issue's `unknown` bucket rather than under a guessed model.)
- **The rollup semantic changes with the widened `Tokens`.** Today cache tokens
  fold into `Input` (`usage.go:137`). After Phase 01, `Input` is pure and the two
  cache counts ride their own fields — in the per-turn records AND the retained
  SessionEnd rollup, or Phase 06's reconciliation (Σ per-turn == rollup) compares
  unlike quantities. Golden fixtures pin the change.
- A window may span several model calls (and, if Stop skips tool-only turns,
  several assistant turns). The per-turn number is a sum over the window — say so
  in the doc comment so nobody reads it as call-level.
- Sidechain partition (pre-decided contingency): if Phase 02 measured subagent
  usage in the parent transcript, bind `isSidechain` and exclude those lines from
  the main window's sums — `SubagentStop` records carry them. If the measurement
  says they never appear, do not bind the field at all.

## Requirements

1. `readTurnUsage(path string, from cursorPos) (window, next cursorPos, err)`
   reading only lines after the cursor; `window` = four summed counts, model
   (last-non-empty), open time (first parsable line timestamp), sidechain sums
   partitioned out when the discriminator is active.
2. Bind `message.model` as the single wire-bound string, `capStr`-bounded at the
   mapper.
3. All four token counts carried through to `activity_output` and the widened
   `Tokens`; `Input` pure in both per-turn and rollup paths.
4. Model absent ⇒ pair still emitted, model field omitted. Never back-filled.
5. Preserve every existing property: 64 MiB bound, skip-whole on oversize,
   best-effort per INV-3, cost never derived, off the hot path.
6. Sentinel test rewritten to prove the exact new claim, including
   timestamp-absent-from-payload and sidechain-bool-only.
7. Mapper builds the pair per the Phase 01 contract (id on both halves; output +
   `duration_ms` on Completed; Started timestamp = open time).

## Architecture

```go
// The projection allowlist, authorised by the decision record. Everything else in the
// transcript still has nowhere to land and is dropped by encoding/json.
type turnLine struct {
    Timestamp   string `json:"timestamp"`   // parsed → time, feeds duration_ms; NEVER egressed raw
    IsSidechain bool   `json:"isSidechain"` // partition discriminator (only if measurement demands it)
    Message *struct {
        Model string        `json:"model"`  // the ONE egressing string; capStr-bounded
        Usage *usageNumbers `json:"usage"`  // four counts (existing struct)
    } `json:"message"`
}
```

`content`, `text`, `thinking`, `tool_input`, `tool_result`, `cwd` remain unbound.
That residual property is what the sentinel test pins — now alongside proof that
`timestamp` and `isSidechain` never appear in built payload bytes.

Window aggregation: sum the four counts (sidechain lines excluded when
partitioned); model = last non-empty in window; open = first parsable timestamp.

## Related code files

| File | Change |
|---|---|
| `adapters/claude-code/usage.go` | `turnLine`, `readTurnUsage`, window aggregation; rollup un-folds cache counts |
| `adapters/claude-code/usage_test.go` | sentinel test narrowed; window cases; model-absent; sidechain partition |
| `adapters/claude-code/mapper.go` | build the `TurnStarted`/`TurnCompleted` pair; `capStr` on model |
| `adapters/claude-code/capabilities.go` | `telemetry.tokens` claim rewritten (per-turn, no longer "at SessionEnd") |
| `client/testdata/golden/` | golden bytes for both halves + updated rollup fixture |

## Implementation steps

1. Add `turnLine` beside `transcriptLine`; keep the SessionEnd rollup path working
   (it is Phase 06's reconciliation total), updating it to the un-folded `Tokens`
   semantics.
2. Implement `readTurnUsage` over the Phase 02 cursor position; return the next
   position so the caller advances only on success; consume complete lines only.
3. Wire the mapper: pair construction, `capStr` on model (matching
   `mapper.go:435`), `duration_ms` only when the open timestamp parsed.
4. Rewrite the sentinel test: transcript with sentinel strings in `content`,
   `thinking`, `tool_input`, `tool_result`, plus a real `model`, a real
   `timestamp`, and a sidechain line; assert the model arrives, every sentinel is
   absent from built payload bytes, the raw timestamp string is absent, and
   sidechain sums stayed out of the main window.
5. Per-turn tests: two windows, disjoint counts; model changing mid-session
   (window boundary respected); model absent; malformed line skipped;
   partially-written final line skipped; cache counts land in their own fields.
6. Update the `capabilities.go` claim to describe what is now true.
7. Golden fixtures for the pair and the changed rollup; `client` + adapter tests
   green.

## Outcome

**Implemented 2026-08-11.** `turnLine` (the allowlist: model egresses, timestamp is parsed and discarded, `isSidechain` is a bool), `readTurnUsage` over the cursor, `aggregateTurnWindowInto`, the rollup un-folded so both derivations are comparable, `MapTurn` building the pair with `capStr` on the model, and `capabilities.go` rewritten (plus a new `telemetry.model`).

**The sentinel test was rewritten to the narrower true claim** and now asserts four things: sentinel content from four field classes absent from the real signed wire body, the model id present, the raw transcript timestamp absent, and sidechain sums excluded from a main-thread turn. It runs with content-capture ON so the client's stripper cannot be what saves it.

**Changed during review:** the windowed read was slurp-and-truncate-at-64MiB, which would have reported a >64MiB window's head as turn N and folded its tail into turn N+1 — conserving total tokens while making two turns carry each other's numbers. It now streams in 4MiB chunks and accumulates, so the window is complete and memory stays bounded. Three tests cover it (multi-chunk aggregation, a line straddling a boundary, and a newline-less line past the cap).

## Todo list

- [x] `turnLine` — allowlist exactly as specced (one egressing string)
- [x] `readTurnUsage` honouring the cursor; window semantics documented
- [x] Four counts reach `activity_output` and widened `Tokens`; `Input` pure everywhere
- [x] Rollup path updated + fixture
- [x] `capStr` bound on model at the mapper
- [x] Sentinel test narrowed (content absent · model present · timestamp raw absent · bool-only sidechain)
- [x] Window, model-change, model-absent, malformed-line, sidechain tests
- [x] `capabilities.go` claim rewritten
- [x] Golden fixtures added
- [x] Confirm cost still absent (assert nil)

## Success criteria

- Two consecutive windows yield disjoint, correct counts in all four fields.
- The model that ran a window is attributed to that window's tokens; model-less
  windows emit without a model.
- Sentinel content from four transcript field classes never appears in payload
  bytes; the model does; the raw timestamp does not.
- With partition active, main-window sums exclude sidechain lines exactly.
- Cost asserted nil; oversize/malformed/partial transcripts still degrade quietly.

## Risk assessment

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| A second egressing string is added later "while we're here" | medium | **high** — the allowlist stops meaning anything | decision record names `model` as the sole entry; sentinel test fails on any other bound string reaching bytes |
| Timestamp string leaks to the wire via a careless refactor | low | medium | Sentinel test asserts its absence from payload bytes explicitly |
| Model mis-attributed on a mid-session switch | medium | medium | Last-non-empty within the window; never cross windows; tested |
| Tokens attributed to the wrong window | medium | high | Cursor tested in Phase 02; window tests here |
| Reconciliation breaks because rollup kept the old fold-in | medium | medium | Rollup updated in the same change; fixture pins it; Phase 06 asserts Σ == rollup |
| Silent regression of the content guarantee | low | **critical** | The sentinel test is the gate; a change that makes it pass trivially is a defect |

## Security considerations

- **The one change that matters in this whole plan.** After it, INV-2 for the
  usage path rests on an allowlist and a test rather than structural
  impossibility. The test is load-bearing, not supplementary.
- Model ids are provider-controlled free text. Bound the length; never
  interpolate into a shell, path, or SQL string.
- Transcript reads stay bounded and best-effort; a hostile transcript must not
  exhaust memory or fail a session.
- No prompt, completion, thinking block, tool input or tool result may become
  reachable. Four separate sentinels, one per field class, prove it — plus the
  timestamp-absence assertion for the locally-parsed binding.

## Next steps

Phase 04 — the same signal for Codex at the granularity its wired surface offers.
