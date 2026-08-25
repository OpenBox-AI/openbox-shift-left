# Phase 03 — Decisions, ADRs, and probes

## Context links

- Parent: [plan.md](plan.md)
- Design: [advise-260825-0236](../reports/advise-260825-0236-local-gateway-detection-tier.md)
  (supersedes the hosting posture of [advise-260824-1841](../reports/advise-260824-1841-full-io-capture-gateway.md))
- Repo rule: `CLAUDE.md` — "A new table, endpoint or service requires an ADR in `docs/adr/`"
- Gate for: phases 04–08. **Nothing in Track B starts until this closes.**

## Overview

- Date: 2026-08-25 (local-gateway revision)
- Description: accept ADR-0019, write the gateway ADR for the LOCAL topology, amend
  ADR-0016's install scope, run the three probes that shape Track B, and open the
  account-registry conversation with the backend.
- Priority: P1
- Implementation status: **partially prepared** — every artifact an unattended
  session can produce is written; the three probe RUNS, ADR-0019's acceptance,
  and the backend filing are user actions (see Todo)
- Review status: not reviewed

## Key insights

- **Three probes are load-bearing and cheap.** All are throwaway-server or inspection work,
  no gateway code. P0 decides who pass-through covers; probe A decides how refusal renders;
  P1 decides whether the OAuth account rule can refuse or only detect.
- **Probe B stopped being a kill switch.** Under local-first + detection-tier, managed
  settings/MDM select which assurance TIER an org reaches; the base tier ships regardless.
  The old fallback ("no managed settings ⇒ stop Track B") is retired with the custody design.
- **The install-scope conflict is still real.** ADR-0016 defaults `init` to project scope;
  `ANTHROPIC_BASE_URL` is read only from managed settings and `~/.claude/settings.json`
  where the Desktop app manages the connection, and background agents need settings rather
  than shell exports. Amending ADR-0016 remains a prerequisite for phase 07 — the value now
  points at localhost instead of an org host.
- **The assurance story must be written as tiers, in the ADR, before code.** Base =
  tamper-evident (detection + attribution). MDM tier = root-owned daemon + managed config +
  optional egress control, org-operated. Central custody = a possible future deployment of
  the same binary, not built now. An ADR that blurs these into one claim repeats the
  overstatement failure this product exists to prevent.
- **Account binding starts here as a backend conversation.** Core needs an account registry
  (org key fingerprints, allowed org UUIDs/domains) as policy input. That may be a new
  surface on their side — their ADR rule, their call; this phase files the ask with the
  evidence contract (what sensors will send).

## Requirements

1. ADR-0019 moved Proposed → Accepted (P1/P3 land in phases 01–02).
2. New ADR-0021: OpenBox gateway as a **local per-developer service** — substitution
   topology, pass-through auth (zero provider secrets held), inspect-without-modifying,
   tiered assurance (base/MDM/central-if-ever), hosting-agnostic listener, availability
   posture (per-dev blast radius, fail-closed-by-accident), account-evidence contract.
3. ADR-0016 amendment: gateway env config installs at user or managed scope.
4. **Probe A — HALT rendering.** Status code + body that refuses a call without tripping
   capability-rejection retry. Unchanged from the original plan.
5. **P0 — BASE_URL auth coverage.** Throwaway localhost server as `ANTHROPIC_BASE_URL`;
   test subscription-OAuth AND API-key sessions; record which traffic redirects and that
   the Authorization header arrives verbatim.
6. **P1 — org-id matchability.** Inspect the OAuth bearer and response headers for a
   matchable org identifier. Also verify the shape of Claude Code's local account state
   (email / org UUID) on this machine.
7. Fail posture: **decided** (validation, 2026-08-25) — always refuse gated calls when
   `/evaluate` is unreachable, regardless of `fail_closed`; no offline grace. ADR-0021
   records the divergence from hook-path posture and its cost.
8. Backend ask filed: account registry as policy input + evidence contract.

## Architecture

No production code. Three probes against the real installed binary, three ADR documents,
one backend ask. Probes A and P0 share the same throwaway HTTP server; P1 is token/header
inspection plus a local-state read.

## Related code files

| Path | Change |
|---|---|
| `docs/adr/ADR-0019-full-content-capture.md` | status → Accepted |
| `docs/adr/ADR-0021-openbox-local-gateway.md` | new — local topology, tiers, pass-through (**drafted**; `local` is in the filename because it is the decision) |
| `docs/adr/ADR-0016-default-install-posture.md` | amendment: scope for gateway env config |
| `plans/reports/probe-260825-halt-rendering.md` | probe A output (**template**) |
| `plans/reports/probe-260825-baseurl-auth-coverage.md` | P0 + P1 output (**template**) |
| `plans/260825-0027-openbox-gateway-full-capture/probes/` | the probe server + runbook (**written**) |
| `CLAUDE.md` | current-state paragraph after acceptance |

## Implementation steps

