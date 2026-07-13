# STORY-E6-S6 — REQUIRE_APPROVAL → CC `ask` (interactive local HITL prompt)

**Epic:** E6 (enforcement — the `apply` leg). **Risk:** low-medium (this refines an EXISTING mapping — E6-S2 already maps `REQUIRE_APPROVAL` → `ask` and is tighten-only/fail-open. The delta is the content-free approval *reason* surfaced to the local prompt and the approval id recorded in the audit — a leaked reason would breach INV-2; a regression in the mapping could wedge/loosen a call). **Status:** DONE 2026-07-14 (build + validations + live E2E + both reviews APPROVE + brian G3 sign-off; committed to main + pushed, no signature).

## Source
- **Backlog:** `.fab7/sdlc/stories/E6-backlog.md` §E6-S6 — "map `REQUIRE_APPROVAL` → CC's `ask` (interactive local prompt), per OD-HITL (brian 2026-07-13). NOT the SDK's server-side `/approval` polling (too heavy for the hot path). Part of the full-SDK-scope verdict handling (OD-ENF-SCOPE)." Deps: E6-S2. Gates: G3.
- **OD-HITL (DECIDED, brian 2026-07-13):** map `REQUIRE_APPROVAL` → CC `ask` (interactive local prompt), **not** server-side `/approval` polling.
- **E6-S2 (DONE, committed ccb7e93):** `mapVerdict` already maps `REQUIRE_APPROVAL` → `ccDecisionAsk` with `govReason(e, "action requires approval per OpenBox governance policy")` (a generic fallback + `policy_id`). `applyDecision` writes the CC `permissionDecision`; `recordEnforcement` appends a content-free `enforcements.jsonl` line. The E6-S2 comment already marks this branch "E6-S6 refines UX".
- **Cross-repo recon (openbox-temporal-sdk-python, 2026-07-14 — Explore):** the reference HITL path is fundamentally **async + server-side**, and the CC path deliberately is **not**:
  - `verdict_handler.enforce_verdict` **returns** `VerdictEnforcementResult(requires_hitl=True)` for `REQUIRE_APPROVAL` (`verdict_handler.py:92-94`) — it does not raise. The caller (`activity_interceptor.py:414-420`) sets `buffer.pending_approval=True` and raises a **retryable** `ApplicationError(type="ApprovalPending")` (`hitl.py:114-125`).
  - The OUTCOME is obtained by **polling** `POST /api/v1/governance/approval` on each Temporal retry (`client.poll_approval`, `client.py:150-198`); `handle_approval_response` (`hitl.py:46-111`) maps it to approved (`Verdict.ALLOW`→`True`) / rejected (`should_stop()`→`ApprovalRejected`) / expired (`ApprovalExpired`) / still-pending (retry). Timeout is server-driven via `approval_expiration_time`.
  - **In Claude Code there is no retry loop and no poll:** the `ask` `permissionDecision` makes Claude Code show the developer a native allow/deny prompt, resolved **synchronously on this machine**. That prompt IS the human-in-the-loop. So the entire SDK poll/expiry/retry apparatus collapses into CC's own `ask` UI, and the hook's ONLY lever on the experience is the `permissionDecisionReason` string.
  - **The one approval-specific field the SDK reads off the evaluate response** is `approval_id` (`GovernanceVerdictResponse.approval_id`, `types.py:142`, parsed at `:186`). It is a server correlation id (like `policy_id` / `governance_event_id`), **not content**. `openbox-shift-left/client/verdict.go` already parses it into `Evaluation.ApprovalID`, but the adapter currently **drops it**. There is NO `approver` / `approval_url` / `approvers` field anywhere — the SDK never consumes one.

