# ADR-0007 — One shared dev-config module across adapters

Status: Accepted — **reconstructed 2026-07-31**.
Reconstructed from: `adapters/common/devconfig`, the `DevConfig` aliases in both
adapters' `creds.go`, `adapters/codex/README.md`, and the OD-SL7-SHARE note in
the SL-7 memory record.

## Context

Building the Codex adapter meant deciding what it should share with Claude Code.
The dev config — dev.json's schema, the env-var contract, the resolution
precedence, the secret-store coordinates — is not tool-specific: it describes the
developer's OpenBox identity and posture, which is the same whichever tool is in
front of it.

Porting it would have produced two definitions of one contract, and they would
have drifted.

## Decision

`adapters/common/devconfig` owns the dev config: the `DevConfig` schema, the
`OPENBOX_*` environment contract, the resolution precedence
(managed > env > user > default), and credential resolution. Adapters alias the
types rather than redefining them.

Tool-specific settings stay with their adapter.

## Consequences

- One dev.json contract, whatever the provider — which is what let RF-B2 fix the
  re-init posture bug in one place for every adapter.
- The shared-module pattern this established is what RF-S1 and RF-S2 extended to
  the rest of the engine, once it became clear the adapters had duplicated far
  more than the config.
- Later additions — the managed/mandate layer, posture resolution — landed here
  by the same argument.
