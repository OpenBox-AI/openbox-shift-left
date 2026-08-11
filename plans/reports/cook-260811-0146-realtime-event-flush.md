# Implementation report — near-real-time event delivery

Plan: [260811-0134-realtime-event-flush](../260811-0134-realtime-event-flush/plan.md) · 2026-08-11 · branch `main`

## Problem

Events reached OpenBox core only at SessionEnd. Every hook appended to a local
spool; delivery ran once, at teardown. By design (hot-path budget), but a
governance dashboard lagging a whole session is not observability.

## Delivered

Debounced detached background flush, default on, shared engine.

| Area | Change |
|---|---|
| `devconfig` | `EnvRealtime`, `RealtimeFlush *bool`, `ResolveRealtime()` (default **on**, first default-on opt-out flag); added to `Posture`/`postureFields()` so `openbox doctor` + session telemetry report it |
| `hookflow` | `realtime.go` + unix/windows detach split: `RealtimeTrigger.Maybe` — gate → per-session `.flushlock` debounce (O_EXCL claim, stale-mtime takeover) → detached `cmd.Start` of `openbox hook <provider> flush`; `Spool.FlushLockPath/TouchFlushLock/ReleaseFlushLock` |
| adapters | both `hookrun.go` call the trigger after `Observe` (skipped on SessionEnd); `flush` subcommand honors `OPENBOX_FLUSH_SESSION`; session-scoped drains hold/release the lock |
| tests | 8 unit (debounce matrix, gate-off zero-I/O, spawn argv/env, test-binary guard, lock lifecycle, drain-ignores-lock); `TestHookRealtimeDelivery` — real binary, mock core, mid-session delivery + 3 unique Idempotency-Keys; `TestPostureFields_CoverEveryConfigControl` reflection guard |
| testbed | `25-realtime.sh` registered in `run-all.sh` — **written, never run** (no local stack) |
| docs | architecture, README, data-and-privacy (timing-only, content posture untouched), CLAUDE.md |

## Verification

- All 11 workspace modules: build, vet, tests green. `gofmt` clean. `GOOS=windows` cross-compile OK.
- `code-reviewer` ran the CI recipe incl. `go test -race` over all 22 packages — green; confirmed INV-1/2/3, race analysis, 5 of 6 acceptance criteria (6th needs a live stack).
- Realtime integration test re-run 5x + 2x, no flake.
- Mutation-checked the new posture guard: removing the entry makes it fail.

## Review findings fixed

1. **High** — `realtime_flush` resolved but absent from `Posture`/`postureFields()`: invisible to `openbox doctor` and session-start telemetry, repeating the `require_verified_bundle` gap the repo's own comments warn about. Fixed + reflection guard so the next boolean control can't repeat it.
2. **Medium** — doc claimed the debounce window "covers the whole flush"; the refresh is once at drain start, so a drain slower than the window can be joined by a second flusher (harmless: atomic rotation + dedupe). Comment now states the real behavior.
3. **Low** — "cannot delay the gate" → "local and bounded" (the trigger does do synchronous local I/O before the enforce gate).

## Not done

- `testbed/25-realtime.sh` has never executed — core :8086 / backend :3000 unreachable here. Run `./testbed/run-all.sh onboard realtime` against a live stack.
- Windows detach verified by cross-compile only, not runtime.
- No codex binary-level realtime test (parity rests on the shared engine + mirrored adapter code + codex unit suite).

## Unresolved questions

1. Run the testbed phase before considering this shippable, or accept the binary-level test as sufficient for now?
2. Is 2s the right debounce window, or tighter (more spawns) / looser (more lag)?
3. Spawn cost: a long tool-heavy session creates many short-lived flusher processes, each with its own TLS handshake. Accepted trade-off of "no daemon" — worth monitoring?
