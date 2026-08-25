# Advise — local gateway, detection-tier assurance, account-binding via core policy

Date: 2026-08-25. Advisory only. `--yagni` applied. Supersedes the hosting posture of
[advise-260824-1841](advise-260824-1841-full-io-capture-gateway.md); its capture/enforcement
design survives. Interview: 5 questions, reframing confirmed.

## Confirmed reframing

**Problem.** Track B of plan 260825-0027 assumes an org-operated gateway holding an org
provider credential — a service the product's target customers can't/won't run. Re-architect
as a per-developer LOCAL gateway (same machine as Claude Code + hooks), pass-through auth,
full capture + synchronous refusal preserved. Tamper-resistance re-scoped: OpenBox base claim
= detection/attribution (tamper-evident); prevention = org's MDM, out of OpenBox scope.
New requirement: org-account-only usage enforced as CORE POLICY from sensor evidence.

**Requirements.**
1. Gateway on the dev machine (localhost listener, daemon), installed by `openbox` tooling; no org service in base architecture.
2. `ANTHROPIC_BASE_URL` → local gateway; headers+bodies to core as SpanData; phases 04–06 semantics unchanged.
3. Synchronous refusal stays: core verdict pre-forward; HALT/BLOCK renders as refusal.
4. Pass-through auth: developer's own credential relays untouched; OpenBox holds zero provider secrets.
5. Account evidence on the wire: credential fingerprint (one-way hash, computed at capture BEFORE header redaction; raw credential never egresses) + local account metadata (email/org UUID from Claude Code local state).
6. Org-account-only is core policy, not gateway logic: core matches evidence → HALT/BLOCK → gateway refuses / session halts (ADR-0020 semantics). No local allowlists (ADR-0017 dogma).
7. Base claim = tamper-EVIDENT: hook-events-without-gateway-spans mismatch signal; doctor checks (liveness, base-url target, config ownership). Docs never claim prevention.
8. MDM enablement, not ownership: root-ownable daemon, managed-settings template, pushable package; operating MDM/egress = org-side.
9. Hosting-agnostic listener retained; no central deployment work built now (YAGNI).
10. Track A (01–02) unchanged, independent.

**Goals.** Zero-infra adoption; same capture surface as original Track B; account policy from
platform; blast radius of a gateway failure = one developer.

**Non-goals.** Credential custody; OpenBox-owned prevention; CI governance; non-Anthropic
formats; vendor-hosted gateway.

**YAGNI cuts.** obx_→DID auth at gateway; `ANTHROPIC_AUTH_TOKEN` distribution; org-credential
custody+KMS; HA/pager/staged fleet cohort; `forceLoginMethod` as requirement (→ optional
org-side hardening).

## Verdict

**Sound pivot, and cheaper than what it replaces — it deletes more than it adds.** It does NOT
"change the plan completely": phases 04–06 are hosting-agnostic (verified against the phase
files — nothing in passthrough/capture/enforcement cares where the process listens). The change
rewrites phase 07, shrinks phase 03's kill condition, deletes the custody machinery, and adds
account-evidence work. ~70–80% of Track B survives verbatim. Two conditions: P0 probe
(BASE_URL under subscription OAuth) must pass for pass-through to cover subscription users —
note this risk existed for the CENTRAL design too — and the docs must hold the
detection-not-prevention line without slippage.

The original design's own fallback logic endorses this: advise-260824-1841 said no managed
settings ⇒ gateway "degrades to an expensive observe-only proxy". Local-first turns that cliff
into a gradient: base tier ships everywhere; MDM orgs climb to near-prevention; the
hosting-agnostic listener keeps true custody reachable if a customer ever demands it.

## What you should do (ordered)

1. **P0 probe first, before touching the plan**: throwaway localhost server as
   `ANTHROPIC_BASE_URL`; test subscription-OAuth AND API-key auth modes; record which traffic
   redirects and whether Authorization survives verbatim. Hours of work; decides pass-through
   viability for the biggest user population.
2. **P1 probe**: is an org id matchable from the OAuth token or response headers? Decides
   whether OAuth-mode account rule is refusal or detection.
3. **Amend the plan** (not restart): 03 — probe B becomes "which assurance tier", drop the
   kill-condition framing; 04 — delete obx_-swap auth, add auth-header pass-through identity
   test; 05 — add credential fingerprint + account metadata (fingerprint computed at capture,
   before the authorization-header redaction; conformance case asserts raw credential absent
   on outbound bytes); 06 — unchanged; 07 — rewrite as local daemon + MDM enablement + doctor
   checks; 08 — add account-HALT case and bypass-visibility assertion.
4. **ADR-0021 rewrite**: local topology, pass-through auth, tiered assurance table
   (base=evident, MDM=hardened, central=custody-if-ever), availability section flips —
   inversion dissolves to per-developer blast radius; dead gateway = fail-closed-by-accident
   (dead localhost), the safe direction.
5. **Backend asks** (new, alongside existing dedupe/retention): account registry (org key
   fingerprints, allowed org UUIDs/domains) as policy input; session-with-turns-but-no-spans
   correlation alert.
6. **Hook-side account stamping**: read Claude Code's local account state (email/org UUID) at
   SessionStart, attach as session metadata — attribution works even where the gateway isn't
   (verify the local state shape first; ecosystem-knowledge tier, trivially checkable).
