# Project assurance contracts

This directory owns the seven public OpenBox project-assurance v1 JSON
contracts. JSON is authoritative; reports are projections from a verified
`openbox.audit-pack/v1` manifest.

| File | `$id` | Authority |
|---|---|---|
| `schema/project-model-v1.schema.json` | `openbox.project-model/v1` | Passive project graph, snapshot identity, discovery provenance, and uncertainty |
| `schema/project-run-profile-v1.schema.json` | `openbox.project-run-profile/v1` | Explicit HTTP entrypoint, fixture bindings, SDK tuple, budgets, and fixed retention posture |
| `schema/sdk-coverage-v1.schema.json` | `openbox.sdk-coverage/v1` | Exact framework/SDK tuple, expected instrumentation, observations, exclusions, and gaps |
| `schema/sandbox-posture-v1.schema.json` | `openbox.sandbox-posture/v1` | Driver tuple and parent/child confinement observations |
| `schema/security-test-v1.schema.json` | `openbox.security-test/v1` | Finding-bound scenario, stimulus, observation plan, predicates, and limits |
| `schema/audit-pack-v1.schema.json` | `openbox.audit-pack/v1` | Final manifest, addressed objects, deterministic judgments, limits, and provenance |
| `schema/policy-proposal-v1.schema.json` | `openbox.policy-proposal/v1` | Inert control candidate, required evidence, risks, and governed-rerun predicate |

The inventory is closed for v1. Validators reject unknown properties except in
explicit key/value maps. Outcome and reachability enums are closed. Integers
are bounded to the interoperable signed 53-bit range, and content identifiers
use `sha256:` plus 64 lowercase hexadecimal digits.

Normalized project-model locations are snapshot-relative. The logical project
root is `.`; absolute source, audit, and temporary paths belong only to the
volatile run envelope and therefore cannot perturb the content identity.

The MVP executable snapshot applies only closed built-in exclusions. Every
`.openbox/audit` and `.openbox/inspect` subtree is excluded, including prior and
nested output. Directory
basenames `.git`, `.hg`, `.svn`, `node_modules`, `.cache`, `__pycache__`,
`.pytest_cache`, `.mypy_cache`, `.ruff_cache`, `.tox`, `.venv`, `venv`, `.next`,
`.turbo`, and `coverage` are pruned from selection without reading file
contents. VCS and cache basenames are matched case-insensitively; explicit run
boundaries are also case-folded on Darwin to fail safely on its usual
case-insensitive filesystems. Recognized
secret paths are `.env` and `.env.*`; `.npmrc`, `.pypirc`, `.netrc`,
`credentials.json`, `secrets.{json,yaml,yml}`, service-account key names, common
private-key names and extensions; and project-local AWS, Docker, Kubernetes,
gcloud, and GitHub CLI credential paths. Their values are never copied, hashed,
or placed in omissions. Secret omissions suppress path examples as potentially
sensitive and set `examplesTruncated=true`; other omission classes retain at
most 16 normalized paths.
Sockets, FIFOs, devices, unsupported special files, external mounts, and
external or broken symlinks are path-only omissions. Safe internal symlinks stay
in source-selection identity but are not materialized into the v1 regular-file
snapshot.

`project_ignore`, `profile_include`, and `profile_exclude` remain closed source
labels for a producer that has an executable input contract for them. This MVP
adds no ignore parser, profile path fields, or CLI flags and must not claim those
rules were applied.

Adding an optional property with defined absence semantics is compatible.
Adding or changing a required property, type, meaning, identifier,
canonicalization rule, or closed-enum member requires v2. There is no read-time
migration, dual write, alias, or compatibility shim.

`testdata/valid/` freezes one initial example for every schema.
`testdata/mutation-cases.json` describes paired adversarial mutations; the
conformance probe applies them to the frozen examples so negative coverage
stays behavioral and compact.

## Cross-field invariants

JSON Schema enforces the closed shapes and all invariants it can express. The
readers and writers implemented in later phases must enforce these remaining
v1 rules exactly; a contradiction is invalid input, never a value to repair:

- Project-model node IDs and selection-rule IDs are unique. Every edge endpoint
  names an existing node, and every omission `ruleId` names a selection rule.
  Omission counts are non-zero, examples are snapshot-relative, and
  `examplesTruncated` is true exactly when fewer examples than the count are
  retained. Git absence is represented only by `present=false`, `head=null`,
  and `dirty=null`. A filesystem-only inspection may represent a detected Git
  repository as `present=true`, `head=null`, and `dirty=null` only when the
  project model contains the exact `git-status` uncertainty. It never runs Git
  or guesses clean/dirty state. A known state uses a boolean `dirty` value.
