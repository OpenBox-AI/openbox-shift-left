---
title: "Inline policy evaluation — one decision path, no tiers"
description: "OpenBox becomes the single decision authority: every gated tool call is evaluated inline. Deletes the local policy evaluator, bundle sync, staleness and signing; keeps local secret redaction."
status: complete — testbed run outstanding
priority: P1
effort: 28h
branch: feat/inline-policy-evaluation — branched off feat/dev-runtime-auth-and-init, must land after it
tags: [enforcement, policy, adr, breaking-change, deletion, privacy]
created: 2026-08-13
advice: plans/reports/advise-260813-0103-inline-evaluation-vs-tiers.md, plans/reports/advise-260813-0140-unresolved-inline-evaluation.md
---

# Inline policy evaluation

`/evaluate` becomes the only decider. The local Go policy evaluator, the bundle it
consumed, and the sync/staleness/signing machinery around it are deleted. Local
**secret redaction** stays — it is content protection, not policy evaluation.

## Why (the argument that carries this, in order)

1. **One evaluation path across the product.** The repo's stated principle is "reuse,
   don't rebuild" — same endpoint, same auth, same tables — and enforcement is the one
   place the developer runtime **forked**, reimplementing the backend's OPA semantics in
   Go. Collapsing it means one home for policy semantics, and every backend policy
   feature works here immediately.
2. **The parity obligation is permanent and already leaking.** ADR-0005 names it: "the
   local evaluator must agree with what the backend's OPA would decide," with known
   deviations "documented rather than assumed away."
3. **Raw rego is a live silent-fail-open hole.** `policysync.go:149` — hand-written rego
   "cannot be evaluated locally", so those orgs' gates simply open.
4. **The Cursor adapter gets enforcement free** — no bundle plumbing to port.
5. **ADR-0008's bundle signing becomes unnecessary here**, deleting a workstream blocked
   on the backend (`require_verified_bundle` still defaults off because nothing signs).
6. Deletion of ~2,200 non-test LOC is the *consequence*, not the argument.

## Decisions already taken (from the advisory rounds)

| # | Decision |
|---|---|
| E1 | Every gated PreToolUse call is evaluated inline by OpenBox; the verdict is applied. |
| E2 | No local policy evaluation. Evaluator, regoparity, builder, bundle, signature, policysync, `dev sync`, staleness all deleted. |
| E3 | No tier vocabulary in code, config, CLI or docs. |
| E4 | Unreachable ⇒ the org's `fail_closed` decides. Default fail-open. Machine-wide, hand-editable in `dev.json`, org-lockable via managed config. No `init` flag. |
| E5 | Slow-but-reachable ⇒ wait for the real verdict, bounded by the provider's hook ceiling, then apply `fail_closed`. |
| E6 | **Latency and capacity are the platform's scope.** No client-side caps tuned here, no caching, no measurement gate. |
| E7 | Content attaches for **all** gated classes (content_capture-gated as today) — Write/Edit bodies begin egressing. |
| E8 | **Local secret redaction runs first; core receives the redacted body.** |
| E9 | Telemetry stays spooled/async. Approvals, lineage, usage unchanged. |

## Phases

| # | Phase | Status | Effort | Depends on |
|---|---|---|---|---|
| 1 | [Gate: dedupe under universal escalation + Codex ceiling](phase-01-gate-dedupe-and-ceilings.md) | done — stack run waived | 3h | — |
| 2 | [ADR-0017 + honest docs ahead of the code](phase-02-adr-and-docs.md) | done | 3h | 1 |
| 3 | [Hook-ceiling capability in the SPI; widen the gate to all classes](phase-03-ceiling-spi-and-widen-gate.md) | done | 5h | 2 |
| 4 | [Content: redact locally, then send](phase-04-redact-then-send.md) | done | 3h | 3 |
| 5 | [Evidence: verdict `policy_id` into posture](phase-05-policy-identity-evidence.md) | done | 2h | 3 |
| 6 | [Delete the local policy path](phase-06-delete-local-policy.md) | done | 5h | 4, 5 |
| 7 | [Rename to three named features; rewrite the privacy docs](phase-07-rename-and-docs-rewrite.md) | done | 4h | 6 |
| 8 | [Verify against the real thing](phase-08-verification.md) | done — stack run waived | 3h | 7 |

