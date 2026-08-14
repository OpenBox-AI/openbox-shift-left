# Phase 02 — `status` on tool `ActivityCompleted`, end to end

## Context links

- Plan: [plan.md](plan.md) · Blocked on: [phase 01 (ADR-0018)](phase-01-adr-0018-dev-turn-content-carrier.md)
- Evidence: [scout-02 §Widget 3](scout/scout-02-write-side-core-sdk-shiftleft.md),
  [scout-01 §3](scout/scout-01-read-side-fe-backend.md)
- Core-side target (read-only): `openbox-core internal/services/activities/observability/errors.go:301-337`

## Overview

- **Date:** 2026-08-13
- **Description:** Emit a top-level `status` on a tool `ActivityCompleted` — the field core
  already reads and no producer has ever written — derived **structurally only**. Fixes Tool
  Health Matrix SUCCESS 0.0%. Includes the adapter-facing schema bump to v1.2, golden re-pin and
  new conformance cases.
- **Priority:** P1 within this plan (smallest change, largest visible win, no new content class).
- **Implementation status:** pending
- **Review status:** pending
- **Effort:** 3h

## Key Insights

1. **The consumer is exact and unforgiving.** `errors.go:332-334` sets
   `IsSuccess = payload.Status != nil && *payload.Status == "completed"` — literal string, top-level
   field, on `ActivityCompleted` only. `metric.ToolName = *payload.ActivityType` (`errors.go:325`),
   and `.total` is already incremented on every `ActivityStarted` (`errors.go:118-127`) with
   latency written unconditionally (`errors.go:146-160`). So today every completion increments
   `tool.<name>.failed` → 0% forever. One correct string fixes the widget with no core change.
2. **`llm_completion` is already excluded from tool metrics** on core `develop` HEAD 68f0398 via
   `IsLLMCompletionActivity` (`errors.go:320-322`, scout-02 §Side note) — so a `status` on a turn
   would do nothing for Tool Health. Emit it on tool results only.
3. **`payload.Status` also writes `governance_events.workflow_status`** for any event type
   (`openbox-core internal/services/activities/governance/storage_event.go:416-418`). Blast radius
   checked: the only non-seed reader in openbox-backend is the compliance evidence export
   (`src/modules/compliance/compliance-source-evidence.service.ts:141`); nothing derives session
   status from it. Acceptable — and a further reason not to put `status` on lifecycle events,
   where it would overwrite a genuinely workflow-scoped column.
4. **Event identity does not move.** `deriveID` (`adapters/claude-code/mapper.go:641-690`) hashes
   an explicit field list — session, event type, tool name, timestamp, span locators, invocation
   id, turn index, agent id. `Status` is not in it, and `activityIDFor`/`turnActivityIDFor`
   (`client/payload.go:260-263,294-310`) do not read it either. `client/approval_key_pin_test.go`
   and `client/turn_key_pin_test.go` must stay green **untouched** — that is the proof.
5. **Field order is the wire contract.** `buildPayload` marshals struct declaration order and
   `client/testdata/golden/*.json` pin the bytes (`client/golden_test.go:11-28`). Append `Status`
   **after `Metadata`, at the end of the struct**, so only the three `activity_*_completed`
   fixtures change and every other fixture stays byte-identical.
6. **Truthfulness is the only hard constraint.** Emitting `"completed"` unconditionally is honest
   **iff** `PostToolUse` does not fire for failed calls; otherwise it inflates SUCCESS to 100% —
   a governance product overstating itself. Hence step 1 is a verification with pre-decided
   branches, not an assumption.

## Requirements

- R1: `status` on the wire is emitted for `ToolResult` (`ActivityCompleted`) only; never
  `TurnStarted/TurnCompleted`, never `WorkflowStarted/Completed`, never `SignalReceived`.
- R2: Derivation is structural: hook identity, a bound boolean, or a bound integer. **Never**
  parsed from tool output text; no new content-bearing field on `HookEvent`.
- R3: `status` is NOT content-gated — it ships identically with `content_capture:false`.
- R4: Value vocabulary is closed: `completed` | `failed`. The client omits anything else
  (defence against a typo silently zeroing the metric).
