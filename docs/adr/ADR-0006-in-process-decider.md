# ADR-0006 — In-process decider; retire the socket sidecar

Status: Accepted — **reconstructed 2026-07-31**.
Reconstructed from: getting-started.md, `decision/inprocess.go`, `install.sh`,
`.goreleaser.yaml`, and the `--enforce` flag's persistence into dev.json.

## Context

Enforcement worked, and nobody could turn it on. It required starting a
sidecar daemon, keeping it running, and setting runtime environment variables
for the hook to find it. Every one of those is a step a developer forgets and an
operator has to support — for a decision that ADR-0005 had already made purely
local and in-memory.

brian's framing was blunt: "no socket, no sidecar at all".

## Decision

**Remove the sidecar entirely.** No `Client`, no listener, no
`openbox sidecar serve`, no `OPENBOX_SIDECAR_SOCKET`. The PreToolUse hook loads
the local bundle and decides in-process, in microseconds. The module is renamed
`sidecar` → `decision`.

**Persist the posture, not the environment.** `openbox init --enforce` writes
enforce, tier2 and findings into dev.json, so the runtime hook needs no
environment variable at all.

**Ship a prebuilt binary.** `install.sh` downloads a GoReleaser-built binary,
falling back to a source build.

The result is `curl | bash`, then `openbox init --enforce`, and governance is
ambient.

## Consequences

- Nothing to start, nothing resident, nothing to leave running.
- dev.json becomes the sole carrier of the enforce posture — which made
  re-init's silent overwrite of those fields a real downgrade, fixed in RF-B2.
- The per-call decision timeout knob became meaningless (there is no wait to
  bound) and lingered as dead code until RF-S1 removed it.
