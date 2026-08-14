# Phase 02 — `status` on tool results, end to end

## Context links

- Plan: [plan.md](plan.md) · Depends: [phase 01](phase-01-adr-0018-dev-turn-content-carrier.md)
- Design owner: superseded plan 2241 phase-01 (merge decision 1); contract rigor from superseded
  plan 2200 phase-02
- Core consumer (read-only): `openbox-core internal/services/activities/observability/errors.go:301-337`

## Overview

- **Date:** 2026-08-13 · **Priority:** P1 · **Status:** pending · **Effort:** 3h
- Emit top-level `status` on tool `ActivityCompleted` — the field core already reads and no
  producer has ever written — derived **structurally only**. This phase ships the `"completed"`
  path + schema v1.2 + goldens + conformance; `"failed"` becomes real when phase 03 wires
  `PostToolUseFailure`. Fixes Tool Health SUCCESS 0.0%.

## Key Insights

1. **The consumer is exact and unforgiving.** `errors.go:332-334`:
   `IsSuccess = payload.Status != nil && *payload.Status == "completed"` — literal string,
   top-level, `ActivityCompleted` only. `.total` already increments on every `ActivityStarted`
   (`errors.go:118-127`), so today every completion increments `tool.<name>.failed` → 0% forever.
2. **`llm_completion` is already excluded from tool metrics** (`errors.go:320-322`, core `develop`
   @ 68f0398) — status on a turn would do nothing for Tool Health. Tool results only.
3. **`payload.Status` also writes `governance_events.workflow_status`** for any event type
   (`storage_event.go:416-418`). Reader inventory: only the compliance evidence export
   (`openbox-backend src/modules/compliance/compliance-source-evidence.service.ts:141`).
   Acceptable — and the reason `status` never goes on lifecycle events, where it would overwrite a
   genuinely workflow-scoped column.
4. **Event identity does not move.** `deriveID` (`adapters/claude-code/mapper.go:641-690`) hashes
   an explicit field list excluding `Status`; `client/approval_key_pin_test.go` +
   `client/turn_key_pin_test.go` stay green **untouched** — that is the proof.
5. **Field order is the wire contract.** `buildPayload` marshals declaration order; goldens pin
   bytes (`client/golden_test.go:11-28`). Append `Status` **after `Metadata`, last in the
   struct**, so only the three `activity_*_completed` fixtures change.
6. **Truthfulness is the only hard constraint.** `"completed"` unconditionally is honest **iff**
   `PostToolUse` does not fire for failed calls. Step 1 verifies; never ship a path that can read
   SUCCESS 100% while calls fail.

## Requirements

- R1: `status` emitted for `ToolResult` (`ActivityCompleted`) only; never turn / lifecycle /
  signal events (assert absence).
- R2: Derivation structural only (hook identity / bound bool / bound int). No new string field on
  `HookEvent`; never parsed from tool output text.
- R3: NOT content-gated — ships identically with `content_capture:false`.
- R4: Closed vocabulary `completed`|`failed`; client omits anything else (a typo must not zero a
  shipped metric).
- R5: Adapter-facing contract → additive **v1.2** with version note (ADR-0014 v1.1 precedent);
  every fixture pinning the version const updated. Phase 03's new event types land in the same
  1.2 (one bump for the plan).
- R6: `activity_id`/`event_id`/approval keys byte-unchanged; 11 modules green `-race`; both
  cross-compiles (windows/amd64, linux/arm64).
- R7: Where a structural exit code exists, set `metadata.exit_code` so the existing promotion
  (`client/payload.go:587-589`) lights up.

## Architecture

```
HookEvent (PostToolUse)             client                        core
  hook identity ──▶ adapter maps ──▶ DevEvent.Status ──▶ payload.status ──▶ ExtractToolMetric.IsSuccess
  (PostToolUseFailure → "failed", phase 03)   (enum, ungated, top-level, appended last)
```

