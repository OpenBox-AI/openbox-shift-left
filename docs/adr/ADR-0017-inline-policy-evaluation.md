# ADR-0017 — Inline policy evaluation: `/evaluate` is the only decider

Status: Accepted. Date: 2026-08-13.

Retires: **ADR-0005 Decision 1 and Decision 2**, and **INV-3b clause 3**
(ADR-0002 §Decision.3). Leaves ADR-0006 intact. Makes ADR-0008 unnecessary for
the developer runtime.

## Context

The enforce gate needs a verdict from org policy before a tool runs. Since
ADR-0005 that verdict has come from a Go reimplementation of the backend's
policy semantics, evaluated against a bundle pulled ahead of time.

ADR-0005 chose that deliberately, and its reasoning was sound for what it
optimized:

> **Decision 1 — evaluate the builder config natively.** The engine implements
> the builder's semantics in pure Go: ordered rules, first match wins, default
> allow. No OPA, no cgo, no rego runtime in a hook that runs on every tool call.

It also named the obligation it was taking on, in the same breath:

> The obligation this creates is parity: the local evaluator must agree with what
> the backend's OPA would decide for the same input. […] their known deviations —
> composite ordering, in particular — are documented rather than assumed away.

That obligation is the problem. It is **permanent and unbounded**: it does not
discharge when the deviations are fixed, because it is re-incurred by every
policy feature the backend ever ships. Two Go primitives must keep agreeing with
an OPA evaluator maintained in another repo, on semantics neither side owns
jointly. The deviations are documented, as promised — which is honest, and also
an admission that the two evaluators already disagree.

The obligation is not merely leaking; in one case it was never met at all.
Hand-written rego has no builder config to reimplement, so `policysync.go`
installs a bundle marked `RawRegoUnlocalized` and warns:

> it cannot be evaluated locally and the decider will serve it fail-open (allow)
> locally

An org that writes raw rego is therefore **not enforced** on the developer
runtime. Its gates open. The warning is printed once, at sync time, to whoever
ran the command.

Meanwhile the repo's stated principle is "reuse, don't rebuild" — same endpoint,
same auth, same tables as the agent runtime. Enforcement is the one place the
developer runtime forked and built a second implementation of something the
platform already has.

## Decision

**Every gated tool call is evaluated inline by `/evaluate`, and that verdict is
applied.** There is one policy implementation in the product, on the server.

The local policy path is deleted: the evaluator, the rego-parity primitives, the
builder, the bundle, its signature check, `policysync`, `openbox dev sync`, and
the session-start staleness gate. **Local secret redaction survives** — see
below; it is content protection, not policy evaluation.

### Failure and timing

- **Unreachable ⇒ the org's `fail_closed` decides.** Default fail-open. It is a
  machine-wide setting in `dev.json`, hand-editable, and org-lockable via managed
  config. It gets no `init` flag: choosing it is a governance decision, not an
  install option.
- **Slow but reachable ⇒ wait for the real verdict**, bounded by the provider's
  hook ceiling, then apply `fail_closed`. A guessed verdict is never substituted
  for a real one that is merely late.
- **Latency and capacity are the platform's scope.** No client-side caps are
  tuned here, no caching, no decision reuse, no measurement gate. A control plane
  that cannot answer a synchronous gate fast enough is a platform problem with a
  platform fix, and hiding it behind a client-side cache would remove the signal
  that says so.

  One client-side boundary remains, and it is not negotiable: **the hook must
  write a verdict before the provider's hook ceiling.** A hook killed mid-flight
  fails open uncontrollably — the tool simply runs — which would defeat a
  fail-closed org silently. The ceiling is 30s on PreToolUse for both adapters
  (`adapters/claude-code/enforce_tier2.go`, `adapters/codex/installer.go`), and
  the whole-hook budget derives from it.

### Content

**Content attaches for all gated classes**, gated as today on `content_capture`.
This is a change in what leaves the machine and it gets its own section below.

**The body is locally redacted first.** Tier-1 secret detection runs before the
payload is built, so core receives the redacted body. `decision/secrets.go`
therefore survives the deletion, for two reasons: it is content *protection*
rather than policy evaluation, and it sees the whole body where core sees at most
`maxBodySize` = 64KB (`client/payload.go:676`).

## What this weakens

Two properties get worse. Both are real and neither is mitigated away.

**1. Enforcement now depends on network reachability, and under the default
fail-open it is bypassable.** Blocking one hostname disables enforcement for that
developer. Today a cached bundle keeps deciding when the network is gone; after
this change there is nothing local to decide with. An org that needs enforcement
to survive a hostile developer must set `fail_closed` — and accept that a control
plane outage then blocks work.

This is the single most important consequence in this document. It is not offset
by anything below.

**2. The server's view of a large write is capped at 64KB.** `capBody` truncates
before egress, so content-based policy matches against at most the first 64KB of
a large file body. A rule that would match at byte 70,000 does not fire.
Content-based policy is therefore not a complete check on large writes. Raising
the cap is a separate decision, not made here.

### Content egress changes

Under enforcement with `content_capture` on, **Write and Edit file bodies now
leave the machine.** They did not before: only high-risk classes escalated, and
those are shell and MCP calls.

