# Phase 03 — `enforcements.jsonl` gains `ts` + `reason`

## Context links

- Plan: [plan.md](plan.md) · Blocks on: — · Parallel-safe with phases 01, 02 (different repo areas)
- Research: [researcher-02](research/researcher-02-shiftleft-audit-record.md)
- Motivating friction: [debug-260814-1231](../reports/debug-260814-1231-session-no-longer-active-halt.md)
  §"Diagnosis-time friction" — correlating three denials to a clock required joining two files.
- Repo: **openbox-shift-left** (this repo).

## Overview

- **Date:** 2026-08-14 · **Priority:** P1 · **Effort:** 1.5h
- **Description:** Add a generated `ts` and the policy-authored `reason` to `EnforcementRecord`,
  amend the two doc comments that currently forbid the reason, and pin both with tests while
  keeping every existing content-exclusion assertion intact.
- **Implementation status:** pending
- **Review status:** not reviewed

## Key Insights

1. **No test blocks this; a doc comment does.** `enforce.go:560` literally says the record carries
   "never the tool content, **the policy reason free text**, or the guardrail reason free text".
   The user decision supersedes it, so the comment is amended in the same change — this repo treats
   invariant-adjacent comments as decisions, not incidental prose.
2. **Two different `Reason`s, and only one is content-derived.** `client.Evaluation.Reason`
   (top-level, policy-authored) vs `client.GuardrailReason.Reason` (nested, can quote scanned
   content). The former is **already** written to stdout on every deny/ask via `GovReason` /
   `ApprovalReason` (`enforce.go:478-486`), documented as "policy-authored … never the tool
   command/file/output content" (`adapters/claude-code/outputcontract.go:35-38`). Writing the same
   string into a same-machine, non-egressing sink is not a new exposure class. The guardrail one
   stays excluded — `GuardrailCategories` already covers it correctly.
3. **`ts` must be generated; nothing at the call site carries one.** `HookEvent`,
   `decision.Decision`, `client.Evaluation` and `ApplyResult` (`enforce.go:71-78`) all lack a
   timestamp (researcher-02 Q5). Generate inline: `time.Now().UTC().Format(time.RFC3339Nano)` —
   the repo-wide convention (`adapters/claude-code/mapper.go:157-158`, codex `mapper.go:150-153`,
   advisories via `client.DevEvent.Timestamp`, `client/event.go:258`).
4. **No clock seam.** No current test needs a deterministic timestamp, and inventing an injectable
   clock for one field would be scope the user did not ask for. Assert **parseability + recency**
   instead. A timestamp is already treated as content-free in this same package
   (`adapters/common/hookflow/duration.go:27`).
5. **No signature change, one writer, both adapters benefit.** `RecordEnforcement` already receives
   the whole `dec` (`enforce.go:522`), and the two call sites
   (`adapters/claude-code/outputcontract.go:72-75`, `adapters/codex/outputcontract.go:87-90`) stay
   untouched — DRY holds by construction.

## Requirements

- R1: `EnforcementRecord` gains `Timestamp string \`json:"ts"\`` (always present) and
  `Reason string \`json:"reason,omitempty"\``.
- R2: `Reason` is `dec.Evaluation.Reason` **verbatim** — raw policy string, NOT `GovReason`-framed
  ("OpenBox governance: …" / "(policy: …)"). The audit sink records what the server said.
- R3: `ts` is generated inside `RecordEnforcement`, UTC, RFC3339Nano.
- R4: No signature change; no call-site change; Codex inherits both fields.
- R5: `GuardrailReason.Reason` stays excluded — categories only, unchanged.
- R6: Doc comments at `enforce.go:517-521` and `:557-565` amended to state what is now recorded and
  what remains excluded, and why the distinction holds.
- R7: Existing negative assertions unchanged and still green.

## Architecture

`RecordEnforcement` (`enforce.go:522-548`) builds the record literal at `:523-534`; the two new
fields are two more lines in that literal. Field naming mirrors `AdvisoryRecord.Timestamp`
(`advisory.go:48`) so the two sinks read alike — but without `omitempty`, since a generated value
is always present. Append the fields to the struct; JSONL has no golden byte pin (the goldens under
`client/testdata/golden/` cover the wire payload, not these sinks), so ordering is free.

## Related code files

