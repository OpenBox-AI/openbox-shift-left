# Verification — phase 01, tool content capture (ADR-0019 P1, contract v1.3)

Date: 2026-08-25. Plan: [260825-0027](../260825-0027-openbox-gateway-full-capture/plan.md),
[phase 01](../260825-0027-openbox-gateway-full-capture/phase-01-tool-content-capture.md).
Scope this session: phase 01 only (owner-chosen). `--tdd --advice --yagni`.

Every claim below is filed by evidence strength. Nothing here was verified by
reading code alone.

## What shipped

| Requirement | Where | Wire slot |
|---|---|---|
| `PostToolUse.tool_response` | `hookevent.go` `ToolResponse` + `toolOutputText`, `mapper.go` `gatedToolOutput` | `activity_output.output` |
| Tool input on the **observe** path | `mapper.go` HookPreToolUse arm, reusing `evaluationContext(e, nil)` | `activity_input.command`/`.arguments`/`.content` |
| `PostToolUseFailure.error` | `mapper.go` failure arm → `Content.ToolOutput` | `activity_output.output` |
| `PermissionDenied.reason` | `mapper.go` → `Content.SignalDetail` | `metadata.denial_reason` |
| `StopFailure.error_details` | `hookevent.go` `ErrorDetails` → `Content.SignalDetail` | `metadata.error_details` |
| Gate | `client.stripContent` nils `Content` — unchanged choke point | — |
| Cap | `capBody` at `structuralActivityOutput` / `buildMetadata` | 64KB, before signing |
| Schema | `dev-event.schema.json` → 1.3, `client.SchemaVersion` → 1.3 | additive |

## Verified — strong (asserted on outbound bytes)

C32–C39 drive the REAL `RunHook` observe path against a REAL `/evaluate` stub over
HTTP and assert on the bytes actually POSTed. Not on a `DevEvent`, not on a mapper
return — the distinction is the point, since a redaction applied after attachment
passes every struct-level test and still leaks.

- **C32/C33** tool output present with capture ON, absent with OFF — and the
  `ActivityCompleted` still ships with OFF, so the case cannot pass by the client
  emitting nothing.
- **C34** a secret in tool output is redacted BEFORE attachment; the placeholder is
  asserted present, so it cannot pass by sending nothing at all.
- **C35** a 70,000-char body arrives ≤ 65,536 runes.
- **C36** observe-path tool input present with capture ON, absent with OFF —
  the SL3-SEC-3 retirement, asserted in both directions.
- **C37** a failed call's free-text error reaches `activity_output.output`, and
  `metadata.error_type` (the UNGATED provider enum) stays clean.
- **C38** both signal free-texts reach `metadata`, and `signal_args` is empty on
  every signal body.
- **C39** detector reach, per credential format (see below).
- `TestEscalationCarriesApprovalContext_ObserveNeverDoes` gained a capture-ON leg
  asserting the observe copy carries the IDENTICAL extract to the gated copy.
  Equality, not presence: two copies of one call disagreeing about the command
  would be worse than either alone, and nothing else pinned them together.

Whole-workspace: all 11 modules green under `go test -count=1 -race ./...`, gofmt
clean, `windows/amd64` + `linux/arm64` cross-compiles OK.

## Code review — findings and disposition

Reviewed 2026-08-25. Four findings were real bugs in this change and are FIXED;
three were stale doc claims outside the reconciled set and are FIXED; two are
pre-existing gaps this change makes newly reachable and are SURFACED, not
silently patched.

| # | Finding | Disposition |
|---|---|---|
| C1 | A body over 512 KiB was returned UNSCANNED by `hookflow.RedactText`, then capped to 64K and signed — so a large `cat`/install log egressed its first 64K unscanned | **Fixed.** Truncate-then-scan. Provably lossless: the client's egress cap is 65536 runes ≤ 256 KiB, strictly under the 512 KiB scan cap, so only bytes that could never egress are discarded. Tripwire `TestRedactTextScansOversizedBodies` verified to fail on the old behavior. |
| H1 | Observe path capped (8 KiB) BEFORE redacting — a secret straddling the boundary would be cut into an unmatchable fragment and shipped | **Fixed.** `evaluationContext` split into `toolInputExtract` (uncapped) + the cap, so the observe arm does redact→cap. |
| M3 | `testbed/35-telemetry.sh` asserted `assert_absent '"output":'` — but `output` is a real column `row_to_json` renders on every row, so this was guaranteed to false-fail on the first live run | **Fixed.** Rewritten as JSONB key tests (`output ? 'output'`, `input ? 'command'`, `metadata ? 'denial_reason'`), matching the `output->>'model'` precedent. |
| C2 (comment) | My new `enforcetarget.go` comment claimed "the PRECISELY redacted bytes" universally — true only for file-semantic tools | **Fixed.** The comment now states the per-class split explicitly. |
| H3/M1/M2 | `docs/architecture.md`, `client/leakscan_test.go`, `cli/internal/backend/approvals.go` still asserted the retired SL3-SEC-3 invariant | **Fixed.** All three reconciled. |