7. Daemon packaging: launchd plist (macOS), systemd unit (Linux), install/uninstall via
   `openbox init`, root-ownable layout for the MDM tier. Windows service later (repo already
   holds Windows at build-verified).

## What you shouldn't do

- **Don't keep the obx_-swap path "just in case".** Custody residue; two auth modes double the
  test surface for an unconfirmed need. Re-add only with a paying reason.
- **Don't put account allowlists or verdict caches in the gateway.** ADR-0017's lesson is the
  repo's scar tissue: local deciders second-guessing `/evaluate` is how orgs went ungoverned.
- **Don't let any doc/pitch say "cannot bypass".** Base tier is tamper-evident. The repo's own
  principle: overstating governance is the failure the product exists to prevent.
- **Don't build MDM profiles/agents.** Ship pushable artifacts + a recipe doc. Owning MDM is
  owning prevention through the back door — the exact scope you just moved out.
- **Don't add heartbeat/fleet-expectation infra in v1.** The cross-channel mismatch signal
  needs zero new client machinery. (Heartbeats need fleet state server-side — later, if ever.)
- **Don't log or egress the raw credential anywhere.** Hash-then-redact at capture; assert on
  outbound bytes (C18/C26 discipline).
- **Don't drop the `GET /protocol` CI oracle.** Hosting-agnostic; still the passthrough-compat
  guard.

## Cheaper/simpler paths, ranked (effort→impact)

1. P0 probe — hours; gates everything else.
2. Plan amendment over plan rewrite — days saved; 04–06 survive nearly verbatim.
3. Hook-side account stamping — one metadata read; attribution everywhere, gateway or not.
4. Skip the body sink in v1 if phase-08 volume numbers allow (cap-only); sink stays a config
   switch, decided by measurement not speculation.

## Benefits

- Zero-infra adoption — the product installs like a dev tool, not like a platform.
- No org-wide provider credential anywhere; OpenBox holds zero provider secrets (pass-through).
- Availability inversion dissolves: one dev's gateway down = one dev blocked, launchd restarts
  it; failure direction is fail-closed-by-accident (safe + self-evident).
- Account governance becomes platform policy (register fingerprints/org ids, get HALT), not
  client config.
- Deletes the heaviest phase-07 items (custody, KMS, HA, staged fleet rollout).
- Localhost hop ≈ 0 latency; only the gated-call verdict round-trip remains (unchanged).

## Trade-offs (honest costs)

- **Prevention is genuinely weaker than custody.** A local admin can kill the daemon and flip
  config; MDM detects/remediates but can't stop root. You chose detection as the base claim —
  correct for the market, but it IS a lower bar, and sales conversations with regulated
  enterprises will hit it.
- **Detection needs at least one channel alive.** A dev who disables hooks AND gateway leaves
  only absence, and absence-of-events is a documented non-signal. MDM tier closes this; base
  tier cannot.
- **OAuth account rule may stay detection-tier** if P1 finds no matchable org id.
- **Per-machine daemons = fleet update surface.** Same story as the hook binary today; a
  self-update channel becomes worth building eventually (out of scope now).
- **Stops being the right call when**: a customer segment makes prevention a purchase blocker.
  Switch cost then: stand the SAME binary up centrally, re-add the credential-swap auth
  (~the deleted phase-07-central work, order of a week, plus KMS/ops). Req 9's
  hosting-agnostic listener exists precisely to keep that door cheap.

## Work checklist

- [ ] P0 probe: BASE_URL redirect under subscription OAuth vs API key; auth header verbatim; report written
- [ ] P1 probe: org-id matchability from OAuth token / response headers; report written
- [ ] Verify Claude Code local account state shape (email/org UUID) on this machine
- [ ] Amend plan 260825-0027 phases 03/04/05/07/08 per above; 06 untouched
- [ ] Rewrite ADR-0021 draft: local topology, pass-through, tiered assurance, availability flip
- [ ] Schema: credential_fingerprint + account metadata fields (v1.4 scope); conformance case — raw credential absent on outbound bytes
- [ ] Hook-side SessionStart account stamping
- [ ] Daemon packaging: launchd + systemd + init/doctor wiring; root-ownable layout
- [ ] Backend asks filed: account registry policy input; session-vs-spans mismatch alert
- [ ] Docs: data-and-privacy + architecture assurance — tier language, zero prevention claims
- [ ] MDM enablement recipe doc (ownership, egress example) — enablement only

## Success metrics

- P0/P1 probe reports exist with yes/no per auth mode — before any plan edit merges
- Fresh `openbox init` on a zero-infra laptop → session's model calls traverse the localhost gateway (testbed green, no org service anywhere)
- Raw credential absent from 100% of outbound bytes; fingerprint present on gateway spans (conformance-asserted)
- Core policy HALT on a non-org fingerprint stops a real model call; session does not retry (testbed)
- Killing the gateway mid-session fires the session-vs-spans mismatch alert (testbed)
- `openbox doctor` reports gateway liveness, config target, config ownership
- Rewritten phase 07 effort ≤ original 2d; no always-on org service remains in the plan

## Unresolved questions

1. P0: does BASE_URL carry subscription-OAuth traffic at all? (Kills/limits pass-through; also
   afflicted the central design — never probed.)
2. P1: org id matchable from token/response headers? (OAuth refusal vs detection.)
3. Where does account-registry state live in the backend (policy data vs new surface)? Backend
   owner's call; new-service rule may demand an ADR there.
4. Fleet update channel for per-machine daemons — deferred, unowned.
