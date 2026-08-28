# Phase 10 — telemetry mappers → contract (`:otel:`)

## Context links

- Parent: [plan.md](plan.md) · Depends: [phase-09](phase-09-telemetry-receiver-daemon.md)
- Scout: [scout-02](scout/scout-02-capture-contract-conformance.md) (the contract path)
- Contract: v1.6 from [phase-08](phase-08-adr-contract-decision.md)

## Overview

- Date: 2026-08-27 · Priority: P1 · Effort: 8h
- Implementation status: **started (2026-08-28)** · Review status: pending
- Reports: [measure-260828 attribute inventory](reports/measure-260828-otel-attribute-inventory.md) · [verification-260828 mapper](reports/verification-260828-phase-10-mapper.md)
- Turn spooled OTel records into normalized DevEvents that pass the existing
  pipeline unchanged, plus OD4's silence finding.

## Amendments recorded before implementation (2026-08-28)

Three things in this phase as written are wrong or incomplete. Recorded here
rather than discovered mid-build.

1. **The mapper cannot live in `telemetry/`.** That module's own guard
   (`telemetry/guard_test.go`) forbids `client` and `decision`, which is what
   quarantines the collector's ~492-package tree. The mapper needs both, so it
   goes in **`cli/internal/telemetryemit`**, mirroring `cli/internal/gatewayemit`
   — which exists for exactly this reason, stated in its own package doc.
2. **The span-attachment gap was real, and is now fixed.** Requirement 1's
   "bodies → observed-span fields" could not have worked: `buildPayload` attaches
   an observed span only through `gatewayObservedSpan`, which returned nil unless
   `GatewayRequestID != ""`. An otel turn with a populated `Span` was accepted,
   spooled, signed and POSTed carrying **no span at all**. Generalized to
   `observedSpan`/`observedSpanID` across all three lanes, with the gateway's
   derivation byte-identical so no stored id moves
   (`client/observedspan_test.go`; drilled).
3. **`maxAttrValueBytes` sat BELOW the wire cap** — 16 KiB against `capBody`'s
   65,536 runes. Content reaches this lane as *attributes*, so every one of them
   truncated ~4x tighter than OD1(c) blesses, and the cap's own mutation drill
   would have exercised only states the receiver cannot produce. Raised to
   4x65536; the relation needs the cross-module test that lands with the mapper.

Two corrections from the corpus (see the measure report): identity attributes are
**record**-level, not resource-level as this phase's Architecture section says;
and model bodies arrive as **`body_ref` filesystem paths**, not inline — which on
an unauthenticated loopback listener is a local-file-read oracle and needs
`os.Root` confinement. That is the highest-severity open item in the phase.

## Key insights

- **Bind at this lane's own edge.** `adapters/claude-code/usage.go`'s transcript
  projection and its mutation-tested sentinel (`usage_test.go:270–278`) must not be
  touched. Telemetry has its own decoder, its own allowlist, its own sentinel. If a
  change here makes `TestFinops_NoContentOnWire` go red, the change is wrong.
- Content ordering is fixed and asserted on outbound bytes:
  adapter redact → client `stripContent` → `capBody` (65,536 runes,
  `client/payload.go:451`). Telemetry content joins the same order, not a parallel one.
- **`contentMetadataKeys` (`client/payload.go:541–574`) is a backstop that must list
  every content key.** Two keys were once missing and routed around the gate. Any new
  telemetry content key goes in it in the same commit.
- Free text NEVER rides `signal_args` — core reads a `SignalReceived` with non-empty
  `signal_args` as a NEW USER GOAL (`age.go:112–137`) and overwrites the alignment
  goal. Route through `metadata` keys via `signalDetailKeyFor`
  (`client/payload.go:576–593`).
- OD1(c): model-call bodies attach in full and truncate at the cap. ~95% will
  truncate. That is the ruling, not a defect — but the cap's mutation drill must
  stay red-on-removal so the truncation stays *bounded* rather than *absent*.

## Requirements

1. Map, under the `:otel:` producer namespace:
   - `api_request` → `TurnStarted`/`TurnCompleted` (`llm_completion`) carrying model,
     the four token counts, cost, duration, provider request ids.
   - `api_request_body` / `api_response_body` → the observed-span body fields on the
     turn (gated, redacted, capped) — the gateway span is the shape precedent.
   - `tool_decision` → decision metadata on the matching activity, never `signal_args`.
   - `tool_result` → tool outcome (`status`, duration) where the hook lane did not
     already report it.
   - `hook_registered` / `hook_execution_*` → engine-health signal (duplicate-engine
     detection becomes continuous, not just a `doctor` warning).
2. **OD4 silence finding:** hook-lane activity with no telemetry in the same session
   window emits a finding through the existing findings loop.
3. Producer election is respected: when this lane is not elected, it emits no
   model-call turn events (phase 12 owns the election; this phase consumes it).
4. Its own sentinel test, mutation-drilled: delete the redaction ⇒ red; delete the
   cap ⇒ red.

## Architecture

- New package inside `telemetry/`: `mapper.go` + `mapper_test.go`.
- Correlate by `session.id`; join tool events by `tool_use_id`; join model calls by
  `request_id` / `client_request_id`.
- `otel_request_id` (phase 08) is derived from the provider's `request_id` when
  present, else a locally minted id — bounded and charset-checked before it reaches
  `activity_id`.
- Identity from resource attributes (`organization.id`, `user.email`,
  `session.id`, `service.version`) is **client-asserted**: it may bind sessions for
  detection; it must never be presented as proof for refusal (ADR-0021 §10 branch).

## Related code files

