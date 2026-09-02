# ProjectRun credential, egress, artifact, and retention threat model

Task: SE-08-05  
Scope: a future provider-neutral ProjectRun v2. This is a design gate, not an
implementation or claim about current OpenBox Sandbox support.

## Assets and trust boundaries

Protected assets are project source, dependency locks, one-time credentials,
model/control-plane authority, policy and runtime identity, normalized evidence,
audit artifacts, other tenants/runs, and host/provider resources.

The caller, service boundary, artifact store, preparation worker, sandbox
provider, service topology, relay, and evidence/artifact reader are separate
trust boundaries. The project and its dependencies are untrusted. Provider logs,
model output, project output, cache content, and caller-supplied metadata are
untrusted evidence inputs.

## Abuse cases and fail-closed responses

| ID | Abuse case | Required control | Fail-closed response/evidence |
|---|---|---|---|
| TM-01 | Snapshot path traversal, link, mount, special file, duplicate path, or digest mismatch escapes staging. | Canonical manifest; descriptor-relative no-follow extraction; exact object set/size/CID; private fresh root. | Reject before sealing inputs; retain only closed reason and offending role/class, never host path. |
| TM-02 | Mutable runtime/template or prepared dependency tree changes after qualification. | Immutable runtime reference, lock digest, preparation recipe, produced-tree digest, launch-time identity recheck. | `not_runnable` on drift; no launch or alternate runtime. |
| TM-03 | Dependency preparation uses an install hook, undeclared registry, writable shared cache, or unbounded output/time. | Separate bounded prepare stage; declared destinations; read-only content-addressed cache; no ambient credentials; exact output tree. | Abort preparation, discard run-owned tree, record destination/budget class; never run project. |
| TM-04 | Caller supplies inline credentials or child inherits host/provider secrets. | Secret references only; closed non-secret environment; ambient-variable sentinel; one-time scoped resolution at launch. | Reject before any service/project start; never retain or hash the value. |
| TM-05 | A secret is echoed in project/model/provider output or an error/log. | Known-secret and credential-pattern scan before digest; bounded raw memory; stable redacted errors. | Omit affected evidence, mark channel inconclusive, revoke reference, continue cleanup; no raw fallback. |
| TM-06 | Model relay becomes a generic credentialed proxy through arbitrary prompts, URLs, files, models, tools, redirects, or methods. | Package-owned closed request schema; exact model/tool/path/method; literal destination; proxy disabled; no redirects; pre/post tuple verification. | Reject with zero upstream calls when possible; after an attempted call retain conservative request/cost and postflight availability. |
| TM-07 | Project reaches another run's loopback service or a host-local daemon. | Logical per-run channels, unguessable scoped credentials, provider network isolation, explicit denied-target probes. | Reject posture/run on any cross-run/unapproved reachability; no weaker network mode. |
| TM-08 | DNS, proxy variables, redirects, alternate address forms, or direct sockets bypass egress policy. | Destination-normalized policy outside child; empty ambient proxy config; exact-IP/channel dialing; direct plus mediated probes. | Deny and record typed egress observation; missing denial logs remain `unavailable`, not observed. |
| TM-09 | Model/runtime/policy/provider identity changes during a run. | Immutable identities and before/after verification tied to the run envelope. | Stop new actions, mark result inconclusive or not_runnable, then clean; never reuse prior posture. |
| TM-10 | Replayed/wrong-stage capability starts a second project, repeats stimulus, or reads another run's artifact. | Run/stage/operation-bound consuming mutation capabilities; separate scoped read capability; durable idempotency records. | Reject without mutation; ambiguous dispatch is never retried. |
| TM-11 | Forged, duplicated, reordered, partial, or cross-run evidence creates a false positive or block. | Closed channels, per-channel sequence, source-projection digest, exact run envelope, normalized availability, independent effect evidence. | Correlator emits contradiction/inconclusive; no positive judgment. |
| TM-12 | Artifact traversal, extra object, role substitution, decompression bomb, or byte/CID mismatch contaminates the pack. | Closed logical roles; bounded uncompressed bytes; content addressing; exact set; caller re-verification; manifest-last publication. | Refuse artifact seal/publication and clean temporary objects. |
| TM-13 | Raw-content retention or redaction failure silently persists sensitive data. | Sole `redacted_digests` mode; raw in bounded memory only; known values neither persisted nor hashed. | Omit affected role/record, set explicit evidence impact, make judgment inconclusive where required. |
| TM-14 | Timeout/cancellation/service crash leaves processes, sockets, credentials, volumes, or provider resources. | Durable stage/cleanup target before dispatch; restart reconciliation; bounded terminal-absence proof; scoped credential revocation. | `cleanup_failed` until absence is proven; block reuse/support and preserve unrelated resources. |
| TM-15 | Process, request, output, token, cost, storage, or duration exhaustion harms the host/provider. | All ten budgets plus input/artifact/store caps; admission before work; saturating counters; cost ceiling before model use. | Reject or terminate at the exact cap, reserve cleanup, retain conservative cost/evidence. |
| TM-16 | Unsupported provider/capability silently falls back to a native host, raw process, another image, or another model. | Exact negotiation, immutable provider/runtime binding, no retry/fallback branch. | `not_runnable`; zero payload execution on the alternate path. |
| TM-17 | A caller or service identity accesses another tenant/run. | mTLS caller authorization, run ownership, asset-bundle identity, per-run capability scope, store namespace isolation. | Authentication/authorization failure before runtime I/O; no resource existence oracle beyond closed status. |

## Release-blocking verification

- Every threat above needs at least one positive and one adversarial conformance
  case, including cancellation at each mutating lifecycle stage.
- Credential values, raw logs, host paths, and provider errors must be seeded
  with markers and proven absent from normalized evidence and public errors.
- Both parent and child must pass allowed and denied filesystem/network probes.
- At least two provider families must pass the same conformance suite before a
  cross-provider stability claim; one provider is enough only for an exact
  single-provider support row.
- The current macOS SRT missing denial categories block a native-denial-log
  `observed` claim but do not permit fabrication; the evidence state is
  `unavailable` until fixed and rerun.

## Explicitly rejected shortcuts

Host worktree mounts, ambient environment copying, inline secret values, shared
writable dependency caches, arbitrary shell bootstrap fields, unrestricted
outbound network, caller-selected model/provider/destination, unbounded sessions,
raw artifact retention, and any unsandboxed/native fallback are design no-go
conditions.
