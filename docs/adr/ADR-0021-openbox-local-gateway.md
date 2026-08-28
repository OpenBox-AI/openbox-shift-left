# ADR-0021 — The OpenBox gateway is a per-developer LOCAL service

Status: **DRAFT — not accepted. Substantially IMPLEMENTED.** Date: 2026-08-25.
**Amended 2026-08-28** (§5 reversed, §8 completed, §10 decided) — see below.

The status split is deliberate and is not a contradiction. Everything in §§1–7 is
built and unit/conformance-verified. Code that does not depend on the open answers
was worth building; accepting an ADR whose load-bearing empirical questions are
unanswered would not be. Nothing has run against a real stack.

**Three sections carried a `TBD(probe)` marker; one still does.** As of
2026-08-28:

- **§5 is REVERSED** by owner ruling OD2 — an in-path TLS relay with a local CA is
  now a product decision. [ADR-0022](ADR-0022-native-telemetry-and-transport-lanes.md) §2.
- **§8's coverage question is ANSWERED** by measurement: the terminal CLI follows
  `ANTHROPIC_BASE_URL`, the desktop app does not, and desktop/OAuth calls are
  reachable by two other lanes instead. What remains open there is narrower and no
  longer blocks coverage.
- **§10 is DECIDED** — detection-only for OAuth, fingerprint refusal for API keys.
- **§9 remains `TBD(probe)`** and is still load-bearing: the refusal shape is an
  empirical answer about a provider we do not control, and filling it in by
  inference is the overstatement failure this product exists to prevent. **This
  ADR must not be accepted while §9 is open.**

See [the probe runbook](../../plans/260825-0027-openbox-gateway-full-capture/probes/RUNBOOK.md).

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

### 5. No MITM proxy — **REVERSED 2026-08-27 by owner ruling OD2**

> **This section no longer holds.** [ADR-0022](ADR-0022-native-telemetry-and-transport-lanes.md)
> §2 adopts an in-path TLS relay with a local CA. What follows is the original
> decision, kept for the record, and then what changed.

Original: a CA that can forge any domain is a large new risk that buys no
assurance a substituting gateway does not already have.

**What changed is the second clause, not the first.** The risk did not shrink. The
alternative's coverage turned out to be smaller than believed: §8 below measured
that the substituting gateway covers the terminal CLI and **not** the desktop app,
and cannot cover a subscription-OAuth session without colliding with §3. "No
assurance a substituting gateway does not already have" was the load-bearing half
of the sentence, and it was false for a large share of real use.

The ruling (OD2, 2026-08-27) is that the transport lane is a **product**: a native
Go TLS relay in the openbox daemon, launchd-managed, scoped to
`api.anthropic.com`, CA generated per developer under `~/.openbox/`. **Not Docker,
not mitmproxy** — those stay refused, and the `goproxy` library compiled into our
own binary is not a reversal of that.

Safeguards, which are what make the reversal acceptable rather than a
capitulation: one upstream host only; a per-developer CA that is never shared;
**one command installs everything and one command removes it all**, proven by a
system-state diff (services unloaded, displaced settings restored from the
activation record, CA and local captures deleted); and §4 stands unchanged —
forwarding stays byte-identical, so the relay still describes what actually
happened.

The cost is stated in ADR-0022's Consequences and not softened here: a CA private
key now exists on the developer's machine, under the same plaintext exposure
ADR-0015 documents for every other credential this product holds.

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

### 8. Coverage — CLIENT resolved, auth mode still open

Two questions were folded together here. The first is now **answered**, by
observation rather than inference.

