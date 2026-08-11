# Phase 04 — testbed e2e + docs

## Context links

- Parent: [plan.md](plan.md) | Depends on: phase-03
- Testbed: `testbed/run-all.sh`, `docs/testbed/e2e.md`
- Docs to touch: `docs/architecture.md`, `README.md`,
  `docs/data-and-privacy.md` (verify no change needed), repo `CLAUDE.md`
  (current-state paragraph)

## Overview

- Date: 2026-08-11 | Priority: P2 | Status: docs complete, testbed phase written but UNRUN | Review: complete (code-reviewer, 2026-08-11)
- Prove real-time delivery against a real local stack ("unit tests are not
  evidence that a hook works") and keep docs true.

## Requirements

1. New testbed scenario: run a real headless session, assert tool-call events
   are queryable in core **before** the session ends (poll during the
   session, bounded wait), and total counts still exact after SessionEnd
   (no loss, no duplicates).
2. Existing scenarios stay green — especially E8-S7 recovery ones (offline
   flush ⇒ recovery files ⇒ later sweep): background flushers failing against
   a down stack must still fail-open and leave recovery files intact.
3. Docs:
   - `docs/architecture.md`: delivery model section — spool + debounced
     background flush (default on, `OPENBOX_REALTIME=0` opt-out), SessionEnd
     as safety net; cite `hookflow.RealtimeTrigger`.
   - `README.md`: one-line latency claim update (near-real-time, ~2s).
   - `docs/data-and-privacy.md`: confirm and state that only *timing*
     changed, content posture untouched.
   - `CLAUDE.md` current-state paragraph: add real-time flush to shipped list
     after verification.

## Implementation steps

1. Write testbed scenario (mirror an existing scenario's structure).
2. `./testbed/run-all.sh` against local stack — all green.
3. Doc edits, each claim cited to symbol/path.

## Todo

- [x] Testbed scenario (mid-session delivery + post-end exactness) —
      `testbed/25-realtime.sh`, registered in `run-all.sh`
- [ ] **Full testbed run green — NOT DONE.** No local OpenBox stack on this
      machine (core :8086 and backend :3000 both unreachable). The phase is
      written and syntax-checked only; it has never executed. Run
      `./testbed/run-all.sh onboard realtime` once a stack is up.
- [x] Doc updates with citations

## Success criteria

Real headless session shows events in core mid-session; full suite green;
docs match reality with sources.

## Risk assessment

Testbed timing sensitivity: debounce window (2s) vs scenario polling —
use bounded polling with a margin (e.g. 15s cap), not fixed sleeps.

## Security considerations

None new — verify the spawned flusher logs nothing secret-bearing (it
discards stderr; ensure that discard doesn't hide INV-1 violations by
reviewing what `runFlush` logs).

## Next steps

Plan complete → review → implement via /ak-cook or equivalent.
