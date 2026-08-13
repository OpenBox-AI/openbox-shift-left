# ADR-0003 — The decision engine is its own module

Status: Accepted — **reconstructed 2026-07-31**. Superseded in part by ADR-0006.
Reconstructed from: `decision/go.mod`'s dependency-direction note, `decision/doc.go`,
and CLAUDE.md's "ADR-0002/0003 (INV-3b carve-out, sidecar module)".

**Scope narrowed by [ADR-0017](ADR-0017-inline-policy-evaluation.md)
(2026-08-13):** policy is no longer evaluated locally, so this module keeps only
secret detection and redaction. The no-network-I/O property below still holds for
what remains, but it is now a property of content protection rather than the
INV-3b clause it was justified by — that clause is retired.

## Context

E6 needed somewhere to evaluate policy locally for the enforce gate. It had to be
importable by every adapter and by the CLI, must not drag the egress client's
concerns into the decision path, and — per INV-3b — must do no network I/O.

At the time the design was a local sidecar the adapters reached over a unix
socket, with `openbox sidecar serve` starting it.

## Decision

The decision engine is a separate module, `decision`.

The dependency direction is fixed: **decision depends on client, never the
reverse.** The engine reuses the client's verdict and content types so a local
decision and a server evaluation are the same shape, but the egress client must
stay usable without pulling the policy engine in.

## Consequences

- The module boundary makes INV-3b checkable: `decision` importing anything that
  does network I/O would be visible in its `go.mod`.
- **Superseded in part by ADR-0006**, which removed the socket, the listener and
  the `sidecar serve` subcommand entirely, and renamed the module from `sidecar`
  to `decision`. The module and the dependency rule survive; the transport does
  not. Comments describing a daemon lingered until RF-S9 removed them.
