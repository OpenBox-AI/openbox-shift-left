# Advice — collapse the enforcement tiers into inline OpenBox evaluation

Date: 2026-08-13 · Driver: **maintenance cost** · Status: advice, not a plan

## Reframed problem

3-tier enforce model costs more to maintain than it returns. Local Go evaluator
duplicates backend OPA semantics (2,198 non-test LOC in `decision/`) under a
**permanent parity obligation** That decision itself admits deviations from;
**cannot evaluate raw rego at all** (`policysync.go:149` → silent fail-open);
drags bundle sync + staleness + signing that backend hasn't finished
(`require_verified_bundle` defaults off).

## Confirmed requirements

1. Every gated PreToolUse call evaluated **inline** by OpenBox; verdict applied.
2. **No local policy evaluation.** Delete evaluator, regoparity, builder translation,
   bundle sync/staleness, `dev sync`.
3. **No tier vocabulary** in code, config, CLI, docs.
4. Unreachable ⇒ org `fail_closed` decides. Default fail-open. Machine-wide, in
   dev.json, hand-editable, org-lockable via managed config. No `init` flag.
5. Slow-but-reachable ⇒ wait for real verdict, bounded ~29s, then `fail_closed`.
6. Telemetry stays spooled/async. Approvals, lineage, usage unchanged.

Goals: delete ~2,200+ LOC + parity tax; one-sentence enforcement story; raw-rego orgs
become enforceable. Non-goals: per-project posture; offline fidelity; latency caching.

## Verdict

**Direction is right and the strategic case is stronger than the LOC case.** Two
implementations of one policy semantics is a correctness liability that grows with
every backend policy feature; raw rego already opens the gate silently. Collapse it.

**But three consequences are unpriced, and one is a privacy regression, not a
performance one.** Ship the direction; do not ship it before pricing these.

## Unpriced consequence 1 — content egress (most important)

Local evaluation can decide **without** egressing content. Inline evaluation cannot.

Today: "Tool commands and file bodies never egress on observe events; an approval
escalation is the one exception and is content-gated" (`CLAUDE.md`). Inline-everything
makes **every gated call** what an escalation is today: tool args — including
Write/Edit **file bodies** — must egress for the server to decide.

Consequences:
- `docs/data-and-privacy.md` needs rewriting, not editing.
- **`content_capture:false` orgs conflict directly**: they opted out of content
  egress, and policy evaluation needs the content. Either they lose enforcement
  fidelity or the opt-out is violated. **Pick explicitly and document it.**
- **Keep `decision/secrets.go`.** Tier-1 local secret detection is *not* policy
  evaluation — it redacts Write/Edit bodies locally so they never leave. Sending a
  body to core to learn it contains a secret defeats the control. Local secret
  redaction must survive the deletion.

## Unpriced consequence 2 — bypass becomes one line

No local evaluation + default fail-open ⇒ `echo "127.0.0.1 core.openbox.ai" >>
/etc/hosts` disables enforcement entirely. Today a cached bundle keeps deciding.

README currently claims local enforcement "prevents mistakes, not motivated bypass".
Post-change the true claim is weaker: *prevents mistakes only while the network
cooperates*. Managed settings + `fail_closed:true` is the only real answer and must
become the documented enterprise posture. Failing to update this claim is exactly the
overstatement `CLAUDE.md` forbids.

## Unpriced consequence 3 — posture evidence disappears

Session posture + `openbox doctor` report `bundle_version`, `bundle_integrity`,
`policy_id`, `bundle_sha256` (`doctor.go:30-47`). No bundle ⇒ those fields vanish ⇒
control plane loses "this endpoint runs policy X at epoch N" — a documented feature
("posture as evidence"), and the point of.

**Replacement required:** `/evaluate` returns policy identity/epoch; session posture
reports the last-seen value. Not optional — without it this is a silent assurance
downgrade.

## Trade-off recorded against a user decision

User chose **wait for real verdict (~29s)** over a tight cap. Honest cost:

- PreToolUse fires on *every* tool call incl. Read/Grep/Glob (dozens per minute).
  Existing escalation budget default is **3,500ms** (`hookflow/tier2.go:23`) — whoever
  chose that did not assume a fast `/evaluate`.
- Arithmetic: 200 calls × 300ms = **+60s wall clock per session**; p99 3s on 2% of
  calls adds ~12s more. Claude Code shows "OpenBox governance…" while it waits, so the
  developer attributes the stall to OpenBox.