Phase 1 is a **blocking investigation** — it can invalidate the approach, so it runs
before the ADR. 4 ‖ 5 after 3. Everything else is sequential.

## File ownership

| Phase | Owns |
|---|---|
| 1 | nothing — investigation; writes `reports/` only |
| 2 | `docs/adr/ADR-0017-*.md`, `docs/adr/README.md` |
| 3 | `provider/provider.go`, `adapters/{claude-code,codex}/capabilities.go`, `adapters/common/hookflow/{gate.go,tier2.go→evaluate.go}` |
| 4 | `adapters/common/hookflow/gate.go` (content path), `client/payload.go` |
| 5 | `adapters/common/devconfig/posture.go`, `cli/cmd/openbox/doctor.go` |
| 6 | `decision/*` (except `secrets.go`), `cli/internal/policysync/*`, `cli/cmd/openbox/main.go` (`dev sync`), `adapters/common/hookflow/staleness.go`, `adapters/common/devconfig/devconfig.go` (bundle fields) |
| 7 | `docs/architecture.md`, `docs/data-and-privacy.md`, `docs/getting-started.md`, `README.md`, CLI help strings |
| 8 | `testbed/*`, `.github/workflows/ci.yml` |

## Acceptance (whole plan)

- A gated call is decided by `/evaluate`; no local policy evaluation happens.
- A **raw-rego** org's gated call is correctly denied — today it fails open.
- Core unreachable ⇒ the org's `fail_closed` applies, both branches asserted.
- The hook **always** writes a verdict before the provider's ceiling; a killed hook is
  impossible by construction, pinned per adapter.
- A known secret in a Write body **never** appears in the outbound payload.
- Local secret redaction still applies with core unreachable.
- Exactly one `ActivityStarted` per gated call, with every class escalating.
- Session posture carries the deciding `policy_id`.
- `grep -ric "tier"` over `*.go`/`*.md` outside ADR history returns 0.
- All 11 modules: build, vet, `-race` green ✅ (+ Windows and linux/arm64 cross-compile). Testbed against a live stack: **NOT RUN** — waived by the operator; see reports/verification-260813-inline-evaluation.md.

## Out of scope

- **Latency: caps, caching, decision reuse, measurement gates** (E6 — platform's scope).
- Per-project posture; the Cursor adapter (benefits, but separate work).
- Server-side changes. If a policy *version* is needed beyond `policy_id`, that is a
  backend ask filed from phase 5, not work done here.
- Raising the 64KB content cap for the evaluation path (phase 2 discloses it; changing it
  needs its own decision).

## Relationship to the auth/init plan

`plans/260812-1212-openbox-auth-command/` is in flight on
`feat/dev-runtime-auth-and-init` and **must land first** — both plans edit
`devconfig.go` heavily, and this one deletes fields the other one is still moving. Five
collision points are already patched in that plan; see its Validation Summary.

## Open questions

0. **The delivery-flag data race is fixed** (`eb53827`, standalone — it was live on
   Bash/MCP, not caused by this plan). **The double-store is only narrowed:** the lost-200
   window survives and is irreducible client-side while core does not dedupe developer
   events. So **phase 3 cannot assume universal escalation is duplicate-free**, and closing
   it properly is a backend ask. See
   [phase 1's finding](reports/finding-260813-dedupe-and-ceilings.md).
   *Question 2 below is answered by the same finding and is retained for history.*
1. **RESOLVED (phase 5): no backend ask.** Policy provenance belongs to whoever decides,
   and that is the control plane now — it already holds the identity of the policy it
   applied, so having the endpoint report it back would be one party attesting to
   another's record. Posture carries `decision_authority` + `failure_policy` instead. See
   ADR-0017 §Policy provenance as evidence.
2. **Codex's hook ceiling is unverified.** Phase 1 must read it; Codex enforcement is
   blocked until it is known.
3. **Notification obligation** for existing `content_capture:true` orgs whose file bodies
   begin egressing (E7) — product/legal call, not engineering. Phase 7 writes the release
   note; who must be told is not ours to decide.
4. **Is 64KB the right cap for content the server evaluates?** `capBody` truncates at
   `maxBodySize = 65536`, so content matching sees at most the first 64KB of a large
   write.
