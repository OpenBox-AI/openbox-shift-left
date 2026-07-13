# ADR-0003: Home of the local enforcement decision sidecar

## Status
accepted — G_ADR ratified by brian 2026-07-13 (jointly with ADR-0002).

<!-- G_ADR gate (Epic E6, story E6-S5). Decision owner: brian. CLAUDE.md: "a new
table/endpoint/service requires an ADR" — E6-S5 introduces a NEW resident
component (a daemon), the first stateful, long-lived process in a repo whose
Phase-1 runtime is entirely stateless fork/exec hooks. This ADR records the
structural choice that novelty forces: where the sidecar lives and how it is
invoked. It is the OD6 counterpart to ADR-0002's INV-3b — the carve-out's bounded
wait is only real because a local sidecar answers it. Ratified jointly. -->

## Context

Spike **S2** (DONE 2026-07-13) proved that a synchronous enforcement decision
**cannot** be a direct HTTP call to openbox-core: `POST /evaluate` measured
**~0.8–1.6 s** (the Temporal governance workflow, loopback floor) — ~16–33× a
tolerable ~50 ms per-tool-call budget. The decision must be made **locally**, from
a synced policy/OPA bundle, in the single-digit-ms band (S2 measured signed local
transport at ~3.6 ms and fork/exec at ~1.5 ms). This is **OD6 = command → local
sidecar**, now confirmed mandatory, and it is the mechanism that makes ADR-0002's
INV-3b bounded wait real rather than aspirational.

