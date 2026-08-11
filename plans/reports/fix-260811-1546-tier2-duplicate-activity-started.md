# Fix: Tier-2 escalation stored the ActivityStarted twice

Date: 2026-08-11 15:46 (+07)
Branch: `fix/tier2-duplicate-activity-started` (off `origin/main` @ 56411f2)
Diagnosis: [debug-260811-1541-activity-id-duplicate-started.md](debug-260811-1541-activity-id-duplicate-started.md)
Option chosen by user: **2 — suppress the redundant client emit** (server-side
dedupe, option 1, deliberately out of scope)

## Root cause

In enforce+Tier-2 mode a gated PreToolUse reached core **twice** with one
`event_id`: the escalation POSTs it synchronously to `/evaluate`, and the observe
copy is spooled and flushed. The mapper clock is pinned per hook invocation
precisely so both derive the same id and collapse under one `Idempotency-Key` —
but core does not dedupe developer events on it (`adapters/claude-code/mapper.go:524`),
so both were stored, each with its own Merkle leaf.

Measured: 7 of 27 Bash activity_ids in the supplied session were `SSC`.

## Change

Provider-agnostic logic went to `hookflow`; both adapters were affected
identically, so fixing only claude-code would have reintroduced the adapter drift
CLAUDE.md forbids.

| File | Change |
|---|---|
| `adapters/common/hookflow/tier2.go` | `Tier2.OnDelivered` — fires after `cl.Emit` returns nil error |
| `adapters/common/hookflow/gate.go` | `EnforceGate.SpoolObserve` + deferred wiring in `Run` |
| `adapters/common/hookflow/engine.go` | `Engine.RecordDeferred` — `Record` split into stash-now / append-later |
| `adapters/claude-code/hookrun.go` | gated PreToolUse defers its observe copy into the gate |
| `adapters/codex/hookrun.go` | same |
| `adapters/common/hookflow/realtime.go` | corrected a doc claim asserting a dedupe that does not exist |
| `testbed/40-approvals.sh` | new step G: no activity_id stored more than once per half |

Three design points worth keeping:

1. **Delivery is keyed on the transport, not the verdict.** `OnDelivered` fires on
   `err == nil` from `Emit`. An escalation that came back with no usable verdict
   (`Tier2FailOpen "no verdict"`) or with REQUIRE_APPROVAL was still *stored*.
   Keying on `decision.Source` would have missed both — `SourceTier2` is absent in
   the first case and replaced by the approval outcome in the second.
2. **Default is to spool.** The suppression is a `defer` with a flag set from
   inside the transport, so every exit path — stale-gate early return, degraded
   escalation, unmappable event — falls back to spooling. A redundant copy is a
   bug; a missing one is lost telemetry.
3. **The duration stash stays unconditional.** `Engine.Record` was
   `ThreadDuration`+`Append`; skipping `Observe` wholesale would have skipped
   `PutStart` and silently blanked `duration_ms` for exactly the escalated calls.
   `RecordDeferred` threads the stash now and returns the append as a closure.

Public API: three additive symbols. Nothing removed, no signature changed. A nil
`SpoolObserve` reproduces the old behavior for callers that spool their own copy.

## Verification

- 7 new tests in `adapters/common/hookflow/observecopy_test.go`, all passing.
- **Negative control run:** with the `OnDelivered` wiring neutered, exactly the two
  suppression tests fail (`…SkippedWhenEscalationDelivered`,
  `…SkippedWhenApprovalWasFiled`) and the four spool-side tests still pass. The
  tests discriminate the fix rather than the scaffolding.
- Full workspace sweep — build + vet + test across all 11 `go.work` modules:
  green. `gofmt` clean. `bash -n` clean.
- Blast radius covered: `hookflow` (gate/tier2/engine/approval hold/rewake/spool),
  both adapters, `client`, `decision`, `cli`.

**Not run: `testbed/run-all.sh`.** No local OpenBox stack (ports 3000/8080/5432/8081
all closed), so the new step G is unexecuted. Per CLAUDE.md unit tests are not
evidence a hook works — this fix is verified at unit level only and still needs
one real headless enforce+T2 session before it can be called done.

## Unresolved questions

1. **Testbed run outstanding** — step G needs a live stack. Run
   `./testbed/run-all.sh` before merging?
2. **Option 1 still open.** Core not deduping remains true, so the lost-200 retry
   path can still double-store, and any future non-spool egress inherits the same
   trap. Should a backend issue be filed against openbox-backend/openbox-core?
3. **Backfill.** Already-stored sessions carry duplicate rows and duplicate Merkle
   leaves. Repair, or forward-only?
4. **H1 latent** — `activity_id` is operation-derived, so two deliberate identical
   operations still collapse onto one id (`SCSC`). Did not fire in the evidence.
   Separate decision; reversing it naively re-breaks the approval loop.
5. **The unpaired `Read`** on a directory path (1 event, no Completed) is
   untouched — expected failure mode or a PostToolUse gap?
6. `.probe-evidence/` (the supplied API dumps) is untracked in the repo root and
   should be deleted or ignored before any commit.