Not fixed — surfaced instead, see *Owner decisions* below: the escalated shell/MCP
`/evaluate` copy carries the command verbatim (C2 proper), and the detector's
nested-JSON blindness (H2). Both are pre-existing; both are now pinned by tests and
disclosed in `docs/data-and-privacy.md`.

One reviewer claim was narrowed by direct measurement: H2 was reported as "the
regex can't match JSON". Probing the real detector shows the named formats
(AWS/GitHub/Stripe/JWT) and the entropy pass all fire fine in *unescaped* JSON.
The actual failure is **escaped** JSON — `precededByAssignment`
(`decision/secrets.go:187-200`) walks back over spaces and quotes but not over a
backslash — which is the shape a nested value takes inside `tool_response`. That is
a different, broader defect than reported: it defeats the entropy pass too, not
only the keyword pattern.

## Verified — measured, and some of them are limits we are keeping

**C39 measures which credential FORMATS the one in-transit control actually
catches.** C34 proves ordering using an AWS key — a format the pattern list covers
by name — which says nothing about reach. After this phase that question got
expensive: tool output egresses at TOOL-CALL cadence and is exactly where
credentials surface.

Driven through a real `PostToolUse.tool_response` as a dotenv dump:

| In tool output | Redacted? | By |
|---|---|---|
| `OPENBOX_API_KEY=obx_…` | yes | `secret_assignment` (keyword `api_key`) |
| `OPENBOX_AGENT_PRIVATE_KEY=<base64>` | yes | entropy fallback — no `private_key` keyword exists |
| `API_KEY=<64 hex>` | yes | `secret_assignment` — charset-agnostic |
| `SESSION_KEY=<base64>` | yes | entropy fallback |
| `DEPLOY_HEX=<64 hex>` | **no** | nothing — no keyword, and hex is under the entropy floor |
| `{"key":"<base64>"}` nested in tool output | yes *(after OD-2)* | entropy fallback — the escape is skipped |
| `{"password":"hunter2…"}` nested in tool output | yes *(after OD-2)* | `secret_assignment` — quoting tolerated |

One standing limit, asserted to leak deliberately, and one that was closed:

1. **For generic secrets the KEYWORD decides, not the charset.** Not fixable by
   lowering the entropy floor: hex caps at 4.0 bits/char against a 4.5 threshold
   *by design* (`decision/secrets.go:50-56`), and below 4.0 every git SHA and UUID
   matches — on the enforce path the redactor **rewrites the file your tool is
   about to write**, so false positives corrupt real content.
2. **Nested JSON used to defeat BOTH generic mechanisms — now CLOSED** (OD-2).
   `tool_response` is JSON, so a nested value arrives escaped, and
   `precededByAssignment` walked back over spaces and quotes but not a backslash.
   Both patterns were widened; the C39 legs are kept as regression guards. Named
   formats were never affected, which is what made the gap easy to miss: an AWS key
   in JSON was caught while a database password was not.

Limit 1 is pinned in both directions: if it ever closes, the test fails and asks
whether that was deliberate. Both are disclosed in `docs/data-and-privacy.md`.

Runner-up risk, deliberately NOT resolved here: **volume.** 64KB bodies at
tool-call cadence through the realtime flusher. Phase 08 owns the measurement and
the backend retention ask; the phase file already carries it.

## NOT verified — needs a live stack

The testbed did not run. These are the claims a run must confirm:

1. Core stores `activity_output.output` as the row's `output` and runs Guardrails
   stage "1" over it. Established from `MAPPING.md` + core reading only — and this
   repo's rule is that reading is not evidence.
2. `metadata.denial_reason` / `metadata.error_details` survive ingest into
   `governance_events.metadata`. **No core reader renders them yet** (the Verify tab
   reads `signal_args`, which this deliberately avoids) — stored-and-queryable, same
   posture as `metadata.event_id`. Stated in `MAPPING.md`, not implied.
3. Volume/latency at tool-call cadence against a real stack.

Dormant assertions are in place for 1 and 2: `testbed/20-capture.sh` (gate OPEN)
and `testbed/35-telemetry.sh` (gate CLOSED, on its existing capture-off session).

## Decisions taken during implementation

- **`Content.ToolOutput` is a NEW field, not a reuse of `Content.Output`.** `Output`
  carries the ADR-0018 turn text that feeds core's alignment extractor. Confirmed
  live at `mapper.go` — the collision is real, not theoretical.
- **The failure hook's `error` shares `ToolOutput` with `tool_response`.** A failed
  activity's output IS its error text, and `status` already discriminates. The two
  never compete: the probe established `PostToolUse` / `PostToolUseFailure` are
  mutually exclusive per call, and the failure hook carries no `tool_response`.
