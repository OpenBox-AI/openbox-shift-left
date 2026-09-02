# Phase 01 — Artifact contracts and immutable run workspace

## Context

- Parent: [plan.md](plan.md)
- Depends on: [Phase 00](phase-00-architecture-and-qualification.md)
- Architecture: [`dc/security-evaluate.md` — Versioned artifacts](../../dc/security-evaluate.md#versioned-artifacts)

## Goal

Create the public evidence contracts and a deterministic, source-preserving run
workspace before implementing discovery or execution. Separate deterministic
content objects from run-specific provenance so hashes remain meaningful.

**Effort:** 5 engineer-days

**Status:** verified

**Dependencies:** SE-00-02, SE-00-03, SE-00-07

## Planned file shape

```text
contracts/project-assurance/
  README.md
  schema/
    project-model-v1.schema.json
    project-run-profile-v1.schema.json
    sdk-coverage-v1.schema.json
    sandbox-posture-v1.schema.json
    security-test-v1.schema.json
    audit-pack-v1.schema.json
    policy-proposal-v1.schema.json
cli/internal/assurance/
  artifact/
  snapshot/
  runfs/
```

This is a data-contract directory, not a new Go module. Go schema loading and
validation remain under the CLI module. If another binary needs these packages,
module extraction requires a later ADR-0011-compatible decision.

## Artifact rules

- Canonical content objects use sorted object keys, UTF-8, fixed integer/string
  encodings, no insignificant whitespace, and SHA-256 over exact canonical bytes.
- Runtime envelopes may include timestamps, temp paths, process IDs, and host
  details; those fields do not alter the digest of normalized content objects.
- A manifest lists every input/output digest, schema ID, producer version,
  source-selection rule, omission, and redaction.
- Writes use a run-owned staging directory, `fsync` where supported, atomic
  rename, and an explicit incomplete marker until finalization.
- A completed run defaults to `.openbox/audit/<run-id>/`. Passive inspection
  writes only its three standalone files to `.openbox/inspect/<inspection-id>/`;
  `--output` selects that exact inspection directory. Source code is never edited.
- The executable snapshot lives in a run-owned OS temp directory, not the source
  tree. `.git`, prior audit output, caches, sockets, secrets, and configured
  exclusions are absent by default.
- Git metadata is provenance only. The manifest hashes the actual selected bytes,
  including selected untracked files; it does not pretend `HEAD` describes a
  dirty project. Filesystem-only inspection records a detected repository with
  null HEAD/dirty fields and an explicit `git-status` uncertainty rather than
  executing Git or guessing state.

## Snapshot safety contract

1. Resolve and reject a project root that is `/`, a home directory, or the audit
   output/temp parent.
2. Walk without following symlinks outside the root; record internal symlinks and
   reject unsafe resolution.
3. Apply built-in secret/cache exclusions, project ignore rules, and explicit
   profile includes/excludes; record which rule omitted each material path class.
4. Copy selected regular files with executable bits and stable relative paths.
5. Hash each source byte stream and the normalized file manifest.
6. Mark the copied tree read-only except declared build/output/temp locations.
7. Compare source selection digests after the run and record any mutation as a
   failed safety invariant.

## Task ledger

| ID | Task | Depends | Status | Owner | Evidence required |
|---|---|---|---|---|---|
| SE-01-01 | Add all v1 JSON schemas and schema inventory documentation | SE-00-03 | verified | root | valid schemas plus positive/negative fixtures |
| SE-01-02 | Implement canonical JSON encoder and content digest types | SE-01-01 | verified | root | golden byte tests across map insertion orders |
| SE-01-03 | Implement run directory lifecycle, incomplete marker, atomic finalize, and cleanup receipt | SE-01-02 | verified | root | crash/interruption and permission tests |
| SE-01-04 | Implement safe project-root resolution and recursive source selection without external symlink traversal | SE-00-07 | verified | root | adversarial filesystem fixture tests |
| SE-01-05 | Implement deterministic snapshot copy, per-file hashes, executable-bit preservation, and read-only source proof | SE-01-04 | verified | root | same tree produces same manifest on repeated runs |
| SE-01-06 | Add default exclusions for VCS, audit output, caches, sockets/devices, and recognized secret files; record omissions | SE-01-04 | verified | root | exclusion matrix with no secret values in output |
| SE-01-07 | Implement manifest assembly that separates normalized-object digests from volatile run provenance | SE-01-02, SE-01-03, SE-01-05 | verified | root | byte-stability and changed-input tests |
| SE-01-08 | Add CLI-internal contract conformance suite and fuzz/path-boundary tests | SE-01-01…SE-01-07 | verified | root | `go test`/fuzz seed results under CLI module |

## Required test cases

- same clean fixture twice → identical normalized object bytes and digests;
- one source byte changes → project snapshot digest changes;
- timestamp/temp path changes → runtime envelope changes but normalized object
  digest does not;
- Git marker with unproven HEAD/dirty state → actual selected bytes are present
  and the unknown Git state is explicit;
- `.env`, private keys, sockets, FIFOs, devices, external symlinks, and nested
  `.openbox/audit` or `.openbox/inspect` → not copied and omission is recorded;
- output directory inside input root → walker does not recurse into it;
- crash before finalize → incomplete run remains identifiable and safe to clean;
- source file mutation during/after copy → run aborts or records an invalid
  snapshot; it never silently evaluates mixed bytes.

## Exit criteria

- [x] All seven public schemas have valid and invalid conformance fixtures.
- [x] Canonical bytes and object digests are deterministic across repeated runs.
- [x] Snapshot tests prove the original source is not a writable execution target.
- [x] Secret/cache/device/symlink exclusions fail safely and remain visible in
      the manifest.
- [x] Interrupted writes cannot produce a valid-looking audit pack.
- [x] No new Go module, backend endpoint, table, or service was introduced.

## Progress log

| Date | Task | Change | Evidence |
|---|---|---|---|
| 2026-08-20 | SE-01-01 | Started the seven-schema inventory after every Phase 00 exit gate verified | Reuse the frozen run-profile draft and existing contract conventions; no Go package, CLI route, module, endpoint, table, service, or compatibility path is authorized by this task |
| 2026-08-20 | SE-01-01 / SE-01-02 | Verified the closed seven-schema inventory and started canonical JSON/content digests | Ajv `8.17.1` on Node `22.20.0` compiled 7/7 strict draft-2020-12 schemas, accepted 7 frozen examples, rejected 41 basic, 24 structural-adversarial, and 7 semantic-adversarial mutations, and replayed the promoted Mastra run profile with 16 schema plus 18 semantic negatives; `evidence/project-assurance-schema-validation.json` pins schema/fixture/probe/toolchain hashes and limits schema proof from later JCS, object-resolution, finalization, tamper, SDK, sandbox, provider, and live claims; independent read-only replay found no remaining SE-01-01 blocker |
| 2026-08-20 | SE-01-02 / SE-01-03 | Verified canonical JSON/content digests and started the run-directory lifecycle | RFC 8785 serialization, UTF-16 ordering, 22 Appendix B finite-number vectors, insertion-order stability, duplicate/Unicode/non-finite/depth/cycle/custom-marshaler rejection, exact lowercase SHA-256 syntax, race, 96,216 bounded fuzz executions, full CLI tests, and vet passed on Go `1.26.7`; `evidence/canonical-json-validation.json` pins final source/test/module hashes and explicitly leaves signed-53 field bounds to pre-canonicalization schema validation; independent read-only replay found no remaining SE-01-02 blocker; no SDK/provider/sandbox/live path ran |
| 2026-08-20 | SE-01-03 / SE-01-04 | Verified the run-directory lifecycle and started safe project-root/source selection | Darwin/APFS tests prove durable incomplete/finalizing/cleaning states, canonical manifest-last exclusive publication, read-only fixed pack shape, inode-pinned no-follow sealing/cleanup, hard-link/mount/replacement refusal, explicit closed orphan cleanup, truthful receipts, and abrupt subprocess recovery at three interruption points; package/race/full CLI/vet and Darwin/Linux/Windows cross-compilation passed on Go `1.26.7`; `evidence/run-directory-lifecycle-validation.json` pins exact hashes and limits filesystem-commit validity, Linux runtime, xattr support, power-loss durability, and the final same-identity empty-entry race; independent review found no other high/medium issue; no SDK/provider/sandbox/live path ran |
| 2026-08-20 | SE-01-04 / SE-01-05 | Verified safe project-root/source selection and started deterministic snapshot copying | Darwin/APFS adversarial fixtures prove canonical boundary rejection, descriptor-relative sorted no-follow traversal, output/temp non-recursion, external-hop retention, fail-closed ambiguous symlinks, schema-aligned Unicode paths, special-file classification, mount-aware regular/directory checks, and nonblocking regular-to-FIFO replacement handling; package/race/full CLI/vet and Darwin/Linux/Windows cross-compilation passed on Go `1.26.7`; `evidence/project-source-selection-validation.json` pins final hashes and limits Linux runtime/mount, atomicity, invalid-UTF-8 fixture, copy, exclusion, omission, and live claims; independent review found no remaining high/medium issue |
| 2026-08-20 | SE-01-05 / SE-01-06 | Verified deterministic snapshot copying and started closed default/profile exclusions | Repeated copies produce identical canonical per-file manifests, snapshot/selection digests, counts, and totals; descriptor-relative bounded reads detect growth and mutations, stable destination mount/source aliases fail before writes, exact destination bytes are rehashed before owner-only `0500`/`0400` sealing, and source bytes/modes remain unchanged; focused/race/full CLI/vet and Darwin/Linux/Windows cross-compilation passed on Go `1.26.7`; `evidence/deterministic-snapshot-copy-validation.json` pins final hashes and caller-cleanup, operational-size, Linux-runtime/mount, hostile-relocation, atomicity, and exclusion limits |
| 2026-08-20 | SE-01-06 / SE-01-07 | Verified closed default exclusions and started normalized-object manifest assembly | Darwin fixtures and ten repeated matrix runs prove deterministic, path-first pruning of VCS/cache/audit/temp/recognized-secret paths and closed socket/FIFO/device/external-symlink/external-mount omission classes; secret path examples are always suppressed, every omission references an active rule, only safe regular files materialize, and focused/race/full CLI/vet plus Darwin/Linux/Windows cross-compilation passed on Go `1.26.7`; `evidence/source-exclusion-validation.json` pins exact hashes and the conservative filename-policy, non-secret path-metadata, pre-policy inventory, no-ignore/profile-parser, privileged-fixture, platform-runtime, and inherited copy limits; manifest visibility remains a Phase 01 exit proof for SE-01-07/08 rather than being inferred from the omission API alone |
| 2026-08-20 | SE-01-07 / SE-01-08 | Verified normalized-object manifest assembly and started CLI-internal contract conformance | The typed assembler owns the exact v1 role/schema/media map, derives the judgment object from authoritative inline bytes, keeps timestamp/temp provenance outside retained content, and produces insertion-order-stable CIDs; `WritePackObjects` plus `FinalizePack` persists deduplicated payloads and seals then exact-set/identity/byte-verifies them before manifest-last publication, while mutation/extra-object failures remain incomplete and cleanup-recoverable; focused/race/repeated/full CLI/vet/format and Darwin/Linux/Windows cross-compilation passed on Go `1.26.7`; `evidence/manifest-assembly-validation.json` pins hashes and the separate schema-validator, filesystem-state, volatile-envelope, hostile-same-UID, platform-runtime, and no-live-execution limits; independent review found no remaining high/medium blocker |
| 2026-08-20 | SE-01-08 / Phase 01 | Verified contract conformance and every Phase 01 exit gate | A bounded opt-in CLI test replays the separately qualified Ajv `8.17.1` toolchain without a new Go dependency; 7/7 strict schemas and fixtures passed 48 basic, 24 structural-adversarial, and 9 semantic-adversarial negatives, while the Mastra profile retained 16 schema plus 18 semantic negatives; a real snapshot proved audit/cache/secret/external-symlink/FIFO omissions survive in a validated project model, CID-bound validated manifest, and finalized pack; two bounded fuzz runs, ten repeated assurance runs, race, all 11 module tests, vet, format, and Linux/Windows cross-compilation passed; `evidence/contract-conformance-validation.json` records exact hashes, commands, cleanup, and limits; independent review approved the final bytes with no high/medium blocker; no SDK, sandbox, project, provider, live, or external-control-plane path ran |
| 2026-08-21 | SE-01-01 | Reopened the project-model contract for the owner-approved filesystem-only Git amendment | Permit a present repository to retain `head=null` and `dirty=null` only with an explicit `git-status` uncertainty; do not execute Git or add a VCS parser, compatibility surface, schema version, or other delivery concept; SE-02-08 is dependency-paused until this invariant and the affected Phase 01 exit proof are reverified |
| 2026-08-21 | SE-01-01 / SE-01-06 | Reverified the Git-unknown project-model invariant and started the inspection-output exclusion amendment | Strict Ajv `8.17.1` compiled 7/7 schemas, accepted 7/7 fixtures, and rejected 48 basic, 25 structural-adversarial, and 10 semantic-adversarial cases; focused Go producer/collector/conformance tests passed; `evidence/project-assurance-schema-validation.json` pins the amended schema, validator, producer, collector, commands, and no-Git/no-live limits; the built-in `.openbox/inspect` exclusion is now the sole active root task |
| 2026-08-21 | SE-01-06 / SE-01-08 | Reverified closed exclusions and started the affected conformance exit proof | Root, prior, nested, and case-variant `.openbox/inspect` trees are now pruned exactly like audit output and retain the closed `audit_output` omission class; repeated/race/all-assurance/full-CLI/vet/cross-compilation proofs passed; `evidence/source-exclusion-validation.json` pins amended hashes, commands, and unchanged limits; contract/manifest integration is now the sole active root task |
| 2026-08-21 | SE-01-08 / Phase 01 | Reverified the affected contract/manifest integration and restored every Phase 01 exit gate | Pinned Ajv `8.17.1` conformance, the actual omission-to-CID-to-finalized-manifest path, and canonical inspection-artifact validation all passed with the amended 48 basic, 25 structural-adversarial, and 10 semantic-adversarial matrix; all-assurance/full-CLI/race/vet/format/cross-compilation passed; `evidence/contract-conformance-validation.json` pins the current harness, integrations, commands, and limits; no Git command, SDK runtime, sandbox, provider, live, or external-control-plane path ran |
| 2026-08-22 | SE-01-01 reopened for the owner-selected local model tuple | Replace the external HTTPS/positive-spend relay contract with one exact credentialless literal-loopback Ollama descriptor and zero monetary cost; retain the one-time child relay bearer, text-only/tool-only generic-proxy boundary, token/duration/content bounds, and redacted-digest posture; do not retain an OpenAI compatibility path or add a second relay mode | Owner selected local Ollama with `granite4.1:3b`; observed server `0.31.1`, client `0.32.14`, model digest `sha256:6fd349357287c7ffc9e38189a93b48ea175d24fc566b38f09cfc564fb7f303eb`, GGUF Granite 3.4B Q4_K_M, and `completion`/`tools` capabilities through literal-loopback `/api/tags` and `/api/show`; SE-04-08 is dependency-blocked until the amended schema/semantic fixtures reverify |
| 2026-08-22 | SE-01-01 reverified; Phase 01 restored; SE-04-08 resumed | The sole relay profile is now the exact credentialless Ollama tuple with zero monetary cost; the compiled parent authority separately fixes `GET /api/version` and `GET /api/tags`, the child/inference authority remains only `POST /api/chat`, and server/model digest, GGUF/Granite/3.4B/Q4_K_M details, and `completion`/`tools` are centrally bound without an OpenAI compatibility route | Pinned Ajv `8.17.1` passed all 7 strict schemas/fixtures, 48 basic, 25 structural-adversarial, 10 semantic-adversarial, and the run-profile 16 schema/18 semantic negatives; Go-Ajv conformance, count-10/race/vet/format, exact evidence replay, and independent read-only review passed; `evidence/project-run-profile-v1.validation.json` and `evidence/project-assurance-schema-validation.json` distinguish declarative validity from runtime tuple observation |
