# ADR-0009 — Server-side idempotency and delivery receipts

Status: Accepted — **reconstructed 2026-07-31**. Shift-left side implemented;
core side is sibling-repo work.
Reconstructed from: story E8-S7 of the E8 evidence-and-assurance epic (its plan is no longer in the repo), `client/client.go`'s
`Idempotency-Key` handling and `ErrDelivery`, and the spool's recovery path.

## Context

The spool was at-most-once by construction: `Emit` logged a failed delivery and
returned nil, so the caller could not tell "delivered" from "lost" and had to
treat every event as delivered. A transport blip silently dropped governance
telemetry.

Retrying was not safe without server support. Re-sending an event whose 200 was
lost in transit would double-count it, and the exploration for E8 confirmed core
had no dedupe that applied: its existing check keys on a tuple that is empty for
lifecycle events, and unknown top-level payload keys are silently discarded.

## Decision

Make retry safe, then retry.

**Core** accepts an `Idempotency-Key` and returns the original verdict for a key
it has already seen, rather than processing the event again. This is new Redis
usage and a new response field, which is why it needs an ADR.

**Shift-left** sends a stable key per event (the event id, which is already
required to be idempotent — INV-5) and keeps it constant across retries, so a
retry after a lost 200 is recognized. `Emit` reports `ErrDelivery` so a durable
caller can distinguish a lost event from a delivered one, and the spool carries
undelivered lines into a recovery file with a bounded attempt count.

## Consequences

- At-least-once delivery with server-side dedupe, instead of at-most-once as a
  data-loss guarantee.
- The retry budget is bounded: after the cap a line is dropped and logged, since
  an event the server will never accept must not be retried forever.
- Until core ships its half, a retry after a lost 200 can double-count. The
  window is small and the direction is safe (duplicate telemetry, never a lost
  block), but it is real and the client comments should say which half is live.
- RF-B5 added `ErrUnbuildable` alongside `ErrDelivery`, because a payload that
  cannot be built is lost in a way retrying cannot fix.
