# Phase 03 — Sandbox SPI and first local driver

## Context

- Parent: [plan.md](plan.md)
- Depends on: [Phase 01](phase-01-artifacts-and-workspace.md)
- Qualification: [Phase 00](phase-00-architecture-and-qualification.md)
- Official references:
  [Codex sandboxing](https://learn.chatgpt.com/docs/sandboxing),
  [Codex permissions](https://learn.chatgpt.com/docs/permissions), and
  [Codex app-server command execution](https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md)

## Goal

Create a project-assurance-specific sandbox interface and qualify or reject an
exact runner tuple. A driver must prove its effective restrictions before
launching the project. It must never retry the command unsandboxed, switch
drivers mid-run, or infer confinement from a host or product name. The former
Codex tuple is rejected until exact loopback endpoint isolation is possible.

**Effort:** 6 engineer-days

**Status:** blocked

**Dependencies:** SE-00-05, Phase 01 workspace and manifest contracts

## Driver contract

The initial interface should remain smaller than a general sandbox API:

```go
type Driver interface {
    Identity(context.Context) (Identity, error)
    Probe(context.Context, ProbeSpec) (Posture, error)
    Run(context.Context, RunSpec) (RunResult, error)
    Cleanup(context.Context, RunID) (CleanupEvidence, error)
}
```

`ProbeSpec` names the immutable snapshot, declared writable temp/output roots,
a separate run-owned protected write root, allowed loopback receiver/fixture
endpoints, explicit denied loopback/external targets, optional external model
destinations, environment names, timeout, and expected child behavior. `Posture` records
provider/version/platform, invocation surface, effective filesystem/network/
process/credential restrictions, child inheritance, probe observations,
denial-evidence availability, and fallback behavior. Its public
`configurationDigest` binds the exact executable-byte digest and the effective
configuration digest plus a derived binding over the immutable snapshot and
exact declared roots, network targets, environment names, timeout, and child
requirement, including whether the qualified loopback managed proxy is required.

`RunSpec` contains an argv vector, never a shell string; a minimal child
environment; cwd inside the snapshot; budgets; output caps; and the exact
qualified posture digest. A posture from a different binary, config, snapshot,
or destination set is invalid.

The driver must use a run-owned, reviewable configuration through the host's
supported flags/state mechanism. It may incorporate mandatory managed
requirements, but it must not silently depend on mutable personal profiles. The
effective merged configuration or an equivalent attestation is hashed into the
posture evidence.

## Safety probes

Before every project run, execute a small built-in probe program through the
same driver/config:

1. read an allowed fixture and write inside the declared temp root;
2. fail to write a randomized sentinel outside all writable roots;
3. reach the exact loopback receiver/fixture endpoints;
4. fail to reach an unapproved loopback endpoint and an unapproved external
   destination without relying on DNS alone;
5. spawn a child that repeats steps 1–4;
6. demonstrate the configured environment allowlist and absence of production
   credential sentinels;
7. hit timeout and output limits predictably; and
8. exercise the sandbox-unavailable path without executing the payload.

External model access is disabled for the deterministic vertical slice. It is
enabled later only when a driver can express and prove the declared provider
destinations without broad ambient egress.

## Task ledger

| ID | Task | Depends | Status | Owner | Evidence required |
|---|---|---|---|---|---|
| SE-03-01 | Define driver types, closed failure reasons, posture schema mapping, and test fake | SE-01-01 | verified | root | interface tests and schema round-trip |
| SE-03-02 | Implement executable discovery and exact identity/version/config hashing without trusting aliases | SE-00-05, SE-03-01 | verified | root | path replacement and version-drift tests |
| SE-03-03 | Build the cross-platform probe helper for filesystem, network, env, child, timeout, and output checks | SE-03-01 | verified | root | direct helper tests plus adversarial targets |
| SE-03-04 | Implement Codex standalone argv/config construction using the qualified public surface | SE-03-02 | verified | root | exact argv goldens; no shell interpolation |
| SE-03-05 | Implement probe execution and fail-closed mapping for missing backend, unsupported platform/profile, and failed child inheritance | SE-03-03, SE-03-04 | verified | root | all failure branches prove payload was not launched |
| SE-03-06 | Implement run launch with process-group ownership, bounded stdout/stderr capture, timeout, cancellation, and exit evidence | SE-03-04, SE-03-05 | verified | root | leak, truncation, timeout, and cancellation tests |
| SE-03-07 | Implement minimal child-environment construction and production credential/endpoint sentinels | SE-03-01 | verified | root | allowlist matrix and no-parent-env inheritance test |
| SE-03-08 | Implement denial/config evidence normalization without inventing unavailable OS-native logs | SE-03-05, SE-03-06 | verified | root | posture distinguishes observed denial, native log, and unavailable evidence |
| SE-03-09 | Implement cleanup verification for process group, sockets, temp writes, and randomized outside sentinel | SE-03-06 | verified | root | forced-crash and orphan-child tests |
| SE-03-10 | Qualify one exact native sandbox tuple as the MVP execution path | SE-03-01…SE-03-09 | blocked | root | exact binary/platform/config parent+child filesystem, network, loopback, credential, fallback, timeout, cleanup, snapshot-CWD, dependency, and runtime-identity proof |

## Stop conditions

- If loopback cannot be allowed while unapproved egress is denied, the tuple is
  rejected. Do not enable unrestricted network to make the test pass.
- If spawned children do not inherit confinement, the tuple is rejected.
- If unavailable sandbox support falls back to raw process execution, the tuple
  is rejected unless the invocation can disable that fallback and the disabled
  behavior is proven.
- If denial logs are unavailable but effect denial is independently proven,
  record `native_denial_log: unavailable`; never fabricate a log. ADR-0020
  decides whether that limits support or only evidence richness.
- An inherited-host driver is not an alias for Codex standalone. It needs its
  own configuration identity and probe result.
- `maxProcesses` remains required profile data and part of the exact probe/run
  binding, but the native tuple does not enforce it. Evidence must label this
  limit unsupported and must not infer a count ceiling from process-group cleanup.

## Exit criteria

- [ ] The exact driver/platform/profile tuple passes the required pre-launch
      parent and child probes; no fallback or project launch occurs during qualification.
- [ ] Project payload cannot run if probe, version, config digest, posture,
      snapshot CWD, dependency identity, or runtime identity
      verification fails.
- [ ] Parent and child restriction proof covers filesystem, network, loopback,
      credentials, fallback, timeout, and cleanup on the exact tuple.
- [x] No command is assembled through a shell. Output, duration, cancellation,
      and process-group cleanup are bounded; process count is explicitly unsupported.
- [x] Probe cleanup proves no run-owned process, socket, or temp residue.
- [x] No code is added to `provider.HookEngine` or `hookflow`.

## Progress log

| Date | Task | Change | Evidence |
|---|---|---|---|
| 2026-08-21 | SE-03-01 | Started the minimal sandbox driver contract after every Phase 02 exit gate verified | Reuse the accepted sandbox-posture schema and qualified Codex candidate; add only closed data types, mapping, and a test fake under `cli/internal/assurance`; no launcher, probe execution, process management, host switching, SDK path, provider hook, or live support claim is authorized by this task |
| 2026-08-21 | SE-03-01 verified; SE-03-02 started | Added the four-method assurance-only SPI, closed failure vocabulary, exact schema mapping, and canonical binding over executable bytes, effective configuration, immutable snapshot, declared roots/targets/environment/timeout, and child requirement | `evidence/sandbox-driver-contract-validation.json`; focused count-10, race, all-assurance, vet, pinned Ajv 7-schema/83-negative replay, and Linux/Windows compilation passed; independent read-only review approved the final hashes; no launcher or support claim exists |
| 2026-08-21 | SE-03-02 verified; SE-03-03 started | Added tuple-specific non-executing discovery for the exact SE-00-05 Codex file plus canonical effective-configuration hashing; no PATH lookup, alias, version subprocess, or candidate widening exists | `evidence/sandbox-identity-validation.json`; the real 214716336-byte executable matched the pinned digest, while alias/FIFO/replacement/in-place/byte/config drift tests, count-10, race, all-assurance, vet, and cross-compilation passed; independent review approved the final hashes; identity remains point-in-time and must be rechecked before launch |
| 2026-08-21 | SE-03-03 verified; SE-03-04 started | Added one bounded JSON helper that repeats filesystem, IP-literal HTTP, environment-name, deterministic output, and cancelable-wait checks in parent and child; managed mode accepts only an explicit loopback HTTP proxy, retains its digest, and is part of the probe binding | `evidence/sandbox-probe-helper-validation.json`; adversarial redirect/body/input/FIFO/symlink/closed-port/remote-proxy/secret tests, count-10, race, vet, schema replay, and Linux/Windows compilation passed; independent review approved the final hashes; observations remain non-verdict evidence |
| 2026-08-21 | SE-03-04 verified; SE-03-05 started | Added exact Codex 0.148.0 standalone argv construction with no shell path, exact qualified loopback/config controls, and a private freshly created empty `CODEX_HOME` token whose path identity, mode, and emptiness are rechecked and whose content identity is configuration-bound | `evidence/sandbox-codex-invocation-validation.json`; exact digest golden, drift/metacharacter/state tests, count-10, race, all-assurance, vet, format, and Darwin/Linux/Windows compile-only checks passed; independent review approved final hashes; no Codex or payload was executed, and launch-time state revalidation remains required |
| 2026-08-21 | SE-03-05 verified; SE-03-06 started | Added one hidden built-in helper re-entry and a probe-only executor with exact typed envelope checks, launch-time Codex/helper/backend/state identity revalidation, protected-sentinel pre/post truth, allowed/denied/direct network observations in parent and child, bounded process-group execution, and closed failure mapping; the API has no project-payload or unsandboxed-fallback path | `evidence/sandbox-probe-execution-validation.json`; zero-launch preflight regressions, fallback precedence, child inheritance, output/timeout/cancel, hidden re-entry, count-10, race, vet, schema replay, format, and Darwin/Linux/Windows compile-only checks passed; independent review approved core source; no live Codex/sandbox/project/SDK/network/provider execution or support claim occurred |
| 2026-08-21 | SE-03-06 verified; SE-03-07 started | Added one-shot expiring probe authorization bound to the exact private Codex state, exact probe/posture/config/helper correlation, bounded run output/time/cancel/exit evidence, and process-group cleanup with bounded absence verification on every exit; no shell, ambient environment, retry, fallback, or driver switch exists | `evidence/sandbox-run-execution-validation.json`; focused count-10, process-leak count-100, package/race/all-assurance/vet/format and Darwin/Linux/Windows compile-only checks passed; independent review approved the task; max-process enforcement remains an explicit unresolved Phase 03 exit/support gate rather than a cleanup-derived claim; no live Codex/sandbox/project/SDK/network/provider execution occurred |
| 2026-08-21 | SE-03-07 verified; SE-03-08 started | Added one closed child-environment builder with mandatory loopback OpenBox URL, generated test credential, and private temp root; exact private values are correlated from probe to run while the public posture binds names only; ambient production coordinates abort before either launch and subprocesses never inherit the parent environment | `evidence/sandbox-environment-validation.json`; focused count-10, package/race/all-assurance/vet/format, pinned Ajv contract replay, and Darwin/Linux/Windows compile-only checks passed; independent review approved the lean boundary; Phase 04 must correlate omitted control variables with exact safe initializer literals; no live Codex/sandbox/project/SDK/provider execution occurred |
| 2026-08-21 | SE-03-08 verified; SE-03-09 started | Added one opaque canonical probe-evidence record that binds the exact binary, effective/helper configuration, and original probe envelope; effect denial comes only from exact helper observations, while native logging requires one exact record per parent/child effect and otherwise remains explicitly unavailable; raw logs, paths, environment values, and secrets are excluded | `evidence/sandbox-evidence-normalization-validation.json`; focused count-25, package/race/all-assurance/full-CLI/vet/format and Darwin/Linux/Windows compile-only checks passed; independent review approved final hashes; no live Codex/sandbox/project/SDK/provider execution occurred |
| 2026-08-21 | SE-03-09 verified; SE-03-10 started | Added a read-only cleanup verifier whose opaque run result binds the exact process-group exit proof, sole writable TMPDIR, isolated CODEX_HOME, loopback sockets, and parent/child outside sentinels; canonical evidence carries a resource-binding digest and only private clean provenance can project a pass | `evidence/sandbox-cleanup-validation.json`; abrupt-parent/orphan, residual socket/path, cross-binding, mutated-count, count-25, race, all-assurance/full-CLI/vet/format and Darwin/Linux/Windows compile-only checks passed; independent review approved final hashes; max-process enforcement and live tuple execution remain unresolved support gates |
| 2026-08-21 | SE-03-10 preflight not runnable; owner decision required | The exact Codex tuple cannot enforce the accepted `maxProcesses=32` budget: `RunSpec` and the pinned Codex argv/config have no count-control surface, while the proven process group owns cleanup only; project launch and a positive support row therefore stop before live execution | `evidence/sandbox-tuple-preflight.json`; source/config/profile hashes and zero-hit enforcement search recorded; options are a new accepted hard-cap driver/enforcement ADR or keeping the MVP execution path not runnable; no live sandbox or project run occurred |
| 2026-08-21 | SE-03-10 owner amendment accepted; Docker candidate selected | Codex remains rejected; the successor is the installed Docker Desktop `4.86.0` / Engine `29.7.2` linux/arm64 tuple with a digest-pinned Node `26.7.0` image and a closed single-container envelope using `--network=none` and `--pids-limit=32` | `evidence/sandbox-driver-selection.json`; official current docs plus read-only local CLI/daemon/image-manifest inspection; image absent locally, no pull/container/project run occurred, and support remains unclaimed pending separately authorized live qualification |
| 2026-08-21 | SE-03-10 static process-budget prerequisite implemented | Added one `MaxProcesses` field to probe and run contracts, closed it to the accepted schema range `1..256`, included it in the canonical probe/posture binding, rejected run mismatches, and made every exported Codex build/probe/run boundary return `unsupported` before discovery or launch for a valid budget | `evidence/sandbox-driver-selection.json`; focused count-25, race, all-assurance, full CLI, vet, format, and Darwin/Linux/Windows compile-only proofs passed; independent read-only review approved final hashes; no Docker adapter, pull, container, project, SDK, provider, or production execution occurred |
| 2026-08-21 | SE-03-10 fail-closed Docker serializer implemented | Added exact digest-pinned Docker argv construction with explicit none network, private cgroup namespace, PID cap, read-only mounts/root, bounded tmpfs, dropped capabilities, non-root user, disabled persistent logging, direct Node entrypoint, names-only child environment, and no shell; an opaque private state owns and rechecks the exact package bootstrap and CID path, and the current package-owned bootstrap throws before project launch | `evidence/sandbox-driver-selection.json`; focused count-25, package count-25, race, all-assurance/full-CLI/vet/format and Darwin/Linux/Windows compile-only proofs passed on Go `1.27.0`; independent read-only review approved hashes `dc5af221...` and `ea43e22e...`; no Docker daemon command, image, container, project, SDK, receiver, fixture, provider, or production execution occurred, so SE-03-10 and the phase remain in progress |
| 2026-08-21 | SE-03-10 staged Docker prestart boundary implemented | Replaced the direct-run candidate with package-private create, full CID correlation, strict bounded inspect, opaque start authorization, and exact-ID forced removal; one fresh empty Docker client config is identity/emptiness checked, configuration-bound, and reused for every command, while the selected image config fixes the only image-owned environment defaults | `evidence/sandbox-driver-selection.json`; Docker-focused count-25, package race, all-assurance/full-CLI/vet/format, Linux arm64/amd64 and Windows compile-only proofs passed on Go `1.27.0`; independent read-only review approved hashes `662b72d2...`, `7c24a4bc...`, and `9f891b14...`; no daemon mutation, image acquisition, container, executor, functional bootstrap, or live probe occurred, so SE-03-10 and Phase 03 remain in progress |
| 2026-08-21 | SE-03-10 Docker CLI executor stopped as `not_runnable` | A bounded create executor was removed after independent review proved a cleanup authority gap: cancellation can kill the client before stdout/CID while the daemon may commit later, and a name/label not-found lookup can race that commit; the approved static serializers remain non-executing | `evidence/sandbox-driver-selection.json`; the final package returned to the independently approved hashes and focused tests passed; no daemon command ran; owner decision is required between an accepted durable control-plane recovery/idempotency protocol with live qualification or rejection of the Docker tuple, so rollup remains 33/77 |
| 2026-08-21 | SE-03-10 parent/phase authority summary reconciled | Updated the phase stop condition and parent architecture, recommendation, and risk rows to match the recorded `not_runnable` Docker CLI cleanup result; no candidate, gate, or task status changed | Markdown/JSON parse and whitespace checks passed; `evidence/sandbox-driver-selection.json` remains the exact evidence record; no code, Docker command, image, container, or external control-plane action occurred, and rollup remains 33/77 pending the same owner decision |
| 2026-08-21 | SE-03-10 verified; Phase 03 verified by reasoned rejection | Applied the owner-approved lean recommendation: reject the Docker tuple rather than add a new control-plane recovery protocol; both exact candidates now fail closed before project launch, and no fallback is selected | `evidence/sandbox-driver-selection.json` records `candidates_rejected`, `reject_docker_tuple`, exact tuple/static hashes, the canceled-create cleanup gap, and zero live Docker mutations; JSON/Markdown/whitespace and changed-scope checks passed; live parent/child and Docker cleanup gates are explicitly `not_applicable`, not passes |
| 2026-08-22 | SE-03-10 reopened; native-only owner amendment accepted | Restore the exact Codex native sandbox as the sole MVP execution path, record `maxProcesses` as declared but unsupported rather than hard-enforced, and remove Docker code and architecture from the delivery path | Owner explicitly approved native reuse and no Docker; no project launch occurs until the exact Codex tuple re-passes the required parent+child filesystem, network, loopback, credential, fallback, timeout, and cleanup probes |
| 2026-08-22 | SE-03-10 verified; Phase 03 reverified on the native tuple | Removed the alternate-driver implementation and the artificial process-budget rejection, re-pinned the installed Codex drift from `0.148.0` to exact `0.149.0`, and qualified it as the sole MVP path; `maxProcesses` remains bound but explicitly unsupported | `evidence/codex-sandbox-requalification.json` is byte-identical across two live probe runs and `evidence/sandbox-driver-selection.json` binds the exact binary/source tuple; parent/child filesystem, network, loopback, credential, fallback, timeout, inheritance, and cleanup passed; focused count-25, race, all-assurance/full-CLI, vet, format, and cross-compilation passed; no project or model launched |
| 2026-08-22 | SE-03-10 reopened by the first project startup probe | The native profile's exact `allow_local_binding=false` setting prevented the project from opening its required literal-loopback application listener, so the prior tuple was insufficient for project execution; only that setting and its exact qualification probe are being amended | The opt-in SE-05-02 startup test reached the native sandbox and exited on `listen EPERM 127.0.0.1:<run-port>` with a matching `network-bind` denial before readiness; SE-05-02 returned to implemented while SE-03-10 is the sole in-progress root task |
| 2026-08-22 | SE-03-10 reverified with the project-required loopback bind | Changed only the exact native profile's local-binding control to `true`; parent and spawned child both opened literal-`127.0.0.1` listeners and direct loopback sockets while direct external sockets, unlisted proxied HTTP, and outside writes remained denied | `evidence/codex-sandbox-requalification.json` repeated byte-identically and `evidence/sandbox-driver-selection.json` binds the amended config; fallback, timeout, process cleanup, credential absence, inheritance, focused count-25/race/full-assurance/vet/format/cross-compile proofs passed; no scenario, model, sink, provider, Docker, or unsandboxed retry ran |
| 2026-08-23 | SE-03-10 reopened and blocked by endpoint isolation | A model-free exact-tuple probe reached the declared and undeclared `127.0.0.1` listeners with status 200. The existing probe used another loopback host (`127.0.0.2`), so it did not prove the plan's approved-versus-unapproved endpoint gate | `evidence/codex-loopback-port-isolation-review.json`; public probe execution and qualified-posture construction now fail `unsupported` before payload launch. A replacement runner or configuration must prove exact endpoint isolation before this task can close |
| 2026-08-24 | SE-03-10 successor candidate: first-party Seatbelt profile | Both rejected candidates were port-blind because neither CLI exposes the per-port loopback rule Seatbelt itself has: Apple's own `/usr/share/sandbox/*.sb` ship `(allow network-outbound (remote ip "localhost:62078"))`, and SRT uses that same form internally for its proxy ports while exposing only the all-or-nothing `allowLocalBinding`. A live spike proved an OpenBox-owned profile distinguishes an approved from an unapproved port on ONE loopback host — with the unapproved listener provably live, which the 2026-08-23 probe lacked — for the parent and a spawned child, and denies external egress and outside writes. Claude Code's own Bash sandbox was ruled out separately: its documented scope is Bash subprocesses only, with file tools, in-process `WebFetch`, and MCP servers outside it | `evidence/seatbelt-loopback-isolation-spike.json` |
| 2026-08-24 | Seatbelt driver implemented through its launch boundary; SE-03-10 still `blocked` | Added profile generation with fail-closed SBPL path validation, a sealed run-owned profile state, backend identity binding, invocation, probe, and run entry points. The probe and run boundaries were generalized onto one `driverState` seam rather than forked per driver, so `executeProbe`/`executeRun` are now shared and the Codex lane's tests and frozen posture fixture pass unchanged. Two non-obvious findings are pinned by tests: a listener needs `network-inbound` as well as `network-bind`, and a sealed profile must be proven to cover the exact probe envelope or the probe's denial observations prove nothing about the launch. **Not yet qualified**: the CLI is not wired to the driver, and no end-to-end project run has happened, so the tuple remains unsupported | `cli/internal/assurance/sandbox/seatbelt*.go`; all 11 modules green under `-race`, both cross-compiles clean |
| 2026-08-24 | SE-03-10 verified on the first-party Seatbelt driver; Phase 03 verified | The stop condition is met by an OpenBox-owned profile: deny by default, then an exact allow list per declared loopback endpoint and bind port. The probe now stands the unapproved endpoint up as a LIVE listener on the same host and proves it reachable outside the sandbox first, so its denial is falsifiable — the weakness that invalidated the 2026-08-22 row. Four deterministic runs and one Ollama-relay run of the pinned Mastra fixture completed with `stimulus-status 200`, `sdk-readiness ready`, one recordingTool event, cleanup evidence, byte-identical source and no residue. Two shared-code defects were found only because a proxy-less driver ran for the first time: `projectRunEnvironment` hard-coded Codex's single-variable prefix, and `requiredChecksPassed` demanded a proxy's 403 for every denial. Codex and SRT stay unsupported and unreachable as fallbacks | `evidence/seatbelt-driver-qualification.json` |
| 2026-08-25 | SE-03-10 and Phase 03 re-blocked; Seatbelt support withdrawn | Live review proved Seatbelt's accepted local TCP syntax permits `0.0.0.0:<declared-port>` as well as loopback and the generated profile permits reads outside the snapshot. The runner also dropped its validated snapshot CWD and accepted Node 22 outside the claimed tuple; the CWD defect is fixed, rejection predicates are pinned by tests, and both selectable drivers fail before project/profile reads | `evidence/seatbelt-driver-withdrawal.json`; the prior qualification remains historical functional evidence only |
