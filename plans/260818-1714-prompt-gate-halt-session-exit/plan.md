---
title: "Prompt gate + HALT terminates the session"
description: "Gate PromptSubmitted through /evaluate like tool calls; a server HALT stops the Claude Code session (continue:false + local latch), not just one call."
status: implemented — unit/conformance-verified; testbed §A3 dormant, awaiting a stack
priority: P1
effort: 6h
branch: main
tags: [enforcement, claude-code, hookflow, prompt-gate, session-halt]
created: 2026-08-18
---

# Prompt gate + HALT session termination

## Objective (user decisions, 2026-08-18)

1. `UserPromptSubmit` becomes a synchronous gate: the PromptSubmitted event is evaluated
   inline by `/evaluate` before the prompt is processed; HALT/BLOCK blocks (and erases) the
   prompt.
2. A HALT **returned by OpenBox** terminates the Claude Code session: the current turn stops
   immediately and the session can never do governed work again.

Diagnosis this responds to: [debug-260818-1656](../reports/debug-260818-1656-halt-block-does-not-stop-loop.md).

## Validated decisions (AskUserQuestion, 2026-08-18)

- **Every server HALT kills** — no authored/unauthored discrimination (keeps ADR-0017 trust
  boundary + plan 260814-2235's no-discrimination stance). Consequence accepted: until the
  core precondition fix deploys, an unauthored HALT terminates sessions.
- **Protocol stop + latch** — `continue:false` + `stopReason` (documented, overrides
  per-event decisions) plus a local per-session latch; every later prompt in that session is
  blocked-and-erased and every later tool call denied, `--resume` included. No process kill:
  the hook protocol has none; in headless `-p` the turn stop ends the run.
- **Fail-closed synthesized HALT stays a per-call deny** — only a HALT actually returned by
  the control plane terminates the session (`FailOpen==true` never kills).
- **REQUIRE_APPROVAL on a prompt: hold, then block if unanswered** — reuse the E9 hold
  unchanged.

## Validation Summary

**Validated:** 2026-08-18 · **Questions asked:** 7 (4 pre-implementation, above; 3
post-implementation, below)

### Confirmed Decisions (post-implementation round)
- **Codex is out of scope, deliberately** ("ignore codex for now — focus on Claude
  Code"): HALT on Codex stays a per-call deny with no latch; the documented limit
  stands, no follow-up queued.
- **Prompts fail closed too**: under `fail_closed:true` an outage blocks prompts as
  well as tool calls — consistent with the fail-closed promise, no prompt carve-out.
- **Latch operability stays as-is**: no doctor listing, no unlatch command;
  enforcements.jsonl + the refusal reason + the documented file location are the
  operator surface.

### Action Items
- [ ] Run `testbed/30-enforce.sh` §A3 against a live stack (the one remaining
  verification gap; also resolves unresolved question 1).

## Design

### Kill discriminator (the part that must be exact)

`dec.SessionHalt` (new bool on `decision.Decision`) set in `EnforceGate.Run` **after** the
approval hold resolves, iff `Verdict==HALT && Source==SourceEvaluate && !FailOpen`. Excluded
by construction:

- fail-closed synthesized HALT (`FailOpen=true`, ApplyFailurePolicy);
- approver REJECT (`Source=approval:decided`) — per-call deny, a human refused the call;
- undecided approval — `ApprovalUndecided` today mutates verdict→HALT with Source left
  `evaluate`, which would kill on every hold timeout. Fix: new
  `SourceApprovalUndecided="approval:undecided"` set inside `ApprovalUndecided` (also makes
  the enforcement audit stop conflating hold-timeouts with real server HALTs).

### Decision literal

New shared literal `DecisionHalt="halt"`. `MapVerdict`: HALT→`DecisionHalt` (BLOCK stays
`DecisionDeny`). `ApplyDecision` downgrades `DecisionHalt`→`DecisionDeny` unless
`dec.SessionHalt`. Contracts render their strongest expression:

- CC PreToolUse: `{"continue":false,"stopReason":R,"hookSpecificOutput":{permissionDecision:"deny",…}}`, applied=`halt`.
- CC UserPromptSubmit (new contract): halt → `{"continue":false,"stopReason":R,"decision":"block","reason":R}`;
  deny → `{"decision":"block","reason":R}`; proceed → write nothing. `ApprovalDecision()`
  = "block" (no native ask for prompts; near-dead — the hold intercepts first).
- Codex: explicit `DecisionHalt`→deny case (its Render's default writes NOTHING — without
  the case a HALT would silently proceed, a regression). Applied stays `deny` ⇒ no latch,
  Codex behavior byte-identical to today (session-kill for Codex is a noted follow-up).

### Latch

`hookflow/sessionhalt.go`: `<UserConfigDir>/openbox/halted-sessions/<sanitized-sid>-<sha8>.json`
(`OPENBOX_HALT_DIR` override for tests), content `{reason, policy_id, ts}` — the
policy-authored reason (already shown locally on denies; INV-2 class unchanged;
enforcements.jsonl stays category-only). Written by the shared gate when
`res.Decision==DecisionHalt && res.Emitted`. Consulted at the top of CC's gated path
(PreToolUse + UserPromptSubmit): observe copy still spooled, then a synthesized
`Source="session-halt"` HALT decision renders continue:false+deny/block with **no server
round-trip**; recorded in the enforcement audit. Corrupt-but-present latch ⇒ still halted
(generic reason): presence is the decided state. Latch write failure ⇒ logged, this turn
still stopped by continue:false (fail-open on later calls; same class as any apply fault).

### Prompt gate wiring (CC)

`gated := ResolveEnforce() && (PreToolUse || UserPromptSubmit)`. Same deferred-spool /
OnDelivered dedupe as tool calls (mapper clock already pinned; inline copy and observe copy
derive one event_id). `promptTarget` implements `EnforceTarget` over the same
`Mapper.Map(HookUserPromptSubmit,…)` used by observe — prompt content rides only under
content_capture and through `Mapper.RedactContent` (redact-before-egress preserved).
ToolInput nil ⇒ no rewrite path; audit `tool_kind:"prompt"`. `EnforceGate.Run` now returns
`ApplyResult`; findings surfacing on a gated prompt runs only when the gate emitted nothing
(one stdout JSON writer per hook run).

### Registration

`UserPromptSubmit` timeout 5→`preToolUseHookTimeoutSec` (30s) + gating statusMessage, in BOTH
`localhooks.go` and `plugin/hooks/hooks.json` (`TestLocalHooksMirrorPluginBundle` pins them).
Ceiling: prompt gate reuses the evaluator's declared 30s Gating ceiling — registered timeout
must equal it. **Existing installs need `openbox init` re-run** (same as ADR-0018 hooks).

## Files

| File | Change |
|---|---|
| decision/redact.go | `Decision.SessionHalt bool` |
| adapters/common/hookflow/enforce.go | `DecisionHalt`; MapVerdict HALT→halt; ApplyDecision downgrade; log session_halt |
| adapters/common/hookflow/approvalhold.go | `SourceApprovalUndecided`; set in ApprovalUndecided |
| adapters/common/hookflow/gate.go | Run returns ApplyResult; sets SessionHalt; writes latch |
| adapters/common/hookflow/sessionhalt.go (new) | latch write/read/sanitize + SessionHaltDecision |
| adapters/common/devconfig/devconfig.go | `EnvHaltDir` |
| adapters/claude-code/outputcontract.go | tool Render halt shape (`Continue *bool`, StopReason); prompt contract |
| adapters/claude-code/promptgate.go (new) | promptTarget, prompt DecisionRequest, record helper |
| adapters/claude-code/hookrun.go | gated set incl. prompt; latch short-circuit; contract dispatch; findings-after-proceed |
| adapters/claude-code/localhooks.go + plugin/hooks/hooks.json | UserPromptSubmit 30s + statusMessage |
| adapters/codex/outputcontract.go | explicit halt→deny case |
| adapters/claude-code/capabilities.go | verdict.apply text: prompt gate + session stop |
| docs/architecture.md, getting-started.md, data-and-privacy.md, README.md, docs/adr/ADR-0020, CLAUDE.md | semantics, re-init, prompt inline egress, limits |

## Acceptance criteria

1. Inline HALT on a gated tool call ⇒ stdout carries `continue:false` + deny; latch file
   exists; next PreToolUse AND next UserPromptSubmit in that session render halt locally
   with zero `/evaluate` requests; observe copies still spool.
2. Inline HALT on a gated prompt ⇒ `{"continue":false,…,"decision":"block"}`; latch as (1).
3. BLOCK (tool or prompt) ⇒ deny/block only — no continue:false, no latch.
4. Fail-closed synthesized HALT ⇒ plain deny, no latch (pinned by test).
5. Undecided approval ⇒ deny/block with approval ref, `source:"approval:undecided"`, no
   latch (pinned by test — the regression that would kill sessions on hold timeouts).
6. Codex: HALT verdict renders deny bytes (no silent proceed), no latch.
7. Gated prompt ALLOW + findings on ⇒ findings line still emitted, single stdout writer.
8. All 11 modules green under `-race`; windows + linux-arm64 cross-compiles pass.
9. Docs updated; testbed assertions added dormant (stack not reachable — same status
   discipline as ADR-0017/0018 work).

## Known limits (to state in docs)

- Parallel tool calls already past the gate when the HALT lands still run (pre-execution
  gate; unavoidable).
- Interactive REPL process stays open (hook protocol has no process exit); it is inert —
  every prompt/tool refused.
- Prompt approval addressability is session-coarse (`activityPairKey` = session+tool for
  signal events; identity byte-pinned, not changed) — worst case a hold waits and blocks;
  direction over-ask/over-block.
- Until the core unauthored-HALT fix deploys, a precondition-failure HALT terminates the
  session (accepted above).

## Unresolved questions

1. Whether core mints pollable approval rows for PromptSubmitted REQUIRE_APPROVAL verdicts —
   testbed to confirm; every failure mode lands on block (safe).
2. ~~Codex session-kill semantics (latch consult)~~ — resolved 2026-08-18: owner
   says ignore Codex for now; the per-call-deny limit stands as documented.
