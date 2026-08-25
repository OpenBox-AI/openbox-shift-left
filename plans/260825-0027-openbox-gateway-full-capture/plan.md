---
title: "Full I/O capture — tool content, thinking, and the LOCAL OpenBox gateway"
description: "Capture every input and output a governed session produces — incl. model request/response headers and bodies via a per-developer local gateway — and evaluate synchronously; org-account binding enforced by core policy."
status: pending
priority: P1
effort: 23d
branch: feat/tool-content-capture
tags: [gateway, local-gateway, content-capture, enforcement, adr-0019, account-binding, telemetry]
created: 2026-08-25
updated: 2026-08-25
---

# Full I/O capture (local-gateway revision)

Capture all inputs and outputs of a governed developer session — tool I/O, thinking, and
full model request/response incl. HTTP headers — and send them to core for evaluation.
The gateway runs **on the developer machine**, installed by `openbox` tooling; no
org-operated service exists in the base architecture.

Design and evidence:
[advise-260824-1841](../reports/advise-260824-1841-full-io-capture-gateway.md) (capture +
enforcement design; its hosting posture is superseded) →
[advise-260825-0236](../reports/advise-260825-0236-local-gateway-detection-tier.md)
(local-first, detection-tier, account binding — the confirmed reframing this revision
implements). Plan-vs-code audit:
[plan review](visuals/plan-review-gateway-full-capture.html) (28 claims, 24 verified).
Surface research: [`research/`](research/).

## Shape

Two tracks. **Track A (phases 01–02)** closes content gaps on the existing hook pipeline —
no new service, ships independently. **Track B (phases 03–08)** builds the **local**
OpenBox gateway: the only route to model-call headers, synchronous refusal of model calls,
and account-binding evidence. Track A must not wait for Track B.

## Decided, do not re-litigate

- **Local-first hosting (product decision, 2026-08-25).** Target customers cannot run
  services. The gateway is a per-developer localhost daemon; the listener stays
  address-configurable (central deployment remains possible later) but no central
  deployment work — custody, KMS, HA, fleet runbooks — is built now.
- **Base assurance claim is DETECTION, not prevention.** Bypass is visible and attributable
  (cross-channel mismatch + doctor); prevention belongs to the org's MDM and is out of
  OpenBox scope. OpenBox ships MDM-pushable artifacts, never MDM tooling. Docs must never
  say "cannot bypass".
- **Pass-through auth.** The developer's own credential relays untouched; OpenBox holds
  zero provider secrets. The obx_→credential-swap design is deleted, not optional.
- **Account binding is core policy, not gateway logic** (ADR-0017 dogma). Sensors attach
  evidence — credential fingerprint + local account metadata — and `/evaluate` returns
  HALT/BLOCK on non-org accounts. No local allowlists, no local verdict caching.
- **No MITM proxy.** Unchanged: costs a CA that can forge any domain, buys no assurance.
- **Gateway substitutes, not chains.** `Claude Code → local gateway → provider`.
- **Inspect without modifying.** Forwarded bytes byte-identical — Authorization header
  included. Redaction applies to the captured copy only.
- **Availability inversion is dissolved, not accepted.** A dead local gateway blocks one
  developer and fails closed by accident (dead localhost); launchd/systemd restarts it.
- **Codex is deferred, not covered** (owner decision, 2026-08-25): this plan governs Claude
  Code only. Codex gateway coverage (`OPENAI_BASE_URL` route, doc-tier) is revisited only
  after the Claude Code gateway works end to end. Not part of this plan.

## Phases

