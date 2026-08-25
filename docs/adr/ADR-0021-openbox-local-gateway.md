# ADR-0021 — The OpenBox gateway is a per-developer LOCAL service

Status: **DRAFT — not accepted, not implemented.** Date: 2026-08-25.

Three sections carry a `TBD(probe)` marker. **They are the load-bearing ones**,
and this ADR must not be accepted while they are open: each is an empirical answer
about a provider we do not control, and filling one in by inference is the
overstatement failure this product exists to prevent. See
[the probe runbook](../../plans/260825-0027-openbox-gateway-full-capture/probes/RUNBOOK.md).

Requires a new service, so this ADR exists per `CLAUDE.md`'s rule. It reuses every
existing table, endpoint and auth path: gateway spans ride the same
`/api/v1/governance/evaluate` with the same `Bearer obx_` + AIP signing, into the
same `sessions` → `governance_events` and Merkle leaves.

Amends **ADR-0016** (install scope — see the separate amendment there). Extends
**ADR-0017** (`/evaluate` is the only decider) to a surface the hooks cannot see.
Does not amend **ADR-0013** (spans stay retired for tool events; a gateway span
describes a real observed HTTP exchange, which is the thing ADR-0013's synthesized
spans were not).

## Context

Hooks see what the agent DOES. They cannot see the model call itself — no hook
carries the request headers, the request body, or the response — so three things
are unreachable today:

1. **Model-call headers and bodies.** The only remaining large gap in full capture
   (ADR-0019). System prompts and raw request bodies are named there as
   uncapturable *from hooks*; a gateway is the surface where they exist.
2. **Synchronous refusal of a model call.** The hook path refuses a TOOL call. It
   cannot refuse the inference itself.
3. **Which account paid for the call.** Account binding needs the credential the
   call actually used, which only the transport sees.

## Decision

A **per-developer localhost daemon** that Claude Code is pointed at via
`ANTHROPIC_BASE_URL`, which forwards to the provider and reports to `/evaluate`.

### 1. Local-first, not org-operated (product decision, 2026-08-25)

Target customers cannot run services. The gateway is installed by `openbox`
tooling and runs as the developer, restarted by launchd/systemd. The listener stays
address-configurable so a central deployment remains possible later, but **no
central-deployment work is built**: no custody, no KMS, no HA, no fleet runbooks.

This dissolves the availability inversion rather than accepting it. A dead gateway
blocks **one** developer, and it fails closed by accident — a dead localhost
listener is a connection refused, not a silent bypass.

### 2. The assurance claim is DETECTION, not prevention — and it has tiers

**This section is the decision.** An ADR that blurs the three tiers into one claim
must not be merged.

| Tier | What the org deploys | What it actually buys |
|---|---|---|
| **Base** (ships regardless) | `openbox init` sets the env at user scope | **Tamper-evident.** A developer can unset the variable; doing so is visible (a session with turns but no gateway spans) and attributable. Bypass is *detected*, never *prevented*. |
| **MDM** (org-operated) | managed settings + root-owned daemon, optionally egress control | Prevention, to the extent the org's MDM provides it. OpenBox ships MDM-pushable artifacts; **OpenBox never ships MDM tooling.** |
| **Central** (not built) | the same binary, deployed centrally | Nothing new in kind. Named only so "we could host it" is not mistaken for "we do". |

**Docs must never say "cannot bypass".** The base tier's honest sentence is: a
bypass leaves a hole in the record, and the hole is queryable.

### 3. Pass-through auth: OpenBox holds zero provider secrets

The developer's own credential relays **untouched**, Authorization header included.
The earlier `obx_`→credential-swap design is deleted, not deferred: it would have
made OpenBox a custodian of every developer's provider credential, which is a
strictly larger blast radius than the one ADR-0015 already argues about.

What egresses instead is a **one-way fingerprint** (`sha256`, truncated) on the
gateway span. The raw credential appears in zero outbound bytes, and that is
conformance-asserted on the wire rather than argued here.

### 4. Inspect without modifying

Forwarded bytes are byte-identical to received bytes. Redaction applies to the
**captured copy** only. A gateway that rewrote the request would change what the
model saw, which makes the capture a description of something that did not happen.

### 5. No MITM proxy

Unchanged from the earlier design: a CA that can forge any domain is a large new
risk that buys no assurance a substituting gateway does not already have.

### 6. Account binding is core POLICY, not gateway logic

ADR-0017's dogma, applied: sensors attach **evidence**, `/evaluate` decides. The
gateway sends the credential fingerprint plus local account metadata (**org UUID +
email, and nothing more** — `organizationName` and `organizationRole` are
excluded); core returns HALT/BLOCK on a non-org account. No local allowlists, no
local verdict caching, no gateway-side account rules.

