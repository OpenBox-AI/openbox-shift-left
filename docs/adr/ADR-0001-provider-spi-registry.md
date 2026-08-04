# ADR-0001 — Provider SPI, with the registry in the composition root

Status: Accepted — **reconstructed 2026-07-31** (see `docs/adr/README.md`).
Reconstructed from: `provider/provider.go`, `cli/internal/providers/providers.go`
(which cites this ADR for the cycle-breaking rule), `decision/doc.go`.

## Context

`openbox init` has to write each developer tool's native configuration, but
the CLI must not own that content: only the adapter knows its tool's config
shape. The obvious arrangement — the CLI importing adapters and adapters
importing the CLI's types — is an import cycle.

## Decision

A standalone `provider` module holds the interfaces and the shared types. Both
the CLI and every adapter import it; it imports nothing.

The concrete registry — which provider name maps to which adapter — lives in the
CLI's composition root (`cli/internal/providers`), not in the SPI module. Putting
it in the SPI would force the SPI to import the adapters while the adapters
import the SPI, which is the cycle this exists to avoid.

`openbox init` owns identity and credentials. It does not own provider config
content. A recognized provider whose adapter has not shipped is a `Stub`:
`Available()` is false, `Plan()` prints the manual configuration, and `Install()`
returns `ErrNotBuilt` so the command exits non-zero for that provider.

An `Installer` receives secret-store *coordinates* and the non-secret DID, never
a secret value (INV-1).

## Consequences

- Adding a provider is one registry entry plus one adapter module.
- The composition root is the only place that imports adapters. RF-S4 later made
  that mechanical with a test, after the CLI had drifted into calling one
  adapter's helpers for provider-neutral work.
- RF-S4 also extended the SPI with `HookEngine`, the runtime half. The original
  package doc claimed four legs while only `Installer` existed.
