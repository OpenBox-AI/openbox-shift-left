# Report — per-turn model and token usage (ADR-0014)

Date 2026-08-11 · plan `260811-1640-coding-agent-token-usage` · branch
`fix/tier2-duplicate-activity-started` · mode `/ak:cook --auto`

## Outcome

Phases 01–05 implemented, reviewed, green. **Phase 06's assertions are written but
have not run**, so by this repo's own rule the feature is implemented, not
verified. Nothing else in the plan was cut.

Coding-agent sessions now emit which model spent how many tokens, per turn. A turn
rides an activity pair (`ActivityStarted`/`ActivityCompleted`, `activity_type:
llm_completion`, `{model, usage{4 counts}}` in `activity_output`) — the AI-Agent
`llm_completion` span's `response_body` shape on the activity carrier, because dev
sessions write no spans (ADR-0013). Both wire types were already accept-listed, so
INV-8 needed nothing.

## What landed

| Area | Change |
|---|---|
| Contract | `TurnStarted`/`TurnCompleted`; `Tokens` + `cache_creation_input`/`cache_read` with **`input` re-defined as pure**; `Model`/`TurnIndex`/`AgentID`/`SessionRollup` on `DevEvent`; schema **1.0 → 1.1** with `x-changelog` |
| Wire | `turnActivityIDFor` (`<session>[:agent:<id>]:turn:<n>`, or `:usage:rollup`), `turnActivityOutput`, `activityLabel` → `llm_completion`; 5 new golden fixtures, existing bytes untouched |
| Claude Code | `Stop` + `SubagentStop` hooks; `hookflow.TurnCursor` (offset+index, agent-scoped); `turnLine` allowlist; streaming `readTurnUsage`; `MapTurn`; rollup un-folded |
| Codex | `MapUsageRollup` at SessionEnd; four-count mapping (**inverse** arithmetic — see below); model from `turn_context.payload.model` |
| Posture | `Finops` `bool` → `*bool`, default **flipped ON**; posture record + resolver pinned to each other |
| Docs | ADR-0014; `data-and-privacy.md` usage section; `MAPPING.md` §2 + §7 items 8–14; `architecture.md` 4 new limits; `COVERAGE.md`, both adapter READMEs, `README.md`, `CLAUDE.md` |
| Testbed | `28-usage.sh`; `40-approvals` step G per activity kind; `20-capture` counts scoped |

## Verification

All 11 modules: `go test` ✓ · `go vet` ✓ · `gofmt` ✓ · `-race` ✓ on
`claude-code`, `hookflow`, `client`. `go build ./cli/...` ✓. Testbed scripts
syntax-clean.

New tests worth naming: `TestRunHook_StopEmitsOneDisjointPairPerFiring` (three
firings ⇒ three disjoint pairs, contiguous indexes), the rewritten
`TestFinops_NoContentOnWire`, `TestTurnActivityIDIsPinned`,
`TestTurnAndRollupShareOneWireShape`, `TestResolveFinops_DefaultOn`,
`TestPluginHooksMatchEngineVocabulary`.

**Not verified:** anything about core's ingest or the backend's read path. Both were
established by reading those repos. `testbed/run-all.sh usage` against a live stack
is what settles it; `MAPPING.md` §7 items 8–14 list exactly what that run must
confirm.

## Decisions taken during implementation

- **The windowed read streams rather than truncates.** Review caught that capping
  the read at 64 MiB would report a large window's head as turn N and fold its tail
  into turn N+1 — total tokens conserved, two turns carrying each other's numbers.
  Now 4 MiB chunks accumulated into one window: complete, and memory still bounded.
  The trigger is realistic — enabling capture mid-session makes the first firing
  read the whole transcript to date as one window.
- **`SessionRollup` is an explicit flag**, not "a turn with no index". Inferring it
  would turn a Claude Code bug (index failed to set) into every turn collapsing onto
  one `activity_id`, which core would dedupe to a single row.
- **`deriveID` folds in `TurnIndex` and `AgentID`**, appended only when set so
  existing ids stay byte-identical. Without it, a main-thread turn and a subagent
  turn closing in the same nanosecond derive one `event_id`.
- **Codex's cache counts are subtracted, not added** — they are sub-counts of
  `input_tokens`, the inverse of Claude Code. Verified arithmetically against 12 real
  rollouts (`total_tokens == input + output` exactly), not assumed from the CC shape.
- **The sidechain partition is unconditional.** The measurement was inconclusive
  (field present on every line, true on none across 32 transcripts), so the
  direction that cannot double-count was chosen; its worst case is a subagent
  reporting nothing, which Phase 06 surfaces as a skip.
- **`ResolveFinops` keeps its name.** It now gates model identity too; renaming a
  config key is a user-visible break, so the widened meaning is documented at the
  definition instead.

## Two things I fixed that were already wrong

- `hookrun.go` described **content** capture as "default off = metadata-only", long
  after it flipped to on (2026-07-15).
- `20-capture.sh`'s `assert_ge "tool calls captured" 4` counted all activity rows.
  Turn pairs ride the same wire types, so it would have passed on two tool calls
  plus two turns. Now scoped to tool activities.

## Open, and whose call it is

1. **The openbox-core issue is written but NOT filed.** `gh` in this checkout
   cannot resolve the private `OpenBox-AI/openbox-core`; filing on another repo is
   outward-facing, so it is left to someone with access. Body ready to paste
   verbatim: [core-issue-activity-usage-extractor.md](core-issue-activity-usage-extractor.md).
   **Until it ships the feature is write-only** — the numbers are stored and
   queryable but reach no dashboard, and `llm_completion` additionally shows up
   under core's *tool* metrics.
2. **Phase 06 needs a live stack.** Not substitutable with unit tests.
3. **Both cost tables** (core's Go one and the backend's TS one) disagree on key
   style and list none of the current Claude Code models, so dev turns will price at
   0 until updated. Flagged in the core issue; the client posture (never derive cost)
   is unaffected.
4. `<synthetic>` appears as a real `message.model` value with real usage. Passed
   through unchanged — filtering would drop real tokens, rewriting would fabricate
   an attribution — so it becomes its own model key server-side.
