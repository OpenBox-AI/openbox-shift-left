# ADR-0022 — Two more model-call lanes, native and local: telemetry and transport

**Status:** Accepted
**Date:** 2026-08-28
**Reverses:** [ADR-0021](ADR-0021-openbox-local-gateway.md) §5 (owner ruling OD2)
**Completes:** ADR-0021 §8 (client coverage) and names §10's branch
**Builds on:** [ADR-0013](ADR-0013-tool-call-as-activity.md) (a turn is an
Activity), [ADR-0014](ADR-0014-turn-as-activity-and-identifier-allowlist.md)
(`llm_completion`), [ADR-0017](ADR-0017-inline-policy-evaluation.md)
(`/evaluate` is the only decider), [ADR-0021](ADR-0021-openbox-local-gateway.md)
(the gateway lane this generalizes)
**Context:** plan `260827-2301-go127-oss-three-lanes`, stage B (phases 08–14);
proposal `plans/visuals/260827-1439-three-lanes-one-pipeline.html`
(artifact 30d03ca3); evidence run openbox-logger `20260827T063932Z-225cac`

Two new **local services** — an OTLP telemetry receiver and an in-path TLS
transport relay — so this ADR exists per `CLAUDE.md`'s rule. Neither adds a
control-plane table, endpoint or service: both feed the pipeline this repo
already has (map → redact → cap → spool → AIP-sign → flush → `/evaluate`), into
the same `sessions` → `governance_events` and Merkle leaves, under the same
`Bearer obx_` + AIP signing.

## Context: the gateway lane governs less than it appears to

ADR-0021 shipped a local relay that Claude Code is pointed at via
`ANTHROPIC_BASE_URL`. Its §8 was then answered by measurement (2026-08-27): the
**terminal CLI follows that variable and the desktop app does not**, because the
desktop app reads gateway routing from its own third-party inference
configuration. On a machine where the developer works in the desktop app, the
model-call tier is inert while every local check reports healthy — "configured
but not in force", the shape §2 promised would be detectable and, for that
client, is not.

Pointing the desktop app at the gateway is possible but collides with ADR-0021
§3: gateway mode replaces the claude.ai login with an org-supplied credential, so
a pass-through relay has no provider credential to forward unless the org holds
an Anthropic API key. For a subscription developer the two designs are
incompatible.

The sibling lab repo `openbox-logger` then demonstrated, on a live desktop
session (run `20260827T063932Z-225cac`), that **desktop and subscription-OAuth
model calls are capturable by two other means** that need no base-URL change,
both reachable through the `env` block of `~/.claude/settings.json`:

- the tool's own **enhanced-telemetry OTel export**
  (`CLAUDE_CODE_ENHANCED_TELEMETRY_BETA`, `OTEL_LOG_RAW_API_BODIES`), and
- an **in-path TLS relay** (`HTTPS_PROXY` + `NODE_EXTRA_CA_CERTS`).

All 97 `/v1/messages` calls in that run carried an OAuth `authorization` header
and zero `x-api-key`. That is the premise change this ADR acts on.

## Decision

### 1. Three lanes, one pipeline; the lane is a CLAIM, not a rank

A model call may be observed by up to three local producers. Each is a different
claim about what the evidence proves, and the difference must be stated wherever
the evidence is read, never averaged into one "model calls are governed":

| Lane | Vantage | Claim | Suppressible by |
|---|---|---|---|
| **T1 transport** | in path, TLS-terminating relay | the call happened, and these were its bytes | removing the env lane (leaves a trace: the CA and service remain) |
| **T2 gateway** | in path, base-URL substitution | as T1, for clients that follow `ANTHROPIC_BASE_URL` | unsetting the variable; **silently inert** for the desktop app |
| **T3 telemetry** | the tool reports its own calls | the tool SAID this call happened | the governed tool itself — it is the reporter |

T3 is the weakest claim in the product and this ADR does not dress it up: a tool
that wants to hide a call simply does not export it. It is adopted because it is
the only lane that covers the desktop app and subscription OAuth today, and
because **partial evidence that names its own limit is worth more than a gap**.
OD4 is the compensating control: telemetry silence on an otherwise-active session
(hooks firing, no OTel) is a **finding**, not an absence.

### 2. The transport lane is a PRODUCT, native, one command in and out (OD2, 2026-08-27)

