# ADR-0002 — INV-3b: enforcement may block, but only in-process

Status: Accepted — **reconstructed 2026-07-31** (see `docs/adr/README.md`).
Reconstructed from: `decision/doc.go`, the INV-3b references throughout the
enforce path, and `adapters/*/enforce_conformance_test.go`.

## Context

INV-3 says the developer runtime is fail-open: OpenBox never blocks, delays or
fails a tool call. Phase-2 enforcement contradicts that by design — blocking a
dangerous call before it runs is the entire point.

The invariant could not simply be dropped. Everything INV-3 protects against —
a hung hook, an outage stalling a developer, telemetry failure breaking work —
still has to hold.

## Decision

INV-3b is a bounded carve-out from INV-3, not a replacement.

Enforcement may deny a tool call, and only under these conditions:

1. **Opt-in.** Enforce is off by default; observe mode is byte-identical to
   observe-only.
2. **Pre-execution and synchronous.** A block happens before the tool runs, or
   not at all. There is no after-the-fact interruption.
3. **In-process, no I/O on the decision path.** The verdict comes from a local
   policy bundle evaluated in memory. No network call, no IPC, nothing to be
   down. (ADR-0003, then ADR-0006, are about how this is realized.)
4. **Fail-open on every fault.** An absent, unreadable or unparseable bundle, a
   panic, a marshal error, a nil stdout — all degrade to proceed. Fail-closed is
   a separate, explicit per-org opt-in.
5. **Tighten-only.** Enforcement can add a deny, an ask or a redaction. It can
   never grant a permission the tool would not otherwise have.

## Consequences

- The conformance suites (C1…C10) exist to pin these properties per provider.
- Tier-2, which does make a network call, is bounded by a wall clock kept under
  the provider's hook timeout, and fails open when the budget is exhausted.
- "Silently ungoverned" is the failure mode to design against: fail-open is
  correct, but it must be visible in the audit rather than quiet.
