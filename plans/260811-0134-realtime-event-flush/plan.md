---
title: "Near-real-time event delivery via debounced background flush"
description: "Deliver spooled governance events to OpenBox core within seconds of each tool call instead of batching at SessionEnd."
status: complete
priority: P1
effort: 7h
branch: main
tags: [hookflow, telemetry, realtime, claude-code, codex]
created: 2026-08-11
completed: 2026-08-11
---

# Near-real-time event delivery

## Problem

Events reach OpenBox core only when the session ends. Every hook appends to a
per-session spool file (`adapters/common/hookflow/spool.go`) and delivery to
`/api/v1/governance/evaluate` runs only in `runFlush` at SessionEnd
(`adapters/claude-code/hookrun.go:210`) or via the manual `flush` subcommand.
By design (INV-3 hot-path budget), but a governance dashboard that lags a whole
session is not real-time observability.

## Approach (user-approved OD decisions, 2026-08-11)

- **Mechanism:** each observe hook triggers a short-lived **detached background
  flush** of the session's spool, debounced via a lockfile so concurrent hook
  processes don't stampede. Events land within ~1–2s of each tool call.
- **Default:** ON. Opt-out via `OPENBOX_REALTIME=0` / dev.json `realtime_flush:false`.
  SessionEnd flush stays as the completeness safety net.
- **Scope:** trigger lives in shared `hookflow` — Claude Code and Codex both get it.

Hot path cost is one debounce stat + (at most once per window) a `cmd.Start()`
of our own binary — no network I/O in the hook process, INV-3 preserved.
Re-delivery races are already safe: spool rotation is an atomic rename (losers
drain 0 events) and the server dedupes on Idempotency-Key (E8-S7).

## Phases

| # | Phase | Status | File |
|---|-------|--------|------|
| 1 | devconfig `ResolveRealtime` flag | complete | [phase-01-devconfig-realtime-flag.md](phase-01-devconfig-realtime-flag.md) |
| 2 | hookflow debounced background-flush trigger | complete | [phase-02-hookflow-realtime-trigger.md](phase-02-hookflow-realtime-trigger.md) |
| 3 | Adapter wiring (claude-code + codex) + session-scoped flush arg | complete | [phase-03-adapter-wiring.md](phase-03-adapter-wiring.md) |
| 4 | Testbed e2e + docs | complete (testbed phase not yet run — stack down) | [phase-04-testbed-and-docs.md](phase-04-testbed-and-docs.md) |

## Acceptance criteria

1. ✅ A PostToolUse hook results in the tool-call events reaching core within
   seconds, while the session is still running — `TestHookRealtimeDelivery`
   (real binary, mock core). Live-stack proof pending (AC6).
2. ✅ Hook hot path stays non-blocking: no network I/O in the hook process; cost
   is a lockfile check plus, at most once per window, a fork+exec.
3. ✅ `OPENBOX_REALTIME=0` / `realtime_flush:false` restores exact prior
   behavior — the gate short-circuits before any filesystem I/O
   (`TestRealtimeMaybe_DisabledIsZeroIO`); `TestHookEndToEndSmoke` keeps its
   legacy contract unchanged under the opt-out.
4. ✅ No duplicate delivery under concurrent flush + SessionEnd — atomic rename
   rotation + Idempotency-Key; asserted end-to-end (3 events, 3 unique keys).
5. ✅ Codex gains the same behavior with no engine fork (shared
   `hookflow.RealtimeTrigger`; adapter diffs are mirrors).
6. ⏸ `testbed/25-realtime.sh` written and registered, **not yet run** — no local
   OpenBox stack available on this machine.

## Review outcome (2026-08-11)

`code-reviewer` ran the full CI recipe (gofmt, vet, `go test -race` across all
11 modules, Windows cross-compile) — all green — and confirmed INV-1/2/3, the
race analysis, and every acceptance criterion above. One High finding, fixed:
`realtime_flush` was resolved but absent from `Posture`/`postureFields()`, so it
was invisible to `openbox doctor` and session-start telemetry — the same gap
`require_verified_bundle` shipped with. Fixed, plus
`TestPostureFields_CoverEveryConfigControl`, which walks `DevConfig` by
reflection so the next boolean control cannot repeat it (mutation-verified: the
guard fails when the entry is removed). Two comment-accuracy fixes: the
debounce doc now states that a drain slower than the window can be joined by a
second flusher (harmless, not "cannot happen"), and "cannot delay the gate"
became the accurate "local and bounded".

## Non-goals

- No long-lived daemon, no new endpoint, no new table (reuse rule / decision record gate).
- No change to what egresses (content posture untouched — timing only).
- Enforce-path latency unchanged (Tier-2 sync escalation already exists).