E6-S5 therefore introduces a **resident local daemon**: `PreToolUse` (enforce
mode) asks it over a Unix domain socket, it answers from an OPA bundle it syncs
from core **out-of-band** (off the hot path — arch R3, "OPA bundles are already
distributed"), and if it is absent/slow/down the hook fails **open** (OD9). This
is a genuinely new kind of component for this repo and must have a home.

Forces:
- **New structural novelty.** Phase-1 is entirely stateless: every hook is a
  fork/exec of the `openbox` binary that spools and exits (INV-3, WIRE-2). A
  long-lived, socket-listening, policy-holding daemon is the first resident
  process — it needs a home the composition root can build and the hook path can
  reach, without polluting the stateless observe path.
- **INV-3b (ADR-0002)** governs it: the daemon must answer within the ~50 ms hook
  budget and fail-safe-absent → fail-open. Its home must not entangle it with any
  unbounded/networked work on the hot path.
- **INV-1 / INV-2:** the sidecar handles a policy bundle and per-call tool inputs
  (for redaction, E6-S4) locally; secrets and content must never leave the
  machine or reach a log/argv. A dedicated module keeps that boundary auditable.
- **Reuse, don't rebuild (CLAUDE.md):** the decision reuses the **existing** OPA
  bundle distribution + Guardrail primitives — the sidecar is a local *evaluator*
  of already-distributed policy, not a new policy service. No new core
  table/endpoint (the async telemetry emit stays the existing `/evaluate`).
- **Multi-module repo:** each component (`cli`, `client`, `provider`,
  `adapters/*`, `contracts/*`, `actions/*`) is its own `go.mod` wired by `replace`
  directives; there is no `go.work`. A new component follows that convention.
- **WIRE-2 precedent:** the hook engine was deliberately unified into the single
  `openbox` binary (`openbox hook <provider> <event>`); Phase-1 ships one binary,
  not a zoo. The sidecar should not silently reintroduce a second shipped binary.

## Decision

Introduce a new top-level module **`sidecar/`**
(`github.com/openbox-ai/openbox-shift-left/sidecar`, own `go.mod`) that owns the
resident decision daemon: the Unix-domain-socket server, the local OPA-bundle
evaluator, the out-of-band bundle sync client, and the decision types the enforce
hook consumes.

- **Invoked as a subcommand of the existing `openbox` engine**
  (`openbox sidecar serve`), not as a second shipped binary — the `cli`
  composition root imports `sidecar/` and wires the subcommand, exactly as WIRE-2
  folded the hooks into the one binary. One artifact ships; the daemon is a mode
  of it.
- **The enforce-mode hook is a client of the socket, not of core.** E6-S1's
  `PreToolUse` (enforce) dials the sidecar's Unix socket with a hard ~50 ms
  timeout (ADR-0002) and returns the `client.Evaluation`; on dial-fail/timeout it
  fails open. `sidecar/` exports the small socket protocol + client both sides
  share. The Phase-1 async observe spool path (`adapters/claude-code`) is
  **untouched** — observe/advisory sessions never talk to the sidecar.
- **`/evaluate` stays the async telemetry channel only** (S2 finding 3): the
  sidecar emits observations to it out-of-band, fire-and-forget, never on the
  synchronous decision path. No new core endpoint.
- **`sidecar/` depends on `client/`** (to emit telemetry + carry the shared
  `Evaluation`/verdict types) and reuses the existing OPA-bundle format; it does
  **not** depend on `cli` or the adapters (they depend on it / on its protocol).
  `cli` and `adapters/claude-code` add a `require` + `replace → ../sidecar`.

## Consequences

Enables:
- E6-S5 builds the daemon in one module with a clean boundary; E6-S1 (sync
  enforce hook) and E6-S4 (local redaction) consume its socket protocol; E6-S3
  (fail-open/fail-closed policy) configures its timeout behavior. ADR-0002's
  bounded wait becomes real code.
- One shipped artifact preserved (WIRE-2 invariant): `openbox sidecar serve` is a
  mode of the same binary developers already install via `dev init`.

Costs / new constraints:
- **First resident process in the repo.** A lifecycle now exists (start / own /
  restart / fail-safe) that Phase-1 never had. Its failure mode is bounded by
  design: absent or unresponsive → the hook fails open (allow, degrade to
  observe), so a dead sidecar never blocks the dev loop.
- One more module + `replace` line in `cli` and `adapters/claude-code`
  (multi-module bookkeeping; no `go.work` to hide it).
- **New invariant (structural):** the sidecar answers only from its **local**
  synced bundle within the INV-3b budget; it MUST NOT put a network round-trip
  (core `/evaluate`, backend, Guardrail API) on the synchronous decision path.
  Bundle sync and telemetry emit are out-of-band only.

Follow-on (explicit human decisions, NOT settled here — layered on top of this
module home, mirroring ADR-0002's deferred ODs):
- **OD-SIDECAR-LIFECYCLE:** who starts/owns the daemon — a per-user
  systemd/launchd user service vs spawned-on-demand by `dev init` / first hook.
  S2 sketched both; the fail-safe-absent → fail-open contract holds either way, so
  this is a rollout-ergonomics decision, deferred to E6-S5 build (owner: brian).
- **OD-SIDECAR-SYNC:** bundle pull interval / push + staleness tolerance (S2 Q4) —
  measured/tuned during E6-S5, off the hot path.
- Sidecar OPA-decision latency (expected single-digit ms) is measured once the
  daemon exists (S2 follow-on, non-blocking).

## Alternatives Considered

1. **Direct synchronous HTTP to core `/evaluate` (no sidecar / no new module).**
   **REJECTED by spike S2 (2026-07-13):** ~0.8–1.6 s per decision, ~16–33× budget,
   even on loopback. Every Bash/Edit/Read would stall a second-plus. This is the
   decision that forces a local component at all.
2. **Fold the daemon into an existing module (`adapters/claude-code` or `cli`).**
   Rejected: the sidecar is provider-agnostic (it serves every future adapter —
   Codex/Cursor — over the same socket), so it does not belong under one adapter;
   and `cli` is the composition root (it *wires* components, it should not *own* a
   resident server's OPA/socket logic). Keeping it a standalone module preserves
   the "adding a provider = zero core change" seam (INV-7 / arch §1b).
3. **Ship a separate `openbox-sidecar` binary.** Rejected: WIRE-2 deliberately
   unified everything into the one `openbox` binary (`openbox hook …`); a second
   shipped binary reverses that. `openbox sidecar serve` (a subcommand backed by
   the `sidecar/` module) gives the daemon without a second artifact to install.
4. **Put the socket protocol/decision types in `client/`.** Rejected: `client/`
   is the runtime *egress transport* (AIP signing, `/evaluate`); the local IPC
   contract is a separate concern (same reasoning ADR-0001 used to keep the
   install-time SPI out of `client/`). `sidecar/` may depend on `client/`, not the
   reverse.
