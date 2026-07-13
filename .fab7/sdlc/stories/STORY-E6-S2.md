# STORY-E6-S2 — `apply(verdict)` on the Claude Code adapter

**Epic:** E6 (enforcement — the `apply` leg). **Risk:** high (this is the first path that turns a governance verdict into an ACTUAL block/ask on a Claude Code tool call — `WouldBlock()` becomes a real block. A wrong deny wedges the dev loop; a leaked reason breaches INV-2). **Status target:** review (build + validations + both reviews, pending brian G3 + Sam G_SEC).

## Source
- **Backlog:** `.fab7/sdlc/stories/E6-backlog.md` §E6-S2 — "map `Evaluation.Verdict` → CC `permissionDecision`: `BLOCK`/`HALT` → `deny` (+ reason); `REQUIRE_APPROVAL` → `ask` (pending OD-HITL); `CONSTRAIN` → allow-with-constraints logged; `ALLOW` → allow. Guardrail redaction → `updatedInput` (E6-S4). The observe→enforce flip is a config flag (arch D7); default observe. `WouldBlock()` becomes the real block." Write scope: `adapters/claude-code/`. Deps: E6-S1. Gates: G3, G_SEC. Invariants: INV-3b, INV-2.
- **E6-S1 (DONE 2026-07-14, committed 268fbfb):** the enforce gate OBTAINS a `sidecar.Decision` synchronously (bounded ~50 ms, fail-open) and surfaces it through `EnforceDecision`; the apply seam was left as a comment in `hookrun.go` (`// E6-S2 apply seam`). E6-S1 explicitly **deferred the durable enforcement-decision record to E6-S2** (STORY-E6-S1 AC-5).
- **Cross-repo recon (openbox-temporal-sdk-python, 2026-07-14):** `verdict_handler.enforce_verdict` (verdict_handler.py:50-103) is the reference cascade this story ports. Order: **HALT > BLOCK > guardrails-validation-failure > REQUIRE_APPROVAL > CONSTRAIN > ALLOW**. HALT→`GovernanceHaltError` (terminate); BLOCK→`GovernanceBlockedError` (non-retryable); a `guardrails_result.validation_passed==false`→`GuardrailsValidationError`, **checked before approval** so a guardrail failure is never swallowed by an approval flow (verdict_handler.py:84-90) and independent of the verdict value; REQUIRE_APPROVAL→`requires_hitl` (the caller raises retryable `ApprovalPending`); CONSTRAIN→logged allow (no data mutation); guardrail **redaction** of the input is a SEPARATE caller step (`_apply_input_redaction`, activity_interceptor.py:441-478) — that is **E6-S4**, not this story.
- **Claude Code hook contract (verified 2026-07-14):** a PreToolUse hook that exits 0 and prints `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny|allow|ask","permissionDecisionReason":"…"}}` has that decision honored — `deny` blocks (Claude sees the reason), `ask` prompts the user, `allow` permits without prompting. Exit 0 + JSON is the decision channel (exit 2 is a blocking error that IGNORES JSON). `updatedInput` rewrites the tool input (reserved for E6-S4).

