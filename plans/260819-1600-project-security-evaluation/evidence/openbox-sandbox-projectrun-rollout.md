# ProjectRun client, conformance, provider, and rollout requirements

Task: SE-08-07  
Status: pre-implementation ownership contract.

## Production client boundary

OpenBox Sandbox owns a non-test client that exposes only:

- capability negotiation and exact service/asset identity;
- typed ProjectRun lifecycle operations from the paper protocol;
- content-addressed input upload and artifact retrieval;
- bounded normalized evidence streaming; and
- cancellation, delete, wait-deleted, and terminal-absence proof.

The client does not expose provider handles, arbitrary RPC, raw store access,
inline secret values, host mounts, arbitrary network rules, fallback selection,
or test constructors. Shift Left owns mapping its immutable snapshot/profile and
evidence contracts into this client; it does not link provider SDKs or recreate
the protocol.

Docker Compose may run the disposable local OpenBox control/data plane used by
a governed test. It is not a ProjectRun provider, sandbox, launcher, or
fallback for the evaluated project.

## Conformance layers

| Layer | Required proof | Owner |
|---|---|---|
| V1 compatibility | Existing golden frames, strict decoding, twenty-scenario provider suites, lifecycle/restart/failure tests remain unchanged. | Sandbox repository |
| V2 protocol | Version/capability skew, closed schema, frame bounds, duplicate/unknown inputs, operation idempotency, consuming capabilities. | Sandbox repository |
| Snapshot/preparation | Exact object set, traversal/link/mount/special rejection, immutable runtime, lock/cache/produced-tree identity, bounded preparation. | Sandbox repository |
| Security | All 17 threat-model abuse cases, parent/child probes, no fallback, secret-marker absence, default-deny egress, tenant isolation. | Sandbox + security reviewers |
| Lifecycle | Cancellation/crash at every mutating stage, durable reconciliation, terminal absence, unrelated-resource preservation. | Sandbox provider maintainers |
| Evidence/artifacts | Channel sequence/binding, missing/unavailable truth, retention/redaction, exact role/CID/media/bytes retrieval. | Sandbox + Shift Left |
| Consumer parity | Phase 05 baseline and Phase 07 governed scenario through the unchanged Shift Left judgment/pack interfaces. | Shift Left repository |
| Operations | Install/upgrade/restart/drain, credential rotation, cache/image lifecycle, capacity, observability, incident rollback. | Sandbox release/operator owners |

Every layer must run offline or against a declared disposable local fixture by
default. Opt-in live provider tests must fail when their tuple is requested but
unavailable; an unset opt-in may skip only when the release record labels it
`not_runnable`.

## Provider qualification matrix

| Provider | Required live proof before support | Current disposition |
|---|---|---|
| macOS native SRT | Full ProjectRun lifecycle; direct and mediated allowed/denied egress; filesystem/child/credential/fallback/timeout/cleanup; denial-log state truthful. | Not qualified. V1 lifecycle passes, but two current denial-category assertions fail. |
| Linux native SRT | Same suite plus bypass-resistant network enforcement for every enabled destination policy and process-budget enforcement. | Not qualified. Source states network-enabled bubblewrap cannot provide the macOS direct-socket guarantee. |
| OpenShell | Exact service/gateway/image/policy/mTLS tuple; ProjectRun lifecycle, restart, artifacts, threats, Phase 05/07 parity. | Not runnable locally: exact gateway tuple was not configured, so current tests skipped. |
| Any later provider | Identical provider-neutral suite and exact immutable tuple record. | Unsupported until qualified. |

One provider may earn an exact support row. Cross-provider stability requires at
least two independently live-qualified providers; source compatibility or mock
conformance is insufficient.

## Migration and rollout

1. Land the accepted ADR and versioned schema tests while v1 remains the only
   production operation.
2. Implement server lifecycle/store and production client behind an off-by-default
   capability; no Shift Left wiring yet.
3. Qualify one provider with the full conformance/security suite.
4. Run the disposable Phase 05 prototype and benchmark required by the separate
   plan; keep it evidence-only and unsupported.
5. Pass Phase 05 baseline and Phase 07 governed parity through the common Shift
   Left interfaces on an exact tuple.
6. Add an explicit `openbox-sandbox` selection only for that tuple. Until a
   tuple qualifies, no execution backend is selected and no fallback exists.
7. Publish support only after install/upgrade/drain/rollback and capacity/cost
   evidence pass.

No automatic migration, fallback, or mid-run driver switch exists. V1 clients
and services continue using version 1 without altered bytes or semantics.

## Rollback

- Disable new ProjectRun admission at capability negotiation.
- Drain or cancel each owned run and require terminal absence before downgrade.
- Preserve v1 serving and its durable records independently.
- Revoke run-scoped credentials and invalidate upload/artifact capabilities.
- Retain only redacted operational metadata needed to diagnose the rollback.
- Return future Shift Left requests as `not_runnable`; do not silently launch
  another backend after a ProjectRun attempt. A fresh run requires a separately
  selected, currently qualified tuple after the failed run is fully cleaned.

## Operational ownership and release evidence

- Sandbox maintainers own wire/schema, service store, production client,
  provider adapters, lifecycle cleanup, installation, and rollback.
- Shift Left maintainers own project snapshot/profile mapping, scenario services,
  normalized correlation/judgment, audit packs, CLI selection, and user support
  documentation.
- Security reviewers own threat-model acceptance and adversarial conformance.
- Release owners publish exact service/client/provider/image/policy/schema
  identities, supported/unsupported tuples, live repetition, performance/cost,
  and rollback results.

The separate implementation plan must assign named owners, estimates, milestones,
and repositories. This discovery record grants no implementation or release
authority.
