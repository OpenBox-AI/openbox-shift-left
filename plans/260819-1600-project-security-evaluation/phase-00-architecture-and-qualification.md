# Phase 00 — Architecture decision and dependency qualification

## Context

- Parent: [plan.md](plan.md)
- Design source: [`dc/security-evaluate.md`](../../dc/security-evaluate.md)
- Current architecture rules: [`CLAUDE.md`](../../CLAUDE.md),
  [`docs/architecture.md`](../../docs/architecture.md),
  [`ADR-0011`](../../docs/adr/ADR-0011-multi-module-layout.md), and
  [`ADR-0017`](../../docs/adr/ADR-0017-inline-policy-evaluation.md)
- External capability sources:
  [Codex sandboxing](https://learn.chatgpt.com/docs/sandboxing),
  [Codex permissions](https://learn.chatgpt.com/docs/permissions),
  [Codex app-server](https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md),
  [Claude sandboxing](https://code.claude.com/docs/en/sandboxing), and
  [Claude sandbox environments](https://code.claude.com/docs/en/sandbox-environments)

## Goal

Turn the discussion draft into an accepted implementation boundary and replace
all version-sensitive assumptions with small executable qualification records.
No production feature code belongs in this phase.

**Effort:** 3 engineer-days

**Status:** verified

**Dependencies:** none

## ADR-0020 decisions

The ADR must decide, rather than merely list:

1. `openbox project` is a separate project-assurance lane inside the one CLI;
2. `cli/internal/assurance` is the initial code boundary, with no new module;
3. framework SDKs remain unchanged and emit their normal wire traffic;
4. application code runs only after an explicit `project test` command and a
   successful sandbox capability probe;
5. a loopback receiver and test identity are separate from production Core;
6. deterministic correlated evidence, not model judgment, owns outcomes;
7. audit packs remain local and proposals remain inert in this plan;
8. native host sandboxes are the MVP execution boundary; OpenBox Sandbox v1 is
   not a full-project runner and needs a versioned v2 decision; and
9. no automatic driver fallback, no source edit, no policy publish, and no
   production attack-event upload occur.

## Qualification records

Write machine-readable observations under
`plans/260819-1600-project-security-evaluation/evidence/` with exact command,
version, platform, exit status, and captured output digest. These records do not
claim support; Phase 03 turns passing probes into a support tuple.

- SDK: pin the Mastra and base-SDK release/commit tuple; resolve exact API URL,
  key, DID/private-key, error-policy, instrumentation, and framework init keys.
- SDK wire: capture auth/evaluate request and response bytes from a minimal
  instrumented process, without a production endpoint.
- Codex: qualify standalone command shape, cwd, child env, timeout, output caps,
  network/loopback behavior, child inheritance, denial reporting, and behavior
  when its sandbox backend is unavailable.
- Claude: qualify standalone sandbox-runtime and inherited Bash sandbox as
  separate modes; explicitly test `failIfUnavailable=true` and
  `allowUnsandboxedCommands=false`.
- OpenBox Sandbox: pin v1 constraints from source and record the exact gaps a
  full project run would require; do not modify that repository here.

## Task ledger

| ID | Task | Depends | Status | Owner | Evidence required |
|---|---|---|---|---|---|
| SE-00-01 | Record clean/dirty state, commit, platform, and installed Codex/Claude versions for every inspected repository/tool | — | verified | root | `evidence/baseline.json` plus command transcript digest |
| SE-00-02 | Draft and index ADR-0020 with decisions, rejected alternatives, authority boundaries, and explicit non-goals | SE-00-01 | verified | root | accepted ADR and index diff |
| SE-00-03 | Freeze schema IDs, additive/breaking compatibility rule, canonical JSON rule, and audit-pack directory contract | SE-00-02 | verified | root | ADR section plus schema inventory test plan |
| SE-00-04 | Pin and qualify the first Mastra/base-SDK tuple from the local clone, including actual config keys and pre-effect events | SE-00-01 | verified | root | reproducible SDK wire probe and package lock |
| SE-00-05 | Run the Codex native-sandbox feasibility spike, including loopback and spawned-child probes | SE-00-01 | verified | root | `evidence/codex-sandbox-qualification.json` |
| SE-00-06 | Run the Claude standalone/inherited feasibility spike and record fail-unavailable/escape behavior | SE-00-01 | verified | root | `evidence/claude-sandbox-qualification.json` |
| SE-00-07 | Decide the v1 HTTP run-profile shape, receiver endpoint subset, retention default, and production-endpoint rejection rule | SE-00-03, SE-00-04, SE-00-05 | verified | root | ADR sections and example profile validated against schema draft |
| SE-00-08 | Search release history/package registries for a shipped `openbox-audit`; decide whether any one-way error shim is needed | SE-00-01 | verified | root | search transcript and ADR decision |

## Tests and evidence gates

- ADR linter/index checks pass and all version-sensitive claims cite source or a
  qualification record.
- SDK probe fails if any request reaches a non-loopback endpoint.
- Sandbox probes include both the initial process and a spawned child.
- A missing Codex/Claude sandbox backend causes a non-zero probe result and never
  executes the protected payload outside confinement.
- If Codex cannot support the required loopback plus explicit model-provider
  posture, ADR-0020 names a different first driver; Phase 03 does not work
  around the failure by enabling unrestricted execution.
- If the SDK requires signing for local validation, the ADR chooses a test-only
  signing path; it must not copy the developer's production key into the run.

## Exit criteria

- [x] ADR-0020 is accepted and indexed.
- [x] First framework/SDK tuple and first sandbox candidate are pinned by exact
      version, with known limitations.
- [x] The schema, run-profile, receiver, retention, and compatibility decisions
      are closed enough that Phase 01 cannot invent them in code.
- [x] OpenBox Sandbox v1 reuse is explicitly accepted or rejected for the MVP;
      any v2 work is routed to Phase 08.
- [x] Every unresolved issue has an owner, stop condition, and affected phase.

## Progress log

| Date | Task | Change | Evidence |
|---|---|---|---|
| 2026-08-19 | SE-00-01 | Started baseline qualification evidence collection | Pending `evidence/baseline.json` and transcript digest |
| 2026-08-19 | SE-00-01 | Verified repository, platform, and installed-tool baseline | `jq -e ... evidence/baseline.json` and canonical transcript SHA-256 recomputation passed; exact outputs pin Shift Left `eaea69b`, Python SDK `1f0429d`, Sandbox `5e88a45`, Codex `0.148.0`, Claude `2.1.235`; LangGraph pin is intentionally deferred to SE-00-04 |
| 2026-08-19 | SE-00-02 | Started ADR-0020 drafting and index update | Dependency SE-00-01 verified; ADR must preserve the nine pre-decided authority boundaries and reject unqualified fallbacks |
| 2026-08-19 | SE-00-02 | Accepted and indexed the project-assurance architecture boundary | `docs/adr/ADR-0020-project-assurance-native-sandbox.md`; index/link/authority-keyword/whitespace checks and `git diff --check` passed; acceptance does not qualify any SDK or sandbox tuple |
| 2026-08-19 | SE-00-03 | Started artifact compatibility and directory-contract decisions | Dependency SE-00-02 verified; no schema implementation is authorized until names and canonicalization are frozen |
| 2026-08-19 | SE-00-03 | Verified the v1 artifact contract and inventory test plan | ADR-0020 pins seven identifiers, strict additive/breaking rules, RFC 8785/JCS plus SHA-256, the manifest-last object layout, and six schema/canonicalization/tamper test gates; structural/whitespace checks and `git diff --check` passed |
| 2026-08-19 | SE-00-04 | Started LangGraph and base-SDK tuple qualification | Exact package release/commit, real configuration keys, normal auth/evaluate wire, and pre-effect events must be captured without a production endpoint or key |
| 2026-08-19 | SE-00-04 | Verified the exact first SDK candidate and started SE-00-05 | `openbox-langgraph-sdk-python==1.0.0` at `a24ccc7` plus base SDK `1.2.0` at `1f0429d`, LangGraph `1.2.11`, and LangChain Core `1.5.6`; isolated Python 3.12 probe observed signed loopback auth/evaluate wire and tool `ActivityStarted` before effect; ALLOW ran once and mock BLOCK stopped the effect but is not OpenBox block proof; 55 base-SDK tests and 24 framework tests passed, with retained async-cleanup warnings and public strict-context/subagent-shape limitations; artifact/body/result digest checks, temp cleanup, scope review, and `git diff --check` passed |
| 2026-08-19 | SE-00-05 | Verified Codex native-sandbox feasibility and started SE-00-06 | Codex CLI `0.148.0` / Seatbelt on macOS `26.5.2` arm64; exact custom `:workspace`-derived profile allowlists only loopback HTTP through the managed proxy; parent and spawned child inherited sandbox/proxy/sanitized env, wrote the run root, received `EPERM` outside it and on direct sockets, and received HTTP 403 for an unlisted `.invalid` host; denial logs captured both processes; invalid state and fault-injected missing `/usr/bin/sandbox-exec` exited nonzero without payload fallback; caller process-group timeout removed the child; native output forwarded 70,000 bytes and has no cap, so Phase 03 must implement streaming caps; deterministic probe digest repeated twice, cleanup/scope checks and `git diff --check` passed |
| 2026-08-19 | SE-00-06 | Verified Claude standalone/inherited feasibility and started SE-00-07 | Standalone SRT `0.0.73` and inherited Claude Code `2.1.235` passed parent/child filesystem, loopback, unlisted-network, credential-removal, escape-denial, backend-fault, timeout, and cleanup checks against local fixtures; `allowLocalBinding=true` exposes every local port on this tuple; inherited backend failure is a Bash error while Claude itself exits zero, so the qualification wrapper proved explicit nonzero mapping `86`; the Claude parent was not syscall-confined and no paid/provider request ran; JSON predicates, script hashes, ADR links, scope, whitespace, and `git diff --check` passed |
| 2026-08-19 | SE-00-07 | Verified the closed HTTP run-profile/receiver contract and started SE-00-08 | Strict Ajv `8.17.1` draft-2020-12 validation passed the local example, 15 schema negatives, and 18 semantic negatives; a fresh exact five-package reinstall matched recorded package hashes and registry integrities and reproduced the result byte-for-byte; ADR-0020 now freezes the auth-once/evaluate state machine, loopback-only production rejection, sole `redacted_digests` retention, closed environment/template/relay authority, and profile/template/budget bounds; no paid, provider, production, or live run occurred |
| 2026-08-19 | SE-00-04 | Owner changed the MVP SDK to the local `openbox-mastra-sdk` clone | Reopened SE-00-04 as the earliest affected task; prior LangGraph qualification remains comparative evidence but no longer satisfies the MVP tuple; SE-00-07 returned to planned because its descriptor is LangGraph-pinned, and partially researched SE-00-08 is paused; no code or evidence was deleted |
| 2026-08-20 | SE-00-04 | Verified the Mastra MVP tuple and started SE-00-07 | Clean local tag/commit `db9863b` pins Mastra SDK `1.0.0`, base SDK `1.0.1`, Mastra Core `1.8.0`, and Node `26.7.0`; the real `withOpenBox` loopback probe reproduced normalized auth/start/completion wire twice, observed ALLOW before one bounded effect, and observed mock BLOCK before no second effect; focused 60-test gate, full lint/typecheck/195-test/build gate, JSON/hash/source-clean/cleanup/scope/whitespace checks passed; no provider, production endpoint, live Core, or paid run occurred; SE-00-07 now owns replacing the obsolete LangGraph descriptor in the frozen profile contract |
| 2026-08-20 | SE-00-07 | Verified the Mastra-only run-profile contract and started SE-00-08 | Exact Ajv `8.17.1` replay matched the artifact result with 16 schema negatives and 18 semantic negatives; the prior LangGraph descriptor is rejected, `recordingTool` is only the sensitive outbound gate, other event classes require later executable readiness proof, and `withOpenBox` explicit-option precedence now makes conflicting/dynamic URL/key/control values `not_runnable`; current ADR/Phase 02/Phase 04 and schema/example/validator hashes match the post-review evidence record; package metadata/integrity, temp cleanup, changed/untracked scope, tracked/untracked whitespace, and clean Mastra clone checks passed; no live/provider/production action occurred |
| 2026-08-20 | SE-00-08 | Verified no shipped `openbox-audit` evidence and closed Phase 00 | Five local repositories have zero reachable exact-name history hits; the sole workspace occurrence is explicitly superseded brainstorm text; 19 public repositories and 48 releases contain zero exact repo/tag/asset hits; Shift Left `v0.1.0`/`v0.2.0` archives match published checksums, contain only `LICENSE`, `README.md`, and `openbox`, and have zero exact README/binary-string hits; relevant registry negatives exclude an inconclusive Crates.io 403; ADR-0020 therefore authorizes no shim, alias, redirect, second binary, or second engine; JSON/ADR-link/no-code-path/scope/whitespace checks passed, with private/deleted/manual distributions retained as an explicit evidence limit |
