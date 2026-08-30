# Phase 12 — one command in, one command out; producer election

## Context links

- Parent: [plan.md](plan.md) · Depends: [phase-09](phase-09-telemetry-receiver-daemon.md), [phase-11](phase-11-transport-proxy-service.md)
- Scout: [scout-01 §D](scout/scout-01-gateway-service-lifecycle.md)
- Rulings: **OD2** (one command each way) · **OD1(c)**, **OD3**, **OD4** consumed here
- Decision: **D-OSS-3** boundary applies here — kardianos supplies unit mechanics
  (via phase 04); the proof-order install, rollback and the activation record are
  **custom and stay custom**

## Overview

- Date: 2026-08-27 · Priority: P1 · Effort: 6h
- Implementation status: **done, with three named deviations** (2026-08-29) · Review status: reviewed; the critical finding is **FIXED and drilled** (see Code review below)
- Report: [verification-260829-phase-12-one-command-and-election](reports/verification-260829-phase-12-one-command-and-election.md)
- The UX contract the owner ruled, and the invariant that keeps three producers from
  corrupting each other's evidence.

## Key insights

- **The existing env module owns exactly one key with one prior file**
  (`gatewayservice/env.go:34,58–60`). Three lanes now need ~19 keys between them.
  Copying it three times is the drift this repo already paid for once. Generalize to
  a **managed-key set + activation record** — the logger's proven discipline:
  managed map, original presence/value per key, before/after SHA-256, refuse on
  conflict, restore only managed keys.
- **First-writer-wins must survive generalization.** The `ANTHROPIC_BASE_URL` lesson
  was that key-ownership alone missed the case where the key is ours and the value
  is someone else's. Every managed key needs the same displaced-value memory, or an
  org's corporate proxy / relay is destroyed on install and lost on removal.
- **The election is a correctness invariant, not a preference.** Two producers
  emitting a turn for the same model call means core's dedupe absorbs one and half
  the evidence vanishes with no error. Namespaces make ids disjoint; the election
  makes the *count* right.
- Removal must not require the thing being removed to still work — the gateway
  already learned this (removal runs *before* the credential gate). Same rule here.
- **The library/custom boundary is fixed** (validation round 2): kardianos writes
  and drives units — through whatever branch phase 04's gate settled — while the
  install *ordering* (unit → start → prove → env), the rollback that removes the
  unit on any later failure, and the activation record are ours. Do not let the
  library's `Install`/`Uninstall` convenience collapse that ordering.

## Requirements

1. **`openbox init --provider claude-code --full`** (name fixed at validation,
   2026-08-27): installs and enables hooks + telemetry + transport, in proof order,
   idempotently.
2. **`openbox init --remove-all`**: removes everything — every managed env key
   restored from the activation record, all three services unloaded and their units
   deleted, CA, logs, spool and raw-body directories removed. Runs **before** any
   credential requirement. Prints exactly what it retired.
3. A shared `activation` package replacing the single-key mechanism, consumed by
   gateway, telemetry and transport.
4. Producer election persisted in `dev.json`, resolved with the existing tri-state
   `*bool`/`flagPassed` pattern; `doctor` prints the elected producer.
5. Defaults per the rulings + validation: **telemetry on once installed**,
   transport opt-in (installed only by `--full` or its own flag), gateway unchanged.

## Architecture

Election precedence when more than one lane is installed — **decided at validation
(2026-08-27), automatic, not developer-chosen**: **transport > gateway >
telemetry**. In-path relays observe real bytes and can enforce; telemetry is
client-asserted and cannot, so the highest-assurance installed lane wins without the
developer having to think about it. Exactly one lane emits model-call turn events;
the others still emit their non-turn evidence (tool decisions, engine health)
because those do not collide.

`doctor` must print the elected producer **and why** — an automatic precedence the
developer cannot see is the "configured but not in force" shape that decision
promised would be detectable.

Activation record (`~/.openbox/activation.json`, 0600):

```
{ lane: "telemetry"|"transport"|"gateway",
  managed: { KEY: value_we_wrote, ... },
  original: { KEY: {present: bool, value: string|null}, ... },
  before_sha256, after_sha256, settings_path, activated_at }
```

Removal restores `original` for exactly the keys in `managed`, refuses if a managed
value changed underneath it (unless forced), and never touches an unmanaged key.

## Related code files

- new: `adapters/common/devconfig/activation.go` (+ tests) — the shared mechanism
- refactor: `cli/internal/gatewayservice/env.go` → consume the shared mechanism
  while keeping its current external behaviour byte-for-byte
- edit: `cli/cmd/openbox/main.go` (flags, mutual exclusion, removal early-exit),
  `initgateway.go` siblings for the two new services, `doctor.go`
- edit: `adapters/common/devconfig/write.go` (election field, tri-state)