- R5: Adapter-facing contract updated as an additive v1.2 with a version note (ADR-0014 v1.1
  precedent), including every fixture that pins the version const.
- R6: `activity_id`, `event_id`, approval keys byte-unchanged; all 11 modules green under
  `-race`; both cross-compiles (windows/amd64, linux/arm64) still build.
- R7: Where a provider exposes a structural exit code, set `metadata.exit_code` so the existing
  client promotion (`client/payload.go:587-589`) lights up — no client change needed for it.

## Architecture

```
HookEvent (PostToolUse)              client                          core
  hook identity ─┐
  is_error?      ├─▶ adapter maps ─▶ DevEvent.Status ─▶ payload.status ─▶ ExtractToolMetric
  exit_code?     ┘   (structural)     (enum, ungated)     (top-level)      .IsSuccess
                     metadata.exit_code ─▶ activity_output.exit_code (already wired)
```

Placement in `governanceEventPayload` (`client/payload.go:28-78`) — appended last:

```go
    Metadata   json.RawMessage `json:"metadata,omitempty"`
    // Status is core's activity/workflow status column and the ONLY signal its
    // tool-success metric reads (observability/errors.go:332-334) …
    Status     string          `json:"status,omitempty"`
```

`DevEvent` gains the same field (`client/event.go:203-217`), schema gains a `status` property with
`enum: ["completed","failed"]`.

**Derivation, decided per branch in step 1:**

| Branch | Condition | Claude Code derivation | Codex derivation |
|---|---|---|---|
| B1 | a structural failure marker is available (`tool_response.is_error` bool, an `exit_code`, or a distinct failure hook) | bind ONLY the bool/int via a numbers-and-bools projection struct (the `usageNumbers` idiom, `adapters/claude-code/usage.go:71-76`) → `completed` \| `failed` | same, if Codex exposes one |
| B2 | `PostToolUse` fires only on success | `status = "completed"` unconditionally; failures surface as an unpaired `ActivityStarted` (`.total` up, `.success`/`.failed` unchanged) — SUCCESS% then reads below 100 honestly | same |
| B3 | fires on failure AND no structural marker exists | **stop-and-replan**: emit no `status` for that provider rather than claim success | idem |

Codex-specific evidence for the branch call: `adapters/codex/hookevent.go:69-76` binds no
`tool_response` field at all; there is no `exit_code` anywhere in `adapters/codex` (grep clean).
Per-provider divergence is acceptable — status derivation is inherently adapter-specific — and B3
for Codex alone does not block the Claude Code fix.

## Related code files

| Path | Change |
|---|---|
| `client/payload.go:28-78` | add `Status` field (last), with doc comment naming the core consumer |
| `client/payload.go:140-160` | set `p.Status` in `case EventToolResult:` only, via the allowlist helper |
| `client/payload.go:556-598` | `structuralActivityOutput` — unchanged; note it already promotes `metadata.exit_code` |
| `client/event.go:28` | `SchemaVersion = "1.2"` |
| `client/event.go:203-217` | `DevEvent.Status string \`json:"status,omitempty"\`` + doc comment |
| `adapters/claude-code/hookevent.go:86-94` | (B1 only) bind a structural-only `tool_response` projection |
| `adapters/claude-code/mapper.go:196-200` | `case HookPostToolUse:` set `ev.Status` (+ `metadata.exit_code` when B1 gives one) |
| `adapters/codex/mapper.go:195` (`case HookPostToolUse:`) | mirror per branch; leave unset under B3 |
| `contracts/dev-event/schema/dev-event.schema.json:6,8,24` + properties | `x-schema-version` → 1.2, new version note, `const` → "1.2", `status` property |
| `contracts/dev-event/conformance/testdata/**/*.json` (17 files) | `"schema_version": "1.1"` → `"1.2"` |
| `client/testdata/golden/activity_{file,shell,mcp}_completed.json` | regenerate (gain `status`) |
| `client/golden_test.go` (`goldenCases()`) | add a `status:"failed"` case + fixture under B1 |
| `client/payload_test.go` | allowlist unit tests (bad value omitted; not set on other types) |
| `adapters/claude-code/enforce_conformance_test.go:79-400` | new cases C20/C21 (naming: `t.Run("C20 …")`) |
| `adapters/claude-code/mapper_test.go`, `adapters/codex/mapper_test.go` | derivation unit tests |