| File:line | Change |
|---|---|
| `adapters/common/hookflow/enforce.go:566-583` | `EnforcementRecord` — add `Timestamp`, `Reason` |
| `adapters/common/hookflow/enforce.go:523-534` | record literal — set both |
| `adapters/common/hookflow/enforce.go:517-521`, `:557-565` | doc comments amended |
| `adapters/claude-code/enforce_test.go:888` | `_ApprovalID` — already sets `Evaluation.Reason` |
| `adapters/claude-code/enforce_test.go:559`, `:803` | negative assertions — keep unchanged |
| `adapters/claude-code/outputcontract.go:72-75`, `adapters/codex/outputcontract.go:87-90` | unchanged (no signature change) |

## Implementation Steps

1. Add the two fields to `EnforcementRecord`.
2. Set them in the literal: `Timestamp: time.Now().UTC().Format(time.RFC3339Nano)`,
   `Reason: dec.Evaluation.Reason`.
3. Amend both doc comments: record now carries the policy-authored reason and a structural
   timestamp; still never the tool content, the guardrail reason free text, or a secret. State the
   `Evaluation.Reason` vs `GuardrailReason.Reason` distinction explicitly so the next reader does
   not re-conflate them.
4. Add `TestRecordEnforcement_ReasonAndTimestamp` in `adapters/claude-code/enforce_test.go`:
   `rec.Reason` equals the policy string verbatim (and is NOT `GovReason`-framed — assert the
   "OpenBox governance:" prefix is absent); `rec.Timestamp` parses with `time.RFC3339Nano` and is
   within a generous recency window of `time.Now()`.
5. Verify `_GuardrailCategoryOnly` (`:803`) and `_NoRedactionLeak` (`:559`) still pass unchanged —
   if either now fails, the change leaked something it should not have.
6. `go test ./...` in the touched modules.

## Todo list

- [ ] Fields added to `EnforcementRecord`
- [ ] Values set in `RecordEnforcement`
- [ ] Both doc comments amended (including the reason-vs-guardrail-reason distinction)
- [ ] `TestRecordEnforcement_ReasonAndTimestamp` added and green
- [ ] `_GuardrailCategoryOnly` and `_NoRedactionLeak` green, unmodified
- [ ] Touched modules green

## Success Criteria

1. Every new `enforcements.jsonl` line has a `ts` that parses as RFC3339Nano and is recent.
2. A denied call's line carries the verbatim server reason, un-framed.
3. Guardrail free text and tool content remain absent — the two negative tests pass without edits.
4. No signature change, no call-site change; Codex records both fields with zero adapter edits.
5. A future repeat of this incident is a one-file lookup — reason and clock in the same record.

## Risk Assessment

- **R-A — the policy reason is not as content-free as the stdout precedent implies** (a policy could
  author a reason that quotes the tool command). *Break signal:* a real `enforcements.jsonl` line is
  found containing tool content sourced from `Evaluation.Reason`. *Pre-decided response:* stop and
  surface — the exposure would already exist on stdout, so the fix is a server/policy-authoring
  concern, not a reason to keep the audit sink blind. Do not silently truncate.
- **R-B — recency assertion flakes** in a slow or heavily parallel CI run. *Break signal:* the new
  test fails on timing, never on format. *Pre-decided response:* adjust in-plan — widen the window;
  do NOT introduce a clock seam for it (Key Insight 4).
- **R-C — a downstream consumer parses these lines strictly** and breaks on new keys. *Break
  signal:* a reader errors on the added fields. *Pre-decided response:* adjust in-plan — both fields
  are additive; a strict reader is the defect. Confirm no testbed assertion does exact-object
  equality on an enforcement line before merging.

## Security Considerations

The sink is same-machine, owner-only (0700 dir / 0600 file, `advisory.go:100-115`) and does not
egress today. The added reason is the same string already printed to stdout on every deny/ask, so
the local exposure class is unchanged. If this sink is ever egressed (the doc comment anticipates a
dashboard), the reason becomes egressed policy text — acceptable because it is policy-authored, not
content-derived, and the amended comment must say so explicitly so the next reader can make that
call with the distinction in hand. Secrets and redacted bodies remain excluded, pinned by
`_NoRedactionLeak`.

## Next steps

Independent of the core fix — merges on its own schedule. It makes phase 04's replay easier to read
(one file instead of a two-file join) but is not a blocker for it.
