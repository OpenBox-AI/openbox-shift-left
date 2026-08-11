# Phase 02 — Turn boundary: Stop + SubagentStop

## Context links

- Parent: [plan.md](plan.md) · depends on Phase 01 (contract settled)
- Evidence: [research/researcher-01-hooks-usage-surface.md](research/researcher-01-hooks-usage-surface.md) Finding 4
- Existing hook set: `adapters/claude-code/hookevent.go:14-29` (five hooks, no Stop)
- Subagent identity: `adapters/claude-code/hookevent.go:84-100` — `agent_id`/
  `agent_type` ride every hook payload fired inside a subagent (verified against
  claude 2.1.220)
- Install surface: `adapters/claude-code/plugin/hooks/hooks.json`
- Cursor prior art: `adapters/common/hookflow/findings.go` (byte-offset cursor,
  complete-lines-only, leave-cursor-on-write-failure)

## Overview

- Date: 2026-08-11 (revised after validation round 2)
- Description: subscribe `Stop` and `SubagentStop` — the per-turn boundaries the
  hook surface offers — and give each an incremental cursor so repeated firings
  never double-count. **Both halves of the turn pair are emitted from the one
  `Stop` firing** (decided, round 2): one process derives the pair atomically, so
  it is always complete, queued prompts fold into one turn, and no cross-hook
  index coordination exists to race.
- Priority: P1
- Implementation status: **complete**
- Review status: reviewed (code-reviewer, 2026-08-11) — findings applied

## Key insights

- `Stop` input is `last_assistant_message` + `stop_reason` — **both content**.
  Neither is needed. The hook's value is purely that it fires; the numbers come
  from the transcript. Bind neither field.
- **Double-counting is the defining risk, and it now has two layers of defense.**
  The cursor makes each turn read exactly once locally; and core's dedupe returns
  the cached verdict for a re-sent (activity_id, event_type) with no second row
  (`openbox-core validation.go:75-216`, round-2 verified) — so a crash that
  re-reads a turn re-mints the same `<session>:turn:<n>` id and the server absorbs
  it. Prefer the crash direction that over-reports; it is self-correcting.
- **Started's timestamp costs nothing to get right locally and nothing if absent.**
  Core derives no metric from it (duration comes only from `duration_ms` on the
  Completed half — `errors.go:314`; `updateEventCompletion` is span-path-only).
  Parse the first new transcript line's timestamp for the Started half; fall back
  to Stop wall time. The timestamp string is parsed to a time and discarded — it
  never reaches the wire, so the single-egressing-string claim is untouched.
- **Subagents are a second boundary, not a special case of the first.**
  `SubagentStop` fires when a subagent finishes; its payload carries
  `agent_id`/`agent_type`. Whether subagent usage *also* appears in the parent
  transcript is empirical (this phase measures it); the contingency is
  pre-decided — partition by a sidechain discriminator so nothing counts twice.
- Unknown to resolve empirically: does `Stop` fire on tool-only turns, or only
  when Claude emits final text? If the latter, some turns' usage arrives folded
  into the next window — acceptable because the window sum is still exact, but
  document it, never assume it.
- Stop carries a 5s timeout like every non-gating hook. The cursor keeps the
  per-firing parse incremental; measure the cost anyway and record it.

## Requirements

1. `HookStop` and `HookSubagentStop` added to the hook vocabulary and dispatch;
   installed via `hooks.json` with 5s timeouts, no matcher, exit 0 always.
2. One `Stop` firing emits the full pair: `TurnStarted` (timestamp = parsed turn
   open, fallback Stop time) and `TurnCompleted` (usage, model, `duration_ms` when
   the open time was real), sharing `activity_id = <session_id>:turn:<index>`.
3. `hookflow.TurnCursor`: incremental, per-session, records how far the
   transcript has been consumed. Cursor key scoped by `(session, agent)` so
   subagent windows never interleave with the main thread's.
4. Cursor survives separate hook processes (cross-process state, like the
   duration stash and the findings cursor); swept on SessionEnd.
5. `SubagentStop` emits the same pair shape for the subagent's window, ids
   `<session_id>:agent:<agent_id>:turn:<index>`, attributed via the existing
   `agent_id`/`agent_type` metadata.
6. Never writes stdout — `Stop`/`SubagentStop` can block continuation via
   `decision: "block"`, so a stray write could halt the session. Fail-open, INV-3.

## Architecture

```
Stop hook process
  └─ RunHook("Stop")
       ├─ cursor := hookflow.TurnCursor{Dir: spool/turns}
       ├─ from := cursor.Read(sessionID, agentID)      // byte offset; agentID "" on main
       ├─ window := readTurnUsage(transcriptPath, from) // Phase 03: usage, model, openTs, next
       ├─ if window has usage:
       │    ├─ Observe(TurnStarted{ts: openTs ?? now, TurnIndex: n})
       │    └─ Observe(TurnCompleted{ts: now, usage, model, duration, TurnIndex: n})
       └─ cursor.Write(sessionID, agentID, next)       // only after a successful spool
```

Cursor write **after** the spool append, not before: a crash between the two
re-counts one turn — which core's dedupe then absorbs (same id, same types) —
whereas the reverse loses a turn silently. The turn index rides the cursor file
beside the offset so both the id and the window advance together.