Owner ruling, recorded verbatim in intent: the transport lane is a **native
system service** — a Go TLS relay in the openbox daemon, launchd-managed, scoped
to `api.anthropic.com`, with a credential-grade CA under `~/.openbox/`. **Not
Docker. Not mitmproxy.** Hard contract: **one command installs, configures and
enables everything; one command removes it all** — services unloaded, displaced
settings restored from the activation record, CA and local captures deleted.

**This formally reverses ADR-0021 §5** ("No MITM proxy"). §5's reasoning was that
a CA able to forge any domain is a large new risk buying no assurance a
substituting gateway does not already have. The second half of that sentence is
what stopped being true: the substituting gateway demonstrably does not cover the
desktop app or an OAuth subscription session, which is a large share of real use.
The risk did not shrink; the alternative's coverage turned out to be smaller than
believed. The safeguards are therefore explicit — scoped to one upstream host,
CA generated per developer and never shared, removal proven by a system-state
diff (V7), and byte-identical forwarding preserved (ADR-0021 §4 stands: a relay
that rewrote the request would describe something that did not happen).

**`goproxy` keeps OD2 intact.** The CONNECT/TLS/CA front-end is
`github.com/elazarl/goproxy` (D-OSS-1) — a Go **library compiled into our own
binary**, not a runtime dependency on another product's process. Adopting it is
the opposite of the Docker/mitmproxy shape OD2 refused. Its adoption is gated on
a spike proving byte-identity and per-chunk SSE flush against the existing
identity suite; failing either is a stop-and-report, not a workaround.

### 3. Exactly one producer per session (the election)

Namespaces make the ids **disjoint**; the election makes the **count** right.
Both are needed, and neither substitutes for the other.

Precedence is automatic: **transport > gateway > telemetry**. In-path relays
outrank a client-asserted lane, because a lane that observed the bytes outranks
one that was told about them.

Without the election, two lanes describing one turn would each emit their own
events for it. The ids being disjoint means core stores both rather than deduping
one away — which is the *better* failure, but still a doubled token count on every
dashboard the numbers feed. Without the namespaces, core's dedupe key
`(agent_id, workflow_id, run_id, activity_id, event_type)` would absorb one as a
duplicate of the other and **half the evidence would vanish with no error
anywhere**. That silent-loss direction is why the namespaces are a correctness
invariant and not a naming preference.

### 4. Two new discriminators, symmetric with the gateway's

| producer | event field | `activity_id` |
|---|---|---|
| hook turn | `turn_index` | `<session>[:agent:<id>]:turn:<n>` |
| gateway | `gateway_request_id` | `<session>:gateway:<id>` |
| Codex rollup | `session_rollup` | `<session>:usage:rollup` |
| **telemetry** | `otel_request_id` | `<session>:otel:<id>` |
| **transport** | `proxy_request_id` | `<session>:proxy:<id>` |

One self-describing field per producer. `client.turnActivityIDFor` branches on a
field's **presence**, never on a value — which is how the gateway branch avoids
ambiguity today, and the property a `producer` enum would have destroyed.

**Rejected:** a single `relay_request_id` plus a `producer` enum. Two fields where
one suffices, and it would make the derivation depend on a value's content.

Both new ids originate upstream — a provider request id relayed through an OTLP
payload, or read off a relayed response — and reach a stored key verbatim. They
are therefore bounded and charset-checked (128 characters, printable ASCII, no
whitespace), and the bound is **declared in the contract** rather than left to
each producer, so a lane added later inherits it instead of having to remember it.

**`gateway_request_id` is retrofitted to the same bound, reversing an earlier
call in this ADR's own drafting.** It was first left alone on the reasoning that
tightening a field a shipped producer already emits is a contract break wearing
the costume of a repair. That reasoning is sound in general and **false here**:
`GatewayRequestID` has exactly ONE production assignment path
(`gatewayemit.EventFor`, fed by `Emitter.requestID`), which returns either an
upstream id already gated by `usableRequestID` or a locally minted `gw-` id of
about 35 characters — and `maxRequestIDLen` is **128**, the same number. The
retrofit therefore rejects nothing any gateway can emit, and leaving it out would
have put three fields of one kind at two contract depths, with the oldest and
most-copied one as the unbounded template. Recorded rather than quietly changed,
because the first reasoning was written down.

