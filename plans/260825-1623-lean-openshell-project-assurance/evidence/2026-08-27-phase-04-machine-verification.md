# Phase 4 machine verification — 2026-08-27

## Result

OS-04-01 through OS-04-03 are implemented. OS-04-04 remains `in_progress`.
This artifact records deterministic machine evidence only; it is not the
required live report review and does not mark Phase 4 verified.

The implemented public command is:

```text
openbox project finalize --evaluation OBSERVATION_PACK --analysis CANDIDATE_JSON --output REPORT_PACK
```

The complete observation/candidate/output gate runs before environment
credential lookup, the online runner, network access, or output creation. The
online part is one fixed local GET-only target-posture capture followed by
deterministic recommendation mapping and owner-only no-replace report sealing.
No model, project image, OpenShell run, installed host skill, control write,
Apply action, or approval decision is reachable from finalization.

## Frozen identities

- report pack: `ai.openbox.project-security-report/v1`
- target posture: `ai.openbox.project-target-posture/v1`
- read contract: `local-dashboard-control-read/v1`
- Phase 3 skill: `openbox-security-evaluation` `1.0.0`, digest
  `sha256:817e35e1db637d3c9a68ea7b0adf444aa1b5e9c2ad3eaa75c22496506ce0fe13`
- Phase 3 standards: `2026-08-26-mvp1`
- Phase 4 recommendation catalog: `2026-08-27-mvp1`, digest
  `sha256:96ba1937ffa01aa8515da33cbd8b374c7981a9b7b160e36fa6a4ba7d60bf3dbe`
- corrected Phase 2 Mastra observation digest:
  `sha256:2e724ab506e2eeea2c40b873fa05135940f0d6ad0fb0bf82609e7f2dca73fe25`

The public catalog and all four report schemas are byte-identical to their
embedded runtime copies. Every JSON contract and embedded JSON resource parses
with `jq empty`.

## Focused evidence

The following focused race suite passed:

```text
go test -race ./cmd/openbox \
  ./internal/assurance/observation \
  ./internal/assurance/runfs \
  ./internal/assurance/safety \
  ./internal/assurance/securityreport \
  ./internal/assurance/targetposture
```

The tests establish:

- both actual installed-host Mastra candidates pass the independent Phase 4
  authority as `no_supported_issue`, with zero issues, zero recommendations,
  and `security_pass: false`;
- a controlled issue cites retained backend semantic activity and an
  independent receipt, derives `tool_activity/recordingTool` from the retained
  backend record rather than candidate prose, and exercises all six frozen
  catalog entries and all five recommendation kinds;
- deterministic mapping distinguishes `new_gap`, `review_existing`, and
  `unavailable`, while unresolved action authority cannot become a supported
  target;
- forged observation identity, wrong evidence authority, missing backend
  authority, observed-as-gap dishonesty, unknown/unsorted standards, unsafe
  credential fields, and control target identity substitution fail closed;
- the target reader performs exactly this sequence twice with GET and
  `X-API-Key`: profile, exact agent, paginated guardrails, paginated policies,
  current policy, and paginated behavior rules. It never calls
  `behavior-rule/current` and uses the returned aggregate behavior-rule hash;
- wrong organization, agent, or exact permission set, response drift, unsafe
  fields, redirects, proxies, encodings, envelopes, pagination, and bounds are
  rejected;
- an existing or raced output target is never replaced, exact staging residue
  is removed, the sealed inventory/modes are exact, and any copied input or
  projection mutation fails verification; and
- report verification and Markdown/JSON/SARIF rendering reconstruct from the
  sealed pack without network or model access.

Generated SARIF passed the official OASIS SARIF 2.1.0 schema at
`sarif-2.1/schema/sarif-schema-2.1.0.json`, pinned by SHA-256
`c3b4bb2d6093897483348925aaa73af03b3e3f4bd4ca38cef26dcb4212a2682e`.

## Repository gates

All 11 modules listed in `go.work` passed each of:

```text
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
```

Those commands were run module-by-module because the repository root itself is
not a Go module. Contract/resource JSON parsing and `git diff --check` also
passed.

## Live qualification still required

The exact local backend health request returned status `200`. However, this
environment has:

```text
OPENBOX_CONTROL_TOKEN=unset
OPENBOX_BACKEND_URL=unset
~/.openbox/.env=absent
~/.openbox/dev.json=present
```

The coordinate can use its fixed default, but no existing organization control
credential is available. Phase 4 is not authorized to mint, rotate, recover, or
copy one. Therefore no real target-posture GET lane was run, no live report pack
was sealed, and no before/after control-plane or cleanup claim is made here.

To move OS-04-04 to `verified`, an authorized run must use the existing exact-
scope host credential to finalize both installed-host candidates against the
corrected observation, prove the local request transcript is GET-only and the
control state is unchanged, retain the sealed report and cleanup evidence, and
obtain human acceptance of the report-review artifact.