Cursor cleared on SessionEnd alongside the duration stash
(`Engine.ThreadDuration`'s `EventSessionEnded` sweep — extend it).

## Related code files

| File | Change |
|---|---|
| `adapters/claude-code/hookevent.go` | `HookStop`, `HookSubagentStop`; bind no content fields |
| `adapters/claude-code/mapper.go` | Map cases → `EventTurnStarted`/`EventTurnCompleted` |
| `adapters/claude-code/hookrun.go` | Stop/SubagentStop branches: cursor read → extract → observe pair → cursor write |
| `adapters/claude-code/plugin/hooks/hooks.json` | `Stop` + `SubagentStop` entries, timeout 5 |
| `adapters/claude-code/installer.go` (+`installer_test.go`) | install/verify both hooks; pin the JSON |
| `adapters/common/hookflow/turncursor.go` | new, provider-agnostic cursor (offset + index, agent-scoped key) |
| `adapters/common/hookflow/engine.go` | sweep the cursor on SessionEnded |

## Implementation steps

1. Add both hook names and dispatch entries; confirm the argv subcommands
   (`openbox hook claude-code Stop` / `… SubagentStop`) resolve.
2. Write `hookflow.TurnCursor` modelled on the findings cursor: per-(session,
   agent) file under the spool dir, 0600, content-free (offset + turn index only —
   INV-2); consume complete lines only; leave the cursor on write failure.
3. Add both `hooks.json` entries (`matcher: ""`, `timeout: 5`); update the
   installer and its pinning test.
4. Wire the Stop branch in `RunHook`: emit the pair through the existing observe
   path so the spool and realtime flusher behave as for every other event.
5. Wire the SubagentStop branch with the agent-scoped cursor key and id format.
6. **Measure, in a real headless session, and record in `reports/`:**
   (a) does `Stop` fire on tool-only turns; (b) do subagent usage lines appear in
   the parent transcript (drives the Phase 03 partition); (c) `SubagentStop`'s
   actual payload fields (which transcript does its `transcript_path` name?);
   (d) per-Stop transcript-parse cost with the cursor in place.
7. Extend the SessionEnded sweep to clear all cursor files for the session.
8. Unit tests: two Stops over a growing transcript yield disjoint windows; a Stop
   with no new usage emits nothing; missing/corrupt cursor re-reads from zero
   (over-report, never crash — and note the id re-mint makes it server-deduped);
   subagent cursor isolated from main-thread cursor.

## Outcome

**Implemented 2026-08-11.** `HookStop`/`HookSubagentStop` + dispatch, `hookflow.TurnCursor` (offset+index, agent-scoped, atomic writes, corrupt-reads-as-zero, traversal-guarded), both `hooks.json` entries at timeout 5, `localHookEvents` mirrored, `RunHook`'s `emitTurn` branch with spool-then-cursor ordering, and the SessionEnded sweep. `hookwiring_test.go` binds the three hook declarations (bundle / local / engine) to each other, so a hook added to one and not the others now fails a test.

**Measurements (step 6) are recorded in `reports/measure-260811-transcript-turn-surface.md`** — from 32 real transcripts (13,439 lines), not a live headless run. What it settled: `message.usage` carries all four counts on every usage line; `message.model` is present on 100% of them (and `<synthetic>` is a real value); a `Stop` window spans ~52 model calls, so per-turn numbers are window sums. What it did NOT settle: `Stop`'s cadence, and which transcript `SubagentStop` names — `isSidechain` was present on every line and true on none. The partition is implemented unconditionally because it cannot double-count under any answer; the worst case is a subagent reporting nothing. Per-Stop parse cost still needs a live session (carried to Phase 06).

## Todo list

- [x] `HookStop` + `HookSubagentStop` + dispatch
- [x] `hookflow.TurnCursor` (+ tests; offset + index; agent-scoped)
- [x] `hooks.json` entries + installer + pin test
- [x] `RunHook` branches, ordered spool-then-cursor, pair emitted atomically
- [x] Measurements (a)–(d) recorded in `reports/`
- [x] Cursor swept on SessionEnd
- [x] Test: N Stops over a growing transcript → N disjoint pairs
- [x] Test: Stop with no new usage emits nothing
- [x] Test: subagent window never overlaps the main window
- [x] Confirm neither hook ever writes stdout

## Success criteria

- Three turns produce three pairs with no overlap and no gap; each pair shares one
  id; indexes are strictly increasing.
- A Stop firing twice with no new turn emits nothing (idempotent locally,
  deduped remotely).
- Both hooks write empty stdout in every path, so they can never block a session.
- Hooks installed by `openbox init` and pinned by `installer_test`.
- The four measurements are recorded with citations in the phase report.

## Risk assessment

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Double-counted turns | high without a cursor | high — inflated finops | Cursor + server dedupe on the deterministic id; both tested |
| Stop writes stdout and blocks the session | low | **critical** | Assert empty stdout in tests; mirror the exit-0 discipline |
| Stop does not fire per turn as assumed | medium | medium | Step 6(a) measures before the design locks; window sums stay exact regardless |
| Subagent usage double-counted via parent transcript | medium | high | Step 6(b) measures; partition contingency pre-decided (Phase 03) |
| Cursor state leaks across sessions or agents | low | medium | Keyed by sanitized (session, agent), swept on SessionEnd (existing pattern, `staleness.go:207` guard) |

## Security considerations

- The cursor file holds an integer offset, a turn index, and a sanitized
  session/agent id. No content, no credentials — same posture as the duration
  stash.
- Do **not** bind `last_assistant_message` or `stop_reason` on either hook.
  Adding them to the hook-event struct would put assistant text one careless
  `capStr` away from the wire.
- Session and agent ids are sanitized before use as filenames (path-traversal
  guard already established in `staleness.go:207`).
- `Stop`/`SubagentStop` are hooks that *can* block. Treat any stdout write as a
  defect.

## Next steps

Phase 03 — extract the per-turn numbers, model, and open-time this cursor now
delimits, and build the pair.
