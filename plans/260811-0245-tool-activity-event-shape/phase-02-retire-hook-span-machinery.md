# Phase 02 — Retire the hook-span machinery

## Context links

- Parent: [plan.md](plan.md)
- Depends on: [Phase 01](phase-01-wire-activity-lifecycle.md) (nothing may reference the
  builders before they are deleted)
- Dissolves the mirror obligation recorded in `docs/adr/ADR-0004-base-wire-unification.md`
  §Amendment — the ADR text is updated in [Phase 03](phase-03-contracts-adr-docs.md)

## Overview

- **Date:** 2026-08-11
- **Description:** Delete the hand-maintained Go mirror of the base SDK's hook-span
  contract and the span builder it feeds, now that no shift-left payload carries a span.
- **Priority:** P1 (it is the payoff, and dead enforcement-path code is a liability)
- **Implementation status:** complete
- **Review status:** not started

## Key insights

1. **This is the single largest simplification available.** ADR-0004 §Amendment names
   `client/hookspan.go` as "the known weak point: nothing mechanically compares it
   against upstream, so it guards local edits only." Retiring the span layer retires the
   obligation outright — no upstreaming, no corpus, no push access needed.
2. **The surface is contained, and it was measured.** References outside the two files:
   `client/spanbuilder_test.go` (55), `client/hookspan_test.go` (19),
   `client/payload.go` (13 — handled in Phase 01), `client/payload_hook_test.go` (9),
   `adapters/codex/wire_test.go` (3), `adapters/claude-code/conformance_parity_test.go`
   (2). No non-test **adapter** file references any of it. Note the `payload.go` and
   `spanbuilder.go` `AssertHookWireShape` hits are all doc comments, not calls.
   `adapters/common/hookflow/duration.go:118` and
   `contracts/dev-event/conformance/schema_guard_test.go:66` mention `buildPayload` in
   comments across module boundaries — not callers, and `buildPayload` survives anyway.
   The one production caller of the *derivations* — `client/approval.go`'s
   `ApprovalKeyFor` — is resolved in Phase 01, which keeps them under new names.
3. **`contracts/dev-event/conformance/` does not touch the wire span** (grep: zero span
   references in `schema.go`). It validates the *adapter-facing* schema, which is frozen.
   So conformance survives untouched — a useful confirmation that the two-layer split
   ADR-0004 established is real and not just documented.
4. **Two adapter test files reference the mirror, not the engine.** They assert wire
   parity; they become assertions about the activity shape. No adapter production code
   changes, which is the invariant to protect.

## Requirements

- Delete `client/hookspan.go` and `client/spanbuilder.go` with their tests.
- Rewrite the two adapter parity tests to assert the activity shape instead of the hook
  shape — keeping their intent (both adapters serialize identically).
- Keep `TraceContextFrom`/trace-id derivation **only if** something outside the span
  builder uses it; otherwise delete it too. Census: 5 references — 1 comment in
  `payload.go`, 1 in `spanbuilder.go`, 3 in `spanbuilder_test.go`. No other owner, so it
  goes with the file.
- No production file under `adapters/` changes.
- `Span.InvocationID` and `Span.OperationID` survive: they feed `activity_id` and the
  duration-stash key and are part of the frozen adapter contract. `Span.Stage` also
  survives, unread — validation decision 4.