- **Signal free text rides `metadata` under per-type keys, not one generic key.**
  The keys are `denial_reason` / `error_details` and NOT the provider's own names,
  because `reason` is already a closed enum on SessionEnd metadata — gating `reason`
  would have stripped that structural enum under capture-off. `signalDetailKeyFor`
  returns `""` for every other event type, so a stray detail is dropped rather than
  landed somewhere undefined.
- **It is a `Content` field, not an adapter-set metadata key.** The gate is then a
  property of the choke point (`stripContent` nils `Content`); a mis-typed metadata
  key cannot route free text around the posture. Both keys are ALSO in
  `contentMetadataKeys` as the backstop.
- **`TestContentGate` now walks its fixture directory.** The hardcoded list meant a
  fixture for a new gated field was never validated — and it caught a bad
  `semantic_type` in the new fixture on the first run.

## Tests that were green but stale — the dangerous class

Nothing went red that should have stayed green. The real work was tests that kept
passing while their PREMISE became false, because their helper leaves
`CaptureContent` at the zero value:

`TestMap_NoContentLeak`, `TestMap_StatusDerivationReadsNoToolOutput` (whose
tool_response sentinel was never actually placed in the payload — vacuous on that
axis), `TestMap_FreeTextErrorNeverEgresses`,
`TestEscalationCarriesApprovalContext_ObserveNeverDoes`, both observe-only binary
tests, and comments in `client/event.go`, `client/golden_test.go`,
`client/payload.go`, `enforcetarget.go`. Each now names the posture it proves.

Three tests were pinned to `OPENBOX_CONTENT_CAPTURE=0` explicitly: they had never
set it, so they ran under the DEFAULT (ON) and passed only because the observe path
structurally carried nothing. Leaving them on the default would have silently
converted them into capture-ON tests asserting the opposite of their names.

**Codex is untouched** and binds none of these fields, so a Claude Code session and
a Codex session under the same posture now send different amounts of content. Stated
in `COVERAGE.md` §3.4 rather than averaged away.

## Owner decisions (OD) — RESOLVED 2026-08-25

Both were surfaced rather than inferred; both are now decided and applied.

**OD-1 — DECIDED: deliberate, documented.** Kept verbatim. Recorded in ADR-0017
§Content (amended) and `docs/data-and-privacy.md`. No code change.

**OD-2 — DECIDED: fixed.** Two widenings in `decision/secrets.go`:
`secret_assignment` tolerates quoting/escaping between the keyword and the
separator, and `precededByAssignment` skips the JSON escape. Written test-first
(`TestRedact_JSONShapedSecrets`, 4 legs red before the change), the two C39 legs
flipped from leak to caught, and the whole `decision` suite plus a 25s fuzz run
(246k execs) is clean. Two regression directions are pinned deliberately: the
redaction must not swallow the `\` terminating a JSON string, and a
backslash-bearing value (`password=C:\Users\…`) must still be redacted whole —
so the value pattern refuses a backslash only as the LAST character rather than
excluding it, which would have silently stopped redacting Windows paths.

The original argument for each, kept for the record:

**OD-1 — the escalated shell/MCP `/evaluate` copy carries the command verbatim.**
`buildDecisionRequest` (`adapters/claude-code/enforce.go:115`) sets
`DecisionRequest.Content` only for a file semantic, so `evaluationContext` gets
`redacted=nil` for shell and MCP and returns the raw text. A token on a `curl`
command line reaches the control plane in the clear. This predates ADR-0019 and is
arguably deliberate — a policy matching on a dangerous command should see the TRUE
command, and unlike a file body nothing is replayed onto the machine. What ADR-0019
changes is the optics: the observe copy of the same call now runs the redactor, so
ordinary telemetry is better protected than the copy sent for a governance
decision. Fixing it narrows what policy can match on; leaving it means the raw
command egresses. Either way it wants a sentence in ADR-0017 or ADR-0019.

**OD-2 — the nested-JSON detector gap.** Two widenings would close it. Both can
only widen matching, and every additional match is unambiguously a secret
assignment. But the same detector **rewrites file bodies on the enforce path**, so
widening it changes what gets written to developers' files — which is why it was
not a cleanup. Recommendation was to take it: MCP results and `cat config.json` are
ordinary traffic, and the state at review time was that they egressed unredacted
under a default-ON posture.

## Unresolved questions

1. Volume at tool-call cadence — measurement deferred to phase 08 by the plan. If
   it forces a body sink, `Content.ToolOutput` is the field that would gain a
   `body_ref` sibling.
2. Superseded by OD-2 above, which is the larger version of the same question.
3. ADR-0019 is still **Proposed**. Phase 03 accepts it. Phase 01 implements its P1
   ahead of that acceptance, which the plan sanctions ("ships alone") but which
   leaves the ADR's status trailing the code until phase 03 runs.
