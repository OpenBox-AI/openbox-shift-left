---
title: Near-real-time event delivery for the developer runtime
date: 2026-08-10
summary: Replaced batch-at-SessionEnd telemetry with a debounced detached flusher; found and fixed a posture-reporting gap and a darwin test-isolation bug along the way
---

# Near-real-time event delivery for the developer runtime

## What happened

Reported symptom: with Claude Code, events did not reach OpenBox core while a
session ran — everything arrived in one batch after the session ended.

Not a bug. By design: every hook appended to a per-session spool
(`adapters/common/hookflow/spool.go`) and delivery ran once, at SessionEnd
(`adapters/claude-code/hookrun.go`). The hot-path budget (INV-3) was the reason.
The cost was that a session's evidence did not exist until it ended, so a running
session and a silent fleet looked identical to the control plane.

Fix: after spooling its event, a hook nudges a detached flusher for that session
(`hookflow.RealtimeTrigger`), debounced through a per-session `.flushlock` so a
burst of tool calls drains once rather than once per hook. ~2s to core. The hook
process still does zero network I/O — a lockfile check plus, at most once per
window, a fork+exec, never a wait on the child. SessionEnd stays the completeness
safety net. Shipped in the shared engine, so Codex got it from the same code.

## Decisions

- **Debounced spawn over inline flush or a daemon.** Inline would put the network
  on the tool-call path; a daemon adds lifecycle and orphan management. The
  lockfile is the only shared state available across short-lived hook processes.
- **Default ON**, opt out with `realtime_flush:false` / `OPENBOX_REALTIME=0`.
  Real-time is the expected posture for a governance product.
- **Timing-only.** No change to what egresses; content posture untouched.

## What the work turned up

Three things worth more than the feature itself:

1. **A posture-reporting gap.** `realtime_flush` resolved correctly but was never
   added to `postureFields()`, so it was invisible to `openbox doctor` and to
   session-start telemetry — the same gap `require_verified_bundle` shipped with,
   which the code's own comments warn about. The existing test could not catch it:
   it checked the field table against itself. Added
   `TestPostureFields_CoverEveryConfigControl`, which walks `DevConfig` by
   reflection, and mutation-checked it (removing the entry does fail it). A test
   that cannot fail for the reason it exists is not coverage.

2. **A darwin test-isolation bug, pre-existing.** Two findings-cursor tests set
   `XDG_CONFIG_HOME` to redirect state, but `os.UserConfigDir()` ignores XDG on
   macOS. They were reading and writing the real
   `~/Library/Application Support/openbox` cursors: green on a fresh machine, red
   forever after. Committed separately as `d420beb`.

3. **The testbed cannot run on this machine.** `testbed/env.sh:28` points at
   `local-stack/docker-compose.local.yml`, which exists in no repo on this box.
   `openbox-core/docker-compose.yml` cannot substitute — it wants a `.env` that is
   not there and contains none of postgres, opa or openbox-fe, three of the seven
   containers preflight requires. The suite has never been runnable here,
   independent of this change.

## Verification

All 11 workspace modules build, vet and pass; gofmt clean; `hookflow`
cross-compiles for Windows. `code-reviewer` ran the CI recipe including
`go test -race` over 22 packages. The load-bearing test is
`TestHookRealtimeDelivery` — real binary, real hooks, mock core, asserting
mid-session delivery and three unique Idempotency-Keys.

Still unproven: the hop this repo cares most about — real hooks against real core
with real Postgres rows. `testbed/25-realtime.sh` is written and registered but
has never executed. Unit tests are not evidence that a hook works, so this is a
real gap, not a formality.

## Next steps

- Run `./testbed/run-all.sh onboard realtime` once a stack exists; the missing
  compose file is the blocker, and `testbed/env.sh:28` should be corrected or the
  file restored.
- Fix testbed XDG isolation on macOS — same darwin trap as (2), but for the
  harness, which means testbed runs have been polluting real state and the
  "spool drained" assertion in `20-capture.sh` passes vacuously.
- Open: is 2s the right debounce window, and is the per-session process spawn
  cost acceptable over a long tool-heavy session?

Branch `feat/realtime-event-delivery`, pushed. No PR opened.

> Historical work record — not durable authority. Prefer docs/specs/ADRs for current decisions.