## Implementation steps

1. Write `activation.go` first, with the logger's semantics; port
   `gatewayservice/env.go` onto it and prove the gateway's existing tests still pass
   **unchanged** (that is the regression bar).
2. Add per-lane managed-key sets; each lane declares its keys, the mechanism owns
   the transaction.
3. Implement the election resolver + `dev.json` field; make "not supplied" distinct
   from "explicitly chosen" (the `flagPassed` lesson) and add the second-invocation
   test — a plain re-run must not revert a deliberate choice.
4. Implement `--full`: hooks → telemetry (unit→start→prove→env) → transport
   (unit→start→prove→CA→env). Any failure rolls back that lane and leaves earlier
   lanes working, reporting precisely what is and is not installed.
5. Implement `--remove-all` in reverse, before the credential gate, tolerant of
   partial state (a lane that was never installed is not an error).
6. Doctor: elected producer + per-lane blocks.
7. State-diff test (V7): snapshot launchctl/settings/`~/.openbox` → install →
   remove → assert the snapshot matches, including a pre-existing foreign proxy and
   base-URL value restored byte-identically.

## Todo

- [x] `activation.go` with original/managed/SHA-256/refuse-on-conflict — `cli/internal/activation/`.
      The refusal sits on REMOVAL, not on install: redirecting the tool is the whole purpose of
      installing a lane, so refusing because the key we exist to change is already set would refuse
      on exactly the machines that need governing. Install replaces, records and REPORTS.
- [ ] gateway ported, its existing tests unchanged and green — **NOT DONE, deliberately.**
      Zero-behavior-change refactor of the only socket-verified lane in this repo, serving neither
      half of the stated outcome. `--remove-all` composes the existing `removeGateway`. The
      *unit/service* layer WAS generalized (`cli/internal/laneservice/`) and the gateway delegates
      to it with **no test edited** — that half carried real DRY value on the path where drift is
      least visible. Owner call: report §6(a).
- [x] per-lane managed-key sets — 13 telemetry + 5 transport, copied verbatim from the proven set,
      pinned by a literal-list test. `OTEL_LOG_RAW_API_BODIES` deliberately subtracted (report §5).
- [x] election resolver + second-invocation test — **DERIVED from the settings file, not stored in
      `dev.json`.** The `dev.json` field was written, tested green, then reverted: a second store of
      derivable state whose drift silences every lane while looking configured. Report §4.
- [x] `--full` install, proof-ordered, partial-failure honest
- [x] `--remove-all` before credential gate, partial-state tolerant — **does NOT delete the
      spool**, against requirement 2's text and per this phase's own Security section: the spool
      lives outside `~/.openbox` and is SHARED with the hook path, which `--remove-all` does not
      remove, so deleting it would destroy undelivered tool-call evidence from a component that is
      still running. Named in the output instead. Report §6(c).
- [x] doctor: elected producer **and why** + lane blocks (unit / configured / reachable / log)
- [x] V7 state-diff test incl. foreign-value restore — bind-free
- [x] `-race` (14 modules), both cross-compiles, `GOWORK=off` for `cli`
- [x] 21 mutation drills run, 21 red on deletion — one was GREEN until its test was strengthened
      (report §3), which is the finding worth more than the ten that passed

## Success criteria

- One command installs and enables everything; one command removes everything.
- After removal, a state diff of launchd units, `~/.claude/settings.json` env, and
  `~/.openbox/` is empty, and a pre-existing foreign proxy/base-URL is restored
  byte-identically.
- Exactly one model-call producer emits per session, provably.
- A plain re-run of `init` never reverts a deliberate opt-out.
- The gateway's pre-existing tests pass with no edits.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| Refactoring the env module regresses the shipped gateway | Port first, gateway tests must pass **unedited** | Any gateway env test needs editing to pass | **Stop** — an edit to those tests is the signal the behaviour changed |
| Two producers both emit turns | Election + disjoint namespaces + soak assertion | Duplicate `llm_completion` activities | Stop; this corrupts usage/cost for every reader |
| Partial install leaves a half-configured machine | Per-lane rollback; `--full` reports precisely what is installed | Doctor shows a lane configured but not reachable | Re-run install for that lane; never leave env pointing at a dead port |
| Removal leaves an orphaned KeepAlive daemon | Removal is unit-unload-then-delete, tolerant and idempotent; V7 asserts | A daemon restarts after removal | P0 — a restart-looping daemon nobody was told about |
| **Assumption: `--remove-all` can restore every displaced value.** Fails if a lane was installed before the activation record existed | Migrate the legacy `gateway-prior-env.json` into the record on first run | Legacy prior file present with no record | **Adjust within the plan**: migration step, tested both directions |
| Automatic election silently picks a lane the developer did not expect | `doctor` prints the elected producer and the reason | Developer reports evidence from an unexpected lane | Doctor output is the fix; do not add a second override switch without a decision |
| Library uninstall convenience skips the unload-then-delete ordering | The ordering stays in our code; the library only executes each step | A unit survives `--remove-all` in V7 | Restore the explicit ordering; do not delegate sequencing to the library |

