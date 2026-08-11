# Phase 02 — hookflow debounced background-flush trigger

## Context links

- Parent: [plan.md](plan.md) | Depends on: phase-01
- Engine + spool: `adapters/common/hookflow/engine.go`, `spool.go`
- Naming convention for spool-dir siblings: `DurationStash` uses a subdir of
  the spool root (`engine.go:49`); spool skips subdirs and non-`.jsonl` files.

## Overview

- Date: 2026-08-11 | Priority: P1 | Status: complete | Review: complete (code-reviewer, 2026-08-11)
- New provider-agnostic component `adapters/common/hookflow/realtime.go`:
  after a spool append, maybe spawn one detached `openbox hook <provider>
  flush` process for this session, debounced.

## Key insights

- **Debounce, not queue.** Hooks are separate short-lived processes; the only
  shared state is the filesystem. A per-session lockfile in the spool dir
  (`<session>.flushlock`, non-`.jsonl` so `FlushAll`/`IsRecoveryFile` ignore
  it) carries the debounce: if its mtime is younger than the window, skip.
- **Atomicity:** create with `O_CREATE|O_EXCL` to claim; if it exists, check
  mtime — younger than window ⇒ skip; older (stale: spawner died) ⇒ take over
  by updating mtime (`os.Chtimes`) and spawn. A lost race costs one redundant
  spawn at worst; the spool's atomic rotate + server idempotency make a
  redundant flush harmless.
- **Lock lifetime:** the *spawned flusher* refreshes mtime when it starts and
  removes the lockfile when its drain finishes. Removal is best-effort; a
  leaked lock goes stale after the window and is taken over.
  *As built (review correction):* the refresh happens once at drain start, not
  continuously, so a drain slower than the window (slow network, long backlog)
  can be joined by a second flusher. Harmless — atomic rotation means the
  second drain finds nothing or a disjoint file, and the server dedupes — but
  the code comment now says this rather than claiming it cannot happen.
- **Detached spawn:** `os.Executable()` + `exec.Command(self, "hook",
  provider, "flush")` with session id passed via env
  (`OPENBOX_FLUSH_SESSION=<id>`, avoids changing the positional arg
  contract), `Setsid` (SysProcAttr), stdin/stdout=nil, stderr=nil (the child
  logs to its own stderr which is discarded — acceptable: flush is already
  fail-open and SessionEnd re-drains). `cmd.Start()` only, never `Wait()` —
  add `cmd.Process.Release()`.
- **Window:** 2s default. At sub-second tool-call rates the flusher batches
  whatever accumulated — one HTTP request per event is already how
  `FlushSession` works; no change needed.
- Gate order: `ResolveRealtime()` checked first; a disabled gate must cost
  zero I/O (byte-identical current behavior).

## Requirements

1. `type RealtimeTrigger struct { Spool Spool; Provider string; Self string; Window time.Duration }`
   with `func (t RealtimeTrigger) Maybe(logger *log.Logger, sessionID string)`.
2. Zero-value-safe defaults (Window 2s, Self from `os.Executable()`).
3. Never blocks, never writes stdout, never returns error to the hook path —
   log-and-continue only (INV-3).
4. Companion: `func (s Spool) FlushLockPath(sessionID string) string`; the
   flush path (phase 03) refreshes + removes it.
5. Unit tests, no network: fake `Self` pointing at a test helper binary or
   assert spawn via a recorded exec (inject `start func(*exec.Cmd) error`
   for testability); debounce cases (fresh lock ⇒ skip, stale ⇒ takeover,
   absent ⇒ claim+spawn); disabled gate ⇒ no lockfile created.

## Related code files

- `adapters/common/hookflow/realtime.go` (new)
- `adapters/common/hookflow/realtime_test.go` (new)
- `adapters/common/hookflow/spool.go` (edit: `FlushLockPath`, ensure
  `FlushAll` ignores `.flushlock` — it already skips non-`.jsonl`)

## Implementation steps

1. `FlushLockPath` + a note in `FlushAll` docs that `.flushlock` is ignored.
2. `realtime.go`: gate → debounce claim → detached spawn, with injected
   `start` hook for tests.
3. Tests per requirement 5.
4. `cd adapters/common/hookflow && go test ./...`

## Todo

- [x] `FlushLockPath` + doc note
- [x] `RealtimeTrigger.Maybe`
- [x] Unit tests (debounce matrix, gate-off, spawn args/env)
- [x] Module tests green

## Success criteria

Trigger spawns ≤1 flusher per window per session; gate off ⇒ no filesystem
writes; all behavior unit-tested without network.

## Risk / security

- Runs our own resolved binary only (`os.Executable()`), no PATH lookup — no
  injection surface. Env carries only a session id (structural, INV-2).
- Worst-case failure mode: spawn fails ⇒ events wait for SessionEnd (current
  behavior). Fail-open preserved.

## Next steps

Phase 03 wires `Maybe` into both adapters and teaches their flush path the
session env var + lock lifecycle.
