# ADR-0021 — Versioned OpenBox Sandbox ProjectRun v2

Date: 2026-08-22
Status: Historical design direction; implementation deferred and superseded for Shift Left execution

Amended 2026-08-26: ProjectRun v2 remains unimplemented and unauthorized. It
is not a fallback for the lean OpenShell development-evaluation workflow. The
associated implementation plan is retained as historical design material only.

## Context

Shift Left has no currently supported project runner. The former exact native
Codex tuple was withdrawn after it failed endpoint-scoped loopback isolation.
Docker Compose may run the disposable local OpenBox control/data plane for a
governed rerun, but it never launches or confines the evaluated project.

OpenBox Sandbox v1 is a strict single-command service. Its request creates an
empty workspace and exposes no project snapshot, environment, secrets, mounts,
service topology, dependency preparation, readiness/stimulus, or artifact
retrieval. Stretching v1 with host mounts, inherited environment, or a shell
bootstrap would weaken its existing boundary.

Phase 08 compared keeping native execution, wrapping v1, adding a versioned v2
capability, and building a second service. Its evidence is recorded under
[`plans/260819-1600-project-security-evaluation/evidence/`](../../plans/260819-1600-project-security-evaluation/evidence/).

## Decision

### Keep native execution fail-closed

No native tuple is currently supported. This ADR does not add a driver,
endpoint, service, protocol schema, dependency, or support tuple. Docker
Compose remains system-plane-only and is not an automatic fallback.

### Preserve v1 exactly

OpenBox Sandbox v1 keeps protocol version `1`, its existing request/response
bytes, empty workspace/environment semantics, and conformance suite. No
ProjectRun field is added to a v1 request and no v1 wrapper is permitted.

### Design ProjectRun as an explicit v2 capability

A later implementation may add a versioned capability beside v1 only with:

- protocol version `2`;
- media type `application/vnd.openbox.project-run.v2+json`;
- capability name `project_run_v2` negotiated before mutation;
- content-addressed immutable inputs and prepared-tree identity;
- closed non-secret environment and secret-reference contracts;
- logical private services and default-deny egress;
- one-shot probe-to-run authority with no fallback;
- bounded prepare, project, readiness, stimulus, observation, artifact, and
  cleanup stages; and
- a production client that exposes typed operations, not provider handles or
  arbitrary RPC.

The provider-neutral requirements, paper lifecycle/protocol, threat model, and
rollout contract are normative design inputs:

- `openbox-sandbox-projectrun-requirements.md`
- `openbox-sandbox-projectrun-protocol.md`
- `openbox-sandbox-projectrun-threat-model.md`
- `openbox-sandbox-projectrun-rollout.md`

### Keep product boundaries narrow

OpenBox Sandbox owns protocol, durable lifecycle, input/artifact storage,
provider adapters, cleanup, production client, and provider qualification.
Shift Left owns project snapshot/profile mapping, fixtures, normalized
correlation, deterministic judgments, audit packs, and explicit driver
selection. Neither side may silently recreate the other's authority.

There is no automatic migration, unsandboxed retry, native fallback, or
mid-run provider switch. After a failed ProjectRun attempt, a user may start a
fresh native run only after terminal cleanup.

### Gate implementation and release separately

Implementation is authorized only by the separate
[`OpenBox Sandbox ProjectRun v2 implementation plan`](../../plans/260822-2330-openbox-sandbox-projectrun-v2/plan.md).
That plan begins as `planned`; this ADR does not start it.

Support requires all of the following:

1. unchanged v1 golden and provider conformance;
2. closed v2 schema and lifecycle conformance;
3. every provider-neutral requirement and threat case passing;
4. exact provider/runtime/service/client identity;
5. terminal cleanup and no-fallback proof at every mutating stage;
6. a disposable Phase 05 prototype and measured benchmark;
7. Phase 05 baseline and Phase 07 governed parity through the unchanged Shift
   Left evidence interfaces; and
8. install, upgrade, drain, rollback, capacity, and cost evidence.

No provider is qualified by this decision. Current macOS SRT denial-evidence
tests fail, Linux does not yet provide the required direct-network guarantee,
and the exact OpenShell gateway tuple was unavailable locally.

## Alternatives

### Keep native-only permanently

Retained as the present product posture, but rejected as the long-term design
because it does not provide a reproducible remote/CI environment or cross-host
provider stability.

### Wrap v1

Rejected. Mounts, inherited environment, or arbitrary shell preparation would
erase the v1 security contract and still omit typed lifecycle/evidence.

### Add a separate project-run service

Deferred. It duplicates authentication, provider adapters, lifecycle recovery,
cleanup, release, and operations without a demonstrated isolation benefit.

## Consequences

- V1 compatibility is simple: v1 stays unchanged.
- The current Shift Left release remains native-Codex-only.
- V2 delivery is larger but has explicit security, conformance, operations,
  and rollback owners.
- The later prototype may fail without changing the supported product; failure
  keeps the system native-only.
- Docker Compose continues to be allowed for the disposable local OpenBox
  system plane and prohibited as the evaluated-project sandbox.

## Revisit triggers

Reconsider the selected direction if v2 cannot preserve v1, cannot close any of
the provider-neutral requirements or threats, needs ambient credentials/host
mounts/broad egress, cannot prove terminal cleanup, or fails Phase 05/07 parity.
In those cases, retain native-only or write a new ADR for a separate service.
