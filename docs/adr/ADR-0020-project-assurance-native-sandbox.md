# ADR-0020 — Project assurance lane with native-sandbox execution

Date: 2026-08-19
Status: Historical; native execution retired 2026-08-26

Amended 2026-08-26 by the lean OpenShell Phase 0 decision: the native Codex,
Claude/SRT, Seatbelt, trusted-testbed, fake-receiver, fixed-profile/scenario,
and governed-rerun implementation is removed. The public CLI retains only
passive inspection and historical pack verification/reporting/proposals. The
next execution lane is the separately planned local OpenShell development
evaluation using normal SDK traffic to local OpenBox Core and read-only backend
collection. OpenShell is observation infrastructure, not production security
enforcement. The remainder of this ADR is retained as historical rationale and
does not describe a reachable runner.

Amended 2026-08-25 for the trusted Mastra constraint lane: arbitrary project
execution remains withdrawn, but the exact dependency-complete Mastra fixture
may run under a distinct `openbox-seatbelt-trusted-testbed` posture. Acceptance
depends on exact source, deterministic profile, lock, vendored SDK, installed
dependency-tree, Node 26.7.0, Seatbelt, and host identities. Host-wide reads,
wildcard-interface bind capability, and the non-kernel `maxProcesses` limit are
accepted only at this byte-pinned testbed boundary. The fixture emits bounded
input provenance; proposals remain inert; a governed rerun requires the exact
candidate digest and a disposable run-owned Compose system that is fully erased.

Amended 2026-08-25 after live adversarial review: the first-party Seatbelt
profile's declared TCP bind rule permits `0.0.0.0:<port>` as well as the required
loopback listener, and the profile permits reads outside the immutable snapshot.
The project launcher also validated but failed to apply its declared snapshot
CWD and accepted an unqualified Node runtime. The CWD bug is corrected, but
Seatbelt support is withdrawn and `project test` again fails before project or
profile reads. Historical Mastra packs remain functional observations only.

Amended 2026-08-23 after implementation review: the exact Codex `0.149.0`
profile reached both a declared and undeclared listener on `127.0.0.1`. Its
domain allowlist is host-scoped, so the tuple does not satisfy the Phase 03
approved-versus-unapproved loopback endpoint gate. The implementation now fails
closed before reading project or profile bytes and cannot mint a qualified Codex
posture. Historical runs remain functional observations, not current sandbox
support. Docker remains system-plane-only and ProjectRun v2 remains deferred.

Amended 2026-08-21 before release: filesystem-only Git state may remain
explicitly unknown, and passive inspection uses a separate fixed three-file
directory rather than an incomplete audit-pack workspace.

Amended 2026-08-22 by explicit owner decision: Codex CLI `0.149.0` on the
qualified macOS arm64 host is the sole MVP execution candidate. Docker is not
an alternative, fallback, or retained implementation. The profile continues
to declare and bind `maxProcesses`, but this native tuple does not enforce a
hard process-count ceiling and no evidence may claim that it does. Project
launch remains gated by fresh exact-version/config parent and child filesystem,
network, loopback, credential, fallback, timeout, and cleanup probes.
The exact profile enables local binding for the required literal-loopback
project entrypoint; it does not authorize a caller-selected bind address.

Amended 2026-08-22 by explicit owner clarification: Docker Compose may deliver
the disposable, loopback-only local OpenBox control/data plane used by an
authorized governed rerun. It may not launch or confine the evaluated project;
that project still runs only through the qualified native Codex sandbox. The
Compose project, volumes, network, credentials, policy, and histories are
run-owned and must be erased together. Because the qualified Mastra MVP is the
unsigned shape, its disposable governed test agent explicitly sets
`signing_required=false`; no DID or private signing key enters the child. That
test-only exemption and a Core auth check are required before project launch.

Amended 2026-08-22 by explicit owner decision: the report reader may add the
exact pinned runtime dependencies `github.com/santhosh-tekuri/jsonschema/v6
v6.0.3` and `github.com/dlclark/regexp2 v1.11.0`, with transitive
`golang.org/x/text v0.14.0`, solely to enforce the offline Draft 2020-12 gate
below. This deliberately spends a second external-dependency budget after
ADR-0015: the alternative is a bespoke schema engine or a runtime Node/Ajv
subprocess. The loader is empty, regular-expression evaluation has a fixed
package-owned timeout, versions are pinned, and these dependencies grant no
network, process, project-execution, or control-plane authority.

## Context

Shift Left currently governs coding-host actions through `provider.HookEngine`
and `adapters/common/hookflow`. That workstation lane is not a project security
evaluator: it does not passively model an application, run the application's
normal entrypoint in a qualified sandbox, correlate framework-SDK traffic with
safe effect evidence, or emit a local audit pack.

The project-assurance design requires those capabilities without creating a
second product binary, changing a framework SDK into a scanner, or mixing
synthetic attack evidence into workstation telemetry. The implementation plan
and its phase ledgers define the delivery sequence and qualification gates:

