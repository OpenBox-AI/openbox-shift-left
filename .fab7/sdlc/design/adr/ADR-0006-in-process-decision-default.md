# ADR-0006: In-process enforcement decision (socket sidecar removed)

## Status
accepted — G_ADR ratified by brian 2026-07-22. **Supersedes ADR-0003** (the
Unix-socket decision daemon) and resolves its deferred **OD-SIDECAR-LIFECYCLE**.
Ratification also accepts, as-is: the deletion of conformance case C8 (no
in-process timeout to bound), the now-inert `EnforceTimeoutMS` knob (retained,
does nothing on the in-process path), and freshness resting solely on `dev sync` +
session-start staleness (no daemon re-poll; matches ADR-0005).

> Decision escalated by brian 2026-07-22: "no socket, no sidecar at all." An earlier
> draft kept `openbox sidecar serve` as an opt-in shared-daemon mode; that is now
> REMOVED. The socket transport, the `Client`, the daemon server/listener, the
> `openbox sidecar serve` command, and `OPENBOX_SIDECAR_SOCKET` are all deleted.
> In-process evaluation is the sole decision transport.

<!-- G_ADR gate. CLAUDE.md: an architecture change to how the enforce decision is
obtained is an ADR-level decision. ADR-0003 introduced a resident daemon as the
decision transport and explicitly DEFERRED "who starts/owns the daemon"
(OD-SIDECAR-LIFECYCLE) to E6-S5 build. This ADR answers it: for the common case,
nobody — the hook decides in-process. Motivated by onboarding simplification
(brian 2026-07-21): the mandatory separate daemon process was the single biggest
setup burden for developers. -->

## Context

ADR-0003 (2026-07-13) made the enforce-mode `PreToolUse` hook a **client of a
resident Unix-socket daemon** (`openbox sidecar serve`), because spike S2 proved a
synchronous `POST /evaluate` to core is ~0.8–1.6 s — far over the ~50 ms hook
budget (INV-3b / ADR-0002). The decision had to be made **locally**. At the time
the local evaluator was assumed to be an **embedded OPA** engine (arch R3), and a
resident daemon was the natural home for "hold the OPA bundle in memory and answer
a socket in single-digit ms."

Two things changed that assumption:

1. **ADR-0005 / E6-S8 replaced OPA with a pure-Go native evaluator.** The decision
   path is now `sidecar.Server.decide` — an **in-memory, no-cgo, no-network** pure
   function over a small JSON bundle (`buildOPAInput` → first-match rule eval) plus
   the Tier-1 secret detector (E6-S9). There is nothing left on the decision path
   that requires a long-lived process: loading and parsing the bundle file is
   sub-millisecond, and the hook is *already* a short-lived per-tool-call process.

2. **The daemon became the dominant onboarding cost.** In practice enforce mode
   required a developer to (a) start `openbox sidecar serve` in a second terminal
   **before** launching `claude`, (b) keep it running, and (c) set
   `OPENBOX_SIDECAR_SOCKET` so the hook and daemon agreed. If the daemon was not up,
   every decision fail-opened (or, under fail-closed, denied every tool call). This
   is exactly the "separate process is complex for the end-user" friction the
   simplification pass set out to remove — and it is the deferred OD-SIDECAR-LIFECYCLE
   question coming due.

Forces:
- **INV-3b still governs.** Whatever obtains the decision must answer within the
  hook budget and fail-open-absent. An in-process evaluation trivially satisfies
  this (microseconds, no I/O beyond one local file read).
- **INV-1 / INV-2.** The tool body handed in for redaction must stay local. An
  in-process evaluation is *strictly stronger* here than the socket: the content
  never crosses a socket boundary at all.
- **Reuse, don't rebuild (CLAUDE.md).** The in-process path must reuse the exact
  `Server.decide` evaluator + secret detector the daemon runs, not fork a second
  decision implementation — otherwise the socket and in-process paths could drift.
- **Back-compat.** Some setups may still want one resident daemon serving many
  invocations (shared bundle cache, a single sync owner). ADR-0003's socket
  transport should remain available, not be deleted.