## Security considerations

- Removal deletes the CA, the raw-body directory and the spool. That is data
  destruction by design — print what is being deleted, and never delete anything
  outside `~/.openbox/` and the managed keys.
- The activation record names every key we touched; it is integrity evidence, not a
  secret, but it lives 0600 beside credentials regardless.
- `--remove-all` must work on a machine with wiped credentials (the gateway
  precedent: otherwise every model call fails against a dead port with no CLI fix).
- Forcing over a changed managed value overwrites someone's deliberate edit —
  require an explicit flag and print the diff.

## Outcome (2026-08-29)

Implemented, unit-verified **bind-free**, 14 modules green under `-race`, both cross-compiles
green, `cli` green under `GOWORK=off`. No socket, no launchd, no stack.

Every pre-existing test passes **unedited** — `git diff --stat -- '*_test.go'` is empty, which is
this phase's own stop-signal check.

The single highest-consequence unverified claim: **that the 13 telemetry env keys are the ones the
installed client actually reads.** Every test asserts JSON we wrote; the consumer drops what it does
not recognize, exactly as core did with `http_status` vs `http_status_code`. A rename here yields a
green suite and a receiver that never gets a record — which OD4 then reports as silence, i.e. as a
finding against the developer. Phase 13 confirms it live.

## Code review (2026-08-29) — one critical, found and FIXED

**The critical was real, and it broke this phase's own success criterion #3**
("exactly one model-call producer emits per session, provably").

The telemetry daemon resolved the election ONCE at its own startup and baked the
answer into a static `telemetryemit.Policy{Elected bool}`. `setupLanes` installs
telemetry BEFORE transport, so on every fresh `--full` the telemetry daemon boots,
correctly elects itself (nothing else is routed yet), freezes that answer — and then
transport is installed and takes the election from it. Both lanes then emit a turn
for the same model call. The activity_id namespaces are deliberately disjoint, so
core stores both rather than rejecting one: **every token count and cost figure
doubles, silently**, while `openbox doctor` reports a clean single elected lane
because it re-resolves from the settings file and has no channel to a sibling
daemon's in-memory decision. The reverse is quieter and costs everything instead:
install telemetry while a stronger lane is routed, remove that lane later, and a
process frozen at "not elected" stays silent forever — and doctor's ELECTED-BUT-ABSENT
warning does not fire, because something IS listening.

The reviewer proved it against the real packages rather than by inference, and no
test reached it: every existing test covered the pure election function or one lane's
install/removal in isolation, so it survived 19 mutation drills by being unreachable
from all of them.

**Fixed by making the gate live, not by bouncing the daemon.** `Policy.Elected`
is a `func() bool` resolved per record; nil suppresses, so the zero value's
guarantee became structural rather than conventional. The reviewer leaned toward the
surgical fix — restart telemetry's unit whenever an install would flip the election —
and that was rejected for the reason this phase already rejected a stored election: a
snapshot of derivable state is a second store with a sync obligation, and the restart
form covers only the paths `init` controls, leaving a hand-edited settings file or an
MDM deployment stale. `TestElectionIsAnsweredPerRecordNotAtConstruction` flips the
answer under a mapper that is already built; two drills (freeze the gate at
construction; treat nil as elected) are red on deletion.

**Warning, also fixed:** the `--provider claude-code`-only guard iterated a Go map to
pick which flag to name, so the error cited a different flag run to run (measured 7/8
`--full`, 1/8 `--transport`). Never wrong, never flaky in the suite — the test matches
the common substring — but a fleet script grepping the flag it passed would match
only sometimes. Now an ordered slice, matching the mutual-exclusion check above it;
verified deterministic over 6 runs of the real binary.

**Also from the review:** the gateway's launchd label and systemd unit name existed as
two independent literals (`gatewayservice` and `laneservice`); `laneservice` now owns
them and `gatewayservice`'s constants are aliases. `serviceName` is documented as
off the production path. `Applied.Replaced`'s comment no longer claims a displaced
value is always the org's — on a re-install it is our own previous write. One test
tightened from presence to value equality. A stray, INCOMPLETE `transport/go.sum` diff
(a partial artifact of a `GOWORK=off` resolve the sandbox denied part-way) was
reverted rather than committed.

## Next steps

Phase 13 proves the bytes — and should test the SEQUENTIAL-INSTALL case explicitly,
not only steady state: the defect above was invisible to every steady-state test.
Phase 14 reconciles the docs.