- [`dc/security-evaluate.md`](../../dc/security-evaluate.md)
- [`plans/260819-1600-project-security-evaluation/plan.md`](../../plans/260819-1600-project-security-evaluation/plan.md)

This ADR accepts the architecture boundary. Version-sensitive SDK and sandbox
claims still require the executable evidence named in that plan; acceptance of
the boundary is not a support claim for any host/version tuple.

## Decision

### One binary, two explicit lanes

Add `openbox project inspect|test|rerun|verify|report|propose` to the existing `openbox`
binary as a project-assurance lane. Its initial Go implementation lives under
`cli/internal/assurance`; it does not add a Go module. Versioned public artifact
contracts live under `contracts/project-assurance`.

The existing workstation lane remains unchanged in ownership:

- `provider.HookEngine` is the coding-host runtime SPI; and
- `adapters/common/hookflow` is the synchronous workstation engine.

Neither package may orchestrate project inspection or project execution.

### Passive inspection and explicit execution

`project inspect`, `project report`, and `project propose` do not execute or
import project code. Application code may run only through an explicit
`project test` command, after the selected sandbox driver passes its exact-
version and effective-configuration capability probes.

If confinement, loopback reachability, credential exclusion, child inheritance,
fallback behavior, timeout, or cleanup cannot be established, the result is
`not_runnable` and the project command is not launched. There is no automatic
driver switch and no unsandboxed retry.

### Existing SDK wire, unchanged

The project uses its existing OpenBox framework SDK and base instrumentation as
normal middleware. Fixtures supply attacker input, poisoned dependency data,
and safe sinks through declared project boundaries. The evaluator does not add
an SDK scan mode, fork an SDK, or claim that the SDK can substitute an arbitrary
in-process tool result.

Missing expected SDK events are missing or unknown coverage. They produce
`inconclusive` or `not_runnable` evidence, never a claim that no action occurred.

### Separate baseline and governed decision paths

Baseline execution uses a loopback-only receiver and a run-scoped test identity.
The receiver captures the SDK's normal auth/evaluate wire traffic and returns
`ALLOW`; it is separate from production Core and does not upload attack traffic.

Only a separately authorized governed rerun may use an isolated real OpenBox
decision path. `blocked` requires all three of these correlated observations:

1. a named real OpenBox decision matched the pre-effect action;
2. the framework SDK applied that decision before execution; and
3. the independently observed safe sink was not invoked.

A hard-coded or mock `BLOCK`, model refusal, process failure, absent event, or
host-sandbox denial cannot satisfy that predicate. A sandbox denial is reported
as `sandbox_prevented` when its required evidence is present.

### Deterministic evidence owns outcomes

Stable predicates over correlated SDK, fixture/sink, sandbox, and process
records own the closed outcome vocabulary: `exploitable`, `blocked`,
`sandbox_prevented`, `not_observed`, `inconclusive`, and `not_runnable`.
Confidence, repetition, coverage, and control reachability remain separate
fields. Model judgment may assist explanation but cannot assign an outcome.

### Local artifacts and inert proposals

Audit packs are content-addressed local artifacts. JSON is authoritative;
Markdown and SARIF are projections. Default retention persists normalized,
redacted evidence and content digests, not unrestricted raw content.

Before rendering, the reader must validate the manifest and every role with a
non-null schema against the exact compiled v1 schema bytes and must run the
accepted run-profile and scenario semantic checks. Structural CID verification
alone is not report authority. The validated capability is internal and cannot
be constructed by a caller.

V1 does not define severity. Reports must say severity is unavailable rather
than derive it from outcome; parity is required for the retained evidence level.
The SARIF projection is canonical SARIF `2.1.0` with `$schema`
`https://docs.oasis-open.org/sarif/sarif/v2.1.0/errata01/os/schemas/sarif-schema-2.1.0.json`
(qualified bytes SHA-256
`c3b4bb2d6093897483348925aaa73af03b3e3f4bd4ca38cef26dcb4212a2682e`).
Its driver is `openbox-project-assurance`, `ruleId` is the finding ID, result
level is `none`, and OpenBox properties retain the scenario ID, outcome,
evidence level, reachability, facts, evidence digests, contradictions, and
limitations. It invents no source location or severity.

`project propose` emits inert control proposals only. It does not edit project
source, register an agent, publish policy, deploy configuration, or write to an
external control plane. Any future apply/upload path requires separate authority
and an ADR for each new endpoint, table, or service it introduces.

### Local sandbox qualification; ProjectRun v2 separately gated

A qualified local sandbox is the MVP execution boundary; there is no container
fallback.

**Historical amendment, 2026-08-24 — the candidate became first-party.** This section
originally named the exact standalone Codex tuple as the sole MVP driver. That
tuple was withdrawn on 2026-08-23 for failing endpoint isolation, and the
standalone SRT candidate was rejected for the same reason. The cause was common
to both and is not a property of the operating system: Seatbelt expresses
per-port loopback rules natively — Apple's own `/usr/share/sandbox/*.sb` ship
`(allow network-outbound (remote ip "localhost:62078"))`, and SRT emits that
same form internally for its proxy ports — but neither product exposes the rule.
Codex's domain policy is host-scoped and SRT offers only the all-or-nothing
`allowLocalBinding`, so in both cases permitting the project's required
`127.0.0.1` bind also exposed every other port on the developer's machine.

