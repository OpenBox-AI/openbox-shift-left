# Phase 08 — OpenBox Sandbox `ProjectRun` v2 decision track

## Context

- Parent: [plan.md](plan.md)
- Current sandbox checkout reviewed at
  `5e88a4548d391e7a0b6c9cb0154e06128f1d00fc`
- Current source constraints:
  `openbox-sandbox/src/runtime_contract/request.rs`,
  `openbox-sandbox/src/protocol/message.rs`,
  `openbox-sandbox/src/srt/mod.rs`, and
  `openbox-sandbox/src/lib.rs`
- Operational distinction:
  `openbox-sandbox/packaging/launcher/README.md`

## Goal

Decide whether and how OpenBox Sandbox should become a later, reproducible
project-execution backend. Do not weaken or overload its current strict
single-command v1 contract. This phase produces an ADR/threat model and a
separate implementation plan; it does not implement v2 under the Shift Left
MVP budget.

**Effort:** 5 engineer-days discovery

**Status:** verified

**Dependencies:** Phases 03–07 supply implemented interfaces and the historical
evidence contract; they do not supply a currently supported driver

## Current v1 fit

V1 has valuable properties: pinned native profiles, mTLS service boundary,
durable lifecycle ownership, prepare/commit execution, typed output limits,
cleanup, and provider/violation evidence. It is intentionally unsuitable for a
developer project today:

- creation has no environment, provider, GPU, secret, or host-mount fields;
- a fresh empty workspace is created for each request;
- execution accepts argv/timeout/output limits only, with fixed `/sandbox`,
  empty environment/stdin, no TTY, and a 300-second maximum;
- the service protocol has no project snapshot upload, dependency preparation,
  service topology, health/stimulus, or artifact retrieval operation; and
- the production client type is exported only under tests.

Reusing v1 by mounting a developer worktree, inheriting the host environment, or
smuggling setup through a shell command would erase the contract's main security
advantages. The correct reuse point is a versioned `ProjectRun` capability.

## Candidate v2 capability

The discovery must specify at least:

- content-addressed immutable snapshot upload/staging and source manifest;
- pinned runtime/template/image plus dependency/cache strategy;
- secret **references** and one-time scoped credentials, never ambient env copy;
- explicit non-secret environment allowlist;
- loopback receiver/fixture topology or authenticated reverse channels;
- model-provider egress relay and default-deny destination policy;
- multiprocess lifecycle, health/stimulus protocol, longer bounded sessions,
  cancellation, output/evidence streaming, and cleanup;
- artifact/receipt retrieval with digest verification;
- sandbox posture, child inheritance, denial and egress decision evidence; and
- a production client API available to Shift Left without linking test support.

The new request must be impossible for a v1 service to interpret as v1. Prefer a
new operation/media type and explicit capability negotiation over adding many
optional fields to `ExecRequest`.

## Task ledger

| ID | Task | Depends | Status | Owner | Evidence required |
|---|---|---|---|---|---|
| SE-08-01 | Freeze v1 behavior and threat assumptions with source/conformance references and current live-test limitations | — | verified | root | v1 contract inventory and passing/failing evidence ledger |
| SE-08-02 | Translate the qualified native-driver `RunSpec`, `Posture`, and evidence needs into provider-neutral ProjectRun requirements | SE-03-10, SE-07-09 | verified | root | traceability matrix with no host-specific leakage |
| SE-08-03 | Compare options: native-only, v1 wrapper, versioned v2 protocol, and separate project-run service | SE-08-01, SE-08-02 | verified | root | decision matrix for security, fidelity, effort, CI/remote use, operations |
| SE-08-04 | Draft v2 lifecycle/protocol/schema for snapshot, prepare, services, exec, evidence retrieval, and cleanup | SE-08-02, SE-08-03 | verified | root | state machine and compatibility tests on paper/prototype |
| SE-08-05 | Draft credential, network, model relay, dependency-cache, artifact, and retention threat model | SE-08-04 | verified | root | abuse cases and fail-closed responses |
| SE-08-06 | Prototype the minimum end-to-end ProjectRun against the Phase 05 fixture without changing v1 wire types | SE-08-04, SE-08-05 | not_applicable | root | owner excluded v2 implementation from this goal; prototype gate moves to the separate implementation plan |
| SE-08-07 | Define production client/export, conformance suite, provider matrix, migration, and rollout/rollback requirements | SE-08-04…SE-08-06 | verified | root | consumer/provider test matrix and operational ownership |
| SE-08-08 | Write the cross-repository ADR and separate implementation plan, or reject/defer v2 with explicit revisit triggers | SE-08-01…SE-08-07 | verified | root | accepted decision plus estimate/owners, or evidence-backed no-go |