Do **not** touch: `client/approval_key_pin_test.go`, `client/turn_key_pin_test.go`,
`adapters/claude-code/usage.go` and `usage_test.go`, `contracts/dev-event/MAPPING.md` /
`COVERAGE.md` (phase 4 owns the prose), `go.mod` files (`golang.org/x/term` pin).

## Implementation Steps

1. **Verify the failure surface (gate for the whole phase).** Determine, for Claude Code
   2.1.22x+: does `PostToolUse` fire when a tool call fails, and what structural marker rides the
   payload? Sources, cheapest first:
   a. the live hooks reference (https://code.claude.com/docs/en/hooks) — per-hook stdin fields and
      whether a distinct failure event exists (prior research recorded a `PostToolUseFailure`
      event: `plans/reports/research-260813-2215-session-content-capture-gaps.md:62`);
   b. the installed binary's own hook documentation (the repo has used it as authority before —
      `adapters/claude-code/hookevent.go:105-112`);
   c. empirical: install the hook in a scratch project, run a deliberately failing `Bash` (e.g.
      `exit 3`) and a failing `Read` (missing path), and inspect the spooled event JSON under the
      spool dir. This needs no OpenBox stack.
   Record the answer + evidence in the PR description and pick B1/B2/B3. Repeat (a)+(c) for Codex
   using `adapters/codex/testdata/posttooluse.json:11` as the shape reference.
2. `client/event.go`: add `DevEvent.Status`; bump `SchemaVersion` to `"1.2"`.
3. `client/payload.go`: add `Status` last in `governanceEventPayload`; add
   `statusFor(ev DevEvent) string` returning `ev.Status` only when it is `"completed"` or
   `"failed"` **and** `ev.EventType == EventToolResult`, else `""`; call it in the
   `case EventToolResult:` arm. Document why the allowlist exists (a typo zeroes a shipped metric).
4. Schema: bump `x-schema-version` and the `schema_version` const to `1.2`; add the 1.2 version
   note (what it adds, why additive); add the `status` property with the enum and a description
   naming the core consumer; keep `additionalProperties: false` intact (it is why the property is
   required at all — `dev-event.schema.json:93`).
5. Update the 17 conformance fixtures' `schema_version` to `1.2`; add one valid fixture carrying
   `status` (`contracts/dev-event/conformance/testdata/valid/tool_result_failed.json` under B1) and
   one invalid fixture with an out-of-enum status.
6. Adapter derivation per branch, Claude Code first: `mapper.go:196-200`. Under B1 add the
   structural projection to `hookevent.go` — bools/ints only, no string fields, mirroring the
   `usageNumbers` "the struct IS the allowlist" idiom, and note in its doc comment that binding a
   string there would be an INV-2 change.
7. Codex: same per branch; if B3, leave `Status` unset and add a `capabilities.go` entry stating
   tool success is unreported for Codex and why.
8. Regenerate goldens deliberately: `go test ./client -run Golden -update`, then **read the diff**
   — exactly one added `"status"` key on the three completed fixtures, no reordering, no other
   value change. A diff that touches `activity_id`, `event_id` or a turn fixture means step 3 set
   the field too broadly: revert and narrow.
9. Conformance cases in `adapters/claude-code/enforce_conformance_test.go`:
   - `C20 a completed tool call reports status "completed" on the outbound bytes`
   - `C21 status ships unchanged with content_capture:false` (proves R3 — the field is not gated)
   - under B1: `C22 a failed tool call reports status "failed"`.
10. Test sweep: `go test ./... -race` per module (`go.work` lists all 11); both cross-compiles
    (`GOOS=windows GOARCH=amd64 go build ./cli/...`, `GOOS=linux GOARCH=arm64 go build ./cli/...`).