- Prediction: this, not correctness, is what gets the tool disabled.
- Hard limit: the wait is bounded by the platform, not preference. Hook killed at 30s
  ⇒ Claude Code proceeds ⇒ **uncontrolled fail-open no `fail_closed` can close**.
  Existing `maxEnforceHookBudget = 30s − 1s` exists for exactly this
  (`enforce_tier2.go:36-42`). Keep that guard.

**Revisit after measurement.** If p95 > ~500ms, a tight cap is the cheaper fix and
costs only the difference between "guessed" and "real" on the slow tail.

## The strategic win, unnamed so far

This makes shift-left **structurally identical to the agent runtime**. `CLAUDE.md`'s
core principle is "reuse, don't rebuild" — same endpoint, same auth, same tables —
and *enforcement is the one place shift-left forked*. Collapsing to inline gives:

- one evaluation path across both halves of the product; one home for policy semantics;
- every backend policy feature works in the developer runtime **immediately**, no
  second implementation, no parity fuzzing;
- the **Cursor adapter (next on the roadmap) gets enforcement for free** — no bundle
  plumbing to port;
- that decision signed-bundle work becomes unnecessary here, deleting a workstream blocked
  on the backend.

Lead that decision with this, not with
LOC.

## Easiest model for developers — three named features, zero numbers

Tier numbers leak implementation into the user's head. Replace with:

| Say this | Was |
|---|---|
| **"OpenBox decides before the tool runs. Can't reach OpenBox? Your org's failure policy applies."** | Tier 1 + Tier 2 |
| **"Secrets in file writes are redacted on your machine, before anything leaves it."** | Tier 1 secret detection |
| **"Findings from OpenBox are surfaced back into your session."** | Tier 3 |

Each is one sentence, states where it happens, and implies its own failure mode.
Also fixes an existing doc bug: `architecture.md:82` says "Within enforce there are
three tiers", but Tier 3 is enforce-independent — `hookrun.go:254`: "Orthogonal to
enforce — findings are advisory feedback in both observe and enforce sessions."

## What not to do

- Don't make non-gating hooks inline. They have **5s** timeouts and no decision to
  make. Only PreToolUse gates. "Every event inline" over-reaches; keep the spool.
- Don't delete the spool/realtime flusher. It works and it's off the hot path.
- Don't delete `decision/secrets.go` (see consequence 1).
- Don't delete `hookflow`'s bounded-run/degrade/approval-hold machinery. It is already
  provider-agnostic and correct; inline-everything **widens its scope**, it doesn't
  replace it. Delete the *narrowing to high-risk classes*, not the mechanism.
- Don't reintroduce a local cache "just for speed" — user explicitly ruled it out.
  Revisit only with measurement in hand.

## Cheaper paths to the same outcome (ranked, effort→impact)

1. **Delete only the parity surface, keep inline scope narrow.** Widen escalation to
   all gated calls but keep the high-risk fast path; deletes the evaluator without
   putting latency on Read/Grep. Rejected by the user (chose every-call inline);
   recorded because it is the cheapest version of the same win.
2. **Policy-declared scope manifest.** Backend serves a few-KB list of tool classes
   its policy governs; client calls inline only for those. Keeps full fidelity, avoids
   wasted round trips, retains a tiny sync. Rejected (no syncing).
3. **Full inline (chosen).** Maximum fidelity and maximum deletion; pays latency on
   every call and egresses content.

## Recommended route

**Phase 0 — measurement gate (do this first, ~half a day).** Against a real stack:
`/evaluate` p50/p95/p99 with realistic payloads (Bash cmd, Write body); tool-calls-per-
session from existing transcripts; compute added wall clock per session. Go/no-go, and
the number that sets the timeout.

**Phase 1.** Inline evaluation as the single decision path. Retire that decision D1+D2
and INV-3b explicitly. Record: content egress change, bypass change, evidence
replacement, and the measured latency.

**Phase 2 — widen, don't rewrite.** Remove the high-risk narrowing so every gated call
escalates; reuse `hookflow`'s existing bounded run, degrade, and approval hold. Keep
`maxEnforceHookBudget` under the platform ceiling.

**Phase 3 — delete.** Evaluator, regoparity, builder, bundle, signature, policysync,
`dev sync`, staleness gates, `require_verified_bundle`, `org_signing_*` pins. **Keep**
`decision/secrets.go`.