- new: `telemetry/mapper.go`, `telemetry/mapper_test.go`, `telemetry/sentinel_test.go`
- edit: `client/payload.go:541–574` (`contentMetadataKeys` additions)
- reference (do not edit): `adapters/claude-code/usage.go`, `usage_test.go:270–278`
- reference: `client/gatewayspan.go:43–65, 155–199` (span shape, `http_status_code`)
- reference: `client/payload.go:143–202` (`buildPayload` routing), `:350–373`
  (`turnActivityIDFor`), `:417–461` (`turnActivityOutput`/`capBody`), `:576–593`

## Implementation steps

1. Enumerate the exact attribute set per event type from the phase-09 corpus; bind
   only those.
2. Map `api_request` → turn pair; assert the four token counts and model land where
   `ExtractModelMetricsFromActivity` reads them (the gateway/hook precedent).
3. Map bodies onto the observed-span fields; reuse `gatewayObservedSpan`'s attribute
   builder so `http_status_code` cannot drift to the short spelling.
4. Route `tool_decision` free text to `metadata`; add a test that fails if it ever
   appears in `signal_args`.
5. Add every new content key to `contentMetadataKeys` in the same commit.
6. Implement the silence detector + finding; make its window configurable and its
   default explicit in ADR-0022.
7. Write the sentinel: capture OFF ⇒ no telemetry content on the wire; capture ON ⇒
   exactly the intended fields, every unrelated sentinel still absent.
8. Run the two mutation drills by hand and record the result in the phase report.

## Todo

- [x] attribute inventory from real corpus — 19 event types, per-event keys, value types
- [x] `api_request` → **TurnCompleted** (model + 4 token counts + duration + ids) — `cli/internal/telemetryemit`
- [x] election-suppression: `Policy`'s ZERO VALUE emits nothing (drilled)
- [x] identity safety: `otel_request_id` bounded + charset-checked, ':' rejected (drilled)
- [x] the otel span is marked `openbox.span_synthetic`; the in-path lanes are not (drilled)
- [x] sentinel: no content on the wire at EITHER posture, on real POSTed bytes (drilled — wholesale `Attrs`→metadata turns it red)
- [x] the `maxAttrValueBytes` ≥ 4x wire-cap relation is now a TEST, not a comment (drilled)
- [ ] bodies → observed span — **DEFERRED**: `body_ref` is a filesystem path, so this needs the `os.Root` confinement root, which follows phase 09's unmade env-key decision. No file is opened today, so no oracle exists yet; the containment must land in the SAME change as the first body read.
- [ ] `tool_decision` → metadata, never `signal_args` (+ negative test) — **DEFERRED**: needs "where the hook lane is silent", which is cross-lane knowledge the election (phase 12) supplies. Emitting now doubles Tool Health rows.
- [ ] `tool_result` → outcome where hooks are silent — **DEFERRED**, same reason
- [ ] `hook_execution_*` → engine-health signal — **DEFERRED (yagni)**: `doctor` already detects a duplicate engine; a second continuous path adds no capability
- [ ] OD4 silence finding — **DEFERRED**: needs the daemon to schedule a window check, and the daemon half is blocked. A pure function with no caller is the WithCapture shape.
- [x] `contentMetadataKeys` — nothing to add: this slice binds no content key (asserted both structurally and on the wire)
- [x] sentinel + drills run and recorded (7 drills, all red on deletion)
- [x] `usage.go` untouched (zero diff); its sentinel still green

## Success criteria

- Real corpus records map to conformant v1.6 events (validated, not asserted).
- Capture OFF ⇒ zero telemetry content on outbound bytes; ON ⇒ only intended fields.
- Deleting redaction or the cap turns the sentinel red (drilled, not claimed).
- `signal_args` is empty on every mapped signal.
- `adapters/claude-code/usage.go` has no diff.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| Double-counting: hook lane and telemetry both emit a turn for the same model call | The election (phase 12) permits one producer; namespaces are disjoint regardless | Two `llm_completion` activities per turn in a soak | **Stop and replan the election** — this corrupts every usage number downstream |
| Free text reaches `signal_args`, overwriting the alignment goal | Route via `signalDetailKeyFor` + a negative test | Negative test red, or Verify tab shows a tool name as the goal | Stop — this silently destroys alignment for the whole session |
| A new content key bypasses the gate | Add to `contentMetadataKeys` in the same commit; sentinel asserts absence with capture OFF | Sentinel shows content with capture off | Fix before merge; this is a privacy regression |
| OD1(c) truncation hides the interesting part of a body | Accepted by ruling; keep the cap bounded and drilled | — | None; documented in COVERAGE.md by phase 14 |
| **Assumption: telemetry token counts match transcript-derived ones.** | Cross-check both lanes on the same session during phase 13 | Counts diverge beyond rounding | **Adjust**: prefer the elected producer, record the discrepancy in the phase report; do not average |
| Silence detector false-positives on a quiet session | Window tuned against the real corpus; finding, not HALT, is the client-side act | Findings noise in normal sessions | Tune the window; the HALT decision stays server-side by design |

## Security considerations

- This phase is where telemetry content first reaches the wire. Every guarantee is a
  mechanism, not a structure: gate, redact, cap — each fallible, each asserted on
  outbound bytes.
- The redactor's reach is keyword-driven plus the retained entropy fallback, as
  phase 06 measured it: an unlabelled high-entropy value inside a 290 KB body is
  as invisible as in a 64 KiB one (C39's stated limit, now at larger volume). Do
  not lower the entropy floor to compensate — below 4.0 every git SHA matches and
  the enforce-path redactor rewrites file bodies.
- Asserted identity (`organization.id`, `user.email`) is detection-grade only.
  Never let it gate a refusal.

## Next steps

Phase 11 builds the transport lane; phase 13 builds replay conformance over this
mapper.