## Decision

Make **in-process evaluation the sole decision transport**, and DELETE the ADR-0003
socket daemon entirely.

- The `decision/` module (renamed from `sidecar/`) exposes a **`Decider` seam** —
  `Decide(ctx, DecisionRequest) Decision` — with exactly one implementation,
  **`InProcessDecider`**: it constructs the in-memory engine, loads the local bundle
  file (`LoadBundleFile`, the same file `openbox dev sync` writes), and answers via
  the pure `decide` function. No socket, no listener, no resident process. An
  absent/unreadable/invalid bundle → cold-start fail-open.
- **DELETED:** the Unix-socket `Client`, the socket server/listener
  (`Server.Serve`/`handleConn`), the `openbox sidecar serve` command, the
  out-of-band `FileBundleSource`/sync loop, `DefaultSocketPath`, the
  `OPENBOX_SIDECAR_SOCKET` env, the `sidecar_socket` config field, and the
  `sourceFailOpenClient` decision source. The `sidecar/` directory/module is
  renamed `decision/` so nothing is named "sidecar".
- **`newDecider()` always returns the in-process decider**, reading
  `ResolveBundlePath()`. There is no transport choice to make.
- **The E6-S2/E6-S3 apply + failure-policy cascade is unchanged.** The `Decision`
  shape (same `Source` tags, same `FailOpen` determination via
  `isRealVerdictSource`) is identical to before, so the tighten-only apply and the
  fail-open/fail-closed policy are untouched. A fail-closed org still denies on a
  no-verdict (no-bundle) outcome.

This resolves **OD-SIDECAR-LIFECYCLE** definitively: there is no daemon, so there is
no lifecycle — the short-lived hook process is the evaluator.

## Consequences

Enables:
- **Enforcement is ambient after `openbox dev init`** with nothing to start and no
  `OPENBOX_SIDECAR_SOCKET` to set — the core of the onboarding simplification. Fewer
  moving parts, fewer failure modes (no "daemon wasn't up → everything denied").
- Tighter INV-2 posture on the default path: redaction content never leaves the
  hook process.

Costs / constraints:
- **No shared resident state.** Each hook invocation re-reads + re-parses the bundle
  file. Measured cost is sub-ms for realistic bundles. If a future bundle grows large
  enough for this to matter, the answer is to optimize the in-process load (mmap /
  cache-by-mtime), NOT to reintroduce a daemon.
- **Sync ownership.** In-process relies on `dev sync` (pull-at-init) + the E6-S8
  session-start staleness check for freshness — matching ADR-0005's chosen
  distribution model (prime-once, staleness-gated). Not a regression: the deleted
  daemon's re-poll was already back-compat-only and off by default.
- The `Decider` interface is a small public surface in `decision/`; with one
  implementation it exists mainly as a test seam.

## Alternatives Considered

1. **Keep the daemon mandatory (status quo, ADR-0003 as-is).** Rejected: it is the
   dominant onboarding cost and its "must be up first" requirement is a sharp,
   silent failure edge (fail-closed denies everything if forgotten). The evaluator
   no longer needs a resident process (ADR-0005).
2. **Keep `openbox sidecar serve` as an opt-in shared daemon (the earlier draft of
   this ADR).** Rejected: it leaves a socket transport, a `Client`, a listener, and a
   second lifecycle in the tree — plus a directory literally named `sidecar/` — for a
   setup no pilot user needs. "No socket, no sidecar at all" (brian) is simpler to
   reason about and to secure (one code path, content never crosses a boundary). If a
   shared-host need ever appears, reintroducing a transport behind the existing
   `Decider` seam is a bounded, additive change.
3. **Auto-spawn / daemonize the sidecar from the first hook.** Rejected: it keeps the
   whole daemon lifecycle (orphan reaping, socket staleness, restart-on-crash,
   per-user singleton races) purely to obtain a decision the hook can now compute
   itself in microseconds. More complexity for no benefit.
