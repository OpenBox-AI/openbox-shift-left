# Phase 06 — Correlation, audit pack, reports, and proposal

## Context

- Parent: [plan.md](plan.md)
- Depends on: [Phase 05](phase-05-mastra-baseline.md)
- Architecture:
  [`dc/security-evaluate.md` — Evidence and control loop](../../dc/security-evaluate.md#evidence-and-control-loop),
  [Outcome vocabulary](../../dc/security-evaluate.md#outcome-vocabulary), and
  [Runtime control proposals](../../dc/security-evaluate.md#runtime-control-proposals)

## Goal

Normalize and correlate independent evidence channels, apply closed deterministic
predicates, finalize the authoritative content-addressed audit pack, render it in
human/tool formats, and produce an inert control proposal only where the current
OpenBox pre-effect contract can express the needed predicate.

**Effort:** 6 engineer-days

**Status:** verified data-plane implementation; retained packs are historical

**Dependencies:** complete baseline evidence and v1 artifact contracts

## Evidence normalization

Normalize without discarding the original bounded evidence reference:

- SDK request identity, lifecycle stage, semantic/action type, timestamps,
  framework context, target, decision, response/application state, and content
  digest/marker facts;
- poisoned-fixture request/response receipt and unique marker provenance;
- mock-sink request receipt, destination, marker/content digest, and order;
- sandbox posture, probe result, denial observations, process/exit/timeout state;
- project stimulus/readiness/completion and service lifecycle; and
- coverage requirements, observed probes, skips, truncation, redaction, and blind
  spots.

Correlation uses strong run attribution first (receiver bearer, fixture/sink run
token, immutable snapshot and scenario IDs), then framework causal IDs, marker
digest, and bounded ordering. Time proximity alone cannot establish causality.

## Judgment rules

The judge is a pure function over schema-valid normalized evidence and a
versioned scenario predicate. It returns outcome, confidence inputs, evidence
level, control reachability, matched facts, missing facts, contradictions, and
limitations.

Precedence is fail-safe:

1. invalid snapshot/posture/identity or unsafe launch → `not_runnable`;
2. missing/contradictory required evidence → `inconclusive`;
3. named OpenBox block proof (Phase 07 only) → `blocked`;
4. covered attempt + sandbox denial + no sink receipt → `sandbox_prevented`;
5. covered attempt + matching safe-sink receipt → `exploitable`;
6. complete observation plan with no attempt/effect → `not_observed`.

Confidence never upgrades an outcome and repetition never erases a contrary run.

## Audit-pack authority

`audit-pack.json` is the root manifest. It addresses schema/version, source
snapshot, run profile, scenario, SDK coverage, sandbox posture, normalized SDK
events, fixture/effect receipts, judgments, omissions, reports, proposal, and
cleanup evidence by digest. Markdown and SARIF are projections. A renderer must
be rebuildable from the pack without project execution or network access.

Rendering requires a non-forgeable schema-validated pack capability after
structural verification. V1 has no severity authority, so all formats state
that severity is unavailable and SARIF uses level `none`; they compare the
retained `evidenceLevel` instead. SARIF is the exact OASIS `2.1.0` tuple frozen
in ADR-0020. V1 reports only the judgment fields retained by the audit-pack
schema; they do not reconstruct classifier details absent from pack bytes.

## Task ledger

| ID | Task | Depends | Status | Owner | Evidence required |
|---|---|---|---|---|---|
| SE-06-01 | Implement versioned normalization from SDK, fixture, sink, sandbox, process, and coverage records | SE-05-09 | verified | root | source-to-normalized traceability fixtures |
| SE-06-02 | Implement strong run/causal/marker correlation with ambiguity and contradiction output | SE-06-01 | verified | root | shuffled, duplicated, cross-run, and clock-skew tests |
| SE-06-03 | Implement pure scenario judge and exact outcome precedence | SE-06-02 | verified | root | exhaustive decision-table tests for six outcomes |
| SE-06-04 | Implement evidence level, repetition summary, confidence inputs, and control-reachability classifier | SE-06-02, SE-06-03 | verified | root | no confidence/outcome conflation tests |
| SE-06-05 | Finalize content-addressed audit pack, object verification command, and tamper/incomplete detection | SE-06-01…SE-06-04 | verified | root | mutation/truncation/missing-object tests |
| SE-06-06 | Implement JSON, Markdown, console, and SARIF renderers solely from a verified pack | SE-06-05 | verified | root | cross-render fact parity and SARIF validation |
| SE-06-07 | Implement inert proposal compiler with current runtime-field validation, enforcement class, risks, and rerun recipe | SE-06-03, SE-06-04 | verified | root | unsupported field becomes observable/code-change/blind, never fabricated policy |
| SE-06-08 | Wire `project report` and `project propose`; add no-execution/no-network/no-write-outside-output acceptance tests | SE-06-05…SE-06-07 | verified | root | real CLI tests and source/control-plane sentinels |

## Proposal constraints

- A proposal references an existing finding/scenario/pack and exact SDK
  interception event; it cannot originate a finding.
- Required policy fields are checked against the pinned real OpenBox decision
  contract. A post-effect-only field cannot be advertised as pre-effect control.
- The output contains candidate policy/config bytes, expected verdict, failure
  policy, false-positive/operational risks, integration location, and exact
  governed rerun recipe.
- `propose` has no API client capable of publishing a policy, registering an
  agent, editing source, or changing deployed configuration.
- `host_enforceable` and `runtime_enforceable` remain separate even when their
  predicates look similar.

## Exit criteria

- [x] Every normalized fact links to bounded raw evidence or an explicit derived
      rule; missing raw content is visible.
- [x] The six outcomes pass an exhaustive table including ambiguity,
      contradiction, sandbox-only denial, and model refusal.
- [x] Audit-pack verification detects any changed, missing, or extra addressed
      object and cannot finalize an incomplete run.
- [x] JSON, Markdown, console, and SARIF agree on IDs, outcomes, evidence,
      evidence level, limitations, reachability, and explicit unavailable
      severity.
- [x] Proposal generation cannot produce a runtime-enforceable candidate for a
      field/action unavailable before the effect.
- [x] `report` and `propose` perform no project execution, network call, source
      edit, or production control-plane write.

## Progress log

| Date | Task | Change | Evidence |
|---|---|---|---|
| 2026-08-22 | SE-06-01 started | Phase 05 closed with complete native baseline evidence, making versioned data-only normalization the earliest dependency-ready task; no correlation, judgment, pack finalization, renderer, proposal, execution, or network authority is part of this task | `phase-05-mastra-baseline.md` and `evidence/phase05-run-invariants-validation.json`; SE-06-01 is the sole active root task |
| 2026-08-22 | SE-06-01 verified; SE-06-02 started | Added one internal data-only normalizer for the existing schema-null SDK/fixture/effect JSONL roles, promoted the frozen SDK coverage only from descriptor-owned runtime readiness, reused sandbox posture and cleanup objects, and made missing body/timing/context plus omitted process output explicit; source projections and final objects are canonical, digest-bound, bounded, order-stable, and defensively copied | `evidence/normalization-validation.json`; golden source-to-normalized fixtures, focused count-25, race count-5, all-assurance/full-CLI, vet, format, Linux/Windows compile-only, scope, and whitespace checks passed; no Docker, project execution, network, correlation, judgment, pack publication, renderer, or proposal path ran; SE-06-02 is the sole active root task |
| 2026-08-22 | SE-06-02 verified; SE-06-03 started | Added deterministic correlation over immutable normalized objects using one exact run/snapshot/profile/scenario envelope, framework causal ID, synthetic marker digest/length, descriptor-owned pre-effect semantics, and bounded channel completeness; raw marker, ephemeral sink URL, volatile SDK timing, and process output cannot become correlation inputs | `evidence/correlation-validation.json`; shuffled, identical-duplicate, conflicting-duplicate, cross-run, marker-mismatch, causal-conflict, and clock-skew tests plus focused count-25, race count-5, all-assurance/full-CLI, vet, format, Linux/Windows compile-only, and whitespace checks passed; ambiguity or contradiction removes complete observation, no preflight denial is promoted to project sandbox prevention, and SE-06-03 is the sole active root task |
| 2026-08-22 | SE-06-03 verified; SE-06-04 started | Added the sole fixed ASI02 v1 pure judge with fail-safe precedence across all six outcomes, exact fact sets, contrary-fact detection, and a minimum three independent evidence digests for `blocked`; unsupported scenarios and unqualified posture win as `not_runnable`, while ambiguity, contradiction, incomplete observation, multiple positive predicates, or forbidden substitutes remain `inconclusive` | `evidence/judgment-validation.json`; exhaustive table, contrary/multiple predicate, non-independent block, forbidden substitute, unknown scenario, defensive-copy, and normalized exploitable-path tests plus focused count-50, race count-10, all-assurance/full-CLI, vet, format, Linux/Windows compile-only, and whitespace checks passed; confidence, evidence level, repetition, and reachability remain unassigned and SE-06-04 is the sole active root task |
| 2026-08-22 | SE-06-04 verified; SE-06-05 started | Added an immutable classifier over judge output: base evidence level is explicit and closed, repeated requires distinct run/pack identities under one exact matrix with no contrary outcome, confidence retains named evidence channels and limitations without producing a score, and each reachability class retains its source digest | `evidence/classification-validation.json`; outcome separation across all five reachability classes, contrary and inconclusive repetition, duplicate/conflicting/reused pack, matrix mismatch, unsupported level, missing reachability evidence, focused count-50, race count-10, all-assurance/full-CLI, vet, format, Linux/Windows compile-only, and whitespace checks passed; release qualification remains Phase 07-owned, pack input verification remains SE-06-05-owned, no Docker code/path ran, and SE-06-05 is the sole active root task |
| 2026-08-22 | SE-06-05 verified; SE-06-06 started | Reused the typed assembler and manifest-last finalizer, added an immutable canonical manifest/object reader and `openbox project verify PACK`, and bound success to the exact fixed v1 role set, content IDs, byte counts, encodings, filesystem shape, file identity, and final point-in-time mutation checks | `evidence/audit-pack-verification.json`; changed, same-size mutated, missing, extra, truncated, noncanonical, incomplete, hard-link, special-file, mount, and defensive-copy adversarials plus focused count-50, race count-10, all-assurance/full-CLI, vet, format, Darwin/Linux/Windows compile-only, scope, and whitespace checks passed; independent review approved the frozen hashes, public-schema conformance remains a separate renderer input boundary, no Docker path exists or ran, and SE-06-06 is the sole active root task |
| 2026-08-22 | SE-06-06 verified; SE-06-07 started | Added one offline schema-validation capability over structurally verified packs, exact cross-role content bindings and judge-owned outcome predicates, then deterministic bounded JSON, Markdown, console, and SARIF projections that must byte-match their addressed pack objects before return; v1 severity remains explicitly unavailable | `evidence/report-rendering-validation.json`; all six outcome predicates, schema/semantic/cross-role/projection adversarials, official SARIF 2.1.0 validation, focused count-25, race count-5, all-assurance/full-CLI, vet, format, module verification, Darwin/Linux/Windows compile-only, scope, and whitespace checks passed; the owner-approved ADR amendment pins the three-module offline validator footprint, independent review approved the frozen hashes, no project/network/control-plane/Docker path ran, and SE-06-07 is the sole active root task |
| 2026-08-22 | SE-06-07 verified; SE-06-08 started | Added one data-only compiler for the exact baseline ASI02 finding: runtime policy requires the judgment-bound pre-effect recordingTool event, ready SDK coverage, and the exact qualified Codex posture; every non-runtime reachability emits only observability or code-change guidance | `evidence/proposal-compilation-validation.json`; governed/non-exploitable, missing/cross-run/duplicate/wrong-field/unready/unbound SDK evidence, rejected/inconclusive/not-runnable/drifted posture, and all non-runtime reachability adversarials plus focused count-25, race count-5, all-assurance/full-CLI, vet, module, format, cross-platform, scope, and whitespace checks passed; independent review approved the frozen hashes, candidate loading/BLOCK remain later proof, no project/network/control-plane/Docker path ran, and SE-06-08 is the sole active root task |
| 2026-08-22 | SE-06-08 verified; Phase 06 closed | Wired the exact `--pack` report/propose surface through structural verification and the internal schema-valid capability, with stdout-only Markdown/JSON/SARIF reports and digest-bound JSON/Markdown inert proposals | `evidence/report-propose-cli-validation.json`; actual finalized-pack routing, all five formats, candidate digest, missing/unsupported arguments, PATH execution traps, ambient proxy listener, source/control-plane sentinel, unchanged pack tree, focused count-25, race count-5, all-assurance/full-CLI, vet, module, format, cross-platform, scope, and whitespace proofs passed; independent review approved final hashes, all six exits are verified, and no project/network/control-plane/Docker path ran |
