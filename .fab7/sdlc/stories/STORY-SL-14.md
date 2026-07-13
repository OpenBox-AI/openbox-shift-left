# STORY-SL-14 — Idempotency hardening (stable/deterministic event_id + delivery-guarantee matrix)

**Risk:** medium (changes the `event_id` derivation — an idempotency key value on the wire; must not regress the spool/flush at-most-once guarantee)

## Source
- **Architecture:** INV-5 (idempotent ingestion — events carry a client id; retries/buffered flush never double-count), INV-3 (fail-open).
- **Backlog:** review follow-up **SL3-IDEMPOTENCY** (`shift-left-backlog.md`) — "core has no event_id field and no dev-event dedupe; a retry after a lost 200 double-counts."
- **Session:** Phase-1 debt review (2026-07-13) item #2. **Honest scope note:** the actual dedupe is server-side (EXT-core); this story hardens shift-left's *half* so the eventual core dedupe is trivially correct, and locks the client's at-most-once guarantee with tests. It is the thinnest of the four debt items.

## Inlined context (verified — builder need not re-read)
- **`event_id` today is random-but-stable:** `adapters/claude-code/mapper.go:295` returns `"cc-" + hex(randomBytes)`; generated ONCE per hook, spooled with the event, and re-read (same id) on every flush/retry — so it is already stable through the delivery lifecycle. The git-action id is already deterministic (`deploy-<env>-<full-sha>`).
- **The client already guarantees at-most-once** (`adapters/claude-code/spool.go:22-26`): delivered events are never re-sent; a ctx-bounded drain persists the undelivered remainder to a `*.rec-*.jsonl` recovery file; orphan `*.flushing.*` files are re-drained. The residual duplicate is purely server-side: on a **lost 200**, the client's own retry (`client` `maxRetries=2`) re-POSTs the same `event_id`, and core (no dedupe) stores it twice.
- **`event_id` is carried in `metadata.event_id`** (`client/payload.go` `buildMetadata`) — core has no first-class field (SL-3 docs). So dedupe, when it lands, keys on that metadata value.
- **Where the risk actually lives:** the only shift-left-originated duplicate is the client-retry-after-lost-200. Everything else is already at-most-once.

## Acceptance Criteria
- **Deterministic, collision-safe `event_id`:** derive the CC `event_id` from the event's own structural fields (session_id + event_type + a per-event distinguisher already in the hook payload, e.g. tool + high-resolution timestamp) via a hash, so (a) the SAME logical event always yields the SAME id (robust even if ever regenerated), and (b) two DISTINCT events never collide. Keep the `cc-` prefix. The git-action id stays as-is (already deterministic).
- **Explicit idempotency key on the wire:** in addition to `metadata.event_id`, send an `Idempotency-Key: <event_id>` request header on `/evaluate` (inert until core consumes it; documented as the EXT-core completing piece) — so the dedupe contract is unambiguous and header-standard.
- **Delivery-guarantee test matrix (the real deliverable):** tests proving — lost-200 retry re-sends the *same* id (no new id); a crash-then-recovery-file drain never re-sends an acked event; a distinct event never reuses another's id; id is stable across spool → rotate → flush → recovery.
- **INV-3 preserved:** no change to fail-open, retry classification, or the hot path; id derivation is pure/allocation-cheap.
- Honest docs: a comment/README line stating server-side dedupe on `event_id` (EXT-core, SL3-IDEMPOTENCY) is the completing half; shift-left guarantees stable+unique ids and client at-most-once.

## Nonfunctional Requirements
- **correctness:** id derivation must be deterministic and collision-free for distinct events (property-style test over generated events).
- **performance:** hot-path id derivation stays O(1)/cheap (no I/O, no secret).
- **compatibility:** existing spooled events (old random ids) still flush fine (id is opaque; no format assumption downstream).

## Write Scope
- `adapters/claude-code/` — `mapper.go` `newID()` derivation + tests.
- `client/` — add the `Idempotency-Key` header in `attempt` (+ doc).

## Dependencies
- **Hard:** STORY-SL-3 (client transport/header), STORY-SL-4 (mapper/spool).
- **External:** [EXT-core] server-side dedupe on `event_id`/`Idempotency-Key` — the completing half (SL3-IDEMPOTENCY); not built here.

## Invariants
- **INV-5:** stable, unique client id per event; retries/recovery never double-count on shift-left's side.
- **INV-3:** fail-open + hot-path budget unchanged.
- **INV-1:** id derives only from non-secret structural fields; never from the key/seed.

## Human Gates
| Gate | Question | Owner | Evidence Needed | Allowed Outcomes |
|---|---|---|---|---|
| G3_REVIEW | Is the id derivation deterministic+collision-safe and the at-most-once matrix intact? | brian | diff review + the delivery-matrix tests | approve / revise |

## Validation
```bash
cd adapters/claude-code && go build ./... && go vet ./... && go test -race ./...
# id determinism + collision-freedom (table/property test); retry re-sends same id; recovery-drain no re-send.
cd client && go test ./...   # Idempotency-Key header present + == metadata.event_id
```

## Stop conditions
- If the hook payload lacks a stable per-event distinguisher for a given event type (making a deterministic id ambiguous) → fall back to the current random-but-stable id for that type, keep the header + matrix tests, and document the exception; do NOT invent a distinguisher that could collide.
- If sending `Idempotency-Key` triggers any core-side rejection today → drop the header (keep `metadata.event_id`) and note it for EXT-core; never break the fail-open path.