## Decision criteria

Choose v2 only if it materially improves at least these properties over native
drivers: reproducible environment, durable cleanup, typed egress/denial evidence,
CI/remote execution, and cross-host policy stability. It must also preserve the
real SDK/fixture behavior and avoid copying the developer's mutable worktree or
ambient credentials.

Reject or defer if the design requires broad host mounts, generic secret/env
inheritance, unrestricted outbound network, unbounded project lifetimes, a shell
bootstrap loophole, or weaker evidence than the native driver. Operational cost
and developer setup are first-class criteria, not afterthoughts.

## Integration gate back into Shift Left

An `openbox-sandbox` driver may be added to `openbox project test` only after:

1. v2 is accepted and versioned in the sandbox repository;
2. a non-test production client is published;
3. snapshot/staging, service topology, scoped identity, and artifact retrieval
   pass cross-provider conformance;
4. the Phase 05 and governed Phase 07 scenarios pass through the same common
   Shift Left driver/evidence interfaces; and
5. no automatic fallback to a native host or raw process occurs.

## Exit criteria

- [x] Current v1 remains byte/behavior compatible and its strict request types
      are not widened under this plan.
- [x] A provider-neutral v2 design or explicit no-go decision addresses every
      ProjectRun requirement and threat.
- [x] The reuse decision compares measurable security/fidelity/operational
      benefits against native host sandboxes.
- [x] A prototype was not built under this discovery-only authority; SE-08-06
      is formally `not_applicable` and the disposable prototype gate is in the
      separate implementation plan.
- [x] Any implementation is moved to an accepted cross-repository plan with
      effort, owners, conformance, rollout, and rollback.

## Progress log