**Phase 4 — replace the evidence.** Server-returned policy id/epoch into session
posture and `openbox doctor`.

**Phase 5 — rename + rewrite docs.** Three named features, no tier numbers.
`architecture.md`, `data-and-privacy.md` (content egress), `getting-started.md`,
README (bypass claim).

**Phase 6 — verify against the real thing.** Testbed: verdict applied inline; timeout
⇒ `fail_closed` both ways; host-blocked ⇒ documented behavior; secret redaction still
local; no content egress when `content_capture:false` (or documented otherwise).

## Benefits

- ~2,200+ LOC deleted plus the permanent parity obligation and its fuzz suite.
- Raw-rego orgs become enforceable — closes a live silent-fail-open hole.
- One policy semantics, one evaluation path, shared with the agent runtime.
- Cursor adapter gets enforcement free.
- that decision bundle-signing workstream no longer needed here.
- `REQUIRE_APPROVAL` becomes available for **any** tool, not just shell/MCP.
- Enforcement model explainable in three sentences.

## Trade-offs

- Latency on every tool call, unbounded until measured; user's 29s choice maximizes it.
- Offline/air-gapped sessions ungoverned under the default fail-open.
- Enforcement bypassable by blocking one host, unless managed settings + fail-closed.
- Tool args (incl. file bodies) egress on every gated call — real privacy change.
- Control-plane load multiplies by tool-call volume; needs a capacity answer.
- **Stops being right if** `/evaluate` p95 exceeds ~500ms, or if the org population is
  substantially offline/air-gapped. Cost to switch away then: reintroducing local
  evaluation means restoring the parity tax — so prefer a *coarse* local fallback
  (static high-risk deny list, ~100 LOC) over resurrecting the evaluator.

## Work checklist

- [ ] Measure `/evaluate` p50/p95/p99 against a real stack with realistic payloads
- [ ] Measure tool-calls-per-session from real transcripts; compute added wall clock
- [ ] Go/no-go on inline-everything using those numbers; set the timeout from data
- [ ] Decide the `content_capture:false` × enforcement conflict, explicitly
- [ ] Write; retire that decision D1+D2 and INV-3b by name
- [ ] Widen escalation to all gated tool classes, reusing `hookflow` machinery
- [ ] Keep `maxEnforceHookBudget` strictly under the platform hook ceiling
- [ ] Add server-returned policy id/epoch to session posture + `openbox doctor`
- [ ] Delete evaluator, regoparity, builder, bundle, signature, policysync, `dev sync`,
      staleness, `require_verified_bundle`, `org_signing_*`
- [ ] Preserve `decision/secrets.go` and its local-redaction tests
- [ ] Remove tier vocabulary from code, config, CLI help, docs
- [ ] Rewrite `data-and-privacy.md` for content egress; fix README's bypass claim
- [ ] Fix `architecture.md:82` (findings are enforce-independent)
- [ ] Testbed: inline verdict, timeout→fail_closed both ways, host-blocked, redaction
- [ ] Verify Codex's hook timeout ceiling separately from Claude Code's 30s

## Success metrics

| Metric | Target |
|---|---|
| Non-test LOC deleted from `decision/` + `policysync` | ≥ 2,000 |
| `grep -ric "tier" --include=*.go --include=*.md` outside decision record history | 0 |
| Added p95 wall clock per tool call | < 200ms (set real target from Phase 0) |
| Added wall clock per session | < 10s at p95 tool-call volume |
| Raw-rego org: gated call correctly denied | passes (today: fails open) |
| Hook exceeding platform ceiling | never — pinned by test |
| Session posture carries policy identity | 100% of sessions with a reachable core |
| Local secret redaction still applies with core unreachable | yes |
| Write body egress when `content_capture:false` | 0 bytes, or documented + decision record'd |

## Unresolved

1. `content_capture:false` × inline enforcement — cannot have both; needs a decision.
2. `/evaluate` latency and rate limits undocumented (docs.openbox.ai publishes none)
   and unmeasured in this repo. Everything above is gated on Phase 0.
3. Does `/evaluate` return policy identity/epoch today? If not, it is a backend ask.
4. Codex hook timeout ceiling unknown; Claude Code's is 30s (PreToolUse) / 5s (rest).
5. Control-plane capacity at tool-call volume — backend owners must confirm.