The resulting candidate was an OpenBox-owned Seatbelt profile invoked through
`/usr/bin/sandbox-exec`. The 2026-08-25 review withdrew it: Seatbelt's accepted
local TCP syntax scopes the port but still permits a wildcard-interface bind,
and the generated profile allowed host-wide reads. The reviewed launch also
dropped snapshot CWD and runtime identity. Codex, SRT, and the first-party
profile are all unsupported, keep their recorded reasons, and are never
reachable as fallbacks.

Owning the profile means owning its correctness, which is the real cost of this
amendment. Three controls carry it: paths that cannot be written into an SBPL
string literal verbatim are rejected rather than escaped, because an escaping
bug in a confinement policy widens the sandbox silently; the sealed profile is
re-verified against its digest immediately before every launch; and the profile
must be proven to cover the exact probe envelope, so a profile can never
authorize a run under an envelope whose denials it does not enforce.
Documentation or package presence is insufficient: support is a tuple of exact driver version
and executable bytes, platform, effective configuration, and passing
parent/child capability probes. The public posture's `configurationDigest`
binds the executable-byte digest, effective-configuration digest, immutable
snapshot digest, exact declared probe envelope, and required managed-proxy
mode. Proxy observations accept only an explicit loopback HTTP proxy and retain
its digest, never an arbitrary ambient proxy. The envelope separately binds a
run-owned protected write root and explicit denied network targets; parent and
child must prove allowed proxy reachability plus proxy and direct denial for
those unapproved targets. Direct access to the declared loopback service is
permitted by the qualified profile because the project must bind and reach its
own literal-loopback listener. The built-in helper executable is
byte-identified, rechecked immediately before launch, and combined with the
effective sandbox configuration digest.

OpenBox Sandbox v1 is not reused as a full-project runner. Its current request
and workspace contract lacks the declared environment, mounts, secrets,
loopback services, and project-workspace semantics this plan requires. Phase 08
may reject reuse or accept a versioned `ProjectRun` v2 ADR and separate
implementation plan; it must not weaken or extend v1 in place.

## Authority boundaries

| Component or command | Authorized here | Not authorized here |
|---|---|---|
| `project inspect` | Passive file/config discovery and local artifacts | Importing or executing project code; behavioral verdicts |
| `project test` | Explicit bounded launch after a successful selected-driver probe | Silent fallback, production credentials/endpoints, destructive or undeclared targets |
| Baseline receiver | Loopback/test-identity capture and `ALLOW` | Production Core traffic or evidence upload |
| Governed rerun | Same pinned scenario against an explicitly authorized isolated real decision path | Mock verdicts presented as policy proof; production publication |
| Judge | Deterministic correlation and evidence classification | LLM-owned outcomes or filling missing evidence with inference |
| `project report` | Projections from the authoritative audit pack | Inventing or upgrading findings |
| `project propose` | Local inert proposal generation | Source edits, policy application, deployment, external writes |
| Phase 08 | Discovery, ADR, and separate planning for `ProjectRun` v2 | Implementing v2 or mutating Sandbox v1 under this plan |

## Artifact contract

### Schema inventory

Phase 01 implements exactly these public v1 identifiers under
`contracts/project-assurance/`:

| Identifier | Owns |
|---|---|
| `openbox.project-model/v1` | Passive project graph, snapshot identity, discovery provenance, and uncertainty |
| `openbox.project-run-profile/v1` | Explicit callable entrypoint, fixture bindings, environment inputs, budgets, and retention posture |
| `openbox.sdk-coverage/v1` | Exact framework/SDK tuple, enabled and expected instrumentation, exclusions, and observed gaps |
| `openbox.sandbox-posture/v1` | Driver/version/platform/configuration plus parent and child capability observations |
| `openbox.security-test/v1` | Finding-bound scenario, preconditions, stimulus, observation plan, predicates, and limits |
| `openbox.audit-pack/v1` | Manifest, evidence references, deterministic judgments, limits, and provenance |
| `openbox.policy-proposal/v1` | Inert control candidate, required interception evidence, risks, and rerun predicate |

This list is closed for the v1 implementation plan. An internal event or helper
shape does not gain a public schema identifier merely because it is serialized.

Filesystem-only inspection may report a detected Git repository with
`head=null` and `dirty=null` only when the project model carries the exact
`git-status` uncertainty. Inspection does not execute Git, parse its object
database, or guess a clean/dirty state.

### Compatibility rule

The `/v1` suffix is the artifact major version.

- An additive v1 change may add only an optional property with defined absence
  semantics, an optional manifest role, or a new schema definition that does
  not change existing validation or judgment. The new v1 validator must still
  accept every previously valid v1 artifact.
- Adding or changing a required property, removing or renaming a property,
  changing a type or meaning, changing an identifier or canonicalization rule,
  or adding a member to a closed enum is breaking and requires `/v2`.
