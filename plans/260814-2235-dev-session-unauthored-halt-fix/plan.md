---
title: "Dev-session unauthored-HALT: cause-aligned core fix"
description: "Stop openbox-core rejecting and latching developer-session events on session status, so an unauthored HALT can never brick a live dev session."
status: pending
priority: P1
effort: 11h
branch: main
tags: [governance, openbox-core, enforcement, dev-sessions, audit]
created: 2026-08-14
---

# Dev-session unauthored-HALT — cause-aligned fix

## Objective

A `kind=developer` session must (a) never have an event rejected for session-status reasons —
events store and evaluate normally; (b) never latch on a HALT — a verdict applies to one call.
Agent-runtime (non-developer) semantics unchanged, pinned by tests.

**CORE-ONLY.** Phases 02/04 planned here, executed in `../openbox-core`. Client verdict handling
untouched — no discrimination, no `fallback_used` wiring, no failure-policy change. ADR-0017's
trust boundary holds: **truthfulness is the decider's obligation.**

## Evidence

- Diagnosis (mechanism, blast radius): [debug-260814-1231](../reports/debug-260814-1231-session-no-longer-active-halt.md)
- Core surface (blocks, latch, attested split, test conventions): [researcher-01](research/researcher-01-core-governance-surface.md)
- Audit record (ts/reason mechanics, comment-vs-test tension): [researcher-02](research/researcher-02-shiftleft-audit-record.md)
- Discriminator chain, VERIFIED end to end: [scout-01](research/scout-01-agent-type-chain.md)

Discriminator: `agent.AgentType != nil && *agent.AgentType == "developer"` — server-derived, in
scope at both blocks (`governance_workflow.go:153-157`, `content/agent.go:21`). Nil/other ⇒
agent-runtime semantics; never loosens runtime governance.

## Phases

| # | Phase | Effort | Status | Blocks on | Repo |
|---|---|---|---|---|---|
| 1 | [Trigger investigation](phase-01-trigger-investigation.md) | 1.5h | pending | — | DB + shift-left |
| 2 | [Core fix + tests](phase-02-core-dev-session-fix.md) | 5h | pending | — | **openbox-core** |
| 3 | [Enforcement record ts + reason](phase-03-enforcement-record-ts-reason.md) | 1.5h | pending | — | shift-left |
| 4 | [Deploy + live replay verification](phase-04-deploy-and-live-replay.md) | 2h | pending | 2 | **openbox-core** + stack |
| 5 | [Docs version-scoping](phase-05-docs-version-scoping.md) | 1h | pending | 4 | shift-left |

Progress: 0/5. Phases 1, 2, 3 are parallel-safe (disjoint repos/files).

## Open decisions

- **Post-attestation dev events → store-and-mark-late, NEVER silent-drop** (user decision). Store
  the event, mark it outside the sealed set, skip the attestation start for it — leaves are appended
  only by `AttestationWorkflow` (`openbox-core internal/services/attestation_workflow.go:159-171`)
  and `FinalizeSessionActivity` recomputes the root over ALL session leaves
  (`activities/attestation/finalize.go:19-20`), so an appended leaf is the only thing that could
  break a signed root. If the two prove undecouplable → **STOP that sub-step, surface to owner**;
  fallback is deny-with-truthful-non-verdict-error (phase 02, R-A).
- **No CLAUDE.md note** (user decision). Docs trim in phase 05, after deploy + verify.

## Success criteria

1. 0 developer-session events rejected for session-status reasons (Blocks 1 and 2, halted half).
2. 0 HALT verdicts with empty `policy_id` observed during the phase-04 live replay.
3. 0 developer sessions flipped to `halted` by a verdict (`UpdateSessionHaltedActivity` not called).
4. openbox-core suite green, including 3 new invariant pins and first-ever Block-2 coverage.
5. Agent-runtime governance tests byte-identical — `TestGovernanceEventWorkflow_HaltedSession`
   (`governance_workflow_test.go:1347`) and `_CompletedSession` (`:1406`) keep asserting HALT.
6. Every `enforcements.jsonl` record carries a `ts` parseable as RFC3339Nano and recent, plus the
   policy-authored `reason`; guardrail free text and tool content still absent.
7. Replay: after a terminal session row is forced mid-session, the next gated call proceeds, its
   event is stored, `advisories.jsonl` shows no empty-policy HALT, session not flipped to halted.
8. The three shift-left caveat blocks cite the fixing core commit sha and are version-scoped.
9. No reachable stack ⇒ phase 04 parks **"implemented, unverified"**, stated in plan status.

## Validation Summary

**Validated:** 2026-08-17 · **Questions asked:** 4 (interview) + 5 (advisory, 2026-08-14)

### Confirmed Decisions
- **R-A stays a hard stop**: if the Merkle leaf-write proves coupled to event storage, phase 02
  stops that sub-step and surfaces; the truthful-error fallback is NOT pre-authorized.
- **Deploy is owner-performed**: phase 04 hands over the merge sha and pauses; verification resumes
  against the running service after the out-of-band staging deploy.
- **PROD ticket filed at phase-02 start** by the implementing session via the connected Atlassian
  tools; its number prefixes the commit (PROD-314 flow).
- **Docs caveats committed before implementation** to pin phase-05's line citations (done — see
  Action Items).

### Action Items
- [x] Commit the three uncommitted docs caveat files (README.md, docs/architecture.md,
  docs/getting-started.md) — pins phase-05 citations.
- [ ] Phase-02 kickoff files the Jira ticket first.
- [ ] Phase-04 kickoff begins with the sha handover for the owner deploy.

## Unresolved questions

1. INV glossary has no single definition site — meaning inferred from repeated inline comments
   (researcher-02 Q1). Not blocking; phase 03 amends the comment it contradicts.
2. Attested store-and-mark-late feasibility rests on a Merkle read phase 02 confirms at
   implementation time (researcher-01 Q4); break signal + fallback = phase-02 R-A.
3. Dev agents registered before `openbox auth` have NULL `agent_type`, keep old behavior; remedy
   (re-register / backend update) out of scope.