- `workflowIDFor` / `activityIDFor` (Phase 01's renamed shared derivations) are **not**
  part of this retirement. Deleting them breaks `ApprovalKeyFor`.

## Architecture

Removing the span layer removes an entire vocabulary from the repo: `HookType`,
`CommonRootFields`, `FamilyRootFields`, `DefaultKind`/`KindFor`, `AssertHookWireShape`,
the 16-/32-hex id regexes and the deterministic `span_id`/`trace_id` derivations.

What remains of the client is one serializer, one signer, one verdict parser — the shape
`client/README.md` describes. The `client/leakscan_test.go` and `errcontract_test.go`
guards are unaffected and keep covering the transport.

## Related code files

| File | Action |
|---|---|
| `client/hookspan.go` | delete |
| `client/hookspan_test.go` | delete |
| `client/spanbuilder.go` | delete (audit `TraceContextFrom` / `newHexID` for other callers first) |
| `client/spanbuilder_test.go` | delete |
| `client/payload_hook_test.go` | delete or fold surviving cases into the activity tests |
| `client/testdata/golden/hook_*.json` | delete (4 files; replaced in Phase 01) |
| `adapters/codex/wire_test.go` | rewrite the 3 assertions against the activity shape |
| `adapters/claude-code/conformance_parity_test.go` | rewrite the 2 assertions |
| `adapters/**/*.go` (non-test) | **no change — verify with a diff** |

## Implementation steps

1. Re-run the reference census after Phase 01 lands; Phase 01 should already have
   removed every `client/payload.go` reference.
2. Audit `TraceContextFrom`, `sessionTraceID` and `newHexID` for callers outside
   `spanbuilder.go`. Delete what is unreferenced; keep and relocate anything still used.
3. Delete the four files and the four hook fixtures.
4. Rewrite `adapters/codex/wire_test.go` and
   `adapters/claude-code/conformance_parity_test.go` to assert: both adapters produce
   `ActivityStarted` for a `ToolCall` and `ActivityCompleted` for a `ToolResult`, with
   identical envelope fields for equivalent input. Preserve the parity intent — this is
   the test that stops the two adapters from drifting.
5. `go build ./...` per module; `go vet ./...`; confirm no unused-symbol or dead-import
   fallout.
6. `git diff --stat -- adapters/` must show only the two test files.

## Todo list

- [x] Post-Phase-01 reference census — every symbol confined to the 4 deleted files, except `AssertHookWireShape` in the 2 adapter tests
- [x] Audit and resolve `TraceContextFrom` / `sessionTraceID` / `newHexID` — no owner outside `spanbuilder.go` (census: 2+3 and 4+4 hits, all in the file and its own test); `sessionTraceID` already went in Phase 01. All deleted, nothing relocated
- [x] Delete 4 source files (fixtures went in Phase 01)
- [x] Rewrite 2 adapter parity tests
- [x] `go build` / `go vet` / `go test` per module — 11/11 clean
- [x] Verify `adapters/` diff contains no production file — exactly 2 files, both `_test.go`

### What the rewritten parity tests assert

`client.AssertHookWireShape` guarded a real property and its deletion would have
left a hole, so it was replaced rather than dropped: `assertActivityWireShape`
now lives in **both** adapter test files, asserting the same envelope contract —
correct `event_type`, no `spans`/`span_count`/`hook_trigger`, the eight required
envelope fields non-empty, `workflow_type=developer-session`, and no client-set
`semantic_type`.

They are deliberate copies, not a shared helper. The adapters are separate Go
modules, and the property under test is that both produce the same shape
*independently*; a helper they both called could drift with them and still pass.
This is the phase's own risk-row mitigation ("compare the two adapters' payloads
to each other, not to a hardcoded literal") expressed across a module boundary.

`adapters/claude-code` had no live wire assertion at all — its parity file was a
documentation matrix. It now has `TestWire_ToolEventsAreActivityPairs`, driving
the real client against a loopback core, which is a net increase in coverage.

The matrix's wire-shape row was **not** simply reworded: its `parity` claim
against the base SDK's `assert_hook_wire_shape` became false, so it split into a
`go-extension` row (our activity envelope, no base analog) and a `base-unmapped`
row (the base assertion we no longer mirror, with the reason). Leaving it as
`parity` would have been a false cross-repo claim in a governance product.

### Deviation

Success criterion 1's grep is not literally empty: `AssertHookWireShape` survives
in **two doc comments**, both saying what the new helper replaced. No code
references it. Removing the name would cost the next reader the reason the helper
exists; the ADR names it in prose for the same reason.

### Verified

- `client/leakscan_test.go` needed no extension. It scans the **whole payload**
  for canaries on a `ToolCall` and a `ToolResult`, so `activity_input` and
  `activity_output` are covered by construction — the phase's security follow-up
  is satisfied as written.
- `contracts/dev-event/conformance` passes with zero edits, confirming insight 3:
  the adapter-facing layer never touched the wire span.

## Success criteria

1. `grep -rn "AssertHookWireShape\|BuildHookSpan\|BuildHookEvent\|HookType" --include="*.go" .`
   returns nothing.
2. `contracts/dev-event/conformance` passes with no edits.
3. `git diff --stat -- adapters/` lists exactly two files, both `_test.go`.
4. All modules in `go.work` build and test clean.

## Risk assessment

| Risk | Mitigation | Signal it broke | Pre-decided response |
|---|---|---|---|
| Deleting a helper still used elsewhere (e.g. a trace id needed by lineage or the approver) | Step 2 audits before deleting, per symbol | Compile error, or a lineage/approval test failing | Relocate the helper to its real owner rather than resurrecting `spanbuilder.go` |
| The adapter parity tests lose their teeth in translation | Rewrite them to compare the two adapters' payloads to each other, not to a hardcoded literal | Both adapters could drift together undetected | Add one golden per adapter so drift shows as a diff |
| Deletion outpaces Phase 01 and breaks the build mid-change | Phase 02 starts only after Phase 01's tests are green | `go build` fails on missing symbols | Sequence properly; do not interleave |

**Assumption that may break:** that no production adapter code touches the mirror. The
census says so today. Signal: a non-test file appears in the `adapters/` diff. Response:
stop — a production dependency means the engine, not just the client, is span-shaped, and
that needs replanning.

## Security considerations

- Removing `AssertHookWireShape` removes a conformance guard. It guarded a shape that no
  longer exists, but the *habit* it enforced (no nested envelopes, no client-set
  `semantic_type`, no content leaking into span fields) must not vanish silently:
  confirm `client/leakscan_test.go` still covers content never reaching the wire, and
  extend it to the activity fields if it does not.
- No change to signing, key handling, or the enforce path.

## Next steps

Phase 03 makes the documentation and the ADR record match the code.

<!-- Updated: Validation Session 1 - reference census hardened (comment-only hits identified, client/approval.go named as the one production caller of the derivations); workflowIDFor/activityIDFor excluded from retirement; Span.Stage retained unread -->
