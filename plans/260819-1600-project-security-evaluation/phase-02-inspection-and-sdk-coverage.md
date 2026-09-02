# Phase 02 — Passive inspection and SDK coverage

## Context

- Parent: [plan.md](plan.md)
- Depends on: [Phase 01](phase-01-artifacts-and-workspace.md)
- Architecture: [`dc/security-evaluate.md` — Project model](../../dc/security-evaluate.md#project-model)
- First framework source: local `../openbox-mastra-sdk` clone at the SE-00-04 pin

## Goal

Implement `openbox project inspect` as a filesystem-only discovery pass. It
builds a provenance-rich project graph, identifies the exact OpenBox SDK
integration expected for a later run, and describes coverage gaps. It never
imports modules, installs packages, invokes a package manager, evaluates project
configuration code, or assigns a behavioral verdict.

**Effort:** 5 engineer-days

**Status:** verified

**Dependencies:** Phase 01 verified; SE-00-04 SDK descriptor decisions

## Planned package shape

```text
cli/cmd/openbox/project.go
cli/cmd/openbox/project_inspect.go
cli/internal/assurance/model/
cli/internal/assurance/inspect/
cli/internal/assurance/sdkdesc/
cli/internal/assurance/testdata/projects/
```

SDK descriptors are versioned data. Each descriptor names package detection,
supported version range, exact environment mapping, initialization signatures,
expected event classes, readiness probes for Phase 04, ignored endpoints, and
known blind spots. A descriptor does not turn package presence into runtime
coverage.

## Discovery scope

The v1 pass reads declarative manifests and bounded source patterns for:

- Python (`pyproject.toml`, requirements/lock files) and Node/TypeScript
  (`package.json` and supported lockfiles);
- agent and HTTP entrypoints;
- LangGraph/LangChain/Mastra/custom SDK imports and initialization;
- tool declarations, MCP server configuration, model routes, retrieval/memory,
  HTTP/DB/file/function surfaces, credentials by variable name only, approvals,
  and external destinations;
- declared sandbox/network boundaries and existing OpenBox integration.

Every graph node/edge carries detector ID, source path/location, confidence,
and one of `declared`, `inferred`, or later `observed`. Unknown dynamic
registration stays an explicit uncertainty.

The normalized in-process graph is the authority for that richer evidence: it
retains confidence, byte column, and source digest. The already accepted closed
`openbox.project-model/v1` provenance projection carries only its frozen
detector, basis, path, and optional line fields; Phase 02 does not silently add
schema properties or claim that confidence/column/digest are serialized there.
Package dependency/import, entrypoint, and environment-reference signals feed
descriptor and coverage derivation but are not invented as public node types.
If passive evidence yields no contract-supported node, inspection fails
explicitly instead of emitting a schema-invalid or synthetic-node graph. No
edge is created until a detector supplies an actual relationship.

## Task ledger

| ID | Task | Depends | Status | Owner | Evidence required |
|---|---|---|---|---|---|
| SE-02-01 | Add `project` routing and the `inspect` flag/parser surface without changing existing command behavior | SE-01-08 | verified | root | CLI usage/golden tests and existing main tests |
| SE-02-02 | Implement bounded manifest readers for Python and Node/TypeScript with size/path limits | SE-01-01 | verified | root | fixture matrix including malformed/oversized manifests |
| SE-02-03 | Implement source-evidence detectors that tokenize/parse supported files without importing or executing them | SE-02-02 | verified | root | import/exec tripwire and adversarial syntax tests |
| SE-02-04 | Build project graph normalization, provenance, uncertainty, and stable identity | SE-02-02, SE-02-03 | verified | root | deterministic graph goldens and collision tests |
| SE-02-05 | Add the pinned Mastra/base-SDK descriptor and descriptor compatibility validation | SE-00-04, SE-01-01 | verified | root | descriptor matches qualified SDK tuple and rejects drift |
| SE-02-06 | Derive expected SDK coverage and known gaps from graph plus descriptor, without claiming observed coverage | SE-02-04, SE-02-05 | verified | root | coverage fixtures for enabled, disabled, bypassed, and unknown surfaces |
| SE-02-07 | Emit canonical project-snapshot manifest input, `project-model.json`, `sdk-coverage.json`, and actionable readiness guidance without publishing the incomplete audit-pack root | SE-02-04, SE-02-06 | verified | root | schema-valid byte-stable output fixtures |
| SE-02-08 | Add no-execution, no-network, no-secret-value, and source-unchanged acceptance tests around the real CLI | SE-02-01…SE-02-07 | verified | root | subprocess/network spies and before/after file manifest |

## Required negative behavior

- An importable Python package whose module writes a sentinel on import must not
  write it.
- A `package.json` script with a sentinel side effect must not run.
- Dynamic tools discovered only through execution remain `unknown`; the scanner
  must not infer their absence.
- Credentials are modeled by variable/secret reference name and boundary, never
  by reading or serializing their values.
- Unsupported SDK version, ambiguous SDK initialization, or conflicting config
  keys produces a coverage warning and blocks automatic behavioral readiness.
- A hard-coded production SDK coordinate, or an initialization site for which
  the descriptor cannot prove exact trusted `apiUrl`/`apiKey` mapping or omission
  through `withOpenBox`, is `not_runnable`. The same applies when `validate`,
  `onApiError`, or `sendActivityStartEvent` is dynamic or conflicts with the
  required `true`/`fail_closed`/`true` values; environment fallbacks are never
  assumed to override an explicit option.
- Static findings use a distinct `risk_hypothesis` kind. They do not use
  `exploitable`, `blocked`, or `not_observed`.

## Exit criteria

- [x] `openbox project inspect` operates without importing, executing, package
      installation, model calls, or network access.
- [x] Repeated inspection of the same selected bytes produces the same project
      model and expected-coverage bytes.
- [x] All graph facts resolve to detector and source evidence.
- [x] The only MVP Mastra descriptor reflects the qualified SDK release, including
      exact configuration keys and known unsupported action classes.
- [x] Dynamic/opaque behavior is explicitly incomplete rather than guessed.
- [x] Existing CLI routes and all 11 module tests remain unaffected.

## Progress log

| Date | Task | Change | Evidence |
|---|---|---|---|
| 2026-08-20 | SE-02-01 | Started the passive `project inspect` parser surface after Phase 01 verified | Reuse the existing CLI router and test conventions; this task authorizes no inspection engine, execution, package installation, endpoint, service, provider hook, compatibility shim, or live path |
| 2026-08-20 | SE-02-01 / SE-02-02 | Verified the inert `project inspect` parser and started bounded declarative manifest readers | The router advertises only `project inspect`, defaults to `.`, parses the documented interspersed `--output`, preserves delimited dash paths and existing help/error precedence, and fails explicitly before any inspector exists; focused/full/race/full-CLI/vet/format plus Linux/Windows compilation passed; `evidence/project-cli-parser-validation.json` pins exact hashes and limits; independent review approved the final missing-value/help-precedence bytes with no blocker; bounded Python and Node/TypeScript manifest reading is now the sole active root task |
| 2026-08-20 | SE-02-02 / SE-02-03 | Verified bounded manifest readers and started source-evidence detection | A closed npm/Python inventory now reads exact Phase 01 snapshot bytes under 1 MiB/file, 8 MiB aggregate, 128-file, 4,096-rune, 64-component, and 64-JSON-depth caps; descriptor-relative Darwin/Linux reads fail closed on mount/symlink/hardlink/type/size/digest drift and never execute package scripts; JSON syntax is validated while TOML/YAML/requirements/line locks are truthfully opaque; repeated/real-Mastra/race/full-CLI/vet/format/cross-compile tests passed; `evidence/manifest-reader-validation.json` pins hashes, commands, selected-clone proof, and limits; independent review found no high/medium blocker; bounded source tokenization is now the sole active root task |
| 2026-08-20 | SE-02-03 / SE-02-04 | Verified passive source evidence and started graph normalization | Bounded lexical detectors now retain exact structural manifest/source locations for declared or inferred agent, tool, entrypoint, OpenBox, model/retrieval/memory/MCP, credential-name, approval, filesystem/process/network, persistence/telemetry, and sanitized-origin evidence without importing or executing project code; malformed/dynamic/opaque/interpolated/unsupported syntax remains explicit uncertainty; sentinel, adversarial, selected-Mastra, race, full-CLI/all-11-module, vet, format, and Darwin/Linux/Windows compile proofs passed; `evidence/source-detector-validation.json` pins closure hashes, commands, limits, and non-observation claims; independent review found no remaining high/medium blocker; deterministic graph normalization is now the sole active root task |
| 2026-08-20 | SE-02-04 / SE-02-05 | Verified normalized graph evidence and started the Mastra descriptor | JCS/SHA-256 identities now separate source-local agent/model/tool/MCP/retrieval/memory/approval declarations while merging genuinely shared SDK/credential/destination/boundary surfaces; provenance retains detector/basis/confidence/path/line/column/digest, collisions fail closed, 64-location truncation is explicit, node-less inputs fail instead of inventing schema content, internal signals are reserved for descriptor/coverage work, and no relationship edge is inferred from co-location; deterministic golden/collision/integration/repeated/race/full-CLI/all-11-module/vet/format/cross-compile proofs passed; `evidence/graph-normalization-validation.json` records hashes and accepted-schema projection limits; independent review approved final bytes; the pinned Mastra/base-SDK descriptor is now the sole active root task |
| 2026-08-21 | SE-02-05 / SE-02-06 | Verified the exact local Mastra descriptor and started expected-coverage derivation | The only descriptor now binds the qualified clean commit/archive/package/lock and base/Core URI/integrity, rejects consumer/registry lookalikes, requires exactly one statically targeted `withOpenBox` site under the accepted coordinate/safe-control shapes and exact executed probe controls, preserves unsigned identity, declares readiness/ignored/unsupported surfaces, and limits pre-effect coverage to `recordingTool` `ActivityStarted`; 47 focused tests/subtests with zero skips, 10 repeats, race, full CLI/all-11-module, vet, format, Darwin/Linux/Windows compilation, exact archive replay, and independent final-hash review passed; `evidence/mastra-sdk-descriptor-validation.json` records commands, fingerprints, results, and trusted-attestation/static-only limits; expected SDK coverage is now the sole active root task |
| 2026-08-21 | SE-02-06 / SE-02-07 | Verified expected SDK coverage and started public artifact emission | Static derivation now binds the project declaration to an exact hashed `1.0.0` value without retaining ranges, credentials, or local paths; wrong/conflicting versions are unsupported, integration `enabled` remains only an expectation over the qualified local-clone candidate, exact `recordingTool` stays unknown/`missing`, and readiness can only be `inconclusive` or `not_runnable`; enabled/disabled/bypassed/unknown fixtures, 96 focused tests/subtests with zero skips, 10 repeats, race, full CLI/all-11-module, vet, format, and Darwin/Linux/Windows compilation passed; `evidence/expected-sdk-coverage-validation.json` pins hashes, commands, classifications, and installed-byte/non-observation limits; independent read-only review approved the final six hashes; schema-valid public emission is now the sole active root task |
| 2026-08-21 | SE-02-07 / SE-02-08 | Verified canonical inspection role inputs and started real-CLI acceptance | Private project-snapshot/project-model objects and a rich-graph digest prevent cross-graph substitution; public provenance is bound to immutable snapshot bytes, generated project-model and sdk-coverage objects are canonical, byte-stable, and valid under the pinned Ajv `8.17.1` schemas, and static coverage remains missing/inconclusive with zero events/probes; the accepted manifest-last contract means Phase 02 emits the project-snapshot manifest role input but does not publish an incomplete audit-pack `manifest.json`; 101 focused tests/subtests with zero skips, 10 repeats, race, all 11 modules, vet, format, and Darwin/Linux/Windows compilation passed; source and snapshot script sentinels stayed absent; `evidence/inspection-artifact-validation.json` pins hashes, CIDs, commands, and identity/live-proof limits; independent read-only review approved the final hashes; real-CLI safety acceptance is now the sole active root task |
| 2026-08-21 | SE-02-08 | Dependency-paused real-CLI acceptance for the owner-approved Phase 01 amendment | Return to SE-02-08 only after the project-model Git-unknown invariant and built-in `.openbox/inspect` output exclusion are contractually frozen and reverified; retain the already verified SE-02-01…07 results and add no audit-pack manifest, output index, or lifecycle engine |
| 2026-08-21 | SE-02-08 | Resumed real-CLI acceptance after the amended Phase 01 exits reverified | Use the fixed three-file `.openbox/inspect/<inspection-id>/` standalone output, keep `--output DIR` exact, and add no `manifest.json`, index, public schema, CID store, audit-pack claim, or reusable lifecycle engine |
| 2026-08-21 | SE-02-08 / Phase 02 | Verified real-CLI safety and every Phase 02 exit gate | The real command atomically publishes exactly three standalone files with no manifest/index/CID store, preserves repeated bytes, excludes prior output, removes only newly created empty parents on failure, rechecks Git-marker identity after source verification, and represents unknown Git only as the exact null-head/null-dirty tuple plus uncertainty; PATH/process, package-script, Python-effect, loopback, secret, source-state, no-clobber, schema, selected-clone, repeated/race/vet/all-11-module/cross-platform proofs passed; the clean pinned `openbox-mastra-sdk@1.0.0` clone remained unchanged and truthfully returned `not_runnable`; `evidence/project-inspect-cli-safety-validation.json` pins hashes, CIDs, commands, limits, and independent approval; no project/model/sandbox/live/control-plane path ran |