| # | Phase | Status | Effort | Depends on |
|---|---|---|---|---|
| 01 | [Tool content capture](phase-01-tool-content-capture.md) | **implemented** (testbed dormant) | 2d | — |
| 02 | [Thinking capture](phase-02-thinking-capture.md) | **implemented** (testbed dormant) | 3d | 01 (shares gate plumbing) |
| 03 | [Decisions, ADRs, probes](phase-03-decisions-and-adrs.md) | **prepared** — probe runs + 2 sign-offs are user actions | 2d | — |
| 04 | [Gateway passthrough core](phase-04-gateway-passthrough-core.md) | pending | 4d | 03 |
| 05 | [Capture, identity, account evidence](phase-05-gateway-capture-pipeline.md) | pending | 4d | 04 |
| 06 | [Gateway enforcement](phase-06-gateway-enforcement.md) | pending | 3d | 05 |
| 07 | [Local daemon + MDM enablement](phase-07-distribution-assurance.md) | pending | 2d | 06 |
| 08 | [Conformance and testbed](phase-08-conformance-testbed.md) | pending | 3d | 07 |

## Acceptance criteria

1. Tool and MCP output present on every `ActivityCompleted` with capture ON.
2. Thinking blocks captured per turn where extended thinking is active.
3. Model request/response headers and bodies reach core as `SpanData`, from a gateway
   running on the developer machine with no org service anywhere.
4. A policy verdict refuses a model call before it leaves the machine; the session does
   not retry around it.
5. A core HALT on a non-org credential fingerprint stops a real model call (account
   binding end to end).
6. The raw provider credential appears in zero outbound bytes; its fingerprint is present
   on gateway spans. Conformance-asserted.
7. Bypass is visible: a session with turns but no gateway spans is detectable in stored
   data, and `openbox doctor` flags a bypass-capable configuration.
8. Forwarded bytes byte-identical to received; zero stream aborts attributable to the
   gateway across a full testbed run.

## Blocking unknowns

Resolve in phase 03 before phase 04 starts:

- **P0 — BASE_URL under subscription OAuth.** Does `ANTHROPIC_BASE_URL` carry
  claude.ai-subscription traffic, or only API-key mode? Negative for OAuth ⇒ pass-through
  covers API-key orgs only; product must say so. (Afflicted the central design too.)
- **Probe A — HALT rendering.** Unchanged: the refusal shape must not trip Claude Code's
  capability-rejection retry (matches on upstream error wording).
- **P1 — org-id matchability.** Can the gateway (or core) match an org id from the OAuth
  token or response headers? Decides OAuth account rule: refusal vs detection-only.
- **Probe B — reframed, non-blocking.** Managed-settings/MDM capability now selects the
  org's assurance TIER (base = tamper-evident ships regardless); it no longer kills Track B.

## Validation Summary

**Validated:** 2026-08-25 · **Questions asked:** 4

### Confirmed decisions

- **Gateway fail posture (OD, owner-chosen): ALWAYS REFUSE.** A gated model call is refused
  when `/evaluate` is unreachable — regardless of the `fail_closed` key. Deliberate
  divergence from the hook path (which keeps posture-driven behavior): the gateway is the
  stronger enforcement point by owner intent. Cost accepted: a core outage refuses gated
  model calls for every governed developer (ungated calls still forward with no round-trip).
  The pre-ADR-0017 ordering lesson gets sharper: no synthesized refusal may fire before an
  evaluation attempt — phase 06's ordering test is the control.
- **Account evidence scope: org UUID + email.** Email is PII, egressed as governance
  evidence (like DID), documented in `docs/data-and-privacy.md`.
- **Body sink: deferred to phase 08 evidence.** Phase 05 ships cap-only (64KB); the sink is
  built only if measured volume demands it.
- **Sequencing: Track A (phase 01) and phase 03 probes run in parallel.**

### Action items — applied 2026-08-25

- [x] Phase 06: requirement 4 rewritten — always refuse on unreachable `/evaluate`;
  ordering test promoted to merge-blocker.
- [x] Phase 03: requirement 7 recorded (refuse, no offline grace); ADR-0021 must state the
  hook/gateway posture divergence and its cost.
- [x] Phase 05: sink/`body_ref` items moved out (cap-only v1); phase 08 holds the
  contingency.
- [x] Phase 05 docs step now carries the `docs/data-and-privacy.md` account rows: email
  (PII) + fingerprint egress; `organizationName`/`organizationRole` explicitly excluded —
  evidence is org UUID + email, nothing more.
- [x] Codex deferral recorded as a decided non-goal (owner, 2026-08-25).