Email is PII, egressed as governance evidence like the DID already is, and
documented as such in `docs/data-and-privacy.md`.

### 7. Fail posture: ALWAYS REFUSE (owner decision, 2026-08-25)

A gated model call is **refused when `/evaluate` is unreachable, regardless of the
`fail_closed` key.** This is a deliberate divergence from the hook path, which
keeps posture-driven behaviour, and it is worth stating both halves:

- **Why.** The gateway is the stronger enforcement point by owner intent. A model
  call that proceeds unevaluated is the one thing this service exists to stop.
- **The cost, accepted.** A core outage refuses gated model calls for **every**
  governed developer. Ungated calls still forward with no round-trip, so the
  outage is not total — but it is not cosmetic either.
- **No offline grace.** None. A grace window is a local verdict cache wearing a
  different name, and §6 rejects those.

**The ordering rule this makes sharper.** Pre-ADR-0017, `ApplyFailurePolicy` ran
BEFORE the evaluation, which was harmless only while the local step produced
verdicts; once it stopped, a fail-closed org denied every gated call *without ever
asking*. Conformance case C1 caught it. Here the same mistake would refuse every
gated model call while reporting a policy decision that no policy made. **No
synthesized refusal may fire before an evaluation attempt** — phase 06's ordering
test is the control, and it is a merge blocker.

### 8. `TBD(probe)` — auth coverage

Which auth modes follow `ANTHROPIC_BASE_URL` is **unresolved**, and it bounds who
this tier covers. If subscription OAuth does not redirect, the gateway tier covers
API-key/console orgs only and **this ADR and the product docs must say so in those
words.** Track B proceeds either way; the probe scopes it.

Source: P0, `plans/reports/probe-260825-baseurl-auth-coverage.md`.

### 9. `TBD(probe)` — the refusal shape

The status code and body a refusal uses is **unresolved**. It must not trip Claude
Code's capability-rejection retry, which matches on upstream error wording: a shape
that does would silently disable a capability for the rest of the session, which is
worse than not refusing. If no candidate qualifies, phase 06 descopes to
observe-only and prevention stays in the hooks.

Source: probe A, `plans/reports/probe-260825-halt-rendering.md`.

### 10. `TBD(probe)` — the OAuth account rule

Whether an org identifier is matchable from an OAuth credential is **unresolved**.
Two branches, both acceptable: matchable ⇒ the account rule can refuse; not
matchable ⇒ it ships **detection-only** for OAuth while API-key fingerprints still
refuse. The branch must be named in this ADR, not left to the reader.

Source: P1, same report as §8.

## What this gateway explicitly CANNOT do

Mandatory, because "gateway" invites the wrong inference:

- **Prevent bypass without MDM.** Base tier is detection. Stated in §2 and it must
  stay stated.
- **Redact at source.** Guardrail redaction is still not wired anywhere in this
  product; the gateway captures and redacts its own copy, and that is all.
- **Cover CI.** A gateway on a developer's machine sees a developer's machine.
- **Cover non-Anthropic wire formats in v1.** One provider's API shape.
- **Cover Codex.** Deferred by owner decision (2026-08-25) until the Claude Code
  gateway works end to end. The `OPENAI_BASE_URL` route is a doc-tier idea, not a
  plan item.
- **Hold a provider credential.** By construction (§3), and that is the point.

## Consequences

**Gained**

- Model request/response headers and bodies as `SpanData` — the last large capture
  gap that hooks structurally cannot reach.
- Synchronous refusal of an inference, before it leaves the machine.
- Account-binding evidence, decided by core policy rather than by client logic.
- Bypass becomes queryable: a session with turns and no gateway spans is a
  detectable shape in stored data, and `openbox doctor` flags a bypass-capable
  configuration.

**Lost — the accepted trade-offs**

- **A second process to install, run and diagnose**, per developer. The install
  surface grows and so does the failure surface.
- **A core outage now refuses model calls** (§7), not merely tool calls.
- **Retention and policy-evaluation volume grow again.** ADR-0019 already filed the
  volume ask for tool bodies and thinking (≤64KB per turn); full model
  request/response bodies are a larger increment, and phase 08 measures it before
  the cap is widened or a body sink is built.
- **The base tier's assurance is weaker than the word "gateway" suggests**, and
  every document that mentions it inherits the duty to say so.

**Not yet proven.** Everything in §§8–10, plus the claim that core stores gateway
spans as their own rows. Nothing here has run against a live stack.
