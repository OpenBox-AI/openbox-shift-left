# STORY-SL-10 — Signing / response error diagnostics (reason-code mapping)

**Risk:** low (diagnostics only; stays fail-open, never changes control flow or blocks a tool call)

## Source
- **Architecture:** `architecture.md` D7 (observe = report-only), INV-3 (observation never blocks).
- **SDK parity target:** `openbox-temporal-sdk-python/openbox/errors.py` `map_signing_error()` — maps openbox-core's machine reason codes to actionable exceptions. Shift-left currently drops them silently.
- **Session:** SDK↔shift-left gap analysis (2026-07-13) — bucket #2, item "signing-error mapping".

## User Value
When `/evaluate` rejects a dev-runtime event (bad signature, replayed nonce, no verifier, clock skew), the operator sees a single actionable diagnostic line instead of a silent fail-open drop — so a mis-onboarded dev agent is diagnosable in seconds rather than "no events appear, no error".

## Inlined context (verified — builder need not re-read)
- **Where events are dropped today:** `client/client.go` `Emit` logs `"openbox: dropping event <id> (<type>): <err>"` on any `post` error and returns `(VerdictUnknown, nil)`. `attempt` returns a generic error; the non-2xx **response body is not parsed** for a reason code.
- **SDK reason codes (from `errors.py:74-135`):** `signature_invalid`, `nonce_replayed`, `did_agent_mismatch`, `verifier_not_configured`, `timestamp_outside_window` / `timestamp_skew`. Core returns these as a machine field in the error JSON (typically `{ "reason": "...", "message": "..." }` or `error`/`code` — the builder confirms the exact key against core's `agent.go` verifier error path before pinning).
- **Fail-open is non-negotiable (INV-3):** this story only enriches the log/diagnostic; `Emit` MUST still return `(VerdictUnknown, nil)` on any transport/4xx failure. No new error is surfaced to the caller, no retry-policy change.

## Acceptance Criteria
- On a non-2xx `/evaluate` response, the client captures the (bounded) response body and, when it carries a recognized reason code, logs a **mapped, actionable** diagnostic — e.g. `verifier_not_configured → the dev agent has no KMS verifier; register signing-off or set signing_required=false (RUNBOOK §3.2)`; `nonce_replayed → a buffered event was re-sent after a lost 200 (INV-5); safe to ignore unless persistent`.
- All five SDK reason codes are mapped; an unrecognized reason logs the raw code + status (no crash, no guess).
- The diagnostic is emitted to the client logger only (stderr for the CLI/hook path); **no stdout write** (INV-3 for the hook binary) and **no secret** (INV-1) — never log the key, seed, signature, or nonce value.
- `Emit` behavior is unchanged: still fail-open `(VerdictUnknown, nil)`; retry classification (5xx/429 retryable, 4xx terminal) unchanged.
- Response-body read is bounded (reuse the existing 1 MiB cap) so a hostile/huge error body can't OOM.

## Nonfunctional Requirements
- **security:** no key/seed/signature/nonce/DID-secret in any diagnostic (INV-1). Reason strings are categories, not content (INV-2).
- **reliability:** parsing the error body is best-effort — a non-JSON or empty body degrades to "status N, no reason" and still fail-open drops.
- **observability:** exactly one diagnostic line per dropped event (no log spam on retries — map on the terminal failure only).

## Write Scope
- `client/` — new `client/signingerr.go` (reason→guidance map + parse); a small hook in `client/client.go` `attempt`/`post`/`Emit` to capture the terminal 4xx body and pass the mapped reason to the drop log.

## Dependencies
- **Hard:** STORY-SL-3 (the client transport this enriches).
- **External (assumed-satisfied):** [EXT-core] — core actually returns these reason codes for dev event types; until EXT-core, a stock core 400s with `invalid event_type` (already a distinct, mappable case worth adding).

## Invariants
- **INV-1:** no secret in diagnostics.
- **INV-2:** reason categories only, never content.
- **INV-3:** fail-open preserved; no control-flow change; no stdout on the hook path.

## Human Gates
| Gate | Question | Owner | Evidence Needed | Allowed Outcomes |
|---|---|---|---|---|
| G3_REVIEW | Is the reason map correct and the fail-open contract preserved? | brian | diff review + tests | approve / revise |

## Validation
```bash
cd client && go build ./... && go vet ./... && go test ./...
# unit: table test feeding each reason-code body → asserts the mapped guidance string;
#       unrecognized reason → raw-code fallback; non-JSON body → "status N" fallback;
#       Emit still returns (VerdictUnknown, nil) in every case (fail-open preserved);
#       log-safety: no key/seed/nonce/signature substring in any emitted diagnostic.
```

## Stop conditions
- If core's error envelope key for the reason code cannot be confirmed against the verifier path → map on the fields that ARE present (status + any `message`), log the raw body shape once, and route a note to re-confirm at EXT-core; do NOT guess a schema.
