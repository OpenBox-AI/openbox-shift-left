# Enforcement audit record: ts + reason addition — research

## Q1: enforcements.jsonl writer

**Struct** `hookflow.EnforcementRecord` — `adapters/common/hookflow/enforce.go:566-583`:
```go
type EnforcementRecord struct {
    SessionID           string           `json:"session_id"`
    ToolKind            string           `json:"tool_kind,omitempty"`
    Verdict             string           `json:"verdict"`
    WouldBlock          bool             `json:"would_block"`
    AppliedDecision     string           `json:"applied_decision,omitempty"` // deny|ask|"" (proceed)
    Source              string           `json:"source,omitempty"`
    FailOpen            bool             `json:"fail_open"`
    Stale               bool             `json:"stale,omitempty"`
    PolicyID            string           `json:"policy_id,omitempty"`
    ApprovalRef         string           `json:"approval_ref,omitempty"`
    Constraints         []map[string]any `json:"constraints,omitempty"`
    GuardrailCategories []string         `json:"guardrail_categories,omitempty"`
    Redacted            bool             `json:"redacted,omitempty"`
    RedactionCategories []string         `json:"redaction_categories,omitempty"`
}
```
No `ts`, no `reason` field today — confirmed.

**Append fn** `RecordEnforcement(logger *log.Logger, sessionID, toolKind string, dec decision.Decision, res ApplyResult)` — `enforce.go:522-548`. Builds `rec` from `dec.Evaluation.*`/`dec.*`/`res.*`, `json.Marshal`s it, calls `AppendJSONL(DefaultEnforcementPath(), line)` (shared helper, `advisory.go:100-115`; 0700 dir / 0600 file / single O_APPEND write). Path: `DefaultEnforcementPath()` `enforce.go:504-514`, override `OPENBOX_ENFORCEMENT_FILE` (`devconfig.EnvEnforcementFile`, `adapters/common/devconfig/devconfig.go:61`).

**Call sites** (exactly 2, one per adapter, identical shape):
- `adapters/claude-code/outputcontract.go:72-75` `recordEnforcement(logger, e *HookEvent, dec, res)` → `kind,_,_,_,_ := classifyTool(e.ToolName); hookflow.RecordEnforcement(logger, e.SessionID, string(kind), dec, res)`. Wired as the `Record` callback at `adapters/claude-code/hookrun.go:242`.
- `adapters/codex/outputcontract.go:87-90` — same shape. Wired at `adapters/codex/hookrun.go:234`.

## Q2: advisories.jsonl ts — format/clock, for convention match

`AdvisoryRecord.Timestamp string `json:"ts,omitempty"`` — `advisory.go:48`. NOT generated at write time: `Advisory.Record` (`advisory.go:74-97`) copies it straight from the already-mapped event: `Timestamp: ev.Timestamp` (`advisory.go:87`), where `ev` is `client.DevEvent`.

