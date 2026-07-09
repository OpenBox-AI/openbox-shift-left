# ADR-0001: Home of the shared provider install-time SPI

## Status
proposed

<!-- G_ADR gate (STORY-SL4-WIRE-1). Decision owner: brian. The "wired" direction
was already decided by brian 2026-07-08 (backlog §"Wiring stories"); this ADR
records the *structural* choice it forces — where the SPI type lives — per
CLAUDE.md ("a new module/service requires an ADR"). -->

## Context

`openbox dev init --provider <tool>` (STORY-SL-2) delegates each tool's native
config to a per-tool **Installer** — the install half of the generic adapter
seam (architecture §1b: `register` / `emit` / `apply` / `capabilities`). SL-2
defined the `Installer` interface + `CredentialRef` inside
`cli/internal/provider`, and the Claude Code adapter (STORY-SL-4) defined its
*own* structurally-identical `CredentialRef` + installer methods. Nothing forced
the two to agree — the adapter did not implement the CLI's interface.

SL4-WIRE-1 makes `dev init --provider claude-code` actually install the plugin,
which requires the CLI to hold a value of the adapter's installer typed as the
SPI interface. That is impossible while the interface lives under the CLI's
`internal/` boundary: an adapter module cannot import `cli/internal/...`. So the
interface + credential type must move somewhere both the `cli` module and every
adapter module can import.

Forces:
- **INV-1**: the SPI carries secret-store *coordinates* + the non-secret DID,
  never the obx_ key or Ed25519 seed value. The type must make that shape the
  only shape.
- **INV-7 / architecture §1b**: "adding a provider = one adapter, zero core
  change." Adapters must depend on a stable, dependency-free SPI, not on the CLI.
- **No import cycle**: the concrete registry (name → installer) references the
  adapters; the adapters reference the SPI. If the registry lived beside the SPI,
  the SPI module would import the adapters that import it — a cycle.
- This is a multi-module repo (each of `cli`, `client`, `adapters/*`,
  `contracts/*` has its own `go.mod`, wired by `replace` directives); there is no
  `go.work`.

## Decision

Introduce a new top-level module **`provider/`**
(`github.com/openbox-ai/openbox-shift-left/provider`, own `go.mod`, **zero
dependencies**) that owns the SPI surface: `Installer`, `CredentialRef`, `Name` +
the name constants, `ErrNotBuilt` / `ErrUnknown`, `Supported()`, and a generic
`Stub` for unbuilt providers.

- The **Claude Code adapter** implements `provider.Installer`; its
  `CredentialRef` becomes a type alias of `provider.CredentialRef` (it no longer
  defines a distinct type).
- The **concrete registry** (name → installer, incl. `claudecode.Installer{}` and
  the codex/cursor stubs) lives in `cli/internal/providers` — the CLI's
  composition root, the one place that imports both the SPI and the adapter
  modules. It stays in `cli` precisely so the SPI module never imports an
  adapter (breaks the cycle).
- `cli` and `adapters/claude-code` each add a `require` + `replace → ../provider`.

Rejected alternatives are listed below.

## Consequences

Enables:
- `dev init --provider claude-code` installs the real plugin bundle + non-secret
  dev config (replacing the SL-2 stub), typed through one shared interface.
- SL-7 (Codex) and SL-8 (Cursor) slot in by adding a `require` on `provider/` and
  registering their installer in `cli/internal/providers` — **zero change** to
  the SPI module or to `dev init`.

Costs / new constraints:
- One more module + `replace` line in `cli` and each adapter (multi-module
  bookkeeping; no `go.work` to hide it).
- **New invariant (structural):** the `provider/` module MUST stay
  dependency-free — it may never import an adapter, the CLI, or the client.
  Concrete installers/registries live in the composition root, never here.
- Revises SL-2's G3-approved `cli/internal/provider` package by relocating the
  interface out of it; `cli/internal/provider` is deleted. Flagged for G3.

Follow-on:
- SL4-WIRE-2 folds the hook entrypoint into the `openbox` engine; it consumes
  this SPI unchanged.

## Alternatives Considered

1. **Keep the SPI in `cli/internal/provider`.** Rejected: adapters cannot import
   another module's `internal/` package, so the adapter could never implement the
   interface — the whole point of WIRE-1.
2. **Put the SPI in the `client/` module (already shared).** Rejected: `client`
   is the *runtime* egress transport (AIP signing, `/evaluate`); the install-time
   seam is a separate concern, and folding it in would make every hook binary
   carry install code and vice-versa. Keep the two seams in separate modules.
3. **Put the registry in the `provider/` module too (one module).** Rejected:
   the registry references the adapters, the adapters reference the SPI →
   import cycle. The registry must stay in the composition root (`cli`).
4. **A single repo-wide module (collapse the multi-module layout).** Rejected:
   out of scope for this story and a much larger structural change; the repo has
   deliberately chosen per-component modules.