Neither id is content. Both are correlation identifiers of the same class as
`policy_id`, never derived from prompt or body text, and neither joins
`contentMetadataKeys`.

### 5. Contract v1.6 — and the repair it forced

Additive. But writing the two branches surfaced that **the contract had been
rejecting shapes the client already emitted**, in two places:

- `session_rollup` was **not a declared property at all**, and the event object is
  `additionalProperties: false`. Every Codex session-usage pair has failed its own
  contract since v1.1.
- `TurnStarted` required `turn_index` **unconditionally**. v1.5 repaired
  `TurnCompleted` for the gateway and left the opening half alone. Stated
  precisely, because the difference matters: the gateway emits **no**
  `TurnStarted` at all (`gatewayemit.EventFor` is `TurnCompleted`-only, and
  deliberately so), so v1.5's half-repair cost it nothing. What it did break is
  the **Codex rollup's** opening half — which is why that pair has been rejected
  in full — and it would have broken any later lane emitting a pair, which is what
  phases 09–11 intend. This is the same defect ADR-0021 records shipping once,
  made twice in adjacent branches for the same reason: the rule was written where
  the bug was noticed rather than where the rule belongs.

So v1.6 puts the rule in ONE place: `$defs.turnProducer`, a five-branch `oneOf`
requiring **exactly one** discriminator, `$ref`'d from both turn branches. A sixth
producer is added there once and both halves accept it. Restating it per branch is
what let the two halves drift apart, and structure — not diligence — is what stops
it happening again.

Exactly-one is enforced in both directions: no discriminator means the client
mints no `activity_id` and the pair never correlates; two means the event is
attributed to a producer that did not observe it.

### 6. Posture: installing is the opt-in; content stays behind the existing gates

Both lanes are **on once installed**, and installing is the opt-in. ADR-0016's
default-on lesson does not transfer: enforcement-by-default is inert without an
org policy, whereas these lanes move real traffic and real content the moment they
exist.

No new privacy key. Content on either lane rides the **existing**
`content_capture` gate and usage rides `finops`, both default-ON. A third key
would let an org opt out of one lane's content while another lane carried the same
bytes — a posture that reads as a choice and is not one.

### 7. Adoptions, and the floor that retired the pins

Recorded here because they are decisions this stage rests on, not incidental
dependency bumps:

