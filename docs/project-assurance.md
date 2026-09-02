# Project assurance

Project assurance provides passive project inspection, one-shot local
development evaluation, native-host issue analysis, and sealed advisory
reporting. It remains separate from developer-runtime hook governance.

## Available commands

```text
openbox project inspect [path] [--output DIR]
openbox project evaluate --image IMAGE --env-file FILE --openbox-agent AGENT_ID --output DIR
openbox project verify PACK
openbox project finalize --evaluation OBSERVATION_PACK --analysis CANDIDATE_JSON --output REPORT_PACK
openbox project report --pack DIR [--format markdown|json|sarif]
openbox project propose --pack DIR [--format json|markdown]
```

`inspect` copies selected source into run-owned temporary storage, performs
bounded lexical discovery, writes exactly `project-snapshot.json`,
`project-model.json`, and `sdk-coverage.json`, and removes the temporary copy. It
does not execute or import project code and does not contact OpenBox or a model.

`verify` dispatches by the committed manifest. It preserves the historical v1
audit-pack verifier, fully reconstructs and schema-validates sealed Phase 2
observation packs, and independently reconstructs sealed Phase 4 security
report packs. `report` emits a verified audit- or security-report projection;
it does not render an observation pack. `propose` remains historical-only.
None of these commands applies, publishes, approves, or deploys an OpenBox
control.

## Development evaluation direction

`openbox project evaluate` is the narrow local execution and observation lane.
It runs the standard OCI `Entrypoint + Cmd` of one pre-existing `linux/arm64`
local image in OpenShell `0.0.111`, uses the preconfigured local OpenBox and
local Ollama route, then reads the exact terminal backend session through the
bounded dashboard API contract. Complete success produces a manifest-last,
owner-only `ai.openbox.project-observation/v1` pack. A failed run retains only
the mutually exclusive `.incomplete` diagnostic form. It accepts no project
path, replacement entrypoint, scenario, service port, health/invoke route,
mount, or production credential.

OpenShell supplies reproducible development-run orchestration and available
runtime observations. It is not a production security boundary or the OpenBox
enforcement plane. Landlock, PID-limit, signing, and other unavailable controls
remain explicit coverage limitations. Production workloads, identities,
credentials, endpoints, data, and automatic control publication are excluded.

The retained one-shot Mastra image is a conformance asset for that adapter and
SDK path. It does not create a customer security report and does not substitute
a fake SDK receiver for local OpenBox Core in the product workflow.

## Native-host analysis and finalization

`openbox init` installs the canonical `openbox-security-evaluation` skill for
the selected Claude Code or Codex provider. The developer explicitly invokes
it with only a verified observation pack and a new candidate path:

```text
openbox-security-evaluation OBSERVATION_PACK NEW_CANDIDATE_JSON
```

The mode-0600 candidate is untrusted and issue-only. The skill makes no OpenBox
request, reads no credential, reruns no project, and never recommends or applies
a control.

`project finalize` first verifies the full observation and candidate offline.
Any integrity, schema, citation, identity, permission, path, or output-preflight
failure occurs before credential lookup, network access, or output creation.
Only after that gate does it use the existing host-side
`OPENBOX_CONTROL_TOKEN` against the exact local backend to capture two matching,
GET-only safe projections of the target agent's current posture. It maps valid
issues through the frozen inert recommendation catalog and publishes an
owner-only, no-clobber `ai.openbox.project-security-report/v1` pack.

The report embeds the verified observation, accepted candidate, standards and
recommendation catalogs, safe target posture, and matching JSON, Markdown, and
SARIF projections. `no_supported_issue` and `inconclusive` both have zero
recommendations and are explicitly not security passes. Verification and
rendering of a sealed report are offline.

## Historical compatibility

The frozen `openbox.audit-pack/v1` contracts remain readable so prior evidence
can still be verified and rendered. Their run-profile, sandbox-posture,
scenario, judgment, and policy-proposal objects are historical read contracts;
they are not the schema or execution design for the OpenShell workflow.

Historical native Codex, Claude/SRT, Seatbelt, governed-rerun, and ProjectRun v2
plans and ADR sections remain decision records only. No corresponding runner,
probe, profile, scenario, receiver, or rerun command is reachable from the CLI.

## Data and authority

- Passive inspection, skill analysis, pack verification, and rendering remain
  local. Finalization makes only the bounded local GET-only posture read after
  its offline gate.
- Reports and historical proposals write to stdout unless redirected by the
  caller.
- Evaluation uses only the pre-existing provider-bound local evaluation key;
  developer environment files cannot contain credential-looking keys.
- Finalization reads the organization control token only from the environment;
  no credential, header, raw policy/guardrail body, or arbitrary backend
  response enters the report pack.
- Missing or invalid evidence fails closed; it never becomes a positive claim.
- OpenBox control suggestions are inert. Apply, approval, publication,
  deployment, and effectiveness verification require separate authority.
