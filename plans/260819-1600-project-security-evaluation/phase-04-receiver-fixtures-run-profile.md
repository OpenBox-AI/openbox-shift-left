# Phase 04 — Local receiver, fixtures, and run profile

## Context

- Parent: [plan.md](plan.md)
- Depends on: [Phase 02](phase-02-inspection-and-sdk-coverage.md) and
  [Phase 03](phase-03-native-sandbox-driver.md)
- SDK source baseline: local `../openbox-mastra-sdk` clone at the SE-00-04 pin
- Architecture: [`dc/security-evaluate.md` — Delivery form](../../dc/security-evaluate.md#delivery-form)

## Goal

Provide all safe, observable services around the project: a run-scoped receiver
for normal SDK traffic, poisoned dependency fixtures, mock effect sinks, an
optional tightly bounded model relay, and a versioned HTTP run profile. Nothing
in this phase judges security behavior.

**Effort:** 6 engineer-days

**Status:** verified

**Dependencies:** SDK descriptor and artifact contracts. A supported sandbox
tuple is unavailable after the accepted Phase 03 rejection, so component and
contract tasks may proceed, but project launch and any sandbox-dependent live
exit proof remain `not_runnable`.

## Receiver contract

The baseline receiver:

- binds a random loopback port and refuses non-loopback listeners;
- generates one run-scoped `obx_test_...` bearer and attributes every request by
  that bearer rather than trusting a project-supplied run ID;
- implements only the qualified SDK subset:
  `GET /api/v1/auth/validate` and
  `POST /api/v1/governance/evaluate`;
- validates method, path, auth, content type, size, schema, and the fixed
  authenticate-once-then-evaluate sequence;
- stores exact bounded request bytes in memory long enough to normalize/judge,
  then persists only redacted projections and content digests;
- returns a schema-conformant baseline `ALLOW`; it never returns a hard-coded
  `BLOCK` as proof of policy behavior;
- rejects unknown routes and records approval polling as unsupported; and
- exposes a readiness endpoint only to the parent orchestrator, not the project
  identity.

The project's child environment receives `OPENBOX_URL` and
`OPENBOX_API_KEY` plus `OPENBOX_VALIDATE=true`,
`OPENBOX_GOVERNANCE_POLICY=fail_closed`, and
`OPENBOX_SEND_ACTIVITY_START_EVENT=true` only when the matching `withOpenBox`
option is omitted. The descriptor rejects conflicting or dynamic explicit
options before launch. Production OpenBox coordinates, keys, DIDs, and private
keys are removed; the unsigned baseline injects no identity key. The original
source and config files are not edited.

## Run-profile v1

The JSON profile declares only repeatable integration details:

- HTTP health check and startup timeout;
- HTTP stimulus method/path/body template and response completion condition;
- project listener address/port environment binding;
- poisoned dependency and mock-sink environment bindings;
- SDK descriptor and required action classes;
- optional model relay/provider/model/destination/data-posture declarations;
- process, request, token, cost, duration, output, and content-retention budgets;
- explicitly allowed environment variable names and non-secret values; and
- cleanup grace period.

The project command remains explicit after `--` on `project test`; a profile
cannot hide arbitrary executable code. Profile values cannot override the
receiver identity, output root, sandbox configuration, or production-endpoint
sentinels.

Before JSON parsing, the profile reader rejects files larger than 262,144 bytes
or lexical object/array nesting deeper than 32. The stimulus body template is
limited to depth 16 and 65,536 serialized UTF-8 bytes and must also fit
`maxRequestBytes` after substitution. Completion statuses are final HTTP
responses in the closed 200–599 range; informational 1xx responses never
complete a stimulus.

## Fixture services

The first scenario needs three loopback services:

1. a deterministic retrieval/tool fixture that returns run-specific poisoned
   content and receipts;
2. a mock egress sink that accepts only the run's synthetic marker and records
   headers/body digest plus arrival order; and
3. for live qualification, the exact local Ollama relay that accepts a one-time
   child bearer, forwards only to the pinned literal-loopback server and
   digest-bound Granite model with zero monetary cost and token/time bounds,
   and records the declared prompt/completion exposure. CI uses a deterministic
   local model fixture and labels those results as harness conformance, not
   model security evidence.

## Task ledger

| ID | Task | Depends | Status | Owner | Evidence required |
|---|---|---|---|---|---|
| SE-04-01 | Implement loopback listener lifecycle, run-scoped identity, readiness, and bounded request server | SE-01-01, SE-03-10 | verified | root | non-loopback bind rejection and lifecycle tests |
| SE-04-02 | Implement qualified auth/validate and baseline evaluate wire responses from SDK conformance fixtures | SE-00-04, SE-04-01 | verified | root | real SDK client contract test |
| SE-04-03 | Implement request validation, ordering, in-memory raw capture, redacted persistence, digesting, and caps | SE-04-01, SE-04-02 | verified | root | malformed/auth/size/redaction/truncation tests |
| SE-04-04 | Implement run-profile parser/validator and HTTP entrypoint driver with template allowlist | SE-01-01 | verified | root | schema fixtures plus size/depth/SSRF/path/template negative cases |
| SE-04-05 | Implement poisoned dependency fixture with run attribution, deterministic response, and receipts | SE-04-04 | verified | root | concurrency/isolation and replay tests |
| SE-04-06 | Implement mock effect sink that accepts only the synthetic marker and records independent receipts | SE-04-04 | verified | root | wrong-run, duplicate, oversized, and secret-rejection tests |
| SE-04-07 | Implement deterministic local model fixture for CI and mark its evidence class as harness-only | SE-04-04 | verified | root | scripted tool-selection trace with no external network |
| SE-04-08 | Implement the exact local Ollama model relay with fixed destination/model digest, one-time bearer, and byte/token/time/zero-cost budgets | SE-03-10, SE-04-04 | verified | root | destination bypass, secret leak, streaming, timeout, model-drift, and budget tests |
| SE-04-09 | Integrate service start/readiness/shutdown receipts and child env overlays into `project test` preflight | SE-04-01…SE-04-08 | verified | root | forced failure at every lifecycle stage leaves no process/socket |

## Security tests

- Requests without the exact one-time bearer receive no useful response and are
  recorded without echoing the credential.
- For the project identity, exactly one successful auth validation must precede
  zero or more evaluations. Evaluate-before-auth and duplicate auth are rejected;
  every accepted or rejected request consumes the shared request budget.
- Production-looking `obx_live_...` keys, configured production OpenBox URLs,
  DID/private-key variables, and provider keys in the child environment abort
  before launch.
- The receiver cannot be rebound to `0.0.0.0`, a LAN address, or a caller-chosen
  privileged port.
- Fixture templates cannot resolve arbitrary URLs, files, environment values, or
  shell expressions.
- The model relay cannot be used as a generic proxy; host, path family, model,
  method, byte/token/cost budget, and redirect behavior are pinned.
- Raw persistence is not a v1 mode. `redacted_digests` is required; redaction
  failure omits the content and marks evidence inconclusive without a raw
  fallback.
- Cleanup handles a project crash, parent cancellation, hung connection,
  streaming response, and child fork.

## Exit criteria

- [x] The pinned SDK descriptor, service set, and child overlay are separately
      verified. Their combined project launch is a Phase 05 startup/run proof,
      not a Phase 04 component claim.
- [x] Baseline response bytes are accepted by the real SDK and do not exercise
      a production decision service.
- [x] Fixture and sink receipts are run-attributable independently of SDK fields.
- [x] CI has a fully local deterministic harness path; live behavior has a
      bounded provider-relay path with data/cost disclosure.
- [x] Unknown endpoints, approval polling, wrong identity, oversized content,
      and production coordinates fail explicitly.
- [x] Every service and secret/token has run-owned cleanup evidence.

## Progress log

| Date | Task | Change | Evidence |
|---|---|---|---|
| 2026-08-21 | SE-04-01 started | Began the smallest sandbox-independent receiver lifecycle after Phase 03 closed by reasoned rejection; no project launch, Docker executor, recovery protocol, SDK route, fixture, relay, or judgment is part of this task | Phase 03 and `evidence/sandbox-driver-selection.json` record both exact tuple rejections; SE-04-01 is dependency-ready for loopback-only component tests, while all sandbox-dependent live claims remain `not_runnable` |
| 2026-08-21 | SE-04-01 verified; SE-04-02 started | Added only an ephemeral IPv4-loopback server, distinct run-scoped project/readiness identities, exact one-shot readiness, closed project/readiness request bounds, body/header/time caps, and bounded idempotent shutdown; SDK routes remain absent until SE-04-02 | `evidence/receiver-lifecycle-validation.json`; focused count-25, race, affected-assurance, full CLI, vet, format, and Darwin/Linux/Windows compile-only checks passed; independent review approved exact hashes; real local loopback lifecycle tests ran, but no SDK, project, sandbox, Docker, provider, production, or control-plane action occurred |
| 2026-08-21 | SE-04-02 verified; SE-04-03 started | Added only the exact auth/evaluate routes, immutable auth and baseline-ALLOW bytes, auth-once ordering, and absent approval polling; the exact Node `26.7.0` client from the accepted Mastra/base-SDK tuple completed real loopback auth/evaluate and interpreted ALLOW as `continue` | `evidence/mastra-receiver-wire-validation.json`; source archive, Node binary, package/lock, complete Mastra dist, and complete base-SDK package trees were exact-bound; the runtime fetch guard allowed only the exact receiver origin/routes; focused count-25, race, affected/full CLI, vet, format, cross-compile, and independent review passed; no project, sandbox, Docker, provider, production, or control-plane execution occurred |
| 2026-08-21 | SE-04-03 verified; SE-04-04 started | Closed request validation and evidence retention around one serialized, bounded state machine: exact bearer attribution, closed route/method labels, canonical body validation, processing-order records, saturating admission, one overflow record per budget, exact known-bearer rejection before hashing, generic redacted-projection digests, and inconclusive no-content failures | `evidence/receiver-request-validation.json`; malformed, duplicate, content-type, sequence, readiness-body, truncation, redaction-failure, known-secret, unsupported-URI, concurrent-order, and concurrent-overflow tests passed; a fresh exact Mastra client replay passed after the final validator changes; focused count-25, race, affected/full CLI, vet, format, cross-compile, and independent review passed; no project, sandbox, Docker, provider, production, BLOCK decision, or control-plane execution occurred |
| 2026-08-21 | SE-04-04 verified; SE-04-05 started | Added one closed v1 parser, typed whole-value template renderer, and literal-loopback HTTP driver; exact schema shape, binding correlations, budgets, template vocabulary, path/SSRF boundaries, non-followed 3xx handling, shared duration deadline, and cleanup reserve are enforced without command, environment, relay, or service authority | `evidence/run-profile-driver-validation.json`; exact boundary/adversarial fixtures, count-25, race, affected/full CLI, vet, format, cross-compile, and independent review passed; authorized relay remains deliberately unavailable until a package-owned SE-04-08 descriptor exists; no project, sandbox, provider, production, BLOCK decision, or control-plane execution occurred |
| 2026-08-21 | SE-04-05 verified; SE-04-06 started | Added one package-owned poisoned dependency service on an ephemeral literal-loopback listener with a private generated route, canonical bounded response, marker-digest attribution, saturating admission, content-free ordered receipts, and independently deadline-bounded idempotent cleanup | `evidence/poison-fixture-validation.json`; same-input replay, two-run concurrent isolation, wrong host/method/path/query/body, overflow, cancellation, count-50, race count-10, affected/full CLI, vet, format, cross-compile, and independent review passed; response shape remains private fixture wire data, and no consumption, effect, project, sandbox, provider, production, or BLOCK claim was made |
| 2026-08-21 | SE-04-06 verified; SE-04-07 started | Added one package-owned safe effect sink on an ephemeral literal-loopback/private route; exactly one canonical marker body is accepted, duplicates and wrong runs reject, complete normalized safe headers and canonical bodies receive independent digests, and rejected/oversized/secret-bearing content receives none | `evidence/effect-sink-validation.json`; wrong-run, duplicate, oversize, secret name/value/trailer, wire-shape, concurrent isolation, count-25, race count-5, affected/full CLI, vet, format, cross-compile, and independent review passed; sequence is processing order, and no application causality, SDK gate, sandbox, provider, production, or security verdict was inferred |
| 2026-08-21 | SE-04-07 verified; SE-04-08 started | Added one package-owned private scripted tool-selection harness on an ephemeral literal-loopback/private route; exact marker-only requests return deterministic canonical `recording-tool` selections labeled `harness_only`, while all receipts retain that evidence class and rejected content receives no promotable digest | `evidence/model-harness-validation.json`; replay, cross-run isolation, malformed/wrong/oversized/secret-bearing requests, count-25, race count-5, affected/full CLI, vet, format, cross-compile, and independent review passed; this is not a provider/model/Mastra protocol and proves no real model behavior, security verdict, project path, or sandbox behavior |
| 2026-08-21 | SE-04-08 implemented; live verification not runnable without owner authority | Added one package-owned OpenAI Responses relay that pins the dated model/origin/path/method/tool and text-only input, keeps the provider credential host-side, gives the child one generated bearer, disables proxy/redirect/retry/stream/store authority, and records bounded redacted request, token, provider-attempt, and conservative cost evidence | `evidence/provider-relay-validation.json`; destination/model/path/method bypass, bearer replay, image/file input, secret, proxy, redirect, timeout, streaming/oversize, token/spend, usage-drift, count-25, race count-5, affected/full CLI, vet, format, cross-compile, and independent review passed against local TLS fakes; no provider credential or paid/live request was authorized, so current model/token-count/pricing behavior is unobserved, SE-04-08 is not verified, and SE-04-09 remains dependency-blocked |
| 2026-08-22 | SE-04-08 dependency-blocked for the owner-selected Ollama replacement | Remove the unverified OpenAI-specific implementation rather than add a second provider path; the replacement must bind the installed literal-loopback Ollama server and exact Granite model digest, use its native non-streaming `/api/chat` tool-call contract, require zero monetary cost, and retain the child-facing one-time bearer and all byte/token/time/secret/generic-proxy bounds | Current local observation: Ollama server `0.31.1`, client `0.32.14`, `granite4.1:3b` digest `sha256:6fd349357287c7ffc9e38189a93b48ea175d24fc566b38f09cfc564fb7f303eb`, with `completion` and `tools`; SE-01-01 is the sole active root task because the accepted schema/ADR currently require an HTTPS external relay and positive spend |
| 2026-08-22 | SE-01-01 reverified; SE-04-08 resumed | The accepted public/compiled contract now authorizes only the exact local Ollama invocation and parent inspection tuple, zero monetary cost, one child bearer, and no OpenAI compatibility route | Strict schema/semantic/conformance replay and independent contract review passed; runtime relay proof is now the sole active root task and remains distinct from project, SDK, sandbox, ALLOW, or BLOCK claims |
| 2026-08-22 | SE-04-08 verified; SE-04-09 started | Replaced the unverified OpenAI path with one package-owned Ollama relay that fixes server/model digest/details/capabilities, parent-only version/tag inspection, child-only native chat, one-time bearer, one exact synthetic `recording-tool` request, no proxy/redirect/retry/stream/persistence authority, zero monetary cost, and truthful postflight-unavailable evidence on failed checks | `evidence/provider-relay-validation.json`; exact local server `0.31.1` and `granite4.1:3b` returned one exact tool selection with 218 input/46 output tokens; pre/post tuple inspection passed, immediate `ollama ps` was empty, count-25/race count-5/all-assurance/full-CLI/vet/format/cross-compile passed, and independent review approved final hashes; no external provider, credential, paid API, project, SDK interception, sandbox, ALLOW, BLOCK, or control-plane claim was made |
| 2026-08-22 | SE-04-09 verified; Phase 04 closed | Added only the reusable service preflight: unsafe SDK or ambient production coordinates fail before listeners, receiver/sink/poison/model services share one cleanup-reserved run context, exact readiness precedes a closed sorted child overlay, and normal close/cancellation/every injected lifecycle failure removes all started listeners and clears launch-only values | `evidence/service-preflight-validation.json`; count-25, race count-5, cancellation count-50, all-assurance, full CLI, vet, format, cross-compile, and independent review passed; the first full replay had one 2 ms timing overrun and passed 10/10 plus full-suite replay; combined project/SDK-overlay launch proof is formally `not_applicable` after both sandbox tuples were rejected, not promoted to a pass |
