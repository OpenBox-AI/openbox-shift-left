# ADR-0019 — Full content capture, under one org gate

Status: **Proposed.** Date: 2026-08-13.

Would retire **SL3-SEC-3** ("tool commands and file bodies never egress on
observe events") and restate **INV-2**. Amends **ADR-0014**'s transcript
allowlist (P3). Builds on **ADR-0018**, which is this posture's first shipped
increment.

**Nothing in this ADR is implemented.** It authorizes three later phases; each
needs its own plan after this is accepted.

## Context

The developer runtime's content posture has been decided piecemeal, and the
pieces no longer add up to a stance:

| Class | Today | Decided by |
|---|---|---|
| Prompt text | egresses, by default | the 2026-07-15 flip |
| Shell command / file body | egresses on a **gated** call only | ADR-0017 |
| Assistant final text | egresses, by default | ADR-0018 |
| Tool output | never | never decided — inherited |
| Thinking | never | never decided — inherited |
| Failure detail (`reason`, `error`, `error_details`) | never | deferred by ADR-0018 |
| Tool input on the **observe** path | never | SL3-SEC-3 |

The last four are not a posture. They are what nobody has needed yet, and each
new feature has argued its own case from scratch — which is how "metadata-only"
came to be repeated in documentation long after it stopped describing the
product.

**Owner decision, 2026-08-13** (recorded in
`plans/reports/research-260813-2215-session-content-capture-gaps.md` §Decision
update): OpenBox is runtime governance an **organization** deploys over its own
developers — company machine, company code, company policy. Collect as much as
the surfaces allow, so evaluation has complete input.

### Arguing against the original rationale, not around it

The metadata-only posture was not arbitrary, and it deserves its own argument
rather than quiet obsolescence. It rested on: *the client should not be the thing
that leaks a developer's work, so the safest default is to send nothing that
could be one.*

What that reasoning gets right is that a governance product which leaks is worse
than no governance product. What it gets wrong is **who the counterparty is**.
The threat model it implies — protecting the developer from their employer's
governance tool — is not the deployment. The org owns the machine, the code and
the policy; the developer's own transcript already holds every one of these
fields in plaintext on that machine (the same trust boundary the signing key
lives in — ADR-0015). Withholding from the control plane what is already sitting
unprotected on disk does not protect anyone. It just makes the evaluation worse.

And the evaluation is the product. Policy `/evaluate`, Guardrails stages 0 and 1,
and goal alignment are all content-consuming, and each has been shipped in a
degraded form because the content was withheld: file bodies were unavailable to
policy until ADR-0017, and alignment scored nothing at all until ADR-0018.

The honest summary is that metadata-only optimized for a threat this deployment
does not have, at the cost of the capability it does.

## Decision

**Capture every content class the provider surfaces expose, by default, under the
existing `content_capture` gate.**

### One gate, not per-class flags

`content_capture` governs all of it. No new config keys. The alternative — a flag
per class — was rejected for now: it multiplies the posture space an org has to
reason about (and the combinations this repo has to test) to serve a use case
nobody has asked for. **Revisit on a customer ask**, and note that the per-class
shape is a superset, so starting with one gate forecloses nothing.

### Invariants restated

**INV-2 v2.** From "content is stripped before egress unless content-capture is
enabled" to:

> Content egresses only under the org's `content_capture` gate; every content
> class is scanned for secrets and redacted **before** it is attached to an
> event, and capped at 64KB before signing. `stripContent` at the client remains
> the single choke point, and no class bypasses it.

**SL3-SEC-3 is retired by name.** "Tool commands and file bodies never egress on
observe events" becomes false the moment P1 ships. It must be retired
deliberately — the conformance cases that pin it (C19's observe-path assertions)
are **updated, not deleted**, to assert the gate rather than the absence.

**What does NOT change:** event identity (`activity_id`, `event_id`, approval
keys — the pin tests stay untouched), the 64KB cap, fail-open, INV-3's
stdout-forbidden discipline, and the rule that the client never derives cost.

### Ordering is the load-bearing control

Every class, without exception: **detect → redact → attach → cap → sign.** A
redaction applied after attachment satisfies every unit test and still leaks;
ADR-0018's C26 is the pattern, asserting on the bytes POSTed rather than on any
intermediate. **A conformance case per class must merge before the flush code
that carries it.**

## Sequencing

Three phases, ordered by how much they cost if wrong.

### P1 — tool content

- `activity_input`: the command, MCP arguments and file body on the **observe**
  path, not only the gated `/evaluate` copy;
- `activity_output.output`: `PostToolUse.tool_response` text, plus the structured
  `tool_output`;
- the free-text failure detail ADR-0018 deferred: `PostToolUseFailure.error`,
  `PermissionDenied.reason`, `StopFailure.error_details`.

This is the largest volume increase in the whole posture — tool output at
tool-call cadence, not turn cadence — and it retires SL3-SEC-3. It goes first
because it needs no amendment to any other ADR.

### P2 — the assistant turn, properly

- move the final text from ADR-0018's span to `activity_output.message`, **once
  [openbox-core#130](https://github.com/OpenBox-AI/openbox-core/issues/130)
  lands**, and delete the span with its synthesized `http.*` attributes;
- `stop_reason` — **if it exists.** ADR-0018's probe found no such field on
  Claude Code 2.1.229's `Stop` payload. Do not plan work around a field that has
  to be re-verified first.

P2 is where ADR-0018's stopgap ends. Until #130 lands, P2 cannot ship: deleting
the span first kills alignment silently.

### P3 — thinking, and the allowlist amendment

**Implemented 2026-08-25** (contract v1.4). Scope landed narrower than proposed,
deliberately: **thinking only.** Intermediate assistant text was NOT bound — the
final reply already egresses from the hook field ADR-0018 bound, and a second
text source would have widened the allowlist twice for one reader.

- thinking blocks from the transcript window → `activity_output.thinking` on
  `TurnCompleted`, gated / redacted / capped;
- the **ADR-0014 amendment** that permits them, written before the binding merged
  and carrying the mechanism table, the scope limit, and the cost;
- ~~intermediate assistant text~~ — not bound. Still open, still needs its own
  amendment.

Staged last, deliberately. ADR-0014 replaced a structural impossibility with a
curated allowlist enforced by one test, and CLAUDE.md names that test
load-bearing. P3 makes it enforce a different property:

> `TestFinops_NoContentOnWire` evolves from *content is absent* to *content is
> present, redacted, and capped*. **A version that passes trivially is a
> defect** — the same rule ADR-0014 set, carried forward rather than dropped
> because the assertion changed direction.

Thinking is also the class where the provider itself is most conservative: Claude
Code's own OTel export redacts thinking unconditionally, with every content flag
enabled. Capturing it locally means going further than the provider will. That is
the org's call to make — it is the org's machine and the text is already in
plaintext on it — but it should be a decision someone made, not a consequence of
"capture everything".

## What full capture still cannot get

Mandatory, because "full" invites the wrong inference:

- **System prompts and raw API request bodies.** Neither is on any hook payload
  or in the transcript. The only source is the provider's own OTel export, a
  different pipeline with no coupling to the `/evaluate` path — a complementary
  collector, not something this client can reach.
- **Per-model-call granularity.** Hooks fire per turn; a turn contains several
  model calls. Everything here is a sum or a concatenation over the turn.
- **Provider-side redactions.** What the provider strips before writing is gone
  before this client sees it.
- **Codex parity.** No failure hook, no assistant-text field, per-session usage
  granularity, and its tool-output surface is unsurveyed. Codex sessions will
  capture materially less than Claude Code ones, and the gap should be stated in
  `COVERAGE.md` rather than discovered from a thin dashboard.
- **Guardrail redaction at source is still not wired.** Unchanged by this ADR,
  and the stakes rise with every class added: local secret detection is
  pattern- and entropy-based, and it is all there is.

## Consequences

- **Documentation rewrite, file by file.** `docs/data-and-privacy.md` (its
  "never" rows become gated rows), `docs/MAPPING.md` §1/§3,
  `COVERAGE.md` §2/§3, `README.md`, `CLAUDE.md`. Each must read as a widening
  with its bounds, never as an improvement.
- **A storage and retention ask for the backend.** 64KB-class payloads at
  tool-call cadence is a different order of volume from today, and retention and
  PII posture for the new classes is a backend decision this ADR does not make.
- **`secret_detection: false` becomes far more consequential.** It is the only
  in-transit control, and it would now expose every class rather than two.
- **The server-side dedupe ask is unchanged** and its cost rises: a
  double-stored event under the lost-200 window now duplicates content, not
  metadata.
- **Approval identity is untouched.** Still its own decision (CLAUDE.md).

## Alternatives considered

**Stay metadata-only.** Rejected by the owner decision above. It also is not the
status quo it sounds like: prompt text, gated bodies and assistant text already
egress, so "metadata-only" describes a product that no longer exists.

**Per-class flags now.** Rejected for KISS, revisitable on a customer ask.
Starting single-gate forecloses nothing.

**OTel collector only** — take content from the provider's own telemetry export
instead. Rejected: a different pipeline, no coupling to the `/evaluate` decision
path, and it cannot inform a synchronous gate. Worth having *as well*, for the
system prompts nothing else can reach.

**Capture everything at once, in one phase.** Rejected: P3 requires amending a
load-bearing test, and bundling that with the volume change of P1 would make one
review carry both risks.

## Acceptance

This ADR stays **Proposed** until the owner accepts it. On acceptance, P1 gets
its own plan; nothing here authorizes an implementation commit.