`governanceEventPayload` gains, appended after `Metadata`:
`Status string \`json:"status,omitempty"\`` (+ doc comment quoting core's comparison).
`DevEvent` gains the same (`client/event.go:203-217`); schema gains `status` with
`enum: ["completed","failed"]`.

**Derivation branches, decided by step 1:**

| Branch | Condition | Response |
|---|---|---|
| B1 (expected) | `PostToolUseFailure` fires INSTEAD of `PostToolUse` on failure (hooks docs, research §2, verified 2026-08-13) | `PostToolUse` → `completed`; failure hook → `failed` (phase 03) |
| B1b | `PostToolUse` fires on failure WITH a structural marker (`is_error` bool / exit int) | derive from the marker via a numbers-and-bools projection (the `usageNumbers` idiom, `adapters/claude-code/usage.go:71-76`) |
| B2 | fires only on success, no failure hook available | `completed` unconditionally; failures = unpaired `ActivityStarted` (SUCCESS% reads below 100 honestly) |
| B3 | both fire per call, or fires on failure with no marker | **stop-and-replan** (candidate: spool-local suppression keyed on `tool_use_id`); never claim success |

Codex: `adapters/codex/hookevent.go:69-76` binds no `tool_response`; no exit code anywhere in
`adapters/codex`; no failure hook known → no `status` for Codex + `capabilities.go` entry
(success-unknown, why). Per-provider divergence is fine; it does not block Claude Code.

## Related code files

| Path | Change |
|---|---|
| `client/event.go:28,203-217` | `SchemaVersion = "1.2"`; `DevEvent.Status` + doc comment |
| `client/payload.go:28-78,140-160` | `Status` field (last); `statusFor(ev)` allowlist, called in `case EventToolResult:` only |
| `adapters/claude-code/mapper.go:196-200` | `case HookPostToolUse:` set `ev.Status = "completed"` (per branch) |
| `adapters/claude-code/hookevent.go:86-94` | (B1b only) structural bool/int projection |
| `adapters/codex/capabilities.go` | success-unknown entry |
| `contracts/dev-event/schema/dev-event.schema.json:6,8,24` + properties | `x-schema-version`/const → 1.2, version note, `status` property (keep `additionalProperties:false`) |
| `contracts/dev-event/conformance/testdata/**/*.json` (17 files) | `schema_version` → 1.2; + valid `tool_result_failed.json`; + invalid out-of-enum fixture |
| `client/testdata/golden/activity_{file,shell,mcp}_completed.json` | regenerate (gain `status`) |
| `client/golden_test.go`, `client/payload_test.go` | failed-status golden case; allowlist unit tests |
| `adapters/claude-code/enforce_conformance_test.go` | C20/C21 |
| `adapters/*/mapper_test.go` | derivation tests |

Do **not** touch: pin tests, `adapters/claude-code/usage.go`+`usage_test.go`,
`contracts/dev-event/MAPPING.md`/`COVERAGE.md` (phase 05 owns prose), `go.mod` (x/term pin).

## Implementation Steps

1. **Verify the failure surface (gates the phase — and phase 03's design).** For installed Claude
   Code: does `PostToolUse` fire on failure; does `PostToolUseFailure` exist/fire; do both fire
   for one failed call? Sources, cheapest first: (a) live hooks reference
   (https://code.claude.com/docs/en/hooks); (b) installed binary's hook docs (repo precedent
   `adapters/claude-code/hookevent.go:105-112`); (c) empirical — hook a scratch project, run a
   failing `Bash` (`exit 3`) + failing `Read`, inspect spooled JSON. No stack needed. Record
   answer + evidence in the PR; pick the branch. Repeat (a)+(c) for Codex
   (`adapters/codex/testdata/posttooluse.json:11` as shape reference).
2. `client/event.go`: `DevEvent.Status`; `SchemaVersion` → `"1.2"`.
3. `client/payload.go`: `Status` appended last; `statusFor` (value allowlist ∧
   `EventToolResult`); doc comment quotes `errors.go:332-334`.
4. Schema: version bump + note + `status` property; 17 fixture bumps; new valid/invalid fixtures.
5. Claude Code mapper derivation per branch; Codex `capabilities.go` entry.
6. Regenerate goldens (`go test ./client -run Golden -update`), **read the diff**: exactly one
   added `"status"` key on the three completed fixtures. A diff touching `activity_id`/`event_id`
   or a turn fixture means step 3 is too broad — revert and narrow.
7. Conformance: `C20 a completed tool call reports status "completed" on the outbound bytes`;
   `C21 status ships unchanged with content_capture:false`.
8. `go test ./... -race` per module (all 11); both cross-compiles; `leakscan_test.go` unchanged
   and green.
9. Commits: `feat(client): report tool activity status so success metrics count` +
   `feat(claude-code): derive tool status structurally`.

## Todo list

- [ ] Step 1 verification recorded; branch chosen
- [ ] DevEvent.Status + SchemaVersion 1.2 + payload field + `statusFor`
- [ ] Schema + 17 fixtures + new valid/invalid fixtures
- [ ] Claude Code derivation; Codex non-support entry
- [ ] Goldens regenerated, diff read line by line
- [ ] C20/C21; pin tests untouched; 11 modules `-race` + cross-compiles green

## Success Criteria

- Tool `ActivityCompleted` carries `"status":"completed"` on the **outbound bytes** (C20); no
  `status` on any turn/lifecycle/signal payload (asserted); identical with capture off (C21).
- Golden diff = exactly one added key per completed tool fixture.
- Pin tests, `TestFinops_NoContentOnWire`, `TestNoGatedContentEgressesWhenCaptureIsOff` green with
  zero edits; `TestSchemaEnumMatchesContract` (`schema_guard_test.go:27`) green.
- Derivation provably content-free: diff adds no string-typed `HookEvent` field.

## Risk Assessment

| Risk | L×I | Mitigation / pre-decided response |
|---|---|---|
| `PostToolUse` fires on failure and completed-unconditional shipped ⇒ SUCCESS reads 100% while failing | M×H | Step 1 gates. Signal: phase 6 shows 100.0% with a known-failing call. Response: stop-and-replan (B1b/B3); never "tune" the metric |
| Wrong literal (`"success"`, `"COMPLETED"`) keeps metric at 0% | M×H | Closed vocab + C20 asserts the literal on bytes; core comparison quoted in comment |
| Schema bump misses a fixture | M×L | 17 files enumerated; run contracts tests immediately after step 4 |
| `workflow_status` on activity rows changes a downstream rendering | L×M | Reader inventory done (compliance export only); response: already ToolResult-scoped, note in ADR |
| Codex divergence read as a bug | M×L | `capabilities.go` entry + phase 05 MAPPING note |

## Security Considerations

- No new content class. Structural derivation only; a bound `is_error` bool / exit int is
  INV-2-safe by the same argument `isSidechain` is (`usage.go:104-106`). Binding `tool_response`
  as a string is ADR-0019 P1 territory, not this phase.
- Two-literal enum cannot encode content; `metadata.exit_code` stays an integer — a message
  alongside it goes to `contentMetadataKeys` (`client/payload.go:419-432`), never promoted.

## Next steps

Phase 03 — failure + lifecycle hooks (makes `"failed"` real).
