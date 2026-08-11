# Phase 03 — adapter wiring (claude-code + codex)

## Context links

- Parent: [plan.md](plan.md) | Depends on: phase-02
- Claude Code: `adapters/claude-code/hookrun.go` (`RunHook`, `runFlush`)
- Codex: `adapters/codex/hookrun.go` (same shape — `runFlush(logger, "")` at
  :67 for the flush subcommand, `runFlush(logger, ev.SessionID)` at :222)

## Overview

- Date: 2026-08-11 | Priority: P1 | Status: complete | Review: complete (code-reviewer, 2026-08-11)
- Call the trigger after every successful spool append (except SessionEnd,
  which flushes inline already); teach the `flush` subcommand the
  `OPENBOX_FLUSH_SESSION` env var and the lock lifecycle.

## Key insights

- Trigger point is **after `ad.Observe(hook, ev)`** in both adapters — the
  event is on disk, so the spawned flusher can see it. Fire on every hook
  except SessionEnd (PreToolUse included: in enforce mode `RunHook` returns
  early inside the gate branch, so place the trigger before that branch, or
  accept enforce PreToolUse events waiting one window — decision: place
  BEFORE the enforce branch; the spawn is fire-and-forget and cannot delay
  the gate).
- `runFlush` changes (both adapters, identical — keep them thin mirrors):
  1. session scope: `sub == "flush"` reads `OPENBOX_FLUSH_SESSION`; set ⇒
     `ad.Flush(ctx, session, cl)`, empty ⇒ `ad.FlushAll` (manual catch-up
     unchanged).
  2. lock lifecycle: refresh lock mtime at start, remove on completion (via
     phase-02 `FlushLockPath`). SessionEnd's flush also removes the lock —
     cheap cleanup of the session's last debounce marker.
- Credential resolution stays in the spawned flusher process (as today in
  `runFlush`) — the hot path still touches no secrets (INV-1).
- The spawned process re-enters `RunHook(sub="flush")` → `devconfig.Pin()`
  applies as usual; no re-entrancy hazard (it never spawns further flushers
  because the flush path doesn't call the trigger).
- Existing integration test pattern: `cli/cmd/openbox/main_test.go:361-446`
  (mock `/evaluate`, assert spool drained). Add a mid-session case.

## Requirements

1. Both adapters call `hookflow.RealtimeTrigger{...}.Maybe(logger, ev.SessionID)`
   after Observe, gated inside `Maybe` (adapters stay unconditional).
2. `flush` subcommand honors `OPENBOX_FLUSH_SESSION`.
3. Flush path refreshes/removes the session flush lock.
4. Integration test (mock core): PostToolUse hook with realtime on ⇒ events
   arrive at mock `/evaluate` without any SessionEnd, spool drained; with
   `OPENBOX_REALTIME=0` ⇒ nothing arrives until SessionEnd (current golden
   behavior byte-identical).
5. Conformance parity: codex mirrors claude-code (existing
   `conformance_parity_test.go` pattern if applicable).

## Related code files

- `adapters/claude-code/hookrun.go` (edit)
- `adapters/codex/hookrun.go` (edit)
- `adapters/claude-code/hookrun` tests + `cli/cmd/openbox/main_test.go` (edit)
- `adapters/codex/` mirror tests (edit)

## Implementation steps

1. Wire trigger into claude-code `RunHook` (before enforce branch, after
   Observe); extend its `runFlush`.
2. Mirror in codex.
3. Integration tests: realtime-on mid-session delivery (poll mock with
   timeout — the flusher is async), realtime-off unchanged, no duplicate
   event ids received when SessionEnd races a background flush.
4. `cd adapters/claude-code && go test ./...` ; same for codex; `go build ./cli/...`.

## Todo

- [x] claude-code trigger + flush session env + lock lifecycle
- [x] codex mirror
- [x] Integration tests (on / off / race)
- [x] All module tests green

## Success criteria

Mid-session events reach mock core within the debounce window; opt-out is
byte-identical to today; no duplicates.

## Risk / security

- Test flakiness from async spawn — poll with generous timeout, don't sleep
  fixed intervals.
- Spawn storms if lock logic regresses — the debounce matrix in phase 02 plus
  an integration assertion (≤N `/evaluate` bursts for M rapid events) guard it.

## Next steps

Phase 04 proves it against a real stack and updates docs.
