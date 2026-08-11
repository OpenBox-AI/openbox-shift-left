# Phase 04 — Codex: session-rollup activity

## Context links

- Parent: [plan.md](plan.md) · depends on Phase 03
- Existing reader: `adapters/codex/usage.go` (rollout `token_count` lines;
  `total_token_usage` is a cumulative snapshot — last valid snapshot IS the rollup)
- Hook surface: `adapters/codex/hookevent.go:9-24` — Codex v0.145.0 exposes 11
  events; this adapter wires the Claude Code parity five, and **`Stop` exists but
  is deliberately unwired** (declared a non-goal there)
- Standing rule: `CLAUDE.md` — provider-agnostic logic lives in `hookflow`;
  adapters must move together or they drift
- Known-limit precedent: `docs/architecture.md#assurance` (Codex's hook cannot be
  mandated by `requirements.toml`)

## Overview

- Date: 2026-08-11 (revised after validation round 2)
- Description: emit one `llm_completion` activity pair per Codex session at
  SessionEnd, carrying the rollup total and the model — and document the per-turn
  limit as **deliberate scope, not impossibility**: Codex's `Stop` hook exists
  unwired, so per-turn is a future adapter change with a named upgrade path
  (wire `Stop` + delta-from-cumulative-total).
- Priority: P2
- Implementation status: **complete**
- Review status: reviewed (code-reviewer, 2026-08-11) — findings applied

## Key insights

- **The decided scope is SessionEnd granularity** (validation round 1, upheld in
  round 2). Do not wire Codex's `Stop` in this plan; do not fabricate turns. The
  honest sentence for `capabilities.go` and `docs/architecture.md` is: "per-turn
  usage is not wired for Codex (its `Stop` hook exists but is out of scope);
  usage arrives once per session".
- **No delta arithmetic is needed at SessionEnd.** `total_token_usage` is
  cumulative (`TokenUsageInfo::append_last_usage` does `add_assign` —
  protocol.rs @ rust-v0.145.0, already cited in `usage.go`), so the existing
  last-valid-snapshot read already IS the session rollup. The change here is the
  carrier (activity pair) and the model binding, not the aggregation.
- Codex's `TokenUsage` maps onto the widened `Tokens` directly: `input_tokens` →
  `Input` (pure), `cached_input_tokens` → `CacheRead`,
  `cache_write_input_tokens` → `CacheCreationInput`, `output_tokens` → `Output`.
  Verify from codex-rs source how `total_tokens` composes (and whether
  `reasoning_output_tokens` is a subset of `output_tokens`) before asserting
  `Total` — record the citation, do not guess.
- The model id is not on the `token_count` line. Locate it in the rollout
  (`session_meta` / turn-context payloads are the candidates), bind exactly one
  string with the same `capStr` discipline, and omit when absent — the core
  issue's `unknown` bucket handles it.
- The rollup pair needs a deterministic id that cannot collide with turn ids or
  tool ids: `activity_id = <session_id>:usage:rollup`, pinned like the others.
- The rollout is a richer document than Claude's transcript line, with more
  content-bearing siblings. The sentinel test must be written against Codex's
  real shape, not copied from Claude's.

## Requirements

1. At SessionEnd (existing hook, existing read path), emit the
   `TurnStarted`/`TurnCompleted` pair with `activity_type: "llm_completion"`,
   `activity_id = <session_id>:usage:rollup`, usage = the session rollup in all
   four counts, model when found.
2. Map Codex's cache fields onto the widened `Tokens`; stop folding them into
   `Input`; verify and document the `total_tokens` composition from source.
3. Extract the model id from the rollout with the single-string discipline and
   `capStr` bound; omit when absent.
