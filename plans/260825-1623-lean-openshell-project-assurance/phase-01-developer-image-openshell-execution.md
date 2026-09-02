# Phase 1 — Run a self-starting local image in OpenShell

**Status:** verified — OS-01-01 through OS-01-04 and the public live
qualification passed on 2026-08-26.

**Parent:** [Lean local OpenShell project security evaluation](plan.md)

## Goal

Add one narrow public command that resolves a developer-supplied local OCI
image, runs its standard entrypoint once in the pinned local OpenShell
development topology, observes that its OpenBox SDK reaches the run-owned Core
ingress, and retains a truthful non-sealed execution record.

This phase proves image publication, OpenShell execution, provider-bound Core
routing, bounded capture, and cleanup. It does not collect backend behavior,
seal an observation pack, analyze security issues, generate a report, recommend
controls, or qualify OpenShell as a production confinement boundary.

## Public contract

```text
openbox project evaluate \
  --image IMAGE \
  --env-file FILE \
  --openbox-agent AGENT_ID \
  --output DIR
```

All four flags are required exactly once and positional arguments are rejected.
The developer supplies:

- a credential-free local Docker reference or image ID resolving to one
  `linux/arm64` image;
- a standard non-empty OCI `Config.Entrypoint + Config.Cmd`, with a clean
  absolute executable and absolute application or script paths;
- `Config.User` set to `1000` or `1000:1000`, the image contract label
  `ai.openbox.project-evaluation.contract=v1`, and no obsolete labels in that
  namespace;
- a strict evaluation environment file containing only non-secret values and
  no reserved OpenBox or OpenAI routing variables;
- the pre-existing bearer-observation project-agent UUID established by
  `openbox auth` with `signing_required=false`; and
- a new output path whose parent is an existing real directory.

The evaluator supports only Darwin arm64 and never accepts a project path,
Dockerfile, replacement argv, service, port, health or invoke route, mount,
upload, SSH, detached execution, forward, later `sandbox exec`, backend crawl,
provider mutation, or source-build fallback.

## Fixed local topology and prerequisites

- Pin the OpenShell CLI, Gateway, and VM driver to `0.0.111`.
- Require the exact registry image
  `registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373`
  to be preloaded. The evaluator must not pull it.
- Require the preconfigured endpointless `obx-openbox-local` provider and its
  `OPENBOX_API_KEY` placeholder. The real bearer key remains provider-side.
- Require the reusable preconfigured `openai-compatible-provider` instance,
  pointed at host Ollama through `host.openshell.internal`, and its inference
  route for `granite4.1:3b` at
  `sha256:6fd349357287c7ffc9e38189a93b48ea175d24fc566b38f09cfc564fb7f303eb`.
- Require healthy local Core, backend, and Ollama endpoints. Availability is a
  precondition, not live qualification evidence.
- Require Ollama to begin unloaded, then preload the exact model after
  preflight so the VM-local OpenShell router can use the host-backed compatible
  endpoint; unload it through the single cleanup path.
- Never create or modify an agent, signing mode, provider, route, policy,
  approval, model, or local-stack service.

Ed25519 is an optional request-attribution layer, not an observation
prerequisite. The evaluator deliberately supplies no signing seed. The
provider-held runtime bearer key is sufficient to persist the agent's real
workflow, tool, policy, and lifecycle behavior because the shared development
agent was prepared during `openbox auth` with signing enforcement disabled.
The retained record must mark cryptographic request attribution absent; that is
a coverage limitation, not a runner failure.

Every failed prerequisite must return before output or runtime resource
creation. No direct-Docker or alternate-runner fallback is allowed.

## Execution design

1. Validate platform, flags, output, environment, image metadata, resolved OCI
   argv, and every pinned local prerequisite without mutation.
2. Resolve the developer reference to its immutable local image ID, generate a
   fresh evaluation identity, and create one owner-only `.incomplete` output
   workspace.
3. Start a run-owned loopback Registry v2 container from the pinned image,
   publish the developer image, verify the published manifest and config bind
   to the inspected local image, and construct an immutable registry-digest
   reference.
4. Start a run-owned Core relay that forwards only the required OpenBox SDK
   validation, evaluation, and approval routes to local Core. Record only
   count-based receipts for the selected agent and evaluation identity.
5. Generate a canonical OpenShell policy that binds the OpenBox provider to
   the relay endpoint, permits the local inference endpoint, and exposes no
   general network or credential surface.
6. Invoke `openshell sandbox create` directly, attached and without a shell,
   using fixed CPU and memory budgets, the immutable published image, the
   generated policy, the preconfigured provider, evaluator-owned environment,
   and the resolved OCI argv after `--`.