| Date | Task | Change | Evidence |
|---|---|---|---|
| 2026-08-22 | SE-08-01 started | Phase 07 closed with one exact supported native tuple and explicit unsupported rows. Phase 08 now freezes current OpenBox Sandbox v1 behavior and live-proof limits before any reuse decision | `evidence/release-validation.json`; the owner explicitly limits this phase to discovery, ADR, and a separate implementation plan, so no v2 code, protocol widening, prototype backend, or Shift Left driver is authorized |
| 2026-08-22 | SE-08-01 verified; SE-08-02 started | Froze the exact clean Sandbox commit, strict v1 create/exec/protocol/lifecycle/result surface, project-run gaps, threat assumptions, and truthful current test ledger without editing the sibling repository. V1 remains an empty-workspace, empty-environment, single-command contract | `evidence/openbox-sandbox-v1-inventory.json`; 86 library and 6 binary tests plus fmt/clippy passed, the real macOS lifecycle test passed, two live SRT denial-evidence assertions failed, and all three OpenShell tests explicitly skipped for an absent gateway tuple and are `not_runnable`, not passes. Rollup is 70/77; provider-neutral ProjectRun requirements are the sole active root task |
| 2026-08-22 | SE-08-02 verified; SE-08-03 started | Translated the proven native run, posture, run-profile, evidence, pack, and governed-cleanup boundaries into 15 provider-neutral requirements. The contract names immutable snapshot/preparation, scoped env/secrets, service topology, egress, one-shot probes, lifecycle, all budgets, evidence, artifacts, retention, failures, cleanup, and a production client without leaking host mechanics | `evidence/openbox-sandbox-projectrun-requirements.md`; every requirement maps both the native proof and the strict v1 primitive/gap, explicit invariants prevent driver/model/provider/fallback authority from entering the child, and the document contains no Seatbelt, `sandbox-exec`, `CODEX_HOME`, proxy-variable, or host-temp-path contract. Rollup is 71/77; the four-option reuse decision is the sole active root task |
| 2026-08-22 | SE-08-03 verified; SE-08-04 started | Compared native-only, a v1 wrapper, a versioned v2 capability, and a separate service across security, fidelity, CI/remote use, cross-host stability, effort, and operations. Selected versioned v2 as the later design direction, retained native-only as the current runner, rejected the v1 wrapper, and deferred a duplicate service | `evidence/openbox-sandbox-options.md`; the selected direction is conditional on all requirements, threat model, conformance, client, and rollback ownership and reverts to native-only if any cannot be met. No implementation/prototype/backend claim exists. Rollup is 72/77; the paper lifecycle/protocol/schema is the sole active root task |
| 2026-08-22 | SE-08-04 verified; SE-08-05 started | Drafted a version-distinct v2 paper state machine from absent through sealed inputs, preparation, probe, services, project, readiness, stimulus, observation, artifact sealing, and terminal cleanup. Closed operations, canonical envelope, evidence pages, artifact retrieval, consuming capabilities, and 12 compatibility/adversarial cases preserve v1 bytes and failure semantics | `evidence/openbox-sandbox-projectrun-protocol.md`; a v1 service must reject v2 before mutation, a v2 service must keep every v1 golden/conformance case, and host mounts, ambient env, inline secrets, arbitrary listeners/egress/provider selection, shell bootstrap fields, and fallback are structurally absent. No schema or prototype code was created. Rollup is 73/77; the cross-cutting threat model is the sole active root task |
| 2026-08-22 | SE-08-05 verified; SE-08-06 formally not applicable; SE-08-07 started | Froze 17 abuse cases across staging, dependencies/cache, credentials, relay/egress, identity, capabilities, evidence, artifacts, retention, cleanup, resources, fallback, and tenancy with exact fail-closed responses and release-blocking verification. Resolved the prototype-row conflict in favor of the owner's narrower discovery/ADR/separate-plan authority | `evidence/openbox-sandbox-projectrun-threat-model.md`; no prototype, branch, benchmark, schema, provider, service, client, or Shift Left driver was created. The future prototype and benchmark become explicit gates in the separate implementation plan, so SE-08-06 is `not_applicable`, not a pass. Rollup is 74 verified plus 1 not-applicable = 75/77 resolved; client/conformance/provider/rollout ownership is the sole active root task |
| 2026-08-22 | SE-08-07 verified; SE-08-08 started | Froze the production-client boundary, layered conformance suite, exact provider qualification matrix, migration sequence, no-fallback rollback, and repository/operations ownership. Native Codex remains the current runner and Compose remains only the disposable local OpenBox system plane | `evidence/openbox-sandbox-projectrun-rollout.md` SHA-256 `89919f877de15386284d67355683153f1dee31f7aed9241e7a22b4cf9c819f17`; all provider rows remain unsupported until their live gates pass, and no implementation or release authority was created. Rollup is 75 verified plus 1 not-applicable = 76/77 resolved; the ADR and separate implementation plan are the sole active root task |
| 2026-08-22 | SE-08-08 verified; Phase 08 closed | Accepted ADR-0021 as a versioned ProjectRun v2 design direction beside unchanged v1 and created a separate planned 46-day cross-repository implementation ledger. Native Codex remains the only current Shift Left project runner; Compose remains the disposable local OpenBox system plane and is never a project sandbox | `evidence/openbox-sandbox-v2-decision-validation.json` SHA-256 `2ec10a2ad92b6961e733dd32c84ceba3889239ac9accfb90b177c8deffcbbca0`; all 11 frozen Sandbox v1 source hashes match the clean exact commit, no v2 Go implementation exists, provider limitations remain explicit, and the separate plan is not authorized or started. Final rollup: 76 verified plus 1 not-applicable = 77/77 resolved |
| 2026-08-23 | Current-runner assumption withdrawn | The v2 design direction and separate authorization boundary remain unchanged, but native Codex is no longer a current runner. Consumer selection and rollback now require a separately qualified tuple and never imply fallback | `evidence/codex-loopback-port-isolation-review.json`; no ProjectRun v2 implementation was authorized or added |