- SDK instrumentation action classes are unique. The schema pins the sole MVP
  tuple and requires one `recordingTool` row. `ready` requires every required
  row to be observed with a positive count and evidence; missing, excluded, or
  not-runnable rows have zero events and evidence explaining the state.
- Sandbox posture contains the complete parent and child capability maps.
  `qualified` is valid only when every required observation is `passed`; no
  failed, inconclusive, missing, or not-runnable probe can be upgraded.
- Security-test outcome fact arrays are the shared v1 judgment vocabulary.
  Facts are correlated observations, not model claims. The judge rejects
  mutually exclusive facts such as `safe_sink_receipt` and
  `safe_sink_not_invoked`; a baseline pack cannot contain `blocked`. The
  optional marker object is the sole executable v1 marker format: eight
  runner-random bytes encoded as sixteen lowercase hexadecimal characters
  after `synthetic-marker-`, with only SHA-256 and byte length retained.
  Absence remains schema-compatible but makes the scenario `not_runnable` for
  the v1 executor; a producer must not supply a default or caller value. The
  MVP executor recognizes only `ASI02-INDIRECT-EGRESS-001` and requires exactly
  once each the nine precondition IDs in the frozen example, their fixed kinds,
  the fixed observation correlations, all five forbidden substitutes, and the
  exact one-attempt/120000-millisecond/1024-record budget. Other schema-valid
  IDs or incomplete/duplicate/drifted requirements are `not_runnable`.
- Audit-pack schema references occur in the fixed order in the schema. Object
  keys are the unique logical roles: the fourteen keys required by the schema,
  plus optional `policy-proposals` only when a proposal exists. Each CID must
  resolve to exactly `bytes` bytes under `objects/sha256/<digest>`, hash to the
  named digest, and validate against the role's schema when non-null. The
  project-model, run-profile, SDK-coverage, sandbox-posture, and scenarios roles
  use their corresponding public IDs; event, judgment, cleanup, snapshot, and
  projection roles use `schema=null` until an accepted ADR adds a public
  contract. Missing, extra, mismatched, or mutated addressed objects invalidate
  the pack. Structured omissions record selection, redaction, truncation,
  unsupported coverage, unavailable evidence, and budget effects without raw
  credential values. `redaction_failed` always has `inconclusive` impact, and a
  truncation omission and the enclosing `truncated` flag must occur together.
  The role map is authoritative for snapshot, profile,
  coverage, posture, cleanup, report, and proposal content; those CIDs are not
  duplicated in provenance. The inline `judgments` array is the authoritative
  judgment value, and `objects.judgments` must equal the CID and canonical byte
  length of `JCS(judgments)` exactly.
  The fixed v1 media mapping is canonical JSON for project model, run profile,
  SDK coverage, sandbox posture, judgments, cleanup receipt, and JSON report;
  canonical JSONL for scenarios, SDK, fixture, and effect events and the
  optional policy proposals; canonical JSON under
  `application/vnd.openbox.project-snapshot` for the snapshot file manifest;
  UTF-8 Markdown for the Markdown report; and canonical SARIF JSON for the
  SARIF report. Every record in optional `policy-proposals` validates as one
  `openbox.policy-proposal/v1` object; there is no unversioned array wrapper. A
  producer never labels persisted payload bytes
  `digest_only` or `omitted`; withheld raw data is described by a retained,
  redacted omission object so `rawContentPersisted=false` remains truthful.
- Timestamp and executable-snapshot absolute path are volatile in-memory run
  provenance. They are not manifest properties or addressed-object content and
  cannot change a normalized object CID. A caller that emits an operational
  log may project that envelope separately, but it is not audit-pack evidence.
- A runtime policy proposal is valid only for `runtime_enforceable` reachability
  with every required interception field available before the effect. Its
  candidate verdict and governed-rerun outcome are both the blocking path; the
  single top-level baseline-pack digest is authoritative.

The promoted run-profile also requires the semantic validator defined by its
Phase 00 qualification: template, generated-binding, duration, relay, and
retention correlations are not inferred from JSON Schema alone.
