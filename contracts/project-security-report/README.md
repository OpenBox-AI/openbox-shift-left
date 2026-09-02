# Project security report contracts

Phase 4 consumes one verified `ai.openbox.project-observation/v1` pack and one
untrusted `ai.openbox.project-security-analysis/v1` candidate. It captures a
bounded GET-only target posture, deterministically maps validated issues to
inert OpenBox recommendations, and seals the exact report pack described by
`schema/manifest.schema.json`.

The report is advisory. These contracts contain no write endpoint, request
payload, credential, Apply operation, approval decision, or enforcement claim.

The frozen recommendation catalog is version `2026-08-27-mvp1`, digest
`sha256:96ba1937ffa01aa8515da33cbd8b374c7981a9b7b160e36fa6a4ba7d60bf3dbe`,
and is bound to `local-dashboard-control-read/v1`. The target posture is a safe
projection captured by two identical local GET-only passes; it is not evidence
that a listed control covered or enforced an issue.

The exact pack inventory is:

```text
observation/run.json
observation/backend.json
observation/openshell.jsonl
observation/effects.json
observation/behavior.json
observation/coverage.json
observation/manifest.json
analysis.json
standards.json
recommendation-catalog.json
target-posture.json
report.json
report.md
report.sarif
manifest.json
```

Every file is owner-read-only, both directories are owner read/execute only,
the top-level manifest is written last, and an existing target is never
replaced. The three report projections are reconstructed from the embedded
inputs during offline verification. `no_supported_issue` and `inconclusive`
contain no issues or recommendations and never represent a security pass.