| ID | Adoption |
|---|---|
| **D-GO-1** | Go floor 1.23.0 → **1.27.0** across `go.work` and all modules. Every adopted dependency resolves at latest **with no pin**; the version-pin scheme is retired and `x/term` is unpinned. Executed by phase 01, alone and green, before everything else. |
| **D-OSS-1** | transport CONNECT/TLS/CA front-end → `github.com/elazarl/goproxy` — **neither Docker nor mitmproxy** (§2) |
| **D-OSS-2** | OTLP intake → `go.opentelemetry.io/collector/receiver/otlpreceiver` as a **library**, handling both encodings. Supersedes the earlier vendored-protobuf fallback; the wire probe now informs configuration, not whether the lane survives. |
| **D-OSS-3** | service lifecycle → `github.com/kardianos/service` — **unit writing only**. Proof-order install, rollback-removes-unit, stdio→file and the activation record stay ours (see ADR-0021's implementation record for why each exists). |

**Structural consequence:** transport is its **own module** (`transport/`, in
`go.work`), not `gateway/transport/`. goproxy inside `gateway/` would breach that
module's credential guard allowlist and, worse, would put credential-path code
outside the scan that exists to bound it. The module carries its own guard
(ADR-0023's shape: direct requires only).

### 8. ADR-0021 §10's branch, named

Whether an org is matchable from an OAuth credential is answered for the
**telemetry** lane and stays open for refusal: `organization.id` and `user.email`
ride every enhanced-telemetry event, so an OAuth session **is** attributable —
but **client-asserted**, by the tool being governed. So the branch is:

- **OAuth** → **detection-tier binding from asserted telemetry.** Attribution is
  evidence, not proof; it can refuse nothing on its own.
- **API key** → the credential **fingerprint** still supports refusal, unchanged.

## Consequences

**What gets better.** Desktop and subscription-OAuth model calls stop being a
silent gap. The gateway's "configured but not in force" blind spot gains a lane
that covers it, and OD4 turns the remaining silence into a finding rather than an
absence.

**What gets worse, stated plainly:**

- **A CA now exists on the developer's machine.** ADR-0021 §5 refused this and
  §5's risk assessment was not wrong — it is accepted, scoped and reversible, not
  disproven. Combined with ADR-0015 (credentials are plaintext, readable by
  anything running as the developer, unprotected on Windows), the CA's private key
  inherits that same exposure. It is a per-developer CA scoped to one upstream
  host, which bounds the blast radius; it does not eliminate it.
- **T3 is suppressible by the thing it observes.** Named in §1, and it is the
  reason the lane must never be reported as equivalent to the in-path lanes.
- **Oversized bodies truncate (OD1(c), owner ruling 2026-08-27).** Model-call
  bodies in the evidence run averaged 290KB and peaked at 566KB; **92 of 97
  exceeded 64KiB**. They egress through the standard redact→cap leg and truncate
  at 65,536 runes, so for roughly 95% of model calls **the tail exists nowhere
  org-side**. No digest scheme, no excerpt logic, no local evidence store — all
  three were proposed and retired. Content-based policy therefore sees a prefix,
  and any reader of that evidence must know it.
- **OD3: these are beta surfaces.** `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA` and
  `OTEL_LOG_RAW_API_BODIES` are accepted as beta and may change per release.
  Mitigation is version-pinned probes plus doctor silence detection — not a
  promise of stability.
- **A gateway- or transport-observed turn contributes nothing to goal
  alignment.** Its span carries the provider's raw response body, not the shape
  core's alignment extractor parses. A silent gap, not corruption; it retires with
  [openbox-core#130](https://github.com/OpenBox-AI/openbox-core/issues/130).
- **`otlpreceiver` brings a large dependency tree** (~98 requires). Accepted:
  hand-rolling an OTLP intake that must handle both encodings is the trade
  D-OSS-2 refuses to make. It is bounded at the module that takes it, per
  ADR-0023.

## What later phases must satisfy

Sentinel tests, listed here so a phase cannot quietly ship without them:

1. **Namespace disjointness across all five producers**, and against a tool-call
   id — `client.TestEveryTurnProducerNamespaceIsDisjoint`.
2. **Exactly one discriminator**, on BOTH turn halves, positively and negatively —
   `conformance.TestOneDiscriminatorValidates` /
   `TestTwoDiscriminatorsRejected` / `TestNoDiscriminatorRejected`.
3. **Existing byte pins unchanged** — `client/turn_key_pin_test.go` and
   `client/approval_key_pin_test.go`. A moved id is a data-loss event, not a test
   failure; reverting is mandatory.
4. **`usage.go`'s INV-2 sentinel untouched.** Neither lane may widen the
   transcript projection's allowlist. Adding a fifth bound field needs its own
   amendment to ADR-0014, for the reason that amendment already states.
5. **One producer per session** — the election, asserted end to end (V4).
6. **One command in, one command out** — a system-state diff after removal
   returns empty (V7): settings restored from the activation record, services
   unloaded, CA deleted.
7. **Byte-identity and per-chunk SSE** on the goproxy path, against the existing
   identity suite, **before** any transport service code (the spike gate).
8. **No synthesized refusal before an evaluation attempt** — ADR-0021 §7's
   ordering rule, which the transport lane inherits along with its fail posture.
9. **The declared id bound and the producer's own bound must keep agreeing.** The
   contract states 128 characters of printable ASCII declaratively;
   `gatewayemit.printableASCII`/`maxRequestIDLen` states the same rule
   imperatively, in Go. `gatewayemit.TestGatewayIDBoundMatchesTheContract` holds
   them together **for all three id fields**, including the two no producer sets
   yet — so phases 09/11 inherit a live check rather than an obligation to
   remember one. This is `CLAUDE.md`'s "bounds have owners, and reusing one is a
   silent regression" enforced before the reuse: widening `maxRequestIDLen` alone
   would make a producer emit ids its own contract rejects, and loosening the
   pattern alone would make the contract accept a shape no producer emits. Note
   the units differ — `maxLength` counts code points, `printableASCII` counts
   bytes — and they agree only because the ASCII pattern leaves no rune wider than
   one byte. A field taking one bound without the other does not inherit that
   equivalence.