11. Commit: `feat(client): report tool activity status so success metrics count` +
    `feat(claude-code): derive tool status structurally` (conventional, no AI references).

## Todo list

- [ ] Step 1 verification done, branch chosen, evidence recorded
- [ ] `DevEvent.Status` + `SchemaVersion` 1.2
- [ ] `governanceEventPayload.Status` (last field) + `statusFor` allowlist
- [ ] Schema: version + note + `status` property + enum
- [ ] 17 conformance fixtures bumped; new valid/invalid status fixtures
- [ ] Claude Code derivation (+ structural `tool_response` projection under B1)
- [ ] Codex derivation or documented non-support
- [ ] `metadata.exit_code` set where a structural exit code exists
- [ ] Goldens regenerated and the diff read line by line
- [ ] C20/C21(/C22) conformance cases
- [ ] 11 modules green under `-race`; both cross-compiles build
- [ ] Pin tests untouched and green

## Success Criteria

- A tool `ActivityCompleted` built by `buildPayload` carries `"status":"completed"` at the top
  level; asserted on the **outbound bytes** in a conformance case, not on a struct field.
- No `status` key on any turn, lifecycle or signal payload (assert absence explicitly).
- `status` present identically with `content_capture:false` (C21).
- `client/testdata/golden/` diff is exactly one added key per completed tool fixture.
- `TestGoldenWirePayloads`, `client/approval_key_pin_test.go`, `client/turn_key_pin_test.go`,
  `TestFinops_NoContentOnWire`, `TestNoGatedContentEgressesWhenCaptureIsOff` all green with no
  edits to the last three.
- `contracts/dev-event` conformance green: `TestSchemaEnumMatchesContract`
  (`contracts/dev-event/conformance/schema_guard_test.go:27`) plus the valid/invalid corpus.
- Derivation is provably content-free: the diff adds no string-typed field to any `HookEvent`.

## Risk Assessment

| Risk | L×I | Mitigation / signal & pre-decided response |
|---|---|---|
| `PostToolUse` fires on failure and B2 was chosen ⇒ SUCCESS reads 100% while calls fail | M×H | Step 1 gates the phase. **Signal:** phase 5 live check shows SUCCESS=100.0% with a known-failing call in the session. **Response:** stop-and-replan — switch to B1 or withdraw the field for that provider; do not "tune" the metric |
| Wrong string (`"success"`, `"COMPLETED"`) silently keeps the metric at 0% | M×H | Client-side allowlist + a conformance case asserting the literal on the outbound bytes; core's comparison quoted in the code comment |
| Schema bump misses a fixture ⇒ conformance red late | M×L | Enumerated: 17 files listed above; run the contracts module tests immediately after step 5 |
| `workflow_status` on activity rows changes a UI/compliance rendering | L×M | Reader inventory taken (backend grep: only compliance evidence export + demo seeder). **Signal:** an activity row renders as a session status somewhere. **Response:** adjust in-plan — restrict to `ToolResult` (already the design) and note in ADR consequences |
| Adding a field reorders keys and churns every fixture | L×M | Field appended last, deliberately; step 8 reads the diff before accepting |
| Codex diverges from Claude Code and looks like a bug | M×L | `capabilities.go` entry + MAPPING note (phase 4) state it explicitly |

## Security Considerations

- No new content class. The derivation must stay structural: an `is_error` **bool** or an exit
  **int** is INV-2-safe by the same argument `isSidechain` is (`adapters/claude-code/usage.go:104-106`);
  binding `tool_response` as a string would put tool output one `capStr` from the wire and is out
  of scope for this phase.
- `status` is ungated on purpose. Confirm nothing in the enum can encode content (two literals).
- `metadata.exit_code` is an integer; if a provider ever reports a *message* alongside it, do not
  promote it — `contentMetadataKeys` (`client/payload.go:419-432`) exists for exactly that class.
- Re-run `client/leakscan_test.go` unchanged: the new field must not create a path that carries a
  canary.

## Next steps

Phase 3 (assistant-turn span) — it appends further fields to the same struct and regenerates the
same golden directory, so it starts only after this phase's diff is merged.
