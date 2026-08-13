# Phase 02 — ADR-0017 + the assurance limits it changes

## Context links

- Parent: [plan.md](plan.md) · Depends on: phase 1 (its two numbers go in the ADR)
- **Blocks every code phase** — it authorizes reversing two accepted ADRs and an invariant.
- Reversing: **ADR-0005** Decision 1 (native evaluator) + Decision 2 (pull at init, check
  staleness), and **INV-3b** ("a hook does no network I/O to decide").
- Prior ADRs: `ADR-0011..0016` (0015/0016 come from the auth/init plan) ⇒ this is **0017**.

## Overview

- **Date:** 2026-08-13
- **Description:** One ADR that argues against the real prior reasoning, records what this
  weakens, and states the two limits a user must be able to find. Full user-doc rewrite is
  phase 7; this phase writes only the ADR and the assurance-limits entries, so no code
  lands before the reasoning exists.
- **Priority:** P1 · **Implementation status:** done · **Review status:** pending

## Key Insights

- **ADR-0005 must be quoted, not paraphrased.** It chose local evaluation deliberately:
  "No OPA, no cgo, no rego runtime in a hook that runs on every tool call," and it named
  the obligation it created — "the local evaluator must agree with what the backend's OPA
  would decide for the same input," with known deviations documented. The reversal's case
  is that this obligation is permanent and unbounded, and that raw rego escapes it
  entirely (`policysync.go:149`).
- **INV-3b is retired, not violated.** An invariant that a hook does no network I/O to
  decide cannot coexist with inline evaluation. Say it is retired and why, so nobody
  later "restores" it as a bug fix.
- **Lead with the structural argument, not the line count.** The strongest case is that
  enforcement was the one place the developer runtime forked from the agent runtime;
  collapsing it gives one policy semantics, and the Cursor adapter inherits enforcement
  for free. LOC deleted is a consequence.
- **Two things get weaker and both must be stated plainly.** (a) Enforcement becomes
  bypassable by blocking one host under the default fail-open — today a cached bundle keeps
  deciding. (b) Write/Edit file bodies begin egressing when content capture is on (E7).
- **One thing gets stronger, and it is the honest headline:** raw-rego orgs go from
  silently ungoverned to actually enforced.
- Latency is out of scope by decision (E6) and the ADR should say so explicitly — otherwise
  the absence of a budget discussion reads as an oversight rather than a boundary.

## Requirements

1. `docs/adr/ADR-0017-inline-policy-evaluation.md`: Status accepted, Date, Context quoting
   ADR-0005's actual rationale, Decision, Consequences, Alternatives rejected, and a
   "What this weakens" section.
2. Alternatives rejected, each with the reason: keep-local-evaluator; narrow inline scope
   to high-risk only (cheapest version of the same win); policy-declared scope manifest
   (rejected — retains a sync).
3. Record E4 semantics: unreachable ⇒ `fail_closed` decides, default fail-open,
   machine-wide, hand-editable, org-lockable, no `init` flag.
4. Record E6: latency and capacity are the platform's scope; no client caps are tuned here.
   Name the one client-side boundary that remains — the hook must write a verdict before
   the provider's ceiling, or the platform fails open uncontrollably.
5. Record E7 + E8: content attaches for all gated classes, content_capture-gated, and the
   body is **locally redacted first**. State the 64KB `capBody` evaluation cap as a limit.
6. Record the phase-1 findings: observed dedupe behaviour at 100% escalation, and Codex's
   ceiling with its source.
7. Record that `decision/secrets.go` survives, and why: it is content protection, not
   policy evaluation, and it sees the whole body where core sees at most 64KB.
8. `docs/adr/README.md` index updated.
9. `docs/architecture.md#assurance--what-the-evidence-proves` gains two limits: enforcement
   depends on reachability (and is bypassable under fail-open), and content-based policy
   sees a capped view of large writes.

## Architecture

No code. One ADR plus two entries in the existing assurance-limits list. Everything
user-facing waits for phase 7, when the shipped behaviour is known.

## Related code files

| Path | Why |
|---|---|
| `docs/adr/ADR-0005-native-policy-evaluator.md` | the decision being reversed; quote it |
| `docs/adr/ADR-0006-in-process-decider.md` | INV-3b's origin; note that "no sidecar" survives — an HTTP call is not a daemon |
| `cli/internal/policysync/policysync.go:127-149` | raw-rego fail-open, the honest headline |
| `client/payload.go:480-522,676-692` | what egresses, the content gate, the 64KB cap |
| `adapters/claude-code/enforce_tier2.go:36-42` | the budget guard that must survive |

## Implementation Steps

1. Read ADR-0005 and ADR-0006 in full; quote the exact prior rationale in Context.
2. Draft ADR-0017 leading with the structural argument, then the parity obligation, then
   raw rego. LOC last.
3. Write "What this weakens": reachability-dependent enforcement, one-line bypass under
   fail-open, file bodies egressing, capped content view.
4. Write "What this strengthens": raw-rego orgs enforced, one policy semantics, Cursor
   inherits enforcement, ADR-0008 signing unnecessary here.
5. Explicitly retire ADR-0005 D1+D2 and INV-3b by name; note that ADR-0006's no-sidecar
   decision is untouched.
6. Add the two assurance limits to `docs/architecture.md`, matching the existing style.
7. Update `docs/adr/README.md`.

## Todo list

- [x] ADR-0017 written; ADR-0005's rationale quoted, not paraphrased
- [x] ADR-0005 D1+D2 and INV-3b retired by name; ADR-0006 explicitly untouched
- [x] Alternatives rejected recorded, incl. the narrow-scope variant
- [x] E4, E6, E7, E8 recorded; 64KB cap stated as a limit
- [x] Phase-1 findings cited in the ADR
- [x] `decision/secrets.go` survival and its reason recorded
- [x] Two assurance limits added to `architecture.md`
- [x] `docs/adr/README.md` updated

## Success Criteria

- A reader who disagrees with the reversal can find their own argument fairly stated in it.
- Searching `INV-3b` lands on a document that says it is retired and why.
- The bypass consequence is findable by someone auditing enforcement, not buried.
- Nobody can conclude from the ADR that file bodies never egress.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| ADR reads as a rationalization of a deletion | M×M | no Alternatives section, or ADR-0005 not quoted | **Adjust:** rewrite. An ADR that does not engage the prior decision is worse than none. |
| The bypass consequence is softened | M×H | a reviewer cannot tell that blocking one host disables enforcement under fail-open | **Stop and replan:** this is the overstatement `CLAUDE.md` forbids. |
| Content egress mentioned only in passing | M×H | grep for `content` in the ADR finds no dedicated section | **Adjust:** E7 changes a privacy property; it gets its own heading. |
| INV-3b left ambiguous | M×M | the invariant still reads as current somewhere | **Adjust:** retire it in one place, by name, and point every mention at ADR-0017. |

## Security Considerations

This phase's product is an honest account of two weakenings and one strengthening. Do not
soften: enforcement now depends on network reachability; under the default fail-open a
developer can disable it by blocking one hostname; file bodies leave the machine on
enforcement when content capture is on; and the server's view of a large write is capped
at 64KB, so content-based policy is not a complete check.

## Next steps

Phase 3 adds the ceiling capability and widens the gate to every class.