- Outcome and control-reachability vocabularies are closed enums. They cannot be
  widened within v1.
- Validators are strict for the schema revision they ship with and reject
  unknown properties. The manifest records the SHA-256 digest of each schema
  used, so an older reader may reject a newer additive document explicitly;
  silent reinterpretation is not a compatibility promise.

There is no read-time migration or dual-write compatibility path. A future v2
reader may offer an explicit offline converter, but it may not relabel v1 bytes
or overwrite their evidence identity.

### Canonical JSON and digests

Every content-addressed JSON object uses the
[RFC 8785 JSON Canonicalization Scheme](https://www.rfc-editor.org/rfc/rfc8785.html)
over I-JSON input, encoded as UTF-8 with no trailing newline. Duplicate object
names, invalid Unicode, non-finite numbers, and data outside the I-JSON domain
are errors. Schema integers must remain within the interoperable signed 53-bit
range; larger or precision-sensitive quantities use schema-constrained decimal
strings.

The content identifier is `sha256:<64 lowercase hexadecimal digits>` over the
canonical bytes. JSONL evidence uses one canonical JSON value per line with a
single LF after every record, including the last; its digest covers the complete
byte stream. Non-JSON projections are hashed over their exact bytes.

### Standalone inspection directory

`project inspect` publishes exactly three files to
`.openbox/inspect/<inspection-id>/`, or to the exact directory selected with
`--output`:

```text
project-snapshot.json
project-model.json
sdk-coverage.json
```

The directory is atomically published without replacement. It has no index,
CID store, `.incomplete`, or `manifest.json`, and it is not an audit pack.
Default inspection output is excluded from every later source snapshot.

### Audit-pack directory

A completed pack has this fixed shape:

```text
.openbox/audit/<run-id>/
  manifest.json
  objects/
    sha256/
      <64-lowercase-hex-digest>
```

`manifest.json` is a canonical `openbox.audit-pack/v1` root that maps stable
logical roles to an object content identifier, media type, schema identifier
when applicable, byte length, and retention/redaction state. Logical roles cover
the project model, run profile, SDK coverage, sandbox posture, scenarios, SDK
events, fixture/effect events, judgments, JSON/Markdown/SARIF renderings, and
policy proposals when present. Reports and proposals are objects in the pack,
not independent evidence authorities.

`manifest.json` is written atomically and last. Before it exists, the directory
is an incomplete run workspace, not an audit pack. The pack root identifier is
`sha256(JCS(manifest.json))` and is reported by the CLI; it is not embedded in
the manifest, avoiding a self-reference. After finalization, mutation is an
error. A missing object, digest/length mismatch, unknown required role, or
schema-validation failure makes the pack incomplete or tampered; the reader
must not repair it or render a positive judgment.

The pack is local and contains only content the retention posture authorizes.
Unretained raw content is represented by its digest and an explicit omission
reason; a digest is not evidence of what withheld content meant.

### Schema inventory test plan

Phase 01 tests must establish all of the following before schema code is
considered implemented:

1. exactly the seven identifiers above exist once, with matching `$id`,
   `apiVersion`, filename inventory, and strict unknown-property behavior;
2. every valid example validates and paired missing-required, unknown-property,
   invalid-enum, unsafe-number, and malformed-digest examples fail;
3. the current v1 validators accept the frozen initial examples after every
   additive change, while a fixture for each breaking-change class is rejected;
4. RFC 8785 golden vectors and adversarial duplicate-name/invalid-Unicode inputs
   pin canonical bytes and SHA-256 across map insertion order;
5. manifest fixtures reject a missing, truncated, mutated, wrongly sized, or
   wrong-schema object and never use an unreferenced report as evidence; and
6. finalization tests prove the manifest is last, the root digest recomputes,
   and a finalized pack cannot be silently rewritten.

Phase 00 still records the HTTP run-profile and receiver subset, exact SDK and
sandbox tuples, and whether the historical `openbox-audit` name ever shipped.
Those qualifications may add details to this ADR but may not weaken the
authority or artifact boundaries above.

No compatibility shim is authorized by this ADR. One is permitted only if
SE-00-08 finds release evidence that the old executable shipped and records a
one-way error-only behavior; it must not create a second implementation path.

SE-00-08 found no such public release evidence in its public UTC search window ending
2026-08-19T16:15:20Z. All reachable refs and tags in the five relevant local
repositories contain no exact path or pickaxe match. The sole workspace mention
is an untracked document explicitly marked brainstorm/working name/to build,
not a shipped artifact. Across 19 public OpenBox-AI repositories, all release
endpoints returned successfully and the 48 releases contain no exact repository,
tag, or asset match; exact repository, commit, and issue searches also returned
zero. The Shift Left `v0.1.0` and `v0.2.0` linux-amd64 archives match their
published checksums, contain only `LICENSE`, `README.md`, and `openbox`, and the
README/binary strings contain no exact name. Relevant PyPI, npm, Homebrew, and
Go coordinates/searches are absent; a Crates.io 403 is recorded as inconclusive
and excluded rather than treated as negative evidence.

Therefore v1 adds no compatibility shim, alias, redirect, or second command
path. This is bounded negative public-source evidence, not a claim about private,
deleted, or manually distributed artifacts. An official released artifact or
package coordinate requires an ADR amendment before compatibility work.
Commands, response digests, archive/binary hashes, exact release assets, and
limits are recorded in
[`openbox-audit-release-search.json`](../../plans/260819-1600-project-security-evaluation/evidence/openbox-audit-release-search.json).

## HTTP run-profile v1 and receiver contract

SE-00-07 closes the source-profile decisions that Phase 01 and Phase 04 must
implement. A v1 profile is declarative integration data, not an executable
manifest. The project command remains required after `--` on `project test` and
cannot appear in the profile. The profile also cannot express the receiver URL
or identity, output root, sandbox driver/settings, OpenBox coordinates, or
secrets.

The reader caps the profile at 262,144 bytes and rejects lexical JSON nesting
deeper than 32 before decoding. The stimulus body template is capped at depth
16 and 65,536 serialized UTF-8 bytes and must fit the declared request-byte
budget after substitution. These are parser invariants because recursive JSON
Schema alone cannot bound aggregate bytes or depth.

The initial application entrypoint is deliberately narrow:

- HTTP only, bound to literal `127.0.0.1` on a parent-allocated ephemeral port
  passed through a declared environment name;
- one `GET` readiness path with bounded polling; and
- one `POST` JSON stimulus path with an explicit expected-status set.

The JSON body template performs string substitution only for the closed tokens
`{{scenario.id}}`, `{{run.marker}}`, `{{fixture.poison.url}}`,
`{{fixture.sink.url}}`, and `{{model.url}}`. It cannot resolve environment
variables, files, URLs, JSON pointers, or shell expressions. Tokens replace a
complete recursive JSON string value; interpolation, template syntax in object
keys, malformed braces, and unmatched tokens are rejected. Static environment
is closed to the optional literal `APP_ENV=security-test`; v1 has no generic
static environment passthrough. Generated binding names are also a closed set,
correlated one-to-one with only the application listener, the three
runner-owned fixture URLs, the run marker, and the scenario ID. Each name and
source is unique. The SDK API URL, test key, and required safe-control values
come from the trusted SDK descriptor and runner, never the profile. The unsigned
v1 baseline injects no DID or signing key and rejects either if already present.

The first SDK descriptor is pinned to
`@openbox-ai/openbox-mastra-sdk@1.0.0+@openbox-ai/openbox-sdk-ts@1.0.1+@mastra/core@1.8.0`;
the profile names exactly one required semantic action class, `recordingTool`.
That class is the observed camelCase `activity_type` for the scenario's
`recording-tool` sensitive outbound tool and is the only pre-effect gate needed
for the finding predicate. It does not claim agent/model, retrieval, HTTP-hook,
workflow, or lifecycle coverage; Phase 05 must qualify each additional expected
class before using it, and a missing event remains a coverage gap. The model
fixture is either `deterministic_local` or the sole v1 `authorized_relay`:
Ollama server `0.31.1` at `http://127.0.0.1:11434`, native `POST /api/chat`, and
`granite4.1:3b` at digest
`sha256:6fd349357287c7ffc9e38189a93b48ea175d24fc566b38f09cfc564fb7f303eb`.
The compiled descriptor owns two additional read-only inspection routes:
`GET /api/version` and `GET /api/tags`. They are called directly by the parent
before and after inference to exact-match server version, model name, full
digest, GGUF/Granite/3.4B/Q4_K_M details, and the exact `completion`/`tools`
capability set. They are not profile fields, child routes, or generic upstream
authority. The sole child/inference route remains `POST /api/chat`. A profile
cannot introduce another provider, destination, path, method, or model. The relay
declaration is inert without that descriptor and separate live-model authority;
it cannot act as a generic proxy. The child receives only the runner-generated
relay URL and one-time relay bearer. Ollama receives no credential, only the
closed synthetic text/tool request. Relay token budgets must be non-zero and
its decimal monetary-cost budget must be exactly zero; the deterministic local
model also requires zero cost. The prior unverified OpenAI relay is removed,
not retained as a compatibility path.

All ten v1 budget values are required: process count, request count, per-request
bytes, duration, stdout bytes, stderr bytes, input tokens, output tokens,
decimal-string USD cost, and cleanup grace. The native Codex MVP enforces the
request, byte, duration, output, token, cost, and cleanup limits; it records
`maxProcesses` as a declared unsupported limit and does not claim a hard process
ceiling. A missing, invalid, or contradictory budget fails preflight, and an
enforced budget terminates the run on exceedance. `maxDurationMs` covers launch through cleanup, so readiness timeout plus
cleanup grace cannot exceed it; the readiness interval cannot exceed its
timeout. A budget never widens automatically. The normalized profile records
the effective values rather than relying on parser defaults.

The default and only v1 retention mode is `redacted_digests`. Exact bounded
request content may exist in memory until normalization and finalization or
abort, but is not written as raw content. The pack persists redacted
projections, byte lengths, and SHA-256 digests for non-credential content.
Known credential values are neither persisted nor hashed. A redaction failure
omits the content and marks the affected evidence `inconclusive`; it never
falls back to raw persistence. The fixed posture is explicit in every effective
profile.

The baseline receiver binds a parent-selected loopback port and has exactly
three routes:

| Caller | Method and path | Behavior |
|---|---|---|
| Project test identity | `GET /api/v1/auth/validate` | Qualified SDK auth response only |
| Project test identity | `POST /api/v1/governance/evaluate` | Qualified SDK response with baseline `ALLOW` only |
| Parent readiness identity | `GET /_openbox/readiness` | Lifecycle readiness only; its distinct one-time bearer is never placed in the child environment |

The project-identity state machine is fixed: exactly one successful auth
validation establishes the run identity, followed by zero or more evaluate
requests until shutdown. Evaluate-before-auth and duplicate auth are rejected;
auth is not repeated before each evaluate. Every accepted or rejected request
consumes the shared request budget. Parent readiness polling is a separate
identity and state, ends after the first ready response or startup timeout, and
cannot establish project authentication.

Wrong identity, content type, method, path, host header, sequence, or size gets
no useful response and is recorded within caps. Unknown routes, approval
polling, redirects, caller-selected ports, non-loopback binds, and any attempt
to configure a baseline `BLOCK` are unsupported. Project attribution comes
from a run-scoped `obx_test_...` bearer generated by the parent, not from a run
ID in the request body.

Production-coordinate rejection occurs before any fixture, receiver, sandbox,
or application process starts. The runner builds the child environment from a
minimal allowlist and generated bindings. The only accepted baseline Core URL
is its own `http://127.0.0.1:<ephemeral-port>` with no userinfo, query, fragment,
redirect, DNS hostname, or caller override. Any `obx_live_...` or `obx_key_...`
credential, pre-existing OpenBox DID/private key/seed, provider credential,
configured non-loopback OpenBox URL, reserved profile environment name, or
untrusted SDK-coordinate override makes preflight `not_runnable`. Values are
rejected, not silently replaced. The explicitly authorized local Ollama relay
has no upstream credential and does not weaken this baseline rule.
The Phase 02 descriptor must also prove the qualified public `withOpenBox`
initialization site. For `apiUrl`/`apiKey`, the only accepted shapes are omission
so the exact `OPENBOX_URL`/`OPENBOX_API_KEY` fallbacks apply, or explicit mapping
to those exact child variables. For `validate`, `onApiError`, and
`sendActivityStartEvent`, the accepted shape is the exact safe literal
`true`/`fail_closed`/`true`, or omission so the runner injects
`OPENBOX_VALIDATE=true`, `OPENBOX_GOVERNANCE_POLICY=fail_closed`, and
`OPENBOX_SEND_ACTIVITY_START_EVENT=true`. Any conflicting or dynamic explicit
value, ambiguous initializer, or hard-coded production coordinate is
`not_runnable`; the runner never assumes an environment fallback outranks an
explicit option.

The frozen draft, valid local example, strict Ajv `8.17.1` draft-2020-12
validation, schema-negative fixtures, and template semantic-negative fixture
are in
[`project-run-profile-v1.schema-draft.json`](../../plans/260819-1600-project-security-evaluation/evidence/project-run-profile-v1.schema-draft.json),
[`project-run-profile-v1.example.json`](../../plans/260819-1600-project-security-evaluation/evidence/project-run-profile-v1.example.json),
and
[`project-run-profile-v1.validation.json`](../../plans/260819-1600-project-security-evaluation/evidence/project-run-profile-v1.validation.json).
The exact public validator package tree, registry integrity values, package
metadata hashes, and script-disabled replay command are retained in
[`ajv-8.17.1.qualification-lock.json`](../../plans/260819-1600-project-security-evaluation/evidence/ajv-8.17.1.qualification-lock.json).
Phase 01 owns the final schema inventory and Go validators; it may implement
these decisions but may not invent new profile authority.

## First SDK candidate

SE-00-04 qualifies this exact local-clone tuple for the initial Mastra fixture:

| Component | Exact candidate |
|---|---|
| OpenBox Mastra SDK | `1.0.0` at tag/commit `db9863bd6659f8b1ce6a33903ea61e4e564be38b` |
| OpenBox base SDK alias | `@openbox-ai/openbox-sdk` → `@openbox-ai/openbox-sdk-ts@1.0.1` |
| Mastra Core | `1.8.0` |
| Node / npm / platform | Node `26.7.0` / npm `11.19.0` / macOS `26.5.2` arm64 |

The public integration is `withOpenBox`. It resolves explicit `apiUrl` and
`apiKey` before `OPENBOX_URL` and `OPENBOX_API_KEY`; optional `agentDid` and
`agentPrivateKey` similarly precede `OPENBOX_AGENT_DID` and
`OPENBOX_AGENT_PRIVATE_KEY`, with both identity fields required together. The
Mastra layer passes those resolved values and `environ:{}` to the base SDK, so
base-only environment names such as `OPENBOX_API_URL` cannot override them.
The descriptor must require `validate=true`, `onApiError=fail_closed`,
`sendActivityStartEvent=true`, the exact locked package versions, and an
unambiguous `withOpenBox` initialization site under the accepted literal-or-
trusted-fallback shapes above. The observed receiver subset is
`GET /api/v1/auth/validate` and `POST /api/v1/governance/evaluate`.

The loopback probe uses a synthetic unsigned `obx_test_...` identity, a real
Mastra `1.8.0` instance, the SDK's normal `withOpenBox` middleware, and no
model/provider. It observed the direct tool
`ActivityStarted` request before the synthetic effect; an `ALLOW` response ran
the effect once, while a mock `BLOCK` response stopped it. The latter proves
only SDK response application, not a real OpenBox decision or `blocked`
outcome. Exact source, lock, normalized wire, test, and probe evidence is in
[`mastra-sdk-tuple.lock.json`](../../plans/260819-1600-project-security-evaluation/evidence/mastra-sdk-tuple.lock.json)
and [`mastra-sdk-wire-qualification.json`](../../plans/260819-1600-project-security-evaluation/evidence/mastra-sdk-wire-qualification.json).

Only the committed lock defines this candidate: the declared base and Mastra
dependencies use caret ranges. The probe does not qualify optional DID signing,
approval polling, model/provider execution, agent/workflow wrappers, or
hook-level instrumentation. With HITL enabled, the observed direct-tool
`ActivityCompleted` event omitted `activity_type`, while its gating
`ActivityStarted` retained it. The SDK's `maxEvaluatePayloadBytes` does not
bound that direct-tool start payload, so the run-profile and receiver retain
their own byte caps. Phase 02 must not infer coverage for an absent event or
widen support beyond this direct top-level tool shape without new executable
qualification.

This is dependency qualification, not a released support tuple. Phase 03 owns
that later judgment.

## First native-sandbox candidate

SE-03-10 requalifies Codex CLI `0.149.0` on macOS `26.5.2` arm64 as the sole
MVP driver after the installed 0.148.0 tuple drifted. The installed binary
SHA-256 is
`f4a74117b8142cda581c95ff753abf4508b5636d89682c1ed77e4a9249af8963`;
the matching `rust-v0.149.0` source tag resolves to
`758ef40f50c1a458425c7cfbf1eb12cbc07af0b0`.

The candidate uses the standalone `codex sandbox` command, not `codex exec` and
not a model invocation. Its permission profile extends `:workspace`, enables
the managed network proxy, and allowlists only `127.0.0.1`, `::1`, and
`localhost`. It enables local binding so the project can open its exact
literal-`127.0.0.1` HTTP entrypoint; the application bind is independently
closed by the run profile and project source. The launcher supplies a six-key sanitized environment containing
an isolated `CODEX_HOME`, a run-owned `TMPDIR`, a qualification marker, and no
API or cloud credential variables.

The executable probe observed both the parent and its spawned child:

- working in the declared run root, writing there, reading a synthetic marker
  outside it, and receiving `EPERM` for declared outside-write targets;
- inheriting `CODEX_SANDBOX=seatbelt`, the sanitized environment, and the
  managed proxy variables;
- reaching the loopback HTTP receiver through that proxy while an unlisted
  `.invalid` host received the proxy's deny response; and
- opening a literal-loopback listener and direct loopback socket while a direct
  external socket received `EPERM`, with the external attempt and outside
  writes present in `--log-denials` output.

The exact profile, normalized parent/child observations, repeated output
digest, timeout cleanup, and startup-failure sentinel are in
[`codex-sandbox-requalification.json`](../../plans/260819-1600-project-security-evaluation/evidence/codex-sandbox-requalification.json).
The probe is a feasible candidate record, not a support tuple.

Three Phase 03 gates remain mandatory. First, `codex sandbox` has no timeout or
output-cap flag, so the driver must own process-group termination, streaming
caps for both output streams, and cleanup; the spike proves timeout cleanup but
only observes native output forwarding. Second, the built-in workspace profile
allows broad reads and temporary-directory writes, which must remain explicit
in the posture rather than being described as a project-only filesystem. Third,
the spike fault-injected backend unavailability by using an outer Seatbelt
profile to deny execution of the inner `/usr/bin/sandbox-exec`; Codex exited
nonzero and never ran the payload unsandboxed. Phase 03 must preserve this
no-fallback behavior in the driver and rerun the injection before project
launch.

SE-03-10 owns the final support judgment for this tuple. The accepted profile
requires `maxProcesses=32`, and the generic probe/run contracts bind and
correlate that value. Codex exposes no hard process-count control, so the limit
is reported as unsupported. Process-group ownership, timeout termination, and
post-exit absence are cleanup evidence only; they are never relabeled as a
concurrent-process ceiling.

## Claude native-sandbox candidates

SE-00-06 qualifies Claude's standalone and inherited modes separately without
changing Codex as the first driver candidate. The standalone candidate is
`@anthropic-ai/sandbox-runtime==0.0.73`, pinned by npm tarball SHA-256
`15555a1e919f6f114ba6b6e4d93289df0d77f72ae92a1236778d4421b17a66ce`,
on Node `22.20.0`. Direct execution reports the CLI's hard-coded fallback
version `1.0.0`; the package metadata and tarball digest are therefore the
authoritative version evidence. The inherited candidate is the installed
Claude Code `2.1.235` arm64 binary with SHA-256
`83b8f806f6f2eea316cfe246628e6c23374711d868f1fd0409db551b877b7748`.

The standalone probe invoked no model. Its parent and child wrote only the run
workspace, received `EPERM` for protected reads and writes, received HTTP 403
for an unlisted host, reached the loopback receiver, and observed none of ten
synthetic credential names. An explicit missing settings file and a
fault-injected unavailable `/usr/bin/sandbox-exec` both exited nonzero without
executing their sentinels. The caller-owned one-second process-group timeout
removed the sandboxed process. The CLI forwarded 70,000 output bytes and has no
native output cap.

The inherited probe ran the actual Claude binary in `--bare` print mode against
a deterministic loopback Messages stub with only the Bash tool enabled. Its
strict settings set `sandbox.enabled=true`, `failIfUnavailable=true`,
`autoAllowBashIfSandboxed=true`, and `allowUnsandboxedCommands=false`. The
normal Bash call produced the same parent/child filesystem, network, loopback,
and credential observations as standalone SRT. A second call supplied
`dangerouslyDisableSandbox=true`; Claude ignored the escape and the protected
write received `EPERM`. A third call fault-injected denial of the inner
`sandbox-exec`; its tool result reported exit 126, no sentinel ran, and the
qualification wrapper returned nonzero status 86.

Two limitations are mandatory support-tuple fields. First, SRT puts loopback
in `NO_PROXY` on this POSIX tuple, so loopback access requires
`allowLocalBinding=true`; that permits direct access to every local port, not
only the test receiver. Second, Claude itself returns status zero when a Bash
backend error is followed by a successful model end-turn. Any inherited driver
must parse the Bash tool result and map backend unavailability to a nonzero
probe result before application launch. An outer SRT cannot kernel-confine the
Claude parent on macOS while preserving the nested Bash Seatbelt profile; the
qualification used a loopback API base, disabled nonessential traffic, and a
loopback deny proxy, observed no unexpected proxy requests, but did not capture
direct raw parent sockets. These candidates remain feasibility records, not
supported drivers. Exact commands, settings hashes, observations, output
digests, and cleanup results are in
[`claude-sandbox-qualification.json`](../../plans/260819-1600-project-security-evaluation/evidence/claude-sandbox-qualification.json).

## Alternatives rejected

**A separate `openbox-audit` executable.** Rejected because command routing and
local artifact conventions belong in the existing CLI, while a second binary
would duplicate composition and obscure the boundary between the two lanes.

**Put project assurance in `provider.HookEngine` or `hookflow`.** Rejected
because those seams own coding-host hook events and synchronous workstation
gating, not application discovery, sandbox lifecycle, fixtures, or audit packs.

**Add an evaluation-specific SDK mode or fork.** Rejected because it would test
a different application path from the one deployed. Normal middleware traffic
and explicit coverage gaps are the evidence contract.

**Treat every sandbox as one interchangeable driver.** Rejected because command
shape, network policy, loopback behavior, child inheritance, fallback, and
denial evidence vary by host version and platform.

**Stretch OpenBox Sandbox v1 into a project runner.** Rejected because adding
mount, environment, secret, and service semantics to v1 would silently change
its threat model and wire contract.

**Use a mock block to prove enforcement.** Rejected because it proves only SDK
response handling, not that a candidate OpenBox control matched the attack.

**Upload audit packs or auto-apply proposals.** Rejected because both cross a
material external-write boundary and can pollute production evidence or replace
live policy without review.

## Consequences

- The CLI gains a second lane while preserving the existing workstation engine
  and Go workspace layout.
- Support claims remain narrower than source or documentation claims and are
  revoked when a pinned tuple fails qualification.
- Baseline evaluation can prove bounded exploit behavior against safe fixtures,
  but cannot prove OpenBox enforcement.
- Governed reruns require explicit authority and a real isolated decision path.
- Missing SDK or sandbox evidence fails closed at the claim layer, even when the
  application process itself exits successfully.
- Phase 08 may produce only a reuse decision and separate plan under this goal;
  implementation of `ProjectRun` v2 remains out of scope.

## Evidence and limits

The initial local baseline is
[`baseline.json`](../../plans/260819-1600-project-security-evaluation/evidence/baseline.json).
It pins repository commits, installed host-tool versions, and platform state.
It does not qualify SDK wire compatibility, sandbox confinement, or any
supported tuple. Those observations remain owned by the later Phase 00 tasks
and the Phase 03 exit proof.
