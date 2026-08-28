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
- Implementation status: pending · Review status: pending
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
developer cannot see is the "configured but not in force" shape ADR-0021 §2 promised
would be detectable.

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

- [ ] `activation.go` with original/managed/SHA-256/refuse-on-conflict
- [ ] gateway ported, its existing tests unchanged and green
- [ ] per-lane managed-key sets
- [ ] election resolver + `dev.json` + second-invocation test
- [ ] `--full` install, proof-ordered, partial-failure honest
- [ ] `--remove-all` before credential gate, partial-state tolerant
- [ ] doctor: elected producer **and why** + three lane blocks
- [ ] V7 state-diff test incl. foreign-value restore
- [ ] `-race`, both cross-compiles

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

## Next steps

Phase 13 proves the bytes; phase 14 reconciles the docs.
