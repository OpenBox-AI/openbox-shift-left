# ProjectRun v2 paper protocol

Task: SE-08-04  
Status: design only; no schema, service, client, provider, or prototype code is
authorized in this goal.

## Compatibility boundary

- V1 remains protocol version `1` with its current request and response bytes.
- ProjectRun uses protocol version `2`, media type
  `application/vnd.openbox.project-run.v2+json`, and an explicit
  `project_run_v2` capability returned before mutation.
- A v1 service rejects version 2 at the existing protocol-version boundary and
  cannot decode a ProjectRun operation as `create` or `exec`.
- A v2 service continues to run the unchanged v1 conformance suite. No field is
  added to v1 `CreateRequest`, `ExecRequest`, or their responses.

## Lifecycle

```text
absent
  -> begun
  -> inputs_sealed
  -> prepared
  -> probed
  -> services_started
  -> project_started
  -> ready
  -> stimulated
  -> observed
  -> artifacts_sealed
  -> deleting
  -> terminally_absent
```

Any state from `begun` onward may transition to `deleting`. Cancellation asks
for that transition; it never starts another provider or raw process. Every
mutating response returns the same request-owned run ID, a consuming capability
for the next transition, and a durable stage receipt. An ambiguous response
retains cleanup authority and never authorizes replay.

## Operations

| Operation | Input authority | Success output | No-side-effect rejection |
|---|---|---|---|
| `capabilities` | none | exact supported versions, providers, limits, evidence classes | unavailable version/provider |
| `begin_project_run` | canonical run envelope and expected asset identities | run ID, lifecycle capability, missing object CIDs | malformed, unsupported, drifted asset/profile |
| `put_input_object` | one requested CID, byte count, media type, bounded bytes | verified object receipt | unexpected CID, digest/size/type mismatch |
| `seal_inputs` | lifecycle capability, exact object-set digest | staged snapshot receipt | missing/extra object, traversal/link/special-file shape |
| `prepare` | consuming capability, preparation recipe digest | prepared-tree/runtime/cache receipts | undeclared network/secret/write, timeout, output or digest drift |
| `probe` | prepared identity plus probe envelope | qualified/rejected posture and one-shot run capability | incomplete parent/child evidence, fallback, unsupported bound |
| `start_services` | one-shot capability, closed logical topology | private service bindings and lifecycle receipt | unknown service/destination, bind/readiness authority failure |
| `start_project` | exact argv, CWD role, environment names, secret references, budgets | process identity and start receipt | mismatch from probe/preparation, stale capability |
| `wait_ready` | fixed readiness declaration and shared deadline | bounded readiness evidence | wrong status/path/sequence, deadline exhausted |
| `stimulate` | fixed scenario ID and canonical rendered request digest | response status/digest and admission receipt | template/budget/route mismatch |
| `observe` | expected evidence channels and completion predicate | bounded normalized evidence pages with sequence/digest | missing/contradictory channel stays explicit |
| `seal_artifacts` | exact required role set and retention mode | artifact inventory/root digest | raw/secret/redaction/role/digest failure |
| `get_artifact` | listed CID and bounded byte range | bytes plus role/media/length/digest | unlisted object or range/cap violation |
| `delete` / `wait_deleted` | retained cleanup target | deletion receipt / terminal absence | unrelated identity or unverifiable absence |

Object upload and artifact download are content-addressed; retries are safe only
for an identical operation ID and digest. Lifecycle capabilities are consuming,
while read-only artifact retrieval uses a separate scoped capability.

## Canonical run envelope

The envelope is closed and canonical. It contains:

- source-manifest digest and required object CIDs;
- immutable runtime/template and preparation-recipe digests;
- dependency lock and optional cache identity;
- fixed application argv/CWD role, non-secret environment map, and secret
  references (never secret values);
- logical service topology for receiver, poison fixture, safe sink, model relay,
  project listener, readiness, and stimulus;
- default-deny logical egress destinations;
- exact SDK descriptor, scenario, required action class, and evidence channels;
- all ten budgets and the sole `redacted_digests` retention mode; and
- expected schema/asset/conformance versions.

Runtime-assigned addresses, paths, handles, ports, credentials, provider names,
and OS primitives are not part of the caller envelope. The service maps logical
roles privately and returns only the scoped bindings the child needs.

## Evidence and artifacts

Evidence pages use one closed record envelope: run ID, channel, monotonically
increasing channel sequence, source projection digest, normalized body, and
availability (`observed`, `missing`, `unavailable`, `not_runnable`). Raw provider
logs are never promoted directly. Required channels cover posture, SDK,
receiver, fixture, effect, process, egress/denial, and cleanup.

Artifacts are sealed only after project and services stop and redaction
completes. The service returns role/media/bytes/CID inventory; Shift Left still
owns the v1 audit-pack schema, deterministic judgment, projections, and
manifest-last local publication.

## Paper conformance cases

1. Every current v1 golden request/response and twenty-scenario provider suite
   remains byte/behavior identical on a v2-capable service.
2. A v1 service rejects every v2 envelope before creating durable state.
3. Version 2 with a v1 operation, version 1 with a v2 operation, unknown fields,
   duplicate keys, oversized frames, and unsupported capability all fail closed.
4. Missing/extra/mutated source objects and prepared-tree drift fail before
   project launch.
5. Ambient environment, inline secret values, host mounts, arbitrary listeners,
   unlisted egress, redirects, shell bootstrap fields, and provider selection are
   structurally unrepresentable.
6. Each lifecycle transition rejects a stale, replayed, wrong-run, or wrong-stage
   capability without mutation.
7. Cancellation/service crash at every mutating stage reconciles only the owned
   run to terminal absence; a possibly dispatched operation is never retried.
8. Parent/child probe failure, missing evidence, denial-log unavailability, and
   unsupported budgets remain rejected/inconclusive and never become qualified.
9. Output/request/token/cost/duration/process/cleanup caps terminate or reject at
   the declared boundary without widening.
10. Artifact role/CID/size/media mismatch, secret detection, or redaction failure
    prevents sealing; no raw fallback exists.
11. Phase 05 baseline and Phase 07 governed fixtures must pass through the same
    Shift Left driver/evidence interfaces before any support row is added.
12. Provider unavailable, service unavailable, and capability drift never select
    native host or raw process fallback.
