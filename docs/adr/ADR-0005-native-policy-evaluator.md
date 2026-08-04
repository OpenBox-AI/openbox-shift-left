# ADR-0005 — Native Go policy evaluator instead of embedded OPA

Status: Accepted — **reconstructed 2026-07-31**.
Reconstructed from: `decision/builder.go`, `decision/regoparity.go`,
`cli/internal/policysync`, and ADR-0008's reference to "ADR-0005 (bundle sync +
staleness pin)".

## Context

The enforce gate needs a verdict from org policy before a tool runs. The backend
holds that policy as a builder config, which it evaluates via OPA.

The first approach embedded OPA in the engine and evaluated the generated rego
locally. That reverses here.

## Decision

Two parts.

**Decision 1 — evaluate the builder config natively.** The engine implements the
builder's semantics in pure Go: ordered rules, first match wins, default allow.
No OPA, no cgo, no rego runtime in a hook that runs on every tool call.

The obligation this creates is parity: the local evaluator must agree with what
the backend's OPA would decide for the same input. The primitives that carry
that obligation are isolated (`decision/regoparity.go` since RF-S9) and their
known deviations — composite ordering, in particular — are documented rather
than assumed away.

**Decision 2 — pull at init, check staleness at session start.** The bundle is
fetched by `openbox dev sync` (and at the end of `openbox init`), written locally,
and pinned by policy id and updated-at. Session start compares the local pin
against the control plane; a mismatch warns under fail-open and marks the
session stale under fail-closed. There is no polling and no push channel.

## Consequences

- A hook does no network I/O to decide (INV-3b holds by construction).
- A developer can run on stale policy between syncs. That is the trade the
  staleness check exists to make visible.
- Raw-rego policies cannot be evaluated natively; they degrade to a fail-open
  local bundle, disclosed in the sync output.