1. Run P0 first (it reuses probe A's server): which auth modes follow `ANTHROPIC_BASE_URL`,
   and does Authorization pass verbatim. Record per-mode.
2. Run probe A: candidate refusal shapes (`403` + Anthropic-shaped error, `400`, custom
   body); observe retry / capability-disable / surfaced-message behavior.
3. Run P1: OAuth token + response-header inspection; local account-state shape check.
4. Accept ADR-0019.
5. Write ADR-0021 per requirement 2, including what the gateway explicitly cannot do:
   prevention without MDM, redaction at source, CI coverage, non-Anthropic formats in v1,
   and Codex sessions (deferred by owner decision until the Claude Code gateway is proven).
6. Amend ADR-0016.
7. Record the validated always-refuse posture in ADR-0021 — the hook/gateway divergence,
   its cost, and the explicit no-offline-grace statement.
8. File the account-registry backend ask with the evidence contract from
   [phase-05](phase-05-gateway-capture-pipeline.md).

## Todo

Prepared 2026-08-25 — the harness, the templates and the drafts exist, so what is
left is the part that needs a human or a credential.

- [x] Probe harness written and smoke-tested: [`probes/probe-server.go`](probes/probe-server.go)
      (stdlib-only, serves P0 + P1 + probe A; reduces every credential to
      `(kind, length, sha256[:8])` **in code**, because these reports are committed)
- [x] [`probes/RUNBOOK.md`](probes/RUNBOOK.md) — per-mode steps, what to record, teardown
- [x] Report templates: `plans/reports/probe-260825-baseurl-auth-coverage.md`,
      `plans/reports/probe-260825-halt-rendering.md`
- [x] `docs/adr/ADR-0021-openbox-local-gateway.md` **drafted** — the tiers are
      explicit and the always-refuse posture (§7) carries the divergence, the cost
      and the no-offline-grace statement. Three `TBD(probe)` slots (§§8–10) are the
      probe answers and **must block acceptance**
- [x] `docs/adr/ADR-0016` amendment **drafted** (user/managed scope for the gateway
      env config; probe-independent, so written in full)
- [x] Backend ask **drafted**: `plans/reports/backend-ask-260825-account-registry.md`
- [ ] **USER: run P0** — per auth mode, does `ANTHROPIC_BASE_URL` redirect, and does
      the credential arrive verbatim
- [ ] **USER: run probe A** — name a refusal shape that does not trip
      capability-rejection retry
- [~] **P1, split.** §3 (local account state) **RUN 2026-08-25** by an agent — it needs no
      credential, no network and no quota, so its blast radius is a file read. Result:
      `oauthAccount.organizationUuid` + `oauthAccount.emailAddress` are strings at a stable
      path, which unblocks phase 05's account evidence. §1 (org id from the credential)
      **remains USER** — it needs the OAuth session to reach the probe server, i.e. P0 1b.
      §2 (response headers) is unresolvable from a throwaway server by construction.
- [ ] **USER: accept ADR-0019** (owner signature — the ADR's own Acceptance section
      says it stays Proposed until the owner accepts; the 2026-08-25 plan validation
      pre-authorised the substance, but the flip is not an agent's to make)
- [ ] **USER: file the backend ask** (outward-facing, cross-repo)
- [ ] Fill ADR-0021 §§8–10 from the probe reports, then review it (tier split is the
      decision — do not merge if the tiers read as one claim)
- [ ] CLAUDE.md current-state updated once ADR-0021 is accepted

## Success criteria

- A named, evidence-backed refusal shape for HALT that does not trigger retry.
- A per-auth-mode yes/no on BASE_URL coverage, from the installed binary — not docs.
- A yes/no on OAuth org-id matchability, with the fallback (detection-only) named if no.
- ADR-0021 states the three assurance tiers in plain language and never claims prevention
  for the base tier.
- The backend ask contains the exact evidence fields sensors will send.

## Risk assessment

| Risk | Mitigation | Signal it broke | Response |
|---|---|---|---|
| P0 negative for OAuth | pass-through still covers API-key orgs | subscription session ignores BASE_URL | scope statement in ADR-0021 + product docs: gateway tier requires API-key/console auth; Track B proceeds |
| No clean HALT shape exists | probe A tries several | every candidate triggers retry or corrupts the session | descope phase 06 to observe-only; keep prevention in the hooks |
| P1 finds nothing matchable | account rule has a detection floor | no org id in token or headers | OAuth account rule ships detection-only; API-key fingerprints still refuse |
| ADR-0021 blurs the tiers | reviewer checklist names all three | ADR reads as one assurance claim | do not merge; the tier split IS the decision |
| Backend rejects the registry ask | evidence contract is sensor-side regardless | ask stalls | ship fingerprint/metadata capture anyway (phase 05); policy matching follows when core lands it |

## Security considerations

- ADR-0021 must state plainly that with pass-through auth **OpenBox holds no provider
  credential anywhere** — and that this is why the custody claim is gone. The fingerprint
  is one-way; the ADR records the hash construction and that raw credentials never egress.
- The probes use real credentials against a throwaway local server; probe reports must not
  contain tokens, only behaviors.

## Next steps

On close, phase 04 begins against the recorded refusal shape and auth coverage.
