# OpenBox Sandbox ProjectRun v2 implementation plan

**Status:** superseded for Shift Left execution; retained as historical design
material and not authorized or started

**Decision:** [ADR-0021](../../docs/adr/ADR-0021-openbox-sandbox-projectrun-v2.md)

**Repositories:** OpenBox Sandbox owns server/protocol/provider/client work;
OpenBox Shift Left owns only consumer mapping, fixtures, evidence parity, and
explicit backend selection.

**Estimated effort:** 46 engineer-days plus provider infrastructure and live
qualification time. Estimates are planning ranges, not commitments.

## Objective

Implement a version-distinct `project_run_v2` capability beside unchanged v1,
then qualify one exact provider and prove Phase 05/07 parity before any Shift
Left support row or driver selection exists.

Docker Compose may run a disposable local OpenBox control/data plane used by a
governed test. It is never the ProjectRun sandbox or evaluated-project launcher.

## Non-goals

- no v1 field or semantic widening;
- no v1 wrapper, host worktree mount, ambient environment, or inline secret;
- no generic RPC, provider handle, arbitrary egress, or shell-bootstrap field;
- no automatic fallback to native execution;
- no SDK fork, scan mode, new Shift Left artifact schema, or judgment change;
- no release claim before exact live provider qualification.

## Required inputs

The implementation must consume, without weakening:

- `openbox-sandbox-projectrun-requirements.md` (PR01–PR15);
- `openbox-sandbox-projectrun-protocol.md`;
- `openbox-sandbox-projectrun-threat-model.md` (TM01–TM17); and
- `openbox-sandbox-projectrun-rollout.md`.

They are stored under the completed parent plan's `evidence/` directory.

## Delivery phases

| Phase | Owner | Estimate | Deliverable and exit gate |
|---|---|---:|---|
| 00 — contract freeze | Sandbox lead + security | 3d | Re-qualify exact v1 bytes/tests; accept v2 schema, media type, capability, lifecycle, budgets, error vocabulary, and compatibility vectors. Every v1 golden remains byte-identical. |
| 01 — durable run/store | Sandbox server | 6d | Begin/upload/seal/delete lifecycle, content-addressed immutable inputs, consuming capabilities, restart reconciliation, exact object sets, and terminal-absence proof. Crash/fault tests cover every mutation. |
| 02 — preparation/runtime | Sandbox provider | 6d | Immutable runtime/template, lock-bound dependency preparation, produced-tree/cache identity, closed writes/network, and bounded logs/time/processes. No shell-bootstrap authority. |
| 03 — services/identity/egress | Sandbox server + security | 7d | Logical private services, secret references, run identities, default-deny egress, model relay, cross-run isolation, credential/redaction tests, and no generic proxy. |
| 04 — execution/evidence/artifacts/client | Sandbox + client owners | 8d | Probe/start/readiness/stimulus/observe/seal/retrieve operations, normalized bound evidence, exact artifacts, cleanup, and a non-test production client. No provider SDK escapes the client. |
| 05 — provider qualification | Provider owners + security | 7d | One exact provider passes PR01–PR15, TM01–TM17, parent/child filesystem/network/loopback/credential/fallback/timeout/process/cleanup, restart, and install/upgrade tests. Other tuples remain unsupported. |
| 06 — disposable prototype/benchmark | Shift Left + Sandbox | 4d | Run the Phase 05 Mastra fixture end to end without changing v1 or public Shift Left contracts. Measure staging, cold/warm preparation, execution, evidence, cleanup, transfer, storage, and local/provider cost. Prototype remains unsupported evidence. |
| 07 — parity and opt-in consumer | Shift Left | 3d | Phase 05 baseline and Phase 07 governed pair pass through existing correlation/judgment/pack interfaces. Add an explicit backend choice only for a qualified tuple; until then execution stays unsupported and no fallback exists. |
| 08 — rollout/rollback | Release + operations | 2d | Install, upgrade, drain, cancel, credential rotation, capacity, observability, incident rollback, terminal cleanup, and support matrix pass. Publish only exact qualified identities. |

## Cross-phase gates

Each phase must:

1. preserve all current v1 conformance and source identities;
2. implement the smallest dependency-ready change;
3. run focused, package, adversarial, race/static, and phase proof suites;
4. record exact commands, versions, digests, artifacts, failures, and limits;
5. mark missing live evidence `not_runnable` or `inconclusive`, never pass; and
6. stop before the next phase if cleanup, fallback, secret, identity, or egress
   evidence is incomplete.

No prototype result may authorize release. No provider may be substituted or
retried unsandboxed when its exact tuple fails.

## Provider workstream

Qualification starts with one provider only after Phase 04:

- macOS SRT must first close its current denial-category evidence failures;
- Linux SRT must prove bypass-resistant direct and mediated network policy;
- OpenShell requires an exact gateway/image/policy/mTLS tuple and live lifecycle;
- another provider requires the same provider-neutral suite and a new exact
  support row.

Cross-provider stability is a later gate requiring two independent live
providers; it is not required to ship the first exact row.

## Rollback

Disable v2 admission, drain/cancel every owned run, prove terminal absence,
revoke scoped capabilities and credentials, retain only authorized redacted
diagnostics, and keep v1 serving independently. Shift Left returns
`not_runnable`; it never silently runs the same request natively. A new native
run requires explicit user action after cleanup.

## Start authority

This file is a separate planning ledger, not an implementation authorization.
Before work starts, repository owners must assign named owners, confirm the
exact starting commits/provider infrastructure, resolve paid/live authority,
and mark exactly one Phase 00 root task `in_progress`.