Nobody should be able to read this ADR and conclude that file bodies stay local.
They do not. What bounds the exposure:

- It is gated on `content_capture`, which an org can turn off
  (`content_capture:false` / `OPENBOX_CONTENT_CAPTURE=0`). With it off, no body
  egresses and the server evaluates metadata axes only.
- Bodies are redacted for known secrets locally first (above).
- Bodies are capped at 64KB.

Guardrail redaction at source is still not wired, so redaction is limited to what
Tier-1 secret detection recognizes. Orgs already running `content_capture:true`
will see a category of content they were not previously sending; telling them is
a product decision, not an engineering one.

## What this strengthens

- **Raw-rego orgs go from silently ungoverned to actually enforced.** This is the
  honest headline: the change closes a fail-open hole that no amount of local
  evaluator work could have closed.
- **One policy semantics.** Every backend policy feature works on the developer
  runtime the day it ships, with no parity work and no second implementation to
  keep in step.
- **The Cursor adapter inherits enforcement for free** — no bundle plumbing to
  port, because there is no bundle.
- **ADR-0008's bundle signing becomes unnecessary here.** It was blocked on the
  backend signing anything (`require_verified_bundle` defaults off because
  nothing signs), and there is now no bundle to verify.

## What is retired, by name

**ADR-0005 Decision 1** (native evaluation) and **Decision 2** (pull at init,
check staleness) are retired. The parity obligation they created is discharged by
deletion: there is no second evaluator to keep in agreement.

**INV-3b clause 3 is retired** — ADR-0002 §Decision.3, "In-process, no I/O on the
decision path. The verdict comes from a local policy bundle evaluated in memory.
No network call, no IPC, nothing to be down."

That clause cannot coexist with inline evaluation, and this is a deliberate
reversal rather than a violation to be fixed later. **Do not restore it.** The
rest of INV-3b stands and still binds:

| Clause | Status |
|---|---|
| 1. Opt-in | superseded by ADR-0016 (enforce on by default), not by this ADR |
| 2. Pre-execution and synchronous | **holds** |
| 3. In-process, no I/O on the decision path | **retired by this ADR** |
| 4. Fail-open on every fault | **holds** — now including "unreachable", per `fail_closed` |
| 5. Tighten-only | **holds** |

**ADR-0006 is untouched.** Its decision was "no socket, no sidecar at all" —
nothing resident, nothing to start, nothing to leave running. A bounded outbound
HTTPS call from a hook is not a daemon. The developer still installs a binary and
runs no service.

## Alternatives rejected

**Keep the local evaluator.** Rejected because the parity obligation is permanent
and re-incurred by every backend policy feature, and because it cannot be made to
cover raw rego at all. Keeping it also keeps two evaluators that already
disagree, with the disagreements documented rather than resolved. The honest form
of this alternative is "accept that raw-rego orgs are unenforced," which is not
acceptable for a governance product.

**Narrow inline evaluation to high-risk classes only** — the cheapest version of
the same win. Rejected: it keeps the entire local policy path alive to decide
everything else, so the parity obligation, the bundle, the sync and the staleness
machinery all remain. It buys the correctness win for shell and MCP while paying
the full maintenance cost, and it leaves Write/Edit decided by the evaluator whose
deviations are known.

**A policy-declared scope manifest** — let policy tell the client which calls need
a round-trip. Rejected: it reintroduces exactly what this ADR removes, a
server-authored artifact the client must fetch, cache, pin and check for
staleness. It is bundle sync with a smaller payload.

## Evidence and its limits

The two empirical questions that could have invalidated this direction were
investigated first
(`plans/260813-0140-inline-policy-evaluation/reports/finding-260813-dedupe-and-ceilings.md`):

- **Codex's hook ceiling is 30s**, written by our own installer under Codex's
  600s default, and scaling automatically if an org raises it. It is self-imposed,
  not provider-imposed, so Codex has more headroom than Claude Code rather than
  less. Enforcement on Codex is not constrained by it.
- **Core does not dedupe developer events on their id.** The duplicate-suppression
  that keeps one gated call to one `ActivityStarted` is entirely client-side, at
  the gate, keyed on transport delivery. It is class-independent, so widening
  escalation to every gated call does not change it. A data race on that flag was
  found and fixed while investigating (`eb53827`).
- **The suppression is narrowed, not closed.** If core commits the row after our
  client has given up waiting, the observe copy is spooled anyway and the call is
  stored twice. That window is irreducible client-side; closing it needs
  server-side dedupe. Under this ADR it applies to every gated call rather than
  to shell and MCP only.

**Limit on all of the above:** it was established against the code and with
targeted race tests, **not** against a live stack. The real per-call row count
under universal escalation has not been observed. This ADR is accepted knowing
that.

## Consequences

- `/evaluate` is on the blocking path of every gated tool call. Its availability
  and latency are now developer-visible in a way they were not.
- Telemetry stays spooled and asynchronous. Approvals, lineage and usage capture
  are unchanged.
- The deleted surface is roughly 2,200 non-test lines. That is a consequence of
  the decision, not a reason for it.