7. Poll the exact sandbox through `Provisioning`, `Ready`, and terminal command
   exit. Treat pre-ready `Error`, an unexpected phase, non-zero command exit,
   deadline expiry, interruption, or missing matching SDK receipts as failure.
8. Capture bounded process stdout, process stderr, and combined OpenShell logs,
   then route success and every failure through the same cleanup path.

Successful state history is:

```text
preflighted -> output_created -> registry_started -> image_published
  -> core_relay_started -> sandbox_creating -> ready -> command_exited
  -> logs_captured -> sandbox_deleted -> registry_removed -> execution_recorded
```

Cleanup deletes only the exact run sandbox and exact run registry
container/tag. A cleanup failure overrides apparent execution success and is
recorded rather than hidden.

## Execution output

Retain the owner-only `.incomplete` directory on success and post-output
failure with exactly:

- `execution.json` — `ai.openbox.project-execution/v1`, including immutable
  image and runtime identities, argv, environment names, phase history,
  bounded receipt counts, cleanup results, limitations, exit classification,
  and safe error text;
- `policy.json` — the canonical policy passed to OpenShell;
- `process.stdout` and `process.stderr` — bounded command capture with
  truncation recorded in `execution.json`; and
- `openshell.log` — bounded combined OpenShell log capture.

Never retain developer environment values, provider credentials, bearer keys,
signing material, or authorization headers. This output is an execution record,
not an observation or audit pack; historical `project verify` must reject it.

## Tasks and exit evidence

| ID | Status | Implementation | Required evidence |
|---|---|---|---|
| OS-01-01 | verified | Add the strict public adapter, environment codec, no-clobber workspace input, local image resolution, and OCI platform/user/label/argv validation. | Focused tests passed and the public live invocation accepted only the conforming local Mastra image and project environment. |
| OS-01-02 | verified | Add typed direct OpenShell subprocesses for pinned preflight, attached create, phase polling, bounded log capture, and exact sandbox deletion. Expose no detach, forward, upload, exec, source-build, provider-mutation, or shell-string surface. | The live run reached `Ready`, launched the exact OCI argv, captured bounded OpenShell logs, observed command exit, and deleted the exact sandbox. |
| OS-01-03 | verified | Add run-owned immutable registry publication, canonical local-only policy generation, provider-bound Core relay, inference-route preflight, count-only receipts, and environment-value redaction. | Evidence `2026-08-26-phase-01-public-mastra-success-17` binds image ID and registry manifest/config, records one matching Core validation, six governance events, the OpenAI-compatible Granite route, and no credential values. |
| OS-01-04 | verified | Add the closed lifecycle, one cleanup path, bounded private output, and `ai.openbox.project-execution/v1` record. | The successful execution record reached `execution_recorded`; sandbox, registry tag/container/volume, and loaded-model cleanup are all true, with read-only residue checks empty. |
| Live qualification | verified | Run the exact public command against the fixed local tuple after all prerequisites are independently preconfigured. | `evidence/2026-08-26-phase-01-public-mastra-success-17` is the successful retained live record; no sandbox, run registry, registry volume, or loaded-model residue remained. |

## Verification

Run the focused deterministic suite from `cli/`:

```text
go test ./cmd/openbox ./internal/assurance/evaluate ./internal/assurance/runfs
```

Before live qualification, verify read-only that the exact registry image,
OpenShell tuple, providers, inference route, Granite digest, local services,
dedicated agent, and conforming developer image are present. Then invoke only
the public command. Direct CLI reconstruction, direct Docker execution,
provider or route creation, image pulls, and manual resource repair do not
qualify the phase.

## Phase acceptance

- Deterministic tests cover the complete public adapter, validation, immutable
  publication, policy, attached lifecycle, bounded records, adversarial output
  replacement, failure classification, and cleanup behavior.
- One live public invocation reaches `execution_recorded`, observes matching
  SDK validation and governance traffic, retains the exact private incomplete
  output, and leaves no run sandbox or registry residue.
- The retained record discloses Landlock, process-limit, bearer-only identity,
  and development-observation limitations without claiming production
  prevention or enforcement.
- The parent and phase ledgers move OS-01 tasks from `implemented` to `verified`
  only after the live evidence above exists. Deterministic tests alone do not
  meet the phase exit.

## Stop conditions

Stop before resource or output creation when the platform, local image,
registry image, OpenShell tuple, provider, inference route, model digest, local
service health, agent identity, environment, or output path is invalid. Stop
the run and enter cleanup on publication mismatch, relay or policy failure,
unexpected sandbox state, timeout, interruption, command failure, log-capture
failure, missing SDK receipts, or resource replacement.

Do not bypass a stop condition by pulling an image, configuring a provider or
route, changing agent signing, substituting a runner, widening policy, or
starting Phase 2 backend collection.