**Which CLIENTS follow `ANTHROPIC_BASE_URL`: the terminal CLI does, the desktop
app does not** (measured 2026-08-27). With the daemon listening, configured at
`~/.claude/settings.json`, and `--gateway-verbose` on: a `claude` session in a
terminal produced `POST /v1/messages` arrivals and three `capture: recorded` events
carrying real provider request ids, while a desktop-app session over the same
install produced **no log lines at all**. Anthropic's own documentation gives the
mechanism — the desktop app reads gateway routing from its
[third-party inference configuration](https://claude.com/docs/third-party/claude-desktop/gateway),
"not from `ANTHROPIC_BASE_URL` or `settings.json`".

So, in the words this section demanded: **`openbox init --gateway` governs model
calls made by the terminal CLI. It governs none made by the desktop app, and
reports nothing about the difference** — `doctor` reads the settings file it wrote,
and the desktop app never consults that file. On a machine where the developer
works in the desktop app, the model-call tier is inert while every local check
looks healthy. That is the "configured but not in force" shape §2 promised would be
detectable and, for this client, is not.

The desktop app *can* be pointed at the gateway — `inferenceProvider: gateway` plus
`inferenceGatewayBaseUrl`, MDM-distributable as a `.mobileconfig` — but that path
**collides with §3**. Gateway mode replaces the claude.ai login with a credential
the org supplies, so a pass-through relay has no provider credential to forward
unless the org holds an Anthropic API key it can put in `inferenceGatewayApiKey`.
For a subscription developer the two designs are incompatible, and closing that is
a product decision, not a wiring detail.

**Still open for THIS lane:** whether subscription OAuth follows a changed base
URL *for the CLI*. The three captured calls above prove the CLI relays and is
captured; they do not prove which auth mode was in play. If OAuth does not
redirect, this tier covers API-key/console orgs only.

**What is no longer open is the COVERAGE question this section was really
asking** (2026-08-27). The sibling lab repo `openbox-logger`, run
`20260827T063932Z-225cac` on a live desktop session, captured **97 `/v1/messages`
calls, every one carrying an OAuth `authorization` header and none carrying
`x-api-key`** — through two lanes that need no base-URL change at all: the tool's
own enhanced-telemetry OTel export, and an in-path TLS relay, both reached through
the `env` block of `~/.claude/settings.json`. So desktop and subscription-OAuth
model calls **are** observable; they are simply not observable *by this lane*.

[ADR-0022](ADR-0022-native-telemetry-and-transport-lanes.md) builds both, which
changes what the open question costs. It is now a question about **this** lane's
reach, not about whether a whole class of developer is ungoverned — and answering
it either way no longer blocks coverage. The honest statement of this lane,
unchanged: `openbox init --gateway` governs model calls made by the terminal CLI,
governs none made by the desktop app, and reports nothing about the difference.

Sources: P0 and its 2026-08-26 amendment,
`plans/reports/probe-260825-baseurl-auth-coverage.md`; the client measurement above
is `~/.openbox/gateway.log` on the author's machine, reproducible with
`--gateway-verbose` and one prompt per client.

### 9. `TBD(probe)` — the refusal shape

**Implementation note (2026-08-25).** The refusal PATH is built
(`gateway/refuse.go`, `gateway/gate.go`); only two constants — `refusalStatus`
and `refusalErrorType` — are provisional, and they are isolated so probe A's
answer is a two-line change. The provisional pair is `403` plus an error type that
is deliberately none of the provider's own literals, chosen to be maximally unlike
a transience signal the client is built to retry. `TestRefusalShapeIsProbePending`
asserts that REQUIREMENT — not the answer — so a future edit cannot quietly pick a
status the client retries around.

If probe A finds no qualifying shape, the descope is already written: phase 06
becomes observe-only, prevention stays in the hooks, and the gate ships with
refusal disabled.

The status code and body a refusal uses is **unresolved**. It must not trip Claude
Code's capability-rejection retry, which matches on upstream error wording: a shape
that does would silently disable a capability for the rest of the session, which is
worse than not refusing. If no candidate qualifies, phase 06 descopes to
observe-only and prevention stays in the hooks.

Source: probe A, `plans/reports/probe-260825-halt-rendering.md`.

### 10. The OAuth account rule — **branch named 2026-08-27**

Whether an org identifier is matchable from an OAuth credential was left with two
branches: matchable ⇒ the account rule can refuse; not matchable ⇒ it ships
**detection-only** for OAuth while API-key fingerprints still refuse. As required,
the branch is named rather than left to the reader.

**The branch taken is detection-only for OAuth.** An org *is* identifiable —
`organization.id` and `user.email` ride every enhanced-telemetry event (evidence
run `20260827T063932Z-225cac`) — but that identity is **asserted by the tool being
governed**, not observed independently. Attribution of that kind is evidence, not
proof, and it can refuse nothing on its own: a client that misreports its org
would be refused on the strength of its own claim, and a client that reports
nothing would be refused on nothing at all.

So:

- **OAuth** → **detection-tier binding from asserted telemetry.** The account rule
  observes and reports; it does not refuse.
- **API key** → the credential **fingerprint** still supports refusal, unchanged
  by this.

This resolves §10 as a decision. It does not resolve §9 (the refusal shape), which
remains a `TBD(probe)`: naming who can be refused says nothing about what a
refusal must look like on the wire.

Source: P1, same report as §8; ADR-0022 §8.

## Implementation record (2026-08-25) — what §§1–7 became

Stated here because an ADR whose code has drifted from it is worse than no ADR.

| Decision | Where it lives | Held by |
|---|---|---|
| §1 local-first | `gateway/`, loopback-only, `openbox gateway` | `TestListenerMustBeLoopback`, and `gateway.Listen` re-checks the address the KERNEL returned, because a name can resolve differently at bind time than at validate time |
| §2 detection tier | `cli/internal/gatewaycheck` + `openbox doctor` | tier inferred from FILE OWNERSHIP, never from a flag OpenBox writes; `TestReportNeverClaimsPrevention` fails on any affirmative prevention claim AND requires the detection framing |
| §3 pass-through | no credential path in `gateway/` at all | an AST guard that resolves import aliases, covers `os`/`syscall`/`io/ioutil`, refuses dot-imports, and is bounded by an import allowlist so its single-module scan is provably complete |
| §4 inspect without modifying | hand-rolled relay | forward-identity asserted in BOTH directions with an enumerated exception set — the no-ADDITIONS leg is the one that matters, since every default this defeats adds rather than removes |
| §6 account evidence | `gateway/capture.go` + `adapters/claude-code/account.go` | fingerprint→redact→cap ordering enforced INSIDE one function, so it is a property of code rather than of a caller's discipline |
| §7 always refuse | `gateway/gate.go` | `Decision.Evaluated` makes the ordering invariant CHECKABLE; asserted across all six refusing branches |

Two things worth recording because they are easy to get wrong later:

- **There is no `fail_closed` input to the gateway's gate at all.** That absence IS
  §7. A posture key there would be a way to switch the gateway's enforcement off,
  which is precisely what the owner decision rejected.
- **An uninterpretable verdict REFUSES.** An empty or unrecognized literal is not
  an allow. This is ADR-0020's trap in a new place: Codex's renderer wrote nothing
  for an unknown literal, which would have made HALT silently proceed.

One deliberate divergence from this ADR's own framing: the doc originally argued
the relay had to be hand-rolled because "every default in net/http undoes the
invariant". That is true of the legacy `Director` path and NOT of the modern
`Rewrite` hook, which strips `X-Forwarded-*` and auto-flushes SSE. The honest
reason to hand-roll is phase 05's two-way tee. Corrected in `gateway/proxy.go`'s
package comment; recorded here so the ADR and the code do not disagree.

## What is reachable from the served handler (state this before believing the table)

**Capture is wired; refusal is not.** The two halves shipped separately and the
distinction is the point:

- **Capture: LIVE (2026-08-26).** `openbox gateway` calls `WithCapture`, and each
  relayed call becomes a `TurnCompleted` carrying the observed exchange. The
  connector is `cli/internal/gatewayemit`, which files the event in the Claude Code
  adapter's spool under the session id the request named, so the existing
  hook-driven flushers deliver it on the existing client, auth and signing. The
  relay process itself performs no credential I/O of any kind — not the provider's
  (§3) and not OpenBox's own, since the flusher owns the signing key.
- **Refusal: written, still uncalled.** `gate.Decide` and `WriteRefusal` are
  tested and drilled, but nothing calls them from `ServeHTTP`, and `WithGate` has
  no production caller. So §7's always-refuse posture is **not** in force: a gated
  model call is not gated, because nothing is. Deliberate — probe A has not named
  a refusal shape, a wrong one silently disables a Claude Code capability for the
  session (§9), and the join should land with the doctor check that distinguishes
  "policy refused" from "gateway dead". §6 account binding attaches its evidence
  but no verdict acts on it yet.

**How capture came to be absent for as long as it was, recorded because the shape
recurs.** `WithCapture` had no production caller at all, so `g.emitter` was nil and
every capture was discarded — while package `gateway` tested the relay against a
stub `Emitter` and package `client` tested the span builder against a hand-written
`DevEvent`. A fake at each end of a seam with no implementation between them keeps
both suites green and proves nothing about the seam. The control is
`cli/cmd/openbox/gatewaycapture_test.go`, which drives the real command into the
real spool and supplies no fake anywhere.

The design gap this section used to name — `Capture` needing the response while the
gate must decide before one exists — **is fixed**: `CaptureRequest`/`Complete`
split it (`gateway/capture.go`), with the fingerprint taken from the live headers
before redaction, once.

A related consequence of §4 that only shows up once wired: a gateway turn's span
carries the provider's RAW response body, which is not the
`{"choices":[{"message":{"content":…}}]}` shape core's alignment extractor
requires. Since the two span producers ride mutually exclusive events, a
gateway-observed turn contributes nothing to goal alignment — it is not corruption,
but it is a silent gap, and alignment for those turns comes from the hook path or
not at all.

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