4. Keep the SessionEnded `metadata.tokens` rollup emission working (reconciliation
   parity with Claude Code's Phase 03).
5. `capabilities.go` + `docs/architecture.md`: per-turn limit stated as deliberate
   scope with the upgrade path named; no doc may imply impossibility.
6. Reuse `hookflow.TurnCursor`? **Not needed** — SessionEnd fires once; no cursor,
   no Codex-local fork of one.
7. Parity test: both adapters emit byte-compatible wire shapes for the
   `llm_completion` pair (sibling of `conformance_parity_test.go`).

## Architecture

```
Claude Code:  N per-turn pairs        ids <session>:turn:<n>       (Stop hook, cursor)
Codex:        1 session-rollup pair   id  <session>:usage:rollup   (SessionEnd, no cursor)
              ↑ same wire carrier, same activity_type, same activity_output shape
```

The derivation difference stays inside each adapter's `usage.go`; nothing
provider-specific reaches `hookflow` or `client`.

## Related code files

| File | Change |
|---|---|
| `adapters/codex/usage.go` | four-count mapping (un-fold caches); model extraction from rollout |
| `adapters/codex/mapper.go` | build the rollup pair; `capStr` on model |
| `adapters/codex/hookrun.go` | SessionEnd branch emits the pair before the flush |
| `adapters/codex/usage_test.go` | Codex-shape sentinel test; mapping + composition tests |
| `adapters/codex/capabilities.go` | `telemetry.tokens` claim: per-session, limit + upgrade path |
| `docs/architecture.md` | assurance section: the Codex granularity limit |
| `client/testdata/golden/` | fixtures for the Codex rollup pair |

## Implementation steps

1. Verify from codex-rs @ rust-v0.145.0: `total_tokens` composition and whether
   `reasoning_output_tokens` ⊂ `output_tokens`; record citations in the phase
   report. Locate the model id's rollout home (`session_meta` first candidate).
2. Extend the rollout reader: bind the model (one string), map the four counts
   onto widened `Tokens` without the `Input` fold-in.
3. Emit the pair from the SessionEnd branch with the pinned
   `<session_id>:usage:rollup` id; add the pin test.
4. Guard the cumulative read as today (negative/decreasing snapshots already
   handled by last-valid-snapshot semantics; keep the nonNeg clamps).
5. Write the Codex sentinel test against the real rollout shape: sentinel strings
   in its content-bearing fields, a real model, assert model-in/sentinels-out of
   built payload bytes.
6. Update `capabilities.go` + `docs/architecture.md` with the honest limit and
   upgrade path; parity test for the wire shape.

## Outcome

**Implemented 2026-08-11.** `MapUsageRollup` emits one `llm_completion` pair at SessionEnd with `activity_id <session>:usage:rollup` (pinned), the four-count mapping, and the model bound from `turn_context.payload.model`. `capabilities.go` states the per-turn limit as scope with the upgrade path named, and its test now FAILS on the words "cannot"/"impossible".

**Step 1's verification found the mapping is the INVERSE of Claude Code's.** Codex's `cached_input_tokens`/`cache_write_input_tokens` are sub-counts already inside `input_tokens`, so the reader SUBTRACTS them to report pure input; adding them (the shape of the CC reader) would double-count the cache on every session. Evidence is arithmetic from 12 real rollouts in `~/.codex/sessions` plus the fixture: `total_tokens == input_tokens + output_tokens` exactly, so neither cache count contributes on top. `reasoning_output_tokens` stays unbound for the same reason. Model location was verified the same way: `payload.model` occurs exactly as often as `turn_context` lines do, so no other line type puts one there — which matters because `turn_context.payload` also holds `developer_instructions` and `cwd`.

**Parity is enforced, not asserted in prose.** The two adapters are separate Go modules and cannot import each other, so `TestTurnAndRollupShareOneWireShape` in `client/` compares the two payload shapes directly, backed by golden fixtures for both.

## Todo list

- [x] `total_tokens` composition + model location verified from source (citations)
- [x] Four-count mapping, `Input` un-folded
- [x] Model bound (single string, `capStr`), omitted when absent
- [x] Rollup pair emitted at SessionEnd; `<session_id>:usage:rollup` pinned
- [x] Codex sentinel test against its own payload shape
- [x] `capabilities.go` claim true for Codex (scope, not impossibility)
- [x] `docs/architecture.md` limit recorded with the upgrade path
- [x] Parity test: both adapters, one wire shape
- [x] Codex module green

## Success criteria

- A Codex session emits exactly one `llm_completion` pair carrying the rollup in
  all four counts, model when the rollout names one.
- No negative counts ever emitted; absent counts omitted, not zero-filled.
- Sentinel test proves no rollout content reaches the wire.
- The documented limit says "unwired by choice, upgrade path exists" — nothing
  stronger, nothing weaker.
- Both adapters green; parity test asserts one shape.

## Risk assessment

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `total_tokens` composition mis-assumed | medium | medium — wrong `Total` | Step 1 verifies from source before mapping; citation recorded |
| Model id not present in rollout | medium | low — `unknown` bucket exists | Omit-when-absent is the contract; core issue handles the bucket |
| Docs overstate ("Codex cannot do per-turn") | medium | medium — false claim, the repo's core failure mode | Wording fixed here: scope, not impossibility; `Stop` exists unwired |
| Adapters drift on the pair shape | medium | **high** | Parity test + shared `client` builders, the standing repo rule |
| Codex rollout schema changes | low | medium | Projection-only parse degrades to no-usage, never a crash |

## Security considerations

- The rollout has more content-bearing siblings than Claude's transcript; the
  sentinel test is written against the real shape for exactly that reason.
- Same single-egressing-string rule: model id only, `capStr`-bounded.
- Codex's hook cannot be mandated by `requirements.toml` (existing documented
  limit), so usage capture is best-effort for Codex in a way it is not for
  Claude Code. Do not let the docs imply otherwise.

## Next steps

Phase 05 — flip the default and tell users what now leaves their machine.
