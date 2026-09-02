# Provider-neutral ProjectRun requirements

Task: SE-08-02  
Source baseline: Shift Left exact native `RunSpec`, `ProbeSpec`, `Posture`, cleanup,
run-profile, and Phase 07 governed evidence; OpenBox Sandbox v1 commit
`5e88a4548d391e7a0b6c9cb0154e06128f1d00fc`.

These are requirements for a later versioned capability, not changes to v1 and
not an implementation specification. Provider implementations may use different
OS primitives, gateways, images, or service managers, but must produce the same
observable contract.

| ID | Provider-neutral requirement | Native evidence traced | V1 primitive to preserve | V2 gap |
|---|---|---|---|---|
| PR-01 | Negotiate an explicit `ProjectRun` operation/version and reject it before mutation when unsupported. | Exact driver/version/config binding; no driver switch. | Protocol version, asset-bundle check, strict unknown-field rejection. | V1 has no capability negotiation or project operation. |
| PR-02 | Accept one canonical content-addressed source manifest and immutable file objects; stage an exact private snapshot without a host worktree mount. | Snapshot digest, source recheck, external-link/mount rejection. | Request-owned identity and fresh workspace. | No upload, manifest, file-object, or staging operation. |
| PR-03 | Bind one immutable runtime/template plus preparation recipe, dependency lock, produced-tree digest, and cache identity. Preparation must be bounded and must not obtain undeclared authority. | Exact Node/npm/framework/SDK lock and unchanged prepared-tree digest. | Deployment-selected template identity and policy hash. | No prepare phase, dependency/cache contract, or produced-tree identity. |
| PR-04 | Accept a closed non-secret environment map and opaque one-time secret references. Resolve secrets only at launch, expose only declared names, retain no values, and reject ambient inheritance. | Closed child environment, production-coordinate preflight, generated test credentials. | V1 environment is absent by construction. | No environment or secret-reference surface. |
| PR-05 | Declare logical local services and authenticated channels for readiness, stimulus, receiver, fixtures, and model relay; allocate runtime coordinates privately and reveal only scoped child endpoints. | Run-owned receiver/poison/sink/model services and literal-loopback child overlay. | Request-owned lifecycle and policy-selected networking. | No service topology or reverse-channel protocol. |
| PR-06 | Enforce default-deny egress over declared logical destinations and return typed allowed/denied observations. Direct bypass, redirects, proxy inheritance, and fallback are forbidden. | Allowed/denied loopback plus direct/proxied external parent/child probes. | Pinned network policy, proxy decisions, optional OS-denial evidence. | V1 destination policy is deployment-owned, not per-project topology-aware. |
| PR-07 | Run an exact parent-and-child capability probe for filesystem, network, credentials, inheritance, fallback, timeout, and cleanup; bind its immutable evidence to one subsequent run authorization. | `ProbeSpec`, one-shot `ProbeExecution`, `Posture`, exact posture digest. | Exact policy readiness and provider evidence. | No common project probe envelope or one-shot probe-to-run token. |
| PR-08 | Provide a prepare/commit project lifecycle: stage, prepare, start services, start project, wait ready, stimulate once, observe completion, stop, retrieve, clean. Every transition is idempotent or has durable reconciliation. | Phase 05 state machine and Phase 07 candidate lifecycle. | Create, ready, prepare/commit exec, cancel, delete, terminal absence, durable recovery. | V1 owns one command, not a multi-service project session. |
| PR-09 | Bind all ten budgets without defaults or widening: processes, requests, request bytes, total duration, stdout, stderr, input tokens, output tokens, decimal cost, and cleanup grace. Report unsupported enforcement before mutation. | Closed run-profile budgets; native process count truthfully unsupported. | Command timeout and independent output/chunk limits. | No request/token/cost/cleanup-grace budgets and no project process budget. |
| PR-10 | Stream bounded stdout, stderr, lifecycle, egress, denial, SDK, fixture, effect, and process observations with monotonic per-channel sequence and explicit missing/unavailable states. Never infer a pass from missing events. | Normalized evidence objects and six-outcome correlation/judgment. | Raw bounded output, real exit, timeout, egress decisions, optional violations. | No normalized multi-channel evidence or SDK/fixture/effect transport. |
| PR-11 | Retrieve artifacts by closed logical role, byte count, media type, and content digest. The caller verifies exact object sets before manifest-last publication. | Content-addressed audit pack, exact role/CID verification. | Versioned bounded response framing. | No artifact listing/retrieval or digest-bound receipts. |
| PR-12 | Keep raw content bounded and ephemeral; persist only redacted projections, lengths, and digests. Known credentials are never persisted or hashed; redaction failure omits evidence and marks it inconclusive. | V1 `redacted_digests` assurance retention. | Durable service records omit argv/output bodies. | Terminal raw stdout/stderr has no project retention/redaction contract. |
| PR-13 | Return a closed failure state with dispatch/creation/cleanup ownership and evidence availability. Never retry on another provider, host, raw process, or unsandboxed path. | Closed failure taxonomy and fallback sentinels. | Structural create/dispatch ambiguity and cleanup target. | No project-stage failure vocabulary or cross-service partial-state model. |
| PR-14 | Prove terminal cleanup for every owned process, socket, service, path, credential reference, and provider resource, including cancellation/restart paths. Preserve unrelated resources. | Cleanup binding/receipt and Phase 07 zero-resource teardown. | Delete, wait-deleted, terminal absence, restart reconciliation. | V1 cleanup does not enumerate project services, sockets, credentials, or artifacts. |
| PR-15 | Export a production client that exposes only typed ProjectRun operations and immutable evidence; it must not expose provider handles, arbitrary control-plane calls, or test-only constructors. | Shift Left common driver boundary and opaque evidence capabilities. | Strict service messages and private provider handles. | Current service client is compiled only for tests. |

## Required invariants

- Snapshot, runtime, preparation, profile, scenario, SDK coverage, sandbox
  posture, evidence, artifacts, and cleanup are one digest-bound run envelope.
- The child cannot choose a provider, model, destination, policy, listener,
  mount, secret value, or fallback driver.
- A service crash, model refusal, missing SDK event, sandbox denial, timeout, or
  mock decision cannot become a real OpenBox `blocked` result.
- An implementation must prove parent and child behavior, not only successful
  command execution or documentation/configuration intent.
- V1 request/response bytes and behavior stay unchanged. ProjectRun uses a new
  operation/media type and explicit capability negotiation.

## Non-requirements for this discovery goal

- No OpenBox Sandbox source change, wire prototype, provider adapter, image,
  deployment, benchmark, or Shift Left runtime driver.
- No host-specific path, binary, proxy variable, OS denial-log grammar, or
  Compose detail is part of the provider-neutral contract.
- No compatibility shim that lets a v1 service interpret ProjectRun as v1.