`DevEvent.Timestamp string `json:"timestamp"` // RFC3339` — `client/event.go:258` (comment says RFC3339; actual format is RFC3339**Nano**, see below — comment is imprecise but harmless, whole-second instants are byte-identical per Go's zero-fraction omission).

Clock source, both adapters, same pattern:
- `adapters/claude-code/mapper.go:157-158`: `now := m.clock(); ts := now.UTC().Format(time.RFC3339Nano)`. `m.clock` defaults to `time.Now` (`mapper.go:716`).
- `adapters/codex/mapper.go:150-153`: identical; default clock `mapper.go:553`.
- Sub-second precision is deliberate: folded into `deriveID` as the per-event distinguisher (`mapper.go:152-156` comment; core parses with `RFC3339Nano`, `client/payload.go:812,818`).

**Convention for new `ts`**: `time.Now().UTC().Format(time.RFC3339Nano)`, UTC, nanosecond precision — matches both advisories.jsonl and every wire timestamp in the repo. `adapters/common/hookflow/duration.go:27` independently states "Records carry only a structural RFC3339 timestamp (INV-2: no content)" — i.e. a timestamp itself is already treated as content-free elsewhere in this same package, supporting that adding `ts` here trips no invariant.

## Q3: constraints on adding fields

**No sentinel/pinned test forbids this record's shape.** Grep for `enforcements`/`EnforcementRecord`/`INV-2`/`allowlist` near the writer surfaces only a **doc comment**, not a test:
- `enforce.go:557-565` (directly above `EnforcementRecord`): *"It is strictly content-free (INV-1/INV-2): verdict/ids/flags plus the guardrail category types only — never the tool content, **the policy reason free text**, or the guardrail reason free text. ... Being category-only keeps the sink safe even if it's later egressed (e.g. to the dashboard) — no free text to leak."*
- Same claim repeated above `RecordEnforcement`, `enforce.go:517-521`: *"Content-free (INV-1/INV-2)."*

This is a **comment-only constraint** — I found no test that asserts the top-level policy `Reason` is absent. Checked the three tests that unmarshal/inspect the JSON line (`adapters/claude-code/enforce_test.go`):
- `TestRecordEnforcement_NoRedactionLeak` (line 559) — asserts `RedactedContent`/original file-body sentinels absent; doesn't touch `Evaluation.Reason`.
- `TestRecordEnforcement_GuardrailCategoryOnly` (line 803) — asserts `GuardrailReason.Reason` free text ("detected 123-45-6789...") and `.Field` ("ssn") are absent, only `GuardrailCategories:["pii"]` present. `dec.Evaluation.Reason` is unset (zero value) in this test, so it can't be asserting its absence either way.
- `TestRecordEnforcement_ApprovalID` (line 888) — **sets** `dec.Evaluation.Reason = "external repository mutation"`, then asserts only that tool content (`"secret-project-x"`) is absent — never asserts the Reason string is absent from the audit line. So adding `Reason` would not break this test.

**Important distinction the doc comment conflates**: `client.Evaluation.Reason` (top-level, policy-authored, e.g. "destructive recursive delete", "production database migration") vs `client.GuardrailReason.Reason` (nested, can quote scanned content like "detected 123-45-6789 in the argument"). The latter is genuinely INV-2-sensitive (content-derived); the former is **already** treated as non-content elsewhere in this exact codepath: `outputContract.Render`'s doc comment (`adapters/claude-code/outputcontract.go:35-38`) says `permissionDecisionReason` — which IS `dec.Evaluation.Reason` via `GovReason`/`ApprovalReason` (`enforce.go:478-486`) — "carries the policy-authored reason, never the tool command/file/output content (INV-2)" and is already written to stdout (same-machine, no egress) on every deny/ask today. Writing the same string into a same-machine, non-egressing `enforcements.jsonl` is not a new exposure class vs. what already happens on stdout.

INV-1/INV-2/INV-3 have no single canonical glossary entry found (see Unresolved). Meaning is consistent by repeated inline comment: INV-1 = no secrets in logs, INV-2 = no tool-content/transcript-derived content leaves via these paths, INV-3 = audit-sink failures never surface/block (`enforce.go:520` "never surfaced (INV-3)"; `advisory.go:19` "never blocking... (INV-3)").

**Net**: adding top-level `reason` (= `dec.Evaluation.Reason`) is not test-blocked, and is arguably already-exposed content per the stdout precedent — but it directly contradicts the literal doc-comment sentence at `enforce.go:560-561` ("never ... the policy reason free text"), which should be updated/amended alongside the code change (this repo's convention per CLAUDE.md: invariant-adjacent comments are treated as decisions, not incidental). `GuardrailReason.Reason` must stay excluded — no change needed there, `GuardrailCategories` already covers it correctly.

## Q4: existing tests to extend

All in `adapters/claude-code/enforce_test.go` (adapter-level; no `hookflow`-package-level `enforce_test.go` peer exists for `EnforcementRecord` itself):
- `TestRecordEnforcement_NoRedactionLeak` (:559) — redaction-leak guard.
- `TestRecordEnforcement_GuardrailCategoryOnly` (:803) — guardrail free-text exclusion.
- `TestRecordEnforcement_ApprovalID` (:888) — `ApprovalRef` correlation.
- `TestRunHook_EnforceApply_Block` (~:598) / `TestRunHook_EnforceApply_Approval` (mentioned near :888+) — end-to-end hook-binary guards that also read `enfFile` and assert INV-2 on both stdout and the audit line.

`adapters/common/hookflow/gate_test.go:62,194` and `observecopy_test.go:40,148,194` only `t.Setenv(devconfig.EnvEnforcementFile, ...)` for isolation (temp-dir redirection) — they don't unmarshal/assert `EnforcementRecord` fields.
`adapters/claude-code/enforce_conformance_test.go` / `enforce_evaluate_test.go` (`TestEnforcementConformance`, `TestEnforcementConformance_Tier2`) exercise the evaluate/apply cascade broadly, not the audit record's field shape specifically.

Extending for `ts`/`reason`: add positive assertions to `TestRecordEnforcement_ApprovalID` (or a new `TestRecordEnforcement_ReasonAndTimestamp`) that `rec.Reason` equals the policy string and `rec.Timestamp`/`rec.Ts` parses as `RFC3339Nano` and is recent; keep the existing negative assertions in `_GuardrailCategoryOnly` (guardrail free text still absent) and `_NoRedactionLeak` (redacted/original content still absent) unchanged.

## Q5: Reason/decision struct availability at write site — signature change?

**`reason`: no signature change needed.** `RecordEnforcement(logger *log.Logger, sessionID, toolKind string, dec decision.Decision, res ApplyResult)` (`enforce.go:522`) already receives the full `dec`. `decision.Decision` (`decision/redact.go:34-54`) embeds `Evaluation client.Evaluation` (`:35`). `client.Evaluation` (`client/verdict.go:113-126`) has `Reason string` at `:115` (no json tag shown in this struct — wire (de)serialization for `Evaluation` lives elsewhere, not tag-driven on this type). So `dec.Evaluation.Reason` is already in scope inside `RecordEnforcement` — just add `Reason: dec.Evaluation.Reason` to the `EnforcementRecord{}` literal (`enforce.go:523-534`).

**`ts`: no signature change needed either, but no existing value to reuse.** Checked every struct reachable at the call site for a timestamp: `HookEvent` (claude-code `adapters/claude-code/hookevent.go:103-123`, codex `adapters/codex/hookevent.go:76-96`) carries none; `decision.Decision` / `client.Evaluation` / `ApplyResult` (`enforce.go:71-78`: `Decision string, Redacted bool, Emitted bool`) carry none. So `ts` must be generated fresh inside `RecordEnforcement` via `time.Now().UTC().Format(time.RFC3339Nano)` (matches Q2 convention) — a same-function addition, not a signature change. A signature/clock-injection change (mirroring `mapper.go`'s `m.clock` seam) would only be needed if tests want a deterministic/mockable clock for the new field; none of the current `RecordEnforcement` tests control time.

## Unresolved questions

1. No single canonical INV-1/INV-2/INV-3 glossary entry found (repo-wide grep hit ~90 files using the labels inline); meaning inferred from repeated comments, not from one definition site — could not fully verify within budget whether a formal decision-level definition exists elsewhere (e.g. an early decision record not surfaced by my greps).
2. Whether the plan intends `reason` to be `dec.Evaluation.Reason` verbatim, or a synthesized string (e.g. via existing `GovReason`/`ApprovalReason` helpers, which add "OpenBox governance: "/policy-id framing) — both are available at the write site; I did not find a stated preference in code.
3. Whether `hookflow` needs a test-injectable clock for the new `ts` field (none exists today in this package, unlike `mapper.go`'s `m.clock`) — a judgment call for the implementer, not something the current tests require.