## Scope boundary (what this story is and is NOT)
- **IS:** the verdict→`permissionDecision` mapping (`mapVerdict`), the stdout writer (`applyDecision`), the wiring in the `PreToolUse` enforce branch, and the durable enforcement-decision audit record E6-S1 deferred here.
- **IS NOT:** guardrail **redaction** / `updatedInput` (E6-S4, gated on content posture); the per-org **fail-closed** policy + explicit timeout knob (E6-S3 — the fail-**open** default is inherited from E6-S1's `sidecar.Client`); the **interactive HITL** prompt-loop refinement (E6-S6 layers on the `ask` mapping this story establishes); the conformance suite (E6-S7).

## The one design rule — governance only TIGHTENS
A non-blocking verdict (CONSTRAIN / ALLOW / UNKNOWN-fail-open) writes **NOTHING** to stdout, so Claude Code's own permission flow is untouched and behaves exactly as in observe mode. Only `deny`/`ask` are ever emitted — enforcement can ADD a restriction, never REMOVE one of Claude Code's built-in prompts. This keeps the observe/advisory path byte-identical whenever nothing is blocked, and makes a fail-open or ALLOW indistinguishable from Phase-1 on stdout.

## Acceptance Criteria
1. **Verdict cascade (OD-ENF-SCOPE)** — `mapVerdict(client.Evaluation)` ports the SDK order: `HALT`/`BLOCK` → `deny`; a failed guardrail validation (`Guardrail!=nil && !Guardrail.Passed`) → `deny`, checked after HALT/BLOCK but before approval and independent of the verdict; `REQUIRE_APPROVAL` → `ask`; `CONSTRAIN`/`ALLOW`/`UNKNOWN` → no decision (proceed).
2. **Apply to stdout** — in enforce mode, a `PreToolUse` decision maps through `applyDecision(stdout, dec)` which writes the CC `permissionDecision` JSON (exit 0 remains). Only `deny`/`ask` are written; everything else writes nothing.
3. **Tighten-only / observe-equivalent** — a CONSTRAIN/ALLOW/UNKNOWN verdict and enforce-OFF are byte-identical on stdout (empty). A test asserts nothing is written for allow/fail-open, and the enforce-off observe path is unchanged.
4. **Fail-open preserved** — sidecar absent/slow → `VerdictUnknown` → nothing written → tool proceeds (OD9). A nil stdout or a marshal/write fault degrades to "proceed"; the apply NEVER wedges or fails a call (INV-3b).
5. **Content-free reason (INV-2)** — the `permissionDecisionReason` surfaces the POLICY-authored reason + policy id (local-only, shown to Claude on the same machine; not egressed), never the tool command/file/output content. A guardrail-failure reason carries only the CATEGORY types (`[pii,…]`), never the guardrail free text.
6. **Durable enforcement audit (E6-S1 deferral)** — each applied decision appends one content-free line to an enforcement audit sink (`enforcements.jsonl`, `OPENBOX_ENFORCEMENT_FILE`), off the blocking path, best-effort (a failure is logged and swallowed — INV-3). Categories/ids/verdict/applied-decision only; never the command/path/content.
7. **Only PreToolUse, enforce-only** — non-`PreToolUse` hooks and observe mode never write a decision (inherited from E6-S1's gate).

## Write Scope
- `adapters/claude-code/enforce.go` — `applyDecision`, `mapVerdict`, `govReason`/`guardrailReason`, the `preToolUseOutput` contract structs, `enforcementRecord`/`DefaultEnforcementPath`/`recordEnforcement`.
- `adapters/claude-code/advisory.go` — factor the JSONL append into a shared `appendJSONL` (one on-disk perms routine for both the SL-9 advisory sink and the enforcement sink).
- `adapters/claude-code/hookrun.go` — thread a `stdout io.Writer`; call `applyDecision` + `recordEnforcement` in the gated `PreToolUse` enforce branch.
- `adapters/claude-code/creds.go` — `OPENBOX_ENFORCEMENT_FILE` env constant.
- `adapters/claude-code/enforce_test.go` — `mapVerdict` cascade, `applyDecision`, E2E block/ask/proceed + audit.
- `cli/cmd/openbox/main.go`, `adapters/claude-code/cmd/openbox-cc-hook/main.go` — pass the process stdout into `RunHook`.

## Invariants
- **INV-3b:** the block is synchronous, pre-execution (a `PreToolUse` `deny`/`ask`), bounded by E6-S1's timeout, fail-open by default.
- **INV-2:** the command/file metadata never egresses; the stdout reason and the durable audit carry categories/ids/policy-authored text only, never tool content.
- **INV-1:** no secret on the hot path / in the record / on stdout.

## Human Gates
| Gate | Question | Owner | Outcomes |
|---|---|---|---|
| G3_REVIEW | Does the apply map the full SDK verdict scope onto CC deny/ask, tighten-only, fail-open, observe-equivalent when nothing blocks? | brian | approve / revise |
| G_SEC | Is the stdout reason + durable audit content-free (INV-2), the path secret-free (INV-1), and the block bounded/pre-execution/fail-open (INV-3b)? | Sam | approve / revise / block |

## Validation
```bash
cd adapters/claude-code && go build ./... && go vet ./... && go test -race ./...
cd ../../sidecar && go build ./... && go test -race ./...
cd ../cli && go build ./... && go vet ./... && go test ./...
# Live: enforce on + `openbox sidecar serve` with the example bundle →
#   rm -rf /        → deny   (stdout permissionDecision + policy reason)
#   github MCP      → ask
#   .env / echo hi  → nothing to stdout (proceed); recorded in enforcements.jsonl
#   enforce off     → nothing to stdout even for rm -rf
```

## Stop conditions
- If the apply ever writes `allow` (loosening CC's own prompts) → STOP: governance is tighten-only.
- If a deny/ask reason or the audit carries the shell command / file body / tool output → STOP (INV-2).
- If an apply fault (nil stdout, marshal error) could wedge or fail a tool call → STOP (must fail-open).
- If this story applies guardrail redaction / `updatedInput` → STOP (that is E6-S4).