## The delta (what E6-S6 adds over E6-S2)
Because CC's `ask` already IS the interactive prompt, E6-S6 is NOT new control flow — it is the faithful HITL **reason + audit** refinement layered on the E6-S2 `ask` mapping:
1. A dedicated `approvalReason(e)` (replacing the generic `govReason` fallback on the `REQUIRE_APPROVAL` branch) that surfaces the full **content-free** approval context the SDK reads off the evaluate response: the policy-authored reason (mirroring the SDK's `f"Approval required: {reason or 'Activity requires human approval'}"`), the `policy_id`, and — the new bit — the **`approval_id`** so the developer/auditor can tie this prompt to the governance approval record.
2. `approval_id` added to the durable `enforcementRecord` (content-free correlation id) so an `ask` decision in `enforcements.jsonl` is correlatable to the governance approval.

## Scope boundary (what this story is and is NOT)
- **IS:** `approvalReason(e)`; wiring `mapVerdict`'s `REQUIRE_APPROVAL` branch to it; adding the content-free `approval_id` to `enforcementRecord`/`recordEnforcement`; refreshing the E6-S2 header/comment ("E6-S6 refines UX" → done); tests + a live `ask` E2E.
- **IS NOT:**
  - **Server-side `/approval` polling / the retry-based `pending_approval` loop / expiry handling** — OD-HITL rejected it; CC's `ask` resolves synchronously (NO new endpoint, NO `poll_approval` port).
  - **Capturing the approval OUTCOME** (approved/denied via PostToolUse correlation) — the PreToolUse hook returns before the developer decides; correlating Pre↔Post is a distinct surface. Noted fast-follow, out of scope.
  - **A `hitl_enabled` on/off toggle** (SDK `config.hitl_enabled` / `skip_hitl_activity_types`) — disabling approval is a new human decision (proceed? deny?) OD-HITL did not rule; the SDK's skip exists only to break a governance-event recursion the dev runtime does not have. Out of scope.
  - The verdict cascade priority, the stdout writer, guardrail redaction (E6-S4), the failure policy (E6-S3), the conformance suite (E6-S7) — all unchanged.
- **NO new config/env, NO new sidecar/core/backend surface.** `approval_id` already arrives on the parsed `Evaluation`.

## The design rules (inherited, must hold)
- **Governance only TIGHTENS.** `REQUIRE_APPROVAL` → `ask` only; a non-blocking verdict still writes nothing. `ask` never becomes `allow`; enforcement never removes a CC prompt.
- **Content-free reason (INV-2).** The reason carries policy-authored text + server ids (`policy_id`, `approval_id`) only — never the tool command / file / output. `approval_id` is an opaque correlation id, provably content-free (same class as `policy_id`/`governance_event_id`, already surfaced/recorded).

## Acceptance Criteria
1. **Approval reason (content-free)** — `mapVerdict` maps `REQUIRE_APPROVAL` → `ask` with `approvalReason(e)`, which surfaces the policy reason (fallback: a generic "requires human approval" message), `policy_id` when present, and `approval_id` when present. A test asserts the approval id and policy id appear and that a tool-content free-text string does NOT.
2. **Approval id in the audit** — `enforcementRecord` gains a `approval_id,omitempty` field; `recordEnforcement` populates it from `dec.Evaluation.ApprovalID`. Absent when core sends none. A test asserts an `ask` audit line carries the approval id and no command/path/content.
3. **Mapping unchanged elsewhere** — HALT/BLOCK/guardrail-fail still `deny`; CONSTRAIN/ALLOW/UNKNOWN still proceed (no decision). Only the `REQUIRE_APPROVAL` reason text + the audit's `approval_id` change; the emitted `permissionDecision` for `ask` is byte-identical (still `"ask"`).
4. **Tighten-only / observe-equivalent preserved** — enforce-off and non-blocking verdicts are byte-identical on stdout; `ask` is emitted ONLY in enforce mode on a `REQUIRE_APPROVAL` (inherited from E6-S1/S2). A test confirms enforce-off writes nothing even for `REQUIRE_APPROVAL`.
5. **Fail-open preserved** — a fail-open (`VerdictUnknown`) decision never yields `ask`; a nil stdout / marshal fault still degrades to proceed (inherited; no regression). No new failure mode.

## Write Scope
- `adapters/claude-code/enforce.go` — add `approvalReason`; point the `REQUIRE_APPROVAL` branch of `mapVerdict` at it; add `ApprovalID` to `enforcementRecord` + set it in `recordEnforcement`; refresh the header/cascade comments ("E6-S6 refines UX" → implemented).
- `adapters/claude-code/enforce_test.go` — `approvalReason` (surfaces reason/policy_id/approval_id, content-free); `mapVerdict` `REQUIRE_APPROVAL` still `ask`; `recordEnforcement` ask line carries `approval_id`; enforce-off byte-identical for `REQUIRE_APPROVAL`.

## Invariants
- **INV-3b:** unchanged — the `ask` is a synchronous, pre-execution, bounded, tighten-only decision; fail-open by default.
- **INV-2:** the approval reason + audit carry policy-authored text + server ids (`policy_id`, `approval_id`) only, never tool content. Shown on this machine only (stdout → Claude Code), never egressed.
- **INV-1:** no secret on the path / in the reason / in the record (approval id is a public correlation id, not a credential).

## Human Gates
| Gate | Question | Owner | Outcomes |
|---|---|---|---|
| G3_REVIEW | Does `REQUIRE_APPROVAL` map to CC `ask` with a clear, content-free approval reason (policy reason + policy_id + approval_id), tighten-only, with no change to the rest of the cascade, and NO server-side polling? | brian | approve / revise |
| G_SEC (light) | Is the approval reason + audit content-free (INV-2 — approval_id is an id, not content), the path secret-free (INV-1), and the `ask` still bounded/pre-execution/tighten-only (INV-3b)? | Sam | approve / revise / block |

## Validation
```bash
cd adapters/claude-code && go build ./... && go vet ./... && go test -race ./...
cd ../../sidecar && go build ./... && go test -race ./...
cd ../cli && go build ./... && go vet ./... && go test ./...
# Live: enforce on + `openbox sidecar serve` with a REQUIRE_APPROVAL rule (e.g. github MCP) →
#   github MCP  → ask (stdout permissionDecision "ask" + reason showing approval/policy ids)
#   rm -rf /    → deny  (unchanged)
#   echo hi     → nothing to stdout (proceed, unchanged)
#   enforce off → nothing to stdout even for the approval-required tool
#   enforcements.jsonl ask line carries approval_id, NO command/path/content
```

## Stop conditions
- If E6-S6 ports the SDK's server-side `/approval` polling / `poll_approval` / `pending_approval` retry loop / expiry → STOP (OD-HITL: CC `ask` resolves synchronously; no polling).
- If the approval reason or audit carries the shell command / file body / tool output → STOP (INV-2).
- If `ask` ever becomes `allow`, or the change loosens/alters the HALT/BLOCK/guardrail/CONSTRAIN/ALLOW mapping → STOP (tighten-only; only the approval reason text + audit approval_id change).
- If this story adds a `hitl_enabled` toggle or any new config/env/endpoint → STOP (out of scope; a new OD).
